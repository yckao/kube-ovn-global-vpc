package siteplan

import (
	"encoding/json"
	"net/netip"
	"reflect"
	"strings"
	"testing"

	api "globalvpc.io/controller/api/v1alpha1"
	"globalvpc.io/controller/internal/siteconfig"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func haFixture() (*api.SiteVpc, siteconfig.Config) {
	v, cfg := fixture()
	cfg.BFDTransactionCommand = []string{"local-ovsdb-client", "transact", "unix:/local/nb.sock"}
	g := &cfg.Grants[0]
	g.GatewayIP = ""
	g.Gateways = []siteconfig.Gateway{{ID: "gw2", IP: "10.240.255.4"}, {ID: "gw1", IP: "10.240.255.2"}}
	g.BFD = &siteconfig.BFD{SourceIP: "10.254.120.1", MinRX: 300, MinTX: 300, Multiplier: 3, NodeSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"role": "infra"}, MatchExpressions: []metav1.LabelSelectorRequirement{
		{Key: "zone", Operator: metav1.LabelSelectorOpIn, Values: []string{"z2", "z1"}},
		{Key: "has-bfd", Operator: metav1.LabelSelectorOpExists},
	}}}
	return v, cfg
}

func TestHAStartsWithoutUnmonitoredRoutesAndPreservesNativeOwnership(t *testing.T) {
	v, cfg := haFixture()
	p, err := Build(v, cfg)
	if err != nil {
		t.Fatal(err)
	}
	routes, exists, err := unstructured.NestedSlice(p.Vpc.Object, "spec", "staticRoutes")
	if err != nil || !exists || len(routes) != 0 {
		t.Fatalf("HA bootstrap must not expose any default route before BFD receipts: %v", routes)
	}
	bfd, _, _ := unstructured.NestedMap(p.Vpc.Object, "spec", "bfdPort")
	if bfd["enabled"] != true || bfd["ip"] != cfg.Grants[0].BFD.SourceIP {
		t.Fatalf("native BFD port declaration missing: %#v", bfd)
	}
	if p.Gateway.Version != "v2" || !p.Gateway.IsHA() || p.Gateway.GatewayIP != "" || p.Gateway.BFDPortName() != "bfd@"+p.VpcName {
		t.Fatal("wrong HA gateway protocol identity")
	}
	want := []siteconfig.Gateway{{ID: "gw1", IP: "10.240.255.2"}, {ID: "gw2", IP: "10.240.255.4"}}
	if !reflect.DeepEqual(p.Gateway.Members(), want) {
		t.Fatalf("members not canonically sorted: %v", p.Gateway.Members())
	}
	excluded, _, _ := unstructured.NestedStringSlice(p.Transit.Object, "spec", "excludeIps")
	if !reflect.DeepEqual(excluded, []string{"10.240.255.1", "10.240.255.3", "10.240.255.5..10.240.255.6"}) {
		t.Fatalf("only explicit gateway member addresses may remain allocatable: %v", excluded)
	}
	encoded, _ := json.Marshal(p.Gateway)
	if strings.Contains(string(encoded), `"gatewayIP"`) || !strings.Contains(string(encoded), `"bfd"`) {
		t.Fatalf("ambiguous gateway wire configuration: %s", encoded)
	}
	if _, exists, _ := unstructured.NestedFieldNoCopy(p.Vpc.Object, "spec", "enableBfd"); exists {
		t.Fatal("must not enable the native external-gateway BFD controller")
	}
}

func TestHAPlanCanonicalAndIndependentOfRuntimeReceipts(t *testing.T) {
	v, cfg := haFixture()
	before, _ := json.Marshal(cfg)
	first, err := Build(v, cfg)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(cfg)
	if string(before) != string(after) {
		t.Fatal("planning mutated administrator configuration")
	}
	g := &cfg.Grants[0]
	g.Gateways[0], g.Gateways[1] = g.Gateways[1], g.Gateways[0]
	g.BFD.NodeSelector.MatchExpressions[0].Values = []string{"z1", "z2"}
	expressions := g.BFD.NodeSelector.MatchExpressions
	expressions[0], expressions[1] = expressions[1], expressions[0]
	v.Status.BFDResources = []api.BFDResourceRecord{{GatewayID: "gw1", UUID: "runtime-id"}}
	second, err := Build(v, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("ordering or runtime BFD UUID changed desired plan")
	}
	other := cfg.Grants[0]
	other.GlobalVpcID = "tenant-blue"
	cfg.Grants = append(cfg.Grants, other)
	third, err := Build(v, cfg)
	if err != nil || !reflect.DeepEqual(first, third) {
		t.Fatal("unrelated tenant changed HA plan", err)
	}
	members := first.Gateway.Members()
	members[0].IP = "different"
	if first.Gateway.Gateways[0].IP == "different" {
		t.Fatal("member copy mutated plan")
	}
}

func TestHARegistrationChangesInvalidatePlan(t *testing.T) {
	v, cfg := haFixture()
	baseline, _ := Build(v, cfg)
	for _, edit := range []func(*siteconfig.Grant){
		func(g *siteconfig.Grant) { g.Gateways[0].ID = "replacement" },
		func(g *siteconfig.Grant) { g.Gateways[0].IP = "10.240.255.3" },
		func(g *siteconfig.Grant) { g.BFD.SourceIP = "10.254.120.2" },
		func(g *siteconfig.Grant) { g.BFD.MinRX = 1000 },
		func(g *siteconfig.Grant) { g.BFD.MinTX = 1000 },
		func(g *siteconfig.Grant) { g.BFD.Multiplier = 5 },
		func(g *siteconfig.Grant) { g.BFD.NodeSelector.MatchLabels["role"] = "gateway" },
	} {
		_, changed := haFixture()
		edit(&changed.Grants[0])
		p, err := Build(v, changed)
		if err != nil {
			t.Fatal(err)
		}
		if p.Hash == baseline.Hash {
			t.Fatal("unsafe routing registration drift reused accepted hash")
		}
	}
}

func TestTransactionTransportChangePreservesAcceptedPlan(t *testing.T) {
	for _, makeFixture := range []func() (*api.SiteVpc, siteconfig.Config){fixture, haFixture} {
		v, cfg := makeFixture()
		first, err := Build(v, cfg)
		if err != nil {
			t.Fatal(err)
		}
		cfg.BFDTransactionCommand = []string{"updated-local-wrapper", "transact", "unix:/local/nb.sock"}
		second, err := Build(v, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(first, second) {
			t.Fatal("runtime transaction access changed tenant plan hash or desired resources")
		}
	}
}

func TestTransitExclusionsProtectEveryNonMemberAddress(t *testing.T) {
	p := netip.MustParsePrefix("10.240.255.0/28")
	for _, members := range [][]string{{"10.240.255.1", "10.240.255.14"}, {"10.240.255.3", "10.240.255.2"}, {"10.240.255.1", "10.240.255.2", "10.240.255.3", "10.240.255.4", "10.240.255.5", "10.240.255.6", "10.240.255.7", "10.240.255.8"}} {
		excluded := map[netip.Addr]bool{}
		for _, raw := range transitExclusions(p, members...) {
			rangeParts := strings.Split(raw.(string), "..")
			first := netip.MustParseAddr(rangeParts[0])
			last := first
			if len(rangeParts) == 2 {
				last = netip.MustParseAddr(rangeParts[1])
			}
			for a := first; a.Compare(last) <= 0; a = a.Next() {
				excluded[a] = true
			}
		}
		allowed := map[netip.Addr]bool{}
		for _, member := range members {
			allowed[netip.MustParseAddr(member)] = true
		}
		for a := p.Addr().Next(); a.Less(siteconfig.LastAddress(p)); a = a.Next() {
			if excluded[a] == allowed[a] {
				t.Fatalf("transit allocation wrong for %s, members %v", a, members)
			}
		}
	}
}
