/* Copyright © 2026 Broadcom, Inc. All Rights Reserved.
   SPDX-License-Identifier: Apache-2.0 */

// Package recovery persists migration serialization, protection and operation journals.
// These private, versioned ConfigMaps are not additional user-facing transition APIs.
package recovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/vmware-tanzu/nsx-operator/pkg/apis/migration/v1alpha1"
)

const (
	Namespace  = "vmware-system-nsx"
	LockName   = "supervisor-network-migration-lock"
	Finalizer  = "migration.network.vmware.com/recovery"
	ownerLabel = "migration.network.vmware.com/holder"
)

var (
	ErrBusy       = errors.New("another migration operation holds the mutation lock")
	ErrFenced     = errors.New("migration session no longer owns the mutation lock")
	ErrPending    = errors.New("external operation outcome is unresolved")
	ErrProtection = errors.New("admission protection has not been confirmed")
)

// Scope uses namespace UIDs, not names that can be deleted and reused. Infrastructure
// protection conservatively covers all namespaces until handlers supply narrower scopes.
type Scope struct {
	AllNamespaces bool        `json:"allNamespaces,omitempty"`
	NamespaceUIDs []types.UID `json:"namespaceUIDs,omitempty"`
}

// Identity is stable across retries and process replacement. Generation separates
// corrected Evaluate requests; UID prevents a recreated CR inheriting old work.
type Identity struct {
	UID        types.UID `json:"uid"`
	Name       string    `json:"name"`
	Kind       string    `json:"kind"`
	Action     string    `json:"action"`
	Generation int64     `json:"generation"`
}

func (i Identity) ID() string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%s/%s/%d", i.Kind, i.UID, i.Action, i.Generation)))
	return "migration-op-" + hex.EncodeToString(sum[:16])
}

// Lock is the sole authority for serialization AND admission protection. Journals
// are audit/recovery records, never an independent source of admission decisions.
// Fence increases on every session acquisition, including same-owner restart.
type Lock struct {
	Version                   int       `json:"version"`
	Holder                    *Identity `json:"holder,omitempty"`
	Fence                     uint64    `json:"fence"`
	Scope                     Scope     `json:"scope"`
	AdmissionProtectionActive bool      `json:"admissionProtectionActive"`
	Checkpoint                string    `json:"checkpoint,omitempty"`
}

type Step struct {
	Target string `json:"target"`
	State  string `json:"state"`
}

type Journal struct {
	Completion      json.RawMessage            `json:"completion,omitempty"`
	Version         int                        `json:"version"`
	Identity        Identity                   `json:"identity"`
	Steps           map[string]Step            `json:"steps"`
	Checkpoints     map[string]json.RawMessage `json:"checkpoints,omitempty"`
	Verified        bool                       `json:"verified"`
	StatusPersisted bool                       `json:"statusPersisted"`
}

// Store always reads through the uncached Reader. Updates use resourceVersion CAS;
// conflicts are returned to the controller, never retried with a stale decision.
type Store struct {
	Client client.Client
	Reader client.Reader
}

func (s *Store) reader() client.Reader {
	if s.Reader != nil {
		return s.Reader
	}
	return s.Client
}

func (s *Store) read(ctx context.Context, name string, value any) (*corev1.ConfigMap, error) {
	cm := &corev1.ConfigMap{}
	if err := s.reader().Get(ctx, types.NamespacedName{Namespace: Namespace, Name: name}, cm); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(cm.Data["record"]), value); err != nil {
		return nil, fmt.Errorf("invalid recovery record %s: %w", name, err)
	}
	switch record := value.(type) {
	case *Journal:
		if err := validateJournal(*record); err != nil {
			return nil, err
		}
	case *Lock:
		if record.Version != 1 || (record.AdmissionProtectionActive && (record.Holder == nil || (!record.Scope.AllNamespaces && len(record.Scope.NamespaceUIDs) == 0))) {
			return nil, errors.New("invalid migration lock")
		}
	}
	return cm, nil
}

func validateJournal(j Journal) error {
	if j.Version != 1 || j.Identity.UID == "" || j.Identity.Name == "" || j.Identity.Generation <= 0 || (j.StatusPersisted && !j.Verified) {
		return errors.New("invalid migration journal")
	}
	for name, step := range j.Steps {
		if name == "" || step.Target == "" || (step.State != "Intent" && step.State != "Applied") || (j.Verified && step.State != "Applied") {
			return errors.New("invalid migration effect checkpoint")
		}
	}
	return nil
}

func (s *Store) write(ctx context.Context, cm *corev1.ConfigMap, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	// Leave headroom below the API-server ConfigMap size limit. Never truncate history.
	if len(b) > 900*1024 {
		return errors.New("migration journal capacity exceeded; retain evidence and investigate")
	}
	cm.Data = map[string]string{"record": string(b)}
	if cm.ResourceVersion == "" {
		return s.Client.Create(ctx, cm)
	}
	return s.Client.Update(ctx, cm)
}

func configMap(name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: Namespace}}
}

// ReadLock is shared with admission integration. Missing/corrupt authority is an
// error; callers must not interpret it as an inactive protection flag.
func (s *Store) ReadLock(ctx context.Context) (Lock, error) {
	var lock Lock
	_, err := s.read(ctx, LockName, &lock)
	if err == nil && (lock.Version != 1 || (lock.Holder == nil && lock.AdmissionProtectionActive)) {
		err = errors.New("invalid migration lock")
	}
	return lock, err
}

// Acquire serializes all Supervisor mutations, a conservative superset of shared
// DVS/VPC scopes. There is deliberately no expiry or timeout-based takeover.
func (s *Store) Acquire(ctx context.Context, id Identity, scope Scope) (*Session, error) {
	if id.UID == "" || id.Name == "" || id.Kind == "" || id.Generation <= 0 {
		return nil, errors.New("incomplete migration operation identity")
	}
	var lock Lock
	cm, err := s.read(ctx, LockName, &lock)
	if apierrors.IsNotFound(err) {
		cm = configMap(LockName)
		lock.Version = 1
	} else if err != nil {
		return nil, err
	}
	if lock.Version != 1 || (lock.Holder == nil && lock.AdmissionProtectionActive) {
		return nil, errors.New("invalid migration lock")
	}
	if lock.Holder != nil {
		old := lock.Holder
		if old.UID != id.UID || old.Kind != id.Kind {
			return nil, ErrBusy
		}
		if old.ID() != id.ID() {
			// Namespace rollback retains protection and journal evidence from interrupted
			// work. Corrected Evaluate may replace only unprotected Evaluate work.
			rollback := id.Action == "Rollback" && id.Kind == "NamespaceNetworkTransition"
			correction := id.Action == "Evaluate" && old.Action == "Evaluate" && !lock.AdmissionProtectionActive
			if !rollback && !correction {
				return nil, ErrBusy
			}
		}
		if (lock.AdmissionProtectionActive || old.ID() == id.ID()) && !reflect.DeepEqual(lock.Scope, scope) {
			return nil, errors.New("cannot change an operation or active admission protection scope")
		}
	}
	lock.Holder = &id
	lock.Scope = scope
	lock.Fence++
	lock.Checkpoint = "Running"
	if err = s.write(ctx, cm, lock); err != nil {
		return nil, err
	}
	session := &Session{store: s, identity: id, fence: lock.Fence}
	if err = session.ensureJournal(ctx); err != nil {
		return nil, err
	}
	return session, nil
}

// Session is a fenced handle. Handlers must use Effect for external mutations and
// honor cancellation. External services must also honor its stable idempotency key
// (or equivalent identity/ownership reconciliation); Kubernetes CAS cannot fence
// an already-in-flight NSX/vCenter request on its own.
type Session struct {
	store    *Store
	identity Identity
	fence    uint64
}

func (s *Session) Identity() Identity { return s.identity }

func (s *Session) lock(ctx context.Context) (*corev1.ConfigMap, Lock, error) {
	if err := ctx.Err(); err != nil {
		return nil, Lock{}, err
	}
	var lock Lock
	cm, err := s.store.read(ctx, LockName, &lock)
	if err != nil {
		return nil, lock, err
	}
	if lock.Version != 1 || lock.Holder == nil || lock.Holder.ID() != s.identity.ID() || lock.Fence != s.fence {
		return nil, lock, ErrFenced
	}
	return cm, lock, nil
}

func (s *Session) Check(ctx context.Context) error { _, _, err := s.lock(ctx); return err }

func (s *Session) ensureJournal(ctx context.Context) error {
	var journal Journal
	_, err := s.store.read(ctx, s.identity.ID(), &journal)
	if err == nil {
		if journal.Version != 1 || journal.Identity != s.identity {
			return errors.New("journal identity mismatch")
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	cm := configMap(s.identity.ID())
	cm.Labels = map[string]string{ownerLabel: string(s.identity.UID)}
	return s.store.write(ctx, cm, Journal{Version: 1, Identity: s.identity, Steps: map[string]Step{}})
}

// Protect persists protection before checking enforcement. A failed or absent
// verifier leaves protection and ownership intact and prevents handler invocation.
func (s *Session) Protect(ctx context.Context, verify func(context.Context, Lock) error) error {
	cm, lock, err := s.lock(ctx)
	if err != nil {
		return err
	}
	if !lock.Scope.AllNamespaces && len(lock.Scope.NamespaceUIDs) == 0 {
		return errors.New("admission protection requires an explicit scope")
	}
	if !lock.AdmissionProtectionActive {
		lock.AdmissionProtectionActive = true
		lock.Checkpoint = "Protected"
		if err = s.store.write(ctx, cm, lock); err != nil {
			return err
		}
	}
	if verify == nil {
		return ErrProtection
	}
	if err = verify(ctx, lock); err != nil {
		return fmt.Errorf("%w: %v", ErrProtection, err)
	}
	return s.Check(ctx)
}

// HadCutover identifies even uncertain Execute intents. Preparation-only rollback
// leaves admission inactive; once Execute was attempted, restoration is protected.
func (s *Store) HadCutover(ctx context.Context, uid types.UID) (bool, error) {
	journals, err := s.History(ctx, uid)
	if err != nil {
		return false, err
	}
	for _, j := range journals {
		if j.Identity.Action == "Execute" {
			return true, nil
		}
	}
	return false, nil
}

// History supplies prior intents and outcomes for rollback and corrected Evaluate.
// Consumers reconcile by recorded identities and must preserve adopted resources.
func (s *Store) History(ctx context.Context, uid types.UID) ([]Journal, error) {
	var cms corev1.ConfigMapList
	if err := s.reader().List(ctx, &cms, client.InNamespace(Namespace), client.MatchingLabels{ownerLabel: string(uid)}); err != nil {
		return nil, err
	}
	var journals []Journal
	for _, cm := range cms.Items {
		var j Journal
		if err := json.Unmarshal([]byte(cm.Data["record"]), &j); err != nil {
			return nil, err
		}
		if err := validateJournal(j); err != nil {
			return nil, err
		}
		if j.Identity.UID != uid {
			return nil, errors.New("migration journal owner mismatch")
		}
		journals = append(journals, j)
	}
	return journals, nil
}

func (s *Session) History(ctx context.Context) ([]Journal, error) {
	if err := s.Check(ctx); err != nil {
		return nil, err
	}
	return s.store.History(ctx, s.identity.UID)
}

// Observation distinguishes authoritative absence from pending/unknown outcomes.
// Probe errors or Unknown never authorize a new external call.
type Observation string

const (
	Absent  Observation = "Absent"
	Pending Observation = "Pending"
	Applied Observation = "Applied"
)

// Effect records intent before apply and outcome after authoritative observation.
// target is the immutable desired resource identity AND payload digest. Probe must
// verify ownership and desired state; an existing unrelated object is an error.
// The apply key is stable over retries. A lost reply is recovered by probing first.
func (s *Session) Effect(ctx context.Context, name, target string, probe func(context.Context, string) (Observation, error), apply func(context.Context, string) error) error {
	if name == "" || target == "" || probe == nil || apply == nil {
		return errors.New("incomplete external operation")
	}
	if err := s.Check(ctx); err != nil {
		return err
	}
	var j Journal
	cm, err := s.store.read(ctx, s.identity.ID(), &j)
	if err != nil {
		return err
	}
	if j.Verified {
		return errors.New("cannot mutate a verified operation")
	}
	step, exists := j.Steps[name]
	if exists && step.Target != target {
		return errors.New("external operation identity/payload changed")
	}
	if exists && step.State == "Applied" {
		return nil
	}
	if !exists {
		if j.Steps == nil {
			j.Steps = map[string]Step{}
		}
		j.Steps[name] = Step{Target: target, State: "Intent"}
		if err = s.store.write(ctx, cm, j); err != nil {
			return err
		}
	}
	key := s.identity.ID() + "/" + name
	observation, err := probe(ctx, key)
	if err != nil {
		return err
	}
	if observation == Absent {
		if err = s.Check(ctx); err != nil {
			return err
		}
		if err = s.checkRequest(ctx); err != nil {
			return err
		}
		if err = apply(ctx, key); err != nil {
			return err
		}
		observation, err = probe(ctx, key)
		if err != nil {
			return err
		}
	}
	if observation != Applied {
		return ErrPending
	}
	if err = s.Check(ctx); err != nil {
		return err
	}
	j.Steps[name] = Step{Target: target, State: "Applied"}
	return s.store.write(ctx, cm, j)
}

// VerifyCompletion must follow the handler's live consistency checks. It rejects
// unresolved effects and durably records verification before CR status success.
func (s *Session) VerifyCompletion(ctx context.Context, completion json.RawMessage) error {
	if err := s.Check(ctx); err != nil {
		return err
	}
	var j Journal
	cm, err := s.store.read(ctx, s.identity.ID(), &j)
	if err != nil {
		return err
	}
	for _, step := range j.Steps {
		if step.State != "Applied" {
			return ErrPending
		}
	}
	if j.Verified {
		return nil
	}
	if len(completion) > 0 && !json.Valid(completion) {
		return errors.New("invalid completion snapshot")
	}
	j.Completion = append(json.RawMessage(nil), completion...)
	j.Verified = true
	return s.store.write(ctx, cm, j)
}

// Finish runs only after CR success has been persisted, including restart recovery.
// Ordering: durable completion -> protection off -> holder release. History remains.
func (s *Store) Finish(ctx context.Context, id Identity) error {
	var lock Lock
	cm, err := s.read(ctx, LockName, &lock)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if lock.Version != 1 || (lock.Holder == nil && lock.AdmissionProtectionActive) {
		return errors.New("invalid migration lock")
	}
	if lock.Holder == nil || lock.Holder.ID() != id.ID() {
		return nil
	}
	var j Journal
	journalCM, err := s.read(ctx, id.ID(), &j)
	if err != nil {
		return err
	}
	if !j.Verified {
		return errors.New("cannot release unverified migration operation")
	}
	if !j.StatusPersisted {
		j.StatusPersisted = true
		if err = s.write(ctx, journalCM, j); err != nil {
			return err
		}
	}
	lock.Checkpoint = "Completed"
	if err = s.write(ctx, cm, lock); err != nil {
		return err
	}
	if lock.AdmissionProtectionActive {
		lock.AdmissionProtectionActive = false
		if err = s.write(ctx, cm, lock); err != nil {
			return err
		}
	}
	lock.Holder = nil
	lock.Scope = Scope{}
	return s.write(ctx, cm, lock)
}

// OwnsLock prevents CR deletion while recovery evidence is still active.
func (s *Store) OwnsLock(ctx context.Context, uid types.UID) (bool, error) {
	lock, err := s.ReadLock(ctx)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return lock.Holder != nil && lock.Holder.UID == uid, nil
}

// Verified lets a restarted dispatcher publish already-verified success without
// repeating a handler when its previous CR status write failed.
func (s *Session) Verified(ctx context.Context) (bool, error) {
	if err := s.Check(ctx); err != nil {
		return false, err
	}
	var j Journal
	_, err := s.store.read(ctx, s.identity.ID(), &j)
	return j.Verified, err
}

// Checkpoint preserves handler-specific recovery data (original backing, resource
// provenance, consumer identities) before effects. Values are immutable and must
// not contain credentials. A later revision uses a distinct operation/name.
func (s *Session) Checkpoint(ctx context.Context, name string, value json.RawMessage) error {
	if name == "" || !json.Valid(value) {
		return errors.New("invalid recovery checkpoint")
	}
	if err := s.Check(ctx); err != nil {
		return err
	}
	var j Journal
	cm, err := s.store.read(ctx, s.identity.ID(), &j)
	if err != nil {
		return err
	}
	if old, ok := j.Checkpoints[name]; ok {
		var a, b any
		if err = json.Unmarshal(old, &a); err != nil {
			return err
		}
		if err = json.Unmarshal(value, &b); err != nil {
			return err
		}
		if !reflect.DeepEqual(a, b) {
			return errors.New("cannot replace a durable recovery checkpoint")
		}
		return nil
	}
	if j.Verified {
		return errors.New("cannot extend a verified operation")
	}
	if j.Checkpoints == nil {
		j.Checkpoints = map[string]json.RawMessage{}
	}
	j.Checkpoints[name] = append(json.RawMessage(nil), value...)
	return s.store.write(ctx, cm, j)
}

// Recheck the persisted request immediately before external submission. In
// particular, a preflight Commit/Finalize handler cannot use Effect before the
// irreversible checkpoint is actually stored by the controller.
func (s *Session) checkRequest(ctx context.Context) error {
	var obj client.Object
	var action api.Action
	var status *api.TransitionStatus
	switch s.identity.Kind {
	case "ControlPlaneNetworkTransition":
		obj = &api.ControlPlaneNetworkTransition{}
	case "NamespaceNetworkTransition":
		obj = &api.NamespaceNetworkTransition{}
	default:
		return errors.New("unsupported transition kind")
	}
	if err := s.store.reader().Get(ctx, client.ObjectKey{Name: s.identity.Name}, obj); err != nil {
		return err
	}
	switch v := obj.(type) {
	case *api.ControlPlaneNetworkTransition:
		action = v.Spec.Action
		if v.Status != nil {
			status = &v.Status.TransitionStatus
		}
	case *api.NamespaceNetworkTransition:
		action = v.Spec.Action
		if v.Status != nil {
			status = &v.Status.TransitionStatus
		}
	}
	if obj.GetUID() != s.identity.UID || obj.GetGeneration() != s.identity.Generation || string(action) != s.identity.Action {
		return ErrFenced
	}
	if action == api.ActionCommit || action == api.ActionFinalize {
		if status == nil || status.IrreversibleAction != string(action) {
			return errors.New("irreversible effect requires a persisted Commit/Finalize checkpoint")
		}
	}
	return nil
}

// Completion restores handler-produced status when the CR status write was lost.
func (s *Session) Completion(ctx context.Context) (json.RawMessage, error) {
	if err := s.Check(ctx); err != nil {
		return nil, err
	}
	var j Journal
	_, err := s.store.read(ctx, s.identity.ID(), &j)
	if err != nil {
		return nil, err
	}
	if !j.Verified || len(j.Completion) == 0 {
		return nil, errors.New("verified operation has no completion status snapshot")
	}
	return j.Completion, nil
}
