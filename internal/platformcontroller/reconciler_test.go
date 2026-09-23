package platformcontroller

import (
	"context"
	"fmt"
	"testing"

	api "globalvpc.io/controller/api/v1alpha2"
	"globalvpc.io/controller/internal/platformconfig"
	"globalvpc.io/controller/internal/platformplan"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func authorityConfig() platformconfig.Config {
	return platformconfig.Config{NetworkClasses: map[string]platformconfig.NetworkClass{"default": {TransportProfile: "wireguard-bgp"}}, Locations: []platformconfig.Location{
		{Name: "dc-a", Region: "region-one", Site: "site-a", DC: "dc-a", BindingNamespace: "location-a", AllowedProjects: []string{"project"}, CIDRPools: []string{"10.240.0.0/14"}},
		{Name: "dc-b", Region: "region-one", Site: "site-b", DC: "dc-b", BindingNamespace: "location-b", AllowedProjects: []string{"project"}, CIDRPools: []string{"10.240.0.0/14"}},
	}}
}

// identityClient adds server-generated identities to the fake API. Status writes
// still use the fake API's resourceVersion conflict checks and status subresources.
type identityClient struct {
	client.Client
	serial int
}

func (c *identityClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if obj.GetUID() == "" {
		c.serial++
		obj.SetUID(types.UID(fmt.Sprintf("generated-%d", c.serial)))
	}
	if obj.GetGeneration() == 0 {
		obj.SetGeneration(1)
	}
	return c.Client.Create(ctx, obj, opts...)
}

type authorityHarness struct {
	t   *testing.T
	r   *Reconciler
	c   client.Client
	vpc client.ObjectKey
}

func publicVPC(name string) *api.VPC {
	return &api.VPC{ObjectMeta: metav1.ObjectMeta{Namespace: "project", Name: name, UID: types.UID("vpc-" + name), Generation: 1}}
}
func publicSubnet(name, vpc, location, cidr string) *api.Subnet {
	return &api.Subnet{ObjectMeta: metav1.ObjectMeta{Namespace: "project", Name: name, UID: types.UID("subnet-" + name), Generation: 1}, Spec: api.SubnetSpec{VPCRef: vpc, LocationRef: location, CIDR: cidr}}
}
func newAuthorityHarness(t *testing.T, vpc *api.VPC, objects ...client.Object) *authorityHarness {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objects = append(objects, vpc)
	c := &identityClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.VPC{}, &api.Subnet{}, &api.NetworkBinding{}).WithObjects(objects...).Build()}
	return &authorityHarness{t: t, c: c, vpc: client.ObjectKeyFromObject(vpc), r: &Reconciler{Client: c, Config: authorityConfig()}}
}
func (h *authorityHarness) step() {
	h.t.Helper()
	if _, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: h.vpc}); err != nil {
		h.t.Fatal(err)
	}
}
func (h *authorityHarness) run(n int) {
	h.t.Helper()
	for i := 0; i < n; i++ {
		h.step()
	}
}
func (h *authorityHarness) network() *api.VPC {
	h.t.Helper()
	v := new(api.VPC)
	if err := h.c.Get(context.Background(), h.vpc, v); err != nil {
		h.t.Fatal(err)
	}
	return v
}
func (h *authorityHarness) subnet(name string) *api.Subnet {
	h.t.Helper()
	s := new(api.Subnet)
	if err := h.c.Get(context.Background(), client.ObjectKey{Namespace: "project", Name: name}, s); err != nil {
		h.t.Fatal(err)
	}
	return s
}
func (h *authorityHarness) bindings() []api.NetworkBinding {
	h.t.Helper()
	var l api.NetworkBindingList
	if err := h.c.List(context.Background(), &l, client.MatchingLabels{platformplan.OwnerLabel: string(h.network().UID)}); err != nil {
		h.t.Fatal(err)
	}
	return l.Items
}
func (h *authorityHarness) binding(location string) *api.NetworkBinding {
	h.t.Helper()
	for _, b := range h.bindings() {
		if b.Spec.LocationRef == location {
			return b.DeepCopy()
		}
	}
	h.t.Fatalf("binding missing at %s", location)
	return nil
}
func (h *authorityHarness) ack(location string) {
	h.t.Helper()
	b := h.binding(location)
	b.Status.AppliedRevision = b.Spec.Revision
	b.Status.ObservedGeneration = b.Generation
	b.Status.Phase = "Ready"
	b.Status.Resources = nil
	b.Status.Gateways = nil
	if b.Spec.Deleting {
		b.Status.Phase = "Deleted"
	} else {
		b.Status.Resources = append(b.Status.Resources, api.ResourceRecord{Kind: "Vpc", Name: b.Spec.NativeVpcName, UID: "native-" + b.Spec.NativeVpcName})
		for _, s := range b.Spec.Subnets {
			if !s.Deleting {
				b.Status.Resources = append(b.Status.Resources, api.ResourceRecord{Kind: "Subnet", Name: s.NativeSubnetName, UID: "native-" + s.NativeSubnetName})
			}
		}
	}
	meta.SetStatusCondition(&b.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "TestApplied", Message: "Local test materializer accepted the snapshot", ObservedGeneration: b.Generation})
	if err := h.c.Status().Update(context.Background(), b); err != nil {
		h.t.Fatal(err)
	}
}

func TestMultipleSubnetsShareVpcAndOfflineLocationDoesNotGateOtherBindings(t *testing.T) {
	h := newAuthorityHarness(t, publicVPC("red"), publicSubnet("apps", "red", "dc-a", "10.241.0.0/24"), publicSubnet("db", "red", "dc-a", "10.241.1.0/24"), publicSubnet("remote", "red", "dc-b", "10.242.0.0/24"))
	h.run(35)
	a, b := h.binding("dc-a"), h.binding("dc-b")
	if len(a.Spec.Subnets) != 2 || len(b.Spec.Subnets) != 1 || len(h.network().Status.SubnetClaims) != 3 {
		t.Fatal("multiple subnet intent or prefix admission lost")
	}
	if h.subnet("apps").Status.NativeVpcName != h.subnet("db").Status.NativeVpcName || h.subnet("apps").Status.NativeSubnetName == h.subnet("db").Status.NativeSubnetName {
		t.Fatal("native resource ownership did not preserve VPC/subnet separation")
	}
	h.ack("dc-a")
	h.run(8)
	if !meta.IsStatusConditionTrue(h.subnet("apps").Status.Conditions, "Ready") {
		t.Fatal("unreachable remote location blocked an already applied local subnet")
	}
	if meta.IsStatusConditionTrue(h.network().Status.Conditions, "Ready") || meta.IsStatusConditionTrue(h.subnet("remote").Status.Conditions, "Ready") {
		t.Fatal("unacknowledged location reported globally ready")
	}
	if b.Spec.Revision == "" || len(b.Status.Resources) != 0 {
		t.Fatal("offline location intent was not retained as pending")
	}
}

func TestPrefixClaimsRejectOverlapWithinVpcButAllowTenantReuse(t *testing.T) {
	h := newAuthorityHarness(t, publicVPC("red"), publicSubnet("a-first", "red", "dc-a", "10.241.0.0/24"), publicSubnet("z-overlap", "red", "dc-b", "10.241.0.128/25"))
	h.run(30)
	if len(h.network().Status.SubnetClaims) != 1 || h.subnet("z-overlap").Status.Phase != "Blocked" || h.subnet("z-overlap").Status.VPCUID != "" {
		t.Fatal("overlapping same-VPC CIDR was admitted")
	}
	blue := publicVPC("blue")
	bs := publicSubnet("blue-apps", "blue", "dc-a", "10.241.0.0/24")
	if err := h.c.Create(context.Background(), blue); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Create(context.Background(), bs); err != nil {
		t.Fatal(err)
	}
	h.vpc = client.ObjectKeyFromObject(blue)
	h.run(30)
	if len(h.network().Status.SubnetClaims) != 1 || h.subnet("blue-apps").Status.VPCUID != "vpc-blue" {
		t.Fatal("independent VPC could not reuse tenant address space")
	}
}

func TestSubnetDeletionWaitsForRemoteWithdrawalAndPreservesSibling(t *testing.T) {
	h := newAuthorityHarness(t, publicVPC("red"), publicSubnet("apps", "red", "dc-a", "10.241.0.0/24"), publicSubnet("db", "red", "dc-a", "10.241.1.0/24"), publicSubnet("remote", "red", "dc-b", "10.242.0.0/24"))
	h.run(35)
	h.ack("dc-a")
	h.ack("dc-b")
	h.run(10)
	s := h.subnet("apps")
	if err := h.c.Delete(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	h.run(12)
	a, b := h.binding("dc-a"), h.binding("dc-b")
	deleting := false
	for _, s := range a.Spec.Subnets {
		if s.Name == "apps" {
			deleting = s.Deleting
		}
	}
	if !deleting || a.Spec.Deleting || len(b.Spec.RemoteSubnets) != 2 {
		t.Fatal("delete request withdrew in-use or unacknowledged local subnet connectivity")
	}
	h.ack("dc-a")
	h.run(8)
	b = h.binding("dc-b")
	if len(b.Spec.RemoteSubnets) != 1 || b.Spec.RemoteSubnets[0].CIDR != "10.241.1.0/24" {
		t.Fatal("source removal acknowledgement did not produce remote withdrawal")
	}
	if h.subnet("apps").DeletionTimestamp.IsZero() || len(h.network().Status.SubnetClaims) != 3 {
		t.Fatal("unacknowledged remote site lost deletion/claim receipts")
	}
	h.ack("dc-b")
	h.run(12)
	var gone api.Subnet
	if err := h.c.Get(context.Background(), client.ObjectKey{Namespace: "project", Name: "apps"}, &gone); !apierrors.IsNotFound(err) {
		t.Fatalf("acknowledged drained subnet remains: %v", err)
	}
	if len(h.binding("dc-a").Spec.Subnets) != 1 || h.subnet("db").Status.VPCUID != "vpc-red" {
		t.Fatal("deleting one subnet removed its VPC or sibling")
	}
	h.ack("dc-a")
	h.ack("dc-b")
	h.run(8)
	if len(h.network().Status.SubnetClaims) != 2 {
		t.Fatal("withdrawn subnet claim was not released after all snapshot acknowledgements")
	}
}

func TestVpcDeletionWaitsForSubnetsAndLocalCleanup(t *testing.T) {
	h := newAuthorityHarness(t, publicVPC("red"), publicSubnet("apps", "red", "dc-a", "10.241.0.0/24"))
	h.run(25)
	h.ack("dc-a")
	h.run(8)
	v := h.network()
	if err := h.c.Delete(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	h.run(8)
	if !controllerutil.ContainsFinalizer(h.network(), platformplan.Finalizer) || h.network().Status.Phase != "Deleting" || h.subnet("apps").DeletionTimestamp != nil {
		t.Fatal("VPC delete cascaded native subnet ownership or escaped guard")
	}
	s := h.subnet("apps")
	if err := h.c.Delete(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	h.run(8)
	h.ack("dc-a")
	h.run(8)
	b := h.binding("dc-a")
	if !b.Spec.Deleting {
		t.Fatal("last subnet removal did not create a durable location tombstone")
	}
	if !controllerutil.ContainsFinalizer(h.network(), platformplan.Finalizer) {
		t.Fatal("VPC finalized before tombstone ack")
	}
	h.ack("dc-a")
	for i := 0; i < 15; i++ {
		h.step()
		var current api.VPC
		if apierrors.IsNotFound(h.c.Get(context.Background(), h.vpc, &current)) {
			return
		}
	}
	t.Fatalf("VPC cleanup did not complete after acknowledged absence: vpc=%+v bindings=%+v", h.network(), h.bindings())
}

func TestSameNameVpcReplacementDoesNotAdoptOldSubnet(t *testing.T) {
	s := publicSubnet("apps", "red", "dc-a", "10.241.0.0/24")
	s.Status.VPCUID = "old-vpc-uid"
	h := newAuthorityHarness(t, publicVPC("red"), s)
	h.run(15)
	if h.subnet("apps").Status.Phase != "Blocked" || len(h.network().Status.SubnetClaims) != 0 || len(h.bindings()) != 0 {
		t.Fatal("same-name VPC adopted old subnet incarnation")
	}
}

func TestRecordedBindingReplacementStopsReconciliation(t *testing.T) {
	h := newAuthorityHarness(t, publicVPC("red"), publicSubnet("apps", "red", "dc-a", "10.241.0.0/24"))
	h.run(25)
	b := h.binding("dc-a")
	if err := h.c.Delete(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	b.ResourceVersion = ""
	b.UID = "replacement-binding"
	if err := h.c.Create(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	h.run(8)
	if h.network().Status.Phase != "Blocked" {
		t.Fatal("same-name internal binding silently adopted")
	}
}

func TestLocationRejoinWaitsForTombstoneAndCreatesNewBindingIncarnation(t *testing.T) {
	h := newAuthorityHarness(t, publicVPC("red"), publicSubnet("apps", "red", "dc-a", "10.241.0.0/24"), publicSubnet("remote", "red", "dc-b", "10.242.0.0/24"))
	h.run(30)
	h.ack("dc-a")
	h.ack("dc-b")
	h.run(8)
	original := h.binding("dc-a")
	if err := h.c.Delete(context.Background(), h.subnet("apps")); err != nil {
		t.Fatal(err)
	}
	h.run(10)
	h.ack("dc-a")
	h.run(5)
	h.ack("dc-b")
	h.run(10)
	if !h.binding("dc-a").Spec.Deleting {
		t.Fatal("empty location did not receive a tombstone")
	}
	replacement := publicSubnet("replacement", "red", "dc-a", "10.241.1.0/24")
	if err := h.c.Create(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	h.run(10)
	if h.subnet("replacement").Status.VPCUID != "" || !h.binding("dc-a").Spec.Deleting || h.binding("dc-a").UID != original.UID {
		t.Fatal("new join resurrected an unacknowledged binding")
	}
	h.ack("dc-a")
	h.ack("dc-b")
	h.run(25)
	joined := h.binding("dc-a")
	if joined.UID == original.UID || joined.Spec.Deleting || len(joined.Spec.Subnets) != 1 || joined.Spec.Subnets[0].UID != string(replacement.UID) {
		t.Fatal("rejoin did not create a fresh binding incarnation")
	}
	if h.subnet("replacement").Status.VPCUID != "vpc-red" {
		t.Fatal("new subnet was never admitted after cleanup")
	}
}
