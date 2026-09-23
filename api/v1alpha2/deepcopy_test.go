package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"
)

func TestCopiesPreserveSnapshotIsolation(t *testing.T) {
	original := &NetworkBinding{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"owner": "original"}}, Spec: bindingFixture(), Status: NetworkBindingStatus{Resources: []ResourceRecord{{Name: "original"}}, Gateways: []GatewayEndpoint{{ID: "original"}}, Conditions: []metav1.Condition{{Reason: "Original"}}}}
	original.Spec.AuthorizedLocations = []string{"dc-b"}
	original.Spec.LinkAllocations = []LinkAllocation{{PeerA: "dc-a/gateway-a", PeerB: "dc-b/gateway-b", ControlVNI: 12001}}
	original.Spec.RemoteSubnets = []RemoteSubnet{{LocationRef: "dc-b", CIDR: "10.242.0.0/24"}}
	original.Spec.Peers = []PeerGateway{{LocationRef: "dc-b", Endpoint: GatewayEndpoint{ID: "original"}}}
	original.Spec.Peers[0].Endpoint.Links = []GatewayLink{{PeerID: "dc-a/gateway-a", TunnelIP: "10.254.1.1"}}
	original.Status.Gateways[0].Links = []GatewayLink{{PeerID: "dc-b/gateway-b", TunnelIP: "10.254.1.2"}}
	copy := original.DeepCopy()
	copy.Labels["owner"] = "changed"
	copy.Spec.Subnets[0].CIDR = "10.242.0.0/24"
	copy.Spec.AuthorizedLocations[0] = "dc-c"
	copy.Spec.RemoteSubnets[0].CIDR = "10.243.0.0/24"
	copy.Spec.LinkAllocations[0].ControlVNI = 12002
	copy.Spec.Peers[0].Endpoint.Links[0].TunnelIP = "10.254.2.1"
	copy.Status.Gateways[0].Links[0].TunnelIP = "10.254.2.2"
	copy.Spec.Peers[0].Endpoint.ID = "changed"
	copy.Status.Resources[0].Name = "changed"
	copy.Status.Gateways[0].ID = "changed"
	copy.Status.Conditions[0].Reason = "Changed"
	if original.Labels["owner"] != "original" || original.Spec.Subnets[0].CIDR != "10.241.0.0/24" || original.Spec.AuthorizedLocations[0] != "dc-b" || original.Spec.RemoteSubnets[0].CIDR != "10.242.0.0/24" || original.Spec.Peers[0].Endpoint.ID != "original" || original.Spec.LinkAllocations[0].ControlVNI != 12001 || original.Status.Resources[0].Name != "original" || original.Status.Gateways[0].ID != "original" || original.Status.Conditions[0].Reason != "Original" {
		t.Fatal("copy changed accepted original snapshot")
	}
	if original.Spec.Peers[0].Endpoint.Links[0].TunnelIP != "10.254.1.1" || original.Status.Gateways[0].Links[0].TunnelIP != "10.254.1.2" {
		t.Fatal("nested public link allocation copy aliases original")
	}
	vpc := &VPC{Status: VPCStatus{Bindings: []BindingReference{{Name: "original"}}, SubnetClaims: []PrefixClaim{{SubnetUID: "original"}}, Conditions: []metav1.Condition{{Reason: "Original"}}}}
	vcopy := vpc.DeepCopy()
	vcopy.Status.Bindings[0].Name = "changed"
	vcopy.Status.SubnetClaims[0].SubnetUID = "changed"
	vcopy.Status.Conditions[0].Reason = "Changed"
	if vpc.Status.Bindings[0].Name != "original" || vpc.Status.SubnetClaims[0].SubnetUID != "original" || vpc.Status.Conditions[0].Reason != "Original" {
		t.Fatal("VPC receipt copy aliases original")
	}
	list := &NetworkBindingList{Items: []NetworkBinding{*original}}
	lc := list.DeepCopy()
	lc.Items[0].Labels["owner"] = "changed"
	if list.Items[0].Labels["owner"] != "original" {
		t.Fatal("list copy aliases metadata")
	}
}
