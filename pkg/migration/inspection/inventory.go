/* Copyright © 2026 Broadcom, Inc. All Rights Reserved.
   SPDX-License-Identifier: Apache-2.0 */

package inspection

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/netip"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	netop "github.com/vmware-tanzu/net-operator-api/api/v1alpha1"

	api "github.com/vmware-tanzu/nsx-operator/pkg/apis/migration/v1alpha1"
)

var netAPIVersion = netop.SchemeGroupVersion.String()

type TopologyReader interface {
	ResolvePortGroup(context.Context, string) (Topology, error)
}

// +kubebuilder:rbac:groups=netoperator.vmware.com,resources=networks;vspheredistributednetworks;ippools,verbs=get;list

// KubernetesInventory reads the source Net Operator v1alpha1 contract. A direct
// API reader is recommended so Evaluate/Plan rechecks do not reuse stale caches.
// Register netop.AddToScheme on the reader's scheme before use.
type KubernetesInventory struct {
	Reader   client.Reader
	Topology TopologyReader
}

func (k KubernetesInventory) NamespaceVDNs(ctx context.Context, namespaces []string) ([]string, error) {
	if k.Reader == nil || len(namespaces) == 0 {
		return nil, fmt.Errorf("reader and namespace batch are required")
	}
	set := map[string]bool{}
	for _, namespace := range namespaces {
		if namespace == "" {
			return nil, fmt.Errorf("namespace must not be empty")
		}
		list := &netop.NetworkList{}
		if err := k.Reader.List(ctx, list, client.InNamespace(namespace)); err != nil {
			return nil, err
		}
		if len(list.Items) == 0 {
			return nil, invalid("namespace %s has no source Networks", namespace)
		}
		for _, item := range list.Items {
			spec := item.Spec
			ref := spec.ProviderRef
			if spec.Type != netop.NetworkTypeVDS || ref.APIGroup != netop.GroupName || ref.Kind != "VSphereDistributedNetwork" || ref.Name == "" || ref.Namespace != "" || (ref.APIVersion != "" && ref.APIVersion != netAPIVersion && ref.APIVersion != "v1alpha1") {
				return nil, invalid("Network %s/%s has unsupported source provider", namespace, item.GetName())
			}
			set[ref.Name] = true
		}
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func (k KubernetesInventory) Resolve(ctx context.Context, name string) (Source, error) {
	s := Source{VDNName: name}
	if k.Reader == nil {
		return s, fmt.Errorf("Kubernetes reader is required")
	}
	vdn := &netop.VSphereDistributedNetwork{}
	if err := k.Reader.Get(ctx, client.ObjectKey{Name: name}, vdn); err != nil {
		return s, err
	}
	spec := vdn.Spec
	if spec.PortGroupID == "" || k.Topology == nil {
		return s, fmt.Errorf("port group and topology reader are required")
	}
	topology, err := k.Topology.ResolvePortGroup(ctx, spec.PortGroupID)
	if err != nil {
		return s, err
	}
	s.Topology = topology
	switch spec.IPAssignmentMode {
	case netop.IPAssignmentModeDHCP:
		if len(spec.IPPools) != 0 || spec.Gateway != "" || spec.SubnetMask != "" {
			return s, invalid("DHCP VDN has conflicting static configuration")
		}
		s.DHCP = true
	case "", netop.IPAssignmentModeStaticPool: // Documented Net Operator default is staticpool.
		s.Mask, s.Gateway = spec.SubnetMask, spec.Gateway
		if len(spec.IPPools) == 0 {
			return s, invalid("static VDN has no IP pools")
		}
		for _, ref := range spec.IPPools {
			if ref.Name == "" || (ref.APIVersion != "" && ref.APIVersion != "v1alpha1" && ref.APIVersion != netAPIVersion) {
				return s, invalid("invalid IPPool reference")
			}
			pool := &netop.IPPool{}
			if err := k.Reader.Get(ctx, client.ObjectKey{Name: ref.Name}, pool); err != nil {
				return s, err
			}
			a, err := ipv4(pool.Spec.StartingAddress)
			if err != nil || pool.Spec.AddressCount < 1 || pool.Spec.AddressCount > 1<<32 {
				return s, invalid("IPPool %s has invalid IPv4 range", ref.Name)
			}
			b := a.As4()
			end := uint64(binary.BigEndian.Uint32(b[:])) + uint64(pool.Spec.AddressCount) - 1
			if end > 1<<32-1 {
				return s, invalid("IPPool %s range overflows IPv4", ref.Name)
			}
			binary.BigEndian.PutUint32(b[:], uint32(end))
			s.Ranges = append(s.Ranges, api.IPAddressRange{Start: a.String(), End: netip.AddrFrom4(b).String()})
		}
		sort.Slice(s.Ranges, func(i, j int) bool { return s.Ranges[i].Start < s.Ranges[j].Start })
	default:
		return s, invalid("unsupported IP assignment mode %q", spec.IPAssignmentMode)
	}
	return s, nil
}
