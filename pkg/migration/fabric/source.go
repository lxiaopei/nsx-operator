/*
Copyright © 2026 Broadcom, Inc. All Rights Reserved.

	SPDX-License-Identifier: Apache-2.0
*/
package fabric

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	netapi "github.com/vmware-tanzu/net-operator-api/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/vmware-tanzu/nsx-operator/pkg/migration/recovery"
)

const (
	SNATNamespace    = "vmware-system-netop"
	SNATFinalizer    = "migration.network.vmware.com/lb-snat"
	sourceOwnerLabel = "migration.network.vmware.com/owner"
)

// SourceRequest references the actual Net Operator Network, not an IPPool name.
// The installed source API has no family selector. Validate must prove that this
// Network/request selects exactly the intended family and source VIP/backend pool.
type SourceRequest struct {
	Network     string
	NetworkUID  types.UID
	Family      string
	RequestedIP string
}

type NetOperatorSource struct {
	Client client.Client
	Reader client.Reader
	// Validate confirms source-IPAM allocation/preservation, family/pool selection,
	// backend membership and exact-IP availability/exclusion contracts. A nil callback
	// is unsupported. Network presence alone cannot establish these properties.
	Validate func(context.Context, SourceRequest) error
}

// Reserve uses the installed typed source API and journals a deterministic request.
// It does not infer exhaustion from CannotAllocIP. Switching an unresolved automatic
// request to fallback requires source-IPAM cancellation reconciliation; this method
// refuses to overwrite a request or allocate a second address to work around it.
func (s *NetOperatorSource) Reserve(ctx context.Context, session *recovery.Session, request SourceRequest) (Reservation, error) {
	if session == nil || s.Client == nil || s.Reader == nil || s.Validate == nil {
		return Reservation{}, errors.New("source IPAM preservation and selection integration unavailable")
	}
	id := session.Identity()
	if id.Kind != "ControlPlaneNetworkTransition" || id.Action != "Evaluate" {
		return Reservation{}, errors.New("source SNAT allocation requires infrastructure Evaluate")
	}
	if request.Network == "" || request.NetworkUID == "" || (request.Family != "IPv4" && request.Family != "IPv6") {
		return Reservation{}, errors.New("source network identity and family required")
	}
	if request.RequestedIP != "" {
		ip, err := address(request.RequestedIP)
		if err != nil || (request.Family == "IPv4") != ip.Is4() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			return Reservation{}, errors.New("invalid requested source SNAT IP")
		}
	}
	if err := session.Check(ctx); err != nil {
		return Reservation{}, err
	}
	if err := s.Validate(ctx, request); err != nil {
		return Reservation{}, err
	}
	network := &netapi.Network{}
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: SNATNamespace, Name: request.Network}, network); err != nil {
		return Reservation{}, err
	}
	if network.UID != request.NetworkUID || !network.DeletionTimestamp.IsZero() || network.Spec.Type != netapi.NetworkTypeVDS {
		return Reservation{}, errors.New("source Network changed or is no longer VDS-backed")
	}
	name := "migration-snat-" + digest(struct {
		UID    types.UID
		Family string
	}{id.UID, request.Family})
	desired := netapi.IPAddressAllocationSpec{PoolRef: corev1.TypedLocalObjectReference{APIGroup: ptr.To(netapi.GroupName), Kind: "Network", Name: request.Network}, RequestedIP: request.RequestedIP}
	var result Reservation
	probe := func(ctx context.Context, _ string) (recovery.Observation, error) {
		obj := &netapi.IPAddressAllocation{}
		err := s.Reader.Get(ctx, client.ObjectKey{Namespace: SNATNamespace, Name: name}, obj)
		if apierrors.IsNotFound(err) {
			history, e := session.History(ctx)
			if e != nil {
				return recovery.Pending, e
			}
			for _, j := range history {
				if _, ok := j.Checkpoints[name+"-source"]; ok {
					return recovery.Pending, errors.New("recorded source allocation disappeared")
				}
			}
			return recovery.Absent, nil
		}
		if err != nil {
			return recovery.Pending, err
		}
		if obj.UID == "" || !obj.DeletionTimestamp.IsZero() || obj.Labels[sourceOwnerLabel] != string(id.UID) || len(obj.OwnerReferences) != 0 || !reflect.DeepEqual(obj.Spec, desired) {
			return recovery.Pending, errors.New("source allocation identity, ownership or request conflict")
		}
		retained := false
		for _, f := range obj.Finalizers {
			if f == SNATFinalizer {
				retained = true
			}
		}
		if !retained {
			return recovery.Pending, errors.New("source SNAT retention finalizer is missing")
		}
		identity := struct {
			UID     types.UID
			Request SourceRequest
		}{obj.UID, request}
		data, err := json.Marshal(identity)
		if err != nil {
			return recovery.Pending, err
		}
		history, err := session.History(ctx)
		if err != nil {
			return recovery.Pending, err
		}
		for _, j := range history {
			if previous, ok := j.Checkpoints[name+"-source"]; ok {
				var old struct {
					UID     types.UID
					Request SourceRequest
				}
				if err := json.Unmarshal(previous, &old); err != nil {
					return recovery.Pending, err
				}
				if old != identity {
					return recovery.Pending, errors.New("source allocation was replaced or request changed")
				}
			}
		}
		if err := session.Checkpoint(ctx, name+"-source", data); err != nil {
			return recovery.Pending, err
		}
		ready := false
		for _, condition := range obj.Status.Conditions {
			if condition.Type == netapi.IPAddressAllocationFail && condition.Status == corev1.ConditionTrue {
				return recovery.Pending, fmt.Errorf("source allocation failed (%s); verify source IPAM before retrying", condition.Reason)
			}
			if condition.Type == netapi.IPAddressAllocationReady && condition.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		if !ready {
			return recovery.Pending, nil
		}
		ip, err := address(obj.Status.IPAddress)
		if err != nil {
			return recovery.Pending, err
		}
		if (request.Family == "IPv4") != ip.Is4() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			return recovery.Pending, errors.New("source IP does not match the requested usable family")
		}
		if request.RequestedIP != "" {
			wanted, err := address(request.RequestedIP)
			if err != nil || wanted != ip {
				return recovery.Pending, errors.New("source IP differs from the exact requested IP")
			}
		}
		result = Reservation{Family: request.Family, IP: ip.String(), Namespace: SNATNamespace, Name: obj.Name, UID: string(obj.UID)}
		return recovery.Applied, nil
	}
	if err := session.Effect(ctx, name, digest(request), probe, func(ctx context.Context, _ string) error {
		if err := s.Validate(ctx, request); err != nil {
			return err
		}
		fresh := &netapi.Network{}
		if err := s.Reader.Get(ctx, client.ObjectKeyFromObject(network), fresh); err != nil {
			return err
		}
		if fresh.UID != network.UID || fresh.ResourceVersion != network.ResourceVersion || !fresh.DeletionTimestamp.IsZero() {
			return errors.New("source Network changed before allocation")
		}
		// Deliberately no transition ownerReference: CR pruning must not release SNAT.
		obj := &netapi.IPAddressAllocation{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: SNATNamespace, Labels: map[string]string{sourceOwnerLabel: string(id.UID)}, Finalizers: []string{SNATFinalizer}}, Spec: desired}
		return s.Client.Create(ctx, obj)
	}); err != nil {
		return Reservation{}, err
	}
	observation, err := probe(ctx, "")
	if err != nil {
		return Reservation{}, err
	}
	if observation != recovery.Applied {
		return Reservation{}, recovery.ErrPending
	}
	return result, nil
}
