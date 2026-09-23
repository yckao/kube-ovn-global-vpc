package sitecontroller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	api "globalvpc.io/controller/api/v1alpha1"
	"globalvpc.io/controller/internal/config"
	legacy "globalvpc.io/controller/internal/controller"
	"globalvpc.io/controller/internal/ovn"
	"globalvpc.io/controller/internal/siteconfig"
	"globalvpc.io/controller/internal/sitegateway"
	"globalvpc.io/controller/internal/siteplan"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type harness struct {
	t         *testing.T
	r         *Reconciler
	p         *siteplan.Plan
	authority client.Client
	infra     *observedClient
	gateway   *gatewayDouble
	network   *networkDouble
	request   ctrl.Request
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := api.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"Vpc", "Subnet"} {
		s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "kubeovn.io", Version: "v1", Kind: kind}, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "kubeovn.io", Version: "v1", Kind: kind + "List"}, &unstructured.UnstructuredList{})
	}
	return s
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	s := testScheme(t)
	v := &api.SiteVpc{ObjectMeta: metav1.ObjectMeta{Name: "tenant", UID: "local-tenant-uid", Generation: 1}, Spec: api.SiteVpcSpec{GlobalVpcID: "tenant-red", SiteID: "site-a", AttachmentID: "infra-a", CIDR: "10.241.0.0/24", Gateway: "10.241.0.1"}}
	cfg := siteconfig.Config{SiteID: "site-a", AttachmentID: "infra-a", Cluster: siteconfig.Cluster{Name: "infra-a", UID: "cluster-a-uid", Kubeconfig: "local.conf", NBCommand: []string{"local-nbctl"}, SBCommand: []string{"local-sbctl"}}, GatewayCommand: []string{"local-gateway"}, GatewayRevision: "registration-v1", Grants: []siteconfig.Grant{{GlobalVpcID: "tenant-red", DelegatedPrefixes: []string{"10.241.0.0/16"}, TransitCIDR: "10.240.255.0/30", RouterIP: "10.240.255.1", GatewayIP: "10.240.255.2"}}}
	authority := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&api.SiteVpc{}).WithObjects(v).Build()
	h := &harness{t: t, authority: authority, request: ctrl.Request{NamespacedName: types.NamespacedName{Name: v.Name}}}
	local := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(object("Subnet", "")).WithObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID(cfg.Cluster.UID)}}).Build()
	h.infra = &observedClient{Client: local, h: h, assignUID: true}
	h.network = &networkDouble{endpoints: true, absent: true, routes: true}
	h.gateway = &gatewayDouble{h: h, ready: true, endpoints: true, records: map[string][]api.GatewayResourceRecord{}}
	h.r = &Reconciler{Client: authority, Config: cfg, Store: legacy.ResourceStore{Client: h.infra, Config: localConfig(cfg)}, Gateway: h.gateway, Network: h.network}
	var err error
	h.p, err = siteplan.Build(v, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func localConfig(c siteconfig.Config) config.Cluster {
	return config.Cluster{Name: c.Cluster.Name, UID: c.Cluster.UID, Site: c.SiteID, Kubeconfig: c.Cluster.Kubeconfig, NBCommand: c.Cluster.NBCommand, SBCommand: c.Cluster.SBCommand}
}

func object(kind, name string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetAPIVersion("kubeovn.io/v1")
	o.SetKind(kind)
	o.SetName(name)
	return o
}

func (h *harness) current() *api.SiteVpc {
	h.t.Helper()
	var v api.SiteVpc
	if err := h.authority.Get(context.Background(), h.request.NamespacedName, &v); err != nil {
		h.t.Fatal(err)
	}
	return &v
}

func (h *harness) reconcile() {
	h.t.Helper()
	if _, err := h.r.Reconcile(context.Background(), h.request); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) durable() {
	h.t.Helper()
	v := h.current()
	if !controllerutil.ContainsFinalizer(v, api.SiteFinalizer) || v.Status.PlanHash != h.p.Hash {
		h.t.Fatal("external mutation before durable local ownership")
	}
}

func (h *harness) readySubnets() {
	h.t.Helper()
	for _, name := range []string{h.p.SubnetName, h.p.TransitName} {
		o := object("Subnet", name)
		if err := h.infra.Get(context.Background(), client.ObjectKeyFromObject(o), o); err != nil {
			h.t.Fatal(err)
		}
		_ = unstructured.SetNestedSlice(o.Object, []interface{}{map[string]interface{}{"type": "Ready", "status": "True"}}, "status", "conditions")
		if err := h.infra.Status().Update(context.Background(), o); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *harness) converge() {
	h.t.Helper()
	h.reconcile()
	h.reconcile()
	h.readySubnets()
	h.reconcile()
	h.reconcile()
	if v := h.current(); v.Status.Phase != "LocalReady" || !meta.IsStatusConditionTrue(v.Status.Conditions, "LocalReady") || !meta.IsStatusConditionTrue(v.Status.Conditions, "GatewayConfigured") {
		h.t.Fatalf("not locally ready: %+v", v.Status)
	}
}

func (h *harness) deleting() {
	h.t.Helper()
	if err := h.authority.Delete(context.Background(), h.current()); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) finishDeletion() {
	h.t.Helper()
	for i := 0; i < 16; i++ {
		h.reconcile()
		var v api.SiteVpc
		err := h.authority.Get(context.Background(), h.request.NamespacedName, &v)
		if apierrors.IsNotFound(err) {
			return
		}
		if err != nil {
			h.t.Fatal(err)
		}
	}
	h.t.Fatalf("local finalizer did not complete: %+v", h.current().Status)
}

type observedClient struct {
	client.Client
	h                       *harness
	writes, deletes, serial int
	assignUID               bool
}

func (c *observedClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.h.durable()
	c.writes++
	c.serial++
	if c.assignUID && obj.GetUID() == "" {
		obj.SetUID(types.UID(fmt.Sprintf("resource-%d", c.serial)))
	}
	return c.Client.Create(ctx, obj, opts...)
}
func (c *observedClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.h.durable()
	c.writes++
	return c.Client.Update(ctx, obj, opts...)
}
func (c *observedClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.h.durable()
	c.writes++
	c.deletes++
	return c.Client.Delete(ctx, obj, opts...)
}

type networkDouble struct {
	endpoints, absent, routes bool
	err                       error
	calls                     int
	observedRoutes            []ovn.Route
	observedRouter            string
}

func (n *networkDouble) EndpointsEmpty(context.Context, config.Cluster, string) (bool, error) {
	n.calls++
	return n.endpoints, n.err
}
func (n *networkDouble) LocalAbsent(context.Context, config.Cluster, string, string, string) (bool, error) {
	n.calls++
	return n.absent, n.err
}
func (n *networkDouble) RoutesReady(_ context.Context, _ config.Cluster, router string, routes []ovn.Route) (bool, error) {
	n.calls++
	n.observedRouter = router
	n.observedRoutes = append([]ovn.Route(nil), routes...)
	return n.routes, n.err
}

type gatewayDouble struct {
	h                                           *harness
	ready, endpoints                            bool
	getErr, ensureErr, deleteErr                error
	loseEnsure, loseDelete                      bool
	ensureCalls, deleteCalls, mutations, serial int
	records                                     map[string][]api.GatewayResourceRecord
}

func (g *gatewayDouble) result(c siteplan.GatewayConfig) sitegateway.Result {
	records := append([]api.GatewayResourceRecord(nil), g.records[c.OwnerUID]...)
	return sitegateway.Result{Version: "v1", OwnerUID: c.OwnerUID, GlobalVpcID: c.GlobalVpcID, GatewayRevision: c.GatewayRevision, Ready: g.ready && len(records) == 2, Absent: len(records) == 0, EndpointsEmpty: g.endpoints, Resources: records}
}
func (g *gatewayDouble) check(c siteplan.GatewayConfig, expected []api.GatewayResourceRecord, ensure bool) error {
	for _, old := range expected {
		found := false
		for _, actual := range g.records[c.OwnerUID] {
			if old.Name == actual.Name && old.Kind == actual.Kind {
				if old.UID != actual.UID {
					return errors.New("gateway resource UID changed")
				}
				found = true
			}
		}
		if ensure && !found {
			return errors.New("persist gateway resource absence before recreating")
		}
	}
	return nil
}
func (g *gatewayDouble) Get(_ context.Context, c siteplan.GatewayConfig, expected []api.GatewayResourceRecord) (sitegateway.Result, error) {
	if g.getErr != nil {
		return sitegateway.Result{}, g.getErr
	}
	if err := g.check(c, expected, false); err != nil {
		return sitegateway.Result{}, err
	}
	return g.result(c), nil
}
func (g *gatewayDouble) Ensure(_ context.Context, c siteplan.GatewayConfig, expected []api.GatewayResourceRecord) (sitegateway.Result, error) {
	g.h.durable()
	g.ensureCalls++
	if g.ensureErr != nil {
		return sitegateway.Result{}, g.ensureErr
	}
	if err := g.check(c, expected, true); err != nil {
		return sitegateway.Result{}, err
	}
	for _, kind := range []string{"Pod", "ConfigMap"} {
		found := false
		for _, r := range g.records[c.OwnerUID] {
			if r.Kind == kind {
				found = true
			}
		}
		if !found {
			g.serial++
			g.mutations++
			g.records[c.OwnerUID] = append(g.records[c.OwnerUID], api.GatewayResourceRecord{APIVersion: "v1", Kind: kind, Namespace: "gateway-system", Name: c.VpcName, UID: fmt.Sprintf("gateway-%d", g.serial)})
		}
	}
	if g.loseEnsure {
		g.loseEnsure = false
		return sitegateway.Result{}, sitegateway.ErrUnknownState
	}
	return g.result(c), nil
}
func (g *gatewayDouble) Delete(_ context.Context, c siteplan.GatewayConfig, expected []api.GatewayResourceRecord) (sitegateway.Result, error) {
	g.h.durable()
	g.deleteCalls++
	if g.h.current().Status.Operation != "DeleteGateway" {
		g.h.t.Fatal("gateway deletion before durable operation")
	}
	if g.deleteErr != nil {
		return sitegateway.Result{}, g.deleteErr
	}
	if err := g.check(c, expected, false); err != nil {
		return sitegateway.Result{}, err
	}
	g.records[c.OwnerUID] = nil
	if g.loseDelete {
		g.loseDelete = false
		return sitegateway.Result{}, sitegateway.ErrUnknownState
	}
	return g.result(c), nil
}
func (g *gatewayDouble) EndpointsEmpty(_ context.Context, c siteplan.GatewayConfig, expected []api.GatewayResourceRecord) (sitegateway.Result, error) {
	if err := g.check(c, expected, false); err != nil {
		return sitegateway.Result{}, err
	}
	return g.result(c), nil
}

func TestIndependentLocalLifecycleAndReplay(t *testing.T) {
	h := newHarness(t)
	h.converge()
	before, writes, mutations := h.current().Status, h.infra.writes, h.gateway.mutations
	h.reconcile()
	if !reflect.DeepEqual(before, h.current().Status) || h.infra.writes != writes || h.gateway.mutations != mutations {
		t.Fatal("steady replay mutated persisted state")
	}
	h.deleting()
	h.finishDeletion()
	for _, o := range []*unstructured.Unstructured{h.p.Vpc, h.p.Subnet, h.p.Transit} {
		if err := h.infra.Get(context.Background(), client.ObjectKeyFromObject(o), o.DeepCopy()); !apierrors.IsNotFound(err) {
			t.Fatalf("local object survived deletion: %v", err)
		}
	}
	if h.gateway.deleteCalls == 0 {
		t.Fatal("local gateway was not withdrawn")
	}
}

func TestLocalReadinessRequiresDefaultAndKubeOVNSourceRoutes(t *testing.T) {
	h := newHarness(t)
	h.converge()
	want := map[ovn.Route]bool{
		{CIDR: "0.0.0.0/0", NextHopIP: h.p.Gateway.GatewayIP, Policy: "dst-ip"}:              true,
		{CIDR: h.current().Spec.CIDR, NextHopIP: h.current().Spec.Gateway, Policy: "src-ip"}: true,
		{CIDR: h.p.Gateway.TransitCIDR, NextHopIP: h.p.Gateway.RouterIP, Policy: "src-ip"}:   true,
	}
	if h.network.observedRouter != h.p.VpcName || len(h.network.observedRoutes) != len(want) {
		t.Fatal("local readiness did not request the exact tenant router route set")
	}
	for _, route := range h.network.observedRoutes {
		if route.Policy == "" {
			route.Policy = "dst-ip"
		}
		if !want[route] {
			t.Fatalf("unexpected route in local readiness: %+v", route)
		}
		delete(want, route)
	}
	if len(want) != 0 {
		t.Fatal("local readiness omitted a Kube-OVN-owned route")
	}
}

func TestLocalDriftRepairAndMissingResourceRecovery(t *testing.T) {
	h := newHarness(t)
	h.converge()
	o := object("Vpc", h.p.VpcName)
	if err := h.infra.Get(context.Background(), client.ObjectKeyFromObject(o), o); err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedSlice(o.Object, []interface{}{}, "spec", "staticRoutes")
	if err := h.infra.Client.Update(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	if err := h.infra.Get(context.Background(), client.ObjectKeyFromObject(o), o); err != nil {
		t.Fatal(err)
	}
	routes, _, _ := unstructured.NestedSlice(o.Object, "spec", "staticRoutes")
	if len(routes) != 1 {
		t.Fatal("local route drift was not repaired")
	}
	old := recorded(h.current(), "Subnet", h.p.SubnetName)
	if err := h.infra.Client.Delete(context.Background(), object("Subnet", h.p.SubnetName)); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	if recorded(h.current(), "Subnet", h.p.SubnetName) != "" {
		t.Fatal("missing object permission was not durably cleared")
	}
	if err := h.infra.Get(context.Background(), client.ObjectKey{Name: h.p.SubnetName}, object("Subnet", "")); !apierrors.IsNotFound(err) {
		t.Fatal("recreation occurred before persisted absence")
	}
	h.reconcile()
	h.readySubnets()
	h.reconcile()
	if got := recorded(h.current(), "Subnet", h.p.SubnetName); got == "" || got == old {
		t.Fatal("recreated local resource UID was not durably recorded")
	}
}

func TestForeignOwnershipPreflightStopsAllLocalWrites(t *testing.T) {
	h := newHarness(t)
	foreign := h.p.Subnet.DeepCopy()
	foreign.SetUID("foreign-resource")
	foreign.SetLabels(map[string]string{api.OwnerLabel: "another-owner", api.ClusterLabel: h.r.Config.Cluster.UID})
	if err := h.infra.Client.Create(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	h.reconcile()
	if h.current().Status.Phase != "Blocked" || h.infra.writes != 0 || h.gateway.ensureCalls != 0 {
		t.Fatal("ownership conflict was detected after side effects")
	}
}

func TestRecordedUIDReplacementIsNeverAdopted(t *testing.T) {
	h := newHarness(t)
	h.converge()
	o := object("Subnet", h.p.SubnetName)
	if err := h.infra.Client.Delete(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	replacement := h.p.Subnet.DeepCopy()
	replacement.SetUID("replacement-uid")
	if err := h.infra.Client.Create(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	writes := h.infra.writes
	h.reconcile()
	if h.current().Status.Phase != "Blocked" || h.infra.writes != writes {
		t.Fatal("recorded UID replacement was accepted")
	}
}

func TestLocalIdentityAndRegistrationChangesBlockRetargeting(t *testing.T) {
	for _, mode := range []string{"cluster-identity", "gateway-revision"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t)
			h.converge()
			if mode == "gateway-revision" {
				h.r.Config.GatewayRevision = "registration-v2"
			} else {
				var ns corev1.Namespace
				if err := h.infra.Client.Get(context.Background(), client.ObjectKey{Name: "kube-system"}, &ns); err != nil {
					t.Fatal(err)
				}
				ns.UID = "other-cluster"
				if err := h.infra.Client.Update(context.Background(), &ns); err != nil {
					t.Fatal(err)
				}
			}
			writes, calls := h.infra.writes, h.gateway.ensureCalls
			h.reconcile()
			if h.current().Status.Phase != "Blocked" || h.infra.writes != writes || h.gateway.ensureCalls != calls {
				t.Fatal("registration/identity mismatch did not stop writes")
			}
		})
	}
}

func TestEndpointsAndUnknownGatewayKeepFinalizerAndResources(t *testing.T) {
	for _, mode := range []string{"ovn-endpoints", "gateway-endpoints", "unknown-get", "unknown-delete"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t)
			h.converge()
			h.deleting()
			switch mode {
			case "ovn-endpoints":
				h.network.endpoints = false
			case "gateway-endpoints":
				h.gateway.endpoints = false
			case "unknown-get":
				h.gateway.getErr = sitegateway.ErrUnknownState
			case "unknown-delete":
				h.gateway.deleteErr = sitegateway.ErrUnknownState
			}
			h.reconcile()
			h.reconcile()
			h.reconcile()
			v := h.current()
			if !controllerutil.ContainsFinalizer(v, api.SiteFinalizer) || len(v.Status.GatewayResources) != 2 || h.infra.deletes != 0 {
				t.Fatalf("uncertain detach removed ownership or resources: %+v", v.Status)
			}
			if mode != "unknown-delete" && h.gateway.deleteCalls != 0 {
				t.Fatal("gateway withdrawn before endpoints and state verified")
			}
		})
	}
}

func TestGatewayResponseLossRecoversWithoutDuplicateResources(t *testing.T) {
	h := newHarness(t)
	h.reconcile()
	h.reconcile()
	h.readySubnets()
	h.gateway.loseEnsure = true
	h.reconcile()
	if h.current().Status.Phase != "Blocked" || len(h.current().Status.GatewayResources) != 0 {
		t.Fatal("response loss was accepted as a receipt")
	}
	h.reconcile()
	h.reconcile()
	if h.current().Status.Phase != "LocalReady" || h.gateway.mutations != 2 || len(h.current().Status.GatewayResources) != 2 {
		t.Fatal("response-loss recovery duplicated or lost gateway resources")
	}
	h.deleting()
	h.reconcile()
	h.gateway.loseDelete = true
	h.reconcile()
	if !controllerutil.ContainsFinalizer(h.current(), api.SiteFinalizer) || h.infra.deletes != 0 {
		t.Fatal("unknown gateway deletion discarded local resources")
	}
	h.finishDeletion()
}

func TestPartialGatewayLossPersistsAbsenceBeforeRecreation(t *testing.T) {
	h := newHarness(t)
	h.converge()
	old := h.gateway.records[h.p.OwnerUID]
	var podUID string
	for _, r := range old {
		if r.Kind == "Pod" {
			podUID = r.UID
		}
	}
	kept := []api.GatewayResourceRecord{}
	for _, r := range old {
		if r.Kind != "Pod" {
			kept = append(kept, r)
		}
	}
	h.gateway.records[h.p.OwnerUID] = kept
	mutations := h.gateway.mutations
	h.reconcile()
	if len(h.current().Status.GatewayResources) != 1 || h.gateway.mutations != mutations {
		t.Fatal("verified partial absence must be persisted before requesting recreation")
	}
	h.reconcile()
	h.reconcile()
	if h.current().Status.Phase != "LocalReady" || len(h.current().Status.GatewayResources) != 2 {
		t.Fatalf("partial gateway loss did not recover: %+v", h.current().Status)
	}
	for _, r := range h.current().Status.GatewayResources {
		if r.Kind == "Pod" && r.UID == podUID {
			t.Fatal("recreated gateway kept old UID")
		}
	}
}

func TestLocalOVNCleanupMustCompleteBeforeFinalization(t *testing.T) {
	h := newHarness(t)
	h.converge()
	h.deleting()
	h.network.absent = false
	for i := 0; i < 10; i++ {
		h.reconcile()
	}
	if !controllerutil.ContainsFinalizer(h.current(), api.SiteFinalizer) {
		t.Fatal("finalized before local OVN absence")
	}
	h.network.absent = true
	h.finishDeletion()
}

func TestLateGatewayResourceIsRemovedBeforeLocalResources(t *testing.T) {
	h := newHarness(t)
	h.converge()
	h.deleting()
	h.reconcile()
	h.reconcile()
	if h.current().Status.Operation != "DeleteResources" {
		t.Fatal("gateway deletion did not reach durable resource stage")
	}
	h.gateway.records[h.p.OwnerUID] = []api.GatewayResourceRecord{{APIVersion: "v1", Kind: "ConfigMap", Namespace: "gateway-system", Name: h.p.VpcName, UID: "late-gateway-config"}}
	h.reconcile()
	if h.current().Status.Operation != "DeleteGateway" || len(h.current().Status.GatewayResources) != 1 || h.infra.deletes != 0 {
		t.Fatal("late gateway object bypassed cleanup")
	}
	h.finishDeletion()
}
