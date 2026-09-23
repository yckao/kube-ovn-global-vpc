package charts

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"globalvpc.io/controller/internal/localconfig"
	"globalvpc.io/controller/internal/platformconfig"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
)

func chartValues(t *testing.T, site bool) map[string]any {
	t.Helper()
	image := map[string]any{"repository": "example.invalid/prebuilt", "digest": "sha256:" + strings.Repeat("a", 64)}
	values := map[string]any{"image": image, "cliImage": image}
	if site {
		raw, err := os.ReadFile("../config/examples/managed/site-a.json")
		if err != nil {
			t.Fatal(err)
		}
		var local map[string]any
		if err = json.Unmarshal(raw, &local); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"namespace", "authorityKubeconfig", "gatewayImage"} {
			delete(local, key)
		}
		values["local"] = local
		values["gatewayImage"] = image
		values["authorityAccess"] = map[string]any{"existingSecret": "location-access"}
	} else {
		raw, err := os.ReadFile("../config/examples/managed/platform.json")
		if err != nil {
			t.Fatal(err)
		}
		var platform map[string]any
		if err = json.Unmarshal(raw, &platform); err != nil {
			t.Fatal(err)
		}
		delete(platform, "registryNamespace")
		values["platform"] = platform
		values["projects"] = []any{map[string]any{"namespace": "project-demo", "subjects": []any{map[string]any{"kind": "Group", "name": "network-users", "apiGroup": "rbac.authorization.k8s.io"}}}}
	}
	return values
}

func helmRender(t *testing.T, chart string, values map[string]any, wantError bool) []unstructured.Unstructured {
	return helmRenderRelease(t, "demo", chart, values, wantError)
}

func helmRenderRelease(t *testing.T, release, chart string, values map[string]any, wantError bool) []unstructured.Unstructured {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for chart validation")
	}
	raw, _ := json.Marshal(values)
	path := filepath.Join(t.TempDir(), "values.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(helm, "template", release, chart, "--namespace", "custom-system", "--include-crds", "--values", path)
	out, err := cmd.CombinedOutput()
	if wantError {
		if err == nil {
			t.Fatal("invalid chart values unexpectedly rendered")
		}
		return nil
	}
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	lint := exec.Command(helm, "lint", chart, "--values", path, "--strict")
	if out, err := lint.CombinedOutput(); err != nil {
		t.Fatalf("helm lint: %v\n%s", err, out)
	}
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	var objects []unstructured.Unstructured
	for {
		var obj unstructured.Unstructured
		err := decoder.Decode(&obj)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if obj.GetKind() != "" {
			objects = append(objects, obj)
		}
	}
	return objects
}

func TestChartsRenderPinnedImagesScopedRBACAndLifecycleHooks(t *testing.T) {
	for _, chart := range []string{"global-vpc", "global-vpc-site"} {
		t.Run(chart, func(t *testing.T) {
			site := chart == "global-vpc-site"
			objects := helmRender(t, chart, chartValues(t, site), false)
			deployments, hooks, crds, configs := 0, 0, 0, 0
			for _, obj := range objects {
				if obj.GetKind() == "Deployment" || obj.GetKind() == "Job" {
					selector, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "template", "spec", "nodeSelector")
					if selector["kubernetes.io/os"] != "linux" || selector["kubernetes.io/arch"] != "amd64" {
						t.Fatal("released OCI images must only schedule on supported Linux/amd64 nodes")
					}
				}
				if obj.GetKind() == "Namespace" && obj.GetName() == "custom-system" {
					t.Fatal("chart must not adopt release namespace")
				}
				if obj.GetKind() == "CustomResourceDefinition" {
					crds++
				}
				if obj.GetKind() == "Deployment" {
					deployments++
					containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
					if len(containers) != 1 || !strings.Contains(containers[0].(map[string]any)["image"].(string), "@sha256:") {
						t.Fatal("controller image is not immutable")
					}
					volumes, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "volumes")
					for _, value := range volumes {
						volume := value.(map[string]any)
						if cm, ok := volume["configMap"].(map[string]any); ok {
							if !strings.HasSuffix(cm["name"].(string), "-config") {
								t.Fatal("config reference lost release scope")
							}
						}
						if secret, ok := volume["secret"].(map[string]any); ok && secret["secretName"] != "location-access" {
							t.Fatal("external access secret was not mounted")
						}
					}
				}
				if obj.GetKind() == "Job" {
					hooks++
					args, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
					argv := args[0].(map[string]any)["args"].([]any)
					flat, _ := json.Marshal(argv)
					if !bytes.Contains(flat, []byte("platform-hook")) || (!bytes.Contains(flat, []byte("check-empty")) && !bytes.Contains(flat, []byte("verify")) && !bytes.Contains(flat, []byte("check-config"))) {
						t.Fatal("lifecycle hook is not a read-only check")
					}
				}
				if obj.GetKind() == "ConfigMap" {
					configs++
					data, _, _ := unstructured.NestedStringMap(obj.Object, "data")
					for _, raw := range data {
						var config map[string]any
						if err := json.Unmarshal([]byte(raw), &config); err != nil {
							t.Fatal(err)
						}
						if site {
							var typed localconfig.Config
							if err := json.Unmarshal([]byte(raw), &typed); err != nil {
								t.Fatal(err)
							}
							if err := typed.Validate(); err != nil {
								t.Fatal(err)
							}
							if config["namespace"] != "custom-system" || config["authorityKubeconfig"] != "/authority/kubeconfig" || !strings.Contains(config["gatewayImage"].(string), "@sha256:") {
								t.Fatal("site config did not derive release namespace/image/access")
							}
							if typed.GatewayNodeSelector["kubernetes.io/os"] != "linux" || typed.GatewayNodeSelector["kubernetes.io/arch"] != "amd64" {
								t.Fatal("gateway image must only schedule on supported Linux/amd64 nodes")
							}
						} else {
							var typed platformconfig.Config
							if err := json.Unmarshal([]byte(raw), &typed); err != nil {
								t.Fatal(err)
							}
							if err := typed.Validate(); err != nil {
								t.Fatal(err)
							}
							if config["registryNamespace"] != "custom-system" {
								t.Fatal("authority registry namespace did not follow release")
							}
						}
					}
				}
				if obj.GetKind() == "ClusterRole" && strings.HasSuffix(obj.GetName(), "-project-editor") {
					rules, _, _ := unstructured.NestedSlice(obj.Object, "rules")
					raw, _ := json.Marshal(rules)
					if bytes.Contains(raw, []byte("networkbindings")) || bytes.Contains(raw, []byte("status")) {
						t.Fatal("project role exposes internal/status API")
					}
				}
			}
			expectedCRDs := 3
			if site {
				expectedCRDs = 1
			}
			if deployments != 1 || hooks != 3 || configs != 2 || crds != expectedCRDs {
				t.Fatalf("unexpected rendered inventory deployment=%d hooks=%d configs=%d crds=%d", deployments, hooks, configs, crds)
			}
		})
	}
}

func TestChartsRejectMutableImagesAndAmbiguousCredentials(t *testing.T) {
	v := chartValues(t, false)
	v["image"] = map[string]any{"repository": "example.invalid/controller", "digest": "latest"}
	helmRender(t, "global-vpc", v, true)
	v = chartValues(t, true)
	v["authorityAccess"] = map[string]any{"existingSecret": "access", "kubeconfig": "private"}
	helmRender(t, "global-vpc-site", v, true)
	v = chartValues(t, true)
	v["authorityAccess"] = map[string]any{}
	helmRender(t, "global-vpc-site", v, true)
	v = chartValues(t, false)
	v["platform"].(map[string]any)["registryNamespace"] = "wrong-namespace"
	helmRender(t, "global-vpc", v, true)
}

func TestSiteCredentialRotationChangesTemplateChecksum(t *testing.T) {
	checksums := []string{}
	for _, fixture := range []string{"test-kubeconfig-one", "test-kubeconfig-two"} {
		values := chartValues(t, true)
		values["authorityAccess"] = map[string]any{"kubeconfig": fixture}
		objects := helmRender(t, "global-vpc-site", values, false)
		secrets := 0
		for _, obj := range objects {
			if obj.GetKind() == "Secret" {
				secrets++
			}
			if obj.GetKind() == "Deployment" {
				annotations, _, err := unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "annotations")
				if err != nil {
					t.Fatal(err)
				}
				checksums = append(checksums, annotations["checksum/authority-access"])
			}
		}
		if secrets != 1 {
			t.Fatal("inline credentials require one chart-owned Secret")
		}
	}
	if len(checksums) != 2 || checksums[0] == "" || checksums[0] == checksums[1] {
		t.Fatal("credential rotation did not change Pod template checksum")
	}
}

func TestPreDeleteOverridePreservesUpgradeAndVerificationGuards(t *testing.T) {
	for _, chart := range []string{"global-vpc", "global-vpc-site"} {
		values := chartValues(t, chart == "global-vpc-site")
		values["lifecycle"] = map[string]any{"preDeleteCheck": false}
		objects := helmRender(t, chart, values, false)
		hooks := map[string]bool{}
		for _, obj := range objects {
			if obj.GetKind() == "Job" {
				hooks[obj.GetAnnotations()["helm.sh/hook"]] = true
			}
		}
		if len(hooks) != 2 || !hooks["test"] || !hooks["pre-upgrade,pre-rollback"] || hooks["pre-delete"] {
			t.Fatal("recovery override removed unrelated read-only guards")
		}
	}
}

func TestBundledCRDsMatchCanonicalDefinitions(t *testing.T) {
	for _, chart := range []string{"global-vpc", "global-vpc-site"} {
		paths, err := filepath.Glob(filepath.Join(chart, "crds", "*.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join("..", "config", "crd", filepath.Base(path)))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%s drifted from canonical CRD", path)
			}
		}
	}
}

func TestControllerChartsHaveClusterWideSingleWriterGuards(t *testing.T) {
	for _, chart := range []string{"global-vpc", "global-vpc-site"} {
		guard := "global-vpc-authority"
		if chart == "global-vpc-site" {
			guard = "global-vpc-site"
		}
		for _, release := range []string{"first-release", "another-release"} {
			objects := helmRenderRelease(t, release, chart, chartValues(t, chart == "global-vpc-site"), false)
			roles, bindings := 0, 0
			for _, object := range objects {
				if object.GetName() != guard {
					continue
				}
				if object.GetKind() == "ClusterRole" {
					roles++
					if object.GetAnnotations()["helm.sh/hook"] != "" {
						t.Fatal("single-writer guard must be a normal Helm resource before hooks run")
					}
				}
				if object.GetKind() == "ClusterRoleBinding" {
					bindings++
					role, _, _ := unstructured.NestedString(object.Object, "roleRef", "name")
					if role != guard {
						t.Fatal("controller binding does not use the cluster-wide guard role")
					}
				}
			}
			if roles != 1 || bindings != 1 {
				t.Fatalf("%s release %s bypassed the cluster-wide guard: roles=%d bindings=%d", chart, release, roles, bindings)
			}
		}
	}
}
