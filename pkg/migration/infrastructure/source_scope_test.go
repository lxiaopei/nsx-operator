/* Copyright © 2026 Broadcom, Inc. All Rights Reserved.
   SPDX-License-Identifier: Apache-2.0 */

package infrastructure

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
	netop "github.com/vmware-tanzu/net-operator-api/api/v1alpha1"
	"github.com/vmware/govmomi/vapi/rest"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/soap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/vmware-tanzu/nsx-operator/pkg/migration/inspection"
)

func TestVCenterSupervisorNetworksReturnsUniqueDVPGIDs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/vcenter/namespace-management/supervisors/supervisor-1/control-plane/networks/settings":
			_, _ = w.Write([]byte(`{"backing":"NETWORK_SEGMENTS","network_segments_info":{"port_groups":["dvpg-cp","dvpg-shared"]}}`))
		case "/api/vcenter/namespace-management/supervisors/supervisor-1/networks":
			_, _ = w.Write([]byte(`{"networks":[{"network":"network-1"},{"network":"network-2"}]}`))
		case "/api/vcenter/namespace-management/supervisors/supervisor-1/networks/network-1":
			_, _ = w.Write([]byte(`{"network_provider":"VSPHERE_NETWORK","vsphere_network":{"portgroup":"dvpg-workload"}}`))
		case "/api/vcenter/namespace-management/supervisors/supervisor-1/networks/network-2":
			_, _ = w.Write([]byte(`{"network_provider":"VSPHERE_NETWORK","vsphere_network":{"portgroup":"dvpg-shared"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	endpoint, err := url.Parse(server.URL)
	require.NoError(t, err)
	client := rest.NewClient(&vim25.Client{Client: soap.NewClient(endpoint, false)})
	reader := VCenterSupervisorNetworks{Client: client, SupervisorID: "supervisor-1"}

	portGroupIDs, err := reader.PortGroupIDs(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"dvpg-cp", "dvpg-shared", "dvpg-workload"}, portGroupIDs)
}

type scopeInventoryStub struct{}

func (scopeInventoryStub) NamespaceVDNs(context.Context, []string) ([]string, error) {
	return nil, nil
}

func (scopeInventoryStub) Resolve(_ context.Context, vdnName string) (inspection.DiscoveredSource, error) {
	return inspection.DiscoveredSource{Source: inspection.Source{VDNName: vdnName, Topologies: []inspection.Topology{{PortGroupID: "dvpg-" + vdnName}}}}, nil
}

type scopeNetworksStub struct{ portGroupIDs []string }

func (stub scopeNetworksStub) PortGroupIDs(context.Context) ([]string, error) {
	return stub.portGroupIDs, nil
}

func TestVCenterControlPlaneScopeMapsBackingsToVDNs(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, netop.AddToScheme(scheme))
	vdnA := &netop.VSphereDistributedNetwork{ObjectMeta: metav1.ObjectMeta{Name: "network-a"}, Spec: netop.VSphereDistributedNetworkSpec{PortGroupID: "dvpg-a"}}
	vdnB := &netop.VSphereDistributedNetwork{ObjectMeta: metav1.ObjectMeta{Name: "network-b"}, Spec: netop.VSphereDistributedNetworkSpec{PortGroupID: "dvpg-b"}}
	kubeReader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vdnA, vdnB).Build()
	scope := VCenterControlPlaneScope{
		Networks:  scopeNetworksStub{portGroupIDs: []string{"dvpg-b", "dvpg-a"}},
		Reader:    kubeReader,
		Inventory: scopeInventoryStub{},
	}

	vdnNames, topologies, err := scope.SourceNetworks(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"network-a", "network-b"}, vdnNames)
	require.Equal(t, []inspection.Topology{{PortGroupID: "dvpg-network-a"}, {PortGroupID: "dvpg-network-b"}}, topologies)
}
