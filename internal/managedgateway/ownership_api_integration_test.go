//go:build integration

package managedgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	api "globalvpc.io/controller/api/v1alpha2"
	core "k8s.io/api/core/v1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// One isolated real API server runs all cases. It does not include a kubelet,
// scheduler, native Kube-OVN controller, garbage collector or gateway runtime.
// Production reconciliation is exercised; only committed-response loss and a
// competing write are injected around the real client.
func TestActualGatewayAPIOwnershipAndAllocation(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS must reference installed envtest binaries")
	}
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
	_ = core.AddToScheme(scheme)
	_ = api.AddToScheme(scheme)
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	version, err := discoveryClient.ServerVersion()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("real API version=%s; scope=Kubernetes UID/immutable/CAS and production gateway reconciler; dataplane not run", version.GitVersion)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	serial := 0
	fixture := func(t *testing.T) (*Reconciler, *api.NetworkBinding, Attachment) {
		t.Helper()
		serial++
		namespace := &core.Namespace{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("ownership-api-%d", serial)}}
		if err := c.Create(ctx, namespace); err != nil {
			t.Fatal(err)
		}
		if err := c.Create(ctx, &core.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: namespace.Name}}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			node := &core.Node{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-node-%d", namespace.Name, i), Labels: map[string]string{"ownership-api": namespace.Name}}}
			if err := c.Create(ctx, node); err != nil {
				t.Fatal(err)
			}
			node.Status = core.NodeStatus{Addresses: []core.NodeAddress{{Type: core.NodeInternalIP, Address: fmt.Sprintf("192.0.2.%d", i+1)}}, Conditions: []core.NodeCondition{{Type: core.NodeReady, Status: core.ConditionTrue}}}
			if err := c.Status().Update(ctx, node); err != nil {
				t.Fatal(err)
			}
			// TaintNodesByCondition admission adds NotReady during creation.
			// Envtest has no node lifecycle controller to remove that taint after
			// our explicit Ready status simulator reports the node healthy.
			node.Spec.Taints = nil
			if err := c.Update(ctx, node); err != nil {
				t.Fatal(err)
			}
			if !eligible(node) || node.Status.Addresses[0].Address == "" {
				t.Fatal("explicit Node Ready simulator did not create an eligible fixture")
			}
		}
		vpc := &api.VPC{ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: namespace.Name}}
		if err := c.Create(ctx, vpc); err != nil {
			t.Fatal(err)
		}
		subnet := &api.Subnet{ObjectMeta: metav1.ObjectMeta{Name: "apps", Namespace: namespace.Name}, Spec: api.SubnetSpec{VPCRef: vpc.Name, LocationRef: "site-a", CIDR: "10.252.1.0/24"}}
		if err := c.Create(ctx, subnet); err != nil {
			t.Fatal(err)
		}
		b := &api.NetworkBinding{ObjectMeta: metav1.ObjectMeta{Name: "binding", Namespace: namespace.Name}, Spec: api.NetworkBindingSpec{
			VPCRef: api.ObjectIdentity{Name: vpc.Name, Namespace: vpc.Namespace, UID: string(vpc.UID)}, LocationRef: "site-a", NetworkClassRef: "default", NativeVpcName: "native-" + namespace.Name,
			LocalASN: 4200000001, NetworkID: uint32(1000 + serial), TransportProfile: "wireguard-bgp", AuthorizedLocations: []string{"site-a"}, Revision: "ownership-api-initial",
			Subnets: []api.BindingSubnet{{Name: subnet.Name, UID: string(subnet.UID), CIDR: subnet.Spec.CIDR, Gateway: "10.252.1.1", NativeSubnetName: "apps-" + namespace.Name}},
		}}
		if err := c.Create(ctx, b); err != nil {
			t.Fatal(err)
		}
		r := &Reconciler{Client: c, Namespace: namespace.Name, ClusterUID: string(namespace.UID), Image: "example.invalid/gateway:api-test", RuntimeDir: "../../gateway", NodeSelector: map[string]string{"ownership-api": namespace.Name}, Replicas: 2, MTU: 1380, AllocationPool: "10.254.1.0/24", PortStart: 32000, PortEnd: 32100}
		a := Attachment{TransitCIDR: "10.253.1.0/29", RouterIP: "10.253.1.1", BFDSourceIP: "10.254.2.1"}
		if b.UID == "" || b.ResourceVersion == "" || b.Generation != 1 {
			t.Fatal("fixture did not receive actual API identity and generation")
		}
		return r, b, a
	}
	publish := func(t *testing.T, r *Reconciler, b *api.NetworkBinding, a Attachment) Result {
		t.Helper()
		result, err := r.Ensure(ctx, b, a)
		if err != nil {
			t.Fatalf("production Ensure failed: %v", err)
		}
		b.Status = api.NetworkBindingStatus{Gateways: result.Gateways, Resources: result.Resources}
		if err := c.Status().Update(ctx, b); err != nil {
			t.Fatal(err)
		}
		if len(result.Gateways) != 2 || len(listPods(t, r)) != 2 {
			t.Fatal("production Ensure did not persist two members and Pods")
		}
		return result
	}

	for _, damage := range []string{"registry-missing", "registry-replaced", "anchor-missing", "claim-rewritten"} {
		t.Run(damage, func(t *testing.T) {
			r, b, a := fixture(t)
			observed := publish(t, r, b, a)
			oldPods := gatewayAPIUIDs(listPods(t, r))
			members := registryCM(t, r, ledgerName(b)).DeepCopy()
			registry := registryCM(t, r, gatewayRegistryName)
			oldUID := registry.UID
			switch damage {
			case "registry-missing", "registry-replaced":
				if err := c.Delete(ctx, registry, client.Preconditions{UID: &registry.UID, ResourceVersion: &registry.ResourceVersion}); err != nil {
					t.Fatal(err)
				}
				if damage == "registry-replaced" {
					registry.ObjectMeta = metav1.ObjectMeta{Name: registry.Name, Namespace: registry.Namespace, Labels: registry.Labels, Annotations: registry.Annotations}
					if err := c.Create(ctx, registry); err != nil {
						t.Fatal(err)
					}
					if registry.UID == oldUID {
						t.Fatal("real API reused the deleted registry UID")
					}
					t.Logf("registry replacement oldUID=%s newUID=%s; identical content is rejected", oldUID, registry.UID)
				}
			case "anchor-missing":
				if err := c.Delete(ctx, registryCM(t, r, gatewayRegistryIdentityName)); err != nil {
					t.Fatal(err)
				}
			case "claim-rewritten":
				state, err := r.decodeRegistry(registry)
				if err != nil {
					t.Fatal(err)
				}
				state.Addresses[string(b.UID)+"/"+observed.Gateways[0].ID+"/health"] = "10.254.1.250"
				storeRegistry(registry, state)
				if err := c.Update(ctx, registry); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := r.Ensure(ctx, b, a); err == nil {
				t.Fatal("existing member accepted damaged allocation state")
			}
			unknown := b.DeepCopy()
			unknown.ObjectMeta = metav1.ObjectMeta{Name: "unknown-binding", Namespace: r.Namespace}
			unknown.Status = api.NetworkBindingStatus{}
			unknown.Spec.NativeVpcName += "-unknown"
			if err := c.Create(ctx, unknown); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Ensure(ctx, unknown, a); err == nil {
				t.Fatal("previously unknown persisted owner bypassed damaged registry")
			}
			if value, err := r.address(ctx, "future-owner/health"); err == nil || value != "" {
				t.Fatal("future unknown allocation bypassed damage guard")
			}
			if !reflect.DeepEqual(oldPods, gatewayAPIUIDs(listPods(t, r))) || !reflect.DeepEqual(members, registryCM(t, r, ledgerName(b))) {
				t.Fatal("rejected operation changed accepted Pods or membership ledger")
			}
			if damage == "registry-missing" || damage == "anchor-missing" {
				name := gatewayRegistryName
				if damage == "anchor-missing" {
					name = gatewayRegistryIdentityName
				}
				var absent core.ConfigMap
				if err := c.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: name}, &absent); !errors.IsNotFound(err) {
					t.Fatal("missing allocation state was silently regenerated")
				}
			}
		})
	}

	for _, stage := range []string{"registry-create", "anchor-create", "address-update", "port-update"} {
		t.Run("lost-response-"+stage, func(t *testing.T) {
			r, _, _ := fixture(t)
			const claim = "owner/member/remote/member"
			port := stage == "port-update"
			if port {
				if _, err := r.address(ctx, claim); err != nil {
					t.Fatal(err)
				}
			}
			uncertain := &lostRegistryResponse{Client: c, operation: "update", target: gatewayRegistryName}
			if stage == "registry-create" {
				uncertain.operation = "create"
			} else if stage == "anchor-create" {
				uncertain.operation, uncertain.target = "create", gatewayRegistryIdentityName
			}
			r.Client = uncertain
			if value, err := r.allocate(ctx, claim, port, "node"); err == nil || value != "" || !uncertain.fired {
				t.Fatal("a committed but unconfirmed allocation was exposed")
			}
			r.Client = c // A new client invocation has no volatile allocation cache.
			value, err := r.allocate(ctx, claim, port, "node")
			if err != nil {
				t.Fatal(err)
			}
			want := "10.254.1.1"
			if port {
				want = "32000"
			}
			if value != want {
				t.Fatal("uncertain committed write consumed a second allocation")
			}
			cm := registryCM(t, r, gatewayRegistryName)
			state, err := r.decodeRegistry(cm)
			if err != nil || len(state.Addresses) != 1 || (port && len(state.Ports) != 1) {
				t.Fatal("lost response corrupted or duplicated retained claims")
			}
			anchor := registryCM(t, r, gatewayRegistryIdentityName)
			if anchor.UID == "" || anchor.Data["registryUID"] != string(cm.UID) {
				t.Fatal("recovered allocation lacks the real immutable UID anchor")
			}
			anchor.Data["registryUID"] = "attempted-replacement"
			if err := c.Update(ctx, anchor); !errors.IsInvalid(err) {
				t.Fatal("real API did not reject immutable anchor data change")
			}
		})
	}

	t.Run("real-cas-conflict-and-concurrent-unique-claims", func(t *testing.T) {
		r, _, _ := fixture(t)
		if _, err := r.address(ctx, "initial"); err != nil {
			t.Fatal(err)
		}
		competing := &gatewayAPICompetingWrite{Client: c, namespace: r.Namespace}
		r.Client = competing
		if value, err := r.address(ctx, "after-real-conflict"); err != nil || value != "10.254.1.2" || competing.conflicts != 1 {
			t.Fatal("production retry did not recover an actual API resourceVersion conflict")
		}
		r.Client = c
		var group sync.WaitGroup
		type outcome struct {
			claim, address, port string
			err                  error
		}
		request := func(claim string) outcome {
			result := outcome{claim: claim}
			result.address, result.err = r.address(ctx, claim)
			if result.err != nil {
				if result.address != "" {
					t.Error("failed address operation exposed an unconfirmed claim")
				}
				return result
			}
			result.port, result.err = r.allocate(ctx, claim, true, "shared-node")
			if result.err != nil && result.port != "" {
				t.Error("failed port operation exposed an unconfirmed claim")
			}
			return result
		}
		results := make(chan outcome, 8)
		for i := 0; i < 8; i++ {
			group.Add(1)
			go func(i int) {
				defer group.Done()
				results <- request(fmt.Sprintf("concurrent-%d/link", i))
			}(i)
		}
		group.Wait()
		close(results)
		addresses, ports := map[string]bool{}, map[string]bool{}
		conflicts, followups := 0, 0
		for first := range results {
			if first.err != nil && !errors.IsConflict(first.err) {
				t.Fatalf("contention returned a non-CAS failure: %v", first.err)
			}
			if first.err != nil {
				conflicts++
			}
			final := first
			// A reconciler may requeue after bounded CAS retries are exhausted.
			// Only a real Conflict is retryable here, with three follow-up calls
			// maximum. Registry corruption or identity errors never get retried.
			for round := 0; final.err != nil && round < 3; round++ {
				followups++
				final = request(first.claim)
				if final.err != nil && !errors.IsConflict(final.err) {
					t.Fatalf("follow-up returned a non-CAS failure: %v", final.err)
				}
			}
			if final.err != nil || final.address == "" || final.port == "" {
				t.Fatal("bounded follow-up reconciliation did not converge")
			}
			if (first.address != "" && first.address != final.address) || (first.port != "" && first.port != final.port) {
				t.Fatal("follow-up reconciliation changed a confirmed claim")
			}
			if addresses[final.address] || ports[final.port] {
				t.Fatal("CAS race reused an address or same-node port")
			}
			addresses[final.address], ports[final.port] = true, true
		}
		state, err := r.decodeRegistry(registryCM(t, r, gatewayRegistryName))
		if err != nil || len(state.Addresses) != 10 || len(state.Ports) != 8 || len(addresses) != 8 || len(ports) != 8 {
			t.Fatal("concurrent allocations were lost or duplicated")
		}
		t.Logf("eight concurrent claims: exhaustedConflictCalls=%d boundedFollowupCalls=%d; confirmed identities retained; exact final counts=10 addresses/8 ports", conflicts, followups)
	})

	for _, damage := range []string{"missing", "foreign-replacement"} {
		t.Run("published-key-"+damage, func(t *testing.T) {
			r, b, a := fixture(t)
			publish(t, r, b, a)
			oldPods := gatewayAPIUIDs(listPods(t, r))
			members := registryCM(t, r, ledgerName(b)).DeepCopy()
			var l ledger
			if err := json.Unmarshal([]byte(members.Data[ledgerData]), &l); err != nil {
				t.Fatal("member ledger decode failed")
			}
			key := &core.Secret{}
			if err := c.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: l.Members[0].SecretName}, key); err != nil {
				t.Fatal("expected generated key was not found")
			}
			oldUID := key.UID
			changed := key.DeepCopy()
			changed.Data["privateKey"] = []byte("attempted-mutation")
			if err := c.Update(ctx, changed); !errors.IsInvalid(err) {
				t.Fatal("real API did not reject immutable key data mutation")
			}
			if err := c.Delete(ctx, key, client.Preconditions{UID: &key.UID, ResourceVersion: &key.ResourceVersion}); err != nil {
				t.Fatal("key deletion fixture failed")
			}
			if damage == "foreign-replacement" {
				key.ObjectMeta = metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}
				if err := c.Create(ctx, key); err != nil || key.UID == oldUID {
					t.Fatal("foreign replacement did not receive a fresh actual UID")
				}
			}
			_, err := r.Ensure(ctx, b, a)
			if err == nil || (damage == "missing" && !strings.Contains(err.Error(), "GatewayKeyLost")) {
				t.Fatal("published key loss/replacement was silently accepted")
			}
			if !reflect.DeepEqual(oldPods, gatewayAPIUIDs(listPods(t, r))) || !reflect.DeepEqual(members, registryCM(t, r, ledgerName(b))) {
				t.Fatal("key refusal changed accepted Pods or public membership")
			}
			if damage == "missing" {
				var absent core.Secret
				if err := c.Get(ctx, client.ObjectKeyFromObject(key), &absent); !errors.IsNotFound(err) {
					t.Fatal("lost published private key was regenerated")
				}
			}
			// Do not log key objects, key bytes, or API validation errors containing data.
			t.Logf("key case=%s originalSecretUID=%s; published identity and gateway Pod UIDs retained", damage, oldUID)
		})
	}

	t.Run("key-create-response-loss-reuses-committed-secret", func(t *testing.T) {
		r, b, a := fixture(t)
		l, err := r.members(ctx, b, a)
		if err != nil {
			t.Fatal(err)
		}
		m := l.Members[0]
		uncertain := &lostRegistryResponse{Client: c, operation: "create", target: m.SecretName}
		r.Client = uncertain
		if err := r.ensureKey(ctx, b, &m); err == nil || !uncertain.fired || m.Endpoint.PublicKey != "" {
			t.Fatal("uncertain private-key creation exposed an unconfirmed public identity")
		}
		committed := &core.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: m.SecretName}, committed); err != nil {
			t.Fatal("uncertain secret write was not actually committed")
		}
		uid := committed.UID
		public := string(committed.Data["publicKey"])
		r.Client = c
		if err := r.ensureKey(ctx, b, &m); err != nil || m.Endpoint.PublicKey != public {
			t.Fatal("restart did not recover the committed key identity")
		}
		current := &core.Secret{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(committed), current); err != nil || current.UID != uid {
			t.Fatal("retry replaced the committed immutable secret")
		}
	})
}

func gatewayAPIUIDs(pods []core.Pod) map[string]types.UID {
	result := map[string]types.UID{}
	for _, pod := range pods {
		result[pod.Name] = pod.UID
	}
	return result
}

// This writer advances the real registry resourceVersion between production's
// read and update. The returned Conflict originates in kube-apiserver itself.
type gatewayAPICompetingWrite struct {
	client.Client
	namespace string
	once      sync.Once
	conflicts int
}

func (c *gatewayAPICompetingWrite) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if obj.GetNamespace() == c.namespace && obj.GetName() == gatewayRegistryName {
		var interference error
		c.once.Do(func() {
			cm := &core.ConfigMap{}
			if interference = c.Client.Get(ctx, client.ObjectKeyFromObject(obj), cm); interference != nil {
				return
			}
			cm.Annotations["test.globalvpc.io/competing-writer"] = "committed"
			interference = c.Client.Update(ctx, cm)
		})
		if interference != nil {
			return interference
		}
	}
	err := c.Client.Update(ctx, obj, opts...)
	if errors.IsConflict(err) {
		c.conflicts++
	}
	return err
}
