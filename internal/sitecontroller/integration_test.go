//go:build integration

package sitecontroller

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	api "globalvpc.io/controller/api/v1alpha1"
	legacy "globalvpc.io/controller/internal/controller"
	"globalvpc.io/controller/internal/siteconfig"
	"globalvpc.io/controller/internal/siteplan"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestIndependentAPIServersDuringSiteLoss validates actual API schema, etcd
// receipts and finalizers. OVN and gateway processes are explicit test doubles;
// this does not claim cross-site packet forwarding or gateway failover proof.
func TestIndependentAPIServersDuringSiteLoss(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS must reference installed envtest binaries")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	type server struct {
		client client.Client
		uid    string
		stop   func()
	}
	start := func() server {
		t.Helper()
		scheme := testScheme(t)
		_ = apiextv1.AddToScheme(scheme)
		e := &envtest.Environment{ControlPlaneStartTimeout: 45 * time.Second, ControlPlaneStopTimeout: 20 * time.Second,
			CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd")}, ErrorIfCRDPathMissing: true}
		for _, kind := range []string{"Vpc", "Subnet"} {
			plural := "vpcs"
			if kind == "Subnet" {
				plural = "subnets"
			}
			preserve := true
			e.CRDs = append(e.CRDs, &apiextv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: plural + ".kubeovn.io"}, Spec: apiextv1.CustomResourceDefinitionSpec{Group: "kubeovn.io", Scope: apiextv1.ClusterScoped, Names: apiextv1.CustomResourceDefinitionNames{Kind: kind, Plural: plural}, Versions: []apiextv1.CustomResourceDefinitionVersion{{Name: "v1", Served: true, Storage: true, Subresources: &apiextv1.CustomResourceSubresources{Status: &apiextv1.CustomResourceSubresourceStatus{}}, Schema: &apiextv1.CustomResourceValidation{OpenAPIV3Schema: &apiextv1.JSONSchemaProps{Type: "object", Properties: map[string]apiextv1.JSONSchemaProps{"spec": {Type: "object", XPreserveUnknownFields: &preserve}, "status": {Type: "object", XPreserveUnknownFields: &preserve}}}}}}}})
		}
		cfg, err := e.Start()
		if err != nil {
			t.Fatal(err)
		}
		stopped := false
		stop := func() {
			if !stopped {
				stopped = true
				if err := e.Stop(); err != nil {
					t.Error(err)
				}
			}
		}
		t.Cleanup(stop)
		cfg.Timeout = 3 * time.Second
		c, err := client.New(cfg, client.Options{Scheme: scheme})
		if err != nil {
			t.Fatal(err)
		}
		var ns corev1.Namespace
		if err := c.Get(ctx, client.ObjectKey{Name: "kube-system"}, &ns); apierrors.IsNotFound(err) {
			ns = corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}}
			if err = c.Create(ctx, &ns); err != nil {
				t.Fatal(err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
		return server{client: c, uid: string(ns.UID), stop: stop}
	}
	a, b := start(), start()
	attach := func(s server, site, globalID, name, cidr, gateway string) *harness {
		t.Helper()
		h := newHarness(t)
		h.authority = s.client
		h.r.Client = s.client
		h.r.Config.SiteID, h.r.Config.AttachmentID = site, "infra-"+site
		h.r.Config.Cluster.Name, h.r.Config.Cluster.UID = "infra-"+site, s.uid
		pool := "10.241.0.0/16"
		if site == "site-b" {
			pool = "10.242.0.0/16"
		}
		h.r.Config.Grants = []siteconfig.Grant{{GlobalVpcID: globalID, DelegatedPrefixes: []string{pool}, TransitCIDR: "10.240.255.0/30", RouterIP: "10.240.255.1", GatewayIP: "10.240.255.2"}}
		h.infra = &observedClient{Client: s.client, h: h}
		h.r.Store = legacy.ResourceStore{Client: h.infra, Config: localConfig(h.r.Config)}
		v := &api.SiteVpc{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: api.SiteVpcSpec{GlobalVpcID: globalID, SiteID: site, AttachmentID: "infra-" + site, CIDR: cidr, Gateway: gateway}}
		if err := s.client.Create(ctx, v); err != nil {
			t.Fatal(err)
		}
		h.request = ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}
		var err error
		h.p, err = siteplan.Build(v, h.r.Config)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	ha := attach(a, "site-a", "tenant-red", "red-a", "10.241.0.0/24", "10.241.0.1")
	hb := attach(b, "site-b", "tenant-red", "red-b", "10.242.0.0/24", "10.242.0.1")
	ha.converge()
	hb.converge()
	if ha.current().UID == hb.current().UID || ha.p.GlobalVpcID != hb.p.GlobalVpcID {
		t.Fatal("global identity is coupled to local API UID")
	}
	if len(ha.current().Status.Resources) != 3 || len(ha.current().Status.GatewayResources) != 2 {
		t.Fatal("real API pruned resource receipts")
	}
	changed := ha.current()
	changed.Spec.CIDR = "10.241.1.0/24"
	if err := a.client.Update(ctx, changed); !apierrors.IsInvalid(err) {
		t.Fatalf("CEL allowed immutable SiteVpc ownership mutation: %v", err)
	}
	before := ha.current().Status
	ha.reconcile()
	if !reflect.DeepEqual(before, ha.current().Status) {
		t.Fatal("real API steady replay changes status")
	}
	b.stop()
	var offline corev1.Namespace
	if err := b.client.Get(ctx, client.ObjectKey{Name: "kube-system"}, &offline); err == nil {
		t.Fatal("site B API was not actually stopped")
	}
	// Repair A while the other authority/Infra API and its etcd are stopped.
	o := object("Vpc", ha.p.VpcName)
	if err := a.client.Get(ctx, client.ObjectKeyFromObject(o), o); err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedSlice(o.Object, []interface{}{}, "spec", "staticRoutes")
	if err := a.client.Update(ctx, o); err != nil {
		t.Fatal(err)
	}
	ha.reconcile()
	if err := a.client.Get(ctx, client.ObjectKeyFromObject(o), o); err != nil {
		t.Fatal(err)
	}
	routes, _, _ := unstructured.NestedSlice(o.Object, "spec", "staticRoutes")
	if len(routes) != 1 || ha.current().Status.Phase != "LocalReady" {
		t.Fatal("peer API outage blocked local drift repair")
	}
	// A fresh local tenant can be created without reading the stopped API.
	blue := attach(a, "site-a", "tenant-blue", "blue-a", "10.241.2.0/24", "10.241.2.1")
	ha.r.Config.Grants = append(ha.r.Config.Grants, blue.r.Config.Grants[0])
	ha.reconcile()
	if ha.current().Status.Phase != "LocalReady" {
		t.Fatal("unrelated local grant invalidated accepted tenant")
	}
	blue.converge()
	blue.network.endpoints = false
	blue.deleting()
	blue.reconcile()
	if blue.current().DeletionTimestamp.IsZero() || len(blue.current().Finalizers) == 0 {
		t.Fatal("real API finalizer did not protect active endpoints")
	}
	blue.network.endpoints = true
	blue.finishDeletion()
	ha.reconcile()
	if ha.current().Status.Phase != "LocalReady" {
		t.Fatal("deleting another tenant damaged existing attachment")
	}
	ha.deleting()
	ha.finishDeletion()
	t.Log("Two real API/etcd stores validated stable global identity, schema immutability, receipt persistence, drift repair, new tenant creation and local detach while the peer API was stopped; OVN and gateway were explicit doubles")
}
