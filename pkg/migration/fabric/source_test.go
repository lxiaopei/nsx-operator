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

	"github.com/vmware-tanzu/nsx-operator/pkg/migration/recovery"
)

type sourceClient struct {
	client.Client
	creates int
}

func (c *sourceClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if allocation, ok := obj.(*netapi.IPAddressAllocation); ok {
		allocation.UID = "allocation-uid"
		c.creates++
	}
	return c.Client.Create(ctx, obj, opts...)
}

func TestTypedSourceReservationCreateWaitAndReuse(t *testing.T) {
	ctx := context.Background()
	store, session, _ := setup(t)
	require.NoError(t, netapi.AddToScheme(store.Client.Scheme()))
	network := &netapi.Network{ObjectMeta: metav1.ObjectMeta{Name: "vip-network", Namespace: SNATNamespace, UID: "network-uid"}, Spec: netapi.NetworkSpec{Type: netapi.NetworkTypeVDS}}
	require.NoError(t, store.Client.Create(ctx, network))
	c := &sourceClient{Client: store.Client}
	source := &NetOperatorSource{Client: c, Reader: store.Reader, Validate: func(context.Context, SourceRequest) error { return nil }}
	request := SourceRequest{Network: network.Name, NetworkUID: network.UID, Family: "IPv4"}
	_, err := source.Reserve(ctx, session, request)
	require.ErrorIs(t, err, recovery.ErrPending)
	require.Equal(t, 1, c.creates)
	list := &netapi.IPAddressAllocationList{}
	require.NoError(t, c.List(ctx, list, client.InNamespace(SNATNamespace)))
	require.Len(t, list.Items, 1)
	allocation := list.Items[0]
	require.Empty(t, allocation.OwnerReferences)
	require.Contains(t, allocation.Finalizers, SNATFinalizer)
	require.Equal(t, "Network", allocation.Spec.PoolRef.Kind)
	allocation.Status = netapi.IPAddressAllocationStatus{IPAddress: "192.0.2.5", Conditions: []netapi.IPAddressAllocationCondition{{Type: netapi.IPAddressAllocationReady, Status: corev1.ConditionTrue}}}
	require.NoError(t, c.Update(ctx, &allocation))
	reserved, err := source.Reserve(ctx, session, request)
	require.NoError(t, err)
	require.Equal(t, "192.0.2.5", reserved.IP)
	require.Equal(t, 1, c.creates)
	request.RequestedIP = "192.0.2.6"
	_, err = source.Reserve(ctx, session, request)
	require.Error(t, err)
	require.Equal(t, 1, c.creates, "never request a second address to replace an unresolved or accepted request")
	request.RequestedIP = ""
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(&allocation), &allocation))
	allocation.UID = "replacement"
	require.NoError(t, c.Update(ctx, &allocation))
	_, err = source.Reserve(ctx, session, request)
	require.ErrorContains(t, err, "replaced")
	require.Equal(t, 1, c.creates)
}

func TestSourceAllocationFailureIsNotPoolExhaustion(t *testing.T) {
	ctx := context.Background()
	store, session, _ := setup(t)
	require.NoError(t, netapi.AddToScheme(store.Client.Scheme()))
	network := &netapi.Network{ObjectMeta: metav1.ObjectMeta{Name: "vip-network", Namespace: SNATNamespace, UID: "network-uid"}, Spec: netapi.NetworkSpec{Type: netapi.NetworkTypeVDS}}
	require.NoError(t, store.Client.Create(ctx, network))
	c := &sourceClient{Client: store.Client}
	source := &NetOperatorSource{Client: c, Reader: store.Reader, Validate: func(context.Context, SourceRequest) error { return nil }}
	request := SourceRequest{Network: network.Name, NetworkUID: network.UID, Family: "IPv4"}
	_, err := source.Reserve(ctx, session, request)
	require.ErrorIs(t, err, recovery.ErrPending)
	list := &netapi.IPAddressAllocationList{}
	require.NoError(t, c.List(ctx, list))
	allocation := list.Items[0]
	allocation.Status.Conditions = []netapi.IPAddressAllocationCondition{{Type: netapi.IPAddressAllocationFail, Status: corev1.ConditionTrue, Reason: netapi.IPAddressAllocationConditionFailureReasonCannotAllocIP}}
	require.NoError(t, c.Update(ctx, &allocation))
	_, err = source.Reserve(ctx, session, request)
	require.ErrorContains(t, err, "CannotAllocIP")
	require.NotErrorIs(t, err, ErrPoolExhausted)
	source.Validate = nil
	_, err = source.Reserve(ctx, session, request)
	require.ErrorContains(t, err, "integration unavailable")
}
