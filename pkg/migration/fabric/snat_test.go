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
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type snatBackend struct {
	supportedErr, readyErr, reserveErr, verifyErr, configureErr error
	reservations, configurations, verifications                 int
	reservation                                                 Reservation
	frontend                                                    []AddressRange
}

func (b *snatBackend) Supported(context.Context, SNATRequest) error { return b.supportedErr }
func (b *snatBackend) LBReady(context.Context, SNATRequest) error   { return b.readyErr }
func (b *snatBackend) Reserve(context.Context, SNATRequest) (Reservation, error) {
	b.reservations++
	return b.reservation, b.reserveErr
}
func (b *snatBackend) VerifySource(context.Context, Reservation) error {
	b.verifications++
	return b.verifyErr
}
func (b *snatBackend) Configure(_ context.Context, _ SNATRequest, _ Reservation, frontend []AddressRange) error {
	b.configurations++
	b.frontend = frontend
	return b.configureErr
}
func (b *snatBackend) VerifyTarget(context.Context, SNATRequest, Reservation) error { return nil }

type failingStatus struct{}

func (failingStatus) Accept(context.Context, *api.ControlPlaneNetworkTransition, Reservation) error {
	return errors.New("status unavailable")
}

func snatSetup(t *testing.T) (*SNATService, *snatBackend, client.Client, *api.ControlPlaneNetworkTransition, SNATRequest) {
	t.Helper()
	c, obj := setup(t)
	b := &snatBackend{reservation: Reservation{Family: "IPv4", IP: "192.0.2.5", Namespace: SNATNamespace, Name: "snat"}}
	s := &SNATService{Backend: b, Status: failingStatus{}}
	request := SNATRequest{Family: "IPv4", LBVPCPath: obj.Status.LoadBalancerVPCPath, VIPRanges: []AddressRange{{"192.0.2.1", "192.0.2.10"}}}
	return s, b, c, obj, request
}

func TestSNATRetryUsesAcceptedStatus(t *testing.T) {
	s, b, c, obj, request := snatSetup(t)
	_, err := s.Bootstrap(context.Background(), obj, request)
	require.ErrorContains(t, err, "status unavailable")
	require.Zero(t, b.configurations)
	require.Equal(t, 1, b.reservations)

	s.Status = &KubernetesReservationStatus{Client: c, Reader: c}
	accepted, err := s.Bootstrap(context.Background(), obj, request)
	require.NoError(t, err)
	require.Equal(t, "192.0.2.5", accepted.IP)
	require.Equal(t, 2, b.reservations, "reserve must be idempotent because status persistence may fail")
	require.Equal(t, 1, b.configurations)
	require.Equal(t, []AddressRange{{"192.0.2.1", "192.0.2.4"}, {"192.0.2.6", "192.0.2.10"}}, b.frontend)

	_, err = s.Bootstrap(context.Background(), obj, request)
	require.NoError(t, err)
	require.Equal(t, 2, b.reservations, "accepted transition status prevents another allocation")
	require.Equal(t, 2, b.configurations)
}

func TestSNATPreconditionsRunBeforeAllocation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*snatBackend, *api.ControlPlaneNetworkTransition, *SNATRequest)
	}{
		{"unsupported contract", func(b *snatBackend, _ *api.ControlPlaneNetworkTransition, _ *SNATRequest) {
			b.supportedErr = errors.New("SNAT API unavailable")
		}},
		{"LB pending", func(b *snatBackend, _ *api.ControlPlaneNetworkTransition, _ *SNATRequest) { b.readyErr = ErrPending }},
		{"unsupported provider", func(_ *snatBackend, o *api.ControlPlaneNetworkTransition, _ *SNATRequest) {
			o.Status.TargetLBProvider = "AVI"
		}},
		{"wrong action", func(_ *snatBackend, o *api.ControlPlaneNetworkTransition, _ *SNATRequest) {
			o.Spec.Action = api.ActionDerive
		}},
		{"unknown family", func(_ *snatBackend, _ *api.ControlPlaneNetworkTransition, r *SNATRequest) { r.Family = "unknown" }},
		{"IPv6 is not supported for VDS", func(_ *snatBackend, _ *api.ControlPlaneNetworkTransition, r *SNATRequest) { r.Family = "IPv6" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, b, _, obj, request := snatSetup(t)
			tc.change(b, obj, &request)
			_, err := s.Bootstrap(context.Background(), obj, request)
			require.Error(t, err)
			require.Zero(t, b.reservations)
			require.Zero(t, b.configurations)
		})
	}
}

func TestSNATInputOnlyOnConfirmedExhaustion(t *testing.T) {
	request := SNATRequest{Family: "IPv4"}
	for _, err := range []error{errors.New("CannotAllocIP"), errors.New("unauthorized"), ErrPending} {
		require.Nil(t, MissingSNATInput(request, err))
	}
	input := MissingSNATInput(request, ErrPoolExhausted)
	require.NotNil(t, input)
	require.Equal(t, "spec.networkConfiguration.lbSnatIPs", input.FieldName)
	request.FallbackIP = "192.0.2.1"
	require.Nil(t, MissingSNATInput(request, ErrPoolExhausted))
}

func TestSNATVerificationDoesNotAllocate(t *testing.T) {
	s, b, _, _, request := snatSetup(t)
	require.NoError(t, s.Verify(context.Background(), request, b.reservation))
	require.Zero(t, b.reservations)
	require.Zero(t, b.configurations)
}
