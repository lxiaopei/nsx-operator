/*
Copyright © 2026 Broadcom, Inc. All Rights Reserved.

	SPDX-License-Identifier: Apache-2.0
*/
package fabric

import (
	"context"
	"errors"
	"fmt"

	api "github.com/vmware-tanzu/nsx-operator/pkg/apis/migration/v1alpha1"
)

var ErrPoolExhausted = errors.New("source VIP pool exhaustion confirmed")

type SNATRequest struct {
	Family     string
	LBVPCPath  string
	FallbackIP string
	VIPRanges  []AddressRange
}

// Reservation identifies an accepted source IP allocation. Name and Namespace
// are empty when the administrator supplied an address directly.
type Reservation struct {
	Family    string
	IP        string
	Namespace string
	Name      string
}

// SNATBackend owns the NSX/IPAM calls. Reserve must use a stable allocation name
// so a retry can find the same resource after a lost response or process restart.
type SNATBackend interface {
	Supported(context.Context, SNATRequest) error
	LBReady(context.Context, SNATRequest) error
	Reserve(context.Context, SNATRequest) (Reservation, error)
	VerifySource(context.Context, Reservation) error
	Configure(context.Context, SNATRequest, Reservation, []AddressRange) error
	VerifyTarget(context.Context, SNATRequest, Reservation) error
}

type ReservationStatus interface {
	Accept(context.Context, *api.ControlPlaneNetworkTransition, Reservation) error
}

type SNATService struct {
	Backend SNATBackend
	Status  ReservationStatus
}

// Bootstrap reserves one address during infrastructure Evaluate, records it in
// transition status, then configures the target. A retry reuses that status entry.
func (s *SNATService) Bootstrap(ctx context.Context, obj *api.ControlPlaneNetworkTransition, request SNATRequest) (Reservation, error) {
	if obj == nil || s.Backend == nil || s.Status == nil || obj.UID == "" || obj.Spec.Action != api.ActionEvaluate {
		return Reservation{}, errors.New("SNAT bootstrap requires the current infrastructure Evaluate transition")
	}
	if request.Family != "IPv4" {
		return Reservation{}, errors.New("Supervisor VDS migration currently supports IPv4 LB SNAT only")
	}
	if obj.Status == nil || obj.Status.TargetLBProvider != api.LoadBalancerProvider("NSX_LB") || obj.Status.LoadBalancerVPCPath != request.LBVPCPath || request.LBVPCPath == "" {
		return Reservation{}, errors.New("resolved NSX LB VPC is required")
	}
	fallback := ""
	if obj.Spec.NetworkConfiguration != nil && len(obj.Spec.NetworkConfiguration.LBSNATIPs) > 0 {
		fallback = obj.Spec.NetworkConfiguration.LBSNATIPs[0]
	}
	if request.FallbackIP != fallback {
		return Reservation{}, errors.New("SNAT request differs from administrator input")
	}
	if _, err := PartitionVIPRanges(request.VIPRanges, nil); err != nil {
		return Reservation{}, err
	}
	if err := s.Backend.Supported(ctx, request); err != nil {
		return Reservation{}, err
	}
	if err := s.Backend.LBReady(ctx, request); err != nil {
		return Reservation{}, err
	}

	accepted := acceptedReservation(obj.Status, request.Family)
	if accepted == nil {
		reserved, err := s.Backend.Reserve(ctx, request)
		if err != nil {
			return Reservation{}, err
		}
		accepted = &reserved
	}
	if err := validateReservation(*accepted, request); err != nil {
		return Reservation{}, err
	}
	if err := s.Backend.VerifySource(ctx, *accepted); err != nil {
		return Reservation{}, err
	}
	if err := s.Status.Accept(ctx, obj, *accepted); err != nil {
		return Reservation{}, err
	}

	// Use the status list as the source of accepted addresses so configuring one
	// family cannot put another accepted address back into the VIP ranges.
	excluded := make([]string, 0, len(obj.Status.LBSNATIPAllocations))
	for _, allocation := range obj.Status.LBSNATIPAllocations {
		excluded = append(excluded, allocation.IPAddress)
	}
	frontend, err := PartitionVIPRanges(request.VIPRanges, excluded)
	if err != nil {
		return Reservation{}, err
	}
	if err := s.Backend.Configure(ctx, request, *accepted, frontend); err != nil {
		return Reservation{}, err
	}
	if err := s.Verify(ctx, request, *accepted); err != nil {
		return Reservation{}, err
	}
	return *accepted, nil
}

func acceptedReservation(status *api.ControlPlaneNetworkTransitionStatus, family string) *Reservation {
	if len(status.LBSNATIPAllocations) == 0 {
		return nil
	}
	allocation := status.LBSNATIPAllocations[0]
	reservation := &Reservation{Family: family, IP: allocation.IPAddress}
	if allocation.AllocationRef != nil {
		reservation.Namespace = allocation.AllocationRef.Namespace
		reservation.Name = allocation.AllocationRef.Name
	}
	return reservation
}

func validateReservation(reservation Reservation, request SNATRequest) error {
	if reservation.Family != request.Family {
		return errors.New("source allocation family does not match the request")
	}
	ip, err := address(reservation.IP)
	if err != nil || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || (request.Family == "IPv4") != ip.Is4() {
		return errors.New("invalid SNAT address or family")
	}
	if (reservation.Namespace == "") != (reservation.Name == "") {
		return errors.New("source allocation reference must include namespace and name")
	}
	if request.FallbackIP != "" {
		wanted, err := address(request.FallbackIP)
		if err != nil || wanted != ip {
			return errors.New("accepted SNAT address differs from supplied fallback")
		}
	}
	return nil
}

func (s *SNATService) Verify(ctx context.Context, request SNATRequest, reservation Reservation) error {
	if s.Backend == nil {
		return errors.New("SNAT integration unavailable")
	}
	if err := s.Backend.Supported(ctx, request); err != nil {
		return err
	}
	if err := s.Backend.LBReady(ctx, request); err != nil {
		return err
	}
	if err := s.Backend.VerifySource(ctx, reservation); err != nil {
		return err
	}
	return s.Backend.VerifyTarget(ctx, request, reservation)
}

func MissingSNATInput(request SNATRequest, err error) *api.RequiredInput {
	if !errors.Is(err, ErrPoolExhausted) || request.FallbackIP != "" || request.Family != "IPv4" {
		return nil
	}
	return &api.RequiredInput{FieldName: "spec.networkConfiguration.lbSnatIPs", Description: fmt.Sprintf("The %s VIP pool is exhausted; supply an available backend address for LB SNAT", request.Family)}
}
