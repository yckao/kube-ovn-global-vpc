package localcontroller

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	api "globalvpc.io/controller/api/v1alpha2"
	"globalvpc.io/controller/internal/platformplan"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Model a native Create that committed before its NetworkBinding status receipt.
// The later tombstone has no old subnet spec, as on a full location detach.
func unrecordedSubnetFixture(t *testing.T) (*localHarness, *unstructured.Unstructured, []any) {
	t.Helper()
	h := newLocalHarness(t, false, false)
	b := h.binding()
	routes := []any{map[string]any{"cidr": "10.242.0.0/24", "nextHops": []any{"10.211.0.2", "10.211.0.3"}}}
	vpc := nativeSpec("Vpc", b.Spec.NativeVpcName, string(b.UID), map[string]any{"destinationRoutes": routes})
	if err := h.c.Create(context.Background(), vpc); err != nil {
		t.Fatal(err)
	}
	subnet := nativeSpec("Subnet", "ps-lost-create", string(b.UID), map[string]any{"vpc": b.Spec.NativeVpcName, "cidrBlock": "10.241.0.0/24"})
	if err := h.c.Create(context.Background(), subnet); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "still-running", Namespace: "workloads", Annotations: map[string]string{"ovn.kubernetes.io/logical_switch": subnet.GetName()}}}
	if err := h.c.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	h.edit(func(spec *api.NetworkBindingSpec) { spec.Subnets = nil; spec.Deleting = true })
	return h, subnet, routes
}
func assertRecoveryDidNotWithdraw(t *testing.T, h *localHarness, want []any) {
	t.Helper()
	vpc := native("Vpc", h.binding().Spec.NativeVpcName)
	if err := h.c.Get(context.Background(), client.ObjectKeyFromObject(vpc), vpc); err != nil {
		t.Fatal(err)
	}
	got, _, _ := unstructured.NestedSlice(vpc.Object, "spec", "destinationRoutes")
	if !reflect.DeepEqual(got, want) || !vpc.GetDeletionTimestamp().IsZero() || h.gateway.deletes != 0 || h.gateway.ensures != 0 {
		t.Fatal("native route or gateway withdrawal preceded subnet recovery and drain")
	}
}
func TestRecoverLostNativeCreateBeforeBindingDeletion(t *testing.T) {
	h, subnet, routes := unrecordedSubnetFixture(t)
	h.step()
	recovered := false
	for _, rec := range h.binding().Status.Resources {
		if rec.Kind == "Subnet" && rec.Name == subnet.GetName() && rec.UID == string(subnet.GetUID()) {
			recovered = true
		}
	}
	if !recovered {
		t.Fatal("lost native create receipt was not recovered")
	}
	assertRecoveryDidNotWithdraw(t, h, routes)
	h.step()
	if !meta.IsStatusConditionFalse(h.binding().Status.Conditions, "Ready") || h.binding().Status.Phase != "Deleting" {
		t.Fatal("active endpoint did not block deletion after recovery")
	}
	assertRecoveryDidNotWithdraw(t, h, routes)
	actual := native("Subnet", subnet.GetName())
	if err := h.c.Get(context.Background(), client.ObjectKeyFromObject(actual), actual); err != nil || !actual.GetDeletionTimestamp().IsZero() {
		t.Fatal("in-use recovered subnet was deleted")
	}
}

type receiptFailureClient struct {
	client.Client
	fail, commit bool
}

func (c *receiptFailureClient) Status() client.SubResourceWriter {
	return &receiptFailureWriter{SubResourceWriter: c.Client.Status(), parent: c}
}

type receiptFailureWriter struct {
	client.SubResourceWriter
	parent *receiptFailureClient
}

func (w *receiptFailureWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if b, ok := obj.(*api.NetworkBinding); ok && w.parent.fail {
		for _, rec := range b.Status.Resources {
			if rec.Name == "ps-lost-create" {
				if w.parent.commit {
					if err := w.SubResourceWriter.Update(ctx, obj, opts...); err != nil {
						return err
					}
				}
				return fmt.Errorf("simulated native receipt status write failure")
			}
		}
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}
func TestRecoveryReceiptWriteFailureDoesNotPermitWithdrawal(t *testing.T) {
	for _, commit := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed-%v", commit), func(t *testing.T) {
			h, _, routes := unrecordedSubnetFixture(t)
			failing := &receiptFailureClient{Client: h.c, fail: true, commit: commit}
			h.r.Client = failing
			if _, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: h.key}); err == nil {
				t.Fatal("expected uncertain status write error")
			}
			assertRecoveryDidNotWithdraw(t, h, routes)
			if (len(h.binding().Status.Resources) > 0) != commit {
				t.Fatal("fixture did not model requested status commit outcome")
			}
			failing.fail = false
			h.run(3, false)
			assertRecoveryDidNotWithdraw(t, h, routes)
		})
	}
}
func TestRecoveryRefusesChangedNativeIdentityBeforeWithdrawal(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprintf("foreign-label-%v", foreign), func(t *testing.T) {
			h, subnet, routes := unrecordedSubnetFixture(t)
			b := h.binding()
			b.Status.Resources = []api.ResourceRecord{{APIVersion: "kubeovn.io/v1", Kind: "Subnet", Name: subnet.GetName(), UID: "recorded-original-uid"}}
			meta.SetStatusCondition(&b.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "PreviouslyReady", ObservedGeneration: b.Generation})
			if err := h.c.Status().Update(context.Background(), b); err != nil {
				t.Fatal(err)
			}
			if foreign {
				subnet.SetLabels(map[string]string{NativeOwnerLabel: "different-binding"})
				if err := h.c.Update(context.Background(), subnet); err != nil {
					t.Fatal(err)
				}
			}
			h.step()
			if h.binding().Status.Phase != "Blocked" || !meta.IsStatusConditionFalse(h.binding().Status.Conditions, "Ready") {
				t.Fatal("changed native owner or UID was not fenced")
			}
			if h.binding().Status.Resources[0].UID != "recorded-original-uid" {
				t.Fatal("original UID receipt was replaced")
			}
			assertRecoveryDidNotWithdraw(t, h, routes)
		})
	}
}
func TestRecoveryIncludesUnrecordedTransitOnDisconnect(t *testing.T) {
	h := newLocalHarness(t, false, false)
	b := h.binding()
	subnet := nativeSpec("Subnet", transitName(b), string(b.UID), map[string]any{"vpc": b.Spec.NativeVpcName})
	if err := h.c.Create(context.Background(), subnet); err != nil {
		t.Fatal(err)
	}
	h.step()
	if h.gateway.deletes != 0 {
		t.Fatal("disconnect ran before transit receipt persisted")
	}
	records := h.binding().Status.Resources
	if len(records) != 1 || records[0].Name != subnet.GetName() || records[0].UID != string(subnet.GetUID()) {
		t.Fatal("unrecorded transit subnet was not recovered")
	}
	// The accepted revision itself is unaffected by receipt recovery.
	if h.binding().Spec.Revision != platformplan.Revision(h.binding().Spec) {
		t.Fatal("recovery changed accepted intent")
	}
}
