package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	api "globalvpc.io/controller/api/v1alpha1"
	"globalvpc.io/controller/internal/config"
	"globalvpc.io/controller/internal/ovn"
	"globalvpc.io/controller/internal/plan"
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
)

type testHarness struct {
	t         *testing.T
	r         *Reconciler
	p         *plan.Plan
	remote    map[string]*observedClient
	ipam      *testIPAM
	network   *testNetwork
	gate      *testGate
	events    []string
	request   ctrl.Request
	authority client.Client
}

func newHarness(t *testing.T) *testHarness {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"Vpc", "Subnet"} {
		scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "kubeovn.io", Version: "v1", Kind: kind}, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "kubeovn.io", Version: "v1", Kind: kind + "List"}, &unstructured.UnstructuredList{})
	}
	v := &api.GlobalVpc{ObjectMeta: metav1.ObjectMeta{Name: "tenant", UID: "tenant-uid", Generation: 1}, Spec: api.GlobalVpcSpec{
		DomainRef: "lab", IPAM: api.IPAMReference{ProviderRef: "registry", ScopeRef: "tenant-vrf"}, TransitCIDR: "10.240.255.0/29",
		Attachments: []api.Attachment{
			{ClusterRef: "dc-a", CIDR: "10.241.0.0/24", Gateway: "10.241.0.1", TransitIP: "10.240.255.1"},
			{ClusterRef: "dc-b", CIDR: "10.242.0.0/24", Gateway: "10.242.0.1", TransitIP: "10.240.255.2"},
		},
	}}
	cfg := config.Config{Domain: "lab", ICNBCommand: []string{"ic-nbctl"}, Providers: map[string][]string{"registry": {"provider"}}}
	for _, suffix := range []string{"a", "b"} {
		cfg.Clusters = append(cfg.Clusters, config.Cluster{Name: "dc-" + suffix, UID: "cluster-" + suffix, Region: "region", Site: "site-" + suffix, DC: "dc-" + suffix,
			Kubeconfig: suffix + ".conf", NBCommand: []string{suffix + "-nbctl"}, SBCommand: []string{suffix + "-sbctl"}, GatewayChassis: []string{suffix + "-gw"}, AZName: "az-" + suffix,
			ICDeployment: config.DeploymentReference{Namespace: "ic", Name: "ovn-ic", UID: "ic-" + suffix}})
	}
	p, err := plan.Build(v, cfg)
	if err != nil {
		t.Fatal(err)
	}
	authority := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.GlobalVpc{}).WithObjects(v).Build()
	h := &testHarness{t: t, p: p, authority: authority, request: ctrl.Request{NamespacedName: types.NamespacedName{Name: v.Name}}, remote: map[string]*observedClient{}}
	h.ipam = &testIPAM{h: h, allocations: map[string]api.Allocation{}}
	h.gate = &testGate{h: h, pauseReady: true, resumeReady: true}
	h.network = &testNetwork{h: h, endpointsEmpty: true, localAbsent: true, connected: true, routesReady: true}
	h.r = &Reconciler{Client: authority, Config: cfg, Stores: map[string]ResourceStore{}, Providers: map[string]IPAM{"registry": h.ipam}, Network: h.network, Gate: h.gate}
	for _, c := range cfg.Clusters {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID(c.UID)}}
		base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(object("Subnet", "")).WithObjects(ns).Build()
		remote := &observedClient{Client: base, h: h, name: c.Name}
		h.remote[c.Name] = remote
		h.r.Stores[c.Name] = ResourceStore{Client: remote, Config: c}
	}
	return h
}

func (h *testHarness) current() *api.GlobalVpc {
	h.t.Helper()
	var v api.GlobalVpc
	if err := h.authority.Get(context.Background(), h.request.NamespacedName, &v); err != nil {
		h.t.Fatal(err)
	}
	return &v
}

func (h *testHarness) reconcile() {
	h.t.Helper()
	if _, err := h.r.Reconcile(context.Background(), h.request); err != nil {
		h.t.Fatal(err)
	}
}

func (h *testHarness) assertDurable() {
	h.t.Helper()
	v := h.current()
	if !contains(v.Finalizers, api.Finalizer) || v.Status.PlanHash != h.p.Hash {
		h.t.Fatalf("external side effect before durable finalizer and accepted hash: %#v", v)
	}
}

func (h *testHarness) readySubnets() {
	h.t.Helper()
	for _, site := range h.p.Attachments {
		for _, name := range []string{site.SubnetName, site.TransitName} {
			o := object("Subnet", name)
			c := h.remote[site.Cluster.Name]
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(o), o); err != nil {
				h.t.Fatal(err)
			}
			_ = unstructured.SetNestedSlice(o.Object, []interface{}{map[string]interface{}{"type": "Ready", "status": "True"}}, "status", "conditions")
			if err := c.Status().Update(context.Background(), o); err != nil {
				h.t.Fatal(err)
			}
		}
	}
}

func (h *testHarness) converge() {
	h.t.Helper()
	h.reconcile() // finalizer
	h.reconcile() // receipts and local resources
	h.readySubnets()
	h.reconcile() // quiesce, publish, resume
	h.reconcile() // verify IC and route convergence
	if v := h.current(); v.Status.Phase != "Ready" || !meta.IsStatusConditionTrue(v.Status.Conditions, "Ready") {
		h.t.Fatalf("did not converge: %+v", v.Status)
	}
}

func (h *testHarness) deleting() {
	h.t.Helper()
	if err := h.authority.Delete(context.Background(), h.current()); err != nil {
		h.t.Fatal(err)
	}
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

type observedClient struct {
	client.Client
	h       *testHarness
	name    string
	writes  int
	serial  int
	getErr  error
	history []int
}

type faultStatusClient struct {
	client.Client
	failOnce  bool
	committed bool
}

func (c *faultStatusClient) Status() client.SubResourceWriter {
	return &faultStatusWriter{SubResourceWriter: c.Client.Status(), parent: c}
}

type faultStatusWriter struct {
	client.SubResourceWriter
	parent *faultStatusClient
}

func (w *faultStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	v, ok := obj.(*api.GlobalVpc)
	if ok && w.parent.failOnce && v.Status.TransitSwitchUID != "" && v.Status.Operation == "Resume" {
		w.parent.failOnce = false
		if w.parent.committed {
			if err := w.SubResourceWriter.Update(ctx, obj, opts...); err != nil {
				return err
			}
		}
		return errors.New("status write response lost")
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func (c *observedClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if c.getErr != nil {
		return c.getErr
	}
	return c.Client.Get(ctx, key, obj, opts...)
}
func (c *observedClient) observe(obj client.Object) {
	c.h.assertDurable()
	c.writes++
	c.h.events = append(c.h.events, "resource:"+c.name+":"+obj.GetName())
	if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == "Vpc" {
		routes, _, _ := unstructured.NestedSlice(u.Object, "spec", "staticRoutes")
		c.history = append(c.history, len(routes))
	}
}
func (c *observedClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.observe(obj)
	if obj.GetUID() == "" {
		c.serial++
		obj.SetUID(types.UID(fmt.Sprintf("%s-resource-%d", c.name, c.serial)))
	}
	return c.Client.Create(ctx, obj, opts...)
}
func (c *observedClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.observe(obj)
	return c.Client.Update(ctx, obj, opts...)
}
func (c *observedClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.observe(obj)
	return c.Client.Delete(ctx, obj, opts...)
}

type testIPAM struct {
	h           *testHarness
	allocations map[string]api.Allocation
	calls       int
	err         error
	malformed   bool
}

func (p *testIPAM) Ensure(_ context.Context, claim api.PrefixClaim) (api.Allocation, error) {
	p.h.assertDurable()
	p.calls++
	p.h.events = append(p.h.events, "ipam")
	if p.err != nil {
		return api.Allocation{}, p.err
	}
	allocation, ok := p.allocations[claim.ClaimUID]
	if !ok {
		allocation = api.Allocation{PrefixClaim: claim, AllocationID: "reserved-" + claim.ClaimUID, ReleasePolicy: "Retain"}
		p.allocations[claim.ClaimUID] = allocation
	}
	if p.malformed {
		allocation.CIDR = "192.0.2.0/24"
	}
	return allocation, nil
}

type testGate struct {
	h           *testHarness
	holder      string
	paused      bool
	pauseReady  bool
	resumeReady bool
	pauses      int
	resumes     int
}

func (g *testGate) Holder(context.Context) (string, error) { return g.holder, nil }
func (g *testGate) Acquire(_ context.Context, uid, _ string) (bool, error) {
	g.h.assertDurable()
	if g.holder != "" && g.holder != uid {
		return false, nil
	}
	g.holder = uid
	g.h.events = append(g.h.events, "acquire")
	return true, nil
}
func (g *testGate) SetPaused(_ context.Context, uid string, paused bool) (bool, error) {
	g.h.assertDurable()
	if g.holder != uid {
		return false, errors.New("not lock holder")
	}
	if paused {
		g.pauses++
		g.h.events = append(g.h.events, "pause")
		g.paused = g.pauseReady
		return g.pauseReady, nil
	}
	g.resumes++
	g.h.events = append(g.h.events, "resume")
	g.paused = false
	return g.resumeReady, nil
}
func (g *testGate) Release(_ context.Context, uid string) error {
	if g.holder != uid {
		return errors.New("not lock holder")
	}
	g.h.events = append(g.h.events, "release")
	g.holder = ""
	return nil
}

type testNetwork struct {
	h                *testHarness
	transitUID       string
	transitOwner     string
	creations        int
	marks            int
	checks           int
	checkErr         error
	endpointsEmpty   bool
	endpointErr      error
	localAbsent      bool
	connected        bool
	routesReady      bool
	lostResponseOnce bool
}

func (n *testNetwork) Check(context.Context, config.Cluster) error { n.checks++; return n.checkErr }
func (n *testNetwork) InspectTransit(_ context.Context, _ []string, _, owner, expected string) (string, error) {
	if n.transitUID == "" {
		return "", nil
	}
	if n.transitOwner != owner {
		return "", errors.New("transit ownership conflict")
	}
	if expected != "" && expected != n.transitUID {
		return "", errors.New("transit UID changed")
	}
	return n.transitUID, nil
}
func (n *testNetwork) Mark(_ context.Context, _ config.Cluster, _ string) error {
	n.h.assertDurable()
	if n.transitUID == "" && !n.h.gate.paused {
		n.h.t.Fatal("marker written before native IC stopped")
	}
	n.marks++
	n.h.events = append(n.h.events, "mark")
	return nil
}
func (n *testNetwork) Gateways(context.Context, config.Cluster, string) error {
	n.h.assertDurable()
	n.h.events = append(n.h.events, "gateway")
	return nil
}
func (n *testNetwork) EnsureTransit(ctx context.Context, command []string, name, owner, expected string) (string, error) {
	n.h.assertDurable()
	if n.transitUID != "" {
		return n.InspectTransit(ctx, command, name, owner, expected)
	}
	if expected != "" {
		return "", ovn.ErrTransitMissing
	}
	if !n.h.gate.paused {
		n.h.t.Fatal("transit created before native IC stopped")
	}
	if n.h.current().Status.TransitSwitchUID != "" {
		n.h.t.Fatal("replacement transit created before old UUID was durably cleared")
	}
	n.creations++
	n.transitUID = fmt.Sprintf("transit-%d", n.creations)
	n.transitOwner = owner
	n.h.events = append(n.h.events, "transit")
	if n.lostResponseOnce {
		n.lostResponseOnce = false
		return "", errors.New("transaction response lost")
	}
	return n.transitUID, nil
}
func (n *testNetwork) DeleteTransit(context.Context, []string, string, string, string) error {
	if !n.h.gate.paused {
		n.h.t.Fatal("transit deleted before native IC stopped")
	}
	n.h.events = append(n.h.events, "delete-transit")
	n.transitUID = ""
	n.transitOwner = ""
	return nil
}
func (n *testNetwork) EndpointsEmpty(context.Context, config.Cluster, string) (bool, error) {
	return n.endpointsEmpty, n.endpointErr
}
func (n *testNetwork) LocalAbsent(context.Context, config.Cluster, string, string, string) (bool, error) {
	return n.localAbsent, nil
}
func (n *testNetwork) Connected(context.Context, config.Cluster, string, string, []string) (bool, error) {
	return n.connected, nil
}
func (n *testNetwork) RoutesReady(_ context.Context, _ config.Cluster, _ string, routes []ovn.Route) (bool, error) {
	if len(routes) != len(n.h.p.Attachments)-1 {
		n.h.t.Fatal("route verification omitted a remote prefix")
	}
	return n.routesReady, nil
}

func TestReconcilePersistsAuthorityBeforeSideEffects(t *testing.T) {
	h := newHarness(t)
	h.reconcile()
	if len(h.events) != 0 || !contains(h.current().Finalizers, api.Finalizer) || h.current().Status.PlanHash != "" {
		t.Fatal("first pass must only persist the finalizer")
	}
	h.reconcile()
	if h.ipam.calls != 3 || len(h.current().Status.Resources) != 6 || h.network.marks != 0 || h.network.creations != 0 {
		t.Fatal("expected reserved claims and pending local networks before IC mutation")
	}
	if reason := meta.FindStatusCondition(h.current().Status.Conditions, "Ready").Reason; reason != "LocalNetworkPending" {
		t.Fatalf("unexpected readiness reason %s", reason)
	}
}

func TestIPAMFailureAndPersistedReceiptReuse(t *testing.T) {
	h := newHarness(t)
	h.ipam.err = errors.New("registry unavailable")
	h.reconcile()
	h.reconcile()
	if len(h.current().Status.Allocations) != 0 || h.remote["dc-a"].writes != 0 || h.remote["dc-b"].writes != 0 {
		t.Fatal("IPAM outage created network resources")
	}
	h.ipam.err = nil
	h.reconcile()
	h.readySubnets()
	h.reconcile()
	h.reconcile()
	before := h.current().Status
	calls := h.ipam.calls
	h.ipam.err = errors.New("registry unavailable again")
	h.reconcile()
	if h.current().Status.Phase != "Ready" || h.ipam.calls != calls || !reflect.DeepEqual(before.Allocations, h.current().Status.Allocations) || !reflect.DeepEqual(before.Resources, h.current().Status.Resources) {
		t.Fatal("accepted network did not reuse its durable receipts during IPAM outage")
	}
}

func TestMalformedReceiptBlocksBeforeNetworking(t *testing.T) {
	h := newHarness(t)
	h.ipam.malformed = true
	h.reconcile()
	h.reconcile()
	if h.current().Status.Phase != "Blocked" || len(h.current().Status.Allocations) != 0 || h.remote["dc-a"].writes != 0 {
		t.Fatal("malformed receipt was accepted")
	}
}

func TestPersistedReceiptCorruptionPreservesExistingNetwork(t *testing.T) {
	h := newHarness(t)
	h.converge()
	v := h.current()
	v.Status.Allocations[0].ScopeRef = "another-tenant-vrf"
	if err := h.authority.Status().Update(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	before, calls := len(h.events), h.ipam.calls
	h.reconcile()
	if h.current().Status.Phase != "Blocked" || len(h.events) != before || h.ipam.calls != calls {
		t.Fatal("corrupt durable receipt caused network mutation or reallocation")
	}
}

func TestOneRemoteSubnetPendingPreventsAllICWrites(t *testing.T) {
	h := newHarness(t)
	h.reconcile()
	h.reconcile()
	h.readySubnets()
	site := h.p.Attachments[1]
	u := object("Subnet", site.TransitName)
	c := h.remote[site.Cluster.Name]
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(u), u); err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedSlice(u.Object, []interface{}{map[string]interface{}{"type": "Ready", "status": "False"}}, "status", "conditions")
	if err := c.Status().Update(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	if h.network.marks != 0 || h.network.creations != 0 || h.gate.pauses != 0 || h.current().Status.Phase != "Pending" {
		t.Fatal("IC transaction started before every local subnet was ready")
	}
}

func TestQuiesceAndResumeAreDurablePhases(t *testing.T) {
	h := newHarness(t)
	h.reconcile()
	h.reconcile()
	h.readySubnets()
	h.gate.pauseReady = false
	h.reconcile()
	if h.network.marks != 0 || h.network.creations != 0 {
		t.Fatal("IC was modified while quiesce was incomplete")
	}
	h.gate.pauseReady, h.gate.resumeReady = true, false
	h.reconcile()
	if h.current().Status.Operation != "Resume" || h.network.creations != 1 || h.gate.holder == "" {
		t.Fatal("resume phase and held lock were not persisted")
	}
	pauses := h.gate.pauses
	h.network.checkErr = errors.New("temporary IC check failure")
	h.reconcile()
	if h.gate.pauses != pauses || h.current().Status.Operation != "Resume" {
		t.Fatal("retrying resume re-quiesced native IC or lost the durable phase")
	}
	h.gate.resumeReady = true
	h.reconcile()
	if h.gate.holder != "" || h.current().Status.Operation != "" {
		t.Fatal("successful resume did not clear transaction state")
	}
}

func TestSteadyReplayPreservesRoutesAndResourceUIDs(t *testing.T) {
	h := newHarness(t)
	h.converge()
	before := h.current().Status.Resources
	pauses := h.gate.pauses
	for i := 0; i < 3; i++ {
		h.reconcile()
	}
	if !reflect.DeepEqual(before, h.current().Status.Resources) || h.gate.pauses != pauses || h.network.creations != 1 {
		t.Fatal("steady replay replaced resources or interrupted native IC")
	}
	for _, c := range h.remote {
		for _, routes := range c.history {
			if routes != 1 {
				t.Fatal("ordinary reconciliation withdrew a remote static route")
			}
		}
		if c.writes != 3 {
			t.Fatalf("idempotent replay unexpectedly wrote resources: %d", c.writes)
		}
	}
}

func TestAcceptedConfigAndSpecChangesBlock(t *testing.T) {
	for _, change := range []string{"config", "spec"} {
		t.Run(change, func(t *testing.T) {
			h := newHarness(t)
			h.converge()
			before := len(h.events)
			if change == "config" {
				h.r.Config.Clusters[0].Kubeconfig = "replacement.conf"
			} else {
				v := h.current()
				v.Spec.Attachments[0].Gateway = "10.241.0.2"
				if err := h.authority.Update(context.Background(), v); err != nil {
					t.Fatal(err)
				}
			}
			h.reconcile()
			if h.current().Status.Phase != "Blocked" || len(h.events) != before {
				t.Fatal("changed accepted intent was applied")
			}
		})
	}
}

func TestForeignAndReplacedResourcesAreNotMutated(t *testing.T) {
	for _, conflict := range []string{"foreign", "replaced"} {
		t.Run(conflict, func(t *testing.T) {
			h := newHarness(t)
			h.converge()
			c := h.remote["dc-a"]
			u := object("Vpc", h.p.Attachments[0].VpcName)
			if err := c.Client.Get(context.Background(), client.ObjectKeyFromObject(u), u); err != nil {
				t.Fatal(err)
			}
			if conflict == "foreign" {
				u.SetLabels(map[string]string{api.OwnerLabel: "another-tenant", api.ClusterLabel: "cluster-a"})
				if err := c.Client.Update(context.Background(), u); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := c.Client.Delete(context.Background(), u); err != nil {
					t.Fatal(err)
				}
				u.SetUID("replacement-uid")
				u.SetResourceVersion("")
				if err := c.Client.Create(context.Background(), u); err != nil {
					t.Fatal(err)
				}
			}
			writes := c.writes
			h.reconcile()
			if h.current().Status.Phase != "Blocked" || c.writes != writes {
				t.Fatal("conflicting object was mutated")
			}
		})
	}
}

func TestClusterIdentityChangeBlocksBeforeIPAM(t *testing.T) {
	h := newHarness(t)
	var ns corev1.Namespace
	c := h.remote["dc-b"].Client
	if err := c.Get(context.Background(), types.NamespacedName{Name: "kube-system"}, &ns); err != nil {
		t.Fatal(err)
	}
	ns.UID = "wrong-cluster"
	if err := c.Update(context.Background(), &ns); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	h.reconcile()
	if h.ipam.calls != 0 || h.current().Status.Phase != "Blocked" {
		t.Fatal("remote identity was not checked before reservation")
	}
}

func TestDeletionBlockedByConsumersAndRemoteFailure(t *testing.T) {
	for _, failure := range []string{"consumers", "remote API", "remote OVN"} {
		t.Run(failure, func(t *testing.T) {
			h := newHarness(t)
			h.converge()
			h.deleting()
			before := len(h.events)
			switch failure {
			case "consumers":
				h.network.endpointsEmpty = false
			case "remote API":
				h.remote["dc-b"].getErr = errors.New("API unavailable")
			case "remote OVN":
				h.network.endpointErr = errors.New("OVN unavailable")
			}
			h.reconcile()
			if !contains(h.current().Finalizers, api.Finalizer) || len(h.events) != before || h.network.transitUID == "" {
				t.Fatal("blocked deletion modified existing network or removed its finalizer")
			}
		})
	}
}

func TestSuccessfulDeletionRetainsIPAMAndWaitsForOVNCleanup(t *testing.T) {
	h := newHarness(t)
	h.converge()
	calls := h.ipam.calls
	allocations := h.current().Status.Allocations
	h.deleting()
	h.network.localAbsent = false
	for i := 0; i < 4; i++ {
		h.reconcile()
	}
	if v := h.current(); !contains(v.Finalizers, api.Finalizer) || !reflect.DeepEqual(allocations, v.Status.Allocations) {
		t.Fatal("cleanup did not wait for local OVN absence or altered retained allocations")
	}
	h.network.localAbsent = true
	h.gate.resumeReady = false
	h.reconcile()
	if h.current().Status.Operation != "DeleteResume" {
		t.Fatal("deletion resume phase was not persisted")
	}
	pauses := h.gate.pauses
	h.reconcile()
	if h.gate.pauses != pauses {
		t.Fatal("delete resume re-quiesced native IC")
	}
	h.gate.resumeReady = true
	h.reconcile()
	var v api.GlobalVpc
	if err := h.authority.Get(context.Background(), h.request.NamespacedName, &v); !apierrors.IsNotFound(err) {
		t.Fatalf("GlobalVpc remains after verified cleanup: %v", err)
	}
	if h.gate.holder != "" || h.gate.paused || h.ipam.calls != calls || len(h.ipam.allocations) != 3 {
		t.Fatal("deletion did not resume the domain or retain registry reservations")
	}
}

func TestLostTransitResponseDiscoveredAfterRestart(t *testing.T) {
	h := newHarness(t)
	h.reconcile()
	h.reconcile()
	h.readySubnets()
	h.network.lostResponseOnce = true
	h.reconcile()
	if h.current().Status.TransitSwitchUID != "" || h.network.transitUID == "" || h.gate.holder == "" || !h.gate.paused {
		t.Fatal("lost response did not retain a recoverable transaction")
	}
	// Recreate the reconciler while keeping only its durable/external dependencies.
	restarted := *h.r
	h.r = &restarted
	h.reconcile()
	h.reconcile()
	if h.current().Status.Phase != "Ready" || h.network.creations != 1 || h.current().Status.TransitSwitchUID != h.network.transitUID || h.gate.holder != "" {
		t.Fatal("restart duplicated a committed transit switch or failed to resume")
	}
}

func TestTransitConflictPreflightBlocksEveryExternalWrite(t *testing.T) {
	for _, conflict := range []string{"foreign on create", "replaced on reconcile", "foreign on delete", "replaced on delete"} {
		t.Run(conflict, func(t *testing.T) {
			h := newHarness(t)
			if conflict == "foreign on create" {
				h.network.transitUID, h.network.transitOwner = "foreign-row", "foreign-owner"
				h.reconcile() // persist finalizer
			} else {
				h.converge()
				if strings.HasPrefix(conflict, "foreign") {
					h.network.transitOwner = "foreign-owner"
				} else {
					h.network.transitUID = "replacement-row"
				}
				// Local drift would normally be repaired. TS identity must be checked
				// first, before even this legitimate local reconciliation write.
				c := h.remote["dc-a"].Client
				u := object("Vpc", h.p.Attachments[0].VpcName)
				if err := c.Get(context.Background(), client.ObjectKeyFromObject(u), u); err != nil {
					t.Fatal(err)
				}
				_ = unstructured.SetNestedSlice(u.Object, []interface{}{}, "spec", "staticRoutes")
				if err := c.Update(context.Background(), u); err != nil {
					t.Fatal(err)
				}
				if strings.HasSuffix(conflict, "delete") {
					h.deleting()
				}
			}
			before, calls, pauses := len(h.events), h.ipam.calls, h.gate.pauses
			h.reconcile()
			if h.current().Status.Phase != "Blocked" || len(h.events) != before || h.ipam.calls != calls || h.gate.pauses != pauses || !contains(h.current().Finalizers, api.Finalizer) {
				t.Fatal("transit conflict did not block provider/local/domain mutations")
			}
		})
	}
}

func TestMissingTransitClearsOldUUIDBeforeLostReplacementResponse(t *testing.T) {
	h := newHarness(t)
	h.converge()
	oldUID := h.current().Status.TransitSwitchUID
	h.network.transitUID, h.network.transitOwner = "", ""
	h.reconcile()
	if h.current().Status.TransitSwitchUID != "" || !h.gate.paused || h.gate.holder == "" || h.network.creations != 1 {
		t.Fatal("observed absence was not durably recorded before replacement")
	}
	h.network.lostResponseOnce = true
	h.reconcile()
	if h.current().Status.TransitSwitchUID != "" || h.network.transitUID == "" || h.network.transitUID == oldUID || !h.gate.paused {
		t.Fatal("lost replacement response was not recoverable")
	}
	restarted := *h.r
	h.r = &restarted
	h.reconcile()
	h.reconcile()
	if h.current().Status.Phase != "Ready" || h.network.creations != 2 || h.current().Status.TransitSwitchUID != h.network.transitUID || h.gate.holder != "" {
		t.Fatal("old transit UUID stranded the replacement after restart")
	}
}

func TestTransitReplacementStatusFailureRecovers(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("status_committed_%t", committed), func(t *testing.T) {
			h := newHarness(t)
			h.converge()
			h.network.transitUID, h.network.transitOwner = "", ""
			h.reconcile() // persist absent old UUID with the domain stopped
			fault := &faultStatusClient{Client: h.authority, failOnce: true, committed: committed}
			h.r.Client = fault
			if _, err := h.r.Reconcile(context.Background(), h.request); err == nil {
				t.Fatal("injected status failure was not surfaced")
			}
			if h.network.creations != 2 || h.network.transitUID == "" || !h.gate.paused {
				t.Fatal("status failure lost the paused recovery boundary")
			}
			restarted := *h.r
			h.r = &restarted
			h.reconcile()
			h.reconcile()
			if h.current().Status.Phase != "Ready" || h.current().Status.TransitSwitchUID != h.network.transitUID || h.network.creations != 2 || h.gate.holder != "" {
				t.Fatal("status failure duplicated or stranded the replacement transit")
			}
		})
	}
}

func TestReadyRequiresICAndStaticRoutes(t *testing.T) {
	h := newHarness(t)
	h.converge()
	for _, pending := range []string{"InterconnectPending", "RoutesPending"} {
		h.network.connected = pending != "InterconnectPending"
		h.network.routesReady = pending != "RoutesPending"
		h.reconcile()
		condition := meta.FindStatusCondition(h.current().Status.Conditions, "Ready")
		if condition == nil || condition.Status != metav1.ConditionFalse || !strings.Contains(condition.Reason, pending) {
			t.Fatalf("reported Ready without %s evidence: %+v", pending, condition)
		}
	}
}
