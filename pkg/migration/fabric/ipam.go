/*
Copyright © 2026 Broadcom, Inc. All Rights Reserved.

	SPDX-License-Identifier: Apache-2.0
*/
package fabric

import (
	"errors"
	"fmt"
	"net/netip"
	"sort"
)

// AddressRange is an inclusive, single-family range discovered from source IPAM.
type AddressRange struct {
	Start string
	End   string
}

// PartitionVIPRanges removes accepted SNAT IPs from frontend eligibility. It uses
// ranges supported by the NSX IPBlock model, never overlapping broad CIDRs. Backend
// fallback IPs outside the VIP pool leave the VIP ranges unchanged. This calculation
// does not itself change Net Operator IPAM or prove backend IP availability.
func PartitionVIPRanges(ranges []AddressRange, snat []string) ([]AddressRange, error) {
	type interval struct{ start, end netip.Addr }
	intervals := make([]interval, 0, len(ranges))
	for _, r := range ranges {
		a, err := address(r.Start)
		if err != nil {
			return nil, err
		}
		b, err := address(r.End)
		if err != nil {
			return nil, err
		}
		if a.BitLen() != b.BitLen() || a.Compare(b) > 0 {
			return nil, errors.New("invalid VIP range")
		}
		intervals = append(intervals, interval{a, b})
	}
	sort.Slice(intervals, func(i, j int) bool { return intervals[i].start.Less(intervals[j].start) })
	for i := 1; i < len(intervals); i++ {
		if intervals[i-1].end.Compare(intervals[i].start) >= 0 {
			return nil, errors.New("overlapping VIP ranges")
		}
	}
	seen := map[netip.Addr]bool{}
	for _, raw := range snat {
		ip, err := address(raw)
		if err != nil {
			return nil, err
		}
		if seen[ip] {
			return nil, errors.New("duplicate SNAT address")
		}
		seen[ip] = true
		next := make([]interval, 0, len(intervals)+1)
		for _, r := range intervals {
			if r.start.BitLen() != ip.BitLen() || ip.Less(r.start) || r.end.Less(ip) {
				next = append(next, r)
				continue
			}
			if r.start.Less(ip) {
				next = append(next, interval{r.start, ip.Prev()})
			}
			if ip.Less(r.end) {
				next = append(next, interval{ip.Next(), r.end})
			}
		}
		intervals = next
	}
	result := make([]AddressRange, 0, len(intervals))
	for _, r := range intervals {
		result = append(result, AddressRange{r.start.String(), r.end.String()})
	}
	return result, nil
}

func address(raw string) (netip.Addr, error) {
	ip, err := netip.ParseAddr(raw)
	if err != nil || ip.Zone() != "" || ip.Is4In6() {
		return netip.Addr{}, fmt.Errorf("invalid IP address %q", raw)
	}
	return ip, nil
}
