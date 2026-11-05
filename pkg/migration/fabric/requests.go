/* Copyright © 2026 Broadcom, Inc. All Rights Reserved.
   SPDX-License-Identifier: Apache-2.0 */

package fabric

import (
	"errors"
	"fmt"
	"net/netip"

	"github.com/vmware/vsphere-automation-sdk-go/runtime/data"
	"github.com/vmware/vsphere-automation-sdk-go/services/nsxt/model"
	"k8s.io/utils/ptr"
)

// EvaluateRequest is implemented by migration resources that can be prepared
// during Evaluate. Callers pass one of the request types below, not an NSX SDK
// model or a generic kind/payload combination. For example:
//
// request := fabric.VPCRequest{ID: "migration-vpc", ProjectID: "default"}
// record, err := service.EnsureEvaluate(ctx, transition.UID, request)
// Set ExpectedUniqueID only when adopting a previously selected live resource.
type EvaluateRequest interface {
	evaluateSpec() (BackendSpec, error)
}

// DeriveRequest is implemented by resources created from converted networks.
type DeriveRequest interface {
	deriveSpec() (BackendSpec, error)
}

// IPBlockVisibility determines where IP-block allocations can be consumed.
type IPBlockVisibility string

const (
	IPBlockPrivate  IPBlockVisibility = "PRIVATE"
	IPBlockExternal IPBlockVisibility = "EXTERNAL"
)

// resourceIdentity names the NSX resource. Display name defaults to ID.
type resourceIdentity struct {
	ID               string
	ProjectID        string
	VPCID            string
	ExpectedUniqueID string
}

// ZoneSpan describes the VDS zones on which a transit gateway is realized.
type ZoneSpan struct {
	ZoneExternalIDs      []string
	CustomVDSAttachments []string
	UseDefaultZones      bool
}

// TransitGatewayRequest describes a migration transit gateway.
type TransitGatewayRequest struct {
	ID               string
	ProjectID        string
	ExpectedUniqueID string
	Span             *ZoneSpan
}

// VPCRequest describes the settings a migration VPC should own.
type VPCRequest struct {
	ID                   string
	ProjectID            string
	ExpectedUniqueID     string
	ExternalIPv4BlockIDs []string
	PrivateIPs           []string
	EnableLoadBalancer   bool
	ServiceProfilePath   string
}

// VPCAttachmentRequest connects a VPC to its connectivity profile.
type VPCAttachmentRequest struct {
	ID                     string
	ProjectID              string
	VPCID                  string
	ExpectedUniqueID       string
	ConnectivityProfileID  string
	PreferredDefaultSNATIP string
}

// VPCSubnetRequest describes a VLAN-extension subnet.
type VPCSubnetRequest struct {
	ID               string
	ProjectID        string
	VPCID            string
	ExpectedUniqueID string
	IPAddresses      []string
	IPBlockIDs       []string
	IPv4SubnetSize   int64
	DVCID            string
}

// IPRange is an inclusive address range managed by an IP block.
type IPRange struct {
	Start string
	End   string
}

// IPBlockRequest describes the address space a migration resource needs.
type IPBlockRequest struct {
	ID               string
	ExpectedUniqueID string
	CIDRs            []string
	Ranges           []IPRange
	ExcludedIPs      []IPRange
	Visibility       IPBlockVisibility
}

// Availability describes where a DVC can be realized.
type Availability struct {
	ZoneExternalIDs []string
}

// DVCRequest describes a VLAN connection derived from a DVPG conversion.
type DVCRequest struct {
	ID                   string
	ExpectedUniqueID     string
	AssociatedIPBlockIDs []string
	GatewayAddresses     []string
	Availability         Availability
	VLANID               int64
}

// ConnectivityProfileRequest describes the profile that attaches a VPC to a TGW.
type ConnectivityProfileRequest struct {
	ID                   string
	ProjectID            string
	ExpectedUniqueID     string
	ExternalIPBlockIDs   []string
	PrivateTGWIPBlockIDs []string
	EnableServiceGateway bool
	EdgeClusterPaths     []string
	TransitGatewayID     string
}

func baseSpec(identity resourceIdentity, kind Kind) BackendSpec {
	return BackendSpec{
		Kind:             kind,
		ID:               identity.ID,
		ProjectID:        identity.ProjectID,
		VPCID:            identity.VPCID,
		ExpectedUniqueID: identity.ExpectedUniqueID,
	}
}

func (r TransitGatewayRequest) evaluateSpec() (BackendSpec, error) {
	if r.Span == nil {
		return BackendSpec{}, errors.New("transit gateway requires a resolved zone span")
	}
	span, err := r.Span.modelValue()
	if err != nil {
		return BackendSpec{}, err
	}
	spec := baseSpec(resourceIdentity{ID: r.ID, ProjectID: r.ProjectID, ExpectedUniqueID: r.ExpectedUniqueID}, TransitGateway)
	spec.TransitGateway = &model.TransitGateway{
		Span:                    span,
		ExternalIpSignalingMode: ptr.To("NONE"),
	}
	return spec, nil
}

func (r VPCRequest) evaluateSpec() (BackendSpec, error) {
	if err := validateIPv4Values("private IPs", r.PrivateIPs); err != nil {
		return BackendSpec{}, err
	}
	externalIPBlockPaths, err := ipBlockPaths(r.ExternalIPv4BlockIDs)
	if err != nil {
		return BackendSpec{}, fmt.Errorf("external IPv4 IP block IDs: %w", err)
	}
	var lbEndpoint *model.LoadBalancerVPCEndpoint
	if r.EnableLoadBalancer {
		lbEndpoint = &model.LoadBalancerVPCEndpoint{Enabled: ptr.To(true)}
	}
	spec := baseSpec(resourceIdentity{ID: r.ID, ProjectID: r.ProjectID, ExpectedUniqueID: r.ExpectedUniqueID}, VPC)
	spec.VPC = &model.Vpc{
		ExternalIpv4Blocks:      externalIPBlockPaths,
		IpAddressType:           ptr.To("IPV4"),
		PrivateIps:              r.PrivateIPs,
		LoadBalancerVpcEndpoint: lbEndpoint,
		VpcServiceProfile:       optionalString(r.ServiceProfilePath),
	}
	return spec, nil
}

func (r VPCAttachmentRequest) evaluateSpec() (BackendSpec, error) {
	if r.PreferredDefaultSNATIP != "" {
		if err := validateIPv4Address("preferred default SNAT IP", r.PreferredDefaultSNATIP); err != nil {
			return BackendSpec{}, err
		}
	}
	spec := baseSpec(resourceIdentity{ID: r.ID, ProjectID: r.ProjectID, VPCID: r.VPCID, ExpectedUniqueID: r.ExpectedUniqueID}, VPCAttachment)
	spec.VPCAttachment = &model.VpcAttachment{
		VpcConnectivityProfile: optionalString(vpcConnectivityProfilePath(r.ProjectID, r.ConnectivityProfileID)),
		PreferredDefaultSnatIp: optionalString(r.PreferredDefaultSNATIP),
	}
	return spec, nil
}

func (r VPCSubnetRequest) deriveSpec() (BackendSpec, error) {
	if err := validateIPv4Values("subnet IP addresses", r.IPAddresses); err != nil {
		return BackendSpec{}, err
	}
	ipBlockPaths, err := ipBlockPaths(r.IPBlockIDs)
	if err != nil {
		return BackendSpec{}, fmt.Errorf("IP block IDs: %w", err)
	}
	spec := baseSpec(resourceIdentity{ID: r.ID, ProjectID: r.ProjectID, VPCID: r.VPCID, ExpectedUniqueID: r.ExpectedUniqueID}, VPCSubnet)
	spec.VPCSubnet = &model.VpcSubnet{
		AccessMode:     ptr.To(model.VpcSubnet_ACCESS_MODE_PUBLIC),
		IpAddressType:  ptr.To("IPV4"),
		IpAddresses:    r.IPAddresses,
		IpBlocks:       ipBlockPaths,
		Ipv4SubnetSize: optionalPositiveInt(r.IPv4SubnetSize),
		VlanConnection: optionalString(dvcPath(r.DVCID)),
	}
	return spec, nil
}

func (r IPBlockRequest) evaluateSpec() (BackendSpec, error) {
	return r.backendSpec(IPBlock)
}

func (r IPBlockRequest) deriveSpec() (BackendSpec, error) {
	return r.backendSpec(IPBlock)
}

func (r IPBlockRequest) backendSpec(kind Kind) (BackendSpec, error) {
	if err := validateIPv4Prefixes("IP block CIDRs", r.CIDRs); err != nil {
		return BackendSpec{}, err
	}
	if err := validateIPv4Ranges(r.Ranges); err != nil {
		return BackendSpec{}, fmt.Errorf("IP block ranges: %w", err)
	}
	if err := validateIPv4Ranges(r.ExcludedIPs); err != nil {
		return BackendSpec{}, fmt.Errorf("excluded IPs: %w", err)
	}
	if r.Visibility != "" && r.Visibility != IPBlockPrivate && r.Visibility != IPBlockExternal {
		return BackendSpec{}, fmt.Errorf("unsupported IP block visibility %q", r.Visibility)
	}
	spec := baseSpec(resourceIdentity{ID: r.ID, ExpectedUniqueID: r.ExpectedUniqueID}, kind)
	spec.IPBlock = &model.IpAddressBlock{
		Cidrs:         r.CIDRs,
		Ranges:        modelRanges(r.Ranges),
		ExcludedIps:   modelRanges(r.ExcludedIPs),
		IpAddressType: ptr.To("IPV4"),
		Visibility:    optionalString(string(r.Visibility)),
	}
	return spec, nil
}

func (r DVCRequest) deriveSpec() (BackendSpec, error) {
	if err := validateIPv4Values("DVC gateway addresses", r.GatewayAddresses); err != nil {
		return BackendSpec{}, err
	}
	associatedIPBlockPaths, err := ipBlockPaths(r.AssociatedIPBlockIDs)
	if err != nil {
		return BackendSpec{}, fmt.Errorf("associated IP block IDs: %w", err)
	}
	spec := baseSpec(resourceIdentity{ID: r.ID, ExpectedUniqueID: r.ExpectedUniqueID}, DVC)
	spec.DVC = &model.DistributedVlanConnection{
		AssociatedIpBlockPaths:    associatedIPBlockPaths,
		GatewayAddresses:          r.GatewayAddresses,
		RestrictedAvailability:    &model.VlanAvailability{ZoneExternalIds: r.Availability.ZoneExternalIDs},
		SubnetExtensionConnection: ptr.To(model.DistributedVlanConnection_SUBNET_EXTENSION_CONNECTION_ENABLED_L2_AND_L3),
		VlanId:                    ptr.To(r.VLANID),
	}
	return spec, nil
}

func (r ConnectivityProfileRequest) evaluateSpec() (BackendSpec, error) {
	externalIPBlockPaths, err := ipBlockPaths(r.ExternalIPBlockIDs)
	if err != nil {
		return BackendSpec{}, fmt.Errorf("external IP block IDs: %w", err)
	}
	privateTGWIPBlockPaths, err := ipBlockPaths(r.PrivateTGWIPBlockIDs)
	if err != nil {
		return BackendSpec{}, fmt.Errorf("private TGW IP block IDs: %w", err)
	}
	var serviceGateway *model.VpcServiceGatewayConfig
	if r.EnableServiceGateway || r.EdgeClusterPaths != nil {
		serviceGateway = &model.VpcServiceGatewayConfig{Enable: ptr.To(r.EnableServiceGateway), EdgeClusterPaths: r.EdgeClusterPaths}
	}
	spec := baseSpec(resourceIdentity{ID: r.ID, ProjectID: r.ProjectID, ExpectedUniqueID: r.ExpectedUniqueID}, ConnectivityProfile)
	spec.ConnectivityProfile = &model.VpcConnectivityProfile{
		ExternalIpBlocks:   externalIPBlockPaths,
		PrivateTgwIpBlocks: privateTGWIPBlockPaths,
		ServiceGateway:     serviceGateway,
		TransitGatewayPath: optionalString(transitGatewayPath(r.ProjectID, r.TransitGatewayID)),
	}
	return spec, nil
}

func (s *ZoneSpan) modelValue() (*data.StructValue, error) {
	if s == nil {
		return nil, nil
	}
	if !s.UseDefaultZones && len(s.ZoneExternalIDs) == 0 && len(s.CustomVDSAttachments) == 0 {
		return nil, errors.New("zone span requires zone IDs or default zones")
	}
	if s.UseDefaultZones && len(s.ZoneExternalIDs) > 0 {
		return nil, errors.New("zone span cannot combine default zones with explicit zone IDs")
	}
	span := &model.ZoneBasedSpan{
		Type_:                model.BaseSpan_TYPE_ZONEBASEDSPAN,
		ZoneExternalIds:      s.ZoneExternalIDs,
		CustomVdsAttachments: s.CustomVDSAttachments,
	}
	if s.UseDefaultZones {
		span.UseDefaultZones = ptr.To(true)
	}
	value, errs := span.GetDataValue__()
	if len(errs) != 0 {
		return nil, errors.Join(errs...)
	}
	structValue, ok := value.(*data.StructValue)
	if !ok {
		return nil, errors.New("could not build transit gateway zone span")
	}
	return structValue, nil
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return ptr.To(value)
}

func optionalPositiveInt(value int64) *int64 {
	if value <= 0 {
		return nil
	}
	return ptr.To(value)
}

func validateIPv4Values(field string, values []string) error {
	for _, value := range values {
		if err := validateIPv4AddressOrPrefix(field, value); err != nil {
			return err
		}
	}
	return nil
}

func validateIPv4Prefixes(field string, values []string) error {
	for _, value := range values {
		prefix, err := netip.ParsePrefix(value)
		if err != nil || !prefix.Addr().Is4() {
			return fmt.Errorf("%s must contain IPv4 CIDRs; got %q", field, value)
		}
	}
	return nil
}

func validateIPv4Ranges(ranges []IPRange) error {
	for _, ipRange := range ranges {
		if err := validateIPv4Address("range start", ipRange.Start); err != nil {
			return err
		}
		if err := validateIPv4Address("range end", ipRange.End); err != nil {
			return err
		}
	}
	return nil
}

func validateIPv4AddressOrPrefix(field, value string) error {
	if address, err := netip.ParseAddr(value); err == nil && address.Is4() {
		return nil
	}
	if prefix, err := netip.ParsePrefix(value); err == nil && prefix.Addr().Is4() {
		return nil
	}
	return fmt.Errorf("%s must contain IPv4 addresses or CIDRs; got %q", field, value)
}

func validateIPv4Address(field, value string) error {
	address, err := netip.ParseAddr(value)
	if err != nil || !address.Is4() {
		return fmt.Errorf("%s must be an IPv4 address; got %q", field, value)
	}
	return nil
}

func ipBlockPaths(ids []string) ([]string, error) {
	return resourcePaths(ids, "/infra/ip-blocks/")
}

func resourcePaths(ids []string, prefix string) ([]string, error) {
	if ids == nil {
		return nil, nil
	}
	paths := make([]string, len(ids))
	for i, id := range ids {
		if !validID(id) {
			return nil, fmt.Errorf("invalid resource ID %q", id)
		}
		paths[i] = prefix + id
	}
	return paths, nil
}

func dvcPath(id string) string {
	if id == "" {
		return ""
	}
	return "/infra/distributed-vlan-connections/" + id
}

func vpcConnectivityProfilePath(projectID, id string) string {
	if id == "" {
		return ""
	}
	return "/orgs/default/projects/" + projectID + "/vpc-connectivity-profiles/" + id
}

func transitGatewayPath(projectID, id string) string {
	if id == "" {
		return ""
	}
	return "/orgs/default/projects/" + projectID + "/transit-gateways/" + id
}

func modelRanges(ranges []IPRange) []model.IpPoolRange {
	if ranges == nil {
		return nil
	}
	result := make([]model.IpPoolRange, len(ranges))
	for i, ipRange := range ranges {
		result[i] = model.IpPoolRange{Start: optionalString(ipRange.Start), End: optionalString(ipRange.End)}
	}
	return result
}
