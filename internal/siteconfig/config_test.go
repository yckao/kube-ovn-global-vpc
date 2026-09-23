package siteconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func fixture() Config {
	return Config{SiteID: "site-a", AttachmentID: "infra-a", Cluster: Cluster{Name: "infra-a", UID: "infra-a-uid", Kubeconfig: "/local/kubeconfig", NBCommand: []string{"local-nbctl"}, SBCommand: []string{"local-sbctl"}},
		GatewayCommand: []string{"local-gateway", "--config", "/local/gateway.json"}, GatewayRevision: "registration-v1",
		Grants: []Grant{{GlobalVpcID: "tenant-red", DelegatedPrefixes: []string{"10.241.0.0/16"}, TransitCIDR: "10.240.255.0/30", RouterIP: "10.240.255.1", GatewayIP: "10.240.255.2"}}}
}

func TestLocalRegistrationRejectsInvalidDelegation(t *testing.T) {
	if err := fixture().Validate(); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		change func(*Config)
	}{
		{"empty site", func(c *Config) { c.SiteID = "" }},
		{"invalid attachment", func(c *Config) { c.AttachmentID = "other/site" }},
		{"missing local identity", func(c *Config) { c.Cluster.UID = "" }},
		{"missing NB command", func(c *Config) { c.Cluster.NBCommand = nil }},
		{"missing gateway command", func(c *Config) { c.GatewayCommand = nil }},
		{"missing gateway revision", func(c *Config) { c.GatewayRevision = "" }},
		{"missing grant", func(c *Config) { c.Grants = nil }},
		{"duplicate grant", func(c *Config) { c.Grants = append(c.Grants, c.Grants[0]) }},
		{"invalid global ID", func(c *Config) { c.Grants[0].GlobalVpcID = "tenant/red" }},
		{"missing delegation", func(c *Config) { c.Grants[0].DelegatedPrefixes = nil }},
		{"overlapping delegations", func(c *Config) {
			c.Grants[0].DelegatedPrefixes = append(c.Grants[0].DelegatedPrefixes, "10.241.1.0/24")
		}},
		{"transit overlaps delegation", func(c *Config) { c.Grants[0].DelegatedPrefixes = []string{"10.240.0.0/16"} }},
		{"host bits", func(c *Config) { c.Grants[0].DelegatedPrefixes = []string{"10.241.1.1/24"} }},
		{"IPv6 delegation", func(c *Config) { c.Grants[0].DelegatedPrefixes = []string{"fd00::/64"} }},
		{"default delegation", func(c *Config) { c.Grants[0].DelegatedPrefixes = []string{"0.0.0.0/0"} }},
		{"duplicate transit IP", func(c *Config) { c.Grants[0].GatewayIP = c.Grants[0].RouterIP }},
		{"transit network IP", func(c *Config) { c.Grants[0].GatewayIP = "10.240.255.0" }},
		{"transit broadcast IP", func(c *Config) { c.Grants[0].GatewayIP = "10.240.255.3" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fixture()
			tc.change(&c)
			if c.Validate() == nil {
				t.Fatal("invalid registration accepted")
			}
		})
	}
}

func TestDifferentTenantsMayDelegateOverlappingCIDRs(t *testing.T) {
	c := fixture()
	other := c.Grants[0]
	other.GlobalVpcID = "tenant-blue"
	c.Grants = append(c.Grants, other)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRejectsRemoteCredentialsAndMultipleDocuments(t *testing.T) {
	encoded, _ := json.Marshal(fixture())
	for _, raw := range []string{string(encoded[:len(encoded)-1]) + `,"remoteKubeconfig":"remote.conf"}`, string(encoded) + ` {}`, string(encoded) + ` broken`} {
		path := filepath.Join(t.TempDir(), "site.json")
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
	path := filepath.Join(t.TempDir(), "site.json")
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
}
