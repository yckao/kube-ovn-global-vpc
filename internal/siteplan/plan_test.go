package siteplan

import (
	"reflect"
	"testing"

	api "globalvpc.io/controller/api/v1alpha1"
	"globalvpc.io/controller/internal/siteconfig"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func fixture() (*api.SiteVpc, siteconfig.Config) {
	return &api.SiteVpc{ObjectMeta: metav1.ObjectMeta{Name: "local-red", UID: "local-red-uid"}, Spec: api.SiteVpcSpec{GlobalVpcID: "tenant-red", SiteID: "site-a", AttachmentID: "infra-a", CIDR: "10.241.1.0/24", Gateway: "10.241.1.1"}},
		siteconfig.Config{SiteID: "site-a", AttachmentID: "infra-a", Cluster: siteconfig.Cluster{Name: "infra-a", UID: "infra-a-uid", Kubeconfig: "/local/kubeconfig", NBCommand: []string{"local-nbctl"}, SBCommand: []string{"local-sbctl"}},
			GatewayCommand: []string{"local-gateway", "--config", "/local/gateway.json"}, GatewayRevision: "registration-v1", Grants: []siteconfig.Grant{{GlobalVpcID: "tenant-red", DelegatedPrefixes: []string{"10.241.0.0/16", "172.31.0.0/16"}, TransitCIDR: "10.240.255.0/29", RouterIP: "10.240.255.1", GatewayIP: "10.240.255.2"}}}
}

func TestPlanUsesOnlyLocalGatewayAndDelegatedIntent(t *testing.T) {
	v, cfg := fixture()
	p, err := Build(v, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if p.OwnerUID != string(v.UID) || p.GlobalVpcID != v.Spec.GlobalVpcID || len(p.Hash) != 64 {
		t.Fatal("missing local/global identity")
	}
	routes, _, _ := unstructured.NestedSlice(p.Vpc.Object, "spec", "staticRoutes")
	want := []interface{}{map[string]interface{}{"policy": "policyDst", "cidr": "0.0.0.0/0", "nextHopIP": cfg.Grants[0].GatewayIP}}
	if !reflect.DeepEqual(routes, want) {
		t.Fatalf("wrong local gateway route: %#v", routes)
	}
	excluded, _, _ := unstructured.NestedStringSlice(p.Transit.Object, "spec", "excludeIps")
	if !reflect.DeepEqual(excluded, []string{"10.240.255.1", "10.240.255.3..10.240.255.6"}) {
		t.Fatalf("gateway must be the only CNI-allocatable transit IP: %v", excluded)
	}
	if p.Gateway.CIDR != v.Spec.CIDR || p.Gateway.RouterIP != cfg.Grants[0].RouterIP || p.Gateway.OwnerUID != string(v.UID) || p.Gateway.GatewayRevision != cfg.GatewayRevision || p.Gateway.SubnetName != p.SubnetName {
		t.Fatal("gateway contract does not preserve local route/ownership")
	}
	for _, obj := range []*unstructured.Unstructured{p.Vpc, p.Subnet, p.Transit} {
		if obj.GetLabels()[api.OwnerLabel] != string(v.UID) || obj.GetLabels()[api.ClusterLabel] != cfg.Cluster.UID {
			t.Fatal("resource ownership lost")
		}
		if _, exists, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "namespaces"); exists {
			t.Fatal("planner must not bind tenant namespaces")
		}
	}
	for _, obj := range []*unstructured.Unstructured{p.Subnet, p.Transit} {
		for key, want := range map[string]bool{"disableInterConnection": true, "natOutgoing": false, "enableDHCP": false, "enableLb": false} {
			got, exists, err := unstructured.NestedBool(obj.Object, "spec", key)
			if err != nil || !exists || got != want {
				t.Fatalf("unsafe %s", key)
			}
		}
	}
}

func TestGlobalIdentityIndependentOfLocalKubernetesUID(t *testing.T) {
	v, cfg := fixture()
	first, err := Build(v, cfg)
	if err != nil {
		t.Fatal(err)
	}
	v.UID = types.UID("new-api-local-uid")
	second, err := Build(v, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if first.GlobalVpcID != second.GlobalVpcID || first.VpcName != second.VpcName || first.SubnetName != second.SubnetName || first.TransitName != second.TransitName {
		t.Fatal("Kubernetes lifecycle changed tenant identity")
	}
	if first.OwnerUID == second.OwnerUID || first.Hash == second.Hash || first.Vpc.GetLabels()[api.OwnerLabel] == second.Vpc.GetLabels()[api.OwnerLabel] {
		t.Fatal("new local CR must not adopt old resources implicitly")
	}
}

func TestUnrelatedTenantGrantDoesNotInvalidateAcceptedPlan(t *testing.T) {
	v, cfg := fixture()
	first, err := Build(v, cfg)
	if err != nil {
		t.Fatal(err)
	}
	other := cfg.Grants[0]
	other.GlobalVpcID = "tenant-blue"
	cfg.Grants = append([]siteconfig.Grant{other}, cfg.Grants...)
	second, err := Build(v, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("unrelated tenant registration changes existing local plan")
	}
}

func TestPlanDeterministicAndDoesNotMutateIntent(t *testing.T) {
	v, cfg := fixture()
	before := v.DeepCopy()
	prefixes := append([]string(nil), cfg.Grants[0].DelegatedPrefixes...)
	first, err := Build(v, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, v) || !reflect.DeepEqual(prefixes, cfg.Grants[0].DelegatedPrefixes) {
		t.Fatal("planner mutated input")
	}
	cfg.Grants[0].DelegatedPrefixes[0], cfg.Grants[0].DelegatedPrefixes[1] = cfg.Grants[0].DelegatedPrefixes[1], cfg.Grants[0].DelegatedPrefixes[0]
	second, err := Build(v, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("delegation ordering changed plan")
	}
}

func TestRejectsUngrantedOrRemoteIntent(t *testing.T) {
	cases := []struct {
		name string
		edit func(*api.SiteVpc, *siteconfig.Config)
	}{
		{"missing local UID", func(v *api.SiteVpc, _ *siteconfig.Config) { v.UID = "" }},
		{"other site", func(v *api.SiteVpc, _ *siteconfig.Config) { v.Spec.SiteID = "site-b" }},
		{"other attachment", func(v *api.SiteVpc, _ *siteconfig.Config) { v.Spec.AttachmentID = "infra-b" }},
		{"ungranted tenant", func(v *api.SiteVpc, _ *siteconfig.Config) { v.Spec.GlobalVpcID = "tenant-blue" }},
		{"ungranted prefix", func(v *api.SiteVpc, _ *siteconfig.Config) { v.Spec.CIDR = "10.242.0.0/24" }},
		{"broader than grant", func(v *api.SiteVpc, _ *siteconfig.Config) { v.Spec.CIDR = "10.240.0.0/15" }},
		{"noncanonical prefix", func(v *api.SiteVpc, _ *siteconfig.Config) { v.Spec.CIDR = "10.241.1.1/24" }},
		{"gateway outside subnet", func(v *api.SiteVpc, _ *siteconfig.Config) { v.Spec.Gateway = "10.241.2.1" }},
		{"gateway network", func(v *api.SiteVpc, _ *siteconfig.Config) { v.Spec.Gateway = "10.241.1.0" }},
		{"gateway broadcast", func(v *api.SiteVpc, _ *siteconfig.Config) { v.Spec.Gateway = "10.241.1.255" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, cfg := fixture()
			tc.edit(v, &cfg)
			if p, err := Build(v, cfg); err == nil || p != nil {
				t.Fatal("invalid input returned actionable plan")
			}
		})
	}
}

func TestTenantOverlapPreservesSeparateRoutingContexts(t *testing.T) {
	v, cfg := fixture()
	red, _ := Build(v, cfg)
	v.Spec.GlobalVpcID, cfg.Grants[0].GlobalVpcID = "tenant-blue", "tenant-blue"
	v.UID = "blue-local-uid"
	blue, err := Build(v, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if red.VpcName == blue.VpcName || red.TransitName == blue.TransitName || red.Gateway.GlobalVpcID == blue.Gateway.GlobalVpcID {
		t.Fatal("overlapping tenants share routing identity")
	}
}

func TestRegistrationChangesInvalidateOnlyItsPlan(t *testing.T) {
	v, cfg := fixture()
	first, _ := Build(v, cfg)
	for _, edit := range []func(*siteconfig.Config){
		func(c *siteconfig.Config) { c.GatewayRevision = "registration-v2" },
		func(c *siteconfig.Config) { c.GatewayCommand = []string{"other-gateway"} },
		func(c *siteconfig.Config) { c.Cluster.UID = "replacement-cluster" },
		func(c *siteconfig.Config) { c.Cluster.Kubeconfig = "/different/kubeconfig" },
		func(c *siteconfig.Config) { c.Grants[0].RouterIP = "10.240.255.3" },
		func(c *siteconfig.Config) { c.Grants[0].DelegatedPrefixes = []string{"10.241.0.0/17"} },
	} {
		_, changed := fixture()
		edit(&changed)
		p, err := Build(v, changed)
		if err != nil {
			t.Fatal(err)
		}
		if p.Hash == first.Hash {
			t.Fatal("local ownership or routing registration drift must invalidate plan")
		}
	}
}

func TestSlash30TransitLeavesGatewayAllocatable(t *testing.T) {
	v, cfg := fixture()
	cfg.Grants[0].TransitCIDR = "10.240.255.0/30"
	p, err := Build(v, cfg)
	if err != nil {
		t.Fatal(err)
	}
	excluded, _, _ := unstructured.NestedStringSlice(p.Transit.Object, "spec", "excludeIps")
	if !reflect.DeepEqual(excluded, []string{"10.240.255.1"}) {
		t.Fatalf("unexpected /30 exclusions: %v", excluded)
	}
}
