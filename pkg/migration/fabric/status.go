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
	if current.Status == nil || obj.Status == nil || current.Status.TargetLBProvider != obj.Status.TargetLBProvider || current.Status.TargetNSXProject != obj.Status.TargetNSXProject || current.Status.LoadBalancerVPCPath != obj.Status.LoadBalancerVPCPath {
		return errors.New("resolved LB topology must be persisted before SNAT acceptance")
	}
	checkpoint := api.LBSNATReservationStatus{IPFamily: r.Family, IPAddress: r.IP, AllocationRef: api.AllocationReference{Namespace: r.Namespace, Name: r.Name}}
	exists := false
	for _, old := range current.Status.LBSNATReservations {
		if old.IPFamily != r.Family {
			continue
		}
		if old != checkpoint {
			return errors.New("accepted SNAT reservation cannot be replaced")
		}
		exists = true
	}
	if !exists {
		current.Status.LBSNATReservations = append(current.Status.LBSNATReservations, checkpoint)
		if err := s.Client.Status().Update(ctx, current); err != nil {
			return err
		}
	}
	obj.Status.LBSNATReservations = append([]api.LBSNATReservationStatus(nil), current.Status.LBSNATReservations...)
	// Refresh resourceVersion so the outer reconciler's status update does not conflict
	// with this deliberately earlier acceptance checkpoint.
	obj.ResourceVersion = current.ResourceVersion
	return nil
}
