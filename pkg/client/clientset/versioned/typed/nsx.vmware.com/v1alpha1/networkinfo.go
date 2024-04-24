/* Copyright © 2023 VMware, Inc. All Rights Reserved.
   SPDX-License-Identifier: Apache-2.0 */

// Code generated by client-gen. DO NOT EDIT.

package v1alpha1

import (
	"context"
	"time"

	v1alpha1 "github.com/vmware-tanzu/nsx-operator/pkg/apis/nsx.vmware.com/v1alpha1"
	scheme "github.com/vmware-tanzu/nsx-operator/pkg/client/clientset/versioned/scheme"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	types "k8s.io/apimachinery/pkg/types"
	watch "k8s.io/apimachinery/pkg/watch"
	rest "k8s.io/client-go/rest"
)

// NetworkInfosGetter has a method to return a NetworkInfoInterface.
// A group's client should implement this interface.
type NetworkInfosGetter interface {
	NetworkInfos(namespace string) NetworkInfoInterface
}

// NetworkInfoInterface has methods to work with NetworkInfo resources.
type NetworkInfoInterface interface {
	Create(ctx context.Context, networkInfo *v1alpha1.NetworkInfo, opts v1.CreateOptions) (*v1alpha1.NetworkInfo, error)
	Update(ctx context.Context, networkInfo *v1alpha1.NetworkInfo, opts v1.UpdateOptions) (*v1alpha1.NetworkInfo, error)
	Delete(ctx context.Context, name string, opts v1.DeleteOptions) error
	DeleteCollection(ctx context.Context, opts v1.DeleteOptions, listOpts v1.ListOptions) error
	Get(ctx context.Context, name string, opts v1.GetOptions) (*v1alpha1.NetworkInfo, error)
	List(ctx context.Context, opts v1.ListOptions) (*v1alpha1.NetworkInfoList, error)
	Watch(ctx context.Context, opts v1.ListOptions) (watch.Interface, error)
	Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts v1.PatchOptions, subresources ...string) (result *v1alpha1.NetworkInfo, err error)
	NetworkInfoExpansion
}

// networkInfos implements NetworkInfoInterface
type networkInfos struct {
	client rest.Interface
	ns     string
}

// newNetworkInfos returns a NetworkInfos
func newNetworkInfos(c *NsxV1alpha1Client, namespace string) *networkInfos {
	return &networkInfos{
		client: c.RESTClient(),
		ns:     namespace,
	}
}

// Get takes name of the networkInfo, and returns the corresponding networkInfo object, and an error if there is any.
func (c *networkInfos) Get(ctx context.Context, name string, options v1.GetOptions) (result *v1alpha1.NetworkInfo, err error) {
	result = &v1alpha1.NetworkInfo{}
	err = c.client.Get().
		Namespace(c.ns).
		Resource("networkinfos").
		Name(name).
		VersionedParams(&options, scheme.ParameterCodec).
		Do(ctx).
		Into(result)
	return
}

// List takes label and field selectors, and returns the list of NetworkInfos that match those selectors.
func (c *networkInfos) List(ctx context.Context, opts v1.ListOptions) (result *v1alpha1.NetworkInfoList, err error) {
	var timeout time.Duration
	if opts.TimeoutSeconds != nil {
		timeout = time.Duration(*opts.TimeoutSeconds) * time.Second
	}
	result = &v1alpha1.NetworkInfoList{}
	err = c.client.Get().
		Namespace(c.ns).
		Resource("networkinfos").
		VersionedParams(&opts, scheme.ParameterCodec).
		Timeout(timeout).
		Do(ctx).
		Into(result)
	return
}

// Watch returns a watch.Interface that watches the requested networkInfos.
func (c *networkInfos) Watch(ctx context.Context, opts v1.ListOptions) (watch.Interface, error) {
	var timeout time.Duration
	if opts.TimeoutSeconds != nil {
		timeout = time.Duration(*opts.TimeoutSeconds) * time.Second
	}
	opts.Watch = true
	return c.client.Get().
		Namespace(c.ns).
		Resource("networkinfos").
		VersionedParams(&opts, scheme.ParameterCodec).
		Timeout(timeout).
		Watch(ctx)
}

// Create takes the representation of a networkInfo and creates it.  Returns the server's representation of the networkInfo, and an error, if there is any.
func (c *networkInfos) Create(ctx context.Context, networkInfo *v1alpha1.NetworkInfo, opts v1.CreateOptions) (result *v1alpha1.NetworkInfo, err error) {
	result = &v1alpha1.NetworkInfo{}
	err = c.client.Post().
		Namespace(c.ns).
		Resource("networkinfos").
		VersionedParams(&opts, scheme.ParameterCodec).
		Body(networkInfo).
		Do(ctx).
		Into(result)
	return
}

// Update takes the representation of a networkInfo and updates it. Returns the server's representation of the networkInfo, and an error, if there is any.
func (c *networkInfos) Update(ctx context.Context, networkInfo *v1alpha1.NetworkInfo, opts v1.UpdateOptions) (result *v1alpha1.NetworkInfo, err error) {
	result = &v1alpha1.NetworkInfo{}
	err = c.client.Put().
		Namespace(c.ns).
		Resource("networkinfos").
		Name(networkInfo.Name).
		VersionedParams(&opts, scheme.ParameterCodec).
		Body(networkInfo).
		Do(ctx).
		Into(result)
	return
}

// Delete takes name of the networkInfo and deletes it. Returns an error if one occurs.
func (c *networkInfos) Delete(ctx context.Context, name string, opts v1.DeleteOptions) error {
	return c.client.Delete().
		Namespace(c.ns).
		Resource("networkinfos").
		Name(name).
		Body(&opts).
		Do(ctx).
		Error()
}

// DeleteCollection deletes a collection of objects.
func (c *networkInfos) DeleteCollection(ctx context.Context, opts v1.DeleteOptions, listOpts v1.ListOptions) error {
	var timeout time.Duration
	if listOpts.TimeoutSeconds != nil {
		timeout = time.Duration(*listOpts.TimeoutSeconds) * time.Second
	}
	return c.client.Delete().
		Namespace(c.ns).
		Resource("networkinfos").
		VersionedParams(&listOpts, scheme.ParameterCodec).
		Timeout(timeout).
		Body(&opts).
		Do(ctx).
		Error()
}

// Patch applies the patch and returns the patched networkInfo.
func (c *networkInfos) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts v1.PatchOptions, subresources ...string) (result *v1alpha1.NetworkInfo, err error) {
	result = &v1alpha1.NetworkInfo{}
	err = c.client.Patch(pt).
		Namespace(c.ns).
		Resource("networkinfos").
		Name(name).
		SubResource(subresources...).
		VersionedParams(&opts, scheme.ParameterCodec).
		Body(data).
		Do(ctx).
		Into(result)
	return
}
