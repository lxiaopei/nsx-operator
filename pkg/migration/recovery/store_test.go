/*
Copyright © 2026 Broadcom, Inc. All Rights Reserved.

	SPDX-License-Identifier: Apache-2.0
*/
package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	api "github.com/vmware-tanzu/nsx-operator/pkg/apis/migration/v1alpha1"
)

func testStore(t *testing.T) (*Store, Identity, Scope) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, api.AddToScheme(scheme))
	obj := &api.NamespaceNetworkTransition{ObjectMeta: metav1.ObjectMeta{Name: "batch", UID: "owner", Generation: 4}, Spec: api.NamespaceNetworkTransitionSpec{TransitionSpec: api.TransitionSpec{Action: api.ActionExecute}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(obj).WithObjects(obj).Build()
	return &Store{Client: c, Reader: c}, Identity{UID: "owner", Name: "batch", Kind: "NamespaceNetworkTransition", Action: "Execute", Generation: 4}, Scope{NamespaceUIDs: []types.UID{"tenant-uid"}}
}

func TestLockOwnershipAndFencing(t *testing.T) {
	ctx := context.Background()
	store, id, scope := testStore(t)
	first, err := store.Acquire(ctx, id, scope)
	require.NoError(t, err)
	other := id
	other.UID = "other"
	_, err = store.Acquire(ctx, other, scope)
	require.ErrorIs(t, err, ErrBusy)
	// A restart adopts the SAME durable operation, with a new fencing token.
	restarted := &Store{Client: store.Client, Reader: store.Reader}
	second, err := restarted.Acquire(ctx, id, scope)
	require.NoError(t, err)
	require.ErrorIs(t, first.Check(ctx), ErrFenced)
	require.NoError(t, second.Check(ctx))
	require.Equal(t, first.Identity().ID(), second.Identity().ID())
	require.NoError(t, second.Protect(ctx, func(context.Context, Lock) error { return nil }))
	rollback := id
	rollback.Action = "Rollback"
	rollback.Generation++
	r, err := store.Acquire(ctx, rollback, scope)
	require.NoError(t, err)
	require.ErrorIs(t, second.Check(ctx), ErrFenced)
	lock, err := store.ReadLock(ctx)
	require.NoError(t, err)
	require.True(t, lock.AdmissionProtectionActive)
	require.NoError(t, r.Check(ctx))
	// Neither another UID nor a different scope may inherit protection.
	_, err = store.Acquire(ctx, other, scope)
	require.ErrorIs(t, err, ErrBusy)
	_, err = store.Acquire(ctx, rollback, Scope{AllNamespaces: true})
	require.ErrorContains(t, err, "scope")
}

func TestProtectionAndReleaseOrdering(t *testing.T) {
	ctx := context.Background()
	store, id, scope := testStore(t)
	session, err := store.Acquire(ctx, id, scope)
	require.NoError(t, err)
	require.ErrorIs(t, session.Protect(ctx, nil), ErrProtection)
	lock, err := store.ReadLock(ctx)
	require.NoError(t, err)
	require.True(t, lock.AdmissionProtectionActive)
	require.ErrorContains(t, store.Finish(ctx, id), "unverified")
	require.NoError(t, session.VerifyCompletion(ctx, nil))
	// Inject a crash after completion, before protection is cleared.
	base := store.Client
	fail := true
	store.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if obj.GetName() == LockName && fail {
			var l Lock
			_, e := store.read(ctx, LockName, &l)
			require.NoError(t, e)
			if l.Checkpoint == "Completed" {
				fail = false
				return errors.New("crash before protection release")
			}
		}
		return c.Update(ctx, obj, opts...)
	}})
	require.Error(t, store.Finish(ctx, id))
	lock, err = store.ReadLock(ctx)
	require.NoError(t, err)
	require.True(t, lock.AdmissionProtectionActive)
	require.NotNil(t, lock.Holder)
	var j Journal
	_, err = store.read(ctx, id.ID(), &j)
	require.NoError(t, err)
	require.True(t, j.StatusPersisted)
	// Restart resumes release without repeating external effects or losing history.
	store.Client = base
	require.NoError(t, store.Finish(ctx, id))
	lock, err = store.ReadLock(ctx)
	require.NoError(t, err)
	require.False(t, lock.AdmissionProtectionActive)
	require.Nil(t, lock.Holder)
	_, err = store.read(ctx, id.ID(), &j)
	require.NoError(t, err)
	require.True(t, j.Verified)
	require.NoError(t, store.Finish(ctx, id))
}

func TestEffectRecoversLostReplyWithoutDuplicate(t *testing.T) {
	ctx := context.Background()
	store, id, scope := testStore(t)
	session, err := store.Acquire(ctx, id, scope)
	require.NoError(t, err)
	created := false
	calls := 0
	probe := func(context.Context, string) (Observation, error) {
		if created {
			return Applied, nil
		}
		return Absent, nil
	}
	apply := func(ctx context.Context, key string) error {
		var j Journal
		_, e := store.read(ctx, id.ID(), &j)
		require.NoError(t, e)
		require.Equal(t, "Intent", j.Steps["create"].State, "intent must precede external call")
		require.Equal(t, id.ID()+"/create", key)
		calls++
		created = true
		return errors.New("reply lost")
	}
	require.ErrorContains(t, session.Effect(ctx, "create", "/vpc/stable#payload", probe, apply), "reply lost")
	require.ErrorIs(t, session.VerifyCompletion(ctx, nil), ErrPending)
	session, err = store.Acquire(ctx, id, scope)
	require.NoError(t, err)
	require.NoError(t, session.Effect(ctx, "create", "/vpc/stable#payload", probe, apply))
	require.Equal(t, 1, calls)
	require.NoError(t, session.Effect(ctx, "create", "/vpc/stable#payload", probe, apply))
	require.Equal(t, 1, calls)
	require.ErrorContains(t, session.Effect(ctx, "create", "/vpc/different", probe, apply), "changed")
	require.NoError(t, session.VerifyCompletion(ctx, nil))
}

func TestUnknownOutcomeNeverReissuesMutation(t *testing.T) {
	for _, state := range []Observation{Pending, "Unknown", ""} {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			store, id, scope := testStore(t)
			session, err := store.Acquire(ctx, id, scope)
			require.NoError(t, err)
			err = session.Effect(ctx, "create", "target", func(context.Context, string) (Observation, error) { return state, nil }, func(context.Context, string) error { t.Fatal("must not apply uncertain effect"); return nil })
			require.ErrorIs(t, err, ErrPending)
		})
	}
}

func TestIntentWriteFailurePreventsExternalCall(t *testing.T) {
	ctx := context.Background()
	store, id, scope := testStore(t)
	session, err := store.Acquire(ctx, id, scope)
	require.NoError(t, err)
	store.Client = interceptor.NewClient(store.Client.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.UpdateOption) error {
		return errors.New("journal unavailable")
	}})
	err = session.Effect(ctx, "create", "target", func(context.Context, string) (Observation, error) {
		t.Fatal("probe before intent persisted")
		return Absent, nil
	}, func(context.Context, string) error { t.Fatal("apply before intent persisted"); return nil })
	require.ErrorContains(t, err, "journal unavailable")
}

func TestPreparationRollbackDoesNotRequireProtection(t *testing.T) {
	ctx := context.Background()
	store, id, scope := testStore(t)
	id.Action = "Derive"
	_, err := store.Acquire(ctx, id, scope)
	require.NoError(t, err)
	cutover, err := store.HadCutover(ctx, id.UID)
	require.NoError(t, err)
	require.False(t, cutover)
	id.Action = "Rollback"
	id.Generation++
	_, err = store.Acquire(ctx, id, scope)
	require.NoError(t, err)
	lock, err := store.ReadLock(ctx)
	require.NoError(t, err)
	require.False(t, lock.AdmissionProtectionActive)
}

func TestCorrectedEvaluateRetainsPriorJournal(t *testing.T) {
	ctx := context.Background()
	store, id, scope := testStore(t)
	id.Action = "Evaluate"
	session, err := store.Acquire(ctx, id, scope)
	require.NoError(t, err)
	require.NoError(t, session.Checkpoint(ctx, "test", json.RawMessage(`{"value":1}`)))
	old := id
	id.Generation++
	_, err = store.Acquire(ctx, id, scope)
	require.NoError(t, err)
	var j Journal
	_, err = store.read(ctx, old.ID(), &j)
	require.NoError(t, err)
	id.Action = "Derive"
	_, err = store.Acquire(ctx, id, scope)
	require.ErrorIs(t, err, ErrBusy)
}

func TestCorruptAuthorityAndCancelledSessionFailClosed(t *testing.T) {
	ctx := context.Background()
	store, id, scope := testStore(t)
	session, err := store.Acquire(ctx, id, scope)
	require.NoError(t, err)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, session.Check(cancelled), context.Canceled)
	cm := &corev1.ConfigMap{}
	require.NoError(t, store.Client.Get(ctx, client.ObjectKey{Namespace: Namespace, Name: LockName}, cm))
	cm.Data["record"] = "bad json"
	require.NoError(t, store.Client.Update(ctx, cm))
	_, err = store.Acquire(ctx, id, scope)
	require.Error(t, err)
	require.Error(t, session.Protect(ctx, func(context.Context, Lock) error { t.Fatal("corrupt lock"); return nil }))
}

func TestAcquireCASRejectsStaleDecision(t *testing.T) {
	ctx := context.Background()
	store, id, scope := testStore(t)
	session, err := store.Acquire(ctx, id, scope)
	require.NoError(t, err)
	require.NoError(t, session.VerifyCompletion(ctx, nil))
	require.NoError(t, store.Finish(ctx, id))
	// Both contenders read the same unowned resourceVersion. Only one may update it.
	var snapshot corev1.ConfigMap
	require.NoError(t, store.Client.Get(ctx, client.ObjectKey{Namespace: Namespace, Name: LockName}, &snapshot))
	other := id
	other.UID = "another"
	_, err = store.Acquire(ctx, other, scope)
	require.NoError(t, err)
	stale := interceptor.NewClient(store.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if key.Name == LockName {
			snapshot.DeepCopyInto(obj.(*corev1.ConfigMap))
			return nil
		}
		return c.Get(ctx, key, obj, opts...)
	}})
	contender := &Store{Client: store.Client, Reader: stale}
	_, err = contender.Acquire(ctx, id, scope)
	require.True(t, apierrors.IsConflict(err), "%v", err)
	lock, err := store.ReadLock(ctx)
	require.NoError(t, err)
	require.Equal(t, other.UID, lock.Holder.UID)
}

func TestRecoveryCheckpointSurvivesRollback(t *testing.T) {
	ctx := context.Background()
	store, id, scope := testStore(t)
	session, err := store.Acquire(ctx, id, scope)
	require.NoError(t, err)
	value := []byte(`{"originalBacking":"dvpg-1","provenance":"Adopted"}`)
	require.NoError(t, session.Checkpoint(ctx, "backing", value))
	require.ErrorContains(t, session.Checkpoint(ctx, "backing", []byte(`{"originalBacking":"dvpg-2"}`)), "replace")
	id.Action = "Rollback"
	id.Generation++
	session, err = store.Acquire(ctx, id, scope)
	require.NoError(t, err)
	journals, err := session.History(ctx)
	require.NoError(t, err)
	found := false
	for _, j := range journals {
		if j.Identity.Action == "Execute" {
			require.JSONEq(t, string(value), string(j.Checkpoints["backing"]))
			found = true
		}
	}
	require.True(t, found)
}

func TestIrreversibleEffectRequiresPersistedCheckpoint(t *testing.T) {
	ctx := context.Background()
	store, id, scope := testStore(t)
	var obj api.NamespaceNetworkTransition
	require.NoError(t, store.Client.Get(ctx, client.ObjectKey{Name: id.Name}, &obj))
	obj.Spec.Action = api.ActionCommit
	require.NoError(t, store.Client.Update(ctx, &obj))
	id.Action = "Commit"
	session, err := store.Acquire(ctx, id, scope)
	require.NoError(t, err)
	applied := false
	probe := func(context.Context, string) (Observation, error) {
		if applied {
			return Applied, nil
		}
		return Absent, nil
	}
	apply := func(context.Context, string) error { applied = true; return nil }
	require.ErrorContains(t, session.Effect(ctx, "delete", "workflow-id", probe, apply), "persisted")
	require.False(t, applied)
	obj.Status = &api.NamespaceNetworkTransitionStatus{TransitionStatus: api.TransitionStatus{IrreversibleAction: "Commit"}}
	require.NoError(t, store.Client.Status().Update(ctx, &obj))
	require.NoError(t, session.Effect(ctx, "delete", "workflow-id", probe, apply))
	require.True(t, applied)
}
