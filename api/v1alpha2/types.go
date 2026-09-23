// Package v1alpha2 defines the managed platform networking API. Public objects
// are project-namespaced; NetworkBinding is an internal, location-scoped snapshot.
package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var GroupVersion = schema.GroupVersion{Group: "platform.globalvpc.io", Version: "v1alpha2"}
var SchemeBuilder = runtime.NewSchemeBuilder(func(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &VPC{}, &VPCList{}, &Subnet{}, &SubnetList{}, &NetworkBinding{}, &NetworkBindingList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
})
var AddToScheme = SchemeBuilder.AddToScheme

const (
	Finalizer           = "platform.globalvpc.io/network-cleanup"
	OwnerLabel          = "platform.globalvpc.io/owner-uid"
	VPCUIDLabel         = "platform.globalvpc.io/vpc-uid"
	LocationLabel       = "platform.globalvpc.io/location"
	DefaultNetworkClass = "default"
)

// VPC is a global logical network managed by the platform. It does not imply a
// stretched subnet, a shared cross-site OVN database, or adoption of native Vpcs.
type VPC struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              VPCSpec   `json:"spec"`
	Status            VPCStatus `json:"status,omitempty"`
}

type VPCSpec struct {
	NetworkClassRef string `json:"networkClassRef,omitempty"`
}

type VPCStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	NetworkID          uint32             `json:"networkID,omitempty"`
	Phase              string             `json:"phase,omitempty"`
	Bindings           []BindingReference `json:"bindings,omitempty"`
	SubnetClaims       []PrefixClaim      `json:"subnetClaims,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// PrefixClaim is a serialized per-VPC admission receipt. A terminating Subnet
// retains its claim until local and remote withdrawal acknowledgements complete.
type PrefixClaim struct {
	SubnetUID   string `json:"subnetUID"`
	CIDR        string `json:"cidr"`
	LocationRef string `json:"locationRef"`
}

type BindingReference struct {
	LocationRef   string `json:"locationRef"`
	Name          string `json:"name"`
	UID           string `json:"uid"`
	NativeVpcName string `json:"nativeVpcName,omitempty"`
}

type VPCList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VPC `json:"items"`
}

// Subnet has its own lifecycle within a VPC. LocationRef selects an actual
// Infra/DC allocation domain; it must not be interpreted as CIDR stretching.
type Subnet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SubnetSpec   `json:"spec"`
	Status            SubnetStatus `json:"status,omitempty"`
}

type SubnetSpec struct {
	VPCRef      string `json:"vpcRef"`
	LocationRef string `json:"locationRef"`
	CIDR        string `json:"cidr"`
}

type SubnetStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Phase              string             `json:"phase,omitempty"`
	VPCUID             string             `json:"vpcUID,omitempty"`
	BindingName        string             `json:"bindingName,omitempty"`
	NativeVpcName      string             `json:"nativeVpcName,omitempty"`
	NativeSubnetName   string             `json:"nativeSubnetName,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

type SubnetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Subnet `json:"items"`
}

// NetworkBinding is written by the platform authority in an administrator-owned
// location namespace, then materialized locally. Its status is the local report.
// A missing snapshot is not a delete instruction: Deleting is an explicit tombstone.
type NetworkBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              NetworkBindingSpec   `json:"spec"`
	Status            NetworkBindingStatus `json:"status,omitempty"`
}

type ObjectIdentity struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

type NetworkBindingSpec struct {
	VPCRef              ObjectIdentity   `json:"vpcRef"`
	NetworkID           uint32           `json:"networkID"`
	LocalASN            uint32           `json:"localASN"`
	LocationRef         string           `json:"locationRef"`
	NetworkClassRef     string           `json:"networkClassRef"`
	NativeVpcName       string           `json:"nativeVpcName"`
	TransportProfile    string           `json:"transportProfile"`
	Subnets             []BindingSubnet  `json:"subnets,omitempty"`
	AuthorizedLocations []string         `json:"authorizedLocations,omitempty"`
	RemoteSubnets       []RemoteSubnet   `json:"remoteSubnets,omitempty"`
	Peers               []PeerGateway    `json:"peers,omitempty"`
	LinkAllocations     []LinkAllocation `json:"linkAllocations,omitempty"`
	Revision            string           `json:"revision"`
	Deleting            bool             `json:"deleting,omitempty"`
}

type BindingSubnet struct {
	Name             string `json:"name"`
	UID              string `json:"uid"`
	CIDR             string `json:"cidr"`
	Gateway          string `json:"gateway"`
	NativeSubnetName string `json:"nativeSubnetName"`
	Deleting         bool   `json:"deleting,omitempty"`
}

type RemoteSubnet struct {
	LocationRef string `json:"locationRef"`
	CIDR        string `json:"cidr"`
}

// GatewayEndpoint contains public membership only. A Ready report is an
// observation, not authority to join a VPC or export arbitrary prefixes.
type GatewayEndpoint struct {
	ID         string        `json:"id"`
	NodeName   string        `json:"nodeName"`
	NodeUID    string        `json:"nodeUID"`
	EndpointIP string        `json:"endpointIP,omitempty"`
	TransitIP  string        `json:"transitIP,omitempty"`
	PublicKey  string        `json:"publicKey,omitempty"`
	LocalASN   uint32        `json:"localASN,omitempty"`
	HealthIP   string        `json:"healthIP,omitempty"`
	Links      []GatewayLink `json:"links,omitempty"`
	Ready      bool          `json:"ready"`
}

// GatewayLink is a public per-peer allocation receipt. PeerID uses the stable
// location/gateway identity; private key material never belongs in a snapshot.
type GatewayLink struct {
	PeerID     string `json:"peerID"`
	ListenPort int32  `json:"listenPort"`
	TunnelIP   string `json:"tunnelIP"`
	ControlVNI uint32 `json:"controlVNI,omitempty"`
}

// LinkAllocation is an authority-issued native control VNI for a canonical pair
// of location/gateway identities. Only links touching the location are delivered.
type LinkAllocation struct {
	PeerA      string `json:"peerA"`
	PeerB      string `json:"peerB"`
	ControlVNI uint32 `json:"controlVNI"`
}

type PeerGateway struct {
	LocationRef string          `json:"locationRef"`
	Endpoint    GatewayEndpoint `json:"endpoint"`
}

type NetworkBindingStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Phase              string             `json:"phase,omitempty"`
	AppliedRevision    string             `json:"appliedRevision,omitempty"`
	Resources          []ResourceRecord   `json:"resources,omitempty"`
	Gateways           []GatewayEndpoint  `json:"gateways,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

type ResourceRecord struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
}

type NetworkBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NetworkBinding `json:"items"`
}
