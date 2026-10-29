/* Copyright © 2026 Broadcom, Inc. All Rights Reserved.
   SPDX-License-Identifier: Apache-2.0 */

package fabric

import (
	"context"
	"errors"
	"reflect"
	"sort"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/vmware-tanzu/nsx-operator/pkg/apis/migration/v1alpha1"
	"github.com/vmware-tanzu/nsx-operator/pkg/migration/recovery"
)

// KubernetesResourceStatus uses uncached reads and status resourceVersion checks.
// Callers must refresh the CR before subsequent status writes; this store does not
// advance the resourceVersion on a caller's potentially stale copy.
type KubernetesResourceStatus struct {
	Client client.Client
	Reader client.Reader
}

func (s *KubernetesResourceStatus) read(ctx context.Context, id recovery.Identity) (client.Object, *api.TransitionStatus, error) {
	if s.Client == nil || s.Reader == nil {
		return nil, nil, errors.New("fabric status store unavailable")
	}
	var obj client.Object
	switch id.Kind {
	case "ControlPlaneNetworkTransition":
		obj = &api.ControlPlaneNetworkTransition{}
	case "NamespaceNetworkTransition":
		obj = &api.NamespaceNetworkTransition{}
	default:
		return nil, nil, errors.New("unsupported transition kind")
	}
	if err := s.Reader.Get(ctx, client.ObjectKey{Name: id.Name}, obj); err != nil {
		return nil, nil, err
	}
	var action api.Action
	var status *api.TransitionStatus
	switch v := obj.(type) {
	case *api.ControlPlaneNetworkTransition:
		action = v.Spec.Action
		if v.Status == nil {
			v.Status = &api.ControlPlaneNetworkTransitionStatus{}
		}
		status = &v.Status.TransitionStatus
	case *api.NamespaceNetworkTransition:
		action = v.Spec.Action
		if v.Status == nil {
			v.Status = &api.NamespaceNetworkTransitionStatus{}
		}
		status = &v.Status.TransitionStatus
	}
	if obj.GetUID() != id.UID || obj.GetGeneration() != id.Generation || string(action) != id.Action || !obj.GetDeletionTimestamp().IsZero() {
		return nil, nil, recovery.ErrFenced
	}
	return obj, status, nil
}

func (s *KubernetesResourceStatus) Load(ctx context.Context, id recovery.Identity, path string) (*Record, error) {
	_, status, err := s.read(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, r := range status.FabricResources {
		if r.Path == path {
			if r.UniqueID == "" || (r.Provenance != "Created" && r.Provenance != "Adopted") {
				return nil, errors.New("invalid fabric status checkpoint")
			}
			return &Record{Path: r.Path, UniqueID: r.UniqueID, Provenance: r.Provenance, Owner: types.UID(r.Owner), Consumers: r.Consumers}, nil
		}
	}
	return nil, nil
}

func (s *KubernetesResourceStatus) Save(ctx context.Context, id recovery.Identity, record Record) error {
	obj, status, err := s.read(ctx, id)
	if err != nil {
		return err
	}
	checkpoint := api.FabricResourceStatus{Path: record.Path, UniqueID: record.UniqueID, Provenance: record.Provenance, Owner: string(record.Owner), Consumers: append([]api.FabricConsumerStatus(nil), record.Consumers...)}

	for i, old := range status.FabricResources {
		if old.Path != record.Path {
			continue
		}
		if old.UniqueID != checkpoint.UniqueID || old.Owner != checkpoint.Owner || old.Provenance != checkpoint.Provenance {
			return errors.New("fabric identity or provenance changed")
		}
		// Consumer history is additive across actions; never forget an earlier user.
		for _, consumer := range old.Consumers {
			found := false
			for _, current := range checkpoint.Consumers {
				if current.UID == consumer.UID {
					if current != consumer {
						return errors.New("fabric consumer identity changed")
					}
					found = true
				}
			}
			if !found {
				checkpoint.Consumers = append(checkpoint.Consumers, consumer)
			}
		}
		sort.Slice(checkpoint.Consumers, func(i, j int) bool { return checkpoint.Consumers[i].UID < checkpoint.Consumers[j].UID })
		if reflect.DeepEqual(old, checkpoint) {
			return nil
		}
		status.FabricResources[i] = checkpoint
		return s.Client.Status().Update(ctx, obj)
	}
	sort.Slice(checkpoint.Consumers, func(i, j int) bool { return checkpoint.Consumers[i].UID < checkpoint.Consumers[j].UID })
	status.FabricResources = append(status.FabricResources, checkpoint)
	return s.Client.Status().Update(ctx, obj)
}
