/*
Copyright © 2026 Broadcom, Inc. All Rights Reserved.

	SPDX-License-Identifier: Apache-2.0
*/
package migration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/vmware/vsphere-automation-sdk-go/runtime/data"
	"github.com/vmware/vsphere-automation-sdk-go/services/nsxt/infra"
	"github.com/vmware/vsphere-automation-sdk-go/services/nsxt/model"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	api "github.com/vmware-tanzu/nsx-operator/pkg/apis/migration/v1alpha1"
	"github.com/vmware-tanzu/nsx-operator/pkg/config"
	"github.com/vmware-tanzu/nsx-operator/pkg/migration/fabric"
	"github.com/vmware-tanzu/nsx-operator/pkg/nsx"
)

// TestFabricNSXIntegration is opt-in and mutates only uniquely named test resources.
// NSX calls are real. The harness does not write Kubernetes objects or validate
// Net Operator, SNAT, or migration traffic.
// Credentials are read from the Jenkins artifact file, never command-line arguments.
func TestFabricNSXIntegration(t *testing.T) {
	path := os.Getenv("NSX_FABRIC_TESTBED")
	if path == "" {
		t.Skip("set NSX_FABRIC_TESTBED and NSX_FABRIC_ALLOW_CREATE=1 for live NSX testing")
	}
	require.Equal(t, "1", os.Getenv("NSX_FABRIC_ALLOW_CREATE"), "live tests require explicit resource creation opt-in")
	contents, err := os.ReadFile(path) // #nosec G703 -- explicit local operator-supplied credential file for opt-in integration test.
	require.NoError(t, err)
	var testbed struct {
		Managers map[string]struct{ IP, Username, Password string } `json:"nsxmanager"`
	}
	require.NoError(t, json.Unmarshal(contents, &testbed))
	manager, ok := testbed.Managers["1"]
	require.True(t, ok)
	require.NotEmpty(t, manager.IP)
	require.NotEmpty(t, manager.Username)
	require.NotEmpty(t, manager.Password)
	ctrllog.SetLogger(logr.Discard())
	cfg := config.NewNSXOpertorConfig()
	cfg.NsxApiManagers = []string{manager.IP}
	cfg.NsxApiUser = manager.Username
	cfg.NsxApiPassword = manager.Password
	cfg.EnableVPCNetwork = true
	cfg.Insecure = true
	cfg.HttpTimeout = 20
	c, err := nsx.NewClient(cfg)
	require.NoError(t, err)
	backend := &countingFabric{FabricBackend: &FabricBackend{Service: &Service{client: c}}}
	project := "default"
	if selected := os.Getenv("NSX_FABRIC_PROJECT"); selected != "" {
		project = selected
	}
	_, err = c.ProjectClient.Get("default", project, nil)
	require.NoError(t, err)
	runID := "wbs143-" + uuid.NewString()[:12]
	owner := types.UID(runID)
	report := struct {
		Manager, Project, RunID, Kubernetes string
		Cases                               []liveFabricCase
	}{Manager: manager.IP, Project: project, RunID: runID, Kubernetes: "no writes; no Supervisor mutation"}
	writeReport := func() {
		reportPath := os.Getenv("NSX_FABRIC_REPORT")
		if reportPath == "" {
			return
		}
		encoded, e := json.MarshalIndent(report, "", "  ")
		if e != nil {
			t.Error(e)
			return
		}
		if e = os.WriteFile(reportPath, encoded, 0600); e != nil { // #nosec G703 -- explicit local operator-supplied report destination.
			t.Error(e)
		}
	}
	t.Cleanup(writeReport)
	writeReport()
	t.Logf("live run %s in project %s; report contains test paths and cleanup results", runID, project)

	vpcRequest := fabric.VPCRequest{ID: runID + "-vpc", ProjectID: project}
	// Reuse only the default TGW's zone span; the test TGW gets its own ID and
	// does not receive transit subnets or workload attachments.
	existing, err := c.TransitGatewayClient.Get("default", project, "default")
	require.NoError(t, err)
	zoneSpan, err := zoneSpanFromPolicyValue(existing.Span)
	require.NoError(t, err)
	tgwRequest := fabric.TransitGatewayRequest{ID: runID + "-tgw", ProjectID: project, Span: zoneSpan}
	testCases := []struct {
		name    string
		request fabric.EvaluateRequest
		spec    fabric.BackendSpec
	}{
		{name: "VPC", request: vpcRequest, spec: fabric.BackendSpec{Kind: fabric.VPC, ID: vpcRequest.ID, ProjectID: project, VPC: &model.Vpc{IpAddressType: ptr.To("IPV4")}}},
		{name: "TransitGateway", request: tgwRequest, spec: fabric.BackendSpec{Kind: fabric.TransitGateway, ID: tgwRequest.ID, ProjectID: project, TransitGateway: &model.TransitGateway{Span: existing.Span, ExternalIpSignalingMode: ptr.To("NONE")}}},
	}
	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			spec, request := testCase.spec, testCase.request
			result := liveFabricCase{Kind: string(spec.Kind), Path: spec.Path()}
			report.Cases = append(report.Cases, result)
			out := &report.Cases[len(report.Cases)-1]
			writeReport() // persist the cleanup target before any remote mutation
			ctx := context.Background()
			_, err := readFabric(c, spec)
			require.True(t, isMissing(err), "test resource must be absent: %v", err)
			t.Cleanup(func() { out.Cleaned = cleanupLiveFabric(t, c, spec, owner, out.UniqueID); writeReport() })
			obj := liveTransition(owner)
			service := &fabric.Service{Backend: backend}
			beforeWrites := backend.writes
			record, err := waitLiveFabric(ctx, service, owner, request, 90*time.Second)
			if err != nil {
				out.Error = err.Error()
				t.Fatal(err)
			}
			require.Equal(t, beforeWrites+1, backend.writes, "waiting for realization must not rewrite NSX defaults")
			out.UniqueID = record.UniqueID
			out.Created = true
			writeReport()
			t.Logf("created and realized %s unique_id=%s", record.Path, record.UniqueID)
			writes := backend.writes
			record2, err := service.EnsureEvaluate(ctx, owner, request)
			require.NoError(t, err)
			require.Equal(t, record.UniqueID, record2.UniqueID)
			require.Equal(t, writes, backend.writes)
			out.Reused = true
			restarted := &fabric.Service{Backend: backend}
			record2, err = restarted.EnsureEvaluate(ctx, owner, request)
			require.NoError(t, err)
			require.Equal(t, record.UniqueID, record2.UniqueID)
			require.Equal(t, writes, backend.writes)
			out.RestartReused = true
			adopter := types.UID(runID + "-adopter")
			_, err = service.EnsureEvaluate(ctx, adopter, request)
			require.ErrorContains(t, err, "explicit adoption")
			adoption := withExpectedUniqueID(request, record.UniqueID)
			adopted, err := service.EnsureEvaluate(ctx, adopter, adoption)
			require.NoError(t, err)
			require.Equal(t, "Adopted", adopted.Provenance)
			require.Equal(t, writes, backend.writes)
			out.Adopted = true
			conflict := spec
			if spec.Kind == fabric.VPC {
				conflict.VPC = &model.Vpc{IpAddressType: ptr.To("IPV4_IPV6")}
			} else {
				conflict.TransitGateway = &model.TransitGateway{Span: existing.Span, ExternalIpSignalingMode: ptr.To("ROUTING")}
			}
			err = backend.FabricBackend.Apply(ctx, conflict, types.UID("foreign-owner"))
			require.ErrorContains(t, err, "another controller")
			out.ForeignMutationRejected = true
			require.Equal(t, api.ActionEvaluate, obj.Spec.Action)
		})
	}
}

type liveFabricCase struct {
	Kind, Path, UniqueID, Error                                               string
	Created, Reused, RestartReused, Adopted, ForeignMutationRejected, Cleaned bool
}

func withExpectedUniqueID(request fabric.EvaluateRequest, uniqueID string) fabric.EvaluateRequest {
	switch typed := request.(type) {
	case fabric.VPCRequest:
		typed.ExpectedUniqueID = uniqueID
		return typed
	case fabric.TransitGatewayRequest:
		typed.ExpectedUniqueID = uniqueID
		return typed
	default:
		return request
	}
}

func zoneSpanFromPolicyValue(span *data.StructValue) (*fabric.ZoneSpan, error) {
	if span == nil {
		return nil, errors.New("default transit gateway has no span")
	}
	spanType, err := span.StringField("type")
	if err != nil || spanType != model.BaseSpan_TYPE_ZONEBASEDSPAN {
		return nil, errors.New("default transit gateway does not use a zone-based span")
	}
	zoneIDs, err := stringListField(span, "zone_external_ids")
	if err != nil {
		return nil, err
	}
	customVDS, err := stringListField(span, "custom_vds_attachments")
	if err != nil {
		return nil, err
	}
	zoneSpan := &fabric.ZoneSpan{ZoneExternalIDs: zoneIDs, CustomVDSAttachments: customVDS}
	if value, ok := span.Fields()["use_default_zones"].(*data.BooleanValue); ok {
		zoneSpan.UseDefaultZones = value.Value()
	}
	return zoneSpan, nil
}

func stringListField(value *data.StructValue, field string) ([]string, error) {
	list, err := value.List(field)
	if err != nil {
		if value.HasField(field) {
			return nil, err
		}
		return nil, nil
	}
	result := make([]string, 0, len(list.List()))
	for _, item := range list.List() {
		entry, ok := item.(*data.StringValue)
		if !ok {
			return nil, fmt.Errorf("transit gateway span field %q contains a non-string value", field)
		}
		result = append(result, entry.Value())
	}
	return result, nil
}

type countingFabric struct {
	*FabricBackend
	writes int
}

func (b *countingFabric) Apply(ctx context.Context, s fabric.BackendSpec, owner types.UID) error {
	b.writes++
	return b.FabricBackend.Apply(ctx, s, owner)
}

func liveTransition(owner types.UID) *api.ControlPlaneNetworkTransition {
	return &api.ControlPlaneNetworkTransition{ObjectMeta: metav1.ObjectMeta{Name: api.ControlPlaneNetworkTransitionName, UID: owner, Generation: 1}, Spec: api.ControlPlaneNetworkTransitionSpec{TransitionSpec: api.TransitionSpec{Action: api.ActionEvaluate}}}
}

func waitLiveFabric(ctx context.Context, s *fabric.Service, owner types.UID, request fabric.EvaluateRequest, timeout time.Duration) (fabric.Record, error) {
	deadline := time.Now().Add(timeout)
	for {
		record, err := s.EnsureEvaluate(ctx, owner, request)
		if err == nil || !errors.Is(err, fabric.ErrPending) {
			return record, err
		}
		if time.Now().After(deadline) {
			return record, errors.New("timed out waiting for NSX fabric realization")
		}
		time.Sleep(2 * time.Second)
	}
}

func cleanupLiveFabric(t *testing.T, c *nsx.Client, s fabric.BackendSpec, owner types.UID, uniqueID string) bool {
	t.Helper()
	obj, err := readFabric(c, s)
	if isMissing(err) {
		return true
	}
	if err != nil {
		t.Errorf("cleanup read failed for %s: %v", s.Path(), err)
		return false
	}
	state, err := fabricState(s, obj)
	if err != nil {
		t.Errorf("cleanup identity check failed for %s: %v", s.Path(), err)
		return false
	}
	if state.Owner != owner || !strings.HasPrefix(s.ID, "wbs143-") || (uniqueID != "" && uniqueID != state.UniqueID) {
		t.Errorf("refusing cleanup of changed/unowned resource %s", s.Path())
		return false
	}
	// Re-read revision/identity was checked above; no recursive deletion or deletion
	// of referenced topology is used. The harness never attaches consumers.
	switch s.Kind {
	case fabric.VPC:
		err = c.VPCClient.Delete("default", s.ProjectID, s.ID, ptr.To(false))
	case fabric.TransitGateway:
		err = c.TransitGatewayClient.Delete("default", s.ProjectID, s.ID)
	case fabric.IPBlock:
		err = infra.NewIpBlocksClient(c.RestConnector).Delete(s.ID)
	}
	if err != nil {
		t.Errorf("cleanup delete failed for %s: %v", s.Path(), err)
		return false
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		obj, err = readFabric(c, s)
		if isMissing(err) {
			t.Logf("cleanup confirmed absent: %s", s.Path())
			return true
		}
		if err != nil {
			t.Errorf("cleanup verification failed: %v", err)
			return false
		}
		deleted := reflect.ValueOf(obj).FieldByName("MarkedForDelete").Interface().(*bool)
		if deleted != nil && *deleted {
			t.Logf("cleanup deletion pending for %s", s.Path())
		}
		time.Sleep(2 * time.Second)
	}
	t.Errorf("cleanup not confirmed for %s", s.Path())
	return false
}
