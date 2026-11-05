/*
Copyright © 2026 Broadcom, Inc. All Rights Reserved.

	SPDX-License-Identifier: Apache-2.0
*/
package fabric

import (
	"context"
	"errors"

	api "github.com/vmware-tanzu/nsx-operator/pkg/apis/migration/v1alpha1"
)

var (
	ErrPoolExhausted           = errors.New("source VIP pool exhaustion confirmed")
	ErrNoEligibleSourceNetwork = errors.New("no eligible source Network is available for IPAM reservation")
)

type SNATRequest struct {
	LBVPCPath  string
	FallbackIP string
	VIPRanges  []AddressRange
}

// Reservation identifies an accepted SNAT IP. Name and Namespace are empty
// when an administrator-supplied IP could not be reserved from a source Network.
type Reservation struct {
	IP        string
	Namespace string
	Name      string
}

// SNATBackend owns the NSX/IPAM calls. Reserve must use a stable allocation name
// so a retry can find the same resource after a lost response or process restart.
// When FallbackIP is set, Reserve should try to reserve that address from an
// eligible source Network. Return ErrNoEligibleSourceNetwork only when no such
// Network can be identified; the service then treats the IP as administrator-
// asserted and skips source verification. Other reservation failures are fatal.
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
	if request.FallbackIP != "" {
		if err := validateSNATIP(request.FallbackIP); err != nil {
			return Reservation{}, err
		}
	}
	if err := validateIPv4VIPRanges(request.VIPRanges); err != nil {
		return Reservation{}, err
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

	accepted := acceptedReservation(obj.Status)
	manualFallback := accepted != nil && !hasSourceAllocation(*accepted)
	if accepted == nil {
		reserved, err := s.Backend.Reserve(ctx, request)
		if errors.Is(err, ErrNoEligibleSourceNetwork) && request.FallbackIP != "" {
			reserved, err = Reservation{IP: request.FallbackIP}, nil
			manualFallback = true
		}
		if err != nil {
			return Reservation{}, err
		}
		accepted = &reserved
	}
	if err := validateReservation(*accepted, request, manualFallback); err != nil {
		return Reservation{}, err
	}
	if hasSourceAllocation(*accepted) {
		if err := s.Backend.VerifySource(ctx, *accepted); err != nil {
			return Reservation{}, err
		}
	}
	if err := s.Status.Accept(ctx, obj, *accepted); err != nil {
		return Reservation{}, err
	}

	// Remove the accepted SNAT address from the frontend VIP ranges if it overlaps.
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

func acceptedReservation(status *api.ControlPlaneNetworkTransitionStatus) *Reservation {
	if len(status.LBSNATIPAllocations) == 0 {
		return nil
	}
	allocation := status.LBSNATIPAllocations[0]
	reservation := &Reservation{IP: allocation.IPAddress}
	if allocation.AllocationRef != nil {
		reservation.Namespace = allocation.AllocationRef.Namespace
		reservation.Name = allocation.AllocationRef.Name
	}
	return reservation
}

func validateReservation(reservation Reservation, request SNATRequest, manualFallback bool) error {
	if err := validateSNATIP(reservation.IP); err != nil {
		return err
	}
	acceptedIP, _ := address(reservation.IP)
	if (reservation.Namespace == "") != (reservation.Name == "") {
		return errors.New("source allocation reference must include namespace and name")
	}
	if request.FallbackIP != "" {
		wanted, err := address(request.FallbackIP)
		if err != nil || wanted != acceptedIP {
			return errors.New("accepted SNAT address differs from supplied fallback")
		}
		if !hasSourceAllocation(reservation) && !manualFallback {
			return errors.New("source Network reservation is required unless no eligible source Network exists")
		}
	} else if !hasSourceAllocation(reservation) {
		return errors.New("SNAT address without a source allocation must be supplied by the administrator")
	}
	return nil
}

func validateSNATIP(value string) error {
	ip, err := address(value)
	if err != nil || !ip.Is4() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return errors.New("SNAT address must be a usable IPv4 address")
	}
	return nil
}

func hasSourceAllocation(reservation Reservation) bool {
	return reservation.Namespace != "" && reservation.Name != ""
}

func validateIPv4VIPRanges(ranges []AddressRange) error {
	for _, vipRange := range ranges {
		start, err := address(vipRange.Start)
		if err != nil || !start.Is4() {
			return errors.New("Supervisor VDS migration supports IPv4 VIP ranges only")
		}
		end, err := address(vipRange.End)
		if err != nil || !end.Is4() {
			return errors.New("Supervisor VDS migration supports IPv4 VIP ranges only")
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
	if hasSourceAllocation(reservation) {
		if err := s.Backend.VerifySource(ctx, reservation); err != nil {
			return err
		}
	}
	return s.Backend.VerifyTarget(ctx, request, reservation)
}

func MissingSNATInput(request SNATRequest, err error) *api.RequiredInput {
	if !errors.Is(err, ErrPoolExhausted) || request.FallbackIP != "" {
		return nil
	}
	return &api.RequiredInput{FieldName: "spec.networkConfiguration.lbSnatIPs", Description: "The IPv4 VIP pool is exhausted; supply an available backend address for LB SNAT"}
}
