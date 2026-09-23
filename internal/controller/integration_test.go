//go:build integration

package controller

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	api "globalvpc.io/controller/api/v1alpha1"
	"globalvpc.io/controller/internal/plan"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestAPIServerLifecycle uses three isolated real API servers and etcd stores.
// Kube-OVN reconciliation and the OVN/IPAM processes are controlled test doubles;
// this validates API behavior, not the overlay dataplane.
func TestAPIServerLifecycle(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS must reference installed envtest binaries")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = api.AddToScheme(scheme)
	_ = apiextv1.AddToScheme(scheme)
	start := func(authority bool) client.Client {
		t.Helper()
		env := &envtest.Environment{ControlPlaneStartTimeout: 45 * time.Second, ControlPlaneStopTimeout: 20 * time.Second}
		if authority {
			env.CRDDirectoryPaths = []string{filepath.Join("..", "..", "config", "crd")}
			env.ErrorIfCRDPathMissing = true
		} else {
			for _, kind := range []string{"Vpc", "Subnet"} {
				plural := "vpcs"
				if kind == "Subnet" {
					plural = "subnets"
				}
				preserve := true
				env.CRDs = append(env.CRDs, &apiextv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: plural + ".kubeovn.io"}, Spec: apiextv1.CustomResourceDefinitionSpec{Group: "kubeovn.io", Scope: apiextv1.ClusterScoped, Names: apiextv1.CustomResourceDefinitionNames{Kind: kind, Plural: plural}, Versions: []apiextv1.CustomResourceDefinitionVersion{{Name: "v1", Served: true, Storage: true, Subresources: &apiextv1.CustomResourceSubresources{Status: &apiextv1.CustomResourceSubresourceStatus{}}, Schema: &apiextv1.CustomResourceValidation{OpenAPIV3Schema: &apiextv1.JSONSchemaProps{Type: "object", Properties: map[string]apiextv1.JSONSchemaProps{"spec": {Type: "object", XPreserveUnknownFields: &preserve}, "status": {Type: "object", XPreserveUnknownFields: &preserve}}}}}}}})
			}
		}
		cfg, err := env.Start()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := env.Stop(); err != nil {
				t.Error(err)
			}
		})
		c, err := client.New(cfg, client.Options{Scheme: scheme})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	h := newHarness(t)
	original := h.current().Spec
	authority := start(true)
	h.authority = authority
	h.r.Client = authority
	for i := range h.r.Config.Clusters {
		remote := start(false)
		var ns corev1.Namespace
		if err := remote.Get(ctx, types.NamespacedName{Name: "kube-system"}, &ns); apierrors.IsNotFound(err) {
			ns = corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}}
			if err = remote.Create(ctx, &ns); err != nil {
				t.Fatal(err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
		h.r.Config.Clusters[i].UID = string(ns.UID)
		cfg := h.r.Config.Clusters[i]
		h.r.Stores[cfg.Name] = ResourceStore{Client: remote, Config: cfg}
	}
	v := &api.GlobalVpc{ObjectMeta: metav1.ObjectMeta{Name: "tenant"}, Spec: original}
	if err := authority.Create(ctx, v); err != nil {
		t.Fatalf("CRD rejected valid GlobalVpc: %v", err)
	}
	p, err := plan.Build(v, h.r.Config)
	if err != nil {
		t.Fatal(err)
	}
	h.p = p
	h.reconcile()
	h.reconcile()
	saved := h.current()
	if len(saved.Status.Resources) != 6 || len(saved.Status.Allocations) != 3 {
		t.Fatalf("API pruned status fields: %+v", saved.Status)
	}
	for _, site := range p.Attachments {
		for _, name := range []string{site.SubnetName, site.TransitName} {
			o := object("Subnet", name)
			store := h.r.Stores[site.Cluster.Name]
			if err = store.Client.Get(ctx, client.ObjectKeyFromObject(o), o); err != nil {
				t.Fatal(err)
			}
			_ = unstructured.SetNestedSlice(o.Object, []interface{}{map[string]interface{}{"type": "Ready", "status": "True"}}, "status", "conditions")
			if err = store.Client.Status().Update(ctx, o); err != nil {
				t.Fatal(err)
			}
		}
	}
	h.reconcile()
	h.reconcile()
	if h.current().Status.Phase != "Ready" {
		t.Fatalf("real API reconciliation did not converge: %+v", h.current().Status)
	}
	before := h.current().Status.Resources
	h.reconcile()
	if !reflect.DeepEqual(before, h.current().Status.Resources) {
		t.Fatal("steady replay changed recorded resource UIDs")
	}
	changed := h.current()
	changed.Spec.Attachments[0].CIDR = "10.249.0.0/24"
	if err = authority.Update(ctx, changed); !apierrors.IsInvalid(err) {
		t.Fatalf("expected CEL immutable-spec rejection, got %v", err)
	}
	// Endpoint presence must keep an actual API deletion pending behind finalizer.
	h.network.endpointsEmpty = false
	h.deleting()
	h.reconcile()
	if h.current().DeletionTimestamp.IsZero() || !contains(h.current().Finalizers, api.Finalizer) {
		t.Fatal("deletion bypassed finalizer")
	}
	h.network.endpointsEmpty = true
	for i := 0; i < 8; i++ {
		h.reconcile()
		var current api.GlobalVpc
		err = authority.Get(ctx, h.request.NamespacedName, &current)
		if apierrors.IsNotFound(err) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	var gone api.GlobalVpc
	if err = authority.Get(ctx, h.request.NamespacedName, &gone); !apierrors.IsNotFound(err) {
		t.Fatalf("GlobalVpc was not finalized: %v %+v", err, gone.Status)
	}
	for _, site := range p.Attachments {
		for _, o := range []*unstructured.Unstructured{site.Vpc, site.Subnet, site.Transit} {
			err = h.r.Stores[site.Cluster.Name].Client.Get(ctx, client.ObjectKeyFromObject(o), o)
			if !apierrors.IsNotFound(err) {
				t.Fatalf("resource remains: %s %v", o.GetName(), err)
			}
		}
	}
	if h.ipam.calls != 3 || len(h.ipam.allocations) != 3 {
		t.Fatal("deletion changed retained allocations")
	}
	t.Log("Three real API servers validated CRD CEL immutability, status receipt/UID persistence, replay, endpoint-blocked finalization, and scoped deletion; OVN/IPAM were test doubles")
}
