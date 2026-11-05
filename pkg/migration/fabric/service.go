/* Copyright © 2026 Broadcom, Inc. All Rights Reserved.
   SPDX-License-Identifier: Apache-2.0 */

// Package fabric prepares migration topology from the transition action and live NSX state.
// It is a shared service, not a complete Evaluate or Derive action handler.
package fabric

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/vmware/vsphere-automation-sdk-go/services/nsxt/model"
	"k8s.io/apimachinery/pkg/types"

	api "github.com/vmware-tanzu/nsx-operator/pkg/apis/migration/v1alpha1"
)

var ErrPending = errors.New("fabric resource is not ready")

type Kind string

const (
	TransitGateway      Kind = "TransitGateway"
	VPC                 Kind = "VPC"
	VPCAttachment       Kind = "VPCAttachment"
	VPCSubnet           Kind = "VPCSubnet"
	IPBlock             Kind = "IPBlock"
	DVC                 Kind = "DVC"
	ConnectivityProfile Kind = "ConnectivityProfile"
)

// BackendSpec is the low-level adapter request. Transition handlers use the
// resource-specific request types in requests.go instead.
type BackendSpec struct {
	Kind        Kind
	ID          string
	DisplayName string
	ProjectID   string
	VPCID       string
	// ExpectedUniqueID pins a previously inspected resource. It is required to
	// reuse a resource owned by another transition and prevents replacement if
	// the selected resource disappears or its identity changes.
	ExpectedUniqueID    string
	TransitGateway      *model.TransitGateway
	VPC                 *model.Vpc
	VPCAttachment       *model.VpcAttachment
	VPCSubnet           *model.VpcSubnet
	IPBlock             *model.IpAddressBlock
	DVC                 *model.DistributedVlanConnection
	ConnectivityProfile *model.VpcConnectivityProfile
}

// State is an authoritative read. Absence is explicit; read errors are never absence.
// Compatible compares every managed configuration field. Ready requires realization.
type State struct {
	Exists     bool
	UniqueID   string
	Owner      types.UID
	Compatible bool
	Ready      bool
}

// Backend must use stable paths and optimistic concurrency, recheck ownership before
// writing, preserve unmanaged fields, and never treat a transport error as absence.
// Apply creates or configures only resources owned by owner. Adoption is read-only.
type Backend interface {
	Inspect(context.Context, BackendSpec) (State, error)
	Apply(context.Context, BackendSpec, types.UID) error
}

type Service struct {
	Backend Backend
}

// Record is the result of a live reconciliation. It is returned to the caller and
// is not persisted as a generic transition status record.
type Record struct {
	Path       string
	UniqueID   string
	Provenance string
	Owner      types.UID
}

func (s BackendSpec) Path() string {
	switch s.Kind {
	case IPBlock:
		return "/infra/ip-blocks/" + s.ID
	case DVC:
		return "/infra/distributed-vlan-connections/" + s.ID
	case TransitGateway:
		return "/orgs/default/projects/" + s.ProjectID + "/transit-gateways/" + s.ID
	case VPC:
		return "/orgs/default/projects/" + s.ProjectID + "/vpcs/" + s.ID
	case VPCAttachment:
		return "/orgs/default/projects/" + s.ProjectID + "/vpcs/" + s.VPCID + "/attachments/" + s.ID
	case VPCSubnet:
		return "/orgs/default/projects/" + s.ProjectID + "/vpcs/" + s.VPCID + "/subnets/" + s.ID
	case ConnectivityProfile:
		return "/orgs/default/projects/" + s.ProjectID + "/vpc-connectivity-profiles/" + s.ID
	default:
		return ""
	}
}

// EnsureEvaluate prepares shared connectivity resources during Evaluate. Use it
// for TGWs, VPCs, attachments, connectivity profiles and IP blocks. Resolve
// reusable shared topology before calling; a missing resource path alone does
// not establish that creating a new shared resource is safe.
func (s *Service) EnsureEvaluate(ctx context.Context, owner types.UID, request EvaluateRequest) (Record, error) {
	if isNilRequest(request) {
		return Record{}, errors.New("Evaluate fabric request is required")
	}
	spec, err := request.evaluateSpec()
	if err != nil {
		return Record{}, err
	}
	return s.ensure(ctx, api.ActionEvaluate, owner, spec)
}

// EnsureDerive prepares resources derived from converted networks during Derive.
// Use it for VLAN-extension subnets, DVCs and any IP blocks created in this phase.
// Resolve existing resources for the same VDN/DVPG/VLAN before requesting creation.
func (s *Service) EnsureDerive(ctx context.Context, owner types.UID, request DeriveRequest) (Record, error) {
	if isNilRequest(request) {
		return Record{}, errors.New("Derive fabric request is required")
	}
	spec, err := request.deriveSpec()
	if err != nil {
		return Record{}, err
	}
	return s.ensure(ctx, api.ActionDerive, owner, spec)
}

func isNilRequest(request any) bool {
	if request == nil {
		return true
	}
	value := reflect.ValueOf(request)
	return value.Kind() == reflect.Pointer && value.IsNil()
}

// ensure reconciles one deterministic NSX path from live NSX state.
func (s *Service) ensure(ctx context.Context, action api.Action, owner types.UID, spec BackendSpec) (Record, error) {
	if owner == "" || s.Backend == nil {
		return Record{}, errors.New("fabric owner and backend are required")
	}
	if err := spec.Validate(); err != nil {
		return Record{}, err
	}
	if action != api.ActionEvaluate && action != api.ActionDerive {
		return Record{}, errors.New("fabric preparation is only permitted during Evaluate or Derive")
	}
	if !allowedForAction(action, spec.Kind) {
		return Record{}, fmt.Errorf("%s cannot prepare %s resources", action, spec.Kind)
	}
	state, err := s.Backend.Inspect(ctx, spec)
	if err != nil {
		return Record{}, err
	}
	if !state.Exists {
		return s.create(ctx, owner, spec)
	}
	if state.UniqueID == "" {
		return Record{}, errors.New("fabric resource has no unique identity")
	}
	if spec.ExpectedUniqueID != "" && spec.ExpectedUniqueID != state.UniqueID {
		return Record{}, errors.New("candidate fabric identity changed")
	}
	if state.Owner != owner {
		return reuseExisting(spec, state)
	}
	return s.updateOwned(ctx, owner, spec, state)
}

func (s *Service) create(ctx context.Context, owner types.UID, spec BackendSpec) (Record, error) {
	if spec.ExpectedUniqueID != "" {
		return Record{}, errors.New("requested fabric candidate no longer exists")
	}
	if err := s.Backend.Apply(ctx, spec, owner); err != nil {
		return Record{}, err
	}
	state, err := s.Backend.Inspect(ctx, spec)
	if err != nil {
		return Record{}, err
	}
	if !state.Exists {
		return Record{}, ErrPending
	}
	if state.UniqueID == "" {
		return Record{}, errors.New("created fabric resource has no unique identity")
	}
	if state.Owner != owner {
		return Record{}, errors.New("created fabric resource is not owned by this transition")
	}
	if !state.Compatible {
		return Record{}, errors.New("created fabric resource does not match the requested configuration")
	}
	if !state.Ready {
		return Record{}, ErrPending
	}
	return Record{Path: spec.Path(), UniqueID: state.UniqueID, Provenance: "Created", Owner: state.Owner}, nil
}

func (s *Service) updateOwned(ctx context.Context, owner types.UID, spec BackendSpec, state State) (Record, error) {
	record := Record{Path: spec.Path(), UniqueID: state.UniqueID, Provenance: "Created", Owner: owner}
	if !state.Compatible {
		spec.ExpectedUniqueID = state.UniqueID
		if err := s.Backend.Apply(ctx, spec, owner); err != nil {
			return Record{}, err
		}
		var err error
		state, err = s.Backend.Inspect(ctx, spec)
		if err != nil {
			return Record{}, err
		}
	}
	if !state.Exists || state.UniqueID != record.UniqueID || state.Owner != owner || !state.Compatible {
		return Record{}, errors.New("fabric resource identity, ownership or configuration changed during reconciliation")
	}
	if !state.Ready {
		return Record{}, ErrPending
	}
	return record, nil
}

func reuseExisting(spec BackendSpec, state State) (Record, error) {
	if spec.ExpectedUniqueID == "" {
		return Record{}, fmt.Errorf("explicit live adoption identity required for %s", spec.Path())
	}
	if !state.Compatible {
		return Record{}, errors.New("adopted fabric configuration is incompatible; it will not be modified")
	}
	if !state.Ready {
		return Record{}, ErrPending
	}
	return Record{Path: spec.Path(), UniqueID: state.UniqueID, Provenance: "Adopted", Owner: state.Owner}, nil
}

// allowedForAction keeps shared connectivity preparation in Evaluate and
// conversion resources in Derive.
func allowedForAction(action api.Action, kind Kind) bool {
	if action == api.ActionEvaluate {
		return evaluatePrerequisite(kind)
	}
	if action == api.ActionDerive {
		return kind == IPBlock || kind == VPCSubnet || kind == DVC
	}
	return false
}

func evaluatePrerequisite(kind Kind) bool {
	return kind == TransitGateway || kind == IPBlock || kind == ConnectivityProfile || kind == VPC || kind == VPCAttachment
}

// Validate rejects malformed requests before the first effect, including read-only
// SDK metadata. Nil managed fields retain NSX defaults; explicitly empty lists
// request empty configuration.
func (s BackendSpec) Validate() error {
	if !validID(s.ID) || (s.Kind != IPBlock && s.Kind != DVC && !validID(s.ProjectID)) {
		return errors.New("fabric identity is required")
	}
	if (s.Kind == VPCSubnet || s.Kind == VPCAttachment) && !validID(s.VPCID) {
		return errors.New("VPC child resource requires a valid VPC ID")
	}
	payloads := []any{s.TransitGateway, s.VPC, s.VPCAttachment, s.VPCSubnet, s.IPBlock, s.DVC, s.ConnectivityProfile}
	kinds := []Kind{TransitGateway, VPC, VPCAttachment, VPCSubnet, IPBlock, DVC, ConnectivityProfile}
	count := 0
	for i, p := range payloads {
		v := reflect.ValueOf(p)
		if v.IsNil() {
			continue
		}
		count++
		if s.Kind != kinds[i] {
			return errors.New("fabric kind and payload differ")
		}
		v = v.Elem()
		for j := 0; j < v.NumField(); j++ {
			field := v.Type().Field(j).Name
			if field == "DisplayName" {
				if !v.Field(j).IsNil() {
					return errors.New("set the requested fabric display name through BackendSpec.DisplayName")
				}
				continue
			}
			if !ManagedField(s.Kind, field) && !v.Field(j).IsZero() {
				return fmt.Errorf("fabric payload must not set server or unsupported field %s", field)
			}
		}
	}
	if s.DisplayName != "" && s.DisplayName != s.ID {
		return errors.New("fabric display name must match its ID")
	}
	if s.ConnectivityProfile != nil {
		tgwPrefix := "/orgs/default/projects/" + s.ProjectID + "/transit-gateways/"
		if s.ConnectivityProfile.TransitGatewayPath == nil || !resourcePathHasID(tgwPrefix, *s.ConnectivityProfile.TransitGatewayPath) {
			return errors.New("VPC connectivity profile requires a TGW path in the same project")
		}
	}
	if s.VPCAttachment != nil {
		profilePrefix := "/orgs/default/projects/" + s.ProjectID + "/vpc-connectivity-profiles/"
		if s.VPCAttachment.VpcConnectivityProfile == nil || !resourcePathHasID(profilePrefix, *s.VPCAttachment.VpcConnectivityProfile) {
			return errors.New("VPC attachment requires a connectivity profile path in the same project")
		}
	}
	if count != 1 {
		return errors.New("exactly one fabric payload is required")
	}
	if s.DVC != nil {
		dvc := s.DVC
		if dvc.VlanId == nil || *dvc.VlanId < 0 || *dvc.VlanId > 4094 || dvc.SubnetExtensionConnection == nil || *dvc.SubnetExtensionConnection != model.DistributedVlanConnection_SUBNET_EXTENSION_CONNECTION_ENABLED_L2_AND_L3 {
			return errors.New("migration DVC requires a resolved VLAN and ENABLED_L2_AND_L3 connectivity")
		}
		availability := dvc.RestrictedAvailability
		if availability == nil || ((availability.SpanPath == nil || *availability.SpanPath == "") && len(availability.ZoneExternalIds) == 0) {
			return errors.New("migration DVC requires resolved zone/span availability")
		}
	}
	if s.VPCSubnet != nil {
		if s.VPCSubnet.VlanConnection == nil || !resourcePathHasID("/infra/distributed-vlan-connections/", *s.VPCSubnet.VlanConnection) {
			return errors.New("VPC subnet requires a valid DVC VLAN connection path")
		}
		if s.VPCSubnet.AccessMode == nil || *s.VPCSubnet.AccessMode != model.VpcSubnet_ACCESS_MODE_PUBLIC {
			return errors.New("migration VLAN extension subnet requires Public access mode")
		}
	}
	return nil
}

func validID(id string) bool {
	if len(id) == 0 || len(id) > 255 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}

	return id != "." && id != ".."
}

func resourcePathHasID(prefix, path string) bool {
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	id := strings.TrimPrefix(path, prefix)
	return validID(id) && !strings.Contains(id, "/")
}

// ManagedField is the explicit set that the SDK adapter compares and configures.
// All other fields, including revision and external-controller metadata, survive updates.
func ManagedField(kind Kind, field string) bool {
	fields := map[Kind][]string{
		TransitGateway:      {"DisplayName", "Span", "ExternalIpSignalingMode", "TransitSubnets"},
		VPC:                 {"DisplayName", "ExternalIpv4Blocks", "IpAddressType", "PrivateIps", "Ipv6ProfilePaths", "LoadBalancerVpcEndpoint", "VpcServiceProfile", "TransitGatewayPath"},
		VPCAttachment:       {"DisplayName", "VpcConnectivityProfile", "PreferredDefaultSnatIp"},
		VPCSubnet:           {"DisplayName", "AccessMode", "IpAddressType", "IpAddresses", "IpBlocks", "Ipv4SubnetSize", "Ipv6PrefixLength", "VlanConnection"},
		IPBlock:             {"DisplayName", "Cidrs", "Ranges", "ExcludedIps", "IpAddressType", "Visibility"},
		DVC:                 {"DisplayName", "AssociatedIpBlockPaths", "GatewayAddresses", "RestrictedAvailability", "SubnetExtensionConnection", "VlanId"},
		ConnectivityProfile: {"DisplayName", "ExternalIpBlocks", "Ipv6Blocks", "PrivateTgwIpBlocks", "ServiceGateway", "TransitGatewayPath"},
	}
	for _, f := range fields[kind] {
		if f == field {
			return true
		}
	}
	return false
}
