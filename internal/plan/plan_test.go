package plan

import (
	"reflect"
	"strings"
	"testing"

	"globalvpc.io/controller/api/v1alpha1"
	"globalvpc.io/controller/internal/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func fixture() (*v1alpha1.GlobalVpc, config.Config) {
	return &v1alpha1.GlobalVpc{ObjectMeta: metav1.ObjectMeta{Name: "red", UID: types.UID("tenant-red")}, Spec: v1alpha1.GlobalVpcSpec{
			DomainRef: "lab", IPAM: v1alpha1.IPAMReference{ProviderRef: "registry", ScopeRef: "vrf-red"}, TransitCIDR: "10.240.255.0/29",
			Attachments: []v1alpha1.Attachment{
				{ClusterRef: "dc-a", CIDR: "10.241.0.0/24", Gateway: "10.241.0.1", TransitIP: "10.240.255.1"},
				{ClusterRef: "dc-b", CIDR: "10.242.0.0/24", Gateway: "10.242.0.1", TransitIP: "10.240.255.2"},
			},
		}}, config.Config{Domain: "lab", ICNBCommand: []string{"ic-nbctl", "--db=unix:/run/ic.sock"}, Providers: map[string][]string{"registry": {"provider", "--config=registry.json"}}, Clusters: []config.Cluster{
			{Name: "dc-a", UID: "cluster-a", Region: "region-1", Site: "site-a", DC: "dc-a", Kubeconfig: "a.conf", NBCommand: []string{"a-nbctl"}, SBCommand: []string{"a-sbctl"}, AZName: "az-a", GatewayChassis: []string{"a-gateway-2", "a-gateway-1"}, ICDeployment: config.DeploymentReference{Namespace: "ic", Name: "ovn-ic", UID: "ic-a"}},
			{Name: "dc-b", UID: "cluster-b", Region: "region-1", Site: "site-b", DC: "dc-b", Kubeconfig: "b.conf", NBCommand: []string{"b-nbctl"}, SBCommand: []string{"b-sbctl"}, AZName: "az-b", GatewayChassis: []string{"b-gateway"}, ICDeployment: config.DeploymentReference{Namespace: "ic", Name: "ovn-ic", UID: "ic-b"}},
		}}
}

func TestBuildRendersCompleteRoutesAndProtectedTransit(t *testing.T) {
	vpc, cfg := fixture()
	p, err := Build(vpc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Claims) != 3 || len(p.Attachments) != 2 || p.UID != string(vpc.UID) || len(p.Hash) != 64 {
		t.Fatalf("incomplete plan: %+v", p)
	}
	for i, site := range p.Attachments {
		remote := p.Attachments[1-i]
		if site.VpcName == remote.VpcName || site.SubnetName == remote.SubnetName || site.TransitName != p.TransitSwitch || remote.TransitName != site.TransitName {
			t.Fatal("local names must be unique and the tenant transit name must be shared")
		}
		routes, _, _ := unstructured.NestedSlice(site.Vpc.Object, "spec", "staticRoutes")
		wantRoutes := []interface{}{map[string]interface{}{"policy": "policyDst", "cidr": remote.Intent.CIDR, "nextHopIP": remote.Intent.TransitIP}}
		if !reflect.DeepEqual(routes, wantRoutes) {
			t.Fatalf("routes = %#v, want %#v", routes, wantRoutes)
		}
		for _, obj := range []*unstructured.Unstructured{site.Vpc, site.Subnet, site.Transit} {
			if _, found, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "namespaces"); found {
				t.Fatal("planner must not bind namespaces")
			}
			if obj.GetLabels()[v1alpha1.OwnerLabel] != p.UID || obj.GetLabels()[v1alpha1.ClusterLabel] != site.Cluster.UID {
				t.Fatal("resource lost ownership labels")
			}
		}
		for _, obj := range []*unstructured.Unstructured{site.Subnet, site.Transit} {
			for field, expected := range map[string]bool{"natOutgoing": false, "enableDHCP": false, "enableLb": false, "disableGatewayCheck": true, "disableInterConnection": true} {
				actual, found, err := unstructured.NestedBool(obj.Object, "spec", field)
				if err != nil || !found || actual != expected {
					t.Fatalf("unsafe Subnet field %s: %v, found=%v, err=%v", field, actual, found, err)
				}
			}
		}
		excluded, _, _ := unstructured.NestedStringSlice(site.Transit.Object, "spec", "excludeIps")
		if !reflect.DeepEqual(excluded, []string{"10.240.255.1..10.240.255.6"}) {
			t.Fatalf("transit must exclude all usable addresses: %v", excluded)
		}
	}
}

func TestBuildDeterministicAndDoesNotMutateInputs(t *testing.T) {
	vpc, cfg := fixture()
	before := vpc.DeepCopy()
	chassisBefore := append([]string(nil), cfg.Clusters[0].GatewayChassis...)
	first, err := Build(vpc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(vpc, before) || !reflect.DeepEqual(cfg.Clusters[0].GatewayChassis, chassisBefore) {
		t.Fatal("Build mutated its input")
	}
	vpc.Spec.Attachments[0], vpc.Spec.Attachments[1] = vpc.Spec.Attachments[1], vpc.Spec.Attachments[0]
	cfg.Clusters[0].GatewayChassis[0], cfg.Clusters[0].GatewayChassis[1] = cfg.Clusters[0].GatewayChassis[1], cfg.Clusters[0].GatewayChassis[0]
	cfg.Clusters[0], cfg.Clusters[1] = cfg.Clusters[1], cfg.Clusters[0]
	second, err := Build(vpc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("shuffling set inputs changed the plan")
	}
}

func TestTenantOverlapIsIsolated(t *testing.T) {
	vpc, cfg := fixture()
	red, err := Build(vpc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	vpc.Name, vpc.UID, vpc.Spec.IPAM.ScopeRef = "blue", types.UID("tenant-blue"), "vrf-blue"
	blue, err := Build(vpc, cfg)
	if err != nil {
		t.Fatalf("same prefixes in separate tenant must be supported: %v", err)
	}
	if red.TransitSwitch == blue.TransitSwitch || red.Attachments[0].VpcName == blue.Attachments[0].VpcName || red.Claims[0].ClaimUID == blue.Claims[0].ClaimUID {
		t.Fatal("overlapping tenants must have independent resources and claim identities")
	}
}

func TestBuildRejectsInvalidIntentBeforeReturningPlan(t *testing.T) {
	cases := []struct {
		name string
		edit func(*v1alpha1.GlobalVpc, *config.Config)
	}{
		{"missing uid", func(v *v1alpha1.GlobalVpc, c *config.Config) { v.UID = "" }},
		{"unregistered domain", func(v *v1alpha1.GlobalVpc, c *config.Config) { v.Spec.DomainRef = "elsewhere" }},
		{"unregistered provider", func(v *v1alpha1.GlobalVpc, c *config.Config) { v.Spec.IPAM.ProviderRef = "unknown" }},
		{"empty scope", func(v *v1alpha1.GlobalVpc, c *config.Config) { v.Spec.IPAM.ScopeRef = " " }},
		{"sparse membership", func(v *v1alpha1.GlobalVpc, c *config.Config) { v.Spec.Attachments = v.Spec.Attachments[:1] }},
		{"unknown cluster", func(v *v1alpha1.GlobalVpc, c *config.Config) { v.Spec.Attachments[1].ClusterRef = "dc-c" }},
		{"duplicate cluster", func(v *v1alpha1.GlobalVpc, c *config.Config) { v.Spec.Attachments[1].ClusterRef = "dc-a" }},
		{"duplicate cluster UID", func(v *v1alpha1.GlobalVpc, c *config.Config) { c.Clusters[1].UID = c.Clusters[0].UID }},
		{"duplicate transit IP", func(v *v1alpha1.GlobalVpc, c *config.Config) {
			v.Spec.Attachments[1].TransitIP = v.Spec.Attachments[0].TransitIP
		}},
		{"site overlap", func(v *v1alpha1.GlobalVpc, c *config.Config) { v.Spec.Attachments[1].CIDR = "10.241.0.128/25" }},
		{"transit overlap", func(v *v1alpha1.GlobalVpc, c *config.Config) { v.Spec.Attachments[1].CIDR = "10.240.255.0/24" }},
		{"noncanonical network", func(v *v1alpha1.GlobalVpc, c *config.Config) { v.Spec.Attachments[0].CIDR = "10.241.0.1/24" }},
		{"IPv6", func(v *v1alpha1.GlobalVpc, c *config.Config) { v.Spec.TransitCIDR = "fd00::/64" }},
		{"broad subnet", func(v *v1alpha1.GlobalVpc, c *config.Config) { v.Spec.TransitCIDR = "0.0.0.0/0" }},
		{"no usable transit", func(v *v1alpha1.GlobalVpc, c *config.Config) { v.Spec.TransitCIDR = "10.240.255.0/31" }},
		{"gateway network address", func(v *v1alpha1.GlobalVpc, c *config.Config) { v.Spec.Attachments[0].Gateway = "10.241.0.0" }},
		{"gateway broadcast address", func(v *v1alpha1.GlobalVpc, c *config.Config) { v.Spec.Attachments[0].Gateway = "10.241.0.255" }},
		{"gateway outside subnet", func(v *v1alpha1.GlobalVpc, c *config.Config) { v.Spec.Attachments[0].Gateway = "10.243.0.1" }},
		{"transit network address", func(v *v1alpha1.GlobalVpc, c *config.Config) { v.Spec.Attachments[0].TransitIP = "10.240.255.0" }},
		{"transit broadcast address", func(v *v1alpha1.GlobalVpc, c *config.Config) { v.Spec.Attachments[0].TransitIP = "10.240.255.7" }},
		{"multicast", func(v *v1alpha1.GlobalVpc, c *config.Config) { v.Spec.TransitCIDR = "224.0.0.0/24" }},
		{"loopback", func(v *v1alpha1.GlobalVpc, c *config.Config) { v.Spec.TransitCIDR = "127.0.0.0/24" }},
		{"duplicate chassis", func(v *v1alpha1.GlobalVpc, c *config.Config) { c.Clusters[0].GatewayChassis = []string{"a", "a"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vpc, cfg := fixture()
			tc.edit(vpc, &cfg)
			if p, err := Build(vpc, cfg); err == nil || p != nil {
				t.Fatalf("invalid intent returned an actionable plan: plan=%v err=%v", p, err)
			}
		})
	}
}

func TestRegistrationDriftChangesPlanHash(t *testing.T) {
	vpc, cfg := fixture()
	original, err := Build(vpc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*config.Config)
	}{
		{"kubeconfig", func(c *config.Config) { c.Clusters[0].Kubeconfig = "other.conf" }},
		{"cluster identity", func(c *config.Config) { c.Clusters[0].UID = "cluster-replacement" }},
		{"location", func(c *config.Config) { c.Clusters[0].Site = "site-replacement" }},
		{"availability zone", func(c *config.Config) { c.Clusters[0].AZName = "az-replacement" }},
		{"gateway", func(c *config.Config) { c.Clusters[0].GatewayChassis = []string{"new-gateway"} }},
		{"local NB", func(c *config.Config) { c.Clusters[0].NBCommand = []string{"new-nbctl"} }},
		{"local SB", func(c *config.Config) { c.Clusters[0].SBCommand = []string{"new-sbctl"} }},
		{"IC Deployment", func(c *config.Config) { c.Clusters[0].ICDeployment.UID = "ic-replacement" }},
		{"IC NB", func(c *config.Config) { c.ICNBCommand = []string{"new-ic-nbctl"} }},
		{"provider", func(c *config.Config) { c.Providers["registry"] = []string{"new-provider"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, changed := fixture()
			tc.edit(&changed)
			p, err := Build(vpc, changed)
			if err != nil {
				t.Fatal(err)
			}
			if p.Hash == original.Hash {
				t.Fatal("registration drift must invalidate accepted plan hash")
			}
		})
	}
}

func TestTransitSlash30ExcludesBothAddresses(t *testing.T) {
	vpc, cfg := fixture()
	vpc.Spec.TransitCIDR = "10.240.255.0/30"
	p, err := Build(vpc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	excluded, _, _ := unstructured.NestedStringSlice(p.Attachments[0].Transit.Object, "spec", "excludeIps")
	if strings.Join(excluded, ",") != "10.240.255.1..10.240.255.2" {
		t.Fatalf("unsafe /30 transit exclusion: %v", excluded)
	}
}

func TestEveryRemotePrefixHasOneRoute(t *testing.T) {
	vpc, cfg := fixture()
	cfg.Clusters = append(cfg.Clusters, config.Cluster{
		Name: "dc-c", UID: "cluster-c", Region: "region-2", Site: "site-c", DC: "dc-c",
		Kubeconfig: "c.conf", NBCommand: []string{"c-nbctl"}, SBCommand: []string{"c-sbctl"},
		AZName: "az-c", GatewayChassis: []string{"c-gateway"},
		ICDeployment: config.DeploymentReference{Namespace: "ic", Name: "ovn-ic", UID: "ic-c"},
	})
	vpc.Spec.Attachments = append(vpc.Spec.Attachments, v1alpha1.Attachment{
		ClusterRef: "dc-c", CIDR: "10.243.0.0/24", Gateway: "10.243.0.1", TransitIP: "10.240.255.3",
	})
	p, err := Build(vpc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, site := range p.Attachments {
		routes, _, _ := unstructured.NestedSlice(site.Vpc.Object, "spec", "staticRoutes")
		if len(routes) != 2 {
			t.Fatalf("cluster %s has %d routes, want two", site.Cluster.Name, len(routes))
		}
		seen := map[string]bool{}
		for _, raw := range routes {
			route := raw.(map[string]interface{})
			cidr := route["cidr"].(string)
			if cidr == site.Intent.CIDR || seen[cidr] {
				t.Fatal("route loops back to the local prefix or duplicates a remote prefix")
			}
			seen[cidr] = true
			for _, remote := range p.Attachments {
				if remote.Intent.CIDR == cidr && route["nextHopIP"] != remote.Intent.TransitIP {
					t.Fatal("remote prefix routes through the wrong transit gateway")
				}
			}
		}
	}
	// Three participants cannot share a /30 with only two usable gateway IPs.
	vpc.Spec.TransitCIDR = "10.240.255.0/30"
	if p, err := Build(vpc, cfg); err == nil || p != nil {
		t.Fatal("insufficient transit capacity returned an actionable plan")
	}
}
