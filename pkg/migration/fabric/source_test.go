/*
Copyright © 2026 Broadcom, Inc. All Rights Reserved.

	SPDX-License-Identifier: Apache-2.0
*/
package fabric

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	netapi "github.com/vmware-tanzu/net-operator-api/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type sourceClient struct {
	client.Client
	creates int
}

func (c *sourceClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if allocation, ok := obj.(*netapi.IPAddressAllocation); ok {
		allocation.UID = "allocation-uid"
		err := c.Client.Create(ctx, obj, opts...)
		if err == nil {
			c.creates++
		}
		return err
	}
	return c.Client.Create(ctx, obj, opts...)
}

func TestSourceAllocationRejectsIPv6(t *testing.T) {
	c, transition := setup(t)
	writer := &sourceClient{Client: c}
	source := &NetOperatorSource{Client: writer, Reader: c, Validate: func(context.Context, SourceRequest) error { return nil }}
	request := SourceRequest{Network: "vip-network", NetworkUID: "network-uid", RequestedIP: "2001:db8::1"}
	_, err := source.Reserve(context.Background(), transition, request)
	require.ErrorContains(t, err, "invalid requested source IPv4 SNAT IP")
	require.Zero(t, writer.creates)
}

func TestSourceAllocationIsTheRetryCheckpoint(t *testing.T) {
	ctx := context.Background()
	c, transition := setup(t)
	require.NoError(t, netapi.AddToScheme(c.Scheme()))
	network := &netapi.Network{ObjectMeta: metav1.ObjectMeta{Name: "vip-network", Namespace: SNATNamespace, UID: "network-uid"}, Spec: netapi.NetworkSpec{Type: netapi.NetworkTypeVDS}}
	require.NoError(t, c.Create(ctx, network))
	writer := &sourceClient{Client: c}
	source := &NetOperatorSource{Client: writer, Reader: c, Validate: func(context.Context, SourceRequest) error { return nil }}
	request := SourceRequest{Network: network.Name, NetworkUID: network.UID}
	_, err := source.Reserve(ctx, transition, request)
	require.ErrorIs(t, err, ErrPending)
	require.Equal(t, 1, writer.creates)

	allocations := &netapi.IPAddressAllocationList{}
	require.NoError(t, c.List(ctx, allocations, client.InNamespace(SNATNamespace)))
	require.Len(t, allocations.Items, 1)
	allocation := &allocations.Items[0]
	require.Empty(t, allocation.OwnerReferences)
	require.Contains(t, allocation.Finalizers, SNATFinalizer)
	require.Equal(t, "Network", allocation.Spec.PoolRef.Kind)
	allocation.Status = netapi.IPAddressAllocationStatus{IPAddress: "192.0.2.5", Conditions: []netapi.IPAddressAllocationCondition{{Type: netapi.IPAddressAllocationReady, Status: corev1.ConditionTrue}}}
	require.NoError(t, c.Update(ctx, allocation))
	reserved, err := source.Reserve(ctx, transition, request)
	require.NoError(t, err)
	require.Equal(t, "192.0.2.5", reserved.IP)
	require.Equal(t, 1, writer.creates, "reconcile reuses the stable allocation name")

	request.RequestedIP = "192.0.2.6"
	_, err = source.Reserve(ctx, transition, request)
	require.ErrorContains(t, err, "request conflicts")
	require.Equal(t, 1, writer.creates)
}

func TestSourceAllocationFailureIsNotPoolExhaustion(t *testing.T) {
	ctx := context.Background()
	c, transition := setup(t)
	require.NoError(t, netapi.AddToScheme(c.Scheme()))
	network := &netapi.Network{ObjectMeta: metav1.ObjectMeta{Name: "vip-network", Namespace: SNATNamespace, UID: "network-uid"}, Spec: netapi.NetworkSpec{Type: netapi.NetworkTypeVDS}}
	require.NoError(t, c.Create(ctx, network))
	writer := &sourceClient{Client: c}
	source := &NetOperatorSource{Client: writer, Reader: c, Validate: func(context.Context, SourceRequest) error { return nil }}
	request := SourceRequest{Network: network.Name, NetworkUID: network.UID}
	_, err := source.Reserve(ctx, transition, request)
	require.ErrorIs(t, err, ErrPending)
	list := &netapi.IPAddressAllocationList{}
	require.NoError(t, c.List(ctx, list, client.InNamespace(SNATNamespace)))
	allocation := list.Items[0]
	allocation.Status.Conditions = []netapi.IPAddressAllocationCondition{{Type: netapi.IPAddressAllocationFail, Status: corev1.ConditionTrue, Reason: netapi.IPAddressAllocationConditionFailureReasonCannotAllocIP}}
	require.NoError(t, c.Update(ctx, &allocation))
	_, err = source.Reserve(ctx, transition, request)
	require.ErrorContains(t, err, "CannotAllocIP")
	require.NotErrorIs(t, err, ErrPoolExhausted)
	source.Validate = nil
	_, err = source.Reserve(ctx, transition, request)
	require.ErrorContains(t, err, "integration unavailable")
}
