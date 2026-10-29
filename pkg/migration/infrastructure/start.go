/* Copyright © 2026 Broadcom, Inc. All Rights Reserved.
   SPDX-License-Identifier: Apache-2.0 */

// Package infrastructure implements Stage 1 prerequisite validation and resume binding.
package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/vmware-tanzu/nsx-operator/pkg/apis/migration/v1alpha1"
	"github.com/vmware-tanzu/nsx-operator/pkg/migration/recovery"
)

const (
	ConditionReady    = "InfrastructurePrerequisitesReady"
	bindingCheckpoint = "infrastructure-start"
)

// Issue distinguishes a missing prerequisite from an inability to observe it.
// Messages are safe for CR status; transport error payloads are not exposed there.
type Issue struct {
	Reason  string
	Message string
	Unknown bool
	Cause   error
}

func (i *Issue) Error() string           { return i.Message }
func (i *Issue) Unwrap() error           { return i.Cause }
func issue(reason, message string) error { return &Issue{Reason: reason, Message: message} }
func readFailure(message string, err error) error {
	return &Issue{Reason: "PrerequisiteReadFailed", Message: message, Unknown: true, Cause: err}
}

// Project identifies an existing NSX Project. UniqueID detects delete/recreate at
// the same path; the service never creates a missing project or falls back on errors.
type Project struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	UniqueID string `json:"uniqueID"`
}

type Projects interface {
	GetProject(context.Context, string) (Project, error)
}
type Readiness interface {
	Check(context.Context, bool, string) error
}

// Service is observational until Bind is called under the shared mutation lock.
// It does not report Evaluate success or create workflows, containers or allocations.
type Service struct {
	Readiness Readiness
	Projects  Projects
	Recovery  *recovery.Store
}

type Binding struct {
	Project Project `json:"project"`
}

// ProjectID accepts an ID, not a path, URL or display-name lookup. Omission alone
// selects default. Do not use the normal operator's default_project setting here.
func ProjectID(value string) (string, error) {
	if value == "" {
		return "default", nil
	}
	if strings.TrimSpace(value) != value || value == "." || value == ".." || strings.ContainsAny(value, "/\\?#%") || len(value) > 256 {
		return "", issue("InvalidNSXProject", "spec.nsxProjectId must be an NSX project ID, not a path or URL")
	}
	for _, r := range value {
		if r < 32 || r == 127 {
			return "", issue("InvalidNSXProject", "spec.nsxProjectId contains a control character")
		}
	}
	return value, nil
}

func hasResources(status *api.ControlPlaneNetworkTransitionStatus) bool {
	return status != nil && (status.PrimaryVPCPath != "" || status.LoadBalancerVPCPath != "" || len(status.NSXWorkflows) > 0 || len(status.LBSNATReservations) > 0 || status.IrreversibleAction != "")
}

func (s *Service) inspect(ctx context.Context, obj *api.ControlPlaneNetworkTransition) (Binding, error) {
	if obj.UID == "" || obj.Generation <= 0 || obj.Name != api.ControlPlaneNetworkTransitionName {
		return Binding{}, issue("InvalidTransitionIdentity", "Infrastructure start requires the persisted singleton transition identity")
	}
	if obj.Spec.SourceMode != "VSPHERE_VDS" || obj.Spec.TargetMode != "NSX_VPC" {
		return Binding{}, issue("UnsupportedNetworkTransition", "Only VSPHERE_VDS to NSX_VPC infrastructure migration is supported")
	}
	id, err := ProjectID(obj.Spec.NSXProjectID)
	if err != nil {
		return Binding{}, err
	}
	path := "/orgs/default/projects/" + id
	if s.Readiness == nil || s.Projects == nil || s.Recovery == nil {
		return Binding{}, issue("PrerequisiteServiceUnavailable", "Infrastructure prerequisite services are not configured")
	}
	history, err := s.Recovery.History(ctx, obj.UID)
	if err != nil {
		return Binding{}, readFailure("Cannot read infrastructure recovery journals", err)
	}
	allowVPC := obj.Status != nil && (obj.Status.LastAction == api.ActionExecute || obj.Status.LastAction == api.ActionFinalize)
	for _, j := range history {
		if j.Identity.Action == string(api.ActionExecute) {
			allowVPC = true
		}
	}
	if err = s.Readiness.Check(ctx, allowVPC, path); err != nil {
		return Binding{}, err
	}
	project, err := s.Projects.GetProject(ctx, id)
	if err != nil {
		return Binding{}, err
	}
	if project.ID != id || project.Path != path || project.UniqueID == "" {
		return Binding{}, issue("InvalidNSXProject", "NSX returned an incomplete or different project identity")
	}
	binding := Binding{Project: project}
	if obj.Status != nil && obj.Status.TargetNSXProject != "" && obj.Status.TargetNSXProject != path && (hasResources(obj.Status) || obj.Spec.Action != api.ActionEvaluate) {
		return Binding{}, issue("RecoveryIdentityConflict", "Resolved project differs from recorded infrastructure; resume using its original project")
	}
	hasBinding := false
	for _, j := range history {
		value, ok := j.Checkpoints[bindingCheckpoint]
		if !ok {
			if len(j.Steps) > 0 {
				return Binding{}, issue("RecoveryIdentityMissing", "Existing infrastructure effects have no project binding; investigate before resuming")
			}
			continue
		}
		hasBinding = true
		var previous Binding
		if err = json.Unmarshal(value, &previous); err != nil {
			return Binding{}, readFailure("Cannot decode the recorded infrastructure project binding", err)
		}
		bound := (hasResources(obj.Status) && obj.Status.TargetNSXProject == previous.Project.Path) || len(j.Steps) > 0 || j.Identity.Action != string(api.ActionEvaluate) || j.Identity.Generation == obj.Generation
		if bound && previous != binding {
			return Binding{}, issue("RecoveryIdentityConflict", "NSX project identity changed after infrastructure work was recorded; preserve existing resources and investigate")
		}
	}
	if hasResources(obj.Status) && !hasBinding {
		return Binding{}, issue("RecoveryIdentityMissing", "Existing infrastructure resources have no recorded project binding; verify adoption before resuming")
	}
	return binding, nil
}

// Check refreshes prerequisite status without advancing action checkpoints.
func (s *Service) Check(ctx context.Context, obj *api.ControlPlaneNetworkTransition) error {
	binding, err := s.inspect(ctx, obj)
	report(obj, binding, err)
	return err
}

func report(obj *api.ControlPlaneNetworkTransition, binding Binding, err error) {
	if obj.Status == nil {
		obj.Status = &api.ControlPlaneNetworkTransitionStatus{}
	}
	condition := metav1.Condition{Type: ConditionReady, ObservedGeneration: obj.Generation, Status: metav1.ConditionTrue, Reason: "PrerequisitesReady", Message: "VPC provider and NCP are ready; the pre-existing NSX Project is resolved"}
	if err != nil {
		condition.Status = metav1.ConditionUnknown
		condition.Reason = "PrerequisiteCheckFailed"
		condition.Message = "Infrastructure prerequisites could not be validated"
		var problem *Issue
		if errors.As(err, &problem) {
			condition.Status = metav1.ConditionFalse
			condition.Reason = problem.Reason
			condition.Message = problem.Message
			if problem.Unknown {
				condition.Status = metav1.ConditionUnknown
			}
		}
	} else {
		obj.Status.TargetNSXProject = binding.Project.Path
	}
	meta.SetStatusCondition(&obj.Status.Conditions, condition)
}

// Bind repeats live checks while serialized, then pins the project identity before
// a handler can perform effects. Historical journals remain available on resume.
func (s *Service) Bind(ctx context.Context, obj *api.ControlPlaneNetworkTransition, session *recovery.Session) error {
	if session == nil {
		return fmt.Errorf("infrastructure start requires a durable recovery session")
	}
	binding, err := s.inspect(ctx, obj)
	report(obj, binding, err)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	if err = session.Checkpoint(ctx, bindingCheckpoint, encoded); err != nil {
		return err
	}
	if obj.Status == nil {
		obj.Status = &api.ControlPlaneNetworkTransitionStatus{}
	}
	obj.Status.TargetNSXProject = binding.Project.Path
	return nil
}
