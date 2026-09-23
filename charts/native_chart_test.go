package charts

import (
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func nativeChartValues() map[string]any {
	return map[string]any{
		"cliImage": map[string]any{"repository": "example.invalid/vpcctl", "digest": "sha256:" + strings.Repeat("a", 64)},
		"bundle":   map[string]any{"target": "v1.16.3"},
		"native":   map[string]any{"target": "v1.16.3", "namespace": "native-system", "deployment": "native-controller", "container": "controller"},
		"ovn":      map[string]any{"namespace": "ovn-system", "deployment": "ovn-central", "container": "ovn-central", "database": "unix:/run/ovn/ovnnb_db.sock"},
	}
}

func TestNativeChartKeepsUpstreamOwnershipAndScopesCrossNamespaceAccess(t *testing.T) {
	objects := helmRender(t, "kube-ovn-global-vpc-extension", nativeChartValues(), false)
	hooks := map[string]bool{}
	guards, execRoles, patchRoles := 0, 0, 0
	for _, object := range objects {
		if object.GetKind() == "Deployment" || object.GetKind() == "CustomResourceDefinition" || object.GetKind() == "Secret" {
			t.Fatal("integration chart must not adopt upstream resources or replace private recovery receipts")
		}
		if object.GetKind() == "ClusterRole" {
			guards++
			if object.GetName() != "global-vpc-native-extension" || object.GetAnnotations()["helm.sh/hook"] != "" {
				t.Fatal("cluster-wide single-release guard must be a normal Helm-owned resource before hooks run")
			}
		}
		if object.GetKind() == "Role" {
			rules, _, _ := unstructured.NestedSlice(object.Object, "rules")
			for _, value := range rules {
				rule := value.(map[string]any)
				encoded, _ := json.Marshal(rule)
				if strings.Contains(string(encoded), `"pods/exec"`) {
					execRoles++
					if object.GetNamespace() != "ovn-system" {
						t.Fatal("OVN exec permissions escaped the selected OVN namespace")
					}
				}
				if strings.Contains(string(encoded), `"deployments"`) && strings.Contains(string(encoded), `"patch"`) {
					patchRoles++
					if object.GetNamespace() != "native-system" || len(rule["resourceNames"].([]any)) != 1 || rule["resourceNames"].([]any)[0] != "native-controller" {
						t.Fatal("native image mutation permission is not scoped to the configured Deployment")
					}
				}
			}
		}
		if object.GetKind() == "Job" {
			hooks[object.GetAnnotations()["helm.sh/hook"]] = true
			containers, _, _ := unstructured.NestedSlice(object.Object, "spec", "template", "spec", "containers")
			encoded, _ := json.Marshal(containers[0])
			for _, flag := range []string{"--controller-namespace=native-system", "--ovn-namespace=ovn-system", "--release-namespace=custom-system"} {
				if !strings.Contains(string(encoded), flag) {
					t.Fatalf("hook did not use configured namespaces: missing %s", flag)
				}
			}
		}
	}
	if guards != 1 || execRoles != 1 || patchRoles != 1 || len(hooks) != 3 || !hooks["post-install,post-upgrade,post-rollback"] || !hooks["pre-delete"] || !hooks["test"] {
		t.Fatalf("unexpected native lifecycle inventory: guards=%d exec=%d patch=%d hooks=%v", guards, execRoles, patchRoles, hooks)
	}
}

func TestNativeOwnershipGuardCannotBeBypassedByAnotherDeploymentTarget(t *testing.T) {
	values := nativeChartValues()
	values["native"].(map[string]any)["deployment"] = "another-controller"
	values["fullnameOverride"] = "another-release-name"
	objects := helmRender(t, "kube-ovn-global-vpc-extension", values, false)
	found := false
	for _, object := range objects {
		if object.GetKind() == "ClusterRole" && object.GetName() == "global-vpc-native-extension" {
			found = true
		}
	}
	if !found {
		t.Fatal("changing target or release name bypassed the shared CRD ownership guard")
	}
}

func TestNativeChartRequiresPublishedInputs(t *testing.T) {
	values := nativeChartValues()
	delete(values, "bundle")
	helmRender(t, "kube-ovn-global-vpc-extension", values, true)
	values = nativeChartValues()
	values["cliImage"].(map[string]any)["digest"] = "latest"
	helmRender(t, "kube-ovn-global-vpc-extension", values, true)
}
