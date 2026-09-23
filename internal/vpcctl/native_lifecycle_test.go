package vpcctl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"globalvpc.io/controller/integration/kube-ovn/destinationroute"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func nativeLifecycleFixture(t *testing.T) (client.WithWatch, nativePlan) {
	t.Helper()
	ns, crd, deployment := nativeTestObjects()
	c := nativeTestClient(t, ns, crd, deployment)
	p := nativeTestPlan(t, c)
	cp, _ := json.Marshal(p.CRDPatch)
	dp, _ := json.Marshal(p.DeploymentPatch)
	if err := c.Patch(context.Background(), nativeCRD(), client.RawPatch(types.JSONPatchType, cp)); err != nil {
		t.Fatal(err)
	}
	if err := c.Patch(context.Background(), deployment, client.RawPatch(types.JSONPatchType, dp)); err != nil {
		t.Fatal(err)
	}
	set, pod := nativeTestReadyObjects(deployment)
	if err := c.Create(context.Background(), set); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	return c, p
}

func nativeLifecycleACK(t *testing.T, active bool) *unstructured.Unstructured {
	t.Helper()
	intents := []destinationroute.Intent{}
	if active {
		intents = append(intents, destinationroute.Intent{CIDR: "10.2.0.0/24", NextHops: []string{"10.1.0.2", "10.1.0.3"}, BFD: destinationroute.BFD{MinRX: 300, MinTX: 300, Multiplier: 3}, SelectionFields: []string{"ip_src", "ip_dst"}})
	}
	compiled, err := destinationroute.Compile(intents)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(intents)
	var routes []any
	_ = json.Unmarshal(data, &routes)
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubeovn.io/v1", "kind": "Vpc", "metadata": map[string]any{"name": "tenant-native", "uid": "native-vpc-uid", "generation": int64(4)}, "spec": map[string]any{"destinationRoutes": routes}, "status": map[string]any{"destinationRoutes": map[string]any{"capability": destinationroute.Capability, "ready": true, "observedGeneration": int64(4), "appliedHash": compiled.Hash}}}}
}

func nativeLifecycleRolloutClient(t *testing.T, base client.WithWatch) client.WithWatch {
	t.Helper()
	return interceptor.NewClient(base, interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, options ...client.PatchOption) error {
		if err := c.Patch(ctx, obj, patch, options...); err != nil {
			return err
		}
		po := &client.PatchOptions{}
		for _, option := range options {
			option.ApplyToPatch(po)
		}
		if deployment, ok := obj.(*appsv1.Deployment); ok && len(po.DryRun) == 0 {
			var pod corev1.Pod
			if err := c.Get(ctx, client.ObjectKey{Namespace: deployment.Namespace, Name: "native-pod"}, &pod); err != nil {
				return err
			}
			pod.Spec = *deployment.Spec.Template.Spec.DeepCopy()
			if err := c.Update(ctx, &pod); err != nil {
				return err
			}
			deployment.Status.ObservedGeneration = deployment.Generation
			if err := c.Status().Update(ctx, deployment); err != nil {
				return err
			}
		}
		return nil
	}})
}

func nativeLifecycleOVNFixture(t *testing.T, c client.Client) {
	t.Helper()
	_, _, deployment := nativeTestObjects()
	deployment.Name, deployment.UID, deployment.ResourceVersion = "ovn-central", "ovn-uid", ""
	deployment.Spec.Selector.MatchLabels = map[string]string{"app": "ovn"}
	deployment.Spec.Template.Labels = map[string]string{"app": "ovn"}
	deployment.Spec.Template.Spec.Containers = []corev1.Container{{Name: "ovn-central", Image: "registry.example/ovn@sha256:" + strings.Repeat("d", 64)}}
	set, pod := nativeTestReadyObjects(deployment)
	set.Name, set.UID, set.Labels = "ovn-rs", "ovn-rs-uid", map[string]string{"app": "ovn"}
	pod.Name, pod.Labels, pod.OwnerReferences[0].UID = "ovn-pod", map[string]string{"app": "ovn"}, set.UID
	pod.Status.ContainerStatuses[0].Name = "ovn-central"
	for _, object := range []client.Object{deployment, set, pod} {
		if err := c.Create(context.Background(), object); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNativeUpgradeAndRollbackPreserveActiveAcknowledgedRoutes(t *testing.T) {
	base, previous := nativeLifecycleFixture(t)
	vpc := nativeLifecycleACK(t, true)
	if err := base.Create(context.Background(), vpc); err != nil {
		t.Fatal(err)
	}
	bundle := previous.Bundle
	bundle.Image = "registry.example/kube-ovn@sha256:" + strings.Repeat("c", 64)
	c := nativeLifecycleRolloutClient(t, base)
	p, err := makeNativeUpgradePlan(context.Background(), c, Options{Context: "infra-a"}, previous, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.CRDPatch) != 2 || p.Previous == nil {
		t.Fatalf("upgrade should retain the schema: %+v", p.CRDPatch)
	}
	a, _ := nativeTestApp(c)
	opts := Options{Context: "infra-a", Timeout: time.Second}
	if err := a.installNativePlan(context.Background(), c, p, opts, false); err != nil {
		t.Fatal(err)
	}
	if err := a.restoreNative(context.Background(), c, p, opts, defaultNativeOVNOptions(), false, false); err != nil {
		t.Fatal(err)
	}
	if err := a.nativeVerify(context.Background(), c, &previous, previous.Namespace, previous.Deployment, previous.Container, opts); err != nil {
		t.Fatal(err)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(vpc.GroupVersionKind())
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(vpc), got); err != nil {
		t.Fatal(err)
	}
	if !nativeJSONEqual(got.Object["spec"], vpc.Object["spec"]) {
		t.Fatal("native upgrade/rollback changed controller-owned route intent")
	}
}

func TestNativeUpgradeRejectsStaleACKDifferentBaselineAndTamperedChain(t *testing.T) {
	c, previous := nativeLifecycleFixture(t)
	vpc := nativeLifecycleACK(t, true)
	_ = unstructured.SetNestedField(vpc.Object, int64(3), "status", "destinationRoutes", "observedGeneration")
	if err := c.Create(context.Background(), vpc); err != nil {
		t.Fatal(err)
	}
	bundle := previous.Bundle
	bundle.Image = "registry.example/kube-ovn@sha256:" + strings.Repeat("c", 64)
	if _, err := makeNativeUpgradePlan(context.Background(), c, Options{}, previous, bundle); err == nil || !strings.Contains(err.Error(), "ACK") {
		t.Fatalf("stale ACK accepted: %v", err)
	}
	bundle.Target, bundle.SourceCommit = "v1.16.4", "a9296ef2a37c6519bc0ecb798082ce139c72f8eb"
	if _, err := makeNativeUpgradePlan(context.Background(), c, Options{}, previous, bundle); err == nil || !strings.Contains(err.Error(), "full upstream") {
		t.Fatalf("partial upstream baseline upgrade accepted: %v", err)
	}
	if err := c.Delete(context.Background(), vpc); err != nil {
		t.Fatal(err)
	}
	bundle = previous.Bundle
	bundle.Image = "registry.example/kube-ovn@sha256:" + strings.Repeat("c", 64)
	p, err := makeNativeUpgradePlan(context.Background(), c, Options{}, previous, bundle)
	if err != nil {
		t.Fatal(err)
	}
	p.Previous.DeploymentUID = "other-deployment"
	if err := validateNativePlan(p); err == nil {
		t.Fatal("receipt chain identity tampering accepted")
	}
}

func TestNativeUninstallRequiresIndependentOVNEvidenceAndResumes(t *testing.T) {
	base, p := nativeLifecycleFixture(t)
	nativeLifecycleOVNFixture(t, base)
	if err := base.Create(context.Background(), nativeLifecycleACK(t, false)); err != nil {
		t.Fatal(err)
	}
	c := nativeLifecycleRolloutClient(t, base)
	a, out := nativeTestApp(c)
	calls := 0
	blockBFD := true
	a.PodExecutor = func(_ context.Context, _ Options, namespace, pod, container string, argv []string, stdout, _ io.Writer) error {
		calls++
		if namespace != "kube-system" || pod != "ovn-pod" || container != "ovn-central" || argv[0] != "ovn-nbctl" {
			t.Fatalf("unexpected exec boundary: %s %s %s %v", namespace, pod, container, argv)
		}
		rows := `[]`
		if blockBFD && argv[len(argv)-1] == "BFD" {
			rows = `[[["uuid","orphan"],["map",[["kube-ovn.io/destination-vpc-uid","deleted-vpc"]]]]]`
		}
		_, err := fmt.Fprintf(stdout, `{"headings":["_uuid","external_ids"],"data":%s}`, rows)
		return err
	}
	opts := Options{Context: "infra-a", Timeout: 5 * time.Millisecond}
	if err := a.restoreNative(context.Background(), c, p, opts, defaultNativeOVNOptions(), false, true); err == nil || !strings.Contains(err.Error(), "BFD") {
		t.Fatalf("orphan BFD allowed stock image restoration: %v", err)
	}
	var d appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: p.Namespace, Name: p.Deployment}, &d); err != nil {
		t.Fatal(err)
	}
	if d.Spec.Template.Spec.Containers[1].Image != p.Bundle.Image {
		t.Fatal("unverified cleanup changed the native image")
	}
	blockBFD = false
	opts.Timeout = time.Second
	if err := a.restoreNative(context.Background(), c, p, opts, defaultNativeOVNOptions(), false, true); err != nil {
		t.Fatal(err)
	}
	if calls < 6 || !strings.Contains(out.String(), "Stock native image restored") {
		t.Fatalf("missing independent cleanup or completion evidence: calls=%d output=%s", calls, out.String())
	}
	crd := nativeCRD()
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(crd), crd); err != nil {
		t.Fatal(err)
	}
	currentSpec, _, _ := unstructured.NestedMap(crd.Object, "spec")
	if !nativeJSONEqual(currentSpec, p.CRDSpec) {
		t.Fatal("uninstall changed fields beyond the two extension schema properties")
	}
	// A completed uninstall is safe to retry without any new mutation.
	noWrites := interceptor.NewClient(c, interceptor.Funcs{Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
		t.Fatal("completed uninstall must not patch")
		return nil
	}})
	if err := a.restoreNative(context.Background(), noWrites, p, opts, defaultNativeOVNOptions(), false, true); err != nil {
		t.Fatal(err)
	}
}

func TestNativeStockRestoreRejectsActiveIntentStaleACKAndForeignOwnership(t *testing.T) {
	for _, scenario := range []string{"active", "stale-ack", "foreign-owner", "wrong-cluster", "unproven-original"} {
		t.Run(scenario, func(t *testing.T) {
			base, p := nativeLifecycleFixture(t)
			vpc := nativeLifecycleACK(t, scenario == "active")
			if scenario == "stale-ack" {
				_ = unstructured.SetNestedField(vpc.Object, int64(2), "status", "destinationRoutes", "observedGeneration")
			}
			if err := base.Create(context.Background(), vpc); err != nil {
				t.Fatal(err)
			}
			if scenario == "foreign-owner" {
				var d appsv1.Deployment
				if err := base.Get(context.Background(), client.ObjectKey{Namespace: p.Namespace, Name: p.Deployment}, &d); err != nil {
					t.Fatal(err)
				}
				d.Labels = map[string]string{"app.kubernetes.io/managed-by": "Helm"}
				if err := base.Update(context.Background(), &d); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "wrong-cluster" {
				p.ClusterUID = "other-cluster"
			}
			if scenario == "unproven-original" {
				p.OriginalRuntimeImages = nil
			}
			c := interceptor.NewClient(base, interceptor.Funcs{Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				t.Fatal("unsafe preflight must not write")
				return nil
			}})
			a, _ := nativeTestApp(c)
			if err := a.restoreNative(context.Background(), c, p, Options{Timeout: time.Millisecond}, defaultNativeOVNOptions(), false, true); err == nil {
				t.Fatal("unsafe restoration was accepted")
			}
		})
	}
}

func TestNativeOVNRowsFailClosed(t *testing.T) {
	valid := `{"headings":["external_ids"],"data":[[["map",[["unrelated","keep"]]]]]}`
	if err := nativeCheckOVNRows([]byte(valid), "BFD"); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{`{}`, `{"headings":["external_ids"],"data":null}`, `{"headings":["external_ids"],"data":[[]]}`, `{"headings":["external_ids"],"data":[[["set",[]]]]}`, valid + " {}", `{"headings":["external_ids"],"data":[[["map",[["kube-ovn.io/destination-vpc-uid",""]]]]]}`} {
		if err := nativeCheckOVNRows([]byte(input), "BFD"); err == nil {
			t.Fatalf("unsafe OVN evidence accepted: %s", input)
		}
	}
}

func TestNativeVerifyRejectsUnreadyAndStaleACK(t *testing.T) {
	c, p := nativeLifecycleFixture(t)
	a, _ := nativeTestApp(c)
	if err := a.nativeVerify(context.Background(), c, &p, p.Namespace, p.Deployment, p.Container, Options{}); err != nil {
		t.Fatal(err)
	}
	vpc := nativeLifecycleACK(t, true)
	_ = unstructured.SetNestedField(vpc.Object, false, "status", "destinationRoutes", "ready")
	if err := c.Create(context.Background(), vpc); err != nil {
		t.Fatal(err)
	}
	if err := a.nativeVerify(context.Background(), c, &p, p.Namespace, p.Deployment, p.Container, Options{}); err == nil {
		t.Fatal("verify accepted unacknowledged route intent")
	}
	if err := c.Delete(context.Background(), vpc); err != nil {
		t.Fatal(err)
	}
	var pod corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: p.Namespace, Name: "native-pod"}, &pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.Now()}}
	if err := c.Status().Update(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
	if err := a.nativeVerify(context.Background(), c, &p, p.Namespace, p.Deployment, p.Container, Options{}); err == nil {
		t.Fatal("verify accepted unready native Pod")
	}
}
