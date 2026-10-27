/* Copyright © 2026 Broadcom, Inc. All Rights Reserved.
   SPDX-License-Identifier: Apache-2.0 */

package inspection

import (
	"context"
	"fmt"
	"sort"

	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// PropertyReader is implemented by govmomi's property.Collector. The caller owns
// vCenter authentication/session lifecycle; inspection only retrieves properties.
type PropertyReader interface {
	RetrieveOne(context.Context, types.ManagedObjectReference, []string, any) error
}

type VCenterTopology struct{ Properties PropertyReader }

func (v VCenterTopology) ResolvePortGroup(ctx context.Context, id string) (Topology, error) {
	t := Topology{PortGroupID: id}
	if v.Properties == nil || id == "" {
		return t, fmt.Errorf("vCenter property reader and port group ID are required")
	}
	var pg mo.DistributedVirtualPortgroup
	if err := v.Properties.RetrieveOne(ctx, types.ManagedObjectReference{Type: "DistributedVirtualPortgroup", Value: id}, []string{"config.distributedVirtualSwitch"}, &pg); err != nil {
		return t, err
	}
	if pg.Config.DistributedVirtualSwitch == nil {
		return t, invalid("port group %s has no distributed switch", id)
	}
	ref := *pg.Config.DistributedVirtualSwitch
	t.SwitchID = ref.Value
	var dvs mo.DistributedVirtualSwitch
	if err := v.Properties.RetrieveOne(ctx, ref, []string{"uuid", "summary.hostMember"}, &dvs); err != nil {
		return t, err
	}
	t.SwitchUUID = dvs.Uuid
	if dvs.Uuid == "" {
		return t, invalid("switch %s has incomplete configuration", ref.Value)
	}
	clusters := map[string]bool{}
	for _, member := range dvs.Summary.HostMember {
		if member.Type != "HostSystem" || member.Value == "" {
			return t, invalid("switch %s has a host member without an identity", ref.Value)
		}
		var host mo.HostSystem
		if err := v.Properties.RetrieveOne(ctx, member, []string{"parent"}, &host); err != nil {
			return t, err
		}
		if host.Parent == nil || host.Parent.Type != "ClusterComputeResource" {
			return t, invalid("switch host %s is not attached to a cluster", member.Value)
		}
		clusters[host.Parent.Value] = true
	}
	if len(clusters) == 0 {
		return t, invalid("switch %s has no attached clusters", ref.Value)
	}
	for id := range clusters {
		t.ClusterIDs = append(t.ClusterIDs, id)
	}
	sort.Strings(t.ClusterIDs)
	return t, nil
}
