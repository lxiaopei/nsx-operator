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

	api "github.com/vmware-tanzu/nsx-operator/pkg/apis/migration/v1alpha1"
	"github.com/vmware-tanzu/nsx-operator/pkg/migration/recovery"
)

var ErrPoolExhausted = errors.New("source VIP pool exhaustion confirmed")

// SNATRequest contains resolved source networks and target identities. The source
// adapter must verify family selection, fallback membership/availability, durable
// protection, and VIP exclusion before returning an accepted reservation.
type SNATRequest struct {
	Family        string
	LBVPCPath     string
	LBVPCUniqueID string
	FallbackIP    string
	VIPRanges     []AddressRange
}

type Reservation struct {
	Family    string
	IP        string
	Namespace string
	Name      string
	UID       string
}

// SNATBackend is the integration boundary for installed source-IPAM and target
// LB APIs. Implementations must journal all mutations through the supplied session.
// There is intentionally no guessed SDK SNAT field or error-message parsing.
type SNATBackend interface {
	// Supported checks singleton IPBlock support, source IPAM preservation and the
	// target LB SNAT API before allocating any address. Missing contracts are errors.
	Supported(context.Context, SNATRequest) error
	LBReady(context.Context, SNATRequest) error
	// Reserve reuses deterministic requests; Exhausted must mean authoritative pool
	// exhaustion, not CannotAllocIP, auth errors, timeouts or an unready controller.
	// A fallback must not race an unresolved automatic request.
	Reserve(context.Context, *recovery.Session, SNATRequest) (Reservation, error)
	VerifySource(context.Context, Reservation) error
	// Configure preserves the source allocation and exclusion, creates the exact
	// dedicated block and target allocation, then configures/verifies LB SNAT.
	Configure(context.Context, *recovery.Session, SNATRequest, Reservation, []AddressRange) error
	// VerifyTarget reads live target allocation, dedicated block and LBService SNAT
	// readiness without creating or allocating anything.
	VerifyTarget(context.Context, SNATRequest, Reservation) error
}

// ReservationStatus must persist the accepted checkpoint on the CR before target
// effects. Implementations must enforce UID/generation and preserve other status.
type ReservationStatus interface {
	Accept(context.Context, *api.ControlPlaneNetworkTransition, Reservation) error
}

type SNATService struct {
	Backend SNATBackend
	Status  ReservationStatus
}

// Bootstrap is called by Stage 1 Evaluate only after source evaluation and empty
// LB VPC preparation. It never signals completion of the entire Evaluate action.
// Derive/Stage 2 must Verify/reuse the recorded bootstrap, not call Reserve again.
func (s *SNATService) Bootstrap(ctx context.Context, session *recovery.Session, obj *api.ControlPlaneNetworkTransition, request SNATRequest) (Reservation, error) {
	if session == nil || obj == nil || s.Backend == nil || s.Status == nil {
		return Reservation{}, errors.New("SNAT bootstrap integration unavailable")
	}
	id := session.Identity()
	if id.Kind != "ControlPlaneNetworkTransition" || id.Action != "Evaluate" || id.UID != obj.UID || id.Generation != obj.Generation || obj.Spec.Action != api.ActionEvaluate {
		return Reservation{}, errors.New("SNAT bootstrap requires the current infrastructure Evaluate session")
	}
	if obj.Status == nil || obj.Status.TargetLBProvider != api.LoadBalancerProvider("NSX_LB") || obj.Status.LoadBalancerVPCPath != request.LBVPCPath || request.LBVPCPath == "" || request.LBVPCUniqueID == "" {
		return Reservation{}, errors.New("resolved NSX LB VPC identity is required")
	}
	if request.Family != "IPv4" && request.Family != "IPv6" {
		return Reservation{}, errors.New("invalid SNAT IP family")
	}
	fallback := ""
	if obj.Spec.NetworkConfiguration != nil {
		fallback = obj.Spec.NetworkConfiguration.LBSNATIPv4
		if request.Family == "IPv6" {
			fallback = obj.Spec.NetworkConfiguration.LBSNATIPv6
		}
	}
	if request.FallbackIP != fallback {
		return Reservation{}, errors.New("SNAT request differs from administrator input")
	}
	if _, err := PartitionVIPRanges(request.VIPRanges, nil); err != nil {
		return Reservation{}, err
	}
	if err := session.Check(ctx); err != nil {
		return Reservation{}, err
	}
	if err := s.Backend.Supported(ctx, request); err != nil {
		return Reservation{}, err
	}
	if err := s.Backend.LBReady(ctx, request); err != nil {
		return Reservation{}, err
	}
	// Freeze the selected target independently of the editable, unaccepted fallback.
	// A later action must reuse the same VPC incarnation, not merely its path.
	target := struct{ Path, UniqueID string }{request.LBVPCPath, request.LBVPCUniqueID}
	targetData, err := json.Marshal(target)
	if err != nil {
		return Reservation{}, err
	}
	name := "fabric-snat-" + request.Family
	history, err := session.History(ctx)
	if err != nil {
		return Reservation{}, err
	}
	var accepted *Reservation
	for _, journal := range history {
		if raw, ok := journal.Checkpoints["fabric-snat-target-"+request.Family]; ok {
			var previous struct{ Path, UniqueID string }
			if err := json.Unmarshal(raw, &previous); err != nil {
				return Reservation{}, err
			}
			if previous != target {
				return Reservation{}, errors.New("SNAT target VPC identity changed")
			}
		}
		if raw, ok := journal.Checkpoints[name]; ok {
			var previous Reservation
			if err := json.Unmarshal(raw, &previous); err != nil {
				return Reservation{}, err
			}
			if accepted != nil && *accepted != previous {
				return Reservation{}, errors.New("conflicting accepted SNAT history")
			}
			accepted = &previous
		}
	}
	if err := session.Checkpoint(ctx, "fabric-snat-target-"+request.Family, targetData); err != nil {
		return Reservation{}, err
	}
	if accepted == nil {
		// Status without journal provenance cannot authorize another allocation.
		for _, r := range obj.Status.LBSNATReservations {
			if r.IPFamily == request.Family {
				return Reservation{}, errors.New("SNAT status has no recovery identity")
			}
		}
		reserved, err := s.Backend.Reserve(ctx, session, request)
		if err != nil {
			return Reservation{}, err
		}
		accepted = &reserved
	}
	if accepted.Family != request.Family || accepted.Namespace != "vmware-system-netop" || accepted.Name == "" || accepted.UID == "" {
		return Reservation{}, errors.New("invalid SNAT source reservation identity")
	}
	ip, err := address(accepted.IP)
	if err != nil {
		return Reservation{}, err
	}
	if ip.IsUnspecified() || ip.IsMulticast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || (request.Family == "IPv4") != ip.Is4() {
		return Reservation{}, errors.New("invalid SNAT address or family")
	}
	if request.FallbackIP != "" {
		wanted, err := address(request.FallbackIP)
		if err != nil || wanted != ip {
			return Reservation{}, errors.New("accepted SNAT address differs from supplied fallback")
		}
	}
	if err := s.Backend.VerifySource(ctx, *accepted); err != nil {
		return Reservation{}, err
	}
	data, err := json.Marshal(accepted)
	if err != nil {
		return Reservation{}, err
	}
	if err := session.Checkpoint(ctx, name, data); err != nil {
		return Reservation{}, err
	}
	if err := s.Status.Accept(ctx, obj, *accepted); err != nil {
		return Reservation{}, err
	}
	// Recheck after status persistence; no target allocation before source acceptance.
	if err := session.Check(ctx); err != nil {
		return Reservation{}, err
	}
	if err := s.Backend.VerifySource(ctx, *accepted); err != nil {
		return Reservation{}, err
	}
	// Keep every accepted family excluded when reconciling either family.
	// Configuring IPv6 must not accidentally restore IPv4 SNAT to VIP eligibility.
	excluded := []string{accepted.IP}
	for _, reservation := range obj.Status.LBSNATReservations {
		if reservation.IPFamily != request.Family {
			excluded = append(excluded, reservation.IPAddress)
		}
	}
	frontend, err := PartitionVIPRanges(request.VIPRanges, excluded)
	if err != nil {
		return Reservation{}, err
	}
	if err := s.Backend.Configure(ctx, session, request, *accepted, frontend); err != nil {
		return Reservation{}, err
	}
	if err := s.Verify(ctx, request, *accepted); err != nil {
		return Reservation{}, err
	}
	return *accepted, nil
}

// Verify is the read-only reuse path for Derive and Stage 2. The caller loads the
// accepted request/reservation from infrastructure recovery history. No allocation
// or repair is attempted if any resource is missing, replaced or unready.
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

// MissingSNATInput is used only after authoritative exhaustion with no supplied
// fallback. All other failures remain operational/configuration errors.
func MissingSNATInput(request SNATRequest, err error) *api.RequiredInput {
	if !errors.Is(err, ErrPoolExhausted) || request.FallbackIP != "" {
		return nil
	}
	if request.Family != "IPv4" && request.Family != "IPv6" {
		return nil
	}
	return &api.RequiredInput{Key: "spec.networkConfiguration.lbSnat" + request.Family, Description: fmt.Sprintf("The %s VIP pool is exhausted; supply an available backend address for LB SNAT", request.Family)}
}
