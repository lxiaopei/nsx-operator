package inventory

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/assert"
	nsxt "github.com/vmware/go-vmware-nsxt"
	"github.com/vmware/go-vmware-nsxt/containerinventory"
)

func TestInventoryService_InitContainerClusterNode(t *testing.T) {
	inventoryService, _ := createService(t)
	appApiService := &nsxt.ManagementPlaneApiFabricContainerClustersApiService{}
	expectNum := 0

	// Normal flow with multiple application
	t.Run("NormalFlow", func(t *testing.T) {
		instances := containerinventory.ContainerClusterNodeListResult{
			Results: []containerinventory.ContainerClusterNode{
				{ExternalId: "1", DisplayName: "App1"},
				{ExternalId: "2", DisplayName: "App2"},
			},
			Cursor: "",
		}
		patches := gomonkey.ApplyMethod(reflect.TypeOf(appApiService), "ListContainerClusterNodes", func(_ *nsxt.ManagementPlaneApiFabricContainerClustersApiService, _ context.Context, _ *nsxt.ListContainerClusterNodesOpts) (containerinventory.ContainerClusterNodeListResult, *http.Response, error) {
			return instances, nil, nil
		})
		defer patches.Reset()
		err := inventoryService.initContainerClusterNode("cluster1")
		assert.NoError(t, err)
		itemNum := len(inventoryService.ClusterNodeStore.List())
		expectNum += 2
		assert.Equal(t, expectNum, itemNum, "expected %d item in the inventory, got %d", expectNum, itemNum)

	})

	// Error when retrieving application instances
	t.Run("ErrorRetrievingInstances", func(t *testing.T) {
		instances := containerinventory.ContainerClusterNodeListResult{
			Results: []containerinventory.ContainerClusterNode{
				{ExternalId: "1", DisplayName: "App1"},
				{ExternalId: "2", DisplayName: "App2"},
			},
			Cursor: "",
		}
		patches := gomonkey.ApplyMethod(reflect.TypeOf(appApiService), "ListContainerClusterNodes", func(_ *nsxt.ManagementPlaneApiFabricContainerClustersApiService, _ context.Context, _ *nsxt.ListContainerClusterNodesOpts) (containerinventory.ContainerClusterNodeListResult, *http.Response, error) {
			return instances, nil, errors.New("list error")
		})
		defer patches.Reset()

		err := inventoryService.initContainerClusterNode("cluster1")
		assert.Error(t, err)
		itemNum := len(inventoryService.ClusterNodeStore.List())
		assert.Equal(t, expectNum, itemNum, "expected %d item in the inventory, got %d", expectNum, itemNum)
	})

	// Application instance with empty ContainerProjectId
	t.Run("EmptyContainerProjectId", func(t *testing.T) {
		instances := containerinventory.ContainerClusterNodeListResult{
			Results: []containerinventory.ContainerClusterNode{
				{ExternalId: "1", DisplayName: "App1"},
			},
			Cursor: "",
		}
		patches := gomonkey.ApplyMethod(reflect.TypeOf(appApiService), "ListContainerClusterNodes", func(_ *nsxt.ManagementPlaneApiFabricContainerClustersApiService, _ context.Context, _ *nsxt.ListContainerClusterNodesOpts) (containerinventory.ContainerClusterNodeListResult, *http.Response, error) {
			return instances, nil, nil
		})
		defer patches.Reset()
		err := inventoryService.initContainerClusterNode("cluster1")
		assert.NoError(t, err)
		itemNum := len(inventoryService.ClusterNodeStore.List())
		assert.Equal(t, expectNum, itemNum, "expected %d item in the inventory, got %d", expectNum, itemNum)
	})

	t.Run("PaginationHandling", func(t *testing.T) {
		instancesPage3 := containerinventory.ContainerClusterNodeListResult{
			Results: []containerinventory.ContainerClusterNode{
				{ExternalId: "3", DisplayName: "App3"},
			},
			Cursor: "cursor1",
		}
		instancesPage4 := containerinventory.ContainerClusterNodeListResult{
			Results: []containerinventory.ContainerClusterNode{
				{ExternalId: "4", DisplayName: "App4"},
			},
			Cursor: "",
		}
		instances := []containerinventory.ContainerClusterNodeListResult{instancesPage3, instancesPage4}
		times := 0
		patches := gomonkey.ApplyMethod(reflect.TypeOf(appApiService), "ListContainerClusterNodes", func(_ *nsxt.ManagementPlaneApiFabricContainerClustersApiService, _ context.Context, _ *nsxt.ListContainerClusterNodesOpts) (containerinventory.ContainerClusterNodeListResult, *http.Response, error) {
			defer func() { times += 1 }()
			return instances[times], nil, nil
		})
		defer patches.Reset()
		err := inventoryService.initContainerClusterNode("cluster1")
		assert.NoError(t, err)
		itemNum := len(inventoryService.ClusterNodeStore.List())
		expectNum += 2
		assert.Equal(t, expectNum, itemNum, "expected %d item in the inventory, got %d", expectNum, itemNum)
	})

}

func TestCleanStaleInventoryClusterNode(t *testing.T) {
	inventoryService, _ := createService(t)

	t.Run("Node deleted", func(t *testing.T) {
		cluster := &containerinventory.ContainerCluster{
			DisplayName:  "known-cluster",
			ExternalId:   "123-known-cluster",
			ResourceType: "ContainerCluster",
		}
		inventoryService.ClusterStore.Add(cluster)
		inventoryService.ClusterNodeStore.Add(&containerinventory.ContainerClusterNode{
			DisplayName:        "test-node",
			ResourceType:       "ContainerClusterNode",
			ContainerClusterId: "123-known-cluster",
		})

		patches := gomonkey.ApplyPrivateMethod(reflect.TypeOf(inventoryService), "isClusterNodeDeleted", func(_ *InventoryService, _ string,
			_ string) bool {
			return true
		})
		defer patches.Reset()

		err := inventoryService.CleanStaleInventoryClusterNode()
		assert.Nil(t, err)
		count := len(inventoryService.ClusterNodeStore.List())
		assert.Equal(t, 1, count)
	})

	t.Run("Failed to delete node", func(t *testing.T) {
		deleteErr := errors.New("failed to delete")
		inventoryService.ClusterNodeStore.Add(&containerinventory.ContainerClusterNode{
			DisplayName:        "test-node",
			ResourceType:       "ContainerClusterNode",
			ContainerClusterId: "123-known-cluster",
		})
		patches := gomonkey.ApplyMethod(reflect.TypeOf(inventoryService), "DeleteResource", func(_ *InventoryService, _ string, _ InventoryType) error {
			return deleteErr
		})
		patches.ApplyPrivateMethod(reflect.TypeOf(inventoryService), "isClusterNodeDeleted", func(_ *InventoryService, _ string, _ string,
			_ string) bool {
			return true
		})
		defer patches.Reset()

		err := inventoryService.CleanStaleInventoryClusterNode()
		assert.Equal(t, deleteErr, err)
		count := len(inventoryService.ClusterNodeStore.List())
		assert.Equal(t, 1, count)
	})

}
