/* Copyright © 2023 VMware, Inc. All Rights Reserved.
   SPDX-License-Identifier: Apache-2.0 */

// Code generated by client-gen. DO NOT EDIT.

package fake

import (
	v1alpha1 "github.com/vmware-tanzu/nsx-operator/pkg/client/clientset/versioned/typed/nsx.vmware.com/v1alpha1"
	rest "k8s.io/client-go/rest"
	testing "k8s.io/client-go/testing"
)

type FakeNsxV1alpha1 struct {
	*testing.Fake
}

func (c *FakeNsxV1alpha1) IPPools(namespace string) v1alpha1.IPPoolInterface {
	return &FakeIPPools{c, namespace}
}

func (c *FakeNsxV1alpha1) NSXServiceAccounts(namespace string) v1alpha1.NSXServiceAccountInterface {
	return &FakeNSXServiceAccounts{c, namespace}
}

func (c *FakeNsxV1alpha1) NetworkInfos(namespace string) v1alpha1.NetworkInfoInterface {
	return &FakeNetworkInfos{c, namespace}
}

func (c *FakeNsxV1alpha1) SecurityPolicies(namespace string) v1alpha1.SecurityPolicyInterface {
	return &FakeSecurityPolicies{c, namespace}
}

func (c *FakeNsxV1alpha1) StaticRoutes(namespace string) v1alpha1.StaticRouteInterface {
	return &FakeStaticRoutes{c, namespace}
}

func (c *FakeNsxV1alpha1) Subnets(namespace string) v1alpha1.SubnetInterface {
	return &FakeSubnets{c, namespace}
}

func (c *FakeNsxV1alpha1) SubnetPorts(namespace string) v1alpha1.SubnetPortInterface {
	return &FakeSubnetPorts{c, namespace}
}

func (c *FakeNsxV1alpha1) SubnetSets(namespace string) v1alpha1.SubnetSetInterface {
	return &FakeSubnetSets{c, namespace}
}

func (c *FakeNsxV1alpha1) VPCs(namespace string) v1alpha1.VPCInterface {
	return &FakeVPCs{c, namespace}
}

func (c *FakeNsxV1alpha1) VPCNetworkConfigurations() v1alpha1.VPCNetworkConfigurationInterface {
	return &FakeVPCNetworkConfigurations{c}
}

// RESTClient returns a RESTClient that is used to communicate
// with API server by this client implementation.
func (c *FakeNsxV1alpha1) RESTClient() rest.Interface {
	var ret *rest.RESTClient
	return ret
}
