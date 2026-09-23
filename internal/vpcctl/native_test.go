package vpcctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"globalvpc.io/controller/integration/kube-ovn/destinationroute"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"
)

func nativeTestBundle() NativeBundle {
	return NativeBundle{APIVersion: nativeAPIVersion, Kind: "NativeBundle", Target: "v1.16.3", SourceCommit: "98af25ffae49193a8dc16bbc39bd8ca4110ec367", PatchSHA256: nativePatchSHA256, Platform: "linux/amd64", Image: "registry.example/kube-ovn@sha256:" + strings.Repeat("b", 64), BaseImage: "registry.example/kube-ovn@sha256:" + strings.Repeat("a", 64), Schema: nativeSchema()}
}

func nativeTestObjects() (*corev1.Namespace, *unstructured.Unstructured, *appsv1.Deployment) {
	crd := nativeCRD()
	crd.SetUID("crd-uid")
	crd.SetResourceVersion("1")
	crd.Object["spec"] = map[string]any{"group": "kubeovn.io", "scope": "Cluster", "versions": []any{map[string]any{"name": "v1", "served": true, "storage": true, "schema": map[string]any{"openAPIV3Schema": map[string]any{"type": "object", "properties": map[string]any{
		"spec":   map[string]any{"type": "object", "properties": map[string]any{"bfdPort": map[string]any{"type": "object"}, "unrelated": map[string]any{"type": "string"}}},
		"status": map[string]any{"type": "object", "properties": map[string]any{"router": map[string]any{"type": "string"}}},
	}}}}}}
	n := int32(1)
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "kube-ovn-controller", Namespace: "kube-system", UID: "deployment-uid", ResourceVersion: "1", Generation: 1}, Spec: appsv1.DeploymentSpec{Replicas: &n, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "native"}}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "native"}}, Spec: corev1.PodSpec{ServiceAccountName: "existing-native", Containers: []corev1.Container{{Name: "sidecar", Image: "registry.example/sidecar:v1"}, {Name: "kube-ovn-controller", Image: nativeTestBundle().BaseImage, Command: []string{"/kube-ovn/start-controller.sh"}, Args: []string{"--existing=true"}, Env: []corev1.EnvVar{{Name: "EXISTING", Value: "retained"}}}}}}}, Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}}
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "cluster-uid"}}, crd, d
}

func nativeTestClient(t *testing.T, objects ...client.Object) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&appsv1.Deployment{}, &corev1.Pod{}).WithObjects(objects...).Build()
	// Unlike a real API server, the fake client returns an empty object for a
	// dry-run patch. Simulate the server response without persisting mutation.
	return interceptor.NewClient(base, interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
		po := &client.PatchOptions{}
		for _, opt := range opts {
			opt.ApplyToPatch(po)
		}
		if len(po.DryRun) == 0 {
			return c.Patch(ctx, obj, p, opts...)
		}
		old := obj.DeepCopyObject().(client.Object)
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), old); err != nil {
			return err
		}
		data, err := json.Marshal(old)
		if err != nil {
			return err
		}
		patchData, err := p.Data(obj)
		if err != nil {
			return err
		}
		decoded, err := jsonpatch.DecodePatch(patchData)
		if err != nil {
			return err
		}
		updated, err := decoded.Apply(data)
		if err != nil {
			return err
		}
		return json.Unmarshal(updated, obj)
	}})
}

func nativeTestApp(c client.Client) (*App, *bytes.Buffer) {
	out := new(bytes.Buffer)
	return &App{Out: out, ErrOut: new(bytes.Buffer), ClientFactory: func(Options) (client.Client, error) { return c, nil }}, out
}

func nativeTestPlan(t *testing.T, c client.Client) nativePlan {
	t.Helper()
	p, err := makeNativePlan(context.Background(), c, "infra-a", "kube-system", "kube-ovn-controller", "kube-ovn-controller", nativeTestBundle())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNativePlanPrivateAndReadOnly(t *testing.T) {
	ns, crd, d := nativeTestObjects()
	base := nativeTestClient(t, ns, crd, d)
	c := interceptor.NewClient(base, interceptor.Funcs{Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
		t.Fatal("plan must not patch")
		return nil
	}})
	a, _ := nativeTestApp(c)
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundle.json")
	planPath := filepath.Join(dir, "plan.json")
	if err := nativeWritePrivateJSON(bundlePath, nativeTestBundle()); err != nil {
		t.Fatal(err)
	}
	if err := a.runNative(context.Background(), []string{"plan", "--bundle", bundlePath, "--out", planPath}, Options{Context: "infra-a"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("plan mode %o", info.Mode().Perm())
	}
	var p nativePlan
	if err := nativeReadJSON(planPath, &p); err != nil {
		t.Fatal(err)
	}
	if p.ClusterUID != ns.UID || len(p.CRDPatch) != 4 || len(p.DeploymentPatch) != 4 || p.DeploymentPatch[3].Path != "/spec/template/spec/containers/1/image" {
		t.Fatalf("incorrect narrow plan: %+v", p)
	}
	if err := nativeWritePrivateJSON(planPath, p); err == nil {
		t.Fatal("must refuse overwriting existing review artifact")
	}
}

func TestNativePatchesPreserveForeignFieldsAndGuardConcurrentChanges(t *testing.T) {
	ns, crd, d := nativeTestObjects()
	c := nativeTestClient(t, ns, crd, d)
	p := nativeTestPlan(t, c)
	crdData, _ := json.Marshal(p.CRDPatch)
	deploymentData, _ := json.Marshal(p.DeploymentPatch)
	if err := c.Patch(context.Background(), nativeCRD(), client.RawPatch(types.JSONPatchType, crdData)); err != nil {
		t.Fatal(err)
	}
	if err := c.Patch(context.Background(), &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: d.Name, Namespace: d.Namespace}}, client.RawPatch(types.JSONPatchType, deploymentData)); err != nil {
		t.Fatal(err)
	}
	afterCRD := nativeCRD()
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(afterCRD), afterCRD); err != nil {
		t.Fatal(err)
	}
	props, err := nativeCRDProperties(afterCRD)
	if err != nil {
		t.Fatal(err)
	}
	foreign, _, _ := unstructured.NestedString(props, "spec", "properties", "unrelated", "type")
	if foreign != "string" {
		t.Fatal("foreign schema field changed")
	}
	var afterD appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(d), &afterD); err != nil {
		t.Fatal(err)
	}
	expected := d.DeepCopy()
	expected.Spec.Template.Spec.Containers[1].Image = p.Bundle.Image
	if !nativeJSONEqual(expected.Spec, afterD.Spec) {
		t.Fatalf("Deployment changed beyond image: %+v", afterD.Spec)
	}
	if err := c.Patch(context.Background(), &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: d.Name, Namespace: d.Namespace}}, client.RawPatch(types.JSONPatchType, deploymentData)); err == nil {
		t.Fatal("stale JSON patch must fail atomically")
	}
}

func TestNativePlanRejectsUnsafeInputs(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*unstructured.Unstructured, *appsv1.Deployment) []client.Object
		want   string
	}{
		{"active intent", func(_ *unstructured.Unstructured, _ *appsv1.Deployment) []client.Object {
			v := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubeovn.io/v1", "kind": "Vpc", "metadata": map[string]any{"name": "active"}, "spec": map[string]any{"destinationRoutes": []any{map[string]any{"cidr": "10.2.0.0/24"}}}}}
			return []client.Object{v}
		}, "active destinationRoutes"},
		{"existing schema", func(crd *unstructured.Unstructured, _ *appsv1.Deployment) []client.Object {
			versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
			v := versions[0].(map[string]any)
			_ = unstructured.SetNestedMap(v, map[string]any{"type": "array"}, "schema", "openAPIV3Schema", "properties", "spec", "properties", "destinationRoutes")
			_ = unstructured.SetNestedSlice(crd.Object, versions, "spec", "versions")
			return nil
		}, "already exists"},
		{"hidden binary", func(_ *unstructured.Unstructured, d *appsv1.Deployment) []client.Object {
			d.Spec.Template.Spec.Containers[1].VolumeMounts = []corev1.VolumeMount{{Name: "binary", MountPath: "/kube-ovn/kube-ovn-controller"}}
			return nil
		}, "hides"},
		{"unknown layout", func(_ *unstructured.Unstructured, d *appsv1.Deployment) []client.Object {
			d.Spec.Template.Spec.Containers[1].Command = []string{"/vendor/start"}
			return nil
		}, "unsupported native container command"},
		{"zero replicas", func(_ *unstructured.Unstructured, d *appsv1.Deployment) []client.Object {
			zero := int32(0)
			d.Spec.Replicas = &zero
			return nil
		}, "positive explicit replicas"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ns, crd, d := nativeTestObjects()
			objects := []client.Object{ns, crd, d}
			objects = append(objects, test.mutate(crd, d)...)
			c := nativeTestClient(t, objects...)
			_, err := makeNativePlan(context.Background(), c, "infra-a", d.Namespace, d.Name, "kube-ovn-controller", nativeTestBundle())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("want %q, got %v", test.want, err)
			}
		})
	}
}

func TestNativeInstallRefusesStaleOrWrongClusterWithoutWrites(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*corev1.Namespace, *unstructured.Unstructured, *appsv1.Deployment)
		want   string
	}{
		{"wrong cluster", func(ns *corev1.Namespace, _ *unstructured.Unstructured, _ *appsv1.Deployment) {
			ns.UID = "other-cluster"
		}, "wrong cluster"},
		{"changed deployment UID", func(_ *corev1.Namespace, _ *unstructured.Unstructured, d *appsv1.Deployment) { d.UID = "new-uid" }, "stale plan"},
		{"changed deployment RV", func(_ *corev1.Namespace, _ *unstructured.Unstructured, d *appsv1.Deployment) {
			d.ResourceVersion = "99"
		}, "stale plan"},
		{"changed CRD RV", func(_ *corev1.Namespace, crd *unstructured.Unstructured, _ *appsv1.Deployment) {
			crd.SetResourceVersion("99")
		}, "stale plan"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ns, crd, d := nativeTestObjects()
			p := nativeTestPlan(t, nativeTestClient(t, ns, crd, d))
			test.mutate(ns, crd, d)
			base := nativeTestClient(t, ns, crd, d)
			c := interceptor.NewClient(base, interceptor.Funcs{Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				t.Fatal("stale plan must not patch")
				return nil
			}})
			a, _ := nativeTestApp(c)
			err := a.installNativePlan(context.Background(), c, p, Options{Context: "infra-a", Timeout: time.Millisecond}, false)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("want %q, got %v", test.want, err)
			}
		})
	}
}

func TestNativeInstallPartialFailureKeepsOriginalDeployment(t *testing.T) {
	ns, crd, d := nativeTestObjects()
	base := nativeTestClient(t, ns, crd, d)
	p := nativeTestPlan(t, base)
	c := interceptor.NewClient(base, interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		po := &client.PatchOptions{}
		for _, opt := range opts {
			opt.ApplyToPatch(po)
		}
		if _, ok := obj.(*appsv1.Deployment); ok && len(po.DryRun) == 0 {
			return errors.New("injected image patch rejection")
		}
		return c.Patch(ctx, obj, patch, opts...)
	}})
	a, _ := nativeTestApp(c)
	err := a.installNativePlan(context.Background(), c, p, Options{Context: "infra-a"}, false)
	if err == nil || !strings.Contains(err.Error(), "PARTIAL INSTALL: schema applied") || !strings.Contains(err.Error(), "no automatic rollback") {
		t.Fatalf("partial state must be explicit: %v", err)
	}
	if err := base.Get(context.Background(), client.ObjectKeyFromObject(d), d); err != nil {
		t.Fatal(err)
	}
	if d.Spec.Template.Spec.Containers[1].Image != p.Bundle.BaseImage {
		t.Fatal("failed image patch changed original image")
	}
	if err := base.Get(context.Background(), client.ObjectKeyFromObject(crd), crd); err != nil {
		t.Fatal(err)
	}
	props, _ := nativeCRDProperties(crd)
	if _, exists, _ := unstructured.NestedMap(props, "spec", "properties", "destinationRoutes"); !exists {
		t.Fatal("partial schema addition should remain for inspection")
	}
}

func TestNativeInstallOwnershipAndMutableImageRefusal(t *testing.T) {
	for _, managed := range []bool{true, false} {
		ns, crd, d := nativeTestObjects()
		want := "immutable"
		if managed {
			d.Labels = map[string]string{"app.kubernetes.io/managed-by": "Helm"}
			want = "owner-managed"
		} else {
			d.Spec.Template.Spec.Containers[1].Image = "registry.example/kube-ovn:v1.16.3"
			want = "mutable"
		}
		c := nativeTestClient(t, ns, crd, d)
		p := nativeTestPlan(t, c)
		a, _ := nativeTestApp(c)
		err := a.installNativePlan(context.Background(), c, p, Options{}, false)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("want %s refusal, got %v", want, err)
		}
	}
}

func TestNativePlanTamperAndBundleValidation(t *testing.T) {
	ns, crd, d := nativeTestObjects()
	p := nativeTestPlan(t, nativeTestClient(t, ns, crd, d))
	p.DeploymentPatch = append(p.DeploymentPatch, nativePatchOperation{"replace", "/spec/replicas", 0})
	if err := validateNativePlan(p); err == nil {
		t.Fatal("plan with extra mutation must be rejected")
	}
	b := nativeTestBundle()
	b.Schema.Spec["maxItems"] = 999
	if err := validateNativeBundle(b); err == nil {
		t.Fatal("changed schema must be rejected")
	}
	b = nativeTestBundle()
	b.Image = "registry.example/kube-ovn:latest"
	if err := validateNativeBundle(b); err == nil {
		t.Fatal("mutable bundle image must be rejected")
	}
}

func TestNativeACKCanonicalHashAndGeneration(t *testing.T) {
	routes := []destinationroute.Intent{{CIDR: "10.2.0.0/24", NextHops: []string{"10.1.0.3", "10.1.0.2"}, BFD: destinationroute.BFD{MinRX: 300, MinTX: 300, Multiplier: 3}, SelectionFields: []string{"ip_dst", "ip_src"}}}
	compiled, err := destinationroute.Compile(routes)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(routes)
	var raw []any
	_ = json.Unmarshal(data, &raw)
	v := unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubeovn.io/v1", "kind": "Vpc", "metadata": map[string]any{"name": "test", "generation": int64(3)}, "spec": map[string]any{"destinationRoutes": raw}, "status": map[string]any{"destinationRoutes": map[string]any{"capability": destinationroute.Capability, "ready": true, "observedGeneration": int64(3), "appliedHash": compiled.Hash}}}}
	if r := nativeACK(v); r.State != "acknowledged" {
		t.Fatalf("canonical acknowledgement rejected: %+v", r)
	}
	_ = unstructured.SetNestedField(v.Object, int64(2), "status", "destinationRoutes", "observedGeneration")
	if r := nativeACK(v); r.State != "mismatch" || len(r.Problems) != 1 {
		t.Fatalf("stale generation acknowledged: %+v", r)
	}
	_ = unstructured.SetNestedField(v.Object, int64(3), "status", "destinationRoutes", "observedGeneration")
	_ = unstructured.SetNestedField(v.Object, "wrong", "status", "destinationRoutes", "appliedHash")
	if r := nativeACK(v); r.State != "mismatch" {
		t.Fatalf("wrong hash acknowledged: %+v", r)
	}
}

func TestNativeRolloutRequiresOwnedReadyPods(t *testing.T) {
	ns, crd, d := nativeTestObjects()
	controller := true
	set := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "native-rs", Namespace: d.Namespace, UID: "rs-uid", Labels: map[string]string{"app": "native"}, OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", UID: d.UID, Controller: &controller}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "native-pod", Namespace: d.Namespace, Labels: map[string]string{"app": "native"}, OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", UID: set.UID, Controller: &controller}}}, Spec: d.Spec.Template.Spec, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "kube-ovn-controller", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	c := nativeTestClient(t, ns, crd, d, set, pod)
	ready, reason, err := nativeRolloutReady(context.Background(), c, d, "kube-ovn-controller", nativeTestBundle().BaseImage)
	if err != nil || !ready {
		t.Fatalf("expected ready: %s %v", reason, err)
	}
	pod.OwnerReferences[0].UID = "foreign-rs"
	if err := c.Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	ready, reason, err = nativeRolloutReady(context.Background(), c, d, "kube-ovn-controller", nativeTestBundle().BaseImage)
	if err != nil || ready || !strings.Contains(reason, "owner chain") {
		t.Fatalf("foreign Pod accepted: %s %v", reason, err)
	}
}

func nativeTestReadyObjects(d *appsv1.Deployment) (*appsv1.ReplicaSet, *corev1.Pod) {
	controller := true
	set := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "native-rs", Namespace: d.Namespace, UID: "rs-uid", Labels: map[string]string{"app": "native"}, OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", UID: d.UID, Controller: &controller}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "native-pod", Namespace: d.Namespace, Labels: map[string]string{"app": "native"}, OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", UID: set.UID, Controller: &controller}}}, Spec: d.Spec.Template.Spec, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "kube-ovn-controller", ImageID: "docker-pullable://" + nativeTestBundle().BaseImage, Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	return set, pod
}

func TestNativeInstallResumesOnlyExactSamePlan(t *testing.T) {
	ns, crd, d := nativeTestObjects()
	c := nativeTestClient(t, ns, crd, d)
	p := nativeTestPlan(t, c)
	patch, _ := json.Marshal(p.CRDPatch)
	if err := c.Patch(context.Background(), nativeCRD(), client.RawPatch(types.JSONPatchType, patch)); err != nil {
		t.Fatal(err)
	}
	// A status/metadata resourceVersion change after schema installation is
	// harmless on resume; the original template and resource identity must match.
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(d), d); err != nil {
		t.Fatal(err)
	}
	d.Annotations = map[string]string{"unrelated": "preserve"}
	if err := c.Update(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	a, out := nativeTestApp(c)
	err := a.installNativePlan(context.Background(), c, p, Options{Context: "infra-a", Timeout: time.Millisecond}, false)
	if err == nil || !strings.Contains(err.Error(), "rollout is unverified") {
		t.Fatalf("resume should reach rollout: %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(d), d); err != nil {
		t.Fatal(err)
	}
	if d.Spec.Template.Spec.Containers[1].Image != p.Bundle.Image || d.Annotations["unrelated"] != "preserve" {
		t.Fatal("resume image patch was not narrow")
	}
	set, pod := nativeTestReadyObjects(d)
	if err := c.Create(context.Background(), set); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	noMorePatches := interceptor.NewClient(c, interceptor.Funcs{Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
		t.Fatal("completed plan must only verify rollout")
		return nil
	}})
	if err := a.installNativePlan(context.Background(), noMorePatches, p, Options{Context: "infra-a", Timeout: time.Second}, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Resuming the same plan") || !strings.Contains(out.String(), "rollout complete") {
		t.Fatalf("resume not explained: %s", out.String())
	}
	// Even with the target schema installed, foreign template changes cannot be adopted.
	d.Spec.Template.Spec.Containers[1].Args = append(d.Spec.Template.Spec.Containers[1].Args, "--foreign-change")
	if err := c.Update(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if err := a.installNativePlan(context.Background(), c, p, Options{}, false); err == nil || !strings.Contains(err.Error(), "template differs") {
		t.Fatalf("resumed foreign template: %v", err)
	}
}

func TestNativeKnownUpstreamLayoutAndBaselineSchema(t *testing.T) {
	_, crd, d := nativeTestObjects()
	d.Spec.Template.Spec.Containers[1].Command = nil
	d.Spec.Template.Spec.Containers[1].Args = []string{"/kube-ovn/start-controller.sh", "--existing=true"}
	if _, err := validateNativeDeployment(d, "kube-ovn-controller"); err != nil {
		t.Fatalf("normal upstream args-only layout rejected: %v", err)
	}
	if err := unstructured.SetNestedField(crd.Object, "Webhook", "spec", "conversion", "strategy"); err != nil {
		t.Fatal(err)
	}
	if _, err := nativeCRDProperties(crd); err == nil {
		t.Fatal("conversion webhook must not be accepted")
	}
	unstructured.RemoveNestedField(crd.Object, "spec", "conversion")
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	unstructured.RemoveNestedField(versions[0].(map[string]any), "schema", "openAPIV3Schema", "properties", "spec", "properties", "bfdPort")
	_ = unstructured.SetNestedSlice(crd.Object, versions, "spec", "versions")
	if _, err := nativeCRDProperties(crd); err == nil {
		t.Fatal("missing baseline bfdPort must not be accepted")
	}
}

func TestNativeMutableImageRequiresRuntimeDigestEvidence(t *testing.T) {
	ns, crd, d := nativeTestObjects()
	d.Spec.Template.Spec.Containers[1].Image = "registry.example/kube-ovn:v1.16.3"
	set, pod := nativeTestReadyObjects(d)
	c := nativeTestClient(t, ns, crd, d, set, pod)
	p := nativeTestPlan(t, c)
	if len(p.InstallBlockers) != 0 || len(p.OriginalRuntimeImages) != 1 {
		t.Fatalf("matching runtime evidence rejected: %+v", p.InstallBlockers)
	}
	pod.Status.ContainerStatuses[0].ImageID = "containerd://sha256:" + strings.Repeat("c", 64)
	if err := c.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	p = nativeTestPlan(t, c)
	if len(p.InstallBlockers) == 0 || !strings.Contains(p.InstallBlockers[0], "index digest") {
		t.Fatalf("unproven platform digest accepted: %+v", p)
	}
	d.Spec.Template.Spec.Containers[1].Image = "registry.example/kube-ovn:v1.16.3@sha256:" + strings.Repeat("a", 64)
	blockers, _ := nativeImagePreflight(context.Background(), c, d, "kube-ovn-controller", nativeTestBundle())
	if len(blockers) != 0 {
		t.Fatalf("equivalent tag@digest refused: %v", blockers)
	}
}

func TestNativeRolloutRejectsSameImageTemplateDrift(t *testing.T) {
	ns, crd, d := nativeTestObjects()
	expected := *d.Spec.Template.DeepCopy()
	d.Spec.Template.Spec.Containers[1].VolumeMounts = []corev1.VolumeMount{{Name: "foreign", MountPath: "/kube-ovn/kube-ovn-controller"}}
	c := nativeTestClient(t, ns, crd, d)
	err := nativeWaitRollout(context.Background(), c, d.Namespace, d.Name, "kube-ovn-controller", d.UID, nativeTestBundle().BaseImage, expected)
	if err == nil || !strings.Contains(err.Error(), "template drifted") {
		t.Fatalf("same-image template drift ignored: %v", err)
	}
}

func TestNativeStatusYAML(t *testing.T) {
	ns, crd, d := nativeTestObjects()
	set, pod := nativeTestReadyObjects(d)
	d.Status.ReadyReplicas = 0
	c := nativeTestClient(t, ns, crd, d, set, pod)
	a, out := nativeTestApp(c)
	if err := a.nativeStatus(context.Background(), c, d.Namespace, d.Name, "kube-ovn-controller", Options{Output: "yaml"}); err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := yaml.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report["schema"] != "absent" || report["clusterUID"] != "cluster-uid" {
		t.Fatalf("invalid YAML report: %s", out.String())
	}
	if report["rolloutReady"] != false || !strings.Contains(out.String(), "docker-pullable://"+nativeTestBundle().BaseImage) {
		t.Fatalf("incomplete rollout must still expose owned Pod runtime identity: %s", out.String())
	}
}

func TestNativeMutableImageUsesSingleVerifiedPodSnapshot(t *testing.T) {
	ns, crd, d := nativeTestObjects()
	d.Spec.Template.Spec.Containers[1].Image = "registry.example/kube-ovn:v1.16.3"
	set, pod := nativeTestReadyObjects(d)
	base := nativeTestClient(t, ns, crd, d, set, pod)
	podLists := 0
	c := interceptor.NewClient(base, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if pods, ok := list.(*corev1.PodList); ok {
			podLists++
			if podLists > 1 {
				pods.Items = nil
				return nil
			}
		}
		return c.List(ctx, list, opts...)
	}})
	blockers, ids := nativeImagePreflight(context.Background(), c, d, "kube-ovn-controller", nativeTestBundle())
	if len(blockers) != 0 || len(ids) != 1 || podLists != 1 {
		t.Fatalf("runtime digest proof must use exactly the owned Ready Pod snapshot: lists=%d ids=%v blockers=%v", podLists, ids, blockers)
	}
}
