/*
Copyright © 2026 Broadcom, Inc. All Rights Reserved.

	SPDX-License-Identifier: Apache-2.0
*/
package fabric

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	netapi "github.com/vmware-tanzu/net-operator-api/api/v1alpha1"
	api "github.com/vmware-tanzu/nsx-operator/pkg/apis/migration/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	SNATNamespace    = "vmware-system-netop"
	SNATFinalizer    = "migration.network.vmware.com/lb-snat"
	sourceOwnerLabel = "migration.network.vmware.com/owner"
)

// SourceRequest identifies the Net Operator Network and address requested for SNAT.
type SourceRequest struct {
	Network     string
	NetworkUID  types.UID
	RequestedIP string
}

type NetOperatorSource struct {
	Client   client.Client
	Reader   client.Reader
	Validate func(context.Context, SourceRequest) error
}

// Reserve creates or reuses the deterministic Net Operator allocation for this
// transition. The allocation object itself is the retry checkpoint.
func (s *NetOperatorSource) Reserve(ctx context.Context, transition *api.ControlPlaneNetworkTransition, request SourceRequest) (Reservation, error) {
	if transition == nil || transition.UID == "" || transition.Spec.Action != api.ActionEvaluate || s.Client == nil || s.Reader == nil || s.Validate == nil {
		return Reservation{}, errors.New("source IPAM Evaluate integration unavailable")
	}
	if request.Network == "" || request.NetworkUID == "" {
		return Reservation{}, errors.New("source Network identity is required")
	}
	if request.RequestedIP != "" {
		ip, err := address(request.RequestedIP)
		if err != nil || !ip.Is4() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			return Reservation{}, errors.New("invalid requested source IPv4 SNAT IP")
		}
	}
	if err := s.Validate(ctx, request); err != nil {
		return Reservation{}, err
	}
	currentTransition := &api.ControlPlaneNetworkTransition{}
	if err := s.Reader.Get(ctx, client.ObjectKeyFromObject(transition), currentTransition); err != nil {
		return Reservation{}, err
	}
	if currentTransition.UID != transition.UID || currentTransition.Generation != transition.Generation || currentTransition.Spec.Action != api.ActionEvaluate || !currentTransition.DeletionTimestamp.IsZero() {
		return Reservation{}, errors.New("source allocation requires the current Evaluate request")
	}

	network := &netapi.Network{}
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: SNATNamespace, Name: request.Network}, network); err != nil {
		return Reservation{}, err
	}
	if network.UID != request.NetworkUID || !network.DeletionTimestamp.IsZero() || network.Spec.Type != netapi.NetworkTypeVDS {
		return Reservation{}, errors.New("source Network changed or is no longer VDS-backed")
	}

	name := sourceAllocationName(transition.UID)
	desired := netapi.IPAddressAllocationSpec{
		PoolRef:     corev1.TypedLocalObjectReference{APIGroup: ptr.To(netapi.GroupName), Kind: "Network", Name: request.Network},
		RequestedIP: request.RequestedIP,
	}
	allocation := &netapi.IPAddressAllocation{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: SNATNamespace,
		Labels:     map[string]string{sourceOwnerLabel: string(transition.UID)},
		Finalizers: []string{SNATFinalizer},
	}, Spec: desired}
	if err := s.Client.Create(ctx, allocation); err != nil && !apierrors.IsAlreadyExists(err) {
		return Reservation{}, err
	}

	allocation = &netapi.IPAddressAllocation{}
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: SNATNamespace, Name: name}, allocation); err != nil {
		return Reservation{}, err
	}
	if allocation.UID == "" || !allocation.DeletionTimestamp.IsZero() || allocation.Labels[sourceOwnerLabel] != string(transition.UID) || len(allocation.OwnerReferences) != 0 || !reflect.DeepEqual(allocation.Spec, desired) {
		return Reservation{}, errors.New("source allocation identity or request conflicts with this transition")
	}
	if !hasFinalizer(allocation.Finalizers, SNATFinalizer) {
		return Reservation{}, errors.New("source SNAT retention finalizer is missing")
	}
	for _, condition := range allocation.Status.Conditions {
		if condition.Type == netapi.IPAddressAllocationFail && condition.Status == corev1.ConditionTrue {
			return Reservation{}, fmt.Errorf("source allocation failed (%s); verify source IPAM before retrying", condition.Reason)
		}
		if condition.Type != netapi.IPAddressAllocationReady || condition.Status != corev1.ConditionTrue {
			continue
		}
		ip, err := address(allocation.Status.IPAddress)
		if err != nil || !ip.Is4() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			return Reservation{}, errors.New("source allocation returned an invalid IPv4 address")
		}
		if request.RequestedIP != "" {
			requested, parseErr := address(request.RequestedIP)
			if parseErr != nil || requested != ip {
				return Reservation{}, errors.New("source allocation differs from the requested IP")
			}
		}
		return Reservation{IP: ip.String(), Namespace: SNATNamespace, Name: name}, nil
	}
	return Reservation{}, ErrPending
}

func hasFinalizer(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func sourceAllocationName(uid types.UID) string {
	// Keep the IPv4 discriminator in the hash to preserve existing allocation names.
	identity := struct {
		UID    types.UID
		Family string
	}{uid, "IPv4"}
	data, _ := json.Marshal(identity)
	hash := sha256.Sum256(data)
	return "migration-snat-" + hex.EncodeToString(hash[:16])
}
