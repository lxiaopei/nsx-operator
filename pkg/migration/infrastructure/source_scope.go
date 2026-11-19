/* Copyright © 2026 Broadcom, Inc. All Rights Reserved.
   SPDX-License-Identifier: Apache-2.0 */

package infrastructure

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"

	netop "github.com/vmware-tanzu/net-operator-api/api/v1alpha1"
	"github.com/vmware/govmomi/vapi/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/vmware-tanzu/nsx-operator/pkg/migration/inspection"
)

// SupervisorNetworkAPI returns the DVPG IDs configured for the Supervisor's
// control-plane and workload networks.
type SupervisorNetworkAPI interface {
	PortGroupIDs(context.Context) ([]string, error)
}

// VCenterSupervisorNetworks reads the vCenter Supervisor Networks API.
type VCenterSupervisorNetworks struct {
	Client       *rest.Client
	SupervisorID string
}

type supervisorNetworkSummary struct {
	Network string `json:"network"`
}

type supervisorNetworkInfo struct {
	Provider       string `json:"network_provider"`
	VSphereNetwork *struct {
		PortGroup string `json:"portgroup"`
	} `json:"vsphere_network"`
}

// PortGroupIDs lists Supervisor network backings and control-plane management
// port groups. Network IDs are resolved through the API instead of being
// mistaken for DVPG IDs.
func (reader VCenterSupervisorNetworks) PortGroupIDs(ctx context.Context) ([]string, error) {
	if reader.Client == nil || reader.SupervisorID == "" {
		return nil, fmt.Errorf("vCenter REST client and Supervisor ID are required")
	}
	basePath := "/api/vcenter/namespace-management/supervisors/" + url.PathEscape(reader.SupervisorID)
	portGroups := make(map[string]struct{})
	var controlPlane struct {
		NetworkSegmentsInfo *struct {
			PortGroups []string `json:"port_groups"`
		} `json:"network_segments_info"`
	}
	if err := reader.get(ctx, basePath+"/control-plane/networks/settings", &controlPlane); err != nil {
		return nil, fmt.Errorf("read Supervisor control-plane networks: %w", err)
	}
	if controlPlane.NetworkSegmentsInfo != nil {
		for _, portGroupID := range controlPlane.NetworkSegmentsInfo.PortGroups {
			if portGroupID != "" {
				portGroups[portGroupID] = struct{}{}
			}
		}
	}

	var networkList struct {
		Networks []supervisorNetworkSummary `json:"networks"`
	}
	if err := reader.get(ctx, basePath+"/networks", &networkList); err != nil {
		return nil, fmt.Errorf("list Supervisor networks: %w", err)
	}
	for _, summary := range networkList.Networks {
		if summary.Network == "" {
			return nil, fmt.Errorf("Supervisor network list contains an empty network ID")
		}
		var network supervisorNetworkInfo
		path := basePath + "/networks/" + url.PathEscape(summary.Network)
		if err := reader.get(ctx, path, &network); err != nil {
			return nil, fmt.Errorf("read Supervisor network %q: %w", summary.Network, err)
		}
		if network.Provider != "VSPHERE_NETWORK" || network.VSphereNetwork == nil || network.VSphereNetwork.PortGroup == "" {
			continue
		}
		portGroups[network.VSphereNetwork.PortGroup] = struct{}{}
	}

	result := make([]string, 0, len(portGroups))
	for portGroupID := range portGroups {
		result = append(result, portGroupID)
	}
	slices.Sort(result)
	if len(result) == 0 {
		return nil, fmt.Errorf("Supervisor API returned no VDS port-group backings")
	}
	return result, nil
}

func (reader VCenterSupervisorNetworks) get(ctx context.Context, path string, response any) error {
	request := reader.Client.Resource(path).Request(http.MethodGet)
	return reader.Client.Do(ctx, request, response)
}

// VCenterControlPlaneScope maps WCP-reported DVPG backings to Net Operator
// VDNs and resolves their source topology for SNAT network comparisons.
type VCenterControlPlaneScope struct {
	Networks  SupervisorNetworkAPI
	Reader    client.Reader
	Inventory inspection.Inventory
}

func (scope VCenterControlPlaneScope) SourceNetworks(ctx context.Context) ([]string, []inspection.Topology, error) {
	if scope.Networks == nil || scope.Reader == nil || scope.Inventory == nil {
		return nil, nil, fmt.Errorf("Supervisor network API, Net Operator reader and source inventory are required")
	}
	portGroupIDs, err := scope.Networks.PortGroupIDs(ctx)
	if err != nil {
		return nil, nil, err
	}
	var vdns netop.VSphereDistributedNetworkList
	if err := scope.Reader.List(ctx, &vdns); err != nil {
		return nil, nil, fmt.Errorf("list Net Operator VDNs: %w", err)
	}
	backings := make(map[string][]string)
	for _, vdn := range vdns.Items {
		if vdn.Name != "" && vdn.Spec.PortGroupID != "" && vdn.DeletionTimestamp.IsZero() {
			backings[vdn.Spec.PortGroupID] = append(backings[vdn.Spec.PortGroupID], vdn.Name)
		}
	}

	vdnSet := make(map[string]struct{})
	for _, portGroupID := range portGroupIDs {
		matchedVDNs := backings[portGroupID]
		if len(matchedVDNs) == 0 {
			return nil, nil, fmt.Errorf("Supervisor DVPG %q has no Net Operator VDN", portGroupID)
		}
		for _, vdnName := range matchedVDNs {
			vdnSet[vdnName] = struct{}{}
		}
	}

	vdnNames := make([]string, 0, len(vdnSet))
	for vdnName := range vdnSet {
		vdnNames = append(vdnNames, vdnName)
	}
	slices.Sort(vdnNames)

	var workloadTopologies []inspection.Topology
	for _, vdnName := range vdnNames {
		discovered, err := scope.Inventory.Resolve(ctx, vdnName)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve Supervisor VDN %q: %w", vdnName, err)
		}
		workloadTopologies = append(workloadTopologies, discovered.Topologies...)
	}
	return vdnNames, workloadTopologies, nil
}
