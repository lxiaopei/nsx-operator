/*
Copyright © 2026 Broadcom, Inc. All Rights Reserved.

	SPDX-License-Identifier: Apache-2.0
*/
package migration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/vmware/vsphere-automation-sdk-go/services/nsxt/infra"
	"github.com/vmware/vsphere-automation-sdk-go/services/nsxt/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	api "github.com/vmware-tanzu/nsx-operator/pkg/apis/migration/v1alpha1"
	"github.com/vmware-tanzu/nsx-operator/pkg/config"
	"github.com/vmware-tanzu/nsx-operator/pkg/migration/fabric"
	"github.com/vmware-tanzu/nsx-operator/pkg/migration/recovery"
	"github.com/vmware-tanzu/nsx-operator/pkg/nsx"
)

// TestFabricNSXIntegration is opt-in and mutates only uniquely named test resources.
// NSX calls are real; Kubernetes objects/journals use a local fake client. It does
// not validate API-server persistence, Net Operator, SNAT, or migration traffic.
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
	}{Manager: manager.IP, Project: project, RunID: runID, Kubernetes: "local fake client; no Supervisor mutation"}
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

	specs := []fabric.Spec{{Kind: fabric.VPC, ID: runID + "-vpc", ProjectID: project, VPC: &model.Vpc{IpAddressType: ptr.To("IPV4")}}}
	// TGW uses only the pre-existing span, not its transit ranges or attachments.
	// No namespace/workload is attached to the disposable TGW or VPC.
	existing, err := c.TransitGatewayClient.Get("default", project, "default")
	require.NoError(t, err)
	specs = append(specs, fabric.Spec{Kind: fabric.TransitGateway, ID: runID + "-tgw", ProjectID: project, TransitGateway: &model.TransitGateway{Span: existing.Span, ExternalIpSignalingMode: ptr.To("NONE")}})
	for _, spec := range specs {
		spec.Consumers = []fabric.Consumer{{Kind: "ControlPlaneNetworkTransition", Name: api.ControlPlaneNetworkTransitionName, UID: owner}}
		t.Run(string(spec.Kind), func(t *testing.T) {
			result := liveFabricCase{Kind: string(spec.Kind), Path: spec.Path()}
			report.Cases = append(report.Cases, result)
			out := &report.Cases[len(report.Cases)-1]
			writeReport() // persist the cleanup target before any remote mutation
			ctx := context.Background()
			// Require absence before registering cleanup: never adopt or delete pre-existing resources.
			_, err := readFabric(c, spec)
			require.True(t, isMissing(err), "test resource must be absent: %v", err)
			t.Cleanup(func() { out.Cleaned = cleanupLiveFabric(t, c, spec, owner, out.UniqueID); writeReport() })
			store, session, obj := liveSession(t, owner, nil)
			service := &fabric.Service{Backend: backend}
			beforeWrites := backend.writes
			record, err := waitLiveFabric(ctx, service, session, spec, 90*time.Second)
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
			record2, err := service.Ensure(ctx, session, spec)
			require.NoError(t, err)
			require.Equal(t, record.UniqueID, record2.UniqueID)
			require.Equal(t, writes, backend.writes)
			out.Reused = true
			// Rebuild the local client from a JSON journal snapshot, then reacquire ownership.
			var journals corev1.ConfigMapList
			require.NoError(t, store.Client.List(ctx, &journals))
			snapshot, err := json.Marshal(journals.Items)
			require.NoError(t, err)
			var restored []corev1.ConfigMap
			require.NoError(t, json.Unmarshal(snapshot, &restored))
			_, restarted, _ := liveSession(t, owner, restored)
			record2, err = service.Ensure(ctx, restarted, spec)
			require.NoError(t, err)
			require.Equal(t, record.UniqueID, record2.UniqueID)
			require.Equal(t, writes, backend.writes)
			out.RestartReused = true
			// A separate transition must explicitly adopt; adoption does not patch the object.
			_, adopter, _ := liveSession(t, types.UID(runID+"-adopter"), nil)
			_, err = service.Ensure(ctx, adopter, spec)
			require.ErrorContains(t, err, "explicit adoption")
			adoption := spec
			adoption.AdoptUniqueID = record.UniqueID
			adopted, err := service.Ensure(ctx, adopter, adoption)
			require.NoError(t, err)
			require.Equal(t, "Adopted", adopted.Provenance)
			require.Equal(t, writes, backend.writes)
			out.Adopted = true
			// Wrong ownership cannot modify even a deliberately incompatible desired payload.
			conflict := spec
			if conflict.Kind == fabric.VPC {
				conflict.VPC = &model.Vpc{IpAddressType: ptr.To("IPV4_IPV6")}
			} else {
				conflict.TransitGateway = &model.TransitGateway{Span: existing.Span, ExternalIpSignalingMode: ptr.To("ROUTING")}
			}
			err = backend.FabricBackend.Apply(ctx, conflict, types.UID("foreign-owner"))
			require.ErrorContains(t, err, "another controller")
			out.ForeignMutationRejected = true
			// Deletion/recreation checks are covered by unit tests; live cleanup does not
			// deliberately reuse resource IDs that may still have realization tombstones.
			require.Equal(t, api.ActionEvaluate, obj.Spec.Action)
		})
	}
}

type liveFabricCase struct {
	Kind, Path, UniqueID, Error                                               string
	Created, Reused, RestartReused, Adopted, ForeignMutationRejected, Cleaned bool
}
type countingFabric struct {
	*FabricBackend
	writes int
}

func (b *countingFabric) Apply(ctx context.Context, s fabric.Spec, owner types.UID) error {
	b.writes++
	return b.FabricBackend.Apply(ctx, s, owner)
}

func liveSession(t *testing.T, owner types.UID, records []corev1.ConfigMap) (*recovery.Store, *recovery.Session, *api.ControlPlaneNetworkTransition) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, api.AddToScheme(scheme))
	obj := &api.ControlPlaneNetworkTransition{ObjectMeta: metav1.ObjectMeta{Name: api.ControlPlaneNetworkTransitionName, UID: owner, Generation: 1}, Spec: api.ControlPlaneNetworkTransitionSpec{TransitionSpec: api.TransitionSpec{Action: api.ActionEvaluate}}}
	objects := []client.Object{obj}
	for i := range records {
		objects = append(objects, records[i].DeepCopy())
	}
	k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	store := &recovery.Store{Client: k, Reader: k}
	session, err := store.Acquire(context.Background(), recovery.Identity{UID: owner, Name: obj.Name, Kind: "ControlPlaneNetworkTransition", Action: "Evaluate", Generation: 1}, recovery.Scope{AllNamespaces: true})
	require.NoError(t, err)
	return store, session, obj
}

func waitLiveFabric(ctx context.Context, s *fabric.Service, session *recovery.Session, spec fabric.Spec, timeout time.Duration) (fabric.Record, error) {
	deadline := time.Now().Add(timeout)
	for {
		record, err := s.Ensure(ctx, session, spec)
		if err == nil || !errors.Is(err, recovery.ErrPending) {
			return record, err
		}
		if time.Now().After(deadline) {
			return record, errors.New("timed out waiting for NSX fabric realization")
		}
		time.Sleep(2 * time.Second)
	}
}

func cleanupLiveFabric(t *testing.T, c *nsx.Client, s fabric.Spec, owner types.UID, uniqueID string) bool {
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
