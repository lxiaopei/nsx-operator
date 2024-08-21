/* Copyright © 2024 VMware, Inc. All Rights Reserved.
   SPDX-License-Identifier: Apache-2.0 */

// Code generated by informer-gen. DO NOT EDIT.

package v1alpha1

import (
	"context"
	time "time"

	vpcv1alpha1 "github.com/vmware-tanzu/nsx-operator/pkg/apis/vpc/v1alpha1"
	versioned "github.com/vmware-tanzu/nsx-operator/pkg/client/clientset/versioned"
	internalinterfaces "github.com/vmware-tanzu/nsx-operator/pkg/client/informers/externalversions/internalinterfaces"
	v1alpha1 "github.com/vmware-tanzu/nsx-operator/pkg/client/listers/vpc/v1alpha1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
	watch "k8s.io/apimachinery/pkg/watch"
	cache "k8s.io/client-go/tools/cache"
)

// IPBlocksInfoInformer provides access to a shared informer and lister for
// IPBlocksInfos.
type IPBlocksInfoInformer interface {
	Informer() cache.SharedIndexInformer
	Lister() v1alpha1.IPBlocksInfoLister
}

type iPBlocksInfoInformer struct {
	factory          internalinterfaces.SharedInformerFactory
	tweakListOptions internalinterfaces.TweakListOptionsFunc
	namespace        string
}

// NewIPBlocksInfoInformer constructs a new informer for IPBlocksInfo type.
// Always prefer using an informer factory to get a shared informer instead of getting an independent
// one. This reduces memory footprint and number of connections to the server.
func NewIPBlocksInfoInformer(client versioned.Interface, namespace string, resyncPeriod time.Duration, indexers cache.Indexers) cache.SharedIndexInformer {
	return NewFilteredIPBlocksInfoInformer(client, namespace, resyncPeriod, indexers, nil)
}

// NewFilteredIPBlocksInfoInformer constructs a new informer for IPBlocksInfo type.
// Always prefer using an informer factory to get a shared informer instead of getting an independent
// one. This reduces memory footprint and number of connections to the server.
func NewFilteredIPBlocksInfoInformer(client versioned.Interface, namespace string, resyncPeriod time.Duration, indexers cache.Indexers, tweakListOptions internalinterfaces.TweakListOptionsFunc) cache.SharedIndexInformer {
	return cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options v1.ListOptions) (runtime.Object, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.CrdV1alpha1().IPBlocksInfos(namespace).List(context.TODO(), options)
			},
			WatchFunc: func(options v1.ListOptions) (watch.Interface, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.CrdV1alpha1().IPBlocksInfos(namespace).Watch(context.TODO(), options)
			},
		},
		&vpcv1alpha1.IPBlocksInfo{},
		resyncPeriod,
		indexers,
	)
}

func (f *iPBlocksInfoInformer) defaultInformer(client versioned.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
	return NewFilteredIPBlocksInfoInformer(client, f.namespace, resyncPeriod, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}, f.tweakListOptions)
}

func (f *iPBlocksInfoInformer) Informer() cache.SharedIndexInformer {
	return f.factory.InformerFor(&vpcv1alpha1.IPBlocksInfo{}, f.defaultInformer)
}

func (f *iPBlocksInfoInformer) Lister() v1alpha1.IPBlocksInfoLister {
	return v1alpha1.NewIPBlocksInfoLister(f.Informer().GetIndexer())
}