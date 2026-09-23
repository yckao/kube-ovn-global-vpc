package v1alpha1

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const SiteFinalizer = "networking.globalvpc.io/site-network-cleanup"
const GlobalIDLabel = "networking.globalvpc.io/global-id-hash"
const AttachmentLabel = "networking.globalvpc.io/attachment-id"

// SiteVpc contains only locally owned intent. GlobalVpcID is an administrator
// assigned identity shared across sites; the Kubernetes UID fences this local
// object's lifecycle and is never used as a cross-site tenant identity.
type SiteVpcSpec struct {
	GlobalVpcID  string `json:"globalVpcID"`
	SiteID       string `json:"siteID"`
	AttachmentID string `json:"attachmentID"`
	CIDR         string `json:"cidr"`
	Gateway      string `json:"gateway"`
}

type SiteVpcStatus struct {
	ObservedGeneration  int64                   `json:"observedGeneration,omitempty"`
	Phase               string                  `json:"phase,omitempty"`
	Operation           string                  `json:"operation,omitempty"`
	PlanHash            string                  `json:"planHash,omitempty"`
	Resources           []ResourceRecord        `json:"resources,omitempty"`
	GatewayResources    []GatewayResourceRecord `json:"gatewayResources,omitempty"`
	BFDResources        []BFDResourceRecord     `json:"bfdResources,omitempty"`
	GatewayReadyCount   int                     `json:"gatewayReadyCount,omitempty"`
	GatewayDesiredCount int                     `json:"gatewayDesiredCount,omitempty"`
	Conditions          []metav1.Condition      `json:"conditions,omitempty"`
}

// BFDResourceRecord fences a controller-owned NB BFD row independently of the
// native Kube-OVN logical router port and static routes that reference it.
type BFDResourceRecord struct {
	GatewayID string `json:"gatewayID"`
	UUID      string `json:"uuid"`
}

// GatewayResourceRecord fences plugin-owned resources by their observed UID.
type GatewayResourceRecord struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
}

type SiteVpc struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SiteVpcSpec   `json:"spec"`
	Status            SiteVpcStatus `json:"status,omitempty"`
}

type SiteVpcList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SiteVpc `json:"items"`
}

func (in *SiteVpc) DeepCopy() *SiteVpc {
	if in == nil {
		return nil
	}
	out := new(SiteVpc)
	b, _ := json.Marshal(in)
	_ = json.Unmarshal(b, out)
	return out
}
func (in *SiteVpc) DeepCopyObject() runtime.Object { return in.DeepCopy() }
func (in *SiteVpcList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(SiteVpcList)
	b, _ := json.Marshal(in)
	_ = json.Unmarshal(b, out)
	return out
}
