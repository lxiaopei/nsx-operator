/*
Copyright © 2026 Broadcom, Inc. All Rights Reserved.

	SPDX-License-Identifier: Apache-2.0
*/
package fabric

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vmware/vsphere-automation-sdk-go/services/nsxt/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/vmware-tanzu/nsx-operator/pkg/apis/migration/v1alpha1"
	"github.com/vmware-tanzu/nsx-operator/pkg/migration/recovery"
)

func setup(t *testing.T) (*recovery.Store, *recovery.Session, *api.ControlPlaneNetworkTransition) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, api.AddToScheme(scheme))
	obj := &api.ControlPlaneNetworkTransition{ObjectMeta: metav1.ObjectMeta{Name: api.ControlPlaneNetworkTransitionName, UID: "infra", Generation: 1}, Spec: api.ControlPlaneNetworkTransitionSpec{TransitionSpec: api.TransitionSpec{Action: api.ActionEvaluate}}, Status: &api.ControlPlaneNetworkTransitionStatus{TargetLBProvider: "NSX_LB", LoadBalancerVPCPath: "/orgs/default/projects/default/vpcs/lb"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(obj).WithObjects(obj).Build()
	store := &recovery.Store{Client: c, Reader: c}
	session, err := store.Acquire(context.Background(), recovery.Identity{UID: obj.UID, Name: obj.Name, Kind: "ControlPlaneNetworkTransition", Action: "Evaluate", Generation: 1}, recovery.Scope{AllNamespaces: true})
	require.NoError(t, err)
	return store, session, obj
}

type backend struct {
	state                 State
	reads                 int
	writes                int
	readErr, errorOnApply error
	pending               bool
	inspect               func(int) State
}

func (b *backend) Inspect(context.Context, Spec) (State, error) {
	b.reads++
	if b.inspect != nil {
		return b.inspect(b.reads), b.readErr
	}
	return b.state, b.readErr
}
func (b *backend) Apply(ctx context.Context, spec Spec, owner types.UID) error {
	if spec.ExpectedUniqueID != "" {
		state, err := b.Inspect(ctx, spec)
		if err != nil {
			return err
		}
		if !state.Exists || state.UniqueID != spec.ExpectedUniqueID {
			return errors.New("fabric resource identity changed")
		}
	}
	b.writes++
	uniqueID := b.state.UniqueID
	if uniqueID == "" {
		uniqueID = "resource-uid"
	}
	b.state = State{Exists: true, UniqueID: uniqueID, Owner: owner, Compatible: true, Ready: !b.pending}
	return b.errorOnApply
}
func vpcSpec() Spec {
	return Spec{Kind: VPC, ID: "lb", ProjectID: "default", CreateIfAbsent: true, VPC: &model.Vpc{}}
}

func TestFabricDoesNotCreateWithoutSharedTopologyInspection(t *testing.T) {
	_, session, _ := setup(t)
	b := &backend{}
	spec := vpcSpec()
	spec.CreateIfAbsent = false
	_, err := (&Service{Backend: b}).Ensure(context.Background(), session, spec)
	require.ErrorContains(t, err, "completed shared-topology candidate inspection")
	require.Zero(t, b.writes)
}

func TestFabricLostReplyAndRestartReconcileByLiveIdentity(t *testing.T) {
	ctx := context.Background()
	store, session, _ := setup(t)
	b := &backend{errorOnApply: errors.New("lost response")}
	s := &Service{Backend: b}
	_, err := s.Ensure(ctx, session, vpcSpec())
	require.ErrorContains(t, err, "lost response")
	require.Equal(t, 1, b.writes)
	restarted, err := store.Acquire(ctx, session.Identity(), recovery.Scope{AllNamespaces: true})
	require.NoError(t, err)
	record, err := s.Ensure(ctx, restarted, vpcSpec())
	require.NoError(t, err)
	require.Equal(t, "Created", record.Provenance)
	require.Equal(t, "resource-uid", record.UniqueID)
	require.Equal(t, 1, b.writes, "retry must inspect the deterministic live path before creating")
	_, err = s.Ensure(ctx, session, vpcSpec())
	require.ErrorIs(t, err, recovery.ErrFenced)
}

func TestFabricRetryRecreatesOnlyAtStablePathAfterObservedAbsence(t *testing.T) {
	_, session, _ := setup(t)
	b := &backend{pending: true}
	s := &Service{Backend: b}
	_, err := s.Ensure(context.Background(), session, vpcSpec())
	require.ErrorIs(t, err, recovery.ErrPending)
	b.state = State{}
	b.pending = false
	record, err := s.Ensure(context.Background(), session, vpcSpec())
	require.NoError(t, err)
	require.Equal(t, "lb", record.Path[strings.LastIndex(record.Path, "/")+1:])
	require.Equal(t, 2, b.writes, "the retry uses the same deterministic NSX ID")
}

func TestFabricChecksCandidateIdentityBeforeMutation(t *testing.T) {
	_, session, _ := setup(t)
	b := &backend{state: State{Exists: true, UniqueID: "replacement", Owner: "infra", Compatible: false}}
	spec := vpcSpec()
	spec.ExpectedUniqueID = "inspected-uid"
	_, err := (&Service{Backend: b}).Ensure(context.Background(), session, spec)
	require.ErrorContains(t, err, "candidate fabric identity changed")
	require.Zero(t, b.writes)
}

func TestFabricPendingStateIsRecheckedLive(t *testing.T) {
	_, session, _ := setup(t)
	b := &backend{pending: true}
	s := &Service{Backend: b}
	_, err := s.Ensure(context.Background(), session, vpcSpec())
	require.ErrorIs(t, err, recovery.ErrPending)
	b.state.Ready = false
	_, err = s.Ensure(context.Background(), session, vpcSpec())
	require.ErrorIs(t, err, recovery.ErrPending)
	require.Equal(t, 1, b.writes)
}

func TestFabricAdoptionAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   State
		adopt   string
		readErr error
		want    string
	}{
		{name: "unknown owner", state: State{Exists: true, UniqueID: "preexisting", Compatible: true, Ready: true}, want: "explicit live adoption"},
		{name: "adopt ready", state: State{Exists: true, UniqueID: "preexisting", Compatible: true, Ready: true}, adopt: "preexisting"},
		{name: "adopt incompatible", state: State{Exists: true, UniqueID: "preexisting", Ready: true}, adopt: "preexisting", want: "incompatible"},
		{name: "missing adoption", adopt: "preexisting", want: "no longer exists"},
		{name: "unavailable", readErr: errors.New("unavailable"), want: "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, session, _ := setup(t)
			b := &backend{state: tc.state, readErr: tc.readErr}
			spec := vpcSpec()
			spec.AdoptUniqueID = tc.adopt
			r, err := (&Service{Backend: b}).Ensure(context.Background(), session, spec)
			if tc.want != "" {
				require.ErrorContains(t, err, tc.want)
			} else {
				require.NoError(t, err)
				require.Equal(t, "Adopted", r.Provenance)
			}
			require.Zero(t, b.writes)
		})
	}
}

func TestFabricReconcilesOwnedConfigurationAndFencesActionChanges(t *testing.T) {
	store, session, obj := setup(t)
	b := &backend{state: State{Exists: true, UniqueID: "owned", Owner: "infra", Compatible: false, Ready: true}}
	spec := vpcSpec()
	spec.ExpectedUniqueID = "owned"
	_, err := (&Service{Backend: b}).Ensure(context.Background(), session, spec)
	require.NoError(t, err)
	require.Equal(t, 1, b.writes)
	require.NoError(t, store.Reader.Get(context.Background(), client.ObjectKeyFromObject(obj), obj))
	obj.Spec.Action = api.ActionPlan
	require.NoError(t, store.Client.Update(context.Background(), obj))
	_, err = (&Service{Backend: b}).Ensure(context.Background(), session, vpcSpec())
	require.ErrorIs(t, err, recovery.ErrFenced)
	require.Equal(t, 1, b.writes)
}

func TestFabricPhaseBoundaryAndSubnetContract(t *testing.T) {
	_, session, _ := setup(t)
	b := &backend{}
	spec := vpcSpec()
	spec.Kind = DVC
	spec.VPC = nil
	spec.DVC = &model.DistributedVlanConnection{VlanId: ptr.To(int64(10)), SubnetExtensionConnection: ptr.To(model.DistributedVlanConnection_SUBNET_EXTENSION_CONNECTION_ENABLED_L2_AND_L3), RestrictedAvailability: &model.VlanAvailability{ZoneExternalIds: []string{"zone-a"}}}
	_, err := (&Service{Backend: b}).Ensure(context.Background(), session, spec)
	require.ErrorContains(t, err, "Evaluate container")
	require.Zero(t, b.writes)
	require.Zero(t, b.reads)

	subnet := Spec{Kind: VPCSubnet, ID: "subnet", ProjectID: "default", VPCID: "vpc", VPCSubnet: &model.VpcSubnet{AccessMode: ptr.To(model.VpcSubnet_ACCESS_MODE_PUBLIC), VlanConnection: ptr.To("/infra/distributed-vlan-connections/dvc-10")}}
	require.Equal(t, "/orgs/default/projects/default/vpcs/vpc/subnets/subnet", subnet.Path())
	require.NoError(t, subnet.Validate())
	subnet.VPCSubnet.VlanConnection = ptr.To("/infra/distributed-vlan-connections/other")
	require.NoError(t, subnet.Validate(), "the planner may select any stable DVC ID after live dependency checks")
	subnet.DisplayName = "different"
	require.ErrorContains(t, subnet.Validate(), "display name must match")
}

func TestSNATStatusAcceptanceIsDurableAndImmutable(t *testing.T) {
	store, _, obj := setup(t)
	status := &KubernetesReservationStatus{Client: store.Client, Reader: store.Reader}
	r := Reservation{Family: "IPv4", IP: "192.0.2.10", Namespace: "vmware-system-netop", Name: "snat", UID: "allocation-uid"}
	require.NoError(t, status.Accept(context.Background(), obj, r))
	fresh := &api.ControlPlaneNetworkTransition{}
	require.NoError(t, store.Reader.Get(context.Background(), client.ObjectKeyFromObject(obj), fresh))
	require.Equal(t, r.IP, fresh.Status.LBSNATReservations[0].IPAddress)
	require.NoError(t, status.Accept(context.Background(), obj, r))
	r.IP = "192.0.2.11"
	require.ErrorContains(t, status.Accept(context.Background(), obj, r), "cannot be replaced")
}
