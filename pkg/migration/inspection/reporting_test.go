/* Copyright © 2026 Broadcom, Inc. All Rights Reserved.
   SPDX-License-Identifier: Apache-2.0 */

package inspection

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/vmware-tanzu/nsx-operator/pkg/apis/migration/v1alpha1"
)

func TestEvaluateReportsBothCRDsAndRejectsStaleSnapshots(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, api.AddToScheme(scheme))
	cp := &api.ControlPlaneNetworkTransition{ObjectMeta: metav1.ObjectMeta{Name: "supervisor-vds-to-vpc", UID: "cp", Generation: 1}, Spec: api.ControlPlaneNetworkTransitionSpec{TransitionSpec: api.TransitionSpec{Action: api.ActionEvaluate}}}
	ns := &api.NamespaceNetworkTransition{ObjectMeta: metav1.ObjectMeta{Name: "batch", UID: "ns", Generation: 1}, Spec: api.NamespaceNetworkTransitionSpec{TransitionSpec: api.TransitionSpec{Action: api.ActionEvaluate}, Namespaces: []string{"tenant"}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(cp, ns).WithObjects(cp, ns).Build()
	inv := &inventoryStub{sources: map[string]Source{"a": source("a")}}
	s := Service{Inventory: inv}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(cp), cp))
	_, err := s.EvaluateControlPlane(ctx, c, cp, []string{"a"})
	require.NoError(t, err)
	require.Nil(t, cp.Status)
	fresh := &api.ControlPlaneNetworkTransition{}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(cp), fresh))
	require.Equal(t, api.PhaseNeedsInput, fresh.Status.Phase)
	require.Contains(t, fresh.Status.RequiredInputs[0].Key, "controlPlaneAndServiceDHCPNetworks/")
	require.Contains(t, fresh.Status.RequiredInputs[0].Description, "spec.networkConfiguration.controlPlaneAndServiceDHCPNetworks")
	_, err = s.EvaluateControlPlane(ctx, c, cp, []string{"a"})
	require.Error(t, err)
	fresh.Spec.NetworkConfiguration = &api.ControlPlaneNetworkConfiguration{ControlPlaneAndServiceDHCPNetworks: []api.DHCPNetworkConfiguration{validInput("a")}}
	fresh.Generation = 2
	require.NoError(t, c.Update(ctx, fresh))
	result, err := s.EvaluateControlPlane(ctx, c, fresh, []string{"a"})
	require.NoError(t, err)
	require.True(t, result.Ready())
	require.Equal(t, "192.168.10.0/24", result.Networks[0].Addressing.CIDR.String())
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(cp), fresh))
	require.Empty(t, fresh.Status.RequiredInputs)
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(ns), ns))
	_, err = s.EvaluateNamespaces(ctx, c, ns)
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(ns), ns))
	require.Equal(t, api.PhaseNeedsInput, ns.Status.Phase)
	require.Contains(t, ns.Status.RequiredInputs[0].Key, "namespaceDHCPNetworks/")
	require.Contains(t, ns.Status.RequiredInputs[0].Description, "spec.networkConfiguration.namespaceDHCPNetworks")
	require.Empty(t, ns.Status.LastAction)
	ns.Spec.NetworkConfiguration = &api.NamespaceNetworkConfiguration{NamespaceDHCPNetworks: []api.DHCPNetworkConfiguration{validInput("a")}}
	ns.Generation = 2
	require.NoError(t, c.Update(ctx, ns))
	_, err = s.EvaluateNamespaces(ctx, c, ns)
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(ns), ns))
	require.Equal(t, api.PhaseEvaluating, ns.Status.Phase)
	require.Empty(t, ns.Status.RequiredInputs)
	// Correcting a syntax-valid but incompatible value must trigger a fresh
	// inspection and clear its failure without claiming action completion.
	ns.Spec.NetworkConfiguration.NamespaceDHCPNetworks[0].GatewayAddress = "192.168.11.1"
	ns.Generation = 3
	require.NoError(t, c.Update(ctx, ns))
	result, err = s.EvaluateNamespaces(ctx, c, ns)
	require.NoError(t, err)
	require.False(t, result.Ready())
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(ns), ns))
	require.Equal(t, api.PhaseEvaluationFailed, ns.Status.Phase)
	ns.Spec.NetworkConfiguration.NamespaceDHCPNetworks[0].GatewayAddress = "192.168.10.1"
	ns.Generation = 4
	require.NoError(t, c.Update(ctx, ns))
	result, err = s.EvaluateNamespaces(ctx, c, ns)
	require.NoError(t, err)
	require.True(t, result.Ready())
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(ns), ns))
	require.Equal(t, api.PhaseEvaluating, ns.Status.Phase)
	require.Equal(t, int64(4), ns.Status.ObservedGeneration)
	require.Empty(t, ns.Status.LastAction)
	inv.err = errors.New("transport unavailable")
	_, err = s.EvaluateNamespaces(ctx, c, ns)
	require.Error(t, err)
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(ns), ns))
	require.Equal(t, api.PhaseBlocked, ns.Status.Phase)
	require.Equal(t, metav1.ConditionUnknown, ns.Status.Conditions[0].Status)
}
