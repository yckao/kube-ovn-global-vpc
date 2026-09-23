// Package v1alpha1 defines the single-authority, routed Global VPC API.
package v1alpha1

import (
	"encoding/json"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var GroupVersion = schema.GroupVersion{Group: "networking.globalvpc.io", Version: "v1alpha1"}
var SchemeBuilder = runtime.NewSchemeBuilder(func(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &GlobalVpc{}, &GlobalVpcList{}, &SiteVpc{}, &SiteVpcList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
})
var AddToScheme = SchemeBuilder.AddToScheme

const Finalizer = "networking.globalvpc.io/network-cleanup"
const OwnerLabel = "networking.globalvpc.io/vpc-uid"
const ClusterLabel = "networking.globalvpc.io/cluster-uid"

type IPAMReference struct {
	ProviderRef string `json:"providerRef"`
	ScopeRef    string `json:"scopeRef"`
}
type Attachment struct {
	ClusterRef string `json:"clusterRef"`
	CIDR       string `json:"cidr"`
	Gateway    string `json:"gateway"`
	TransitIP  string `json:"transitIP"`
}
type GlobalVpcSpec struct {
	DomainRef   string        `json:"domainRef"`
	IPAM        IPAMReference `json:"ipam"`
	TransitCIDR string        `json:"transitCIDR"`
	Attachments []Attachment  `json:"attachments"`
}
type PrefixClaim struct {
	ClaimUID string `json:"claimUID"`
	VpcUID   string `json:"vpcUID"`
	ScopeRef string `json:"scopeRef"`
	CIDR     string `json:"cidr"`
}
type Allocation struct {
	PrefixClaim   `json:",inline"`
	AllocationID  string `json:"allocationID"`
	ReleasePolicy string `json:"releasePolicy"`
}
type ResourceRecord struct {
	ClusterRef string `json:"clusterRef"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
}
type AttachmentStatus struct {
	ClusterRef        string `json:"clusterRef"`
	VpcName           string `json:"vpcName"`
	SubnetName        string `json:"subnetName"`
	TransitSwitchName string `json:"transitSwitchName"`
}
type GlobalVpcStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Phase              string             `json:"phase,omitempty"`
	Operation          string             `json:"operation,omitempty"`
	PlanHash           string             `json:"planHash,omitempty"`
	Allocations        []Allocation       `json:"allocations,omitempty"`
	Resources          []ResourceRecord   `json:"resources,omitempty"`
	TransitSwitchUID   string             `json:"transitSwitchUID,omitempty"`
	Attachments        []AttachmentStatus `json:"attachments,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}
type GlobalVpc struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              GlobalVpcSpec   `json:"spec"`
	Status            GlobalVpcStatus `json:"status,omitempty"`
}
type GlobalVpcList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GlobalVpc `json:"items"`
}

// JSON copying keeps this small alpha API independent of a code-generator binary.
func (in *GlobalVpc) DeepCopy() *GlobalVpc {
	if in == nil {
		return nil
	}
	out := new(GlobalVpc)
	b, _ := json.Marshal(in)
	_ = json.Unmarshal(b, out)
	return out
}
func (in *GlobalVpc) DeepCopyObject() runtime.Object { return in.DeepCopy() }
func (in *GlobalVpcList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(GlobalVpcList)
	b, _ := json.Marshal(in)
	_ = json.Unmarshal(b, out)
	return out
}
