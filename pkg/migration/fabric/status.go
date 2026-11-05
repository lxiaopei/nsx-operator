/*
Copyright © 2026 Broadcom, Inc. All Rights Reserved.

	SPDX-License-Identifier: Apache-2.0
*/
package fabric

import (
	"context"
	"errors"

	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/vmware-tanzu/nsx-operator/pkg/apis/migration/v1alpha1"
)

// KubernetesReservationStatus persists acceptance before target effects. Reader
// must be uncached; conflicts are retried by reconciliation, never blindly overwritten.
type KubernetesReservationStatus struct {
	Client client.Client
	Reader client.Reader
}

func (s *KubernetesReservationStatus) Accept(ctx context.Context, obj *api.ControlPlaneNetworkTransition, r Reservation) error {
	if s.Client == nil || s.Reader == nil {
		return errors.New("SNAT status persistence unavailable")
	}
	current := &api.ControlPlaneNetworkTransition{}
	if err := s.Reader.Get(ctx, client.ObjectKeyFromObject(obj), current); err != nil {
		return err
	}
	if current.UID != obj.UID || current.Generation != obj.Generation || current.Spec.Action != api.ActionEvaluate || !current.DeletionTimestamp.IsZero() {
		return errors.New("SNAT Evaluate request changed")
	}
	if current.Status == nil || obj.Status == nil || current.Status.TargetLBProvider != obj.Status.TargetLBProvider || current.Status.TargetNSXProjectPath != obj.Status.TargetNSXProjectPath || current.Status.LoadBalancerVPCPath != obj.Status.LoadBalancerVPCPath {
		return errors.New("resolved LB topology must be persisted before SNAT acceptance")
	}
	ip, err := address(r.IP)
	if err != nil || !ip.Is4() {
		return errors.New("Supervisor VDS LB SNAT allocation must be IPv4")
	}
	checkpoint := api.IPAllocationStatus{IPAddress: r.IP}
	if r.Namespace != "" || r.Name != "" {
		if r.Namespace == "" || r.Name == "" {
			return errors.New("source allocation reference must include namespace and name")
		}
		checkpoint.AllocationRef = &api.NamespacedName{Namespace: r.Namespace, Name: r.Name}
	}
	if len(current.Status.LBSNATIPAllocations) > 0 {
		old := current.Status.LBSNATIPAllocations[0]
		if old.IPAddress != checkpoint.IPAddress || !sameAllocationRef(old.AllocationRef, checkpoint.AllocationRef) {
			return errors.New("accepted SNAT reservation cannot be replaced")
		}
	} else {
		current.Status.LBSNATIPAllocations = append(current.Status.LBSNATIPAllocations, checkpoint)
		if err := s.Client.Status().Update(ctx, current); err != nil {
			return err
		}
	}
	obj.Status.LBSNATIPAllocations = append([]api.IPAllocationStatus(nil), current.Status.LBSNATIPAllocations...)
	// Refresh resourceVersion so the outer reconciler's status update does not conflict
	// with this deliberately earlier acceptance checkpoint.
	obj.ResourceVersion = current.ResourceVersion
	return nil
}

func sameAllocationRef(a, b *api.NamespacedName) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
