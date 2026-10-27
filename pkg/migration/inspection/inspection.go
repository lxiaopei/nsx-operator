/* Copyright © 2026 Broadcom, Inc. All Rights Reserved.
   SPDX-License-Identifier: Apache-2.0 */

package inspection

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/vmware-tanzu/nsx-operator/pkg/apis/migration/v1alpha1"
	migrationstatus "github.com/vmware-tanzu/nsx-operator/pkg/migration/status"
)

const ConditionNetworkInputsValidated = "NetworkInputsValidated"
const inputPrefix = "networkInspection/"

// ConfigurationError identifies incompatible inventory, as distinct from a
// transient API failure. It becomes EvaluationFailed, never NeedsInput.
type ConfigurationError struct{ Message string }

func (e *ConfigurationError) Error() string { return e.Message }
func invalid(format string, args ...any) error {
	return &ConfigurationError{Message: fmt.Sprintf(format, args...)}
}

type Topology struct {
	PortGroupID string
	SwitchID    string
	SwitchUUID  string
	ClusterIDs  []string
}

// Source is authoritative inventory evidence. DHCP is never inferred from an
// empty IP pool. Unsupported/unknown assignment modes must fail discovery.
type Source struct {
	VDNName       string
	Topology      Topology
	DHCP          bool
	Mask, Gateway string
	Ranges        []api.IPAddressRange
}

type Inventory interface {
	NamespaceVDNs(context.Context, []string) ([]string, error)
	Resolve(context.Context, string) (Source, error)
}

type Network struct {
	Source     Source
	Addressing Addressing
}

type Result struct {
	Networks       []Network
	RequiredInputs []api.RequiredInput
	Problems       []string
}

func (r Result) Ready() bool {
	return len(r.RequiredInputs) == 0 && len(r.Problems) == 0 && len(r.Networks) > 0
}

type Service struct{ Inventory Inventory }

// InspectNamespaces derives VDN membership from namespaced Network resources,
// not the administrator's input list. Shared VDNs are inspected once.
func (s Service) InspectNamespaces(ctx context.Context, namespaces []string, inputs []api.DHCPNetworkConfiguration) (Result, error) {
	if s.Inventory == nil {
		return Result{}, fmt.Errorf("inventory is required")
	}
	names, err := s.Inventory.NamespaceVDNs(ctx, namespaces)
	if err != nil {
		return Result{}, err
	}
	return s.Inspect(ctx, names, inputs)
}

// Inspect accepts the authoritative source VDN set. For control plane this must
// come from WCP's CPVM/service network configuration, never networkConfiguration. API or
// transport errors are returned for retry and are not classified as missing input.
func (s Service) Inspect(ctx context.Context, names []string, inputs []api.DHCPNetworkConfiguration) (Result, error) {
	r := Result{}
	if s.Inventory == nil {
		return r, fmt.Errorf("inventory is required")
	}
	set := map[string]bool{}
	for _, name := range names {
		if name == "" {
			return r, fmt.Errorf("empty source VDN identity")
		}
		set[name] = true
	}
	if len(set) == 0 {
		return r, fmt.Errorf("source network inventory is empty")
	}
	names = make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	byName := map[string]api.DHCPNetworkConfiguration{}
	for _, in := range inputs {
		if !set[in.VDNName] {
			r.Problems = append(r.Problems, fmt.Sprintf("unknown input VDN %q", in.VDNName))
			continue
		}
		if _, found := byName[in.VDNName]; found {
			r.Problems = append(r.Problems, fmt.Sprintf("duplicate input VDN %q", in.VDNName))
			continue
		}
		byName[in.VDNName] = in
	}
	backings := map[string]Network{}
	for _, name := range names {
		source, err := s.Inventory.Resolve(ctx, name)
		if err != nil {
			var configuration *ConfigurationError
			if errors.As(err, &configuration) {
				r.Problems = append(r.Problems, fmt.Sprintf("VDN %s: %v", name, err))
				continue
			}
			return Result{}, fmt.Errorf("resolve VDN %s: %w", name, err)
		}
		if source.VDNName != name || source.Topology.PortGroupID == "" || source.Topology.SwitchID == "" || source.Topology.SwitchUUID == "" || len(source.Topology.ClusterIDs) == 0 {
			r.Problems = append(r.Problems, fmt.Sprintf("VDN %s has incomplete backing topology", name))
			continue
		}
		in, supplied := byName[name]
		mask, gateway, ranges := source.Mask, source.Gateway, source.Ranges
		if source.DHCP {
			missing, problems := checkInput(name, in)
			r.RequiredInputs = append(r.RequiredInputs, missing...)
			r.Problems = append(r.Problems, problems...)
			if len(missing) > 0 || len(problems) > 0 {
				continue
			}
			mask, gateway, ranges = in.SubnetMask, in.GatewayAddress, []api.IPAddressRange{*in.DHCPRange}
		} else if supplied {
			r.Problems = append(r.Problems, fmt.Sprintf("VDN %s is static; user DHCP inputs are not applicable", name))
			continue
		}
		addressing, err := ValidateAddressing(mask, gateway, ranges)
		if err != nil {
			r.Problems = append(r.Problems, fmt.Sprintf("VDN %s: %v", name, err))
			continue
		}
		n := Network{Source: source, Addressing: addressing}
		key := source.Topology.SwitchUUID + "/" + source.Topology.PortGroupID
		if previous, exists := backings[key]; exists && (previous.Source.DHCP != source.DHCP || !reflect.DeepEqual(previous.Addressing, addressing)) {
			r.Problems = append(r.Problems, fmt.Sprintf("VDNs %s and %s have conflicting inputs for backing %s", previous.Source.VDNName, name, key))
		}
		backings[key] = n
		r.Networks = append(r.Networks, n)
	}
	sort.Strings(r.Problems)
	return r, nil
}

func checkInput(name string, in api.DHCPNetworkConfiguration) ([]api.RequiredInput, []string) {
	var missing []api.RequiredInput
	var missingFields []string
	var problems []string
	fields := []struct{ name, value string }{{"subnetMask", in.SubnetMask}, {"gatewayAddress", in.GatewayAddress}}
	start, end := "", ""
	if in.DHCPRange != nil {
		start, end = in.DHCPRange.Start, in.DHCPRange.End
	}
	fields = append(fields, struct{ name, value string }{"dhcpRange.start", start}, struct{ name, value string }{"dhcpRange.end", end})
	for _, f := range fields {
		if f.value == "" {
			missingFields = append(missingFields, f.name)
			continue
		}
		valid := true
		if f.name == "subnetMask" {
			m, err := netip.ParseAddr(f.value)
			valid = err == nil && m.Is4()
			if valid {
				b := m.As4()
				_, bits := net.IPMask(b[:]).Size()
				valid = bits == 32
			}
		} else {
			_, err := ipv4(f.value)
			valid = err == nil
		}
		if !valid {
			problems = append(problems, fmt.Sprintf("VDN %s: invalid %s %q", name, f.name, f.value))
		}
	}
	if a, err := ipv4(start); err == nil {
		if b, err := ipv4(end); err == nil && a.Compare(b) > 0 {
			problems = append(problems, fmt.Sprintf("VDN %s: DHCP range start exceeds end", name))
		}
	}
	if len(missingFields) > 0 {
		missing = append(missing, api.RequiredInput{Key: fmt.Sprintf("%s%x", inputPrefix, sha256.Sum256([]byte(name))), Description: fmt.Sprintf("Supply %s for DHCP-backed VDN %s in the transition's subnet inputs", strings.Join(missingFields, ", "), name)})
	}
	// Check relationships among available values even if another field is absent.
	if m, err := netip.ParseAddr(in.SubnetMask); err == nil && m.Is4() {
		b := m.As4()
		ones, bits := net.IPMask(b[:]).Size()
		if a, err := ipv4(start); err == nil && bits == 32 {
			p := netip.PrefixFrom(a, ones).Masked()
			for _, value := range []string{end, in.GatewayAddress} {
				if ip, err := ipv4(value); err == nil && !p.Contains(ip) {
					problems = append(problems, fmt.Sprintf("VDN %s: address %s is outside %s", name, value, p))
				}
			}
		}
	}
	return missing, problems
}

// Apply reports this service's observation only. The caller persists it with the
// status package's optimistic writer against the inspected CR snapshot. It does
// not record action completion or erase other services' requiredInputs.
func (r Result) Apply(target *api.TransitionStatus, generation int64) error {
	if target == nil {
		return fmt.Errorf("status is required")
	}
	next := target.DeepCopy()
	phase, reason, message, condition := api.PhaseEvaluating, "NetworkInspectionComplete", "Source networks and addressing validated", metav1.ConditionTrue
	if !r.Ready() {
		phase, reason, message, condition = api.PhaseNeedsInput, "MissingNetworkInputs", "Supply the required DHCP network inputs", metav1.ConditionFalse
	}
	if len(r.Problems) > 0 || (len(r.Networks) == 0 && len(r.RequiredInputs) == 0) {
		phase, reason, message, condition = api.PhaseEvaluationFailed, "InvalidNetworkConfiguration", strings.Join(r.Problems, "; "), metav1.ConditionFalse
		if message == "" {
			message = "Source network inventory is empty"
		}
	}
	if len(message) > 4096 {
		message = message[:4093] + "..."
	}
	if err := migrationstatus.Observe(next, generation, phase, metav1.Condition{Type: ConditionNetworkInputsValidated, Status: condition, Reason: reason, Message: message}); err != nil {
		return err
	}
	kept := make([]api.RequiredInput, 0)
	for _, input := range next.RequiredInputs {
		if !strings.HasPrefix(input.Key, inputPrefix) &&
			!strings.HasPrefix(input.Key, "namespaceDHCPNetworks/") &&
			!strings.HasPrefix(input.Key, "controlPlaneAndServiceDHCPNetworks/") {
			kept = append(kept, input)
		}
	}
	kept = append(kept, r.RequiredInputs...)
	if len(kept) > 1000 {
		return fmt.Errorf("network diagnostics exceed requiredInputs limit of 1000")
	}
	next.RequiredInputs = kept
	if condition == metav1.ConditionTrue && len(kept) > 0 {
		next.Phase = api.PhaseNeedsInput
	}
	*target = *next
	return nil
}
