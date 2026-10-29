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
	"sort"

	"github.com/vmware/vsphere-automation-sdk-go/runtime/data/serializers/cleanjson"
	"github.com/vmware/vsphere-automation-sdk-go/services/nsxt/model"
	"k8s.io/apimachinery/pkg/types"

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
	Consumers           []Consumer
	TransitGateway      *model.TransitGateway
	VPC                 *model.Vpc
	IPBlock             *model.IpAddressBlock
	DVC                 *model.DistributedVlanConnection
	ConnectivityProfile *model.VpcConnectivityProfile
}

// MarshalJSON includes the SDK's opaque polymorphic TGW span in the journal and
// payload digest. encoding/json alone serializes data.StructValue as {}, losing
// transport-zone changes and allowing a different payload to reuse an intent.
func (s Spec) MarshalJSON() ([]byte, error) {
	type plain Spec
	var span json.RawMessage
	if s.TransitGateway != nil && s.TransitGateway.Span != nil {
		encoded, err := cleanjson.NewDataValueToJsonEncoder().Encode(s.TransitGateway.Span)
		if err != nil {
			return nil, err
		}
		span = json.RawMessage(encoded)
	}
	return json.Marshal(struct {
		plain
		Span json.RawMessage
	}{plain(s), span})
}

type Consumer struct {
	Kind string
	Name string
	UID  types.UID
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
	Inspect(context.Context, Spec) (State, error)
	Apply(context.Context, Spec, types.UID) error
}

type Service struct{ Backend Backend }

// Record retains creation/adoption provenance and consumer UIDs across action
// changes. No service method deletes resources or removes consumers; cleanup must
// also inspect all journals and live consumers, including other transition CRs.
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

// Ensure creates/adopts/configures one resource, journaling intent first. Callers
// supply resources in dependency order and stop on ErrPending. Every retry checks
// live state, even if recovery.Effect already recorded the operation as Applied.
func (s *Service) Ensure(ctx context.Context, session *recovery.Session, spec Spec) (Record, error) {
	if session == nil || s.Backend == nil {
		return Record{}, errors.New("fabric preparation requires a recovery session and backend")
	}
	if err := spec.Validate(); err != nil {
		return Record{}, err
	}
	if session.Identity().Action != "Evaluate" && session.Identity().Action != "Derive" {
		return Record{}, errors.New("fabric preparation is only permitted during Evaluate or Derive")
	}
	if session.Identity().Action == "Evaluate" && spec.Kind != TransitGateway && spec.Kind != VPC {
		return Record{}, errors.New("Evaluate container preparation cannot realize DVCs, blocks or connectivity profiles; use the dedicated SNAT bootstrap")
	}
	if err := session.Check(ctx); err != nil {
		return Record{}, err
	}
	spec.Consumers = append([]Consumer(nil), spec.Consumers...)
	sort.Slice(spec.Consumers, func(i, j int) bool { return spec.Consumers[i].UID < spec.Consumers[j].UID })
	path := spec.Path()
	name := "fabric-" + digest(path)
	history, err := session.History(ctx)
	if err != nil {
		return Record{}, err
	}
	var prior *Record
	for _, journal := range history {
		if value, ok := journal.Checkpoints[name+"-observed"]; ok {
			var r Record
			if err := json.Unmarshal(value, &r); err != nil {
				return Record{}, err
			}
			if r.Path != path || r.UniqueID == "" {
				return Record{}, errors.New("invalid fabric identity checkpoint")
			}
			if prior != nil && (prior.UniqueID != r.UniqueID || prior.Provenance != r.Provenance || prior.Owner != r.Owner) {
				return Record{}, errors.New("conflicting fabric history")
			}
			prior = &r
		}
	}
	state, err := s.Backend.Inspect(ctx, spec)
	if err != nil {
		return Record{}, err
	}
	if prior != nil && (!state.Exists || state.UniqueID != prior.UniqueID) {
		return Record{}, fmt.Errorf("recorded fabric resource %s was removed or replaced", path)
	}
	record := Record{Path: path, Provenance: "Created", Owner: session.Identity().UID, Consumers: spec.Consumers}
	if prior != nil {
		record.Provenance = prior.Provenance
		record.Owner = prior.Owner
	}
	if state.Exists {
		if state.UniqueID == "" {
			return Record{}, errors.New("fabric resource has no unique identity")
		}
		if prior == nil && state.Owner != session.Identity().UID {
			if spec.AdoptUniqueID == "" || spec.AdoptUniqueID != state.UniqueID {
				return Record{}, fmt.Errorf("explicit adoption identity required for %s", path)
			}
			record.Provenance = "Adopted"
			record.Owner = state.Owner
		}
		if record.Owner != state.Owner {
			return Record{}, errors.New("fabric ownership changed")
		}
		if record.Provenance == "Adopted" && !state.Compatible {
			return Record{}, errors.New("adopted fabric configuration is incompatible; it will not be modified")
		}
	} else if spec.AdoptUniqueID != "" {
		return Record{}, errors.New("requested adoption resource does not exist")
	}
	// Persist adoption/creation decision and consumers before any external effect.
	// Same-action retries cannot substitute a new payload or adoption decision.
	intent := struct {
		Spec   Spec
		Record Record
	}{spec, record}
	data, err := json.Marshal(intent)
	if err != nil {
		return Record{}, err
	}
	if err := session.Checkpoint(ctx, name+"-intent", data); err != nil {
		return Record{}, err
	}
	pin := func(current State) error {
		observed := record
		observed.UniqueID = current.UniqueID
		data, err := json.Marshal(observed)
		if err != nil {
			return err
		}
		return session.Checkpoint(ctx, name+"-observed", data)
	}
	if state.Exists {
		if err := pin(state); err != nil {
			return Record{}, err
		}
	}
	probe := func(ctx context.Context, _ string) (recovery.Observation, error) {
		current, err := s.Backend.Inspect(ctx, spec)
		if err != nil {
			return recovery.Pending, err
		}
		if prior != nil && (!current.Exists || current.UniqueID != prior.UniqueID) {
			return recovery.Pending, errors.New("fabric resource identity changed")
		}
		if current.Exists {
			if current.Owner != record.Owner || current.UniqueID == "" {
				return recovery.Pending, errors.New("fabric resource ownership conflict")
			}
			if record.Provenance == "Adopted" && current.UniqueID != spec.AdoptUniqueID && prior == nil {
				return recovery.Pending, errors.New("adoption identity changed")
			}
			if err := pin(current); err != nil {
				return recovery.Pending, err
			}
			if !current.Compatible {
				if record.Provenance == "Adopted" {
					return recovery.Pending, errors.New("adopted configuration changed")
				}
				return recovery.Absent, nil // desired configuration is authoritatively absent
			}
			if !current.Ready {
				return recovery.Pending, nil
			}
			return recovery.Applied, nil
		}
		if record.Provenance == "Adopted" {
			return recovery.Pending, errors.New("adopted resource disappeared")
		}
		return recovery.Absent, nil
	}
	if err := session.Effect(ctx, name, path+":"+digest(spec), probe, func(ctx context.Context, _ string) error {
		return s.Backend.Apply(ctx, spec, session.Identity().UID)
	}); err != nil {
		return Record{}, err
	}
	observation, err := probe(ctx, "")
	if err != nil {
		return Record{}, err
	}
	if observation != recovery.Applied {
		return Record{}, recovery.ErrPending
	}
	state, err = s.Backend.Inspect(ctx, spec)
	if err != nil {
		return Record{}, err
	}
	if !state.Exists || !state.Ready || !state.Compatible || state.UniqueID == "" || state.Owner != record.Owner {
		return Record{}, recovery.ErrPending
	}
	if err := pin(state); err != nil {
		return Record{}, err
	}
	record.UniqueID = state.UniqueID
	if prior != nil && record.UniqueID != prior.UniqueID {
		return Record{}, errors.New("fabric resource replaced during verification")
	}
	data, err = json.Marshal(record)
	if err != nil {
		return Record{}, err
	}
	if err := session.Checkpoint(ctx, name+"-identity", data); err != nil {
		return Record{}, err
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
