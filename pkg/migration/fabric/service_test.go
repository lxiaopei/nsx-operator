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
	api "github.com/vmware-tanzu/nsx-operator/pkg/apis/migration/v1alpha1"
	"github.com/vmware/vsphere-automation-sdk-go/services/nsxt/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func setup(t *testing.T) (client.Client, *api.ControlPlaneNetworkTransition) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, api.AddToScheme(scheme))
	obj := &api.ControlPlaneNetworkTransition{ObjectMeta: metav1.ObjectMeta{Name: api.ControlPlaneNetworkTransitionName, UID: "infra", Generation: 1}, Spec: api.ControlPlaneNetworkTransitionSpec{TransitionSpec: api.TransitionSpec{Action: api.ActionEvaluate}}, Status: &api.ControlPlaneNetworkTransitionStatus{TargetLBProvider: "NSX_LB", LoadBalancerVPCPath: "/orgs/default/projects/default/vpcs/lb"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(obj).WithObjects(obj).Build()
	return c, obj
}

type backend struct {
	state                 State
	reads, writes         int
	readErr, errorOnApply error
	pending               bool
}

func (b *backend) Inspect(context.Context, Spec) (State, error) {
	b.reads++
	if b.state.Exists {
		b.state.Ready = !b.pending
	}
	return b.state, b.readErr
}
func (b *backend) Apply(_ context.Context, _ Spec, owner types.UID) error {
	b.writes++
	if !b.state.Exists {
		b.state.UniqueID = "resource-uid"
	}
	b.state.Exists = true
	b.state.Owner = owner
	b.state.Compatible = true
	b.state.Ready = !b.pending
	return b.errorOnApply
}

func vpcSpec() Spec {
	return Spec{Kind: VPC, ID: "lb", ProjectID: "default", CreateIfAbsent: true, VPC: &model.Vpc{}}
}

func evaluatePrerequisiteSpecs() []Spec {
	return []Spec{
		{Kind: IPBlock, ID: "migration-block", CreateIfAbsent: true, IPBlock: &model.IpAddressBlock{}},
		{Kind: ConnectivityProfile, ID: "migration-profile", ProjectID: "default", CreateIfAbsent: true, ConnectivityProfile: &model.VpcConnectivityProfile{TransitGatewayPath: ptr.To("/orgs/default/projects/default/transit-gateways/migration-tgw")}},
		{Kind: VPCAttachment, ID: "default", ProjectID: "default", VPCID: "migration-vpc", CreateIfAbsent: true, VPCAttachment: &model.VpcAttachment{VpcConnectivityProfile: ptr.To("/orgs/default/projects/default/vpc-connectivity-profiles/migration-profile")}},
	}
}

func TestEnsureRequiresSharedTopologyInspectionBeforeCreate(t *testing.T) {
	_, _ = setup(t)
	b := &backend{}
	spec := vpcSpec()
	spec.CreateIfAbsent = false
	_, err := (&Service{Backend: b}).Ensure(context.Background(), api.ActionEvaluate, "infra", spec)
	require.ErrorContains(t, err, "completed shared-topology candidate inspection")
	require.Zero(t, b.writes)
}

func TestEnsureRetriesByInspectingLiveStablePath(t *testing.T) {
	b := &backend{errorOnApply: errors.New("lost response")}
	s := &Service{Backend: b}
	_, err := s.Ensure(context.Background(), api.ActionEvaluate, "infra", vpcSpec())
	require.ErrorContains(t, err, "lost response")
	require.Equal(t, 1, b.writes)
	b.errorOnApply = nil
	record, err := s.Ensure(context.Background(), api.ActionEvaluate, "infra", vpcSpec())
	require.NoError(t, err)
	require.Equal(t, "Created", record.Provenance)
	require.Equal(t, "resource-uid", record.UniqueID)
	require.Equal(t, 1, b.writes, "the retry must inspect the live path before creating")
}

func TestEnsureRequiresExplicitAdoption(t *testing.T) {
	b := &backend{state: State{Exists: true, UniqueID: "existing", Compatible: true, Ready: true}}
	spec := vpcSpec()
	_, err := (&Service{Backend: b}).Ensure(context.Background(), api.ActionEvaluate, "infra", spec)
	require.ErrorContains(t, err, "explicit live adoption identity")
	require.Zero(t, b.writes)
	spec.AdoptUniqueID = "existing"
	record, err := (&Service{Backend: b}).Ensure(context.Background(), api.ActionEvaluate, "infra", spec)
	require.NoError(t, err)
	require.Equal(t, "Adopted", record.Provenance)
	require.Zero(t, b.writes)
}

func TestEnsureDoesNotMutateInWrongAction(t *testing.T) {
	b := &backend{}
	spec := vpcSpec()
	spec.Kind, spec.VPC, spec.DVC = DVC, nil, &model.DistributedVlanConnection{VlanId: ptr.To(int64(10)), SubnetExtensionConnection: ptr.To(model.DistributedVlanConnection_SUBNET_EXTENSION_CONNECTION_ENABLED_L2_AND_L3), RestrictedAvailability: &model.VlanAvailability{ZoneExternalIds: []string{"zone-a"}}}
	_, err := (&Service{Backend: b}).Ensure(context.Background(), api.ActionEvaluate, "infra", spec)
	require.ErrorContains(t, err, "Evaluate cannot prepare DVC")
	require.Zero(t, b.reads)
	require.Zero(t, b.writes)
}

func TestEvaluatePreparesTGWConnectivityPrerequisites(t *testing.T) {
	for _, spec := range evaluatePrerequisiteSpecs() {
		t.Run(string(spec.Kind), func(t *testing.T) {
			b := &backend{}
			record, err := (&Service{Backend: b}).Ensure(context.Background(), api.ActionEvaluate, "infra", spec)
			require.NoError(t, err)
			require.Equal(t, spec.Path(), record.Path)
			require.Equal(t, 1, b.writes)
		})
	}
}

func TestConnectivityProfileAndAttachmentRequireResolvedReferences(t *testing.T) {
	profile := evaluatePrerequisiteSpecs()[1]
	profile.ConnectivityProfile.TransitGatewayPath = ptr.To("/orgs/default/projects/default/transit-gateways/")
	require.ErrorContains(t, profile.Validate(), "requires a TGW path")

	attachment := evaluatePrerequisiteSpecs()[2]
	attachment.VPCAttachment.VpcConnectivityProfile = ptr.To("/orgs/default/projects/other/vpc-connectivity-profiles/profile")
	require.ErrorContains(t, attachment.Validate(), "requires a connectivity profile path")
}

func TestDeriveCannotCreateEvaluateConnectivityPrerequisites(t *testing.T) {
	for _, spec := range evaluatePrerequisiteSpecs()[1:] {
		t.Run(string(spec.Kind), func(t *testing.T) {
			b := &backend{}
			_, err := (&Service{Backend: b}).Ensure(context.Background(), api.ActionDerive, "infra", spec)
			require.ErrorContains(t, err, "cannot prepare")
			require.Zero(t, b.reads)
			require.Zero(t, b.writes)
		})
	}
}

func TestEnsureWaitsForRealization(t *testing.T) {
	b := &backend{pending: true}
	s := &Service{Backend: b}
	_, err := s.Ensure(context.Background(), api.ActionEvaluate, "infra", vpcSpec())
	require.ErrorIs(t, err, ErrPending)
	b.pending = false
	record, err := s.Ensure(context.Background(), api.ActionEvaluate, "infra", vpcSpec())
	require.NoError(t, err)
	require.Equal(t, "lb", record.Path[len(record.Path)-2:])
	require.Equal(t, 1, b.writes)
}

func TestSNATStatusAcceptanceIsDurableAndImmutable(t *testing.T) {
	c, obj := setup(t)
	status := &KubernetesReservationStatus{Client: c, Reader: c}
	r := Reservation{Family: "IPv4", IP: "192.0.2.10", Namespace: SNATNamespace, Name: "snat"}
	require.NoError(t, status.Accept(context.Background(), obj, r))
	fresh := &api.ControlPlaneNetworkTransition{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(obj), fresh))
	require.Equal(t, r.IP, fresh.Status.LBSNATIPAllocations[0].IPAddress)
	require.NoError(t, status.Accept(context.Background(), obj, r))
	r.IP = "192.0.2.11"
	require.ErrorContains(t, status.Accept(context.Background(), obj, r), "cannot be replaced")
}
