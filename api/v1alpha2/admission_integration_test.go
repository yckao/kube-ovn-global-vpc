//go:build integration

package v1alpha2

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestManagedAPIAdmission uses a real API server to validate the public API and
// internal snapshot contracts. It does not claim dataplane or peer discovery proof.
func TestManagedAPIAdmission(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS must reference installed envtest binaries")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	e := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd")}, ErrorIfCRDPathMissing: true, ControlPlaneStartTimeout: 45 * time.Second, ControlPlaneStopTimeout: 20 * time.Second}
	cfg, err := e.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	if err = AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err = corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"project-red", "project-blue", "location-a"} {
		if err = c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	vpc := &VPC{ObjectMeta: metav1.ObjectMeta{Namespace: "project-red", Name: "red"}}
	if err = c.Create(ctx, vpc); err != nil {
		t.Fatal(err)
	}
	if vpc.Spec.NetworkClassRef != DefaultNetworkClass || vpc.UID == "" {
		t.Fatal("default class or server identity missing")
	}
	other := &VPC{ObjectMeta: metav1.ObjectMeta{Namespace: "project-blue", Name: "red"}}
	if err = c.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	if other.UID == vpc.UID {
		t.Fatal("project-scoped VPC identities were conflated")
	}
	changed := vpc.DeepCopy()
	changed.Spec.NetworkClassRef = "native"
	if err = c.Update(ctx, changed); !apierrors.IsInvalid(err) {
		t.Fatalf("class change should be rejected: %v", err)
	}
	vpc.Status.ObservedGeneration = vpc.Generation
	vpc.Status.NetworkID = 11001
	vpc.Status.SubnetClaims = []PrefixClaim{{SubnetUID: "claim-uid", CIDR: "10.241.0.0/24", LocationRef: "dc-a"}}
	vpc.Status.Bindings = []BindingReference{{LocationRef: "dc-a", Name: "binding", UID: "binding-uid", NativeVpcName: "pv-red-a"}}
	if err = c.Status().Update(ctx, vpc); err != nil {
		t.Fatal(err)
	}
	var observed VPC
	if err = c.Get(ctx, client.ObjectKeyFromObject(vpc), &observed); err != nil {
		t.Fatal(err)
	}
	if len(observed.Status.SubnetClaims) != 1 || len(observed.Status.Bindings) != 1 {
		t.Fatal("durable admission receipts were pruned")
	}
	base := SubnetSpec{VPCRef: "red", LocationRef: "dc-a", CIDR: "10.241.0.0/24"}
	for i, tc := range []struct {
		cidr  string
		valid bool
	}{{"10.241.0.0/24", true}, {"10.241.0.0/30", true}, {"10.0.0.0/8", true}, {"10.241.0.1/24", false}, {"256.241.0.0/24", false}, {"10.241.0.0/31", false}, {"0.0.0.0/0", false}, {"10.241.0.0/33", false}, {"2001:db8::/64", false}, {"garbage", false}} {
		s := &Subnet{ObjectMeta: metav1.ObjectMeta{Namespace: "project-red", Name: fmt.Sprintf("case-%d", i)}, Spec: base}
		s.Spec.CIDR = tc.cidr
		err = c.Create(ctx, s)
		if tc.valid && err != nil {
			t.Fatalf("canonical subnet %s rejected: %v", tc.cidr, err)
		}
		if !tc.valid && !apierrors.IsInvalid(err) {
			t.Fatalf("invalid subnet %s accepted: %v", tc.cidr, err)
		}
	}
	subnet := &Subnet{ObjectMeta: metav1.ObjectMeta{Namespace: "project-red", Name: "apps"}, Spec: base}
	if err = c.Create(ctx, subnet); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func(*SubnetSpec){func(s *SubnetSpec) { s.VPCRef = "blue" }, func(s *SubnetSpec) { s.LocationRef = "dc-b" }, func(s *SubnetSpec) { s.CIDR = "10.242.0.0/24" }} {
		copy := subnet.DeepCopy()
		edit(&copy.Spec)
		if err = c.Update(ctx, copy); !apierrors.IsInvalid(err) {
			t.Fatalf("immutable subnet identity changed: %v", err)
		}
	}
	binding := &NetworkBinding{ObjectMeta: metav1.ObjectMeta{Namespace: "location-a", Name: "binding"}, Spec: bindingFixture()}
	if err = c.Create(ctx, binding); err != nil {
		t.Fatal(err)
	}
	binding.Spec.Revision = "revision-2"
	binding.Spec.Subnets = append(binding.Spec.Subnets, BindingSubnet{Name: "db", UID: "db-uid", CIDR: "10.241.1.0/24", Gateway: "10.241.1.1", NativeSubnetName: "ps-red-db"})
	binding.Spec.AuthorizedLocations = []string{"dc-b"}
	binding.Spec.RemoteSubnets = []RemoteSubnet{{LocationRef: "dc-b", CIDR: "10.242.0.0/24"}}
	binding.Spec.Peers = []PeerGateway{{LocationRef: "dc-b", Endpoint: GatewayEndpoint{ID: "gateway-b", NodeName: "node-b", NodeUID: "node-b-uid", EndpointIP: "192.0.2.2", TransitIP: "10.240.255.2", Ready: true}}}
	binding.Spec.LinkAllocations = []LinkAllocation{{PeerA: "dc-a/gateway-a", PeerB: "dc-b/gateway-b", ControlVNI: 12001}}
	if err = c.Update(ctx, binding); err != nil {
		t.Fatalf("complete snapshot update rejected: %v", err)
	}
	for _, edit := range []func(*NetworkBindingSpec){func(s *NetworkBindingSpec) { s.VPCRef.UID = "replacement" }, func(s *NetworkBindingSpec) { s.VPCRef.Namespace = "project-blue" }, func(s *NetworkBindingSpec) { s.LocationRef = "dc-b" }, func(s *NetworkBindingSpec) { s.NativeVpcName = "foreign-vpc" }, func(s *NetworkBindingSpec) { s.TransportProfile = "geneve-bgp" }, func(s *NetworkBindingSpec) { s.NetworkID++ }, func(s *NetworkBindingSpec) { s.LocalASN++ }, func(s *NetworkBindingSpec) {
		s.LinkAllocations[0].PeerA, s.LinkAllocations[0].PeerB = s.LinkAllocations[0].PeerB, s.LinkAllocations[0].PeerA
	}, func(s *NetworkBindingSpec) {
		s.Peers[0].Endpoint.Links = []GatewayLink{{PeerID: "dc-a/gateway-a", ListenPort: 51820, TunnelIP: "256.0.0.1"}}
	}} {
		copy := binding.DeepCopy()
		edit(&copy.Spec)
		if err = c.Update(ctx, copy); !apierrors.IsInvalid(err) {
			t.Fatalf("binding retarget accepted: %v", err)
		}
	}
	binding.Status.ObservedGeneration = binding.Generation
	binding.Status.AppliedRevision = binding.Spec.Revision
	binding.Status.Gateways = []GatewayEndpoint{{ID: "gateway-a", NodeName: "node-a", NodeUID: "node-a-uid", EndpointIP: "192.0.2.1", TransitIP: "10.240.255.1", LocalASN: 64512, HealthIP: "10.254.1.1", Links: []GatewayLink{{PeerID: "dc-b/gateway-b", ListenPort: 51820, TunnelIP: "10.254.2.1", ControlVNI: 12001}}, PublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", Ready: true}}
	binding.Status.Resources = []ResourceRecord{{APIVersion: "kubeovn.io/v1", Kind: "Vpc", Name: "pv-red-a", UID: "native-vpc-uid"}}
	if err = c.Status().Update(ctx, binding); err != nil {
		t.Fatal(err)
	}
	var got NetworkBinding
	if err = c.Get(ctx, client.ObjectKeyFromObject(binding), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.AppliedRevision != "revision-2" || len(got.Status.Gateways) != 1 || got.Status.Gateways[0].PublicKey == "" || len(got.Status.Gateways[0].Links) != 1 || got.Status.Gateways[0].Links[0].ControlVNI != 12001 || len(got.Status.Resources) != 1 || len(got.Spec.Subnets) != 2 || len(got.Spec.Peers) != 1 || len(got.Spec.LinkAllocations) != 1 || got.Spec.LinkAllocations[0].ControlVNI != 12001 {
		t.Fatal("snapshot, public membership or native identity receipts were pruned")
	}
	binding = &got
	binding.Spec.Deleting = true
	if err = c.Update(ctx, binding); err != nil {
		t.Fatal(err)
	}
	revived := binding.DeepCopy()
	revived.Spec.Deleting = false
	if err = c.Update(ctx, revived); !apierrors.IsInvalid(err) {
		t.Fatalf("removing deletion tombstone was accepted: %v", err)
	}
	if err = c.Patch(ctx, binding, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"deleting":false}}`))); !apierrors.IsInvalid(err) {
		t.Fatalf("clearing deletion tombstone was accepted: %v", err)
	}
}
