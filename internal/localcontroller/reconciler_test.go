package localcontroller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"reflect"
	"testing"

	api "globalvpc.io/controller/api/v1alpha2"
	"globalvpc.io/controller/integration/kube-ovn/destinationroute"
	"globalvpc.io/controller/internal/localconfig"
	"globalvpc.io/controller/internal/platformplan"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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

func localFixtureConfig() localconfig.Config {
	return localconfig.Config{LocationRef: "dc-a", ClusterUID: "cluster-a", Namespace: "operator-system", AuthorityNamespace: "location-a", AuthorityKubeconfig: "unused-test-reference", GatewayNodeSelector: map[string]string{"gateway": "true"}, TransitPool: "10.211.0.0/16", TransitPrefixLength: 29, BFDSourcePool: "10.251.0.0/16", ControlPool: "10.252.0.0/16", GatewayReplicas: 2, GatewayImage: "example.invalid/gateway:test", MTU: 1380, PortStart: 51820, PortEnd: 52820, RuntimeDir: "/unused-test-runtime"}
}
func localFixtureBinding(cross bool) *api.NetworkBinding {
	b := &api.NetworkBinding{ObjectMeta: metav1.ObjectMeta{Name: platformplan.BindingName("vpc-red", "dc-a"), Namespace: "operator-system", UID: "local-binding-a", Generation: 1, Annotations: map[string]string{SourceUIDAnnotation: "authority-binding-a", SourceGenerationAnnotation: "1"}}, Spec: api.NetworkBindingSpec{VPCRef: api.ObjectIdentity{Namespace: "project", Name: "red", UID: "vpc-red"}, NetworkID: 11001, LocalASN: 4200000001, LocationRef: "dc-a", NetworkClassRef: "default", NativeVpcName: platformplan.VpcName("vpc-red", "dc-a"), TransportProfile: "wireguard-bgp", AuthorizedLocations: []string{"dc-a"}, Subnets: []api.BindingSubnet{{Name: "apps", UID: "apps-uid", CIDR: "10.241.0.0/24", Gateway: "10.241.0.1", NativeSubnetName: "ps-apps"}, {Name: "db", UID: "db-uid", CIDR: "10.241.1.0/24", Gateway: "10.241.1.1", NativeSubnetName: "ps-db"}}}}
	if cross {
		b.Spec.AuthorizedLocations = append(b.Spec.AuthorizedLocations, "dc-b")
		b.Spec.RemoteSubnets = []api.RemoteSubnet{{LocationRef: "dc-b", CIDR: "10.242.0.0/24"}}
	}
	b.Spec.Revision = platformplan.Revision(b.Spec)
	return b
}

type localIdentityClient struct {
	client.Client
	serial int
}

func (c *localIdentityClient) Create(ctx context.Context, o client.Object, opts ...client.CreateOption) error {
	if o.GetUID() == "" {
		c.serial++
		o.SetUID(types.UID(fmt.Sprintf("native-created-%d", c.serial)))
	}
	if o.GetGeneration() == 0 {
		o.SetGeneration(1)
	}
	return c.Client.Create(ctx, o, opts...)
}
func (c *localIdentityClient) Update(ctx context.Context, o client.Object, opts ...client.UpdateOption) error {
	old := o.DeepCopyObject().(client.Object)
	if c.Client.Get(ctx, client.ObjectKeyFromObject(o), old) == nil {
		a, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(old)
		b, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
		if !reflect.DeepEqual(a["spec"], b["spec"]) {
			o.SetGeneration(old.GetGeneration() + 1)
		}
	}
	return c.Client.Update(ctx, o, opts...)
}
func localTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if e := api.AddToScheme(s); e != nil {
		t.Fatal(e)
	}
	if e := corev1.AddToScheme(s); e != nil {
		t.Fatal(e)
	}
	if e := apiextv1.AddToScheme(s); e != nil {
		t.Fatal(e)
	}
	for _, kind := range []string{"Vpc", "Subnet", "IP"} {
		s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "kubeovn.io", Version: "v1", Kind: kind}, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "kubeovn.io", Version: "v1", Kind: kind + "List"}, &unstructured.UnstructuredList{})
	}
	return s
}
func fakeLocalClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return &localIdentityClient{Client: fake.NewClientBuilder().WithScheme(localTestScheme(t)).WithStatusSubresource(&api.NetworkBinding{}, native("Vpc", ""), native("Subnet", "")).WithObjects(objects...).Build()}
}
func capabilityObject() *apiextv1.CustomResourceDefinition {
	field := apiextv1.JSONSchemaProps{Type: "array"}
	spec := apiextv1.JSONSchemaProps{Type: "object", Properties: map[string]apiextv1.JSONSchemaProps{"destinationRoutes": field}}
	schema := apiextv1.JSONSchemaProps{Type: "object", Properties: map[string]apiextv1.JSONSchemaProps{"spec": spec}}
	return &apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "vpcs.kubeovn.io"},
		Spec:       apiextv1.CustomResourceDefinitionSpec{Versions: []apiextv1.CustomResourceDefinitionVersion{{Name: "v1", Served: true, Schema: &apiextv1.CustomResourceValidation{OpenAPIV3Schema: &schema}}}},
	}
}

type gatewayDouble struct {
	ensures, deletes int
	ready            bool
}

func (g *gatewayDouble) Ensure(_ context.Context, b *api.NetworkBinding, a Allocation) (GatewayResult, error) {
	g.ensures++
	p, _ := netip.ParsePrefix(a.TransitCIDR)
	first := p.Addr().Next().Next()
	second := first.Next()
	suffix := "10"
	if b.Spec.LocationRef == "dc-b" {
		suffix = "20"
	}
	return GatewayResult{Ready: g.ready, Reason: "TestGatewayReport", Gateways: []api.GatewayEndpoint{{ID: "gateway-1", NodeName: "node-1", NodeUID: b.Spec.LocationRef + "-node-1", EndpointIP: "192.0.2." + suffix, TransitIP: first.String(), LocalASN: b.Spec.LocalASN, PublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", Ready: g.ready}, {ID: "gateway-2", NodeName: "node-2", NodeUID: b.Spec.LocationRef + "-node-2", EndpointIP: "198.51.100." + suffix, TransitIP: second.String(), LocalASN: b.Spec.LocalASN, PublicKey: "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=", Ready: g.ready}}}, nil
}
func (g *gatewayDouble) Delete(_ context.Context, _ *api.NetworkBinding) (bool, error) {
	g.deletes++
	return true, nil
}

type localHarness struct {
	t       *testing.T
	c       client.Client
	r       *Reconciler
	gateway *gatewayDouble
	key     client.ObjectKey
}

func newLocalHarness(t *testing.T, cross, capability bool, objects ...client.Object) *localHarness {
	t.Helper()
	b := localFixtureBinding(cross)
	objects = append(objects, b, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "cluster-a"}})
	if capability {
		objects = append(objects, capabilityObject())
	}
	c := fakeLocalClient(t, objects...)
	g := &gatewayDouble{ready: true}
	return &localHarness{t: t, c: c, key: client.ObjectKeyFromObject(b), gateway: g, r: &Reconciler{Client: c, Config: localFixtureConfig(), Gateway: g}}
}
func (h *localHarness) binding() *api.NetworkBinding {
	h.t.Helper()
	b := new(api.NetworkBinding)
	if e := h.c.Get(context.Background(), h.key, b); e != nil {
		h.t.Fatal(e)
	}
	return b
}
func (h *localHarness) step() {
	h.t.Helper()
	if _, e := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: h.key}); e != nil {
		h.t.Fatal(e)
	}
}
func (h *localHarness) run(n int, simulate bool) {
	h.t.Helper()
	for i := 0; i < n; i++ {
		h.step()
		if simulate {
			simulateNative(h.t, h.c, true)
		}
	}
}
func (h *localHarness) edit(edit func(*api.NetworkBindingSpec)) {
	h.t.Helper()
	b := h.binding()
	edit(&b.Spec)
	b.Spec.Revision = platformplan.Revision(b.Spec)
	if e := h.c.Update(context.Background(), b); e != nil {
		h.t.Fatal(e)
	}
}

func nativeList(kind string) *unstructured.UnstructuredList {
	l := &unstructured.UnstructuredList{}
	l.SetAPIVersion("kubeovn.io/v1")
	l.SetKind(kind + "List")
	return l
}

// simulateNative is the explicit Kube-OVN test double. It acknowledges the exact
// desired hash and generation, and never executes OVSDB or creates network links.
func simulateNative(t *testing.T, c client.Client, routes bool) {
	t.Helper()
	ctx := context.Background()
	for _, kind := range []string{"Subnet", "Vpc"} {
		list := nativeList(kind)
		if e := c.List(ctx, list); e != nil {
			t.Fatal(e)
		}
		for i := range list.Items {
			o := &list.Items[i]
			if o.GetLabels()[NativeOwnerLabel] == "" {
				continue
			}
			before := o.DeepCopy()
			if o.Object["status"] == nil {
				o.Object["status"] = map[string]any{}
			}
			if kind == "Subnet" {
				_ = unstructured.SetNestedSlice(o.Object, []any{map[string]any{"type": "Ready", "status": "True"}}, "status", "conditions")
			} else if routes {
				raw, found, _ := unstructured.NestedSlice(o.Object, "spec", "destinationRoutes")
				if !found {
					continue
				}
				encoded, _ := json.Marshal(raw)
				var intents []destinationroute.Intent
				if e := json.Unmarshal(encoded, &intents); e != nil {
					t.Fatal(e)
				}
				plan, e := destinationroute.Compile(intents)
				if e != nil {
					t.Fatal(e)
				}
				_ = unstructured.SetNestedMap(o.Object, map[string]any{"capability": destinationroute.Capability, "ready": true, "appliedHash": plan.Hash, "observedGeneration": o.GetGeneration()}, "status", "destinationRoutes")
			}
			if !reflect.DeepEqual(before.Object, o.Object) {
				if e := c.Status().Update(ctx, o); e != nil {
					t.Fatal(e)
				}
			}
		}
	}
}

func TestLocalOnlyCreatesNativeVpcAndMultipleSubnetsWithoutGateway(t *testing.T) {
	h := newLocalHarness(t, false, false)
	h.run(24, true)
	b := h.binding()
	if b.Status.AppliedRevision != b.Spec.Revision || !meta.IsStatusConditionTrue(b.Status.Conditions, "Ready") || h.gateway.ensures != 0 {
		t.Fatal("local-only native network required a gateway or failed to apply")
	}
	subs := nativeList("Subnet")
	if e := h.c.List(context.Background(), subs); e != nil {
		t.Fatal(e)
	}
	if len(subs.Items) != 2 {
		t.Fatal("local-only mode created transit subnet or lost a workload subnet")
	}
	h.edit(func(s *api.NetworkBindingSpec) { s.Deleting = true })
	h.run(24, true)
	b = h.binding()
	if b.Status.Phase != "Deleted" || b.Status.AppliedRevision != b.Spec.Revision || len(b.Status.Resources) != 0 {
		t.Fatal("local-only deletion did not finish")
	}
	var ledger corev1.ConfigMap
	if e := h.c.Get(context.Background(), client.ObjectKey{Namespace: h.r.Config.Namespace, Name: "managed-network-allocations"}, &ledger); e != nil {
		t.Fatal("delete discarded allocation ledger")
	}
}

func TestLegacyNativeCapabilityBlocksGatewayAndRouteMutation(t *testing.T) {
	h := newLocalHarness(t, true, false)
	h.run(24, true)
	condition := meta.FindStatusCondition(h.binding().Status.Conditions, "Ready")
	if condition == nil || condition.Reason != "NativeCapabilityRequired" || h.gateway.ensures != 0 || h.binding().Status.AppliedRevision != "" {
		t.Fatal("legacy Kube-OVN was treated as supporting the native extension")
	}
	v := native("Vpc", h.binding().Spec.NativeVpcName)
	if e := h.c.Get(context.Background(), client.ObjectKeyFromObject(v), v); e != nil {
		t.Fatal(e)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(v.Object, "spec", "destinationRoutes"); found {
		t.Fatal("native route intent written before capability check")
	}
}

func TestNativeRouteReadinessRequiresAppliedHashAndGeneration(t *testing.T) {
	h := newLocalHarness(t, true, true)
	for i := 0; i < 28; i++ {
		h.step()
		simulateNative(t, h.c, false)
	}
	b := h.binding()
	if h.gateway.ensures == 0 || b.Status.AppliedRevision != "" {
		t.Fatal("gateway registration or native acknowledgement gate failed")
	}
	v := native("Vpc", b.Spec.NativeVpcName)
	if e := h.c.Get(context.Background(), client.ObjectKeyFromObject(v), v); e != nil {
		t.Fatal(e)
	}
	raw, _, _ := unstructured.NestedSlice(v.Object, "spec", "destinationRoutes")
	data, _ := json.Marshal(raw)
	var intents []destinationroute.Intent
	_ = json.Unmarshal(data, &intents)
	p, e := destinationroute.Compile(intents)
	if e != nil {
		t.Fatal(e)
	}
	for _, status := range []map[string]any{{"capability": destinationroute.Capability, "ready": true, "appliedHash": "stale", "observedGeneration": v.GetGeneration()}, {"capability": destinationroute.Capability, "ready": true, "appliedHash": p.Hash, "observedGeneration": v.GetGeneration() - 1}} {
		if e = h.c.Get(context.Background(), client.ObjectKeyFromObject(v), v); e != nil {
			t.Fatal(e)
		}
		if v.Object["status"] == nil {
			v.Object["status"] = map[string]any{}
		}
		if err := unstructured.SetNestedMap(v.Object, status, "status", "destinationRoutes"); err != nil {
			t.Fatal(err)
		}
		if e = h.c.Status().Update(context.Background(), v); e != nil {
			t.Fatal(e)
		}
		h.run(3, false)
		if h.binding().Status.AppliedRevision != "" {
			t.Fatal("stale native route status acknowledged current snapshot")
		}
	}
	simulateNative(t, h.c, true)
	h.run(4, false)
	if h.binding().Status.AppliedRevision != h.binding().Spec.Revision {
		_ = h.c.Get(context.Background(), client.ObjectKeyFromObject(v), v)
		t.Fatalf("exact native transaction receipt was not accepted: native=%+v expectedHash=%s", v.Object, p.Hash)
	}
}

func TestSubnetInUsePreservesGatewayAndNativeRouteIntent(t *testing.T) {
	for _, kind := range []string{"Pod", "IP", "AttachedIP"} {
		t.Run(kind, func(t *testing.T) {
			h := newLocalHarness(t, true, true)
			h.run(30, true)
			b := h.binding()
			if b.Status.AppliedRevision != b.Spec.Revision {
				t.Fatalf("fixture did not converge: status=%+v ensures=%d", b.Status, h.gateway.ensures)
			}
			var endpoint client.Object
			if kind == "Pod" {
				endpoint = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "workloads", Name: "in-use", Annotations: map[string]string{"ovn.kubernetes.io/logical_switch": "ps-apps"}}}
			} else {
				ip := native("IP", "in-use")
				if kind == "IP" {
					ip.Object["spec"] = map[string]any{"subnet": "ps-apps"}
				} else {
					ip.Object["spec"] = map[string]any{"attachSubnets": []any{"ps-apps"}}
				}
				endpoint = ip
			}
			if e := h.c.Create(context.Background(), endpoint); e != nil {
				t.Fatal(e)
			}
			v := native("Vpc", b.Spec.NativeVpcName)
			if e := h.c.Get(context.Background(), client.ObjectKeyFromObject(v), v); e != nil {
				t.Fatal(e)
			}
			before := v.DeepCopy()
			calls := h.gateway.ensures
			h.edit(func(s *api.NetworkBindingSpec) { s.Subnets[0].Deleting = true })
			h.run(5, true)
			condition := meta.FindStatusCondition(h.binding().Status.Conditions, "Ready")
			if condition == nil || condition.Reason != "SubnetInUse" || h.gateway.ensures != calls || h.binding().Status.AppliedRevision == h.binding().Spec.Revision {
				t.Fatal("in-use subnet changed gateway runtime or acknowledged deletion")
			}
			if e := h.c.Get(context.Background(), client.ObjectKeyFromObject(v), v); e != nil {
				t.Fatal(e)
			}
			if !reflect.DeepEqual(v.Object["spec"], before.Object["spec"]) {
				t.Fatal("in-use delete rewrote native route intent")
			}
			if e := h.c.Delete(context.Background(), endpoint); e != nil {
				t.Fatal(e)
			}
			h.run(22, true)
			if h.binding().Status.AppliedRevision != h.binding().Spec.Revision {
				t.Fatal("drained subnet did not finish local phase")
			}
			if e := h.c.Get(context.Background(), client.ObjectKey{Name: "ps-apps"}, native("Subnet", "ps-apps")); !apierrors.IsNotFound(e) {
				t.Fatal("drained native subnet remains")
			}
			if e := h.c.Get(context.Background(), client.ObjectKey{Name: "ps-db"}, native("Subnet", "ps-db")); e != nil {
				t.Fatal("deletion removed sibling native subnet")
			}
		})
	}
}

func TestForeignNativeVpcOrRecordedReplacementIsNeverAdopted(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(fmt.Sprint(replacement), func(t *testing.T) {
			h := newLocalHarness(t, false, false)
			if replacement {
				h.run(20, true)
				b := h.binding()
				v := native("Vpc", b.Spec.NativeVpcName)
				if e := h.c.Get(context.Background(), client.ObjectKeyFromObject(v), v); e != nil {
					t.Fatal(e)
				}
				if e := h.c.Delete(context.Background(), v); e != nil {
					t.Fatal(e)
				}
				v.SetResourceVersion("")
				v.SetUID("replacement-vpc")
				if e := h.c.Create(context.Background(), v); e != nil {
					t.Fatal(e)
				}
			} else {
				v := nativeSpec("Vpc", h.binding().Spec.NativeVpcName, "another-owner", map[string]any{"staticRoutes": []any{map[string]any{"cidr": "0.0.0.0/0", "nextHopIP": "192.0.2.1"}}})
				if e := h.c.Create(context.Background(), v); e != nil {
					t.Fatal(e)
				}
			}
			h.run(4, true)
			if h.binding().Status.Phase != "Blocked" || h.gateway.ensures != 0 {
				t.Fatal("foreign native identity was adopted")
			}
		})
	}
}

func TestRecordedNativeAbsenceBlocksBeforeRecreation(t *testing.T) {
	for _, kind := range []string{"Vpc", "Subnet"} {
		t.Run(kind, func(t *testing.T) {
			h := newLocalHarness(t, false, false)
			h.run(20, true)
			name := h.binding().Spec.NativeVpcName
			if kind == "Subnet" {
				name = "ps-apps"
			}
			o := native(kind, name)
			if e := h.c.Get(context.Background(), client.ObjectKeyFromObject(o), o); e != nil {
				t.Fatal(e)
			}
			oldUID := string(o.GetUID())
			if e := h.c.Delete(context.Background(), o); e != nil {
				t.Fatal(e)
			}
			h.run(4, false)
			if h.binding().Status.Phase != "Blocked" {
				t.Fatal("missing recorded native object was not fenced")
			}
			if e := h.c.Get(context.Background(), client.ObjectKey{Name: name}, native(kind, name)); !apierrors.IsNotFound(e) {
				t.Fatal("missing native identity was recreated before recovery authorization")
			}
			kept := false
			for _, r := range h.binding().Status.Resources {
				if r.Kind == kind && r.Name == name && r.UID == oldUID {
					kept = true
				}
			}
			if !kept {
				t.Fatal("missing native identity lost its recovery receipt")
			}
		})
	}
}

type uncertainGatewayDouble struct {
	calls int
	gone  bool
}

func (g *uncertainGatewayDouble) Ensure(context.Context, *api.NetworkBinding, Allocation) (GatewayResult, error) {
	return GatewayResult{}, fmt.Errorf("local-only path unexpectedly ensured a gateway")
}
func (g *uncertainGatewayDouble) Delete(context.Context, *api.NetworkBinding) (bool, error) {
	g.calls++
	return g.gone, nil
}
func TestDisconnectChecksUncertainGatewayCreationWithoutPublishedEndpoints(t *testing.T) {
	h := newLocalHarness(t, false, false)
	uncertain := &uncertainGatewayDouble{}
	h.r.Gateway = uncertain
	h.run(4, false)
	b := h.binding()
	condition := meta.FindStatusCondition(b.Status.Conditions, "Ready")
	if uncertain.calls == 0 || len(b.Status.Gateways) != 0 || b.Status.AppliedRevision != "" || condition == nil || condition.Reason != "GatewayWithdrawalPending" {
		t.Fatal("disconnect ignored possible unpublished gateway resources")
	}
	uncertain.gone = true
	h.run(20, true)
	if h.binding().Status.AppliedRevision != h.binding().Spec.Revision {
		t.Fatal("confirmed gateway cleanup did not unblock local-only network")
	}
}
