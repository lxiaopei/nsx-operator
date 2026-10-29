/*
Copyright © 2026 Broadcom, Inc. All Rights Reserved.

	SPDX-License-Identifier: Apache-2.0
*/
package migration

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"

	vapierrors "github.com/vmware/vsphere-automation-sdk-go/lib/vapi/std/errors"
	"github.com/vmware/vsphere-automation-sdk-go/services/nsxt/infra"
	"github.com/vmware/vsphere-automation-sdk-go/services/nsxt/model"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/vmware-tanzu/nsx-operator/pkg/migration/fabric"
	"github.com/vmware-tanzu/nsx-operator/pkg/nsx"
)

const fabricOwnerScope = "migration.network.vmware.com/owner"

// FabricBackend uses the authenticated NSX service and typed Policy SDK. It never
// searches by display name or uses PATCH to overwrite an unknown resource.
type FabricBackend struct{ Service *Service }

func (b *FabricBackend) client(ctx context.Context) (*nsx.Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if b.Service == nil {
		return nil, errors.New("NSX fabric service is unavailable")
	}
	return b.Service.Client()
}

func readFabric(c *nsx.Client, s fabric.Spec) (any, error) {
	switch s.Kind {
	case fabric.TransitGateway:
		if c.TransitGatewayClient == nil {
			return nil, errors.New("NSX transit gateway client unavailable")
		}
		return c.TransitGatewayClient.Get("default", s.ProjectID, s.ID)
	case fabric.VPC:
		if c.VPCClient == nil {
			return nil, errors.New("NSX VPC client unavailable")
		}
		return c.VPCClient.Get("default", s.ProjectID, s.ID)
	case fabric.IPBlock:
		if c.RestConnector == nil {
			return nil, errors.New("NSX Policy connector unavailable")
		}
		return infra.NewIpBlocksClient(c.RestConnector).Get(s.ID, nil)
	case fabric.DVC:
		if c.RestConnector == nil {
			return nil, errors.New("NSX Policy connector unavailable")
		}
		return infra.NewDistributedVlanConnectionsClient(c.RestConnector).Get(s.ID)
	case fabric.ConnectivityProfile:
		if c.VPCConnectivityProfilesClient == nil {
			return nil, errors.New("NSX connectivity profile client unavailable")
		}
		return c.VPCConnectivityProfilesClient.Get("default", s.ProjectID, s.ID)
	default:
		return nil, errors.New("unsupported fabric resource")
	}
}

func isMissing(err error) bool {
	var value vapierrors.NotFound
	var pointer *vapierrors.NotFound
	return errors.As(err, &value) || errors.As(err, &pointer)
}

func desiredFabric(s fabric.Spec) any {
	switch s.Kind {
	case fabric.TransitGateway:
		return *s.TransitGateway
	case fabric.VPC:
		return *s.VPC
	case fabric.IPBlock:
		return *s.IPBlock
	case fabric.DVC:
		return *s.DVC
	case fabric.ConnectivityProfile:
		return *s.ConnectivityProfile
	default:
		return nil
	}
}

// The SDK resource structs have common typed metadata but no shared Go interface.
// Reflection is restricted to known structs and the explicit managed-field allowlist.
func fabricState(s fabric.Spec, obj any) (fabric.State, error) {
	v := reflect.ValueOf(obj)
	state := fabric.State{Exists: true, Compatible: true}
	str := func(name string) string {
		p := v.FieldByName(name).Interface().(*string)
		if p == nil {
			return ""
		}
		return *p
	}
	if str("Path") != s.Path() || str("Id") != s.ID || str("UniqueId") == "" {
		return state, errors.New("NSX returned a different or incomplete fabric identity")
	}
	state.UniqueID = str("UniqueId")
	if deleted := v.FieldByName("MarkedForDelete").Interface().(*bool); deleted != nil && *deleted {
		return state, errors.New("fabric resource is being deleted")
	}
	for _, tag := range v.FieldByName("Tags").Interface().([]model.Tag) {
		if tag.Scope != nil && *tag.Scope == fabricOwnerScope {
			if tag.Tag == nil || *tag.Tag == "" || state.Owner != "" {
				return state, errors.New("invalid fabric ownership tags")
			}
			state.Owner = types.UID(*tag.Tag)
		}
	}
	desired := reflect.ValueOf(desiredFabric(s))
	for i := 0; i < v.NumField(); i++ {
		if !fabric.ManagedField(s.Kind, v.Type().Field(i).Name) {
			continue
		}
		a, z := v.Field(i), desired.Field(i)
		// Unspecified optional fields retain NSX defaults. Explicitly empty lists are
		// explicit so a reviewed configuration can remove an obsolete attachment.
		if (z.Kind() == reflect.Pointer || z.Kind() == reflect.Slice) && z.IsNil() {
			continue
		}
		if a.Kind() == reflect.Slice && a.Len() == 0 && z.Len() == 0 {
			continue
		}
		if want, ok := z.Interface().([]string); ok {
			got := slices.Clone(a.Interface().([]string))
			want = slices.Clone(want)
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				state.Compatible = false
			}
			continue
		}
		if !reflect.DeepEqual(a.Interface(), z.Interface()) {
			state.Compatible = false
		}
	}
	return state, nil
}

func (b *FabricBackend) Inspect(ctx context.Context, s fabric.Spec) (fabric.State, error) {
	if err := s.Validate(); err != nil {
		return fabric.State{}, err
	}
	c, err := b.client(ctx)
	if err != nil {
		return fabric.State{}, err
	}
	obj, err := readFabric(c, s)
	if isMissing(err) {
		return fabric.State{}, nil
	}
	if err != nil {
		return fabric.State{}, err
	}
	state, err := fabricState(s, obj)
	if err != nil {
		return state, err
	}
	if !state.Compatible {
		return state, nil
	}
	if c.RealizedEntitiesClient == nil {
		return state, errors.New("NSX realization client unavailable")
	}
	entities, err := c.RealizedEntitiesClient.List(s.Path(), nil)
	if err != nil {
		return state, err
	}
	state.Ready = len(entities.Results) > 0
	for _, entity := range entities.Results {
		if entity.State != nil && *entity.State == model.GenericPolicyRealizedResource_STATE_ERROR {
			return state, fmt.Errorf("fabric realization failed for %s", s.Path())
		}
		if entity.State == nil || *entity.State != model.GenericPolicyRealizedResource_STATE_REALIZED {
			state.Ready = false
		}
	}
	if err := ctx.Err(); err != nil {
		return fabric.State{}, err
	}
	return state, nil
}

func (b *FabricBackend) Apply(ctx context.Context, s fabric.Spec, owner types.UID) error {
	if owner == "" {
		return errors.New("fabric owner UID is required")
	}
	if err := s.Validate(); err != nil {
		return err
	}
	c, err := b.client(ctx)
	if err != nil {
		return err
	}
	obj, err := readFabric(c, s)
	if err != nil && !isMissing(err) {
		return err
	}
	var value reflect.Value
	if err == nil {
		state, err := fabricState(s, obj)
		if err != nil {
			return err
		}
		if state.Owner != owner {
			return errors.New("refusing to modify fabric owned by another controller")
		}
		if state.Compatible {
			return nil
		}
		// Carry the GET revision into PUT; a concurrent writer yields a conflict.
		value = reflect.New(reflect.TypeOf(obj)).Elem()
		value.Set(reflect.ValueOf(obj))
	} else {
		desired := desiredFabric(s)
		value = reflect.New(reflect.TypeOf(desired)).Elem()
		value.FieldByName("Id").Set(reflect.ValueOf(ptr.To(s.ID)))
		value.FieldByName("Tags").Set(reflect.ValueOf([]model.Tag{{Scope: ptr.To(fabricOwnerScope), Tag: ptr.To(string(owner))}}))
	}
	desired := reflect.ValueOf(desiredFabric(s))
	for i := 0; i < value.NumField(); i++ {
		if fabric.ManagedField(s.Kind, value.Type().Field(i).Name) {
			if (desired.Field(i).Kind() == reflect.Pointer || desired.Field(i).Kind() == reflect.Slice) && desired.Field(i).IsNil() {
				continue
			}
			value.Field(i).Set(desired.Field(i))
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// PUT requires the revision for existing objects, including a create race. No
	// overwrite connector or retry with a stale revision is used here.
	switch s.Kind {
	case fabric.TransitGateway:
		_, err = c.TransitGatewayClient.Update("default", s.ProjectID, s.ID, value.Interface().(model.TransitGateway))
	case fabric.VPC:
		_, err = c.VPCClient.Update("default", s.ProjectID, s.ID, value.Interface().(model.Vpc))
	case fabric.IPBlock:
		_, err = infra.NewIpBlocksClient(c.RestConnector).Update(s.ID, value.Interface().(model.IpAddressBlock))
	case fabric.DVC:
		_, err = infra.NewDistributedVlanConnectionsClient(c.RestConnector).Update(s.ID, value.Interface().(model.DistributedVlanConnection))
	case fabric.ConnectivityProfile:
		_, err = c.VPCConnectivityProfilesClient.Update("default", s.ProjectID, s.ID, value.Interface().(model.VpcConnectivityProfile))
	}
	return err
}
