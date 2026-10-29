/* Copyright © 2026 Broadcom, Inc. All Rights Reserved.
   SPDX-License-Identifier: Apache-2.0 */

// Package fabric prepares migration topology under durable mutation ownership.
// It is a shared service, not a complete Evaluate or Derive action handler.
package fabric

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/vmware/vsphere-automation-sdk-go/services/nsxt/model"
	"k8s.io/apimachinery/pkg/types"

	api "github.com/vmware-tanzu/nsx-operator/pkg/apis/migration/v1alpha1"
	"github.com/vmware-tanzu/nsx-operator/pkg/migration/recovery"
)

type Kind string

const (
	TransitGateway      Kind = "TransitGateway"
	VPC                 Kind = "VPC"
	IPBlock             Kind = "IPBlock"
	DVC                 Kind = "DVC"
	ConnectivityProfile Kind = "ConnectivityProfile"
)

// Spec is an internal, reviewed resource request, not a CRD. Exactly one payload
// must be set. Payload identity, tags and server metadata are supplied by Backend.
// DVS/transport-zone discovery and approved plan synthesis belong to callers.
type Spec struct {
	Kind                Kind
	ID                  string
	ProjectID           string
	AdoptUniqueID       string
	ExpectedUniqueID    string
	Consumers           []Consumer
	TransitGateway      *model.TransitGateway
	VPC                 *model.Vpc
	IPBlock             *model.IpAddressBlock
	DVC                 *model.DistributedVlanConnection
	ConnectivityProfile *model.VpcConnectivityProfile
}

type Consumer = api.FabricConsumerStatus

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
	Inspect(context.Context, Spec) (State, error)
	Apply(context.Context, Spec, types.UID) error
}

// ResourceStatus persists identities in the transition status using optimistic concurrency.
type ResourceStatus interface {
	Load(context.Context, recovery.Identity, string) (*Record, error)
	Save(context.Context, recovery.Identity, Record) error
}

type Service struct {
	Backend Backend
	Status  ResourceStatus
}

// Record returns creation/adoption provenance and consumer UIDs across action
// changes. No service method deletes resources or removes consumers; cleanup must
// also inspect all transition status records and live consumers.
type Record struct {
	Path       string
	UniqueID   string
	Provenance string
	Owner      types.UID
	Consumers  []Consumer
}

func (s Spec) Path() string {
	switch s.Kind {
	case IPBlock:
		return "/infra/ip-blocks/" + s.ID
	case DVC:
		return "/infra/distributed-vlan-connections/" + s.ID
	case TransitGateway:
		return "/orgs/default/projects/" + s.ProjectID + "/transit-gateways/" + s.ID
	case VPC:
		return "/orgs/default/projects/" + s.ProjectID + "/vpcs/" + s.ID
	case ConnectivityProfile:
		return "/orgs/default/projects/" + s.ProjectID + "/vpc-connectivity-profiles/" + s.ID
	default:
		return ""
	}
}

func digest(v any) string {
	data, _ := json.Marshal(v)
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:16])
}

// Ensure reconciles a deterministic NSX path and persists its observed identity in
// CR status. It does not write operation steps or payloads to the recovery journal.
// Callers must preserve these status checkpoints when publishing action results.
func (s *Service) Ensure(ctx context.Context, session *recovery.Session, spec Spec) (Record, error) {
	if session == nil || s.Backend == nil || s.Status == nil {
		return Record{}, errors.New("fabric preparation requires a recovery session, backend and status store")
	}
	if err := spec.Validate(); err != nil {
		return Record{}, err
	}
	id := session.Identity()
	if id.Action != "Evaluate" && id.Action != "Derive" {
		return Record{}, errors.New("fabric preparation is only permitted during Evaluate or Derive")
	}
	if id.Action == "Evaluate" && spec.Kind != TransitGateway && spec.Kind != VPC {
		return Record{}, errors.New("Evaluate container preparation cannot realize DVCs, blocks or connectivity profiles; use the dedicated SNAT bootstrap")
	}
	if err := session.CheckMutation(ctx); err != nil {
		return Record{}, err
	}
	prior, err := s.Status.Load(ctx, id, spec.Path())
	if err != nil {
		return Record{}, err
	}
	record := Record{Path: spec.Path(), Provenance: "Created", Owner: id.UID, Consumers: spec.Consumers}
	if prior != nil {
		record.UniqueID, record.Provenance, record.Owner = prior.UniqueID, prior.Provenance, prior.Owner
	}
	inspect := func() (State, error) {
		state, err := s.Backend.Inspect(ctx, spec)
		if err != nil {
			return State{}, err
		}
		if record.UniqueID != "" && (!state.Exists || state.UniqueID != record.UniqueID) {
			return State{}, errors.New("recorded fabric resource was removed or replaced; identity changed")
		}
		if !state.Exists {
			if spec.AdoptUniqueID != "" {
				return State{}, errors.New("requested adoption resource does not exist")
			}
			return state, nil
		}
		if state.UniqueID == "" {
			return State{}, errors.New("fabric resource has no unique identity")
		}
		if record.UniqueID == "" && state.Owner != id.UID {
			if spec.AdoptUniqueID == "" || spec.AdoptUniqueID != state.UniqueID {
				return State{}, fmt.Errorf("explicit adoption identity required for %s", record.Path)
			}
			record.Provenance, record.Owner = "Adopted", state.Owner
		}
		if record.Owner != state.Owner {
			return State{}, errors.New("fabric ownership changed")
		}
		if record.Provenance == "Adopted" && !state.Compatible {
			return State{}, errors.New("adopted fabric configuration is incompatible; it will not be modified")
		}
		record.UniqueID = state.UniqueID
		// Pin identity even while realization is pending and before configuring it.
		if err := session.CheckMutation(ctx); err != nil {
			return State{}, err
		}
		if err := s.Status.Save(ctx, id, record); err != nil {
			return State{}, err
		}
		return state, nil
	}
	state, err := inspect()
	if err != nil {
		return Record{}, err
	}
	if !state.Exists || !state.Compatible {
		if err := session.CheckMutation(ctx); err != nil {
			return Record{}, err
		}
		// Backend rechecks ownership/revision. The expected incarnation closes the
		// read-to-write gap if the resource disappeared or was replaced meanwhile.
		spec.ExpectedUniqueID = record.UniqueID
		if err := s.Backend.Apply(ctx, spec, id.UID); err != nil {
			return Record{}, err
		}
		state, err = inspect()
		if err != nil {
			return Record{}, err
		}
	}
	if !state.Exists || !state.Compatible || !state.Ready {
		return Record{}, recovery.ErrPending
	}
	return record, nil
}

// Validate rejects malformed requests before the first effect, including read-only
// SDK metadata. Nil managed fields retain NSX defaults; explicitly empty lists
// request empty configuration.
func (s Spec) Validate() error {
	if !validID(s.ID) || (s.Kind != IPBlock && s.Kind != DVC && !validID(s.ProjectID)) || len(s.Consumers) == 0 {
		return errors.New("fabric identity and consumer UIDs are required")
	}
	seen := map[types.UID]bool{}
	for _, c := range s.Consumers {
		if c.UID == "" || c.Name == "" || c.Kind == "" || seen[c.UID] {
			return errors.New("invalid or duplicate fabric consumer")
		}
		seen[c.UID] = true
	}
	payloads := []any{s.TransitGateway, s.VPC, s.IPBlock, s.DVC, s.ConnectivityProfile}
	kinds := []Kind{TransitGateway, VPC, IPBlock, DVC, ConnectivityProfile}
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
			if !ManagedField(s.Kind, field) && !v.Field(j).IsZero() {
				return fmt.Errorf("fabric payload must not set server or unsupported field %s", field)
			}
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

// ManagedField is the explicit set that the SDK adapter compares and configures.
// All other fields, including revision and external-controller metadata, survive updates.
func ManagedField(kind Kind, field string) bool {
	fields := map[Kind][]string{
		TransitGateway:      {"Span", "ExternalIpSignalingMode", "TransitSubnets"},
		VPC:                 {"ExternalIpv4Blocks", "IpAddressType", "PrivateIps", "Ipv6ProfilePaths", "LoadBalancerVpcEndpoint", "VpcServiceProfile"},
		IPBlock:             {"Cidrs", "Ranges", "ExcludedIps", "IpAddressType", "Visibility"},
		DVC:                 {"AssociatedIpBlockPaths", "GatewayAddresses", "RestrictedAvailability", "SubnetExtensionConnection", "VlanId"},
		ConnectivityProfile: {"ExternalIpBlocks", "Ipv6Blocks", "PrivateTgwIpBlocks", "ServiceGateway", "TransitGatewayPath"},
	}
	for _, f := range fields[kind] {
		if f == field {
			return true
		}
	}
	return false
}
