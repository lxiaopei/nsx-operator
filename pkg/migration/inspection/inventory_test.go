/* Copyright © 2026 Broadcom, Inc. All Rights Reserved.
   SPDX-License-Identifier: Apache-2.0 */

package inspection

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	netop "github.com/vmware-tanzu/net-operator-api/api/v1alpha1"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type topologyStub struct{}

func (topologyStub) ResolvePortGroup(_ context.Context, id string) (Topology, error) {
	t := source("a").Topology
	t.PortGroupID = id
	return t, nil
}
func TestKubernetesInventory(t *testing.T) {
	ctx := context.Background()
	network := func(name, ns string) *netop.Network {
		return &netop.Network{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: netop.NetworkSpec{Type: netop.NetworkTypeVDS, ProviderRef: netop.NetworkProviderReference{APIGroup: netop.GroupName, Kind: "VSphereDistributedNetwork", Name: "a"}}}
	}
	vdn := &netop.VSphereDistributedNetwork{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Spec: netop.VSphereDistributedNetworkSpec{PortGroupID: "dvportgroup-1", IPAssignmentMode: netop.IPAssignmentModeDHCP}}
	scheme := runtime.NewScheme()
	require.NoError(t, netop.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(network("n1", "ns1"), network("n2", "ns2"), vdn).Build()
	k := KubernetesInventory{Reader: c, Topology: topologyStub{}}
	names, err := k.NamespaceVDNs(ctx, []string{"ns2", "ns1"})
	require.NoError(t, err)
	require.Equal(t, []string{"a"}, names)
	s, err := k.Resolve(ctx, "a")
	require.NoError(t, err)
	require.True(t, s.DHCP)
	require.Equal(t, "dvportgroup-1", s.Topology.PortGroupID)
	_, err = k.NamespaceVDNs(ctx, []string{"missing"})
	require.ErrorAs(t, err, new(*ConfigurationError))
	vdn.Spec = netop.VSphereDistributedNetworkSpec{PortGroupID: "dvportgroup-1", Gateway: "192.168.10.1", SubnetMask: "255.255.255.0", IPPools: []netop.IPPoolReference{{Name: "pool"}}}
	require.NoError(t, c.Update(ctx, vdn))
	pool := &netop.IPPool{ObjectMeta: metav1.ObjectMeta{Name: "pool"}, Spec: netop.IPPoolSpec{StartingAddress: "192.168.10.100", AddressCount: 101}}
	require.NoError(t, c.Create(ctx, pool))
	s, err = k.Resolve(ctx, "a")
	require.NoError(t, err)
	require.False(t, s.DHCP)
	require.Equal(t, "192.168.10.200", s.Ranges[0].End)
	r, err := (Service{Inventory: k}).InspectNamespaces(ctx, []string{"ns1", "ns2"}, nil)
	require.NoError(t, err)
	require.True(t, r.Ready())
	pool.Spec = netop.IPPoolSpec{StartingAddress: "255.255.255.250", AddressCount: 10}
	require.NoError(t, c.Update(ctx, pool))
	_, err = k.Resolve(ctx, "a")
	require.ErrorContains(t, err, "overflows")
	vdn.Spec = netop.VSphereDistributedNetworkSpec{PortGroupID: "dvportgroup-1", IPAssignmentMode: netop.IPAssignmentModeNone}
	require.NoError(t, c.Update(ctx, vdn))
	_, err = k.Resolve(ctx, "a")
	require.ErrorAs(t, err, new(*ConfigurationError))
}

func TestVCenterTopologyWithSimulator(t *testing.T) {
	model := simulator.VPX()
	model.Host = 0
	simulator.Test(func(ctx context.Context, c *vim25.Client) {
		finder := find.NewFinder(c, true)
		dc, err := finder.DefaultDatacenter(ctx)
		require.NoError(t, err)
		finder.SetDatacenter(dc)
		groups, err := finder.NetworkList(ctx, "*")
		require.NoError(t, err)
		require.NotEmpty(t, groups)
		reader := VCenterTopology{Properties: property.DefaultCollector(c)}
		var groupID string
		for _, group := range groups {
			if group.Reference().Type == "DistributedVirtualPortgroup" {
				groupID = group.Reference().Value
				break
			}
		}
		require.NotEmpty(t, groupID)
		topology, err := reader.ResolvePortGroup(ctx, groupID)
		require.NoError(t, err)
		require.NotEmpty(t, topology.SwitchID)
		require.NotEmpty(t, topology.SwitchUUID)
		require.NotEmpty(t, topology.ClusterIDs)
		_, err = reader.ResolvePortGroup(ctx, "dvportgroup-does-not-exist")
		require.Error(t, err)
	}, model)
}
