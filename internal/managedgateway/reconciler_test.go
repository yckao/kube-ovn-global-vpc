package managedgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	api "globalvpc.io/controller/api/v1alpha2"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func setup(t *testing.T) (*Reconciler, *api.NetworkBinding, Attachment) {
	t.Helper()
	s := runtime.NewScheme()
	_ = core.AddToScheme(s)
	_ = api.AddToScheme(s)
	objects := []client.Object{}
	for i := 1; i <= 3; i++ {
		objects = append(objects, &core.Node{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("node-%d", i), UID: types.UID(fmt.Sprintf("node-uid-%d", i)), Labels: map[string]string{"gateway": "true"}}, Status: core.NodeStatus{Addresses: []core.NodeAddress{{Type: core.NodeInternalIP, Address: fmt.Sprintf("192.0.2.%d", i)}}, Conditions: []core.NodeCondition{{Type: core.NodeReady, Status: core.ConditionTrue}}}})
	}
	var mu sync.Mutex
	serial := 0
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objects...).WithStatusSubresource(&core.Pod{}).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
		mu.Lock()
		serial++
		o.SetUID(types.UID(fmt.Sprintf("resource-%d", serial)))
		mu.Unlock()
		return c.Create(ctx, o, opts...)
	}}).Build()
	r := &Reconciler{Client: c, Namespace: "local", ClusterUID: "cluster-a", Image: "example.invalid/gateway:test", RuntimeDir: "../../gateway", NodeSelector: map[string]string{"gateway": "true"}, Replicas: 2, MTU: 1380, AllocationPool: "10.254.1.0/24", PortStart: 32000, PortEnd: 32100, RolloutProbe: func(context.Context, *core.Pod, map[string]any) (bool, error) { return true, nil }}
	b := &api.NetworkBinding{ObjectMeta: metav1.ObjectMeta{Name: "binding-a", Namespace: "local", UID: "binding-uid-a"}, Spec: api.NetworkBindingSpec{VPCRef: api.ObjectIdentity{Name: "tenant", Namespace: "project", UID: "tenant-uid"}, LocationRef: "site-a", NativeVpcName: "native-a", LocalASN: 4200000001, NetworkID: 1001, TransportProfile: "wireguard-bgp", AuthorizedLocations: []string{"site-a", "site-b"}, Subnets: []api.BindingSubnet{{Name: "one", UID: "s1", CIDR: "10.252.1.0/24"}, {Name: "two", UID: "s2", CIDR: "10.252.2.0/24"}}}}
	a := Attachment{TransitCIDR: "10.253.1.0/29", RouterIP: "10.253.1.1", BFDSourceIP: "10.254.2.1"}
	return r, b, a
}
func listPods(t *testing.T, r *Reconciler) []core.Pod {
	t.Helper()
	var p core.PodList
	if err := r.Client.List(context.Background(), &p, client.InNamespace(r.Namespace)); err != nil {
		t.Fatal(err)
	}
	return p.Items
}
func markReady(t *testing.T, r *Reconciler) {
	t.Helper()
	for _, p := range listPods(t, r) {
		p.Status.Conditions = []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}}
		if err := r.Client.Status().Update(context.Background(), &p); err != nil {
			t.Fatal(err)
		}
	}
}
func ensure(t *testing.T, r *Reconciler, b *api.NetworkBinding, a Attachment) Result {
	t.Helper()
	v, e := r.Ensure(context.Background(), b, a)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func TestDurableIdentitiesAndMultiSubnetRuntime(t *testing.T) {
	r, b, a := setup(t)
	first := ensure(t, r, b, a)
	if len(first.Gateways) != 2 || len(listPods(t, r)) != 2 {
		t.Fatal("members not created")
	}
	markReady(t, r)
	second := ensure(t, r, b, a)
	if !second.Ready {
		t.Fatal(second.Reason)
	}
	for i, g := range first.Gateways {
		h := second.Gateways[i]
		if h.ID != g.ID || h.PublicKey != g.PublicKey || h.NodeUID != g.NodeUID || h.TransitIP != g.TransitIP {
			t.Fatal("identity changed")
		}
	}
	for _, p := range listPods(t, r) {
		if p.Spec.HostNetwork || !p.Spec.HostPID || *p.Spec.AutomountServiceAccountToken {
			t.Fatal("unsafe Pod isolation")
		}
		var cm core.ConfigMap
		if err := r.Client.Get(context.Background(), types.NamespacedName{Namespace: r.Namespace, Name: p.Spec.Volumes[0].ConfigMap.Name}, &cm); err != nil {
			t.Fatal(err)
		}
		var c map[string]any
		_ = json.Unmarshal([]byte(cm.Data["gateway.json"]), &c)
		if len(c["gateway"].(map[string]any)["cidrs"].([]any)) != 2 {
			t.Fatal("multiple subnets lost")
		}
		if strings.Contains(cm.Data["gateway.json"], "privateKey\"") {
			t.Fatal("private key leaked into config")
		}
	}
}
func TestReciprocalDiscoveryPublishesBeforePods(t *testing.T) {
	r, b, a := setup(t)
	b.Spec.RemoteSubnets = []api.RemoteSubnet{{LocationRef: "site-b", CIDR: "10.252.3.0/24"}}
	b.Spec.Peers = []api.PeerGateway{{LocationRef: "site-b", Endpoint: api.GatewayEndpoint{ID: "g-remote", NodeUID: "remote-node", NodeName: "remote-node", EndpointIP: "192.0.2.9", HealthIP: "10.254.3.1", LocalASN: 4200000002, PublicKey: "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="}}}
	v := ensure(t, r, b, a)
	if v.Reason != "DiscoveringPeerAllocations" || len(v.Gateways) != 2 || len(listPods(t, r)) != 0 {
		t.Fatal("peer discovery did not wait safely")
	}
	for i, g := range v.Gateways {
		if len(g.Links) != 1 || g.PublicKey == "" {
			t.Fatal("public allocations missing")
		}
		b.Spec.Peers[0].Endpoint.Links = append(b.Spec.Peers[0].Endpoint.Links, api.GatewayLink{PeerID: peerID("site-a", g.ID), ListenPort: int32(33000 + i), TunnelIP: fmt.Sprintf("10.254.3.%d", i+2)})
	}
	ensure(t, r, b, a)
	if len(listPods(t, r)) != 2 {
		t.Fatal("reciprocal peers did not converge")
	}
}
func TestNodeUIDReplacementBlocksWithoutReusingIdentity(t *testing.T) {
	r, b, a := setup(t)
	ensure(t, r, b, a)
	var n core.Node
	_ = r.Client.Get(context.Background(), types.NamespacedName{Name: "node-1"}, &n)
	n.UID = "new-node-uid"
	_ = r.Client.Update(context.Background(), &n)
	_, err := r.Ensure(context.Background(), b, a)
	if err == nil || !strings.Contains(err.Error(), "NodeIdentityChanged") {
		t.Fatalf("expected fencing, got %v", err)
	}
	if len(listPods(t, r)) != 2 {
		t.Fatal("old gateways modified")
	}
}
func TestRolloutRetiresOnlyOneAndWaitsNativeHealth(t *testing.T) {
	r, b, a := setup(t)
	ensure(t, r, b, a)
	markReady(t, r)
	b.Spec.Subnets = append(b.Spec.Subnets, api.BindingSubnet{Name: "three", UID: "s3", CIDR: "10.252.4.0/24"})
	r.RolloutProbe = func(context.Context, *core.Pod, map[string]any) (bool, error) { return false, nil }
	v := ensure(t, r, b, a)
	if v.Reason != "WaitingForSiblingNativeConvergence" || len(listPods(t, r)) != 2 {
		t.Fatal("unhealthy sibling allowed rollout")
	}
	r.RolloutProbe = func(context.Context, *core.Pod, map[string]any) (bool, error) { return true, nil }
	v = ensure(t, r, b, a)
	if v.Reason != "RollingGatewayGeneration" || len(listPods(t, r)) != 1 {
		t.Fatal("expected one retirement")
	}
	ensure(t, r, b, a)
	if len(listPods(t, r)) != 2 {
		t.Fatal("replacement not created")
	}
	v = ensure(t, r, b, a)
	if len(listPods(t, r)) != 2 || v.Ready {
		t.Fatal("unready replacement allowed second retirement")
	}
	markReady(t, r)
	ensure(t, r, b, a)
	if len(listPods(t, r)) != 1 {
		t.Fatal("second retirement not allowed after readiness")
	}
}

func TestWithdrawalProbeReceivesEachSurvivorsDesiredConfig(t *testing.T) {
	r, b, a := setup(t)
	b.Spec.RemoteSubnets = []api.RemoteSubnet{{LocationRef: "site-b", CIDR: "10.252.3.0/24"}, {LocationRef: "site-b", CIDR: "10.252.4.0/24"}}
	b.Spec.Peers = []api.PeerGateway{{LocationRef: "site-b", Endpoint: api.GatewayEndpoint{ID: "g-remote", NodeUID: "remote-node", NodeName: "remote-node", EndpointIP: "192.0.2.9", HealthIP: "10.254.3.1", LocalASN: 4200000002, PublicKey: "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="}}}
	v := ensure(t, r, b, a)
	for i, g := range v.Gateways {
		b.Spec.Peers[0].Endpoint.Links = append(b.Spec.Peers[0].Endpoint.Links, api.GatewayLink{PeerID: peerID("site-a", g.ID), ListenPort: int32(33000 + i), TunnelIP: fmt.Sprintf("10.254.3.%d", i+2)})
	}
	ensure(t, r, b, a)
	markReady(t, r)
	if !ensure(t, r, b, a).Ready {
		t.Fatal("initial generation not ready")
	}
	b.Spec.RemoteSubnets = b.Spec.RemoteSubnets[:1]
	probed := 0
	r.RolloutProbe = func(ctx context.Context, sibling *core.Pod, desired map[string]any) (bool, error) {
		probed++
		var cm core.ConfigMap
		if err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: sibling.Spec.Volumes[0].ConfigMap.Name}, &cm); err != nil {
			return false, err
		}
		var old map[string]any
		if err := json.Unmarshal([]byte(cm.Data["gateway.json"]), &old); err != nil {
			return false, err
		}
		if desired["gateway"].(map[string]any)["gatewayID"] != old["gateway"].(map[string]any)["gatewayID"] {
			t.Fatal("probe received retiring member's config instead of survivor's")
		}
		oldLink := old["transport"].(map[string]any)["links"].([]any)[0].(map[string]any)
		newLink := desired["transport"].(map[string]any)["links"].([]map[string]any)[0]
		if len(oldLink["delegatedPrefixes"].([]any)) != 2 || len(newLink["delegatedPrefixes"].([]string)) != 1 || oldLink["localTunnelIP"] != newLink["localTunnelIP"] {
			t.Fatal("probe did not receive the survivor's withdrawn-prefix configuration")
		}
		return true, nil
	}
	if result := ensure(t, r, b, a); result.Reason != "RollingGatewayGeneration" || probed != 1 || len(listPods(t, r)) != 1 {
		t.Fatalf("withdrawal did not safely retire one generation: %+v, probes=%d", result, probed)
	}
}
func TestDeleteWaitsForPodAbsenceAndRetainsAllocationReceipts(t *testing.T) {
	r, b, a := setup(t)
	ensure(t, r, b, a)
	pods := listPods(t, r)
	pods[0].Finalizers = []string{"test.example/hold"}
	_ = r.Client.Update(context.Background(), &pods[0])
	gone, err := r.Delete(context.Background(), b)
	if err != nil || gone {
		t.Fatal("premature completion", err)
	}
	var secretList core.SecretList
	_ = r.Client.List(context.Background(), &secretList)
	if len(secretList.Items) != 2 {
		t.Fatal("keys removed before Pod absence")
	}
	var held core.Pod
	_ = r.Client.Get(context.Background(), client.ObjectKeyFromObject(&pods[0]), &held)
	held.Finalizers = nil
	_ = r.Client.Update(context.Background(), &held)
	gone, err = r.Delete(context.Background(), b)
	if err != nil || !gone {
		t.Fatal("cleanup incomplete", err)
	}
	var cm core.ConfigMap
	if err = r.Client.Get(context.Background(), types.NamespacedName{Namespace: r.Namespace, Name: "managed-gateway-allocations"}, &cm); err != nil {
		t.Fatal("allocation receipts removed")
	}
}
func TestForeignLedgerAndModifiedKeyAreNeverAdopted(t *testing.T) {
	r, b, a := setup(t)
	cm := &core.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ledgerName(b), Namespace: r.Namespace}, Data: map[string]string{ledgerData: "{}"}}
	_ = r.Client.Create(context.Background(), cm)
	if _, err := r.Ensure(context.Background(), b, a); err == nil {
		t.Fatal("foreign ledger adopted")
	}
	if len(listPods(t, r)) != 0 {
		t.Fatal("foreign collision created gateway")
	}
}
func TestConcurrentAllocatorUsesUniqueReceipts(t *testing.T) {
	r, _, _ := setup(t)
	if _, err := r.address(context.Background(), "initial"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	values := map[string]bool{}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ip, err := r.address(context.Background(), fmt.Sprintf("claim-%d", i))
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if values[ip] {
				t.Error("duplicate address")
			}
			values[ip] = true
		}(i)
	}
	wg.Wait()
	if len(values) != 8 {
		t.Fatal("claims missing")
	}
}
func TestNativePairAllocationsAreAuthorityIssuedAndSymmetric(t *testing.T) {
	r, b, a := setup(t)
	b.Spec.TransportProfile = "geneve-bgp"
	b.Spec.RemoteSubnets = []api.RemoteSubnet{{LocationRef: "site-b", CIDR: "10.252.3.0/24"}}
	b.Spec.Peers = []api.PeerGateway{{LocationRef: "site-b", Endpoint: api.GatewayEndpoint{ID: "remote", EndpointIP: "192.0.2.9", HealthIP: "10.254.3.1", LocalASN: 4200000002}}}
	v := ensure(t, r, b, a)
	if len(listPods(t, r)) != 0 {
		t.Fatal("native link without authority VNI")
	}
	for i, g := range v.Gateways {
		vni := uint32(2000 + i)
		b.Spec.LinkAllocations = append(b.Spec.LinkAllocations, api.LinkAllocation{PeerA: peerID("site-a", g.ID), PeerB: "site-b/remote", ControlVNI: vni})
		b.Spec.Peers[0].Endpoint.Links = append(b.Spec.Peers[0].Endpoint.Links, api.GatewayLink{PeerID: peerID("site-a", g.ID), ListenPort: 6082, TunnelIP: fmt.Sprintf("10.254.3.%d", i+2), ControlVNI: vni})
	}
	v = ensure(t, r, b, a)
	if len(listPods(t, r)) != 2 {
		t.Fatal(v.Reason)
	}
	for _, g := range v.Gateways {
		if g.PublicKey != "" || g.Links[0].ListenPort != 6082 {
			t.Fatal("native transport accidentally uses key or wrong port")
		}
	}
}

func TestPublishedKeyLossAndLedgerLossRequireRecovery(t *testing.T) {
	for _, lost := range []string{"key", "ledger"} {
		t.Run(lost, func(t *testing.T) {
			r, b, a := setup(t)
			ensure(t, r, b, a)
			if lost == "key" {
				var keys core.SecretList
				_ = r.Client.List(context.Background(), &keys)
				_ = r.Client.Delete(context.Background(), &keys.Items[0])
			} else {
				var cm core.ConfigMap
				_ = r.Client.Get(context.Background(), types.NamespacedName{Namespace: r.Namespace, Name: ledgerName(b)}, &cm)
				_ = r.Client.Delete(context.Background(), &cm)
			}
			_, err := r.Ensure(context.Background(), b, a)
			if err == nil {
				t.Fatal("lost durable identity was regenerated")
			}
			if len(listPods(t, r)) != 2 {
				t.Fatal("running members changed")
			}
		})
	}
}
func TestInsufficientNodesDoesNotBurnControlAddresses(t *testing.T) {
	r, b, a := setup(t)
	r.Replicas = 4
	for i := 0; i < 3; i++ {
		_, err := r.Ensure(context.Background(), b, a)
		if err == nil {
			t.Fatal("too few distinct nodes accepted")
		}
	}
	var cms core.ConfigMapList
	_ = r.Client.List(context.Background(), &cms)
	if len(cms.Items) != 0 {
		t.Fatal("failed placement consumed durable allocations")
	}
}
func TestNativeForeignReciprocalVNIDoesNotDeploy(t *testing.T) {
	r, b, a := setup(t)
	b.Spec.TransportProfile = "vxlan-evpn"
	b.Spec.RemoteSubnets = []api.RemoteSubnet{{LocationRef: "site-b", CIDR: "10.252.3.0/24"}}
	b.Spec.Peers = []api.PeerGateway{{LocationRef: "site-b", Endpoint: api.GatewayEndpoint{ID: "remote", EndpointIP: "192.0.2.9", HealthIP: "10.254.3.1", LocalASN: 4200000002}}}
	v := ensure(t, r, b, a)
	for i, g := range v.Gateways {
		b.Spec.LinkAllocations = append(b.Spec.LinkAllocations, api.LinkAllocation{PeerA: peerID("site-a", g.ID), PeerB: "site-b/remote", ControlVNI: uint32(2000 + i)})
		b.Spec.Peers[0].Endpoint.Links = append(b.Spec.Peers[0].Endpoint.Links, api.GatewayLink{PeerID: peerID("site-a", g.ID), ListenPort: 4788, TunnelIP: fmt.Sprintf("10.254.3.%d", i+2), ControlVNI: uint32(3000 + i)})
	}
	if _, err := r.Ensure(context.Background(), b, a); err == nil {
		t.Fatal("inconsistent authority allocation adopted")
	}
	if len(listPods(t, r)) != 0 {
		t.Fatal("native Pods created with mismatched VNI")
	}
}

func TestCleanupOwnershipLabelDriftDoesNotBecomeFalseAbsence(t *testing.T) {
	for _, kind := range []string{"Pod", "ConfigMap", "Secret"} {
		t.Run(kind, func(t *testing.T) {
			r, b, a := setup(t)
			ensure(t, r, b, a)
			var o client.Object
			switch kind {
			case "Pod":
				p := listPods(t, r)[0]
				o = &p
			case "ConfigMap":
				var cms core.ConfigMapList
				_ = r.Client.List(context.Background(), &cms, client.MatchingLabels{api.OwnerLabel: string(b.UID)})
				o = &cms.Items[0]
			case "Secret":
				var keys core.SecretList
				_ = r.Client.List(context.Background(), &keys)
				o = &keys.Items[0]
			}
			delete(o.GetLabels(), api.OwnerLabel)
			_ = r.Client.Update(context.Background(), o)
			gone, err := r.Delete(context.Background(), b)
			if err == nil || gone {
				t.Fatal("ownership drift falsely reported cleanup")
			}
			if len(listPods(t, r)) != 2 {
				t.Fatal("partial deletion occurred before preflight")
			}
		})
	}
}

func TestOwnedPodHashCannotHideRuntimeTampering(t *testing.T) {
	for _, change := range []func(*core.Pod){func(p *core.Pod) { p.Spec.Containers[0].Image = "example.invalid/changed:test" }, func(p *core.Pod) { p.Spec.HostNetwork = true }, func(p *core.Pod) { p.Spec.Containers[0].Command = []string{"sleep", "3600"} }, func(p *core.Pod) { p.Spec.AutomountServiceAccountToken = ptr(true) }, func(p *core.Pod) { p.Spec.Volumes[1].Secret.SecretName = "foreign-key" }} {
		r, b, a := setup(t)
		ensure(t, r, b, a)
		markReady(t, r)
		p := listPods(t, r)[0]
		change(&p)
		_ = r.Client.Update(context.Background(), &p)
		if _, err := r.Ensure(context.Background(), b, a); err == nil {
			t.Fatal("modified owned Pod was accepted based on hash annotation")
		}
		if len(listPods(t, r)) != 2 {
			t.Fatal("partial rollout happened after tamper")
		}
	}
}
