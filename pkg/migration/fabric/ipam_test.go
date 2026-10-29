/*
Copyright © 2026 Broadcom, Inc. All Rights Reserved.

	SPDX-License-Identifier: Apache-2.0
*/
package fabric

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPartitionVIPRanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ranges []AddressRange
		snat   []string
		want   []AddressRange
		fail   bool
	}{
		{name: "ipv4 middle", ranges: []AddressRange{{"192.0.2.1", "192.0.2.10"}}, snat: []string{"192.0.2.5"}, want: []AddressRange{{"192.0.2.1", "192.0.2.4"}, {"192.0.2.6", "192.0.2.10"}}},
		{name: "ipv6 ends", ranges: []AddressRange{{"2001:db8::", "2001:db8::ffff"}}, snat: []string{"2001:db8::", "2001:db8::ffff"}, want: []AddressRange{{"2001:db8::1", "2001:db8::fffe"}}},
		{name: "singleton", ranges: []AddressRange{{"192.0.2.1", "192.0.2.1"}}, snat: []string{"192.0.2.1"}, want: []AddressRange{}},
		{name: "backend fallback", ranges: []AddressRange{{"192.0.2.1", "192.0.2.10"}}, snat: []string{"198.51.100.1"}, want: []AddressRange{{"192.0.2.1", "192.0.2.10"}}},
		{name: "overlap", ranges: []AddressRange{{"192.0.2.1", "192.0.2.10"}, {"192.0.2.5", "192.0.2.20"}}, fail: true},
		{name: "mixed family", ranges: []AddressRange{{"192.0.2.1", "2001:db8::1"}}, fail: true},
		{name: "mapped", snat: []string{"::ffff:192.0.2.1"}, fail: true},
		{name: "duplicate", snat: []string{"192.0.2.1", "192.0.2.1"}, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PartitionVIPRanges(tc.ranges, tc.snat)
			if tc.fail {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.want, got)
			}
		})
	}
}
