/*
Copyright © 2026 Broadcom, Inc. All Rights Reserved.

	SPDX-License-Identifier: Apache-2.0
*/
package controllers

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	api "github.com/vmware-tanzu/nsx-operator/pkg/apis/migration/v1alpha1"
	"github.com/vmware-tanzu/nsx-operator/pkg/migration/recovery"
	"github.com/vmware-tanzu/nsx-operator/pkg/migration/transition"
)

func recoveryController(t *testing.T, action api.Action) (*Reconciler, ctrl.Request, *api.ControlPlaneNetworkTransition) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, api.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	obj := &api.ControlPlaneNetworkTransition{ObjectMeta: metav1.ObjectMeta{Name: api.ControlPlaneNetworkTransitionName, UID: "infra", Generation: 3, Finalizers: []string{recovery.Finalizer}}, Spec: api.ControlPlaneNetworkTransitionSpec{TransitionSpec: api.TransitionSpec{Action: action}}, Status: &api.ControlPlaneNetworkTransitionStatus{TransitionStatus: api.TransitionStatus{LastAction: action, ActionState: api.ActionStateRunning, ActionGeneration: 3}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(obj).WithObjects(obj).Build()
	r := &Reconciler{Client: c, APIReader: c, Engine: &transition.Engine{Enabled: true, Handlers: map[transition.Key]transition.Handler{}}, Stage: transition.Infrastructure, Recovery: &recovery.Store{Client: c, Reader: c}}
	return r, ctrl.Request{NamespacedName: types.NamespacedName{Name: obj.Name}}, obj
}

func TestExecuteFailsClosedUntilProtectionConfirmed(t *testing.T) {
	ctx := context.Background()
	r, key, obj := recoveryController(t, api.ActionExecute)
	calls := 0
	r.Engine.Handlers[transition.Key{Stage: r.Stage, Action: api.ActionExecute}] = actionHandlerFunc(func(ctx context.Context, req transition.Request) (transition.Result, error) {
		calls++
		require.NotNil(t, req.Execution)
		return transition.Result{Outcome: transition.OutcomeSucceeded}, nil
	})
	_, err := r.Reconcile(ctx, key)
	require.ErrorIs(t, err, recovery.ErrProtection)
	require.Zero(t, calls)
	lock, err := r.Recovery.ReadLock(ctx)
	require.NoError(t, err)
	require.True(t, lock.AdmissionProtectionActive)
	require.NotNil(t, lock.Holder)
	r.ConfirmProtection = func(ctx context.Context, l recovery.Lock) error {
		require.True(t, l.AdmissionProtectionActive)
		require.True(t, l.Scope.AllNamespaces)
		return nil
	}
	_, err = r.Reconcile(ctx, key)
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.NoError(t, r.Get(ctx, key.NamespacedName, obj))
	require.Equal(t, api.ActionStateSucceeded, obj.Status.ActionState)
	lock, err = r.Recovery.ReadLock(ctx)
	require.NoError(t, err)
	require.False(t, lock.AdmissionProtectionActive)
	require.Nil(t, lock.Holder)
}

func TestVerifiedOperationRecoversFailedStatusWrite(t *testing.T) {
	ctx := context.Background()
	r, key, obj := recoveryController(t, api.ActionExecute)
	r.ConfirmProtection = func(context.Context, recovery.Lock) error { return nil }
	calls := 0
	r.Engine.Handlers[transition.Key{Stage: r.Stage, Action: api.ActionExecute}] = actionHandlerFunc(func(ctx context.Context, req transition.Request) (transition.Result, error) {
		calls++
		req.Status.NSXWorkflows = []api.NSXWorkflowStatus{{VDNName: "source-vdn", WorkflowID: "stable-workflow"}}
		req.Object.(*api.ControlPlaneNetworkTransition).Status.DetectedSourceLBProvider = "HAPROXY"
		return transition.Result{Outcome: transition.OutcomeSucceeded}, nil
	})
	base := r.Client
	fail := true
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, o client.Object, opts ...client.SubResourceUpdateOption) error {
		if sub == "status" && fail {
			fail = false
			return errors.New("status unavailable")
		}
		return c.SubResource(sub).Update(ctx, o, opts...)
	}})
	_, err := r.Reconcile(ctx, key)
	require.ErrorContains(t, err, "status unavailable")
	require.Equal(t, 1, calls)
	lock, err := r.Recovery.ReadLock(ctx)
	require.NoError(t, err)
	require.True(t, lock.AdmissionProtectionActive)
	// Restart with new engine and Store instances, sharing only durable API objects.
	restarted := *r
	restarted.Client = base
	restarted.Recovery = &recovery.Store{Client: base, Reader: base}
	restarted.Engine = &transition.Engine{Enabled: true, Handlers: r.Engine.Handlers}
	_, err = restarted.Reconcile(ctx, key)
	require.NoError(t, err)
	require.Equal(t, 1, calls, "must not repeat verified handler")
	require.NoError(t, r.Get(ctx, key.NamespacedName, obj))
	require.Equal(t, api.ActionStateSucceeded, obj.Status.ActionState)
	require.Equal(t, []api.NSXWorkflowStatus{{VDNName: "source-vdn", WorkflowID: "stable-workflow"}}, obj.Status.NSXWorkflows)
	require.Equal(t, api.LoadBalancerProvider("HAPROXY"), obj.Status.DetectedSourceLBProvider)
	lock, err = r.Recovery.ReadLock(ctx)
	require.NoError(t, err)
	require.Nil(t, lock.Holder)
}

func TestPlanRequiresFreshnessAdmissionIntegration(t *testing.T) {
	ctx := context.Background()
	r, key, _ := recoveryController(t, api.ActionPlan)
	r.Engine.Handlers[transition.Key{Stage: r.Stage, Action: api.ActionPlan}] = actionHandlerFunc(func(context.Context, transition.Request) (transition.Result, error) {
		t.Fatal("Plan without freshness admission")
		return transition.Result{}, nil
	})
	_, err := r.Reconcile(ctx, key)
	require.ErrorContains(t, err, "freshness admission")
}

func TestFinalizerPrecedesHandlerAndSurvivesFailure(t *testing.T) {
	ctx := context.Background()
	r, key, obj := recoveryController(t, api.ActionEvaluate)
	obj.Finalizers = nil
	require.NoError(t, r.Update(ctx, obj))
	calls := 0
	r.Engine.Handlers[transition.Key{Stage: r.Stage, Action: api.ActionEvaluate}] = actionHandlerFunc(func(ctx context.Context, req transition.Request) (transition.Result, error) {
		calls++
		var live api.ControlPlaneNetworkTransition
		require.NoError(t, r.Get(ctx, key.NamespacedName, &live))
		require.Contains(t, live.Finalizers, recovery.Finalizer)
		return transition.Result{}, errors.New("uncertain external outcome")
	})
	_, err := r.Reconcile(ctx, key)
	require.NoError(t, err)
	require.Zero(t, calls)
	_, err = r.Reconcile(ctx, key)
	require.ErrorContains(t, err, "uncertain external outcome")
	require.Equal(t, 1, calls)
	require.NoError(t, r.Get(ctx, key.NamespacedName, obj))
	require.NoError(t, r.Delete(ctx, obj))
	_, err = r.Reconcile(ctx, key)
	require.Error(t, err)
	require.NoError(t, r.Get(ctx, key.NamespacedName, obj))
	require.Contains(t, obj.Finalizers, recovery.Finalizer)
	owned, err := r.Recovery.OwnsLock(ctx, obj.UID)
	require.NoError(t, err)
	require.True(t, owned)
}

func TestNextActionFinishesPreviousDurableRelease(t *testing.T) {
	ctx := context.Background()
	r, key, obj := recoveryController(t, api.ActionEvaluate)
	session, err := r.Recovery.Acquire(ctx, identity(obj, api.ActionEvaluate), recovery.Scope{AllNamespaces: true})
	require.NoError(t, err)
	require.NoError(t, session.VerifyCompletion(ctx, nil))
	obj.Status.ActionState = api.ActionStateSucceeded
	require.NoError(t, r.Status().Update(ctx, obj))
	// Simulate a client requesting Plan between CR success and holder release.
	obj.Spec.Action = api.ActionPlan
	obj.Generation++
	require.NoError(t, r.Update(ctx, obj))
	r.ConfirmFreshnessAdmission = func(context.Context) error { return nil }
	calls := 0
	r.Engine.Handlers[transition.Key{Stage: r.Stage, Action: api.ActionPlan}] = actionHandlerFunc(func(context.Context, transition.Request) (transition.Result, error) {
		calls++
		return transition.Result{Outcome: transition.OutcomeSucceeded}, nil
	})
	_, err = r.Reconcile(ctx, key)
	require.NoError(t, err)
	lock, err := r.Recovery.ReadLock(ctx)
	require.NoError(t, err)
	require.Nil(t, lock.Holder)
	_, err = r.Reconcile(ctx, key)
	require.NoError(t, err)
	require.Equal(t, 1, calls)
}

func TestDeletingTransitionCompletesRecoveryBeforeFinalizerRemoval(t *testing.T) {
	ctx := context.Background()
	r, key, obj := recoveryController(t, api.ActionEvaluate)
	fail := true
	r.Engine.Handlers[transition.Key{Stage: r.Stage, Action: api.ActionEvaluate}] = actionHandlerFunc(func(context.Context, transition.Request) (transition.Result, error) {
		if fail {
			return transition.Result{}, errors.New("recovery pending")
		}
		return transition.Result{Outcome: transition.OutcomeSucceeded}, nil
	})
	_, err := r.Reconcile(ctx, key)
	require.Error(t, err)
	require.NoError(t, r.Get(ctx, key.NamespacedName, obj))
	require.NoError(t, r.Delete(ctx, obj))
	fail = false
	result, err := r.Reconcile(ctx, key)
	require.NoError(t, err)
	require.Positive(t, result.RequeueAfter)
	require.NoError(t, r.Get(ctx, key.NamespacedName, obj))
	require.Contains(t, obj.Finalizers, recovery.Finalizer)
	lock, err := r.Recovery.ReadLock(ctx)
	require.NoError(t, err)
	require.Nil(t, lock.Holder)
	_, err = r.Reconcile(ctx, key)
	require.NoError(t, err)
	err = r.Get(ctx, key.NamespacedName, obj)
	require.True(t, apierrors.IsNotFound(err), "%v", err)
	journals, err := r.Recovery.History(ctx, "infra")
	require.NoError(t, err)
	require.Len(t, journals, 1)
	require.True(t, journals[0].StatusPersisted)
}
