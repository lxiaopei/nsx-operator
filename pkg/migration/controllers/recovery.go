/* Copyright © 2026 Broadcom, Inc. All Rights Reserved.
   SPDX-License-Identifier: Apache-2.0 */

package controllers

import (
	"context"
	"errors"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	api "github.com/vmware-tanzu/nsx-operator/pkg/apis/migration/v1alpha1"
	"github.com/vmware-tanzu/nsx-operator/pkg/migration/recovery"
	"github.com/vmware-tanzu/nsx-operator/pkg/migration/transition"
)

func identity(obj client.Object, action api.Action) recovery.Identity {
	kind := "NamespaceNetworkTransition"
	if _, ok := obj.(*api.ControlPlaneNetworkTransition); ok {
		kind = "ControlPlaneNetworkTransition"
	}
	return recovery.Identity{UID: obj.GetUID(), Name: obj.GetName(), Kind: kind, Action: string(action), Generation: obj.GetGeneration()}
}

func (r *Reconciler) recoveryLifecycle(ctx context.Context, obj client.Object) (bool, error) {
	if r.Recovery == nil || !r.Engine.Enabled {
		return !obj.GetDeletionTimestamp().IsZero(), nil
	}
	if obj.GetDeletionTimestamp().IsZero() {
		var action api.Action
		switch v := obj.(type) {
		case *api.ControlPlaneNetworkTransition:
			action = v.Spec.Action
		case *api.NamespaceNetworkTransition:
			action = v.Spec.Action
		}
		if r.Engine.Handlers[transition.Key{Stage: r.Stage, Action: action}] != nil && !controllerutil.ContainsFinalizer(obj, recovery.Finalizer) {
			before := obj.DeepCopyObject().(client.Object)
			controllerutil.AddFinalizer(obj, recovery.Finalizer)
			// The metadata event queues the next pass before any action or external work.
			return true, r.Patch(ctx, obj, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
		}
		return false, nil
	}
	if !controllerutil.ContainsFinalizer(obj, recovery.Finalizer) {
		return true, nil
	}
	owned, err := r.Recovery.OwnsLock(ctx, obj.GetUID())
	if err != nil {
		return true, err
	}
	if owned {
		// Do not silently release on deletion. Continue the recorded operation so
		// recovery can finish; journals and protection outlive the deletion request.
		return false, nil
	}
	old := obj.DeepCopyObject().(client.Object)
	controllerutil.RemoveFinalizer(obj, recovery.Finalizer)
	return true, r.Patch(ctx, obj, client.MergeFromWithOptions(old, client.MergeFromWithOptimisticLock{}))
}

func (r *Reconciler) prepareRecovery(ctx context.Context, req transition.Request) (*recovery.Session, error) {
	if req.Action == api.ActionPlan {
		if r.ConfirmFreshnessAdmission == nil {
			return nil, errors.New("WBS 1.1.4.1 freshness admission is not confirmed; Plan is blocked")
		}
		if err := r.ConfirmFreshnessAdmission(ctx); err != nil {
			return nil, err
		}
	}
	if !controllerutil.ContainsFinalizer(req.Object, recovery.Finalizer) {
		return nil, errors.New("migration recovery finalizer is missing")
	}

	if !req.Object.GetDeletionTimestamp().IsZero() {
		lock, err := r.Recovery.ReadLock(ctx)
		if err != nil {
			return nil, err
		}
		id := identity(req.Object, req.Action)
		if lock.Holder == nil || lock.Holder.UID != id.UID || (lock.Holder.ID() != id.ID() && req.Action != api.ActionRollback) {
			return nil, errors.New("deleting transition can only resume its held operation or eligible namespace rollback")
		}
	}
	scope := recovery.Scope{AllNamespaces: r.Stage == transition.Infrastructure}
	if batch, ok := req.Object.(*api.NamespaceNetworkTransition); ok {
		for _, name := range batch.Spec.Namespaces {
			var ns corev1.Namespace
			if err := r.reader().Get(ctx, types.NamespacedName{Name: name}, &ns); err != nil {
				return nil, err
			}
			if ns.UID == "" {
				return nil, fmt.Errorf("namespace %s has no UID", name)
			}
			scope.NamespaceUIDs = append(scope.NamespaceUIDs, ns.UID)
		}
		sort.Slice(scope.NamespaceUIDs, func(i, j int) bool { return scope.NamespaceUIDs[i] < scope.NamespaceUIDs[j] })
	}
	session, err := r.Recovery.Acquire(ctx, identity(req.Object, req.Action), scope)
	if err != nil {
		return nil, err
	}
	// Dependencies are re-read under durable ownership, not merely the process mutex.
	if blocked, err := r.dependencyBlock(ctx, req.Action, req.Status); err != nil {
		return nil, err
	} else if blocked != "" {
		return nil, errors.New(blocked)
	}
	if r.Infrastructure != nil && r.Stage == transition.Infrastructure {
		if err = r.Infrastructure.Bind(ctx, req.Object.(*api.ControlPlaneNetworkTransition), session); err != nil {
			return nil, err
		}
	}
	protect := req.Action == api.ActionExecute
	if req.Action == api.ActionRollback {
		protect, err = r.Recovery.HadCutover(ctx, req.Object.GetUID())
		if err != nil {
			return nil, err
		}
	}
	if protect {
		if err = session.Protect(ctx, r.ConfirmProtection); err != nil {
			return nil, err
		}
	}
	return session, nil
}

func (r *Reconciler) finishRecovery(ctx context.Context, obj client.Object, action api.Action, status *api.TransitionStatus) error {
	if r.Recovery == nil {
		return nil
	}
	if status.LastAction != action || status.ActionState != api.ActionStateSucceeded || status.ActionGeneration != obj.GetGeneration() {
		return nil
	}
	return r.Recovery.Finish(ctx, identity(obj, action))
}
