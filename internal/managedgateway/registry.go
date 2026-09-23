package managedgateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	api "globalvpc.io/controller/api/v1alpha2"
	"globalvpc.io/controller/internal/siteconfig"
	core "k8s.io/api/core/v1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const gatewayRegistryName = "managed-gateway-allocations"
const gatewayRegistryIdentityName = "managed-gateway-allocation-identity"
const gatewayRegistryDigest = "platform.globalvpc.io/gateway-allocation-digest"

func registryDigest(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
func storeRegistry(cm *core.ConfigMap, reg allocationRegistry) {
	body, _ := json.Marshal(reg)
	cm.Data = map[string]string{ledgerData: string(body)}
	if cm.Annotations == nil {
		cm.Annotations = map[string]string{}
	}
	cm.Annotations[gatewayRegistryDigest] = registryDigest(string(body))
}
func (r *Reconciler) decodeRegistry(cm *core.ConfigMap) (allocationRegistry, error) {
	var reg allocationRegistry
	raw := cm.Data[ledgerData]
	if cm.UID == "" || cm.Labels[componentLabel] != "gateway-allocator" || cm.Annotations[gatewayRegistryDigest] != registryDigest(raw) || json.Unmarshal([]byte(raw), &reg) != nil || reg.ClusterUID != r.ClusterUID || reg.Addresses == nil || reg.Ports == nil {
		return reg, fmt.Errorf("GatewayAllocationRegistryInvalid: corrupt or foreign registry requires fenced recovery")
	}
	pool, err := siteconfig.Prefix(r.AllocationPool)
	if err != nil || r.PortStart < 1024 || r.PortEnd > 65535 || r.PortEnd < r.PortStart {
		return reg, fmt.Errorf("invalid gateway allocation pools")
	}
	addresses, ports := map[string]bool{}, map[string]bool{}
	for claim, rawIP := range reg.Addresses {
		if _, err := siteconfig.Host(rawIP, pool); claim == "" || err != nil || addresses[rawIP] {
			return reg, fmt.Errorf("GatewayAllocationRegistryInvalid: duplicate or invalid retained address")
		}
		addresses[rawIP] = true
	}
	for claim, port := range reg.Ports {
		node, key, ok := strings.Cut(claim, "/")
		identity := node + "/" + strconv.Itoa(port)
		if !ok || node == "" || key == "" || port < r.PortStart || port > r.PortEnd || ports[identity] || reg.Addresses[key] == "" {
			return reg, fmt.Errorf("GatewayAllocationRegistryInvalid: duplicate, orphaned or invalid retained port")
		}
		ports[identity] = true
	}
	return reg, nil
}
func validateGatewayClaims(reg allocationRegistry, owner, profile string, gateways []api.GatewayEndpoint) error {
	for _, g := range gateways {
		prefix := owner + "/" + g.ID
		if owner == "" || g.ID == "" || g.NodeUID == "" || reg.Addresses[prefix+"/health"] != g.HealthIP || g.HealthIP == "" {
			return fmt.Errorf("GatewayAllocationReceiptMismatch: member health identity disagrees with retained registry")
		}
		for _, link := range g.Links {
			key := prefix + "/" + link.PeerID
			if link.PeerID == "" || link.TunnelIP == "" || reg.Addresses[key] != link.TunnelIP {
				return fmt.Errorf("GatewayAllocationReceiptMismatch: member control address disagrees with retained registry")
			}
			if profile == "wireguard-bgp" && (link.ListenPort == 0 || reg.Ports[g.NodeUID+"/"+key] != int(link.ListenPort)) {
				return fmt.Errorf("GatewayAllocationReceiptMismatch: member port disagrees with retained registry")
			}
		}
	}
	return nil
}

func gatewayStatusEvidence(b *api.NetworkBinding, namespace string) bool {
	if len(b.Status.Gateways) > 0 {
		return true
	}
	for _, resource := range b.Status.Resources {
		if resource.Namespace == namespace && strings.HasPrefix(resource.Name, "pg-") && (resource.Kind == "ConfigMap" || resource.Kind == "Secret" || resource.Kind == "Pod") {
			return true
		}
	}
	return false
}

// registry validates the retained identity and every published member claim,
// including claims belonging to a different Binding. Only allocate may request
// bootstrap; Ensure can validate before touching resources without burning
// reservations when there are too few eligible nodes.
func (r *Reconciler) registry(ctx context.Context, bootstrap bool, extra ...*api.NetworkBinding) (*core.ConfigMap, allocationRegistry, error) {
	var cms core.ConfigMapList
	if err := r.Client.List(ctx, &cms, client.InNamespace(r.Namespace)); err != nil {
		return nil, allocationRegistry{}, err
	}
	var bindings api.NetworkBindingList
	if err := r.Client.List(ctx, &bindings, client.InNamespace(r.Namespace)); err != nil {
		return nil, allocationRegistry{}, err
	}
	evidence := false
	ledgers := []ledger{}
	for _, cm := range cms.Items {
		if cm.Name == gatewayRegistryName || cm.Name == gatewayRegistryIdentityName {
			continue
		}
		if cm.Labels[componentLabel] != "gateway" && !strings.HasPrefix(cm.Name, "pg-") {
			continue
		}
		evidence = true
		raw, memberLedger := cm.Data[ledgerData]
		if !memberLedger {
			continue
		}
		var l ledger
		if json.Unmarshal([]byte(raw), &l) != nil || l.OwnerUID == "" || cm.Name != "pg-"+Short(l.OwnerUID) || cm.Labels[api.OwnerLabel] != l.OwnerUID || cm.Labels[componentLabel] != "gateway" {
			return nil, allocationRegistry{}, fmt.Errorf("GatewayAllocationReceiptMismatch: invalid member ledger")
		}
		ownerFound := false
		for _, ref := range cm.OwnerReferences {
			if string(ref.UID) == l.OwnerUID && ref.Kind == "NetworkBinding" && ref.APIVersion == api.GroupVersion.String() && ref.Controller != nil && *ref.Controller {
				ownerFound = true
			}
		}
		if !ownerFound {
			return nil, allocationRegistry{}, fmt.Errorf("GatewayAllocationReceiptMismatch: member ledger ownership changed")
		}
		ledgers = append(ledgers, l)
	}
	for _, b := range bindings.Items {
		if gatewayStatusEvidence(&b, r.Namespace) {
			evidence = true
		}
	}
	for _, b := range extra {
		if gatewayStatusEvidence(b, r.Namespace) {
			evidence = true
		}
	}
	// Resources may survive a lost member-ledger write or externally deleted CM.
	var pods core.PodList
	if err := r.Client.List(ctx, &pods, client.InNamespace(r.Namespace)); err != nil {
		return nil, allocationRegistry{}, err
	}
	for _, p := range pods.Items {
		if p.Labels[componentLabel] == "gateway" || strings.HasPrefix(p.Name, "pg-") {
			evidence = true
		}
	}
	var keys core.SecretList
	if err := r.Client.List(ctx, &keys, client.InNamespace(r.Namespace), client.MatchingLabels{componentLabel: "gateway"}); err != nil {
		return nil, allocationRegistry{}, err
	}
	if len(keys.Items) > 0 {
		evidence = true
	}
	identityKey := types.NamespacedName{Namespace: r.Namespace, Name: gatewayRegistryIdentityName}
	var identity core.ConfigMap
	identityErr := r.Client.Get(ctx, identityKey, &identity)
	if identityErr != nil && !errors.IsNotFound(identityErr) {
		return nil, allocationRegistry{}, identityErr
	}
	key := types.NamespacedName{Namespace: r.Namespace, Name: gatewayRegistryName}
	cm := &core.ConfigMap{}
	err := r.Client.Get(ctx, key, cm)
	if errors.IsNotFound(err) {
		if identityErr == nil || evidence {
			return nil, allocationRegistry{}, fmt.Errorf("GatewayAllocationRegistryLost: retained identity or member receipts require fenced recovery")
		}
		if !bootstrap {
			return nil, allocationRegistry{}, nil
		}
		*cm = core.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: map[string]string{componentLabel: "gateway-allocator"}}}
		storeRegistry(cm, allocationRegistry{ClusterUID: r.ClusterUID, Addresses: map[string]string{}, Ports: map[string]int{}})
		if err = r.Client.Create(ctx, cm); errors.IsAlreadyExists(err) {
			return nil, allocationRegistry{}, errors.NewConflict(core.Resource("configmaps"), key.Name, fmt.Errorf("allocator bootstrap raced"))
		} else if err != nil {
			return nil, allocationRegistry{}, err
		}
	} else if err != nil {
		return nil, allocationRegistry{}, err
	}
	reg, err := r.decodeRegistry(cm)
	if err != nil {
		return nil, allocationRegistry{}, err
	}
	if errors.IsNotFound(identityErr) {
		if len(reg.Addresses) > 0 || len(reg.Ports) > 0 || evidence {
			return nil, allocationRegistry{}, fmt.Errorf("GatewayAllocationIdentityLost: retained reservations require fenced recovery")
		}
		identity = core.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: identityKey.Name, Namespace: identityKey.Namespace, Labels: map[string]string{componentLabel: "gateway-allocator-identity"}}, Immutable: ptr(true), Data: map[string]string{"clusterUID": r.ClusterUID, "registryUID": string(cm.UID)}}
		if err = r.Client.Create(ctx, &identity); errors.IsAlreadyExists(err) {
			return nil, allocationRegistry{}, errors.NewConflict(core.Resource("configmaps"), identityKey.Name, fmt.Errorf("allocator identity bootstrap raced"))
		} else if err != nil {
			return nil, allocationRegistry{}, err
		}
	}
	if identity.UID == "" || identity.Immutable == nil || !*identity.Immutable || identity.Labels[componentLabel] != "gateway-allocator-identity" || identity.Data["clusterUID"] != r.ClusterUID || identity.Data["registryUID"] != string(cm.UID) {
		return nil, allocationRegistry{}, fmt.Errorf("GatewayAllocationIdentityChanged: registry UID changed; fenced recovery required")
	}
	for _, l := range ledgers {
		gateways := make([]api.GatewayEndpoint, 0, len(l.Members))
		for _, m := range l.Members {
			gateways = append(gateways, m.Endpoint)
		}
		if err := validateGatewayClaims(reg, l.OwnerUID, l.Profile, gateways); err != nil {
			return nil, allocationRegistry{}, err
		}
	}
	for _, b := range bindings.Items {
		if err := validateGatewayClaims(reg, string(b.UID), b.Spec.TransportProfile, b.Status.Gateways); err != nil {
			return nil, allocationRegistry{}, err
		}
	}
	for _, b := range extra {
		if err := validateGatewayClaims(reg, string(b.UID), b.Spec.TransportProfile, b.Status.Gateways); err != nil {
			return nil, allocationRegistry{}, err
		}
	}
	return cm, reg, nil
}

func (r *Reconciler) allocate(ctx context.Context, key string, port bool, node string) (string, error) {
	if key == "" || (port && (node == "" || strings.Contains(node, "/"))) {
		return "", fmt.Errorf("gateway allocation identity is empty or invalid")
	}
	var result string
	err := retry.RetryOnConflict(wait.Backoff{Duration: 10 * time.Millisecond, Factor: 1.5, Jitter: 0.1, Steps: 10}, func() error {
		result = ""
		cm, reg, err := r.registry(ctx, true)
		if err != nil {
			return err
		}
		if port {
			if reg.Addresses[key] == "" {
				return fmt.Errorf("gateway port needs a retained link address")
			}
			claim := node + "/" + key
			if v, ok := reg.Ports[claim]; ok {
				result = strconv.Itoa(v)
				return nil
			}
			used := map[int]bool{}
			for k, v := range reg.Ports {
				if strings.HasPrefix(k, node+"/") {
					used[v] = true
				}
			}
			for v := r.PortStart; v <= r.PortEnd; v++ {
				if !used[v] {
					reg.Ports[claim] = v
					result = strconv.Itoa(v)
					break
				}
			}
		} else {
			if v, ok := reg.Addresses[key]; ok {
				result = v
				return nil
			}
			used := map[string]bool{}
			for _, v := range reg.Addresses {
				used[v] = true
			}
			pool, _ := netip.ParsePrefix(r.AllocationPool)
			for ip := pool.Addr().Next(); ip.IsValid() && pool.Contains(ip.Next()); ip = ip.Next() {
				if !used[ip.String()] {
					result = ip.String()
					reg.Addresses[key] = result
					break
				}
			}
		}
		if result == "" {
			return fmt.Errorf("gateway allocation pool exhausted")
		}
		storeRegistry(cm, reg)
		return r.Client.Update(ctx, cm)
	})
	if err != nil {
		return "", err
	}
	return result, nil
}
