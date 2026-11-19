/* Copyright © 2026 Broadcom, Inc. All Rights Reserved.
   SPDX-License-Identifier: Apache-2.0 */

package infrastructure

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/vmware-tanzu/nsx-operator/pkg/apis/migration/v1alpha1"
	"github.com/vmware-tanzu/nsx-operator/pkg/migration/inspection"
	"github.com/vmware-tanzu/nsx-operator/pkg/migration/transition"
)

type evaluationScope struct{}

func (evaluationScope) SourceNetworks(context.Context) ([]string, []inspection.Topology, error) {
	return []string{"cp-network"}, nil, nil
}

type evaluationInspector struct{ result inspection.Result }

func (inspector evaluationInspector) InspectControlPlane(_ context.Context, _ *api.ControlPlaneNetworkTransition, vdns []string, _ ...inspection.Topology) (inspection.Result, error) {
	if len(vdns) != 1 || vdns[0] != "cp-network" {
		return inspection.Result{}, nil
	}
	return inspector.result, nil
}

type evaluationSNAT struct{ calls int }

func (snat *evaluationSNAT) Ensure(_ context.Context, _ *api.ControlPlaneNetworkTransition, _ inspection.LBSNATDecision) (api.IPAllocationStatus, bool, error) {
	snat.calls++
	return api.IPAllocationStatus{IPAddress: "192.0.2.10", AllocationRef: &api.NamespacedName{Namespace: "vmware-system-netop", Name: "migration-snat"}}, true, nil
}

func TestEvaluateHandlerWaitsForDHCPInputBeforeReservingSNAT(t *testing.T) {
	resource := evaluationTransition()
	snat := &evaluationSNAT{}
	handler := EvaluateHandler{Scope: evaluationScope{}, Inspector: evaluationInspector{result: inspection.Result{
		RequiredInputs: []api.RequiredInput{{FieldName: "spec.networkConfiguration.controlPlaneAndServiceDHCPNetworks[vdnName=cp-network]", Description: "Supply DHCP network details"}},
	}}, SNAT: snat}

	result, err := handler.Reconcile(context.Background(), evaluationRequest(resource))
	require.NoError(t, err)
	require.Equal(t, transition.OutcomeNeedsInput, result.Outcome)
	require.Equal(t, 0, snat.calls)
	require.Len(t, resource.Status.RequiredInputs, 1)
	require.Empty(t, resource.Status.LBSNATIPAllocations)
}

func TestEvaluateHandlerClearsOldFindingsWhenScopeDiscoveryFails(t *testing.T) {
	resource := evaluationTransition()
	resource.Status.RequiredInputs = []api.RequiredInput{{FieldName: "spec.networkConfiguration.controlPlaneAndServiceDHCPNetworks[vdnName=old]"}}
	meta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{
		Type: inspection.ConditionNetworkInputsValidated, Status: metav1.ConditionTrue,
		Reason: "InputsValidated", Message: "previous successful inspection", ObservedGeneration: 1,
	})
	meta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{
		Type: "EvaluationPreview", Status: metav1.ConditionTrue,
		Reason: "SourcePreviewReady", Message: "previous preview", ObservedGeneration: 1,
	})
	handler := EvaluateHandler{
		Scope: failingEvaluationScope{}, Inspector: evaluationInspector{}, SNAT: &evaluationSNAT{},
	}

	_, err := handler.Reconcile(context.Background(), evaluationRequest(resource))
	require.ErrorContains(t, err, "network discovery is unavailable")
	require.Empty(t, resource.Status.RequiredInputs)
	require.Equal(t, metav1.ConditionUnknown, meta.FindStatusCondition(resource.Status.Conditions, inspection.ConditionNetworkInputsValidated).Status)
	require.Equal(t, "NetworkDiscoveryUnavailable", meta.FindStatusCondition(resource.Status.Conditions, inspection.ConditionNetworkInputsValidated).Reason)
	require.Equal(t, metav1.ConditionUnknown, meta.FindStatusCondition(resource.Status.Conditions, "EvaluationPreview").Status)
}

type failingEvaluationScope struct{}

func (failingEvaluationScope) SourceNetworks(context.Context) ([]string, []inspection.Topology, error) {
	return nil, nil, fmt.Errorf("injected scope failure")
}

func TestEvaluateHandlerRecordsAcceptedSourceReservation(t *testing.T) {
	resource := evaluationTransition()
	snat := &evaluationSNAT{}
	handler := EvaluateHandler{Scope: evaluationScope{}, Inspector: evaluationInspector{result: inspection.Result{
		Networks: []inspection.Network{{Source: inspection.Source{VDNName: "cp-network"}}},
		LBSNAT:   &inspection.LBSNATDecision{Selection: inspection.AllocateFromBackendNetwork, AllocationSource: "dvportgroup-1"},
	}}, SNAT: snat}

	result, err := handler.Reconcile(context.Background(), evaluationRequest(resource))
	require.NoError(t, err)
	require.Equal(t, transition.OutcomeSucceeded, result.Outcome)
	require.Equal(t, 1, snat.calls)
	require.Equal(t, "192.0.2.10", resource.Status.LBSNATIPAllocations[0].IPAddress)
	require.Equal(t, "migration-snat", resource.Status.LBSNATIPAllocations[0].AllocationRef.Name)
	require.Equal(t, api.LoadBalancerProvider("VSPHERE_FOUNDATION"), resource.Status.SourceLBProvider)
	require.Contains(t, meta.FindStatusCondition(resource.Status.Conditions, "EvaluationPreview").Message, "VDN cp-network")
}

func TestEvaluateHandlerRunsAfterDurableActionCheckpoint(t *testing.T) {
	resource := evaluationTransition()
	resource.Status = &api.ControlPlaneNetworkTransitionStatus{}
	snat := &evaluationSNAT{}
	handler := EvaluateHandler{Scope: evaluationScope{}, Inspector: evaluationInspector{result: inspection.Result{
		Networks: []inspection.Network{{Source: inspection.Source{VDNName: "cp-network"}}},
		LBSNAT:   &inspection.LBSNATDecision{Selection: inspection.AllocateFromBackendNetwork, AllocationSource: "dvportgroup-1"},
	}}, SNAT: snat}
	engine := &transition.Engine{Enabled: true, Handlers: map[transition.Key]transition.Handler{{Stage: transition.Infrastructure, Action: api.ActionEvaluate}: handler}}

	result, err := engine.Reconcile(context.Background(), evaluationRequest(resource))
	require.NoError(t, err)
	require.Equal(t, transition.OutcomeInProgress, result.Outcome)
	require.Equal(t, 0, snat.calls, "persist the action checkpoint before reserving an IP")
	result, err = engine.Reconcile(context.Background(), evaluationRequest(resource))
	require.NoError(t, err)
	require.Equal(t, transition.OutcomeSucceeded, result.Outcome)
	require.Equal(t, "Evaluated", resource.Status.Phase)
	require.Equal(t, 1, snat.calls)
}

func evaluationTransition() *api.ControlPlaneNetworkTransition {
	return &api.ControlPlaneNetworkTransition{
		ObjectMeta: metav1.ObjectMeta{Name: api.ControlPlaneNetworkTransitionName, UID: "cp-transition", Generation: 1},
		Spec:       api.ControlPlaneNetworkTransitionSpec{TransitionSpec: api.TransitionSpec{Action: api.ActionEvaluate}},
		Status:     &api.ControlPlaneNetworkTransitionStatus{TransitionStatus: api.TransitionStatus{LastAction: api.ActionEvaluate, ActionState: api.ActionStateRunning, ActionGeneration: 1}},
	}
}

func evaluationRequest(resource *api.ControlPlaneNetworkTransition) transition.Request {
	return transition.Request{Stage: transition.Infrastructure, Object: resource, Action: api.ActionEvaluate, Generation: resource.Generation, Status: &resource.Status.TransitionStatus}
}
