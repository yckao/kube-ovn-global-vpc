//go:build integration

package localcontroller

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	api "globalvpc.io/controller/api/v1alpha2"
	"globalvpc.io/controller/internal/platformconfig"
	"globalvpc.io/controller/internal/platformcontroller"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

type controlServer struct {
	c       client.Client
	env     *envtest.Environment
	running bool
}

func (s *controlServer) stop(t *testing.T) {
	t.Helper()
	if s.running {
		s.running = false
		if e := s.env.Stop(); e != nil {
			t.Error(e)
		}
	}
}
func simulatorCRD(kind, plural string) *apiextv1.CustomResourceDefinition {
	preserve := true
	fields := map[string]apiextv1.JSONSchemaProps{}
	if kind == "Vpc" {
		fields["destinationRoutes"] = apiextv1.JSONSchemaProps{Type: "array", Items: &apiextv1.JSONSchemaPropsOrArray{Schema: &apiextv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: &preserve}}}
	}
	shape := &apiextv1.JSONSchemaProps{Type: "object", Properties: map[string]apiextv1.JSONSchemaProps{"spec": {Type: "object", XPreserveUnknownFields: &preserve, Properties: fields}, "status": {Type: "object", XPreserveUnknownFields: &preserve}}}
	return &apiextv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: plural + ".kubeovn.io"}, Spec: apiextv1.CustomResourceDefinitionSpec{Group: "kubeovn.io", Scope: apiextv1.ClusterScoped, Names: apiextv1.CustomResourceDefinitionNames{Kind: kind, ListKind: kind + "List", Plural: plural, Singular: plural[:len(plural)-1]}, Versions: []apiextv1.CustomResourceDefinitionVersion{{Name: "v1", Served: true, Storage: true, Subresources: &apiextv1.CustomResourceSubresources{Status: &apiextv1.CustomResourceSubresourceStatus{}}, Schema: &apiextv1.CustomResourceValidation{OpenAPIV3Schema: shape}}}}}
}
func startControlServer(t *testing.T, local bool) *controlServer {
	t.Helper()
	e := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd")}, ErrorIfCRDPathMissing: true, ControlPlaneStartTimeout: 45 * time.Second, ControlPlaneStopTimeout: 20 * time.Second}
	if local {
		e.CRDs = []*apiextv1.CustomResourceDefinition{simulatorCRD("Vpc", "vpcs"), simulatorCRD("Subnet", "subnets"), simulatorCRD("IP", "ips")}
	}
	cfg, err := e.Start()
	if err != nil {
		t.Fatal(err)
	}
	s := &controlServer{env: e, running: true}
	t.Cleanup(func() { s.stop(t) })
	s.c, err = client.New(cfg, client.Options{Scheme: localTestScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func ensureNamespace(t *testing.T, c client.Client, name string) *corev1.Namespace {
	t.Helper()
	ns := &corev1.Namespace{}
	e := c.Get(context.Background(), client.ObjectKey{Name: name}, ns)
	if apierrors.IsNotFound(e) {
		ns.Name = name
		e = c.Create(context.Background(), ns)
	}
	if e != nil {
		t.Fatal(e)
	}
	return ns
}

// Three real API servers exercise the complete authority -> sync -> local native
// intent -> native/gateway test doubles -> status sync -> authority control chain.
// The native CRDs and protocol acknowledgements are explicit simulators. This
// proves API ownership/lifecycle/isolation, not OVSDB or cross-site forwarding.
func TestThreeAPIsManagedControlChainAndAuthorityOutage(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS must reference installed envtest binaries")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	authority := startControlServer(t, false)
	a := startControlServer(t, true)
	b := startControlServer(t, true)
	for _, name := range []string{"project", "location-a", "location-b", "global-vpc-system"} {
		ensureNamespace(t, authority.c, name)
	}
	locations := []*controlServer{a, b}
	syncers := []*Syncer{}
	locals := []*Reconciler{}
	gateways := []*gatewayDouble{}
	for i, server := range locations {
		ensureNamespace(t, server.c, "operator-system")
		ensureNamespace(t, server.c, "workloads")
		ns := ensureNamespace(t, server.c, "kube-system")
		cfg := localFixtureConfig()
		cfg.ClusterUID = string(ns.UID)
		cfg.ControlPool = "172.30.0.0/16"
		if i == 1 {
			cfg.LocationRef = "dc-b"
			cfg.AuthorityNamespace = "location-b"
			cfg.TransitPool = "10.253.0.0/16"
			cfg.BFDSourcePool = "10.254.0.0/16"
			cfg.ControlPool = "172.31.0.0/16"
		}
		g := &gatewayDouble{ready: true}
		gateways = append(gateways, g)
		locals = append(locals, &Reconciler{Client: server.c, Config: cfg, Gateway: g})
		syncers = append(syncers, &Syncer{Authority: authority.c, Local: server.c, Config: cfg})
	}
	pc := platformconfig.Config{NetworkClasses: map[string]platformconfig.NetworkClass{"default": {TransportProfile: "wireguard-bgp"}}, Locations: []platformconfig.Location{{Name: "dc-a", Region: "region-one", Site: "site-a", DC: "dc-a", BindingNamespace: "location-a", AllowedProjects: []string{"project"}, CIDRPools: []string{"10.240.0.0/14"}}, {Name: "dc-b", Region: "region-one", Site: "site-b", DC: "dc-b", BindingNamespace: "location-b", AllowedProjects: []string{"project"}, CIDRPools: []string{"10.240.0.0/14"}}}}
	controller := &platformcontroller.Reconciler{Client: authority.c, Config: pc}
	vpc := &api.VPC{ObjectMeta: metav1.ObjectMeta{Namespace: "project", Name: "red"}}
	if e := authority.c.Create(ctx, vpc); e != nil {
		t.Fatal(e)
	}
	for _, x := range []struct{ name, location, cidr string }{{"apps", "dc-a", "10.241.0.0/24"}, {"db", "dc-a", "10.241.1.0/24"}, {"remote", "dc-b", "10.242.0.0/24"}} {
		s := &api.Subnet{ObjectMeta: metav1.ObjectMeta{Namespace: "project", Name: x.name}, Spec: api.SubnetSpec{VPCRef: "red", LocationRef: x.location, CIDR: x.cidr}}
		if e := authority.c.Create(ctx, s); e != nil {
			t.Fatal(e)
		}
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vpc)}
	cycle := func() {
		t.Helper()
		if _, e := controller.Reconcile(ctx, request); e != nil {
			t.Fatal(e)
		}
		for i, server := range locations {
			if e := syncers[i].Once(ctx); e != nil {
				t.Fatal(e)
			}
			var list api.NetworkBindingList
			if e := server.c.List(ctx, &list, client.InNamespace("operator-system")); e != nil {
				t.Fatal(e)
			}
			for _, snapshot := range list.Items {
				if _, e := locals[i].Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&snapshot)}); e != nil {
					t.Fatal(e)
				}
			}
			simulateNative(t, server.c, true)
			if e := syncers[i].Once(ctx); e != nil {
				t.Fatal(e)
			}
		}
	}
	ready := false
	for i := 0; i < 80; i++ {
		cycle()
		if e := authority.c.Get(ctx, request.NamespacedName, vpc); e != nil {
			t.Fatal(e)
		}
		if meta.IsStatusConditionTrue(vpc.Status.Conditions, "Ready") {
			ready = true
			break
		}
	}
	if !ready {
		t.Fatalf("complete control chain failed to converge: %+v", vpc.Status)
	}
	if gateways[0].ensures == 0 || gateways[1].ensures == 0 {
		t.Fatal("gateway lifecycle was bypassed by fabricated authority status")
	}
	var apps api.Subnet
	if e := authority.c.Get(ctx, client.ObjectKey{Namespace: "project", Name: "apps"}, &apps); e != nil {
		t.Fatal(e)
	}
	nativeA := native("Vpc", apps.Status.NativeVpcName)
	if e := a.c.Get(ctx, client.ObjectKeyFromObject(nativeA), nativeA); e != nil {
		t.Fatal(e)
	}
	nativeUID := nativeA.GetUID()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "in-use", Namespace: "workloads", Annotations: map[string]string{"ovn.kubernetes.io/logical_switch": apps.Status.NativeSubnetName}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "placeholder", Image: "example.invalid/never-started"}}}}
	if e := a.c.Create(ctx, pod); e != nil {
		t.Fatal(e)
	}
	if e := authority.c.Delete(ctx, &apps); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 10; i++ {
		cycle()
	}
	if e := authority.c.Get(ctx, client.ObjectKeyFromObject(&apps), &apps); e != nil || apps.DeletionTimestamp == nil {
		t.Fatal("in-use public subnet did not remain protected")
	}
	var localB api.NetworkBindingList
	if e := b.c.List(ctx, &localB, client.InNamespace("operator-system")); e != nil {
		t.Fatal(e)
	}
	kept := false
	for _, remote := range localB.Items[0].Spec.RemoteSubnets {
		if remote.CIDR == "10.241.0.0/24" {
			kept = true
		}
	}
	if !kept {
		t.Fatal("in-use delete prematurely removed the remote route")
	}
	if e := a.c.Delete(ctx, pod, client.GracePeriodSeconds(0)); e != nil {
		t.Fatal(e)
	}
	removed := false
	for i := 0; i < 70; i++ {
		cycle()
		e := authority.c.Get(ctx, client.ObjectKey{Namespace: "project", Name: "apps"}, &apps)
		if apierrors.IsNotFound(e) {
			removed = true
			break
		}
		if e != nil {
			t.Fatal(e)
		}
	}
	if !removed {
		t.Fatal("source drain and remote withdrawal did not finalize subnet")
	}
	for i := 0; i < 15; i++ {
		cycle()
	}
	if e := a.c.Get(ctx, client.ObjectKeyFromObject(nativeA), nativeA); e != nil || nativeA.GetUID() != nativeUID {
		t.Fatal("subnet deletion changed native VPC identity")
	}
	var db api.Subnet
	if e := authority.c.Get(ctx, client.ObjectKey{Namespace: "project", Name: "db"}, &db); e != nil || !meta.IsStatusConditionTrue(db.Status.Conditions, "Ready") {
		t.Fatal("sibling subnet did not recover ready")
	}
	before := nativeA.DeepCopy()
	var snapshots api.NetworkBindingList
	if e := a.c.List(ctx, &snapshots, client.InNamespace("operator-system")); e != nil {
		t.Fatal(e)
	}
	snapshot := snapshots.Items[0].DeepCopy()
	authority.stop(t)
	failed, stop := context.WithTimeout(context.Background(), time.Second)
	e := syncers[0].Once(failed)
	stop()
	if e == nil {
		t.Fatal("stopped authority API unexpectedly remained reachable")
	}
	// Restart only the local reconciler; accepted snapshots and allocation receipts
	// remain in the independently running site's API.
	restarted := &Reconciler{Client: a.c, Config: locals[0].Config, Gateway: gateways[0]}
	for i := 0; i < 5; i++ {
		if _, e = restarted.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(snapshot)}); e != nil {
			t.Fatal(e)
		}
		simulateNative(t, a.c, true)
	}
	var retained api.NetworkBinding
	if e = a.c.Get(context.Background(), client.ObjectKeyFromObject(snapshot), &retained); e != nil {
		t.Fatal(e)
	}
	if retained.Spec.Revision != snapshot.Spec.Revision || retained.Status.AppliedRevision != snapshot.Spec.Revision {
		t.Fatal("local restart during authority outage lost accepted configuration")
	}
	if e = a.c.Get(context.Background(), client.ObjectKeyFromObject(nativeA), nativeA); e != nil {
		t.Fatal(e)
	}
	if nativeA.GetUID() != before.GetUID() || !reflect.DeepEqual(nativeA.Object["spec"], before.Object["spec"]) {
		t.Fatal("authority outage changed local native routing")
	}
	routes, _, _ := unstructured.NestedSlice(nativeA.Object, "spec", "destinationRoutes")
	if len(routes) == 0 {
		t.Fatal("test did not preserve any cross-site route intent")
	}
}
