/* Copyright © 2026 Broadcom, Inc. All Rights Reserved.
   SPDX-License-Identifier: Apache-2.0 */

package inspection

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/vmware-tanzu/nsx-operator/pkg/apis/migration/v1alpha1"
)

type inventoryStub struct {
	sources map[string]Source
	calls   int
	err     error
}

func (i *inventoryStub) NamespaceVDNs(context.Context, []string) ([]string, error) {
	return []string{"a", "a"}, i.err
}
func (i *inventoryStub) Resolve(_ context.Context, name string) (Source, error) {
	i.calls++
	return i.sources[name], i.err
}
func source(name string) Source {
	return Source{VDNName: name, DHCP: true, Topology: Topology{PortGroupID: "dvportgroup-1", SwitchID: "dvs-1", SwitchUUID: "uuid", ClusterIDs: []string{"domain-c1"}}}
}
func validInput(name string) api.DHCPNetworkConfiguration {
	return api.DHCPNetworkConfiguration{VDNName: name, SubnetMask: "255.255.255.0", GatewayAddress: "192.168.10.1", DHCPRange: &api.IPAddressRange{Start: "192.168.10.100", End: "192.168.10.200"}}
}

func TestInspectionAndDiagnostics(t *testing.T) {
	ctx := context.Background()
	inv := &inventoryStub{sources: map[string]Source{"a": source("a")}}
	s := Service{Inventory: inv}
	r, err := s.InspectNamespaces(ctx, []string{"ns1", "ns2"}, nil)
	require.NoError(t, err)
	require.Equal(t, 1, inv.calls)
	require.False(t, r.Ready())
	require.Len(t, r.RequiredInputs, 1)
	require.Contains(t, r.RequiredInputs[0].Description, "dhcpRange.start")
	require.Contains(t, r.RequiredInputs[0].Description, "VDN a")
	st := api.TransitionStatus{RequiredInputs: []api.RequiredInput{{Key: "other", Description: "another service"}}, LastAction: api.ActionEvaluate, ActionGeneration: 1, ActionState: api.ActionStateRunning}
	require.NoError(t, r.Apply(&st, 1))
	require.Equal(t, api.PhaseNeedsInput, st.Phase)
	require.Len(t, st.RequiredInputs, 2)
	r, err = s.Inspect(ctx, []string{"a"}, []api.DHCPNetworkConfiguration{validInput("a")})
	require.NoError(t, err)
	require.True(t, r.Ready())
	require.NoError(t, r.Apply(&st, 2))
	require.Len(t, st.RequiredInputs, 1)
	require.Equal(t, "other", st.RequiredInputs[0].Key)
	require.Equal(t, api.PhaseNeedsInput, st.Phase)
	require.Equal(t, api.ActionStateRunning, st.ActionState)
	require.Equal(t, int64(1), st.ActionGeneration)
	require.Equal(t, metav1.ConditionTrue, st.Conditions[0].Status)
	st.RequiredInputs = nil
	require.NoError(t, r.Apply(&st, 2))
	require.Equal(t, api.PhaseEvaluating, st.Phase)
	before := st.DeepCopy()
	require.Error(t, r.Apply(&st, 1))
	require.Equal(t, *before, st)
}

func TestInvalidInputsAreNotNeedsInput(t *testing.T) {
	s := Service{Inventory: &inventoryStub{sources: map[string]Source{"a": source("a")}}}
	for _, inputs := range [][]api.DHCPNetworkConfiguration{
		{validInput("unknown")}, {validInput("a"), validInput("a")},
		{{VDNName: "a", GatewayAddress: "2001:db8::1"}},
		{{VDNName: "a", SubnetMask: "255.255.255.0", DHCPRange: &api.IPAddressRange{Start: "192.168.10.100", End: "192.168.11.100"}}},
	} {
		r, err := s.Inspect(context.Background(), []string{"a"}, inputs)
		require.NoError(t, err)
		require.NotEmpty(t, r.Problems)
		st := api.TransitionStatus{}
		require.NoError(t, r.Apply(&st, 1))
		require.Equal(t, api.PhaseEvaluationFailed, st.Phase)
	}
}

func TestStaticAndSharedBacking(t *testing.T) {
	a, b := source("a"), source("b")
	inv := &inventoryStub{sources: map[string]Source{"a": a, "b": b}}
	s := Service{Inventory: inv}
	input := validInput("b")
	input.DHCPRange.End = "192.168.10.201"
	r, err := s.Inspect(context.Background(), []string{"b", "a"}, []api.DHCPNetworkConfiguration{validInput("a"), input})
	require.NoError(t, err)
	require.False(t, r.Ready())
	require.Contains(t, strings.Join(r.Problems, ""), "conflicting")
	a.DHCP = false
	a.Mask = "255.255.255.0"
	a.Gateway = "192.168.10.1"
	a.Ranges = []api.IPAddressRange{{Start: "192.168.10.10", End: "192.168.10.90"}}
	inv.sources["a"] = a
	r, err = s.Inspect(context.Background(), []string{"a"}, nil)
	require.NoError(t, err)
	require.True(t, r.Ready())
	require.Empty(t, r.RequiredInputs)
	r, err = s.Inspect(context.Background(), []string{"a"}, []api.DHCPNetworkConfiguration{validInput("a")})
	require.NoError(t, err)
	require.NotEmpty(t, r.Problems)
	inv.err = errors.New("unavailable")
	_, err = s.Inspect(context.Background(), []string{"a"}, nil)
	require.Error(t, err)
	inv.err = invalid("unsupported source")
	r, err = s.Inspect(context.Background(), []string{"a"}, nil)
	require.NoError(t, err)
	require.NotEmpty(t, r.Problems)
}
