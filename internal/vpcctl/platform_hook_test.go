package vpcctl

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	api "globalvpc.io/controller/api/v1alpha2"
	"globalvpc.io/controller/internal/platformplan"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func platformTerminalFixture() *api.NetworkBinding {
	b := &api.NetworkBinding{ObjectMeta: metav1.ObjectMeta{Name: platformplan.BindingName("vpc-fixture", "dc-a"), Namespace: "operator-system", UID: "local-binding", Generation: 3, Annotations: map[string]string{"platform.globalvpc.io/source-binding-uid": "authority-binding"}}, Spec: api.NetworkBindingSpec{VPCRef: api.ObjectIdentity{Namespace: "project", Name: "production", UID: "vpc-fixture"}, NetworkID: 10001, LocalASN: 4200000001, LocationRef: "dc-a", NetworkClassRef: "default", NativeVpcName: platformplan.VpcName("vpc-fixture", "dc-a"), TransportProfile: "wireguard-bgp", AuthorizedLocations: []string{"dc-a"}, Deleting: true}}
	b.Spec.Revision = platformplan.Revision(b.Spec)
	b.Status = api.NetworkBindingStatus{ObservedGeneration: b.Generation, Phase: "Deleted", AppliedRevision: b.Spec.Revision, Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, ObservedGeneration: b.Generation, Reason: "CleanupComplete", Message: "Owned native and gateway resources are absent; allocation receipts retained", LastTransitionTime: metav1.Now()}}}
	return b
}

func platformHookClient(t *testing.T, objects ...client.Object) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{api.AddToScheme, corev1.AddToScheme, appsv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.NetworkBinding{}, &appsv1.Deployment{}, &corev1.Pod{}).WithObjects(objects...).Build()
}

func platformRunHook(t *testing.T, c client.Client, role, action string, extra ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	app := &App{Out: &out, ClientFactory: func(Options) (client.Client, error) { return c, nil }}
	args := []string{"internal", "platform-hook", "--role", role, "--action", action, "--namespace", "operator-system"}
	args = append(args, extra...)
	err := app.Run(context.Background(), args)
	return out.String(), err
}

func TestPlatformTerminalReceiptRequiresCurrentCompleteAcknowledgement(t *testing.T) {
	valid := platformTerminalFixture()
	if !platformCompletedBinding(*valid) {
		t.Fatal("real controller-style terminal receipt rejected")
	}
	for name, mutate := range map[string]func(*api.NetworkBinding){
		"stale generation":        func(b *api.NetworkBinding) { b.Status.ObservedGeneration-- },
		"stale condition":         func(b *api.NetworkBinding) { b.Status.Conditions[0].ObservedGeneration-- },
		"missing condition":       func(b *api.NetworkBinding) { b.Status.Conditions = nil },
		"unexpected ready":        func(b *api.NetworkBinding) { b.Status.Conditions[0].Status = metav1.ConditionTrue },
		"wrong reason":            func(b *api.NetworkBinding) { b.Status.Conditions[0].Reason = "GatewayCleanupPending" },
		"wrong phase":             func(b *api.NetworkBinding) { b.Status.Phase = "Deleting" },
		"unapplied revision":      func(b *api.NetworkBinding) { b.Status.AppliedRevision = "old" },
		"changed accepted intent": func(b *api.NetworkBinding) { b.Spec.NetworkID++ },
		"not deleting": func(b *api.NetworkBinding) {
			b.Spec.Deleting = false
			b.Spec.Revision = platformplan.Revision(b.Spec)
			b.Status.AppliedRevision = b.Spec.Revision
		},
		"native residue":          func(b *api.NetworkBinding) { b.Status.Resources = []api.ResourceRecord{{Kind: "Vpc", Name: "old"}} },
		"gateway residue":         func(b *api.NetworkBinding) { b.Status.Gateways = []api.GatewayEndpoint{{ID: "old"}} },
		"missing source identity": func(b *api.NetworkBinding) { b.Annotations = nil },
		"missing UID":             func(b *api.NetworkBinding) { b.UID = "" },
		"foreign name":            func(b *api.NetworkBinding) { b.Name = "other" },
		"unknown finalizer":       func(b *api.NetworkBinding) { b.Finalizers = []string{"example.invalid/cleanup"} },
		"duplicate ready":         func(b *api.NetworkBinding) { b.Status.Conditions = append(b.Status.Conditions, b.Status.Conditions[0]) },
	} {
		t.Run(name, func(t *testing.T) {
			b := valid.DeepCopy()
			mutate(b)
			if platformCompletedBinding(*b) {
				t.Fatal("incomplete receipt accepted")
			}
		})
	}
}

func TestPlatformUninstallRetainsTerminalReceiptsAndBlocksActualResidue(t *testing.T) {
	receipt := platformTerminalFixture()
	allocation := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: receipt.Namespace, Name: "managed-network-allocations", UID: "ledger"}, Data: map[string]string{"state.json": "retained fixture"}}
	base := platformHookClient(t, receipt, allocation)
	writes := 0
	c := interceptor.NewClient(base, interceptor.Funcs{Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
		writes++
		return nil
	}, Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
		writes++
		return nil
	}, Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
		writes++
		return nil
	}})
	out, err := platformRunHook(t, c, "site", "check-empty")
	if err != nil || !strings.Contains(out, "1 acknowledged") {
		t.Fatalf("completed site cleanup blocked: %v %s", err, out)
	}
	if writes != 0 {
		t.Fatal("read-only hook mutated retained state")
	}
	if err = base.Get(context.Background(), client.ObjectKeyFromObject(receipt), &api.NetworkBinding{}); err != nil {
		t.Fatal("cleanup receipt was removed")
	}
	if err = base.Get(context.Background(), client.ObjectKeyFromObject(allocation), &corev1.ConfigMap{}); err != nil {
		t.Fatal("allocation ledger was removed")
	}
	if _, err = platformRunHook(t, c, "authority", "check-empty"); err == nil {
		t.Fatal("authority must finish deleting its binding; only local receipts may remain")
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "orphan-gateway", Namespace: receipt.Namespace, Labels: map[string]string{"platform.globalvpc.io/component": "gateway"}}}
	if err = base.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	if _, err = platformRunHook(t, c, "site", "check-empty"); err == nil || !strings.Contains(err.Error(), "gateway Pods") {
		t.Fatalf("gateway residue was accepted: %v", err)
	}
	pending := receipt.DeepCopy()
	pending.Status.ObservedGeneration--
	if _, err = platformRunHook(t, platformHookClient(t, pending), "site", "check-empty"); err == nil {
		t.Fatal("stale terminal receipt allowed uninstall")
	}
}

func TestPlatformConfigGuardPreservesRetainedAllocationIdentity(t *testing.T) {
	receipt := platformTerminalFixture()
	before := map[string]any{"clusterUID": "cluster-a", "transitPool": "172.30.0.0/20", "gatewayImage": "old-image"}
	raw, _ := json.Marshal(before)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "site-config", Namespace: receipt.Namespace}, Data: map[string]string{"site.json": string(raw)}}
	c := platformHookClient(t, receipt, cm)
	path := filepath.Join(t.TempDir(), "desired.json")
	run := func(after map[string]any) error {
		raw, _ := json.Marshal(after)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		_, err := platformRunHook(t, c, "site", "check-config", "--config-map", cm.Name, "--desired-config", path)
		return err
	}
	if err := run(map[string]any{"clusterUID": "cluster-a", "transitPool": "172.30.0.0/20", "gatewayImage": "new-image"}); err != nil {
		t.Fatalf("image-only upgrade rejected: %v", err)
	}
	if err := run(map[string]any{"clusterUID": "cluster-b", "transitPool": "172.30.0.0/20", "gatewayImage": "new-image"}); err == nil || !strings.Contains(err.Error(), "retained allocation receipts") {
		t.Fatalf("drained identity silently migrated: %v", err)
	}
	if err := run(map[string]any{"clusterUID": "cluster-a", "transitPool": "172.31.0.0/20", "gatewayImage": "new-image"}); err == nil {
		t.Fatal("drained pool silently migrated")
	}
}

func TestPlatformVerifyAcceptsCompletedLocalReceiptAndRejectsStaleOne(t *testing.T) {
	receipt := platformTerminalFixture()
	_, _, d := nativeTestObjects()
	d.Namespace = receipt.Namespace
	d.Name = "site"
	d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "controller", Image: nativeTestBundle().BaseImage}}
	set, pod := nativeTestReadyObjects(d)
	pod.Status.ContainerStatuses[0].Name = "controller"
	c := platformHookClient(t, receipt, d, set, pod)
	out, err := platformRunHook(t, c, "site", "verify", "--deployment", d.Name)
	if err != nil || !strings.Contains(out, "retained acknowledged cleanup snapshot") {
		t.Fatalf("terminal receipt failed site verification: %v %s", err, out)
	}
	var stale api.NetworkBinding
	if err = c.Get(context.Background(), client.ObjectKeyFromObject(receipt), &stale); err != nil {
		t.Fatal(err)
	}
	stale.Status.Conditions[0].ObservedGeneration--
	if err = c.Status().Update(context.Background(), &stale); err != nil {
		t.Fatal(err)
	}
	if _, err = platformRunHook(t, c, "site", "verify", "--deployment", d.Name); err == nil {
		t.Fatal("stale terminal status passed verification")
	}
}

func TestDrainRespectsFinalizersAndProjectBoundary(t *testing.T) {
	sub := &api.Subnet{ObjectMeta: metav1.ObjectMeta{Name: "apps", Namespace: "selected", UID: "sub", Finalizers: []string{api.Finalizer}}, Spec: api.SubnetSpec{VPCRef: "production", LocationRef: "dc-a", CIDR: "10.60.1.0/24"}}
	vpc := &api.VPC{ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: "selected", UID: "vpc"}}
	foreign := &api.Subnet{ObjectMeta: metav1.ObjectMeta{Name: "apps", Namespace: "other", UID: "foreign"}}
	c := platformHookClient(t, sub, vpc, foreign)
	app := App{ClientFactory: func(Options) (client.Client, error) { return c, nil }}
	err := app.Run(context.Background(), []string{"--timeout", "20ms", "drain", "--project", "selected"})
	if err == nil || !strings.Contains(err.Error(), "cleanup remains pending") {
		t.Fatalf("finalizer was bypassed: %v", err)
	}
	got := &api.Subnet{}
	if err = c.Get(context.Background(), client.ObjectKeyFromObject(sub), got); err != nil || got.DeletionTimestamp == nil || len(got.Finalizers) != 1 {
		t.Fatalf("drain stripped or ignored finalizer: %+v %v", got, err)
	}
	if err = c.Get(context.Background(), client.ObjectKeyFromObject(vpc), &api.VPC{}); err != nil {
		t.Fatal("VPC deleted before Subnet cleanup")
	}
	if err = c.Get(context.Background(), client.ObjectKeyFromObject(foreign), got); err != nil || got.DeletionTimestamp != nil {
		t.Fatal("other project was modified")
	}
	// Clear only the selected fixture through simulated controller cleanup.
	if err = c.Get(context.Background(), client.ObjectKeyFromObject(sub), got); err != nil {
		t.Fatal(err)
	}
	got.Finalizers = nil
	if err = c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	if err = app.Run(context.Background(), []string{"--timeout", "1s", "drain", "--project", "selected"}); err != nil {
		t.Fatal(err)
	}
	if err = c.Get(context.Background(), client.ObjectKeyFromObject(vpc), &api.VPC{}); !apierrors.IsNotFound(err) {
		t.Fatal("drained VPC still present", err)
	}
}
