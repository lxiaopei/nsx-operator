/*
Copyright © 2026 Broadcom, Inc. All Rights Reserved.

	SPDX-License-Identifier: Apache-2.0
*/
package infrastructure

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/vmware-tanzu/nsx-operator/pkg/apis/migration/v1alpha1"
	"github.com/vmware-tanzu/nsx-operator/pkg/migration/recovery"
)

type readinessFunc func(context.Context, bool, string) error

func (f readinessFunc) Check(ctx context.Context, allow bool, path string) error {
	return f(ctx, allow, path)
}

type projectFunc func(context.Context, string) (Project, error)

func (f projectFunc) GetProject(ctx context.Context, id string) (Project, error) { return f(ctx, id) }

func setup(t *testing.T) (*Service, *api.ControlPlaneNetworkTransition) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, api.AddToScheme(scheme))
	obj := &api.ControlPlaneNetworkTransition{ObjectMeta: metav1.ObjectMeta{Name: api.ControlPlaneNetworkTransitionName, UID: "infra", Generation: 1}, Spec: api.ControlPlaneNetworkTransitionSpec{TransitionSpec: api.TransitionSpec{SourceMode: "VSPHERE_VDS", TargetMode: "NSX_VPC", Action: api.ActionEvaluate}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(obj).Build()
	service := &Service{Recovery: &recovery.Store{Client: c, Reader: c}, Readiness: readinessFunc(func(context.Context, bool, string) error { return nil }), Projects: projectFunc(func(_ context.Context, id string) (Project, error) {
		return Project{ID: id, Path: "/orgs/default/projects/" + id, UniqueID: "unique-" + id}, nil
	})}
	return service, obj
}
func acquire(t *testing.T, s *Service, obj *api.ControlPlaneNetworkTransition) *recovery.Session {
	t.Helper()
	session, err := s.Recovery.Acquire(context.Background(), recovery.Identity{UID: obj.UID, Kind: "ControlPlaneNetworkTransition", Name: obj.Name, Action: string(obj.Spec.Action), Generation: obj.Generation}, recovery.Scope{AllNamespaces: true})
	require.NoError(t, err)
	return session
}

func TestProjectSelectionAndStatus(t *testing.T) {
	for _, id := range []string{"", "tenant-project"} {
		t.Run(id, func(t *testing.T) {
			service, obj := setup(t)
			obj.Spec.NSXProjectID = id
			require.NoError(t, service.Check(context.Background(), obj))
			want := id
			if want == "" {
				want = "default"
			}
			require.Equal(t, "/orgs/default/projects/"+want, obj.Status.TargetNSXProject)
			require.True(t, meta.IsStatusConditionTrue(obj.Status.Conditions, ConditionReady))
			require.Empty(t, obj.Status.LastAction, "start prerequisites do not complete Evaluate")
			require.Empty(t, obj.Status.PrimaryVPCPath)
			require.Empty(t, obj.Status.NSXWorkflows)
			require.Empty(t, obj.Status.LBSNATReservations)
		})
	}
	for _, id := range []string{"/orgs/default/projects/x", "..", ".", " x", "x/child", "x%2fy", "x?query", "x\n"} {
		_, err := ProjectID(id)
		require.Error(t, err, id)
	}
}

func TestPrerequisitesGateProjectReadsAndRetainRecovery(t *testing.T) {
	service, obj := setup(t)
	obj.Status = &api.ControlPlaneNetworkTransitionStatus{TransitionStatus: api.TransitionStatus{NSXWorkflows: []api.NSXWorkflowStatus{{VDNName: "network", WorkflowID: "recorded-workflow"}}, LastAction: api.ActionDerive, ActionState: api.ActionStateFailed, ActionGeneration: 1}, PrimaryVPCPath: "/existing-vpc"}
	service.Readiness = readinessFunc(func(context.Context, bool, string) error { return issue("NCPNotReady", "Wait for NCP") })
	service.Projects = projectFunc(func(context.Context, string) (Project, error) {
		t.Fatal("project read before readiness")
		return Project{}, nil
	})
	require.Error(t, service.Check(context.Background(), obj))
	require.Equal(t, "NCPNotReady", meta.FindStatusCondition(obj.Status.Conditions, ConditionReady).Reason)
	require.Equal(t, "recorded-workflow", obj.Status.NSXWorkflows[0].WorkflowID)
	require.Equal(t, "/existing-vpc", obj.Status.PrimaryVPCPath)
	require.Equal(t, api.ActionStateFailed, obj.Status.ActionState)
	service.Readiness = readinessFunc(func(context.Context, bool, string) error {
		return readFailure("Cannot read provider", errors.New("forbidden"))
	})
	require.Error(t, service.Check(context.Background(), obj))
	require.Equal(t, metav1.ConditionUnknown, meta.FindStatusCondition(obj.Status.Conditions, ConditionReady).Status)
}

func TestProjectIdentityBoundAcrossRestart(t *testing.T) {
	ctx := context.Background()
	service, obj := setup(t)
	require.NoError(t, service.Check(ctx, obj))
	session := acquire(t, service, obj)
	require.NoError(t, service.Bind(ctx, obj, session))
	// Binding survives a lost status update and a new service/process instance.
	obj.Status = nil
	restarted := *service
	require.NoError(t, restarted.Check(ctx, obj))
	session = acquire(t, &restarted, obj)
	require.NoError(t, restarted.Bind(ctx, obj, session))
	history, err := session.History(ctx)
	require.NoError(t, err)
	require.Len(t, history, 1)
	require.Len(t, history[0].Checkpoints, 1)
	restarted.Projects = projectFunc(func(_ context.Context, id string) (Project, error) {
		return Project{ID: id, Path: "/orgs/default/projects/" + id, UniqueID: "replacement"}, nil
	})
	require.Error(t, restarted.Check(ctx, obj))
	require.Equal(t, "RecoveryIdentityConflict", meta.FindStatusCondition(obj.Status.Conditions, ConditionReady).Reason)
}

func TestProjectCorrectionBeforeEffectsOnly(t *testing.T) {
	for _, effects := range []bool{false, true} {
		t.Run(map[bool]string{false: "no effects", true: "uncertain effects"}[effects], func(t *testing.T) {
			ctx := context.Background()
			service, obj := setup(t)
			require.NoError(t, service.Check(ctx, obj))
			session := acquire(t, service, obj)
			require.NoError(t, service.Bind(ctx, obj, session))
			if effects {
				err := session.Effect(ctx, "container", "/recorded-vpc", func(context.Context, string) (recovery.Observation, error) { return recovery.Pending, nil }, func(context.Context, string) error { t.Fatal("unknown outcome must not create"); return nil })
				require.ErrorIs(t, err, recovery.ErrPending)
			}
			obj.Generation++
			obj.Spec.NSXProjectID = "tenant-project"
			err := service.Check(ctx, obj)
			if effects {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			session = acquire(t, service, obj)
			require.NoError(t, service.Bind(ctx, obj, session))
			obj.Status.PrimaryVPCPath = "/orgs/default/projects/tenant-project/vpcs/primary"
			require.NoError(t, service.Check(ctx, obj), "old unused project discovery must not override the new accepted identity")
		})
	}
}

func TestNSXLookupFailureDoesNotFallbackOrEraseIdentity(t *testing.T) {
	service, obj := setup(t)
	obj.Spec.NSXProjectID = "tenant-project"
	obj.Status = &api.ControlPlaneNetworkTransitionStatus{TargetNSXProject: "/orgs/default/projects/tenant-project", LoadBalancerVPCPath: "/existing-lb-vpc"}
	calls := 0
	service.Projects = projectFunc(func(_ context.Context, id string) (Project, error) {
		calls++
		require.Equal(t, "tenant-project", id)
		return Project{}, &Issue{Reason: "NSXProjectReadFailed", Message: "Cannot read NSX project", Unknown: true}
	})
	require.Error(t, service.Check(context.Background(), obj))
	require.Equal(t, 1, calls)
	require.Equal(t, "/existing-lb-vpc", obj.Status.LoadBalancerVPCPath)
	require.Equal(t, "/orgs/default/projects/tenant-project", obj.Status.TargetNSXProject)
}

func TestResumeAllowsVPCOnlyAfterExecuteCheckpoint(t *testing.T) {
	service, obj := setup(t)
	allow := false
	service.Readiness = readinessFunc(func(_ context.Context, vpc bool, _ string) error { allow = vpc; return nil })
	require.NoError(t, service.Check(context.Background(), obj))
	require.False(t, allow)
	obj.Status.LastAction = api.ActionExecute
	obj.Status.ActionState = api.ActionStateFailed
	require.NoError(t, service.Check(context.Background(), obj))
	require.True(t, allow)
}

func TestLegacyEffectsRequireVerifiedProjectAdoption(t *testing.T) {
	service, obj := setup(t)
	obj.Status = &api.ControlPlaneNetworkTransitionStatus{TargetNSXProject: "/orgs/default/projects/default", PrimaryVPCPath: "/existing-primary"}
	require.Error(t, service.Check(context.Background(), obj))
	require.Equal(t, "RecoveryIdentityMissing", meta.FindStatusCondition(obj.Status.Conditions, ConditionReady).Reason)
	require.Equal(t, "/existing-primary", obj.Status.PrimaryVPCPath)
}

func TestReadinessChangeUnderLockUpdatesCondition(t *testing.T) {
	ctx := context.Background()
	service, obj := setup(t)
	require.NoError(t, service.Check(ctx, obj))
	service.Readiness = readinessFunc(func(context.Context, bool, string) error { return issue("NCPNotReady", "NCP restarted") })
	require.Error(t, service.Bind(ctx, obj, acquire(t, service, obj)))
	require.Equal(t, "NCPNotReady", meta.FindStatusCondition(obj.Status.Conditions, ConditionReady).Reason)
	require.False(t, meta.IsStatusConditionTrue(obj.Status.Conditions, ConditionReady))
}
