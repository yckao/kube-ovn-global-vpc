//go:build integration

package localcontroller

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	api "globalvpc.io/controller/api/v1alpha2"
	"globalvpc.io/controller/internal/platformplan"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// These tests use real API-assigned UIDs, resourceVersions, status subresources,
// and delete preconditions. The native CRDs and native readiness acknowledgements
// remain explicit simulators: no Kube-OVN, gateway process, OVSDB, or packets run.
type ownershipAPIHarness struct {
	t       *testing.T
	ctx     context.Context
	c       client.Client
	r       *Reconciler
	key     client.ObjectKey
	gateway *gatewayDouble
}

func ownershipAPIServer(t *testing.T) (*controlServer, context.Context) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS must reference installed envtest binaries")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return startControlServer(t, true), ctx
}

func newOwnershipAPIHarness(t *testing.T, ctx context.Context, c client.Client, name string) *ownershipAPIHarness {
	t.Helper()
	cfg := localFixtureConfig()
	cfg.Namespace = "ownership-" + name
	ensureNamespace(t, c, cfg.Namespace)
	cfg.ClusterUID = string(ensureNamespace(t, c, "kube-system").UID)
	b := localFixtureBinding(false)
	b.UID, b.ResourceVersion, b.Generation = "", "", 0
	b.Namespace = cfg.Namespace
	b.Spec.VPCRef.UID = "ownership-" + name
	b.Name = platformplan.BindingName(b.Spec.VPCRef.UID, b.Spec.LocationRef)
	b.Spec.NativeVpcName = platformplan.VpcName(b.Spec.VPCRef.UID, b.Spec.LocationRef)
	b.Spec.Subnets = b.Spec.Subnets[:1]
	b.Spec.Subnets[0].NativeSubnetName = "ps-ownership-" + name
	b.Annotations[SourceUIDAnnotation] = "authority-" + name
	b.Spec.Revision = platformplan.Revision(b.Spec)
	if err := c.Create(ctx, b); err != nil {
		t.Fatal(err)
	}
	if b.UID == "" || b.ResourceVersion == "" || b.Generation != 1 {
		t.Fatal("API did not assign a new binding identity and generation")
	}
	gateway := &gatewayDouble{ready: true}
	return &ownershipAPIHarness{t: t, ctx: ctx, c: c, key: client.ObjectKeyFromObject(b), gateway: gateway,
		r: &Reconciler{Client: c, Config: cfg, Gateway: gateway}}
}

func (h *ownershipAPIHarness) binding() *api.NetworkBinding {
	h.t.Helper()
	b := &api.NetworkBinding{}
	if err := h.c.Get(h.ctx, h.key, b); err != nil {
		h.t.Fatal(err)
	}
	return b
}

func (h *ownershipAPIHarness) step() {
	h.t.Helper()
	if _, err := h.r.Reconcile(h.ctx, ctrl.Request{NamespacedName: h.key}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *ownershipAPIHarness) converge() {
	h.t.Helper()
	for attempt := 0; attempt < 30; attempt++ {
		h.step()
		// This deliberately invokes the existing native status simulator. It does
		// not emulate API storage: status updates still go through the real API.
		simulateNative(h.t, h.c, true)
		b := h.binding()
		if b.Status.AppliedRevision == b.Spec.Revision && meta.IsStatusConditionTrue(b.Status.Conditions, "Ready") {
			return
		}
	}
	h.t.Fatalf("native API fixture did not converge: %+v", h.binding().Status)
}

func (h *ownershipAPIHarness) object(kind, name string) *unstructured.Unstructured {
	h.t.Helper()
	o := native(kind, name)
	if err := h.c.Get(h.ctx, client.ObjectKeyFromObject(o), o); err != nil {
		h.t.Fatal(err)
	}
	return o
}

func (h *ownershipAPIHarness) deleting() {
	h.t.Helper()
	b := h.binding()
	previousGeneration := b.Generation
	b.Spec.Deleting = true
	b.Spec.Subnets = nil
	b.Spec.Revision = platformplan.Revision(b.Spec)
	if err := h.c.Update(h.ctx, b); err != nil {
		h.t.Fatal(err)
	}
	if b.Generation != previousGeneration+1 {
		h.t.Fatal("real API did not advance the accepted tombstone generation")
	}
}

func ownershipReceipt(t *testing.T, b *api.NetworkBinding, kind, name string) api.ResourceRecord {
	t.Helper()
	for _, receipt := range b.Status.Resources {
		if receipt.APIVersion == "kubeovn.io/v1" && receipt.Kind == kind && receipt.Name == name {
			return receipt
		}
	}
	t.Fatalf("missing durable native %s/%s receipt", kind, name)
	return api.ResourceRecord{}
}

func assertOwnershipObjectUnchanged(t *testing.T, want, got *unstructured.Unstructured) {
	t.Helper()
	if got.GetUID() != want.GetUID() || got.GetResourceVersion() != want.GetResourceVersion() || !reflect.DeepEqual(got.Object, want.Object) {
		t.Fatalf("refused native object was mutated: wanted UID/RV %s/%s, got %s/%s", want.GetUID(), want.GetResourceVersion(), got.GetUID(), got.GetResourceVersion())
	}
}

func assertOwnershipBlocked(t *testing.T, b *api.NetworkBinding, message string) {
	t.Helper()
	condition := meta.FindStatusCondition(b.Status.Conditions, "Ready")
	if b.Status.Phase != "Blocked" || condition == nil || condition.Status != metav1.ConditionFalse || condition.ObservedGeneration != b.Generation || !strings.Contains(condition.Message, message) {
		t.Fatalf("ownership rejection was not recorded against current generation: %+v", b.Status)
	}
}

func TestAPINativeForeignOwnerCollision(t *testing.T) {
	server, ctx := ownershipAPIServer(t)
	for _, kind := range []string{"Vpc", "Subnet"} {
		t.Run(kind, func(t *testing.T) {
			h := newOwnershipAPIHarness(t, ctx, server.c, "foreign-"+strings.ToLower(kind))
			b := h.binding()
			name := b.Spec.NativeVpcName
			if kind == "Subnet" {
				name = b.Spec.Subnets[0].NativeSubnetName
			}
			foreign := nativeSpec(kind, name, "another-binding-owner", map[string]any{"foreignSentinel": "must-survive"})
			if err := h.c.Create(ctx, foreign); err != nil {
				t.Fatal(err)
			}
			before := h.object(kind, name)
			for attempt := 0; attempt < 3; attempt++ {
				h.step()
			}
			blocked := h.binding()
			assertOwnershipBlocked(t, blocked, "another owner")
			assertOwnershipObjectUnchanged(t, before, h.object(kind, name))
			if len(blocked.Status.Resources) != 0 || blocked.Status.AppliedRevision != "" || h.gateway.ensures != 0 || h.gateway.deletes != 0 {
				t.Fatal("foreign collision was acknowledged or permitted gateway side effects")
			}
		})
	}
}

func ownershipReplace(ctx context.Context, c client.Client, old *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	uid, rv := old.GetUID(), old.GetResourceVersion()
	if err := c.Delete(ctx, old, client.Preconditions{UID: &uid, ResourceVersion: &rv}); err != nil {
		return nil, err
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(old), native(old.GetKind(), old.GetName())); !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("old native object did not disappear before replacement: %v", err)
	}
	replacement := old.DeepCopy()
	replacement.SetUID("")
	replacement.SetResourceVersion("")
	replacement.SetGeneration(0)
	replacement.SetCreationTimestamp(metav1.Time{})
	replacement.SetManagedFields(nil)
	delete(replacement.Object, "status")
	if err := c.Create(ctx, replacement); err != nil {
		return nil, err
	}
	if replacement.GetUID() == "" || replacement.GetUID() == uid {
		return nil, fmt.Errorf("API did not assign a different native incarnation")
	}
	return replacement, nil
}

func TestAPINativeUIDReplacementBlocksReconcileAndDeletion(t *testing.T) {
	server, ctx := ownershipAPIServer(t)
	for _, kind := range []string{"Vpc", "Subnet"} {
		t.Run(kind, func(t *testing.T) {
			h := newOwnershipAPIHarness(t, ctx, server.c, "replace-"+strings.ToLower(kind))
			h.converge()
			b := h.binding()
			name := b.Spec.NativeVpcName
			if kind == "Subnet" {
				name = b.Spec.Subnets[0].NativeSubnetName
			}
			receipt := ownershipReceipt(t, b, kind, name)
			old := h.object(kind, name)
			if receipt.UID != string(old.GetUID()) {
				t.Fatal("baseline receipt does not contain the actual API-assigned UID")
			}
			replacement, err := ownershipReplace(ctx, h.c, old)
			if err != nil {
				t.Fatal(err)
			}
			if replacement.GetLabels()[NativeOwnerLabel] != string(b.UID) {
				t.Fatal("replacement fixture must retain the matching owner label")
			}
			before := h.object(kind, name)
			ensures, deletes := h.gateway.ensures, h.gateway.deletes
			for _, tombstone := range []bool{false, true} {
				if tombstone {
					h.deleting()
				}
				for attempt := 0; attempt < 3; attempt++ {
					h.step()
				}
				blocked := h.binding()
				assertOwnershipBlocked(t, blocked, "UID changed")
				if got := ownershipReceipt(t, blocked, kind, name); got != receipt {
					t.Fatalf("replacement overwrote original native receipt: %+v", got)
				}
				if tombstone && blocked.Status.AppliedRevision == blocked.Spec.Revision {
					t.Fatal("refused deletion was acknowledged as applied")
				}
				assertOwnershipObjectUnchanged(t, before, h.object(kind, name))
				if h.gateway.ensures != ensures || h.gateway.deletes != deletes {
					t.Fatal("UID rejection permitted gateway withdrawal or recreation")
				}
			}
		})
	}
}

// Inject a concurrent API mutation exactly after the production delete guard and
// before its Delete reaches storage. We do not synthesize a Conflict: the real
// API evaluates the production UID/resourceVersion delete preconditions.
type ownershipDeleteRaceClient struct {
	client.Client
	targetName    string
	replace       bool
	fired         bool
	preconditions *metav1.Preconditions
	guarded       *unstructured.Unstructured
	concurrent    *unstructured.Unstructured
	deleteErr     error
}

func (c *ownershipDeleteRaceClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if c.fired || obj.GetObjectKind().GroupVersionKind().Kind != "Subnet" || obj.GetName() != c.targetName {
		return c.Client.Delete(ctx, obj, opts...)
	}
	c.fired = true
	options := &client.DeleteOptions{}
	for _, option := range opts {
		option.ApplyToDelete(options)
	}
	c.preconditions = options.Preconditions
	old := native("Subnet", c.targetName)
	if err := c.Client.Get(ctx, client.ObjectKeyFromObject(old), old); err != nil {
		return err
	}
	c.guarded = old.DeepCopy()
	if c.replace {
		concurrent, err := ownershipReplace(ctx, c.Client, old)
		if err != nil {
			return err
		}
		c.concurrent = concurrent
	} else {
		labels := old.GetLabels()
		labels[NativeOwnerLabel] = "concurrent-owner"
		old.SetLabels(labels)
		if err := c.Client.Update(ctx, old); err != nil {
			return err
		}
		c.concurrent = old.DeepCopy()
	}
	c.deleteErr = c.Client.Delete(ctx, obj, opts...)
	return c.deleteErr
}

func TestAPINativeDeletionPreconditionsFenceConcurrentMutation(t *testing.T) {
	server, ctx := ownershipAPIServer(t)
	for _, replace := range []bool{false, true} {
		name := "owner-resourceversion"
		if replace {
			name = "replacement-uid"
		}
		t.Run(name, func(t *testing.T) {
			h := newOwnershipAPIHarness(t, ctx, server.c, "delete-"+name)
			h.converge()
			b := h.binding()
			subnet := b.Spec.Subnets[0].NativeSubnetName
			receipt := ownershipReceipt(t, b, "Subnet", subnet)
			h.deleting()
			race := &ownershipDeleteRaceClient{Client: h.c, targetName: subnet, replace: replace}
			h.r.Client = race
			for attempt := 0; attempt < 8 && !race.fired; attempt++ {
				h.step()
			}
			if !race.fired || race.concurrent == nil || !apierrors.IsConflict(race.deleteErr) {
				t.Fatalf("real API did not reject the stale production delete: fired=%v err=%v", race.fired, race.deleteErr)
			}
			p := race.preconditions
			if p == nil || p.UID == nil || p.ResourceVersion == nil || *p.UID != race.guarded.GetUID() || *p.ResourceVersion != race.guarded.GetResourceVersion() {
				t.Fatal("production delete omitted exact UID or resourceVersion preconditions")
			}
			before := h.object("Subnet", subnet)
			if !before.GetDeletionTimestamp().IsZero() || before.GetUID() != race.concurrent.GetUID() || before.GetResourceVersion() != race.concurrent.GetResourceVersion() {
				t.Fatal("stale delete touched the concurrent native object")
			}
			for attempt := 0; attempt < 3; attempt++ {
				h.step()
			}
			blocked := h.binding()
			assertOwnershipBlocked(t, blocked, map[bool]string{false: "another owner", true: "UID changed"}[replace])
			if ownershipReceipt(t, blocked, "Subnet", subnet) != receipt || blocked.Status.AppliedRevision == blocked.Spec.Revision {
				t.Fatal("failed native deletion retired its receipt or acknowledged cleanup")
			}
			assertOwnershipObjectUnchanged(t, before, h.object("Subnet", subnet))
		})
	}
}

type ownershipReceiptConflictClient struct {
	client.Client
	name     string
	fired    bool
	conflict error
}

func (c *ownershipReceiptConflictClient) Status() client.SubResourceWriter {
	return &ownershipReceiptConflictWriter{SubResourceWriter: c.Client.Status(), parent: c}
}

type ownershipReceiptConflictWriter struct {
	client.SubResourceWriter
	parent *ownershipReceiptConflictClient
}

func (w *ownershipReceiptConflictWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	b, ok := obj.(*api.NetworkBinding)
	if ok && b.Name == w.parent.name && !w.parent.fired && len(b.Status.Resources) > 0 {
		w.parent.fired = true
		current := &api.NetworkBinding{}
		if err := w.parent.Client.Get(ctx, client.ObjectKeyFromObject(b), current); err != nil {
			return err
		}
		current.Annotations["ownership-test.globalvpc.io/concurrent-writer"] = "advanced-resourceversion"
		if err := w.parent.Client.Update(ctx, current); err != nil {
			return err
		}
		w.parent.conflict = w.SubResourceWriter.Update(ctx, b, opts...)
		return w.parent.conflict
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func TestAPINativeRecoveryReceiptConflictFencesWithdrawal(t *testing.T) {
	server, ctx := ownershipAPIServer(t)
	h := newOwnershipAPIHarness(t, ctx, server.c, "receipt-conflict")
	b := h.binding()
	name := b.Spec.Subnets[0].NativeSubnetName
	routes := []any{map[string]any{"cidr": "10.242.0.0/24", "nextHops": []any{"10.211.0.2", "10.211.0.3"},
		"bfd": map[string]any{"minRX": int64(300), "minTX": int64(300), "multiplier": int64(3)}, "selectionFields": []any{"ip_src", "ip_dst"}}}
	vpc := nativeSpec("Vpc", b.Spec.NativeVpcName, string(b.UID), map[string]any{"destinationRoutes": routes})
	subnet := nativeSpec("Subnet", name, string(b.UID), map[string]any{"vpc": b.Spec.NativeVpcName, "cidrBlock": "10.241.0.0/24"})
	for _, obj := range []client.Object{vpc, subnet} {
		if err := h.c.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "referencing-endpoint", Namespace: b.Namespace,
		Annotations: map[string]string{"ovn.kubernetes.io/logical_switch": name}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "placeholder", Image: "example.invalid/never-started"}}}}
	if err := h.c.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	h.deleting()
	beforeVpc, beforeSubnet := h.object("Vpc", vpc.GetName()), h.object("Subnet", name)
	race := &ownershipReceiptConflictClient{Client: h.c, name: b.Name}
	h.r.Client = race
	_, err := h.r.Reconcile(ctx, ctrl.Request{NamespacedName: h.key})
	if !race.fired || !apierrors.IsConflict(err) || !apierrors.IsConflict(race.conflict) || len(h.binding().Status.Resources) != 0 {
		t.Fatalf("real status CAS conflict did not stop receipt recovery: %v", err)
	}
	assertOwnershipObjectUnchanged(t, beforeVpc, h.object("Vpc", vpc.GetName()))
	assertOwnershipObjectUnchanged(t, beforeSubnet, h.object("Subnet", name))
	if h.gateway.ensures != 0 || h.gateway.deletes != 0 {
		t.Fatal("unpersisted receipt permitted gateway side effects")
	}
	// A new reconciler rediscovers the same committed native UID, records it,
	// and only then evaluates endpoint drain. No UID is supplied by a fake client.
	h.r = &Reconciler{Client: h.c, Config: h.r.Config, Gateway: h.gateway}
	h.step()
	if got := ownershipReceipt(t, h.binding(), "Subnet", name); got.UID != string(subnet.GetUID()) {
		t.Fatal("restart did not recover the committed native Subnet UID")
	}
	h.step()
	condition := meta.FindStatusCondition(h.binding().Status.Conditions, "Ready")
	if condition == nil || condition.Reason != "SubnetInUse" {
		t.Fatal("recovered receipt did not protect the API-referencing endpoint")
	}
	assertOwnershipObjectUnchanged(t, beforeVpc, h.object("Vpc", vpc.GetName()))
	assertOwnershipObjectUnchanged(t, beforeSubnet, h.object("Subnet", name))
	if h.gateway.ensures != 0 || h.gateway.deletes != 0 {
		t.Fatal("endpoint drain gate permitted route or gateway withdrawal")
	}
	uid, rv := pod.UID, pod.ResourceVersion
	if err := h.c.Delete(ctx, pod, client.GracePeriodSeconds(0), client.Preconditions{UID: &uid, ResourceVersion: &rv}); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 30; attempt++ {
		h.step()
		simulateNative(t, h.c, true)
		if h.binding().Status.Phase == "Deleted" {
			break
		}
	}
	deleted := h.binding()
	if deleted.Status.Phase != "Deleted" || len(deleted.Status.Resources) != 0 || deleted.Status.AppliedRevision != deleted.Spec.Revision {
		t.Fatalf("receipt-fenced deletion did not complete after endpoint removal: %+v", deleted.Status)
	}
	for _, obj := range []*unstructured.Unstructured{vpc, subnet} {
		if err := h.c.Get(ctx, client.ObjectKeyFromObject(obj), native(obj.GetKind(), obj.GetName())); !apierrors.IsNotFound(err) {
			t.Fatalf("owned native %s remained after confirmed deletion: %v", obj.GetKind(), err)
		}
	}
}
