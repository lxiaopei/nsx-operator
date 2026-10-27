/* Copyright © 2026 Broadcom, Inc. All Rights Reserved.
   SPDX-License-Identifier: Apache-2.0 */

package inspection

import (
	"testing"

	"github.com/stretchr/testify/require"

	api "github.com/vmware-tanzu/nsx-operator/pkg/apis/migration/v1alpha1"
)

func TestValidateAddressing(t *testing.T) {
	for _, tc := range []struct {
		name, mask, gateway, start, end string
		valid                           bool
	}{
		{"derived", "255.255.255.0", "192.168.10.1", "192.168.10.100", "192.168.10.200", true},
		{"mask hole", "255.0.255.0", "192.168.10.1", "192.168.10.100", "192.168.10.200", false},
		{"wrong gateway", "255.255.255.0", "192.168.11.1", "192.168.10.100", "192.168.10.200", false},
		{"gateway in pool", "255.255.255.0", "192.168.10.150", "192.168.10.100", "192.168.10.200", false},
		{"reversed", "255.255.255.0", "192.168.10.1", "192.168.10.200", "192.168.10.100", false},
		{"range crosses subnet", "255.255.255.0", "192.168.10.1", "192.168.10.100", "192.168.11.200", false},
		{"family mismatch", "255.255.255.0", "192.168.10.1", "2001:db8::1", "2001:db8::2", false},
		{"mapped IPv4", "255.255.255.0", "192.168.10.1", "::ffff:192.168.10.100", "192.168.10.200", false},
		{"broadcast", "255.255.255.0", "192.168.10.1", "192.168.10.100", "192.168.10.255", false},
		{"network", "255.255.255.0", "192.168.10.1", "192.168.10.0", "192.168.10.200", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := ValidateAddressing(tc.mask, tc.gateway, []api.IPAddressRange{{Start: tc.start, End: tc.end}})
			if !tc.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "192.168.10.0/24", a.CIDR.String())
			require.NoError(t, a.ValidateExistingAddresses([]string{"192.168.10.50"}))
			require.Error(t, a.ValidateExistingAddresses([]string{"192.168.11.50"}))
		})
	}
}
