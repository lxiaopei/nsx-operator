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

func (b *backend) Inspect(context.Context, BackendSpec) (State, error) {
	b.reads++
	if b.state.Exists {
		b.state.Ready = !b.pending
	}
	return b.state, b.readErr
}
func (b *backend) Apply(_ context.Context, _ BackendSpec, owner types.UID) error {
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

func vpcRequest() VPCRequest {
	return VPCRequest{ID: "lb", ProjectID: "default"}
}

func evaluatePrerequisiteRequests() []struct {
	name    string
	request EvaluateRequest
} {
	return []struct {
		name    string
		request EvaluateRequest
	}{
		{name: "IPBlock", request: IPBlockRequest{ID: "migration-block"}},
		{name: "ConnectivityProfile", request: ConnectivityProfileRequest{ID: "migration-profile", ProjectID: "default", TransitGatewayID: "migration-tgw"}},
		{name: "VPCAttachment", request: VPCAttachmentRequest{ID: "default", ProjectID: "default", VPCID: "migration-vpc", ConnectivityProfileID: "migration-profile"}},
	}
}

func TestEnsureRetriesByInspectingLiveStablePath(t *testing.T) {
	b := &backend{errorOnApply: errors.New("lost response")}
	s := &Service{Backend: b}
	_, err := s.EnsureEvaluate(context.Background(), "infra", vpcRequest())
	require.ErrorContains(t, err, "lost response")
	require.Equal(t, 1, b.writes)
	b.errorOnApply = nil
	record, err := s.EnsureEvaluate(context.Background(), "infra", vpcRequest())
	require.NoError(t, err)
	require.Equal(t, "Created", record.Provenance)
	require.Equal(t, "resource-uid", record.UniqueID)
	require.Equal(t, 1, b.writes, "the retry must inspect the live path before creating")
}

func TestEnsureRequiresExpectedIdentityToAdopt(t *testing.T) {
	b := &backend{state: State{Exists: true, UniqueID: "existing", Compatible: true, Ready: true}}
	spec := vpcRequest()
	_, err := (&Service{Backend: b}).EnsureEvaluate(context.Background(), "infra", spec)
	require.ErrorContains(t, err, "explicit live adoption identity")
	require.Zero(t, b.writes)
	spec.ExpectedUniqueID = "existing"
	record, err := (&Service{Backend: b}).EnsureEvaluate(context.Background(), "infra", spec)
	require.NoError(t, err)
	require.Equal(t, "Adopted", record.Provenance)
	require.Zero(t, b.writes)
}

func TestEnsureDoesNotReplaceMissingSelectedResource(t *testing.T) {
	b := &backend{}
	spec := vpcRequest()
	spec.ExpectedUniqueID = "previously-selected"
	_, err := (&Service{Backend: b}).EnsureEvaluate(context.Background(), "infra", spec)
	require.ErrorContains(t, err, "requested fabric candidate no longer exists")
	require.Zero(t, b.writes)
}

func TestEvaluatePreparesTGWConnectivityPrerequisites(t *testing.T) {
	for _, testCase := range evaluatePrerequisiteRequests() {
		t.Run(testCase.name, func(t *testing.T) {
			b := &backend{}
			record, err := (&Service{Backend: b}).EnsureEvaluate(context.Background(), "infra", testCase.request)
			require.NoError(t, err)
			spec, err := testCase.request.evaluateSpec()
			require.NoError(t, err)
			require.Equal(t, spec.Path(), record.Path)
			require.Equal(t, 1, b.writes)
		})
	}
}

func TestConnectivityProfileAndAttachmentRequireResolvedReferences(t *testing.T) {
	profile, err := (ConnectivityProfileRequest{ID: "profile", ProjectID: "default"}).evaluateSpec()
	require.NoError(t, err)
	require.ErrorContains(t, profile.Validate(), "requires a TGW path")

	attachment, err := (VPCAttachmentRequest{ID: "default", ProjectID: "default", VPCID: "vpc"}).evaluateSpec()
	require.NoError(t, err)
	require.ErrorContains(t, attachment.Validate(), "requires a connectivity profile path")
}

func TestPhaseSpecificRequestTypes(t *testing.T) {
	var _ EvaluateRequest = TransitGatewayRequest{}
	var _ EvaluateRequest = VPCRequest{}
	var _ EvaluateRequest = VPCAttachmentRequest{}
	var _ EvaluateRequest = ConnectivityProfileRequest{}
	var _ EvaluateRequest = IPBlockRequest{}
	var _ DeriveRequest = VPCSubnetRequest{}
	var _ DeriveRequest = DVCRequest{}
	var _ DeriveRequest = IPBlockRequest{}
}

func TestResourceRequestsBuildNSXPayloads(t *testing.T) {
	vpc, err := (VPCRequest{ID: "migration-vpc", ProjectID: "default", ExternalIPv4BlockIDs: []string{"external"}}).evaluateSpec()
	require.NoError(t, err)
	require.Equal(t, VPC, vpc.Kind)
	require.Equal(t, "/orgs/default/projects/default/vpcs/migration-vpc", vpc.Path())
	require.Equal(t, "IPV4", *vpc.VPC.IpAddressType)
	require.Equal(t, []string{"/infra/ip-blocks/external"}, vpc.VPC.ExternalIpv4Blocks)
	require.Empty(t, vpc.VPC.Ipv6ProfilePaths)

	tgw, err := (TransitGatewayRequest{ID: "migration-tgw", ProjectID: "default", Span: &ZoneSpan{ZoneExternalIDs: []string{"zone-a"}}}).evaluateSpec()
	require.NoError(t, err)
	require.NotNil(t, tgw.TransitGateway.Span)
	require.Equal(t, "NONE", *tgw.TransitGateway.ExternalIpSignalingMode)

	profile, err := (ConnectivityProfileRequest{ID: "migration-profile", ProjectID: "default", TransitGatewayID: "migration-tgw"}).evaluateSpec()
	require.NoError(t, err)
	require.Empty(t, profile.ConnectivityProfile.Ipv6Blocks)
	require.Equal(t, "/orgs/default/projects/default/transit-gateways/migration-tgw", *profile.ConnectivityProfile.TransitGatewayPath)

	attachment, err := (VPCAttachmentRequest{ID: "default", ProjectID: "default", VPCID: "migration-vpc", ConnectivityProfileID: "migration-profile"}).evaluateSpec()
	require.NoError(t, err)
	require.Equal(t, "/orgs/default/projects/default/vpc-connectivity-profiles/migration-profile", *attachment.VPCAttachment.VpcConnectivityProfile)

	subnet, err := (VPCSubnetRequest{ID: "migration-subnet", ProjectID: "default", VPCID: "migration-vpc", DVCID: "migration-dvc", IPBlockIDs: []string{"workload-block"}}).deriveSpec()
	require.NoError(t, err)
	require.Equal(t, model.VpcSubnet_ACCESS_MODE_PUBLIC, *subnet.VPCSubnet.AccessMode)
	require.Equal(t, "IPV4", *subnet.VPCSubnet.IpAddressType)
	require.Nil(t, subnet.VPCSubnet.Ipv6PrefixLength)
	require.Equal(t, "/infra/distributed-vlan-connections/migration-dvc", *subnet.VPCSubnet.VlanConnection)
	require.Equal(t, []string{"/infra/ip-blocks/workload-block"}, subnet.VPCSubnet.IpBlocks)

	ipBlock, err := (IPBlockRequest{ID: "workload-block", CIDRs: []string{"192.0.2.0/24"}}).evaluateSpec()
	require.NoError(t, err)
	require.Equal(t, "IPV4", *ipBlock.IPBlock.IpAddressType)

	dvc, err := (DVCRequest{ID: "migration-dvc", VLANID: 10, Availability: Availability{ZoneExternalIDs: []string{"zone-a"}}}).deriveSpec()
	require.NoError(t, err)
	require.NoError(t, dvc.Validate())
	require.Equal(t, int64(10), *dvc.DVC.VlanId)
	require.Equal(t, model.DistributedVlanConnection_SUBNET_EXTENSION_CONNECTION_ENABLED_L2_AND_L3, *dvc.DVC.SubnetExtensionConnection)
}

func TestResourceRequestsRejectIPv6(t *testing.T) {
	tests := []struct {
		name    string
		request interface{ evaluateSpec() (BackendSpec, error) }
	}{
		{name: "VPC private IP", request: VPCRequest{ID: "vpc", PrivateIPs: []string{"2001:db8::/64"}}},
		{name: "attachment SNAT IP", request: VPCAttachmentRequest{ID: "attachment", PreferredDefaultSNATIP: "2001:db8::1"}},
		{name: "IP block CIDR", request: IPBlockRequest{ID: "block", CIDRs: []string{"2001:db8::/64"}}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := testCase.request.evaluateSpec()
			require.ErrorContains(t, err, "IPv4")
		})
	}

	_, err := (VPCSubnetRequest{ID: "subnet", IPAddresses: []string{"2001:db8::1"}}).deriveSpec()
	require.ErrorContains(t, err, "IPv4")
	_, err = (DVCRequest{ID: "dvc", GatewayAddresses: []string{"2001:db8::1/64"}}).deriveSpec()
	require.ErrorContains(t, err, "IPv4")
}

func TestEnsureWaitsForRealization(t *testing.T) {
	b := &backend{pending: true}
	s := &Service{Backend: b}
	_, err := s.EnsureEvaluate(context.Background(), "infra", vpcRequest())
	require.ErrorIs(t, err, ErrPending)
	b.pending = false
	record, err := s.EnsureEvaluate(context.Background(), "infra", vpcRequest())
	require.NoError(t, err)
	require.Equal(t, "lb", record.Path[len(record.Path)-2:])
	require.Equal(t, 1, b.writes)
}

func TestSNATStatusAcceptanceIsDurableAndImmutable(t *testing.T) {
	c, obj := setup(t)
	status := &KubernetesReservationStatus{Client: c, Reader: c}
	r := Reservation{IP: "192.0.2.10", Namespace: SNATNamespace, Name: "snat"}
	require.NoError(t, status.Accept(context.Background(), obj, r))
	fresh := &api.ControlPlaneNetworkTransition{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(obj), fresh))
	require.Equal(t, r.IP, fresh.Status.LBSNATIPAllocations[0].IPAddress)
	require.NoError(t, status.Accept(context.Background(), obj, r))
	r.IP = "192.0.2.11"
	require.ErrorContains(t, status.Accept(context.Background(), obj, r), "cannot be replaced")
}
