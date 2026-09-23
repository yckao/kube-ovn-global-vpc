package vpcctl

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	api "globalvpc.io/controller/api/v1alpha2"
	"globalvpc.io/controller/internal/platformconfig"
	"globalvpc.io/controller/internal/platformplan"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

func additivePlatformFixture() (platformconfig.Config, platformconfig.Config) {
	before := platformconfig.Config{RegistryNamespace: "operator-system", NetworkClasses: map[string]platformconfig.NetworkClass{"default": {TransportProfile: "wireguard-bgp"}}, Locations: []platformconfig.Location{{Name: "dc-a", Region: "region-1", Site: "site-a", DC: "dc-a", BindingNamespace: "global-vpc-dc-a", AllowedProjects: []string{"project-demo"}, CIDRPools: []string{"10.60.0.0/16"}}}}
	var after platformconfig.Config
	raw, _ := json.Marshal(before)
	_ = json.Unmarshal(raw, &after)
	after.Locations = append(after.Locations, platformconfig.Location{Name: "dc-b", Region: "region-1", Site: "site-b", DC: "dc-b", BindingNamespace: "global-vpc-dc-b", AllowedProjects: []string{"project-demo"}, CIDRPools: []string{"10.61.0.0/16"}})
	return before, after
}

func TestPlatformConfigGuardAllowsNewLocationWithExistingNetworks(t *testing.T) {
	before, after := additivePlatformFixture()
	beforeRaw, _ := json.Marshal(before)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "authority-config", Namespace: "operator-system"}, Data: map[string]string{"platform.json": string(beforeRaw)}}
	active := platformTerminalFixture()
	active.Spec.Deleting = false
	active.Spec.Revision = platformplan.Revision(active.Spec)
	active.Status = api.NetworkBindingStatus{}
	vpc := &api.VPC{ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: "project-demo", UID: "vpc-existing"}}
	subnet := &api.Subnet{ObjectMeta: metav1.ObjectMeta{Name: "apps-a", Namespace: "project-demo", UID: "subnet-existing"}, Spec: api.SubnetSpec{VPCRef: vpc.Name, LocationRef: "dc-a", CIDR: "10.60.1.0/24"}}
	c := platformHookClient(t, cm, active, vpc, subnet)
	path := filepath.Join(t.TempDir(), "platform.json")
	run := func(cfg platformconfig.Config) (string, error) {
		raw, _ := json.Marshal(cfg)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		return platformRunHook(t, c, "authority", "check-config", "--config-map", cm.Name, "--desired-config", path)
	}
	out, err := run(after)
	if err != nil || !strings.Contains(out, "Additive location registration") {
		t.Fatalf("adding DC-B with active DC-A network rejected: %v %s", err, out)
	}
	// Ordering of complete existing entries is irrelevant during an addition.
	reordered := after
	reordered.Locations = append([]platformconfig.Location(nil), after.Locations...)
	reordered.Locations[0], reordered.Locations[1] = reordered.Locations[1], reordered.Locations[0]
	if _, err = run(reordered); err != nil {
		t.Fatalf("reordered additive location registration rejected: %v", err)
	}
	for name, mutate := range map[string]func(*platformconfig.Config){
		"existing pool edit": func(cfg *platformconfig.Config) { cfg.Locations[0].CIDRPools = []string{"10.62.0.0/16"} },
		"existing grant edit": func(cfg *platformconfig.Config) {
			cfg.Locations[0].AllowedProjects = append(cfg.Locations[0].AllowedProjects, "another-project")
		},
		"existing identity edit":          func(cfg *platformconfig.Config) { cfg.Locations[0].DC = "replacement-dc" },
		"existing binding namespace edit": func(cfg *platformconfig.Config) { cfg.Locations[0].BindingNamespace = "replacement-bindings" },
		"class transport edit": func(cfg *platformconfig.Config) {
			cfg.NetworkClasses["default"] = platformconfig.NetworkClass{TransportProfile: "geneve-bgp"}
		},
		"class registration edit": func(cfg *platformconfig.Config) {
			cfg.NetworkClasses["extra"] = platformconfig.NetworkClass{TransportProfile: "wireguard-bgp"}
		},
		"registry edit":             func(cfg *platformconfig.Config) { cfg.RegistryNamespace = "new-registry" },
		"existing location removal": func(cfg *platformconfig.Config) { cfg.Locations = cfg.Locations[1:] },
		"duplicate binding namespace": func(cfg *platformconfig.Config) {
			cfg.Locations[1].BindingNamespace = cfg.Locations[0].BindingNamespace
		},
		"duplicate location name": func(cfg *platformconfig.Config) { cfg.Locations[1].Name = cfg.Locations[0].Name },
		"unreserved new location": func(cfg *platformconfig.Config) { cfg.Locations[1].CIDRPools = nil },
		"invalid new grant":       func(cfg *platformconfig.Config) { cfg.Locations[1].AllowedProjects = []string{"*"} },
	} {
		t.Run(name, func(t *testing.T) {
			_, candidate := additivePlatformFixture()
			mutate(&candidate)
			if _, err := run(candidate); err == nil {
				t.Fatal("unsafe change was included in additive registration")
			}
		})
	}
	// Registering locations must not become an exception for site allocator edits.
	siteCM := cm.DeepCopy()
	siteCM.Data = map[string]string{"site.json": string(beforeRaw)}
	siteClient := platformHookClient(t, siteCM, active)
	raw, _ := json.Marshal(after)
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = platformRunHook(t, siteClient, "site", "check-config", "--config-map", cm.Name, "--desired-config", path); err == nil {
		t.Fatal("site allocator accepted authority-only additive exception")
	}
}

func TestAuthorityPlanPreservesDCAUntilSubnetUsesNewLocation(t *testing.T) {
	before, after := additivePlatformFixture()
	vpc := &api.VPC{ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: "project-demo", UID: "vpc-existing"}}
	subnetA := api.Subnet{ObjectMeta: metav1.ObjectMeta{Name: "apps-a", Namespace: vpc.Namespace, UID: "subnet-a"}, Spec: api.SubnetSpec{VPCRef: vpc.Name, LocationRef: "dc-a", CIDR: "10.60.1.0/24"}, Status: api.SubnetStatus{VPCUID: string(vpc.UID)}}
	initial, err := platformplan.Build(vpc, []api.Subnet{subnetA}, nil, before)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := platformplan.Build(vpc, []api.Subnet{subnetA}, initial, after)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(initial, unchanged) {
		t.Fatal("registering unused DC-B changed DC-A binding")
	}
	subnetB := api.Subnet{ObjectMeta: metav1.ObjectMeta{Name: "apps-b", Namespace: vpc.Namespace, UID: "subnet-b"}, Spec: api.SubnetSpec{VPCRef: vpc.Name, LocationRef: "dc-b", CIDR: "10.61.1.0/24"}, Status: api.SubnetStatus{VPCUID: string(vpc.UID)}}
	if err = platformplan.ValidateSubnet(vpc, &subnetB, before); err == nil {
		t.Fatal("DC-B subnet authorized before location registration")
	}
	if err = platformplan.ValidateSubnet(vpc, &subnetB, after); err != nil {
		t.Fatal(err)
	}
	expanded, err := platformplan.Build(vpc, []api.Subnet{subnetA, subnetB}, initial, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(expanded) != 2 || expanded[0].Name != initial[0].Name || expanded[0].Namespace != initial[0].Namespace || expanded[0].Spec.NativeVpcName != initial[0].Spec.NativeVpcName {
		t.Fatal("second-location materialization replaced DC-A identity")
	}
	if len(expanded[0].Spec.RemoteSubnets) != 1 || expanded[0].Spec.RemoteSubnets[0].CIDR != subnetB.Spec.CIDR || expanded[1].Spec.LocationRef != "dc-b" || expanded[1].Namespace != after.Locations[1].BindingNamespace {
		t.Fatal("DC-B subnet did not generate the expected new binding and remote export")
	}
}

func TestProgressiveHelmExamplesAreAdditive(t *testing.T) {
	load := func(name string) map[string]any {
		raw, err := os.ReadFile(filepath.Join("..", "..", "config", "examples", "helm", name))
		if err != nil {
			t.Fatal(err)
		}
		data, err := yaml.YAMLToJSONStrict(raw)
		if err != nil {
			t.Fatal(err)
		}
		var values map[string]any
		if err := json.Unmarshal(data, &values); err != nil {
			t.Fatal(err)
		}
		return values
	}
	before, after := load("authority-dc-a.yaml"), load("authority.yaml")
	if !platformAddsLocations(before["platform"].(map[string]any), after["platform"].(map[string]any)) {
		t.Fatal("documented DC-A to DC-B registration no longer passes the additive lifecycle guard")
	}
	if !nativeJSONEqual(before["projects"], after["projects"]) {
		t.Fatal("progressive registration example unexpectedly changes tenant RBAC")
	}
	if platformAddsLocations(after["platform"].(map[string]any), before["platform"].(map[string]any)) {
		t.Fatal("rollback removing DC-B bypassed normal active-resource protection")
	}
}
