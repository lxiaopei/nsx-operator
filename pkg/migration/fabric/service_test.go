/*
Copyright © 2026 Broadcom, Inc. All Rights Reserved.

	SPDX-License-Identifier: Apache-2.0
*/
package fabric

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vmware/vsphere-automation-sdk-go/runtime/data"
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
}

func (b *backend) Inspect(context.Context, Spec) (State, error) { b.reads++; return b.state, b.readErr }
func (b *backend) Apply(_ context.Context, _ Spec, owner types.UID) error {
	b.writes++
	b.state = State{Exists: true, UniqueID: "resource-uid", Owner: owner, Compatible: true, Ready: !b.pending}
	return b.errorOnApply
}
func vpcSpec() Spec {
	return Spec{Kind: VPC, ID: "lb", ProjectID: "default", VPC: &model.Vpc{}, Consumers: []Consumer{{Kind: "ControlPlaneNetworkTransition", Name: api.ControlPlaneNetworkTransitionName, UID: "infra"}}}
}

func TestFabricLostReplyAndRestart(t *testing.T) {
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
	require.Equal(t, 1, b.writes)
	_, err = s.Ensure(ctx, session, vpcSpec())
	require.ErrorIs(t, err, recovery.ErrFenced)
	b.state.UniqueID = "replacement"
	_, err = s.Ensure(ctx, restarted, vpcSpec())
	require.ErrorContains(t, err, "replaced")
	require.Equal(t, 1, b.writes)
}

func TestPendingFabricIdentityRetained(t *testing.T) {
	_, session, _ := setup(t)
	b := &backend{pending: true}
	s := &Service{Backend: b}
	_, err := s.Ensure(context.Background(), session, vpcSpec())
	require.ErrorIs(t, err, recovery.ErrPending)
	b.state = State{} // pending resource deleted before realization
	_, err = s.Ensure(context.Background(), session, vpcSpec())
	require.ErrorContains(t, err, "removed or replaced")
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
		{name: "unknown owner", state: State{Exists: true, UniqueID: "preexisting", Compatible: true, Ready: true}, want: "explicit adoption"},
		{name: "adopt ready", state: State{Exists: true, UniqueID: "preexisting", Compatible: true, Ready: true}, adopt: "preexisting"},
		{name: "adopt incompatible", state: State{Exists: true, UniqueID: "preexisting", Ready: true}, adopt: "preexisting", want: "incompatible"},
		{name: "missing adoption", adopt: "preexisting", want: "does not exist"},
		{name: "unavailable", readErr: errors.New("unavailable"), want: "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, session, _ := setup(t)
			b := &backend{state: tc.state, readErr: tc.readErr}
			s := &Service{Backend: b}
			spec := vpcSpec()
			spec.AdoptUniqueID = tc.adopt
			r, err := s.Ensure(context.Background(), session, spec)
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

func TestFabricRejectsPayloadAndActionChanges(t *testing.T) {
	store, session, obj := setup(t)
	b := &backend{}
	s := &Service{Backend: b}
	spec := vpcSpec()
	_, err := s.Ensure(context.Background(), session, spec)
	require.NoError(t, err)
	spec.VPC.PrivateIps = []string{"10.0.0.0/24"}
	_, err = s.Ensure(context.Background(), session, spec)
	require.ErrorContains(t, err, "checkpoint")
	obj.Spec.Action = api.ActionPlan
	require.NoError(t, store.Client.Update(context.Background(), obj))
	// Persisted request check prevents side effects even with an otherwise valid handle.
	spec = vpcSpec()
	spec.ID = "another"
	b.state = State{}
	_, err = s.Ensure(context.Background(), session, spec)
	require.ErrorIs(t, err, recovery.ErrFenced)
	require.Equal(t, 1, b.writes)
}

func TestFabricPhaseBoundary(t *testing.T) {
	_, session, _ := setup(t)
	b := &backend{}
	s := &Service{Backend: b}
	spec := vpcSpec()
	spec.VPC = nil
	spec.Kind = DVC
	spec.DVC = &model.DistributedVlanConnection{VlanId: ptr.To(int64(10)), SubnetExtensionConnection: ptr.To(model.DistributedVlanConnection_SUBNET_EXTENSION_CONNECTION_ENABLED_L2_AND_L3), RestrictedAvailability: &model.VlanAvailability{ZoneExternalIds: []string{"zone-a"}}}
	_, err := s.Ensure(context.Background(), session, spec)
	require.ErrorContains(t, err, "Evaluate container")
	require.Zero(t, b.writes)
	require.Zero(t, b.reads)
	spec = vpcSpec()
	spec.VPC.Id = &spec.ID
	_, err = s.Ensure(context.Background(), session, spec)
	require.ErrorContains(t, err, "server or unsupported field")
	require.Zero(t, b.reads)
}

func TestStatusAcceptanceIsDurableAndImmutable(t *testing.T) {
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

func TestTGWSpanParticipatesInPayloadIdentity(t *testing.T) {
	spec := vpcSpec()
	spec.Kind = TransitGateway
	spec.VPC = nil
	spec.TransitGateway = &model.TransitGateway{Span: data.NewStructValue("span", map[string]data.DataValue{"transport_zone_path": data.NewStringValue("zone-a")})}
	first := digest(spec)
	spec.TransitGateway.Span = data.NewStructValue("span", map[string]data.DataValue{"transport_zone_path": data.NewStringValue("zone-b")})
	require.NotEqual(t, first, digest(spec))
}

func TestDVCRequiresResolvedL2L3Configuration(t *testing.T) {
	spec := vpcSpec()
	spec.Kind = DVC
	spec.VPC = nil
	spec.DVC = &model.DistributedVlanConnection{}
	require.ErrorContains(t, spec.Validate(), "ENABLED_L2_AND_L3")
	spec.DVC.VlanId = ptr.To(int64(10))
	spec.DVC.SubnetExtensionConnection = ptr.To(model.DistributedVlanConnection_SUBNET_EXTENSION_CONNECTION_ENABLED_L2_AND_L3)
	require.ErrorContains(t, spec.Validate(), "zone/span")
	spec.DVC.RestrictedAvailability = &model.VlanAvailability{ZoneExternalIds: []string{"zone-a"}}
	require.NoError(t, spec.Validate())
}
