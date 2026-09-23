package controller

import (
	"context"
	"errors"
	"reflect"
	"testing"

	api "globalvpc.io/controller/api/v1alpha1"
	"globalvpc.io/controller/internal/config"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// mutationClient observes attempted writes, including writes later rejected by
// the API, so guard tests prove refusal before mutation rather than final state.
type mutationClient struct {
	client.Client
	writes  []client.Object
	deletes []*client.DeleteOptions
}

func (c *mutationClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.writes = append(c.writes, obj.DeepCopyObject().(client.Object))
	if obj.GetUID() == "" {
		obj.SetUID("api-assigned-uid")
	}
	return c.Client.Create(ctx, obj, opts...)
}
func (c *mutationClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.writes = append(c.writes, obj.DeepCopyObject().(client.Object))
	return c.Client.Update(ctx, obj, opts...)
}
func (c *mutationClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.writes = append(c.writes, obj.DeepCopyObject().(client.Object))
	return c.Client.Patch(ctx, obj, patch, opts...)
}
func (c *mutationClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.writes = append(c.writes, obj.DeepCopyObject().(client.Object))
	d := &client.DeleteOptions{}
	for _, opt := range opts {
		opt.ApplyToDelete(d)
	}
	c.deletes = append(c.deletes, d)
	return c.Client.Delete(ctx, obj, opts...)
}

func resourceScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, api.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []string{"Vpc", "Subnet"} {
		s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "kubeovn.io", Version: "v1", Kind: kind}, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "kubeovn.io", Version: "v1", Kind: kind + "List"}, &unstructured.UnstructuredList{})
	}
	return s
}

func resourceObject(kind, name, uid string) *unstructured.Unstructured {
	o := object(kind, name)
	o.SetUID(types.UID(uid))
	o.SetLabels(map[string]string{api.OwnerLabel: "tenant-uid", api.ClusterLabel: "infra-uid", "vendor": "preserve"})
	o.Object["spec"] = map[string]interface{}{"namespaces": []interface{}{"old-namespace"}, "staticRoutes": []interface{}{map[string]interface{}{"cidr": "10.2.0.0/24", "nextHopIP": "10.255.0.2"}}, "vendorDefault": true}
	o.Object["status"] = map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Ready", "status": "True"}}, "vendorState": "preserve"}
	return o
}

func resourceStore(t *testing.T, clusterUID string, objects ...client.Object) (ResourceStore, *mutationClient) {
	t.Helper()
	objects = append(objects, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID(clusterUID)}})
	c := &mutationClient{Client: fake.NewClientBuilder().WithScheme(resourceScheme(t)).WithObjects(objects...).Build()}
	return ResourceStore{Client: c, Config: config.Cluster{Name: "infra", UID: "infra-uid"}}, c
}

func TestResourceStoreRefusesOwnershipAndReplacementBeforeWrites(t *testing.T) {
	for _, mutation := range []string{"owner", "cluster", "uid"} {
		for _, operation := range []string{"ensure", "delete", "withdraw", "ready"} {
			t.Run(mutation+"/"+operation, func(t *testing.T) {
				o := resourceObject("Vpc", "tenant", "recorded-uid")
				switch mutation {
				case "owner":
					labels := o.GetLabels()
					labels[api.OwnerLabel] = "another-tenant"
					o.SetLabels(labels)
				case "cluster":
					labels := o.GetLabels()
					labels[api.ClusterLabel] = "another-cluster"
					o.SetLabels(labels)
				case "uid":
					o.SetUID("replacement-uid")
				}
				s, c := resourceStore(t, "infra-uid", o)
				var err error
				switch operation {
				case "ensure":
					_, err = s.Ensure(context.Background(), resourceObject("Vpc", "tenant", ""), "recorded-uid")
				case "delete":
					_, err = s.Delete(context.Background(), "Vpc", "tenant", "tenant-uid", "recorded-uid")
				case "withdraw":
					err = s.WithdrawRoutes(context.Background(), "tenant", "tenant-uid", "recorded-uid")
				case "ready":
					_, err = s.Ready(context.Background(), "Vpc", "tenant", "tenant-uid", "recorded-uid")
				}
				if err == nil || len(c.writes) != 0 {
					t.Fatalf("ownership guard failed: error %v, writes %d", err, len(c.writes))
				}
			})
		}
	}
}

func TestResourceStoreRejectsChangedClusterIdentity(t *testing.T) {
	for _, operation := range []string{"ensure", "delete", "withdraw", "ready"} {
		t.Run(operation, func(t *testing.T) {
			s, c := resourceStore(t, "replacement-cluster", resourceObject("Vpc", "tenant", "recorded-uid"))
			var err error
			switch operation {
			case "ensure":
				_, err = s.Ensure(context.Background(), resourceObject("Vpc", "tenant", ""), "recorded-uid")
			case "delete":
				_, err = s.Delete(context.Background(), "Vpc", "tenant", "tenant-uid", "recorded-uid")
			case "withdraw":
				err = s.WithdrawRoutes(context.Background(), "tenant", "tenant-uid", "recorded-uid")
			case "ready":
				_, err = s.Ready(context.Background(), "Vpc", "tenant", "tenant-uid", "recorded-uid")
			}
			if err == nil || len(c.writes) != 0 {
				t.Fatalf("cluster identity guard failed: error %v, writes %d", err, len(c.writes))
			}
		})
	}
}

func TestResourceStorePreservesVendorFieldsAndReplaysWithoutWrites(t *testing.T) {
	o := resourceObject("Vpc", "tenant", "recorded-uid")
	o.SetAnnotations(map[string]string{"vendor.example/default": "retained"})
	s, c := resourceStore(t, "infra-uid", o)
	desired := resourceObject("Vpc", "tenant", "")
	desired.Object["spec"] = map[string]interface{}{"namespaces": []interface{}{"new-namespace"}, "staticRoutes": []interface{}{map[string]interface{}{"cidr": "10.3.0.0/24", "nextHopIP": "10.255.0.3"}}}
	uid, err := s.Ensure(context.Background(), desired, "recorded-uid")
	if err != nil || uid != "recorded-uid" {
		t.Fatalf("ensure failed: %q, %v", uid, err)
	}
	after, err := s.Get(context.Background(), "Vpc", "tenant")
	if err != nil {
		t.Fatal(err)
	}
	spec := after.Object["spec"].(map[string]interface{})
	if spec["vendorDefault"] != true || !reflect.DeepEqual(after.Object["status"], o.Object["status"]) || !reflect.DeepEqual(after.GetLabels(), o.GetLabels()) || !reflect.DeepEqual(after.GetAnnotations(), o.GetAnnotations()) {
		t.Fatal("vendor fields were overwritten")
	}
	if !reflect.DeepEqual(spec["staticRoutes"], desired.Object["spec"].(map[string]interface{})["staticRoutes"]) {
		t.Fatal("desired routes were not updated")
	}
	if _, err = s.Ensure(context.Background(), desired, uid); err != nil || len(c.writes) != 1 {
		t.Fatalf("replay was not idempotent: %v, writes %d", err, len(c.writes))
	}
}

func TestResourceStoreMissingResourceReturnsNewUIDForJournal(t *testing.T) {
	s, c := resourceStore(t, "infra-uid")
	v := &api.GlobalVpc{Status: api.GlobalVpcStatus{Resources: []api.ResourceRecord{{ClusterRef: "infra", Kind: "Vpc", Name: "tenant", UID: "deleted-uid"}}}}
	uid, err := s.Ensure(context.Background(), resourceObject("Vpc", "tenant", ""), recorded(v, "infra", "Vpc", "tenant"))
	if !errors.Is(err, ErrRecordedResourceMissing) || uid != "" || len(c.writes) != 0 {
		t.Fatalf("recorded absence must be persisted before recreation: %q %v, writes %d", uid, err, len(c.writes))
	}
	if !updateRecord(v, "infra", "Vpc", "tenant", "") {
		t.Fatal("journal did not acknowledge confirmed absence")
	}
	uid, err = s.Ensure(context.Background(), resourceObject("Vpc", "tenant", ""), recorded(v, "infra", "Vpc", "tenant"))
	if err != nil || uid != "api-assigned-uid" || len(c.writes) != 1 {
		t.Fatalf("recreation failed: %q %v", uid, err)
	}
	// The external create committed, but its response/status receipt was lost.
	// Replay with the durable blank journal must recover that same object.
	recovered, err := s.Ensure(context.Background(), resourceObject("Vpc", "tenant", ""), recorded(v, "infra", "Vpc", "tenant"))
	if err != nil || recovered != uid || len(c.writes) != 1 {
		t.Fatalf("lost response recovery changed identity: %q %v", recovered, err)
	}
	if !updateRecord(v, "infra", "Vpc", "tenant", uid) || len(v.Status.Resources) != 1 || recorded(v, "infra", "Vpc", "tenant") != uid {
		t.Fatal("recreated UID did not replace the stale journal entry")
	}
	if updateRecord(v, "infra", "Vpc", "tenant", uid) {
		t.Fatal("unchanged journal receipt triggered a write")
	}
	if _, err = s.Ensure(context.Background(), resourceObject("Vpc", "tenant", ""), uid); err != nil || len(c.writes) != 1 {
		t.Fatalf("recreated object failed replay: %v", err)
	}
}

func TestResourceStoreDeleteUsesUIDAndResourceVersion(t *testing.T) {
	o := resourceObject("Vpc", "tenant", "recorded-uid")
	o.SetResourceVersion("17")
	s, c := resourceStore(t, "infra-uid", o)
	gone, err := s.Delete(context.Background(), "Vpc", "tenant", "tenant-uid", "recorded-uid")
	if err != nil || gone || len(c.deletes) != 1 {
		t.Fatalf("initial delete result: %v %v", gone, err)
	}
	p := c.deletes[0].Preconditions
	if p == nil || p.UID == nil || *p.UID != "recorded-uid" || p.ResourceVersion == nil || *p.ResourceVersion != "17" {
		t.Fatalf("missing delete preconditions: %#v", p)
	}
	gone, err = s.Delete(context.Background(), "Vpc", "tenant", "tenant-uid", "recorded-uid")
	if err != nil || !gone || len(c.deletes) != 1 {
		t.Fatalf("absence was not acknowledged idempotently: %v %v", gone, err)
	}
}

func TestResourceStoreWaitsForTerminatingObject(t *testing.T) {
	o := resourceObject("Vpc", "tenant", "recorded-uid")
	now := metav1.Now()
	o.SetDeletionTimestamp(&now)
	o.SetFinalizers([]string{"vendor.example/cleanup"})
	s, c := resourceStore(t, "infra-uid", o)
	if _, err := s.Ensure(context.Background(), resourceObject("Vpc", "tenant", ""), "recorded-uid"); err == nil {
		t.Fatal("terminating object accepted")
	}
	if gone, err := s.Delete(context.Background(), "Vpc", "tenant", "tenant-uid", "recorded-uid"); err != nil || gone {
		t.Fatalf("terminating object claimed absent: %v %v", gone, err)
	}
	if ready, err := s.Ready(context.Background(), "Vpc", "tenant", "tenant-uid", "recorded-uid"); err != nil || ready {
		t.Fatalf("terminating object claimed ready: %v %v", ready, err)
	}
	if len(c.writes) != 0 {
		t.Fatal("terminating object was mutated")
	}
}

func TestResourceStoreWithdrawRoutesPreservesOtherFields(t *testing.T) {
	s, c := resourceStore(t, "infra-uid", resourceObject("Vpc", "tenant", "recorded-uid"))
	if err := s.WithdrawRoutes(context.Background(), "tenant", "tenant-uid", "recorded-uid"); err != nil {
		t.Fatal(err)
	}
	o, _ := s.Get(context.Background(), "Vpc", "tenant")
	routes, _, _ := unstructured.NestedSlice(o.Object, "spec", "staticRoutes")
	vendor, _, _ := unstructured.NestedBool(o.Object, "spec", "vendorDefault")
	if len(routes) != 0 || !vendor {
		t.Fatal("route withdrawal changed unrelated fields")
	}
	if err := s.WithdrawRoutes(context.Background(), "tenant", "tenant-uid", "recorded-uid"); err != nil || len(c.writes) != 1 {
		t.Fatalf("withdrawal replay was not idempotent: %v", err)
	}
}
