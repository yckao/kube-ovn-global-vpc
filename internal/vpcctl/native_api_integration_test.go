//go:build integration

package vpcctl

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestNativeCLIAPI validates actual structural-schema admission, server defaulting,
// JSON Patch guards and resumable installation against an isolated API server.
// No native controller, scheduler or dataplane is running in this test.
func TestNativeCLIAPI(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS must reference installed envtest binaries")
	}
	environment := &envtest.Environment{ControlPlaneStartTimeout: 45 * time.Second}
	cfg, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ns, crd, deployment := nativeTestObjects()
	ns.UID = ""
	ns.ResourceVersion = ""
	if err := c.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	crd.SetUID("")
	crd.SetResourceVersion("")
	_ = unstructured.SetNestedMap(crd.Object, map[string]any{"plural": "vpcs", "singular": "vpc", "kind": "Vpc", "listKind": "VpcList"}, "spec", "names")
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	versions[0].(map[string]any)["subresources"] = map[string]any{"status": map[string]any{}}
	_ = unstructured.SetNestedSlice(crd.Object, versions, "spec", "versions")
	if err := c.Create(ctx, crd); err != nil {
		t.Fatal(err)
	}
	if err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, client.ObjectKeyFromObject(crd), crd); err != nil {
			return false, err
		}
		conditions, _, _ := unstructured.NestedSlice(crd.Object, "status", "conditions")
		for _, raw := range conditions {
			condition := raw.(map[string]any)
			if condition["type"] == "Established" && condition["status"] == "True" {
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	deployment.UID = ""
	deployment.ResourceVersion = ""
	deployment.Generation = 0
	deployment.Status = appsv1.DeploymentStatus{}
	deployment.Spec.Template.Spec.Containers[1].Command = nil
	deployment.Spec.Template.Spec.Containers[1].Args = []string{"/kube-ovn/start-controller.sh", "--existing=true"}
	if err := c.Create(ctx, deployment); err != nil {
		t.Fatal(err)
	}
	p, err := makeNativePlan(ctx, c, "envtest", deployment.Namespace, deployment.Name, "kube-ovn-controller", nativeTestBundle())
	if err != nil {
		t.Fatal(err)
	}
	if err := validateNativePlan(p); err != nil {
		t.Fatal(err)
	}
	crdData, _ := json.Marshal(p.CRDPatch)
	deploymentData, _ := json.Marshal(p.DeploymentPatch)
	dryCRD := nativeCRD()
	if err := c.Patch(ctx, dryCRD, client.RawPatch(types.JSONPatchType, crdData), client.DryRunAll); err != nil {
		t.Fatal("structural CRD dry-run:", err)
	}
	dryDeployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: deployment.Name, Namespace: deployment.Namespace}}
	if err := c.Patch(ctx, dryDeployment, client.RawPatch(types.JSONPatchType, deploymentData), client.DryRunAll); err != nil {
		t.Fatal("Deployment dry-run:", err)
	}
	expectedSpec, expectedTemplate := nativeExpectedState(p)
	drySpec, _, _ := unstructured.NestedMap(dryCRD.Object, "spec")
	if !nativeJSONEqual(drySpec, expectedSpec) || !nativeJSONEqual(dryDeployment.Spec.Template, expectedTemplate) {
		t.Fatal("real API dry-run changed fields beyond planned schema/image")
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(crd), crd); err != nil {
		t.Fatal(err)
	}
	currentSpec, _, _ := unstructured.NestedMap(crd.Object, "spec")
	if !nativeJSONEqual(currentSpec, p.CRDSpec) || crd.GetResourceVersion() != p.CRDVersion {
		t.Fatal("CRD dry-run persisted changes")
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(deployment), deployment); err != nil {
		t.Fatal(err)
	}
	if !nativeJSONEqual(deployment.Spec.Template, p.Template) || deployment.ResourceVersion != p.DeploymentVersion {
		t.Fatal("Deployment dry-run persisted changes")
	}
	// Simulate schema success followed by an interrupted image step. The saved
	// plan must resume on a real API despite the expected CRD RV advancement.
	if err := c.Patch(ctx, nativeCRD(), client.RawPatch(types.JSONPatchType, crdData)); err != nil {
		t.Fatal(err)
	}
	a, _ := nativeTestApp(c)
	err = a.installNativePlan(ctx, c, p, Options{Context: "envtest", Timeout: 20 * time.Millisecond}, false)
	if err == nil || !strings.Contains(err.Error(), "PARTIAL INSTALL: schema and image applied, but rollout is unverified") {
		t.Fatalf("real API resume must reach (unavailable) rollout, got %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(deployment), deployment); err != nil {
		t.Fatal(err)
	}
	if !nativeJSONEqual(deployment.Spec.Template, expectedTemplate) {
		t.Fatal("actual image patch changed foreign/defaulted Pod fields")
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(crd), crd); err != nil {
		t.Fatal(err)
	}
	currentSpec, _, _ = unstructured.NestedMap(crd.Object, "spec")
	if !nativeJSONEqual(currentSpec, expectedSpec) {
		t.Fatal("actual schema patch changed foreign fields")
	}
	if err := c.Patch(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: deployment.Name, Namespace: deployment.Namespace}}, client.RawPatch(types.JSONPatchType, deploymentData)); err == nil {
		t.Fatal("server accepted stale Deployment RV/template patch")
	}
	// A second resume verifies existing changes and still reports unverified
	// rollout instead of pretending the fixture has running native replicas.
	err = a.installNativePlan(ctx, c, p, Options{Context: "envtest", Timeout: 20 * time.Millisecond}, false)
	if err == nil || !strings.Contains(err.Error(), "rollout is unverified") {
		t.Fatalf("existing-image resume must preserve the verification boundary: %v", err)
	}
	// Helm recovery state round-trips through an actual Secret, with the typed
	// template and generic JSON patch values remaining semantically identical.
	h := nativeHookOptions{Action: "verify", Release: "native-test", Namespace: "kube-system", Secret: "native-test-state", ControllerNamespace: deployment.Namespace, Deployment: deployment.Name, Container: "kube-ovn-controller", OVN: defaultNativeOVNOptions()}
	state := nativeHookState{APIVersion: nativeAPIVersion, Kind: "NativeHelmState", Release: h.Release, Pending: &p}
	var secret *corev1.Secret
	if err := nativeSaveHookState(ctx, c, h, &secret, state); err != nil {
		t.Fatal(err)
	}
	if secret.UID == "" || secret.ResourceVersion == "" {
		t.Fatal("receipt persisted without server identity")
	}
	_, loaded, err := nativeLoadHookState(ctx, c, h)
	if err != nil || loaded.Pending == nil || loaded.Active != nil {
		t.Fatalf("real Secret did not round-trip guarded pending plan: %v", err)
	}
	if err := a.nativeHook(ctx, c, Options{Timeout: 20 * time.Millisecond}, h); err == nil || !strings.Contains(err.Error(), "completed receipt") {
		t.Fatalf("Helm verify accepted incomplete rollout: %v", err)
	}
	// Rollback cannot use only an image switch: lack of independent OVN cleanup
	// evidence blocks stock restoration before any native mutation.
	h.Action = "uninstall"
	if err := a.nativeHook(ctx, c, Options{Context: "envtest", Timeout: 2 * time.Second}, h); err == nil || !strings.Contains(err.Error(), "OVN") {
		t.Fatalf("uninstall accepted missing OVN evidence: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(deployment), deployment); err != nil || !nativeJSONEqual(deployment.Spec.Template, expectedTemplate) {
		t.Fatalf("blocked uninstall changed the native image: %v", err)
	}
	if _, _, err := nativeLoadHookState(ctx, c, h); err != nil {
		t.Fatalf("blocked uninstall lost its recovery receipt: %v", err)
	}
	// Validate the eventual narrow schema-removal transaction against structural
	// admission without pretending that this API-only fixture proved cleanup.
	if err := c.Get(ctx, client.ObjectKeyFromObject(crd), crd); err != nil {
		t.Fatal(err)
	}
	removal, _ := json.Marshal(nativeSchemaRestorePatch(crd, p.Bundle.Schema))
	dryRestore := nativeCRD()
	if err := c.Patch(ctx, dryRestore, client.RawPatch(types.JSONPatchType, removal), client.DryRunAll); err != nil {
		t.Fatalf("schema restoration dry-run: %v", err)
	}
	dryRestoredSpec, _, _ := unstructured.NestedMap(dryRestore.Object, "spec")
	if !nativeJSONEqual(dryRestoredSpec, p.CRDSpec) {
		t.Fatal("schema restoration dry-run modified unrelated CRD fields")
	}
}
