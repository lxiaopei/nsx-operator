/*
Copyright © 2026 Broadcom, Inc. All Rights Reserved.

	SPDX-License-Identifier: Apache-2.0
*/
package migration

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	vapierrors "github.com/vmware/vsphere-automation-sdk-go/lib/vapi/std/errors"
	"github.com/vmware/vsphere-automation-sdk-go/services/nsxt/model"
	"github.com/vmware/vsphere-automation-sdk-go/services/nsxt/orgs/projects"
	"github.com/vmware/vsphere-automation-sdk-go/services/nsxt/orgs/projects/vpcs"
	"k8s.io/utils/ptr"

	"github.com/vmware-tanzu/nsx-operator/pkg/migration/fabric"
	"github.com/vmware-tanzu/nsx-operator/pkg/nsx"
)

type fabricVPCClient struct {
	projects.VpcsClient
	get    func() (model.Vpc, error)
	update func(model.Vpc) (model.Vpc, error)
}

type fabricSubnetClient struct {
	vpcs.SubnetsClient
	get    func() (model.VpcSubnet, error)
	update func(model.VpcSubnet) (model.VpcSubnet, error)
}

func (c fabricSubnetClient) Get(string, string, string, string) (model.VpcSubnet, error) {
	return c.get()
}
func (c fabricSubnetClient) Update(_, _, _, _ string, v model.VpcSubnet) (model.VpcSubnet, error) {
	return c.update(v)
}

func (c fabricVPCClient) Get(string, string, string) (model.Vpc, error)         { return c.get() }
func (c fabricVPCClient) Update(_, _, _ string, v model.Vpc) (model.Vpc, error) { return c.update(v) }
func fabricSpec() fabric.Spec {
	return fabric.Spec{Kind: fabric.VPC, ID: "lb", ProjectID: "default", VPC: &model.Vpc{PrivateIps: []string{"10.0.0.0/24"}}}
}
func currentVPC() model.Vpc {
	return model.Vpc{Id: ptr.To("lb"), DisplayName: ptr.To("lb"), Path: ptr.To("/orgs/default/projects/default/vpcs/lb"), UniqueId: ptr.To("uid"), Revision: ptr.To(int64(5)), Tags: []model.Tag{{Scope: ptr.To(fabricOwnerScope), Tag: ptr.To("infra")}}, Description: ptr.To("preserve")}
}

func TestFabricSDKPreservesMetadataAndRevision(t *testing.T) {
	writes := 0
	b := &FabricBackend{Service: &Service{client: &nsx.Client{VPCClient: fabricVPCClient{
		get: func() (model.Vpc, error) { return currentVPC(), nil },
		update: func(v model.Vpc) (model.Vpc, error) {
			writes++
			require.Equal(t, int64(5), *v.Revision)
			require.Equal(t, "preserve", *v.Description)
			require.Equal(t, []string{"10.0.0.0/24"}, v.PrivateIps)
			require.Equal(t, "lb", *v.DisplayName)
			return v, nil
		},
	}}}}
	require.NoError(t, b.Apply(context.Background(), fabricSpec(), "infra"))
	require.Equal(t, 1, writes)
	require.ErrorContains(t, b.Apply(context.Background(), fabricSpec(), "another-owner"), "another controller")
	require.Equal(t, 1, writes)
}

func TestFabricSDKReadFailuresNeverCreate(t *testing.T) {
	for _, err := range []error{vapierrors.Unauthorized{}, errors.New("timeout")} {
		b := &FabricBackend{Service: &Service{client: &nsx.Client{VPCClient: fabricVPCClient{get: func() (model.Vpc, error) { return model.Vpc{}, err }, update: func(model.Vpc) (model.Vpc, error) { t.Fatal("must not write"); return model.Vpc{}, nil }}}}}
		require.Error(t, b.Apply(context.Background(), fabricSpec(), "infra"))
	}
}

func TestFabricSDKCreateUsesStableIdentity(t *testing.T) {
	b := &FabricBackend{Service: &Service{client: &nsx.Client{VPCClient: fabricVPCClient{
		get: func() (model.Vpc, error) { return model.Vpc{}, vapierrors.NotFound{} },
		update: func(v model.Vpc) (model.Vpc, error) {
			require.Equal(t, "lb", *v.Id)
			require.Equal(t, "lb", *v.DisplayName)
			require.Nil(t, v.Revision)
			require.Equal(t, "infra", *v.Tags[0].Tag)
			return v, nil
		},
	}}}}
	require.NoError(t, b.Apply(context.Background(), fabricSpec(), "infra"))
}

func TestFabricSDKUsesIDAsSubnetDisplayName(t *testing.T) {
	spec := fabric.Spec{Kind: fabric.VPCSubnet, ID: "tenant-subnet", DisplayName: "tenant-subnet", ProjectID: "default", VPCID: "tenant-vpc", VPCSubnet: &model.VpcSubnet{AccessMode: ptr.To(model.VpcSubnet_ACCESS_MODE_PUBLIC), VlanConnection: ptr.To("/infra/distributed-vlan-connections/tenant-dvc")}}
	actual := model.VpcSubnet{Id: ptr.To(spec.ID), DisplayName: ptr.To(spec.ID), Path: ptr.To(spec.Path()), UniqueId: ptr.To("subnet-uid"), Revision: ptr.To(int64(3)), AccessMode: ptr.To(model.VpcSubnet_ACCESS_MODE_PUBLIC), VlanConnection: ptr.To("/infra/distributed-vlan-connections/tenant-dvc"), Tags: []model.Tag{{Scope: ptr.To(fabricOwnerScope), Tag: ptr.To("infra")}}}
	writes := 0
	b := &FabricBackend{Service: &Service{client: &nsx.Client{SubnetsClient: fabricSubnetClient{
		get: func() (model.VpcSubnet, error) { return actual, nil },
		update: func(v model.VpcSubnet) (model.VpcSubnet, error) {
			writes++
			require.Equal(t, int64(3), *v.Revision)
			require.Equal(t, spec.ID, *v.Id)
			require.Equal(t, spec.ID, *v.DisplayName)
			return v, nil
		},
	}}}}
	state, err := fabricState(spec, actual)
	require.NoError(t, err)
	require.True(t, state.Compatible)
	actual.DisplayName = ptr.To("wrong-name")
	state, err = fabricState(spec, actual)
	require.NoError(t, err)
	require.False(t, state.Compatible, "display name is part of the requested resource identity contract")
	actual.DisplayName = ptr.To(spec.ID)
	actual.AccessMode = ptr.To(model.VpcSubnet_ACCESS_MODE_PRIVATE)
	require.NoError(t, b.Apply(context.Background(), spec, "infra"))
	require.Equal(t, 1, writes)
}

func TestFabricSDKMetadataAndCompatibility(t *testing.T) {
	spec := fabricSpec()
	v := currentVPC()
	state, err := fabricState(spec, v)
	require.NoError(t, err)
	require.False(t, state.Compatible)
	v.PrivateIps = spec.VPC.PrivateIps
	state, err = fabricState(spec, v)
	require.NoError(t, err)
	require.True(t, state.Compatible)
	v.MarkedForDelete = ptr.To(true)
	_, err = fabricState(spec, v)
	require.ErrorContains(t, err, "deleted")
	v = currentVPC()
	v.Path = ptr.To("/wrong")
	_, err = fabricState(spec, v)
	require.ErrorContains(t, err, "identity")
}

func TestFabricSDKDefaultsAndSetOrdering(t *testing.T) {
	spec := fabricSpec()
	spec.VPC.PrivateIps = []string{"10.0.0.0/24", "10.0.1.0/24"}
	v := currentVPC()
	v.PrivateIps = []string{"10.0.1.0/24", "10.0.0.0/24"}
	v.IpAddressType = ptr.To("IPV4")
	state, err := fabricState(spec, v)
	require.NoError(t, err)
	require.True(t, state.Compatible)
	spec.VPC.IpAddressType = ptr.To("IPV4_IPV6")
	state, err = fabricState(spec, v)
	require.NoError(t, err)
	require.False(t, state.Compatible)
}

func TestFabricSDKPreservesDefaultTransitSubnets(t *testing.T) {
	spec := fabric.Spec{Kind: fabric.TransitGateway, ID: "tgw", ProjectID: "default", TransitGateway: &model.TransitGateway{ExternalIpSignalingMode: ptr.To("NONE")}}
	actual := model.TransitGateway{Id: ptr.To(spec.ID), Path: ptr.To(spec.Path()), UniqueId: ptr.To("tgw-uid"), ExternalIpSignalingMode: ptr.To("NONE"), TransitSubnets: []string{"100.64.0.0/21"}}
	state, err := fabricState(spec, actual)
	require.NoError(t, err)
	require.True(t, state.Compatible, "omission must preserve NSX-supplied transit subnets")
	spec.TransitGateway.TransitSubnets = []string{}
	state, err = fabricState(spec, actual)
	require.NoError(t, err)
	require.False(t, state.Compatible, "explicitly empty requests a change")
}

func TestFabricSDKRejectsMissingOrReplacedCheckpoint(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			b := &FabricBackend{Service: &Service{client: &nsx.Client{VPCClient: fabricVPCClient{
				get: func() (model.Vpc, error) {
					if missing {
						return model.Vpc{}, vapierrors.NotFound{}
					}
					return currentVPC(), nil
				},
				update: func(model.Vpc) (model.Vpc, error) {
					t.Fatal("must not mutate a missing or replaced resource")
					return model.Vpc{}, nil
				},
			}}}}
			spec := fabricSpec()
			spec.ExpectedUniqueID = "original-uid"
			require.Error(t, b.Apply(context.Background(), spec, "infra"))
		})
	}
}
