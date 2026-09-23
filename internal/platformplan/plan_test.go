package platformplan

import (
	"reflect"
	"testing"

	api "globalvpc.io/controller/api/v1alpha2"
	"globalvpc.io/controller/internal/platformconfig"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func testConfig() platformconfig.Config {
	return platformconfig.Config{NetworkClasses: map[string]platformconfig.NetworkClass{"default": {TransportProfile: "wireguard-bgp"}}, Locations: []platformconfig.Location{
		{Name: "dc-a", Region: "region-one", Site: "site-a", DC: "dc-a", BindingNamespace: "location-a", AllowedProjects: []string{"project-red"}, CIDRPools: []string{"10.240.0.0/14"}},
		{Name: "dc-b", Region: "region-one", Site: "site-b", DC: "dc-b", BindingNamespace: "location-b", AllowedProjects: []string{"project-red"}, CIDRPools: []string{"10.240.0.0/14"}},
	}}
}
func fixtureVPC() *api.VPC {
	return &api.VPC{ObjectMeta: metav1.ObjectMeta{Namespace: "project-red", Name: "red", UID: "vpc-red"}, Status: api.VPCStatus{NetworkID: 11001}}
}
func fixtureSubnet(name, uid, location, cidr string) api.Subnet {
	return api.Subnet{ObjectMeta: metav1.ObjectMeta{Namespace: "project-red", Name: name, UID: types.UID(uid)}, Spec: api.SubnetSpec{VPCRef: "red", LocationRef: location, CIDR: cidr}, Status: api.SubnetStatus{VPCUID: "vpc-red"}}
}

func TestBuildMultipleNativeSubnetsAndAbsentLocations(t *testing.T) {
	v := fixtureVPC()
	subs := []api.Subnet{fixtureSubnet("apps", "uid-apps", "dc-a", "10.241.0.0/24"), fixtureSubnet("db", "uid-db", "dc-a", "10.241.1.0/24"), fixtureSubnet("remote", "uid-remote", "dc-b", "10.242.0.0/24")}
	bindings, err := Build(v, subs, nil, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 2 {
		t.Fatalf("want one binding per concrete location, got %d", len(bindings))
	}
	a, b := bindings[0], bindings[1]
	if a.Namespace != "location-a" || b.Namespace != "location-b" || a.Spec.VPCRef.Namespace != "project-red" {
		t.Fatal("tenant and location authority namespaces conflated")
	}
	if len(a.Spec.Subnets) != 2 || len(b.Spec.Subnets) != 1 || len(a.Spec.RemoteSubnets) != 1 || len(b.Spec.RemoteSubnets) != 2 {
		t.Fatal("multiple subnet membership was not materialized")
	}
	if a.Spec.NativeVpcName == b.Spec.NativeVpcName || a.Spec.Subnets[0].NativeSubnetName == a.Spec.Subnets[1].NativeSubnetName {
		t.Fatal("native identities collide")
	}
	if len(a.Spec.Peers) != 0 || len(b.Spec.Peers) != 0 {
		t.Fatal("unregistered gateway endpoints were invented")
	}
	if a.Spec.Revision == "" || b.Spec.Revision == "" || !reflect.DeepEqual(a.Spec.AuthorizedLocations, []string{"dc-a", "dc-b"}) {
		t.Fatal("membership snapshot is incomplete")
	}
	other, err := Build(v, []api.Subnet{subs[2], subs[1], subs[0]}, nil, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(bindings, other) {
		t.Fatal("informer/list ordering changed stable snapshots")
	}
}

func TestBuildWithdrawsDeletingSubnetWithoutDeletingSibling(t *testing.T) {
	subs := []api.Subnet{fixtureSubnet("apps", "uid-apps", "dc-a", "10.241.0.0/24"), fixtureSubnet("db", "uid-db", "dc-a", "10.241.1.0/24"), fixtureSubnet("remote", "uid-remote", "dc-b", "10.242.0.0/24")}
	existing, err := Build(fixtureVPC(), subs, nil, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	now := metav1.Now()
	subs[0].DeletionTimestamp = &now
	got, err := Build(fixtureVPC(), subs, existing, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Spec.Deleting || len(got[0].Spec.Subnets) != 2 || !got[0].Spec.Subnets[0].Deleting || got[0].Spec.Subnets[1].Deleting {
		t.Fatal("subnet drain destroyed or lost sibling intent")
	}
	if len(got[1].Spec.RemoteSubnets) != 2 {
		t.Fatal("delete request withdrew an in-use or unacknowledged subnet")
	}
	got[0].Status.AppliedRevision = got[0].Spec.Revision
	got[0].Status.Resources = []api.ResourceRecord{{Kind: "Subnet", Name: got[0].Spec.Subnets[1].NativeSubnetName, UID: "native-db"}}
	withdrawn, err := Build(fixtureVPC(), subs, got, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(withdrawn[1].Spec.RemoteSubnets) != 1 || withdrawn[1].Spec.RemoteSubnets[0].CIDR != "10.241.1.0/24" {
		t.Fatal("source acknowledged removal did not withdraw only the deleted prefix")
	}
	empty, err := Build(fixtureVPC(), subs[2:], existing, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 2 || !empty[0].Spec.Deleting || len(empty[0].Spec.Subnets) != 0 {
		t.Fatal("last subnet absence did not become an explicit location tombstone")
	}
}

func TestGatewayReportsDoNotUseHealthAsMembershipExpiry(t *testing.T) {
	subs := []api.Subnet{fixtureSubnet("apps", "uid-apps", "dc-a", "10.241.0.0/24"), fixtureSubnet("remote", "uid-remote", "dc-b", "10.242.0.0/24")}
	existing, err := Build(fixtureVPC(), subs, nil, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	existing[1].Status.Gateways = []api.GatewayEndpoint{{ID: "g-b", NodeName: "node-b", NodeUID: "node-b-uid", EndpointIP: "192.0.2.2", TransitIP: "10.240.255.2", Ready: true}}
	up, err := Build(fixtureVPC(), subs, existing, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	existing[1].Status.Gateways[0].Ready = false
	down, err := Build(fixtureVPC(), subs, existing, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(down[0].Spec.Peers) != 1 || up[0].Spec.Revision != down[0].Spec.Revision {
		t.Fatal("gateway health transition deleted accepted membership or churned transport")
	}
	existing[1].Status.Gateways = append(existing[1].Status.Gateways, existing[1].Status.Gateways[0])
	if _, err = Build(fixtureVPC(), subs, existing, testConfig()); err == nil {
		t.Fatal("duplicate remote gateway registration accepted")
	}
}

func TestValidateSubnetKeepsProjectLocationAndIdentityBoundaries(t *testing.T) {
	v := fixtureVPC()
	s := fixtureSubnet("apps", "uid-apps", "dc-a", "10.241.0.0/24")
	if err := ValidateSubnet(v, &s, testConfig()); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func(*api.Subnet){func(s *api.Subnet) { s.Namespace = "foreign" }, func(s *api.Subnet) { s.Spec.LocationRef = "unknown" }, func(s *api.Subnet) { s.Spec.CIDR = "172.20.0.0/24" }, func(s *api.Subnet) { s.Spec.CIDR = "10.241.0.1/24" }, func(s *api.Subnet) { s.Status.VPCUID = "previous-vpc-uid" }} {
		copy := s.DeepCopy()
		edit(copy)
		if err := ValidateSubnet(v, copy, testConfig()); err == nil {
			t.Fatal("invalid grant, location or same-name VPC replacement accepted")
		}
	}
}
