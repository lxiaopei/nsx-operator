/*
Copyright © 2026 Broadcom, Inc. All Rights Reserved.

	SPDX-License-Identifier: Apache-2.0
*/
package fabric

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	api "github.com/vmware-tanzu/nsx-operator/pkg/apis/migration/v1alpha1"
	"github.com/vmware-tanzu/nsx-operator/pkg/migration/recovery"
)

type snatBackend struct {
	supportedErr, readyErr, reserveErr, verifyErr, configureErr error
	reservations, configurations, verifications                 int
	reservation                                                 Reservation
	frontend                                                    []AddressRange
}

func (b *snatBackend) Supported(context.Context, SNATRequest) error { return b.supportedErr }
func (b *snatBackend) LBReady(context.Context, SNATRequest) error   { return b.readyErr }
func (b *snatBackend) Reserve(context.Context, *recovery.Session, SNATRequest) (Reservation, error) {
	b.reservations++
	return b.reservation, b.reserveErr
}
func (b *snatBackend) VerifySource(context.Context, Reservation) error {
	b.verifications++
	return b.verifyErr
}
func (b *snatBackend) Configure(_ context.Context, _ *recovery.Session, _ SNATRequest, _ Reservation, frontend []AddressRange) error {
	b.configurations++
	b.frontend = frontend
	return b.configureErr
}

type failingStatus struct{}

func (failingStatus) Accept(context.Context, *api.ControlPlaneNetworkTransition, Reservation) error {
	return errors.New("status unavailable")
}

func snatSetup(t *testing.T) (*SNATService, *snatBackend, *recovery.Store, *recovery.Session, *api.ControlPlaneNetworkTransition, SNATRequest) {
	t.Helper()
	store, session, obj := setup(t)
	b := &snatBackend{reservation: Reservation{Family: "IPv4", IP: "192.0.2.5", Namespace: "vmware-system-netop", Name: "snat", UID: "source-uid"}}
	s := &SNATService{Backend: b, Status: &KubernetesReservationStatus{Client: store.Client, Reader: store.Reader}}
	request := SNATRequest{Family: "IPv4", LBVPCPath: obj.Status.LoadBalancerVPCPath, LBVPCUniqueID: "lb-uid", VIPRanges: []AddressRange{{"192.0.2.1", "192.0.2.10"}}}
	return s, b, store, session, obj, request
}

func TestSNATStatusFailureResumesAcceptedSource(t *testing.T) {
	s, b, store, session, obj, request := snatSetup(t)
	status := s.Status
	s.Status = failingStatus{}
	_, err := s.Bootstrap(context.Background(), session, obj, request)
	require.ErrorContains(t, err, "status unavailable")
	require.Zero(t, b.configurations)
	require.Equal(t, 1, b.reservations)
	restarted, err := store.Acquire(context.Background(), session.Identity(), recovery.Scope{AllNamespaces: true})
	require.NoError(t, err)
	s.Status = status
	accepted, err := s.Bootstrap(context.Background(), restarted, obj, request)
	require.NoError(t, err)
	require.Equal(t, "192.0.2.5", accepted.IP)
	require.Equal(t, 1, b.reservations)
	require.Equal(t, 1, b.configurations)
	require.Equal(t, []AddressRange{{"192.0.2.1", "192.0.2.4"}, {"192.0.2.6", "192.0.2.10"}}, b.frontend)
	require.Empty(t, obj.Status.LastAction, "bootstrap does not complete Evaluate")
	b.verifyErr = errors.New("source allocation UID changed")
	_, err = s.Bootstrap(context.Background(), restarted, obj, request)
	require.ErrorContains(t, err, "UID changed")
	require.Equal(t, 1, b.configurations)
	require.Equal(t, 1, b.reservations)
}

func TestSNATPreconditionsBeforeAllocation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*snatBackend, *api.ControlPlaneNetworkTransition, *SNATRequest)
	}{
		{"unsupported contract", func(b *snatBackend, _ *api.ControlPlaneNetworkTransition, _ *SNATRequest) {
			b.supportedErr = errors.New("SNAT API unavailable")
		}},
		{"LB pending", func(b *snatBackend, _ *api.ControlPlaneNetworkTransition, _ *SNATRequest) {
			b.readyErr = recovery.ErrPending
		}},
		{"AVI", func(_ *snatBackend, o *api.ControlPlaneNetworkTransition, _ *SNATRequest) {
			o.Status.TargetLBProvider = "AVI"
		}},
		{"Derive", func(_ *snatBackend, o *api.ControlPlaneNetworkTransition, _ *SNATRequest) {
			o.Spec.Action = api.ActionDerive
		}},
		{"unknown family", func(_ *snatBackend, _ *api.ControlPlaneNetworkTransition, r *SNATRequest) { r.Family = "unknown" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, b, _, session, obj, request := snatSetup(t)
			tc.change(b, obj, &request)
			_, err := s.Bootstrap(context.Background(), session, obj, request)
			require.Error(t, err)
			require.Zero(t, b.reservations)
			require.Zero(t, b.configurations)
		})
	}
}

func TestSNATInputOnlyOnConfirmedExhaustion(t *testing.T) {
	request := SNATRequest{Family: "IPv6"}
	for _, err := range []error{errors.New("CannotAllocIP"), errors.New("unauthorized"), recovery.ErrPending} {
		require.Nil(t, MissingSNATInput(request, err))
	}
	input := MissingSNATInput(request, ErrPoolExhausted)
	require.NotNil(t, input)
	require.Equal(t, "spec.networkConfiguration.lbSnatIPv6", input.Key)
	request.FallbackIP = "2001:db8::1"
	require.Nil(t, MissingSNATInput(request, ErrPoolExhausted))
}

func TestSNATDualStackUsesSeparateCheckpoints(t *testing.T) {
	s, b, _, session, obj, request := snatSetup(t)
	_, err := s.Bootstrap(context.Background(), session, obj, request)
	require.NoError(t, err)
	request.Family = "IPv6"
	request.VIPRanges = []AddressRange{{"2001:db8::1", "2001:db8::ff"}}
	b.reservation = Reservation{Family: "IPv6", IP: "2001:db8::10", Namespace: "vmware-system-netop", Name: "snat-v6", UID: "v6-uid"}
	_, err = s.Bootstrap(context.Background(), session, obj, request)
	require.NoError(t, err)
	require.Len(t, obj.Status.LBSNATReservations, 2)
	require.Equal(t, 2, b.reservations)
	_, err = s.Bootstrap(context.Background(), session, obj, request)
	require.NoError(t, err)
	require.Equal(t, 2, b.reservations)
}

func (b *snatBackend) VerifyTarget(context.Context, SNATRequest, Reservation) error { return nil }

func TestDeriveSNATVerificationDoesNotAllocate(t *testing.T) {
	s, b, _, _, _, request := snatSetup(t)
	require.NoError(t, s.Verify(context.Background(), request, b.reservation))
	require.Zero(t, b.reservations)
	require.Zero(t, b.configurations)
}

func TestSNATTargetMismatchDoesNotPoisonRecovery(t *testing.T) {
	s, b, _, session, obj, request := snatSetup(t)
	_, err := s.Bootstrap(context.Background(), session, obj, request)
	require.NoError(t, err)
	request.LBVPCUniqueID = "replacement"
	_, err = s.Bootstrap(context.Background(), session, obj, request)
	require.ErrorContains(t, err, "identity changed")
	request.LBVPCUniqueID = "lb-uid"
	_, err = s.Bootstrap(context.Background(), session, obj, request)
	require.NoError(t, err)
	require.Equal(t, 1, b.reservations)
}
