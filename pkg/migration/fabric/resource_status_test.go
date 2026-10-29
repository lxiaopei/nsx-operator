/* Copyright © 2026 Broadcom, Inc. All Rights Reserved.
   SPDX-License-Identifier: Apache-2.0 */

package fabric

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/vmware-tanzu/nsx-operator/pkg/apis/migration/v1alpha1"
	"github.com/vmware-tanzu/nsx-operator/pkg/migration/recovery"
)

func TestResourceStatusPreservesConsumersAndIdentity(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"ControlPlaneNetworkTransition", "NamespaceNetworkTransition"} {
		t.Run(kind, func(t *testing.T) {
			store, session, obj := setup(t)
			id := session.Identity()
			if kind == "NamespaceNetworkTransition" {
				ns := &api.NamespaceNetworkTransition{ObjectMeta: metav1.ObjectMeta{Name: "batch", UID: "batch", Generation: 1}, Spec: api.NamespaceNetworkTransitionSpec{TransitionSpec: api.TransitionSpec{Action: api.ActionDerive}}}
				c := fake.NewClientBuilder().WithScheme(store.Client.Scheme()).WithStatusSubresource(ns).WithObjects(ns).Build()
				store.Client, store.Reader = c, c
				id = recovery.Identity{Kind: kind, Name: ns.Name, UID: ns.UID, Generation: 1, Action: "Derive"}
			}
			status := &KubernetesResourceStatus{Client: store.Client, Reader: store.Reader}
			record := Record{Path: "/infra/ip-blocks/block", UniqueID: "resource", Provenance: "Created", Owner: id.UID, Consumers: []Consumer{{Kind: kind, Name: id.Name, UID: id.UID}}}
			require.NoError(t, status.Save(ctx, id, record))
			record.Consumers = []Consumer{{Kind: kind, Name: "other", UID: "other"}}
			require.NoError(t, status.Save(ctx, id, record))
			saved, err := status.Load(ctx, id, record.Path)
			require.NoError(t, err)
			require.Len(t, saved.Consumers, 2)
			record.UniqueID = "replacement"
			require.ErrorContains(t, status.Save(ctx, id, record), "identity or provenance changed")
			stale := id
			stale.Generation++
			_, err = status.Load(ctx, stale, record.Path)
			require.ErrorIs(t, err, recovery.ErrFenced)
			if kind == "ControlPlaneNetworkTransition" {
				require.NoError(t, store.Reader.Get(ctx, client.ObjectKeyFromObject(obj), obj))
				require.Equal(t, "NSX_LB", string(obj.Status.TargetLBProvider))
			}
		})
	}
}

func TestFabricRejectsVerifiedOperation(t *testing.T) {
	store, session, _ := setup(t)
	require.NoError(t, session.VerifyCompletion(context.Background(), nil))
	b := &backend{}
	_, err := (&Service{Backend: b, Status: &KubernetesResourceStatus{Client: store.Client, Reader: store.Reader}}).Ensure(context.Background(), session, vpcSpec())
	require.ErrorContains(t, err, "verified operation")
	require.Zero(t, b.writes)
}
