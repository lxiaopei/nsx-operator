/* Copyright © 2023 VMware, Inc. All Rights Reserved.
   SPDX-License-Identifier: Apache-2.0 */

package subnetset

import (
	"context"
	"reflect"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/vmware-tanzu/nsx-operator/pkg/apis/vpc/v1alpha1"
)

// SubnetSet controller should watch event of Namespace, when there are some updates of Namespace labels,
// controller should build tags and update VpcSubnetSetSet according to new labels.

type EnqueueRequestForNamespace struct {
	Client client.Client
}

func (e *EnqueueRequestForNamespace) Create(_ context.Context, _ event.CreateEvent, _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	log.V(1).Info("Namespace create event, do nothing")
}

func (e *EnqueueRequestForNamespace) Delete(_ context.Context, _ event.DeleteEvent, _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	log.V(1).Info("Namespace delete event, do nothing")
}

func (e *EnqueueRequestForNamespace) Generic(_ context.Context, _ event.GenericEvent, _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	log.V(1).Info("Namespace generic event, do nothing")
}

func (e *EnqueueRequestForNamespace) Update(_ context.Context, updateEvent event.UpdateEvent, l workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	obj := updateEvent.ObjectNew.(*v1.Namespace)
	err := requeueSubnetSet(e.Client, obj.Name, l)
	if err != nil {
		log.Error(err, "Failed to requeue subnet")
	}
}

var PredicateFuncsNs = predicate.Funcs{
	CreateFunc: func(e event.CreateEvent) bool {
		return false
	},
	UpdateFunc: func(e event.UpdateEvent) bool {
		oldObj := e.ObjectOld.(*v1.Namespace)
		newObj := e.ObjectNew.(*v1.Namespace)
		log.V(1).Info("Receive Namespace update event", "Name", oldObj.Name)
		if reflect.DeepEqual(oldObj.ObjectMeta.Labels, newObj.ObjectMeta.Labels) {
			log.Info("Label of Namespace is not changed, ignore it", "name", oldObj.Name)
			return false
		}
		return true
	},
	DeleteFunc: func(e event.DeleteEvent) bool {
		return false
	},
}

func listSubnetSet(c client.Client, ctx context.Context, options ...client.ListOption) (*v1alpha1.SubnetSetList, error) {
	subnetSetList := &v1alpha1.SubnetSetList{}
	err := c.List(ctx, subnetSetList, options...)
	if err != nil {
		return nil, err
	}
	return subnetSetList, nil
}

func requeueSubnetSet(c client.Client, namespace string, q workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
	subnetSetList, err := listSubnetSet(c, context.Background(), client.InNamespace(namespace))
	if err != nil {
		log.Error(err, "Failed to list all the SubnetSets")
		return err
	}
	for _, subnetSet := range subnetSetList.Items {
		log.Info("Requeue SubnetSet because Namespace updated", "Namespace", subnetSet.Namespace, "Name", subnetSet.Name)
		q.Add(reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      subnetSet.Name,
				Namespace: subnetSet.Namespace,
			},
		})
	}
	return nil
}
