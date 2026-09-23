package siteconfig

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func haFixture() Config {
	c := fixture()
	c.BFDTransactionCommand = []string{"local-ovsdb-client", "transact", "unix:/local/nb.sock"}
	g := &c.Grants[0]
	g.GatewayIP, g.TransitCIDR = "", "10.240.255.0/28"
	g.Gateways = []Gateway{{ID: "gw1", IP: "10.240.255.2"}, {ID: "gw2", IP: "10.240.255.3"}}
	g.BFD = &BFD{SourceIP: "10.254.120.1", MinRX: 300, MinTX: 300, Multiplier: 3}
	return c
}

func TestHAGrantsRejectAmbiguousOrUnmonitorableMembers(t *testing.T) {
	if err := haFixture().Validate(); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		edit func(*Grant)
	}{
		{"legacy with HA", func(g *Grant) { g.GatewayIP = "10.240.255.4" }},
		{"single HA member", func(g *Grant) { g.Gateways = g.Gateways[:1] }},
		{"too many members", func(g *Grant) { g.Gateways = append(g.Gateways, make([]Gateway, 7)...) }},
		{"missing BFD", func(g *Grant) { g.BFD = nil }},
		{"BFD without HA", func(g *Grant) { g.Gateways = nil; g.GatewayIP = "10.240.255.2" }},
		{"no gateways", func(g *Grant) { g.Gateways = nil; g.BFD = nil }},
		{"duplicate ID", func(g *Grant) { g.Gateways[1].ID = g.Gateways[0].ID }},
		{"invalid ID", func(g *Grant) { g.Gateways[1].ID = "gw/2" }},
		{"duplicate IP", func(g *Grant) { g.Gateways[1].IP = g.Gateways[0].IP }},
		{"router IP", func(g *Grant) { g.Gateways[1].IP = g.RouterIP }},
		{"outside transit", func(g *Grant) { g.Gateways[1].IP = "10.240.254.3" }},
		{"network address", func(g *Grant) { g.Gateways[1].IP = "10.240.255.0" }},
		{"broadcast address", func(g *Grant) { g.Gateways[1].IP = "10.240.255.15" }},
		{"IPv6 gateway", func(g *Grant) { g.Gateways[1].IP = "fd00::1" }},
		{"source in transit", func(g *Grant) { g.BFD.SourceIP = "10.240.255.14" }},
		{"source in delegation", func(g *Grant) { g.BFD.SourceIP = "10.241.10.1" }},
		{"unspecified source", func(g *Grant) { g.BFD.SourceIP = "0.0.0.0" }},
		{"loopback source", func(g *Grant) { g.BFD.SourceIP = "127.0.0.1" }},
		{"link local source", func(g *Grant) { g.BFD.SourceIP = "169.254.1.1" }},
		{"multicast source", func(g *Grant) { g.BFD.SourceIP = "224.0.0.1" }},
		{"broadcast source", func(g *Grant) { g.BFD.SourceIP = "255.255.255.255" }},
		{"IPv6 source", func(g *Grant) { g.BFD.SourceIP = "fd00::1" }},
		{"source prefix", func(g *Grant) { g.BFD.SourceIP = "10.254.120.1/32" }},
		{"no receive timer", func(g *Grant) { g.BFD.MinRX = 0 }},
		{"receive timer too short", func(g *Grant) { g.BFD.MinRX = 99 }},
		{"transmit timer too long", func(g *Grant) { g.BFD.MinTX = 60001 }},
		{"multiplier too short", func(g *Grant) { g.BFD.Multiplier = 1 }},
		{"multiplier too large", func(g *Grant) { g.BFD.Multiplier = 256 }},
		{"invalid selector", func(g *Grant) {
			g.BFD.NodeSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"bad/key/extra": "yes"}}
		}},
		{"invalid selector expression", func(g *Grant) {
			g.BFD.NodeSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "node", Operator: metav1.LabelSelectorOpIn}}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := haFixture()
			tc.edit(&c.Grants[0])
			if err := c.Validate(); err == nil {
				t.Fatal("unsafe HA registration accepted")
			}
		})
	}
}

func TestHAMaximumMembersAndTimerBoundaries(t *testing.T) {
	c := haFixture()
	g := &c.Grants[0]
	for i, ip := range []string{"10.240.255.4", "10.240.255.5", "10.240.255.6", "10.240.255.7", "10.240.255.8", "10.240.255.9"} {
		g.Gateways = append(g.Gateways, Gateway{ID: string(rune('a' + i)), IP: ip})
	}
	g.BFD.MinRX, g.BFD.MinTX, g.BFD.Multiplier = 100, 60000, 255
	g.BFD.NodeSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "kubernetes.io/hostname", Operator: metav1.LabelSelectorOpIn, Values: []string{"infra-1", "infra-2"}}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestHATransactionTransportIsRequiredOnlyForHA(t *testing.T) {
	for _, argv := range [][]string{nil, {}, {"", "transact"}} {
		c := haFixture()
		c.BFDTransactionCommand = argv
		if c.Validate() == nil {
			t.Fatal("HA accepted missing transaction transport")
		}
		legacy := fixture()
		legacy.BFDTransactionCommand = argv
		if err := legacy.Validate(); err != nil {
			t.Fatal("unused optional HA transport broke legacy registration", err)
		}
	}
}
