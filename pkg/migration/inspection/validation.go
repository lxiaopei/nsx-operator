/* Copyright © 2026 Broadcom, Inc. All Rights Reserved.
   SPDX-License-Identifier: Apache-2.0 */

// Package inspection implements read-only source network inspection shared by
// the control-plane and namespace Evaluate handlers.
package inspection

import (
	"fmt"
	"net"
	"net/netip"

	api "github.com/vmware-tanzu/nsx-operator/pkg/apis/migration/v1alpha1"
)

// Addressing is validated source configuration, not an inventory of free IPs.
// Ranges may be DHCP or static pools; existing addresses need only lie in CIDR.
type Addressing struct {
	CIDR    netip.Prefix
	Gateway netip.Addr
	Ranges  []api.IPAddressRange
}

func ipv4(value string) (netip.Addr, error) {
	a, err := netip.ParseAddr(value)
	if err != nil || !a.Is4() || a.IsUnspecified() || a.IsMulticast() {
		return netip.Addr{}, fmt.Errorf("%q must be a unicast IPv4 address", value)
	}
	return a, nil
}

// ValidateAddressing checks supplied/discovered values without filling missing
// fields. CIDR is derived from the first range start and mask. The current
// source subnetMask contract is IPv4-only; there is no administrator CIDR input.
func ValidateAddressing(mask, gateway string, ranges []api.IPAddressRange) (Addressing, error) {
	var result Addressing
	m, err := netip.ParseAddr(mask)
	if err != nil || !m.Is4() {
		return result, fmt.Errorf("subnetMask must be a contiguous IPv4 mask")
	}
	b := m.As4()
	ones, bits := net.IPMask(b[:]).Size()
	if bits != 32 {
		return result, fmt.Errorf("subnetMask must be contiguous")
	}
	g, err := ipv4(gateway)
	if err != nil {
		return result, fmt.Errorf("gatewayAddress: %w", err)
	}
	if len(ranges) == 0 {
		return result, fmt.Errorf("at least one source address range is required")
	}
	start, err := ipv4(ranges[0].Start)
	if err != nil {
		return result, fmt.Errorf("range start: %w", err)
	}
	prefix := netip.PrefixFrom(start, ones).Masked()
	usable := func(a netip.Addr) bool {
		if !prefix.Contains(a) {
			return false
		}
		// /31 and /32 have no network/broadcast exclusion (RFC 3021).
		if ones >= 31 {
			return true
		}
		x := a.As4()
		base := prefix.Addr().As4()
		broadcast := true
		for i := range x {
			broadcast = broadcast && x[i] == base[i]|^b[i]
		}
		return a != prefix.Addr() && !broadcast
	}
	if !usable(g) {
		return result, fmt.Errorf("gatewayAddress is not a usable address in %s", prefix)
	}
	for _, r := range ranges {
		s, e1 := ipv4(r.Start)
		e, e2 := ipv4(r.End)
		if e1 != nil || e2 != nil || s.Compare(e) > 0 || !usable(s) || !usable(e) {
			return result, fmt.Errorf("range %s-%s must be ordered usable IPv4 addresses within %s", r.Start, r.End, prefix)
		}
		if s.Compare(g) <= 0 && g.Compare(e) <= 0 {
			return result, fmt.Errorf("gatewayAddress overlaps source allocation range")
		}
	}
	return Addressing{CIDR: prefix, Gateway: g, Ranges: append([]api.IPAddressRange(nil), ranges...)}, nil
}

// ValidateExistingAddresses deliberately does not require membership in a pool.
func (a Addressing) ValidateExistingAddresses(addresses []string) error {
	for _, value := range addresses {
		ip, err := ipv4(value)
		if err != nil || !a.CIDR.Contains(ip) {
			return fmt.Errorf("existing address %q is outside %s", value, a.CIDR)
		}
	}
	return nil
}
