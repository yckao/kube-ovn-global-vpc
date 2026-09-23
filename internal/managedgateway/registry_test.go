package managedgateway

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	api "globalvpc.io/controller/api/v1alpha2"
	core "k8s.io/api/core/v1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func registryCM(t *testing.T, r *Reconciler, name string) *core.ConfigMap {
	t.Helper()
	cm := new(core.ConfigMap)
	if err := r.Client.Get(context.Background(), types.NamespacedName{Namespace: r.Namespace, Name: name}, cm); err != nil {
		t.Fatal(err)
	}
	return cm
}
func testRemote(b *api.NetworkBinding) {
	b.Spec.RemoteSubnets = []api.RemoteSubnet{{LocationRef: "site-b", CIDR: "10.252.3.0/24"}}
	b.Spec.Peers = []api.PeerGateway{{LocationRef: "site-b", Endpoint: api.GatewayEndpoint{ID: "g-remote", NodeUID: "remote-node", NodeName: "remote-node", EndpointIP: "192.0.2.9", HealthIP: "10.254.3.1", LocalASN: 4200000002, PublicKey: "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="}}}
}
func TestGatewayRegistryDamageBlocksExistingAndUnknownOwners(t *testing.T) {
	for _, damage := range []string{"missing", "replacement", "identity-missing", "checksum", "foreign-cluster", "duplicate-address", "removed-health", "removed-port", "rewritten-link"} {
		t.Run(damage, func(t *testing.T) {
			ctx := context.Background()
			r, b, a := setup(t)
			testRemote(b)
			before := ensure(t, r, b, a)
			cm := registryCM(t, r, gatewayRegistryName)
			reg, err := r.decodeRegistry(cm)
			if err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "missing", "replacement":
				if err := r.Client.Delete(ctx, cm); err != nil {
					t.Fatal(err)
				}
				if damage == "replacement" {
					cm.UID = ""
					cm.ResourceVersion = ""
					if err := r.Client.Create(ctx, cm); err != nil {
						t.Fatal(err)
					}
				}
			case "identity-missing":
				if err := r.Client.Delete(ctx, registryCM(t, r, gatewayRegistryIdentityName)); err != nil {
					t.Fatal(err)
				}
			default:
				first := before.Gateways[0]
				health := string(b.UID) + "/" + first.ID + "/health"
				link := string(b.UID) + "/" + first.ID + "/" + first.Links[0].PeerID
				switch damage {
				case "checksum":
					cm.Data[ledgerData] += " "
				case "foreign-cluster":
					reg.ClusterUID = "other-cluster"
				case "duplicate-address":
					reg.Addresses["unrelated-retained-owner"] = reg.Addresses[health]
				case "removed-health":
					delete(reg.Addresses, health)
				case "removed-port":
					delete(reg.Ports, first.NodeUID+"/"+link)
				case "rewritten-link":
					reg.Addresses[link] = "10.254.1.250"
				}
				if damage != "checksum" {
					storeRegistry(cm, reg)
				}
				if err := r.Client.Update(ctx, cm); err != nil {
					t.Fatal(err)
				}
			}
			beforeLedger := registryCM(t, r, ledgerName(b))
			// Existing links used to skip allocate(), hiding a lost registry. Ensure
			// must validate all recorded claims before it can reuse those members.
			if _, err := r.Ensure(ctx, b, a); err == nil {
				t.Fatal("existing member ignored registry damage")
			}
			if !reflect.DeepEqual(beforeLedger, registryCM(t, r, ledgerName(b))) {
				t.Fatal("blocked Ensure changed accepted member identities")
			}
			unknown := b.DeepCopy()
			unknown.Name = "new-binding"
			unknown.UID = "new-binding-uid"
			unknown.Spec.VPCRef.UID = "another-tenant"
			if _, err := r.Ensure(ctx, unknown, a); err == nil {
				t.Fatal("new owner bypassed damaged registry")
			}
			if value, err := r.address(ctx, "future-unknown-owner/health"); err == nil || value != "" {
				t.Fatalf("unknown allocation escaped damage guard: %q %v", value, err)
			}
			if damage == "missing" {
				var current core.ConfigMap
				if err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: gatewayRegistryName}, &current); !errors.IsNotFound(err) {
					t.Fatal("damaged registry was recreated")
				}
			}
		})
	}
}

type lostRegistryResponse struct {
	client.Client
	operation, target string
	fired             bool
}

func (c *lostRegistryResponse) Create(ctx context.Context, o client.Object, opts ...client.CreateOption) error {
	if err := c.Client.Create(ctx, o, opts...); err != nil {
		return err
	}
	if !c.fired && c.operation == "create" && o.GetName() == c.target {
		c.fired = true
		return fmt.Errorf("simulated committed create response loss")
	}
	return nil
}
func (c *lostRegistryResponse) Update(ctx context.Context, o client.Object, opts ...client.UpdateOption) error {
	if err := c.Client.Update(ctx, o, opts...); err != nil {
		return err
	}
	if !c.fired && c.operation == "update" && o.GetName() == c.target {
		c.fired = true
		return fmt.Errorf("simulated committed update response loss")
	}
	return nil
}
func TestGatewayRegistryRecoversLostWriteResponses(t *testing.T) {
	for _, stage := range []string{"registry-create", "anchor-create", "address-update", "port-update"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			r, _, _ := setup(t)
			const claim = "binding/member/remote/member"
			if stage == "port-update" {
				if _, err := r.address(ctx, claim); err != nil {
					t.Fatal(err)
				}
			}
			c := &lostRegistryResponse{Client: r.Client}
			switch stage {
			case "registry-create":
				c.operation, c.target = "create", gatewayRegistryName
			case "anchor-create":
				c.operation, c.target = "create", gatewayRegistryIdentityName
			default:
				c.operation, c.target = "update", gatewayRegistryName
			}
			r.Client = c
			port := stage == "port-update"
			if value, err := r.allocate(ctx, claim, port, "node-1"); err == nil || value != "" || !c.fired {
				t.Fatalf("uncertain allocation escaped: %q %v", value, err)
			}
			restarted := *r
			restarted.Client = c.Client
			value, err := restarted.allocate(ctx, claim, port, "node-1")
			if err != nil {
				t.Fatal(err)
			}
			want := "10.254.1.1"
			if port {
				want = "32000"
			}
			if value != want {
				t.Fatalf("retry changed allocation: %s", value)
			}
			cm := registryCM(t, r, gatewayRegistryName)
			reg, err := r.decodeRegistry(cm)
			if err != nil {
				t.Fatal(err)
			}
			if len(reg.Addresses) != 1 || (port && len(reg.Ports) != 1) {
				t.Fatal("uncertain write consumed another reservation")
			}
			identity := registryCM(t, r, gatewayRegistryIdentityName)
			if identity.Immutable == nil || !*identity.Immutable || identity.Data["registryUID"] != string(cm.UID) {
				t.Fatal("allocation exposed without durable immutable identity")
			}
		})
	}
}

func TestGatewayRegistryStatusReceiptAndUnrelatedPortConflict(t *testing.T) {
	for _, damage := range []string{"status-rewrite", "unrelated-duplicate-port", "network-address", "broadcast-address"} {
		t.Run(damage, func(t *testing.T) {
			ctx := context.Background()
			r, b, a := setup(t)
			testRemote(b)
			observed := ensure(t, r, b, a)
			cm := registryCM(t, r, gatewayRegistryName)
			reg, err := r.decodeRegistry(cm)
			if err != nil {
				t.Fatal(err)
			}
			first := observed.Gateways[0]
			switch damage {
			case "status-rewrite":
				b.Status.Gateways = observed.Gateways
				b.Status.Gateways[0].HealthIP = "10.254.1.250"
			case "unrelated-duplicate-port":
				reg.Addresses["another-owner/link"] = "10.254.1.250"
				reg.Ports[first.NodeUID+"/another-owner/link"] = int(first.Links[0].ListenPort)
			case "network-address":
				reg.Addresses["another-owner/link"] = "10.254.1.0"
			case "broadcast-address":
				reg.Addresses["another-owner/link"] = "10.254.1.255"
			}
			if damage != "status-rewrite" {
				storeRegistry(cm, reg)
				if err := r.Client.Update(ctx, cm); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := r.Ensure(ctx, b, a); err == nil {
				t.Fatal("damaged status/retained allocation was accepted")
			}
		})
	}
}
