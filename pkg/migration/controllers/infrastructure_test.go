/*
Copyright © 2026 Broadcom, Inc. All Rights Reserved.

	SPDX-License-Identifier: Apache-2.0
*/
package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"

	api "github.com/vmware-tanzu/nsx-operator/pkg/apis/migration/v1alpha1"
	"github.com/vmware-tanzu/nsx-operator/pkg/migration/infrastructure"
	"github.com/vmware-tanzu/nsx-operator/pkg/migration/transition"
)

type infraReadiness func(context.Context, bool, string) error

func (f infraReadiness) Check(ctx context.Context, b bool, p string) error { return f(ctx, b, p) }

type infraProjects func(context.Context, string) (infrastructure.Project, error)

func (f infraProjects) GetProject(ctx context.Context, p string) (infrastructure.Project, error) {
	return f(ctx, p)
}

func TestInfrastructureStartGateAndResume(t *testing.T) {
	ctx := context.Background()
	r, key, obj := recoveryController(t, api.ActionEvaluate)
	obj.Spec.SourceMode = "VSPHERE_VDS"
	obj.Spec.TargetMode = "NSX_VPC"
	obj.Spec.NSXProjectID = "tenant-project"
	require.NoError(t, r.Update(ctx, obj))
	ready := false
	checks := 0
	calls := 0
	r.Infrastructure = &infrastructure.Service{Recovery: r.Recovery, Readiness: infraReadiness(func(context.Context, bool, string) error {
		checks++
		if !ready {
			return &infrastructure.Issue{Reason: "NCPNotReady", Message: "NCP not ready"}
		}
		return nil
	}), Projects: infraProjects(func(_ context.Context, id string) (infrastructure.Project, error) {
		return infrastructure.Project{ID: id, Path: "/orgs/default/projects/" + id, UniqueID: "uid"}, nil
	})}
	r.Engine.Handlers[transition.Key{Stage: r.Stage, Action: api.ActionEvaluate}] = actionHandlerFunc(func(ctx context.Context, req transition.Request) (transition.Result, error) {
		calls++
		history, err := req.Execution.History(ctx)
		require.NoError(t, err)
		require.Len(t, history, 1)
		require.Contains(t, history[0].Checkpoints, "infrastructure-start", "project binding must precede handler invocation")
		require.Equal(t, "/orgs/default/projects/tenant-project", req.Object.(*api.ControlPlaneNetworkTransition).Status.TargetNSXProject)
		return transition.Result{Outcome: transition.OutcomeInProgress}, nil
	})
	result, err := r.Reconcile(ctx, key)
	require.NoError(t, err)
	require.Positive(t, result.RequeueAfter)
	require.Zero(t, calls)
	require.NoError(t, r.Get(ctx, key.NamespacedName, obj))
	require.Equal(t, "EvaluationFailed", obj.Status.Phase)
	require.Equal(t, "NCPNotReady", meta.FindStatusCondition(obj.Status.Conditions, infrastructure.ConditionReady).Reason)
	owned, err := r.Recovery.OwnsLock(ctx, obj.UID)
	require.NoError(t, err)
	require.False(t, owned, "readiness must gate lock and external work")
	ready = true
	checks = 0
	_, err = r.Reconcile(ctx, key)
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.Equal(t, 2, checks, "repeat live prerequisites under the durable lock")
	_, err = r.Reconcile(ctx, key)
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	history, err := r.Recovery.History(ctx, obj.UID)
	require.NoError(t, err)
	require.Len(t, history, 1, "resume must reuse the operation rather than starting another")
	require.NoError(t, r.Get(ctx, key.NamespacedName, obj))
	require.Equal(t, api.ActionStateRunning, obj.Status.ActionState)
}
