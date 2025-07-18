package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type AddressBindingSpec struct {
	// VMName contains the VM's name
	VMName string `json:"vmName"`
	// InterfaceName contains the interface name of the VM, if not set, the first interface of the VM will be used
	InterfaceName string `json:"interfaceName,omitempty"`
	// IPAddressAllocationName contains name of the external IPAddressAllocation.
	// IP address will be allocated from an external IPBlock of the VPC when this field is not set.
	IPAddressAllocationName string `json:"ipAddressAllocationName,omitempty"`
}

type AddressBindingStatus struct {
	// Conditions describes current state of AddressBinding.
	Conditions []Condition `json:"conditions,omitempty"`
	// IP Address for port binding.
	IPAddress string `json:"ipAddress"`
}

// +genclient
//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:storageversion

// AddressBinding is used to manage 1:1 NAT for a VM/NetworkInterface.
type AddressBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AddressBindingSpec   `json:"spec"`
	Status AddressBindingStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// AddressBindingList contains a list of AddressBinding.
type AddressBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AddressBinding `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AddressBinding{}, &AddressBindingList{})
}
