package localconfig

import (
	"testing"
)

func validConfig() Config {
	return Config{LocationRef: "dc-a", ClusterUID: "cluster-uid", Namespace: "global-vpc-system", AuthorityNamespace: "global-vpc-dc-a", AuthorityKubeconfig: "/authority/kubeconfig", GatewayNodeSelector: map[string]string{"gateway": "true"}, TransitPool: "172.30.0.0/20", TransitPrefixLength: 27, BFDSourcePool: "172.30.16.0/24", ControlPool: "172.30.32.0/20", PortStart: 20000, PortEnd: 29999, GatewayReplicas: 2, GatewayImage: "example.invalid/gateway@sha256:unused", RuntimeDir: "/opt/global-vpc/gateway", MTU: 1380}
}
func TestManagedLocalInfrastructureConfiguration(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Config){func(c *Config) { c.ControlPool = c.TransitPool }, func(c *Config) { c.ControlPool = c.BFDSourcePool }, func(c *Config) { c.PortStart = 6082; c.PortEnd = 6082 }, func(c *Config) { c.PortStart = 6081; c.PortEnd = 6081 }, func(c *Config) { c.PortStart = 4788; c.PortEnd = 4789 }, func(c *Config) { c.RuntimeDir = "" }, func(c *Config) { c.GatewayReplicas = 8; c.TransitPrefixLength = 29 }} {
		c := validConfig()
		mutate(&c)
		if c.Validate() == nil {
			t.Fatal("invalid local infrastructure reservation accepted")
		}
	}
}
func TestMaintainedSiteExamplesLoad(t *testing.T) {
	for _, path := range []string{"../../config/examples/managed/site-a.json", "../../config/examples/managed/site-b.json"} {
		if _, err := Load(path); err != nil {
			t.Fatal(path, err)
		}
	}
}
