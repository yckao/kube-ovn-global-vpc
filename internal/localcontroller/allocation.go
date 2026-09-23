package localcontroller

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"

	api "globalvpc.io/controller/api/v1alpha2"
	"globalvpc.io/controller/internal/localconfig"
	"globalvpc.io/controller/internal/siteconfig"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	allocationLedgerName       = "managed-network-allocations"
	allocationIdentityName     = "managed-network-allocation-identity"
	allocationLabel            = "platform.globalvpc.io/allocator"
	allocationDigestAnnotation = "platform.globalvpc.io/allocation-digest"
	// AllocationReceiptAnnotation is local runtime state. Authority synchronization
	// preserves it, and a new local Binding UID must never adopt an old receipt.
	AllocationReceiptAnnotation = "platform.globalvpc.io/allocation-receipt"
)

// Allocation is retained after deletion. Reusing addresses requires explicit
// node fencing; Kubernetes absence alone does not prove an isolated process stopped.
type Allocation struct {
	TransitCIDR string `json:"transitCIDR"`
	RouterIP    string `json:"routerIP"`
	BFDSourceIP string `json:"bfdSourceIP"`
}
type allocatorState struct {
	ClusterUID  string                `json:"clusterUID"`
	Allocations map[string]Allocation `json:"allocations"`
	BindingUIDs map[string]string     `json:"bindingUIDs"`
}
type allocationReceipt struct {
	ClusterUID string     `json:"clusterUID"`
	SourceUID  string     `json:"sourceUID"`
	BindingUID string     `json:"bindingUID"`
	LedgerUID  string     `json:"ledgerUID"`
	Allocation Allocation `json:"allocation"`
}

func ipAt(p netip.Prefix, index uint32) netip.Addr {
	b := p.Addr().As4()
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], binary.BigEndian.Uint32(b[:])+index)
	return netip.AddrFrom4(out)
}

func allocationDigest(raw string) string {
	digest := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(digest[:])
}
func writeAllocationState(cm *corev1.ConfigMap, state allocatorState) {
	b, _ := json.Marshal(state)
	cm.Data = map[string]string{"state.json": string(b)}
	if cm.Annotations == nil {
		cm.Annotations = map[string]string{}
	}
	cm.Annotations[allocationDigestAnnotation] = allocationDigest(string(b))
}
func readAllocationState(cm *corev1.ConfigMap, cfg localconfig.Config) (allocatorState, error) {
	var state allocatorState
	raw := cm.Data["state.json"]
	if cm.UID == "" || cm.Labels[allocationLabel] != "true" || cm.Annotations[allocationDigestAnnotation] != allocationDigest(raw) || json.Unmarshal([]byte(raw), &state) != nil || state.ClusterUID != cfg.ClusterUID || state.Allocations == nil || state.BindingUIDs == nil || len(state.Allocations) != len(state.BindingUIDs) {
		return state, fmt.Errorf("local allocation ledger is corrupt or belongs to another cluster; fenced recovery required")
	}
	pool, _ := siteconfig.Prefix(cfg.TransitPool)
	bfdPool, _ := siteconfig.Prefix(cfg.BFDSourcePool)
	usedTransit, usedBFD, usedUID := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for owner, a := range state.Allocations {
		p, err := siteconfig.Prefix(a.TransitCIDR)
		_, bfdErr := siteconfig.Host(a.BFDSourceIP, bfdPool)
		uid := state.BindingUIDs[owner]
		if owner == "" || uid == "" || usedUID[uid] || err != nil || bfdErr != nil || p.Bits() != cfg.TransitPrefixLength || !pool.Contains(p.Addr()) || !pool.Contains(siteconfig.LastAddress(p)) || a.RouterIP != p.Addr().Next().String() || usedTransit[a.TransitCIDR] || usedBFD[a.BFDSourceIP] {
			return state, fmt.Errorf("retained local allocation is invalid, duplicated or outside installed pools; fenced recovery required")
		}
		usedTransit[a.TransitCIDR], usedBFD[a.BFDSourceIP], usedUID[uid] = true, true, true
	}
	return state, nil
}

func validateAllocationReceipts(bindings []api.NetworkBinding, ledger *corev1.ConfigMap, state allocatorState) error {
	for i := range bindings {
		b := &bindings[i]
		raw := b.Annotations[AllocationReceiptAnnotation]
		if raw == "" {
			continue
		}
		var receipt allocationReceipt
		owner := b.Annotations[SourceUIDAnnotation]
		a, exists := state.Allocations[owner]
		if json.Unmarshal([]byte(raw), &receipt) != nil || !exists || receipt.ClusterUID != state.ClusterUID || receipt.SourceUID != owner || receipt.BindingUID != string(b.UID) || state.BindingUIDs[owner] != string(b.UID) || receipt.LedgerUID != string(ledger.UID) || receipt.Allocation != a {
			return fmt.Errorf("local allocation receipt does not match retained ledger; fenced recovery required")
		}
	}
	return nil
}

func bootstrapAllocationEvidence(bindings []api.NetworkBinding) bool {
	for _, b := range bindings {
		if b.Annotations[AllocationReceiptAnnotation] != "" || len(b.Status.Resources) > 0 || len(b.Status.Gateways) > 0 || b.Status.AppliedRevision != "" {
			return true
		}
	}
	return false
}

// allocate commits the retained reservation and then a receipt on the local
// Binding before exposing any addresses to native resources or gateway processes.
// Retrying an uncertain write recovers the same reservation; it never guesses
// that a missing or replaced ledger is a fresh installation.
func allocate(ctx context.Context, c client.Client, cfg localconfig.Config, b *api.NetworkBinding) (Allocation, error) {
	if err := cfg.Validate(); err != nil {
		return Allocation{}, err
	}
	owner := b.Annotations[SourceUIDAnnotation]
	if owner == "" || b.UID == "" || b.Namespace != cfg.Namespace {
		return Allocation{}, fmt.Errorf("local allocation requires a persisted binding and source UID")
	}
	var bindings api.NetworkBindingList
	if err := c.List(ctx, &bindings, client.InNamespace(cfg.Namespace)); err != nil {
		return Allocation{}, err
	}
	foundBinding := false
	for _, current := range bindings.Items {
		if current.Name == b.Name && current.UID == b.UID && current.Annotations[SourceUIDAnnotation] == owner {
			foundBinding = true
		}
	}
	if !foundBinding {
		return Allocation{}, fmt.Errorf("local allocation binding identity is missing or changed")
	}
	key := client.ObjectKey{Namespace: cfg.Namespace, Name: allocationLedgerName}
	identityKey := client.ObjectKey{Namespace: cfg.Namespace, Name: allocationIdentityName}
	var identity corev1.ConfigMap
	identityErr := c.Get(ctx, identityKey, &identity)
	if identityErr != nil && !apierrors.IsNotFound(identityErr) {
		return Allocation{}, identityErr
	}
	var cm corev1.ConfigMap
	err := c.Get(ctx, key, &cm)
	if apierrors.IsNotFound(err) {
		if identityErr == nil || bootstrapAllocationEvidence(bindings.Items) {
			return Allocation{}, fmt.Errorf("local allocation ledger is missing beside retained identity or receipts; fenced recovery required")
		}
		state := allocatorState{ClusterUID: cfg.ClusterUID, Allocations: map[string]Allocation{}, BindingUIDs: map[string]string{}}
		cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: map[string]string{allocationLabel: "true"}}}
		writeAllocationState(&cm, state)
		if err = c.Create(ctx, &cm); err != nil {
			return Allocation{}, err
		}
	} else if err != nil {
		return Allocation{}, err
	}
	state, err := readAllocationState(&cm, cfg)
	if err != nil {
		return Allocation{}, err
	}
	if apierrors.IsNotFound(identityErr) {
		// Only an interrupted, still-empty bootstrap may create the anchor. Once
		// reservations exist, disappearance of the anchor is a recovery event.
		if len(state.Allocations) != 0 || bootstrapAllocationEvidence(bindings.Items) {
			return Allocation{}, fmt.Errorf("local allocation identity is missing beside retained state; fenced recovery required")
		}
		immutable := true
		identity = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: identityKey.Name, Namespace: identityKey.Namespace, Labels: map[string]string{allocationLabel: "identity"}}, Immutable: &immutable, Data: map[string]string{"clusterUID": cfg.ClusterUID, "ledgerUID": string(cm.UID)}}
		if err := c.Create(ctx, &identity); err != nil {
			return Allocation{}, err
		}
	}
	if identity.UID == "" || identity.Immutable == nil || !*identity.Immutable || identity.Labels[allocationLabel] != "identity" || identity.Data["clusterUID"] != cfg.ClusterUID || identity.Data["ledgerUID"] != string(cm.UID) {
		return Allocation{}, fmt.Errorf("local allocation identity or ledger UID changed; fenced recovery required")
	}
	if err := validateAllocationReceipts(bindings.Items, &cm, state); err != nil {
		return Allocation{}, err
	}
	// Include the caller as well: a stale request must not ignore its own receipt.
	if err := validateAllocationReceipts([]api.NetworkBinding{*b}, &cm, state); err != nil {
		return Allocation{}, err
	}
	a, accepted := state.Allocations[owner]
	if accepted && state.BindingUIDs[owner] != string(b.UID) {
		return Allocation{}, fmt.Errorf("local binding UID changed for retained allocation; fenced recovery required")
	}
	if !accepted {
		for _, uid := range state.BindingUIDs {
			if uid == string(b.UID) {
				return Allocation{}, fmt.Errorf("local binding source identity changed for retained allocation; fenced recovery required")
			}
		}
		pool, _ := netip.ParsePrefix(cfg.TransitPool)
		bfdPool, _ := netip.ParsePrefix(cfg.BFDSourcePool)
		usedTransit, usedBFD := map[string]bool{}, map[string]bool{}
		for _, previous := range state.Allocations {
			usedTransit[previous.TransitCIDR], usedBFD[previous.BFDSourceIP] = true, true
		}
		for i := uint32(0); i < uint32(1)<<(cfg.TransitPrefixLength-pool.Bits()); i++ {
			p := netip.PrefixFrom(ipAt(pool, i<<(32-cfg.TransitPrefixLength)), cfg.TransitPrefixLength)
			if !usedTransit[p.String()] {
				a.TransitCIDR, a.RouterIP = p.String(), p.Addr().Next().String()
				break
			}
		}
		for i := uint32(1); i < (uint32(1)<<(32-bfdPool.Bits()))-1; i++ {
			ip := ipAt(bfdPool, i).String()
			if !usedBFD[ip] {
				a.BFDSourceIP = ip
				break
			}
		}
		if a.TransitCIDR == "" || a.BFDSourceIP == "" {
			return Allocation{}, fmt.Errorf("reserved local network pool exhausted; retained allocations are not automatically reused")
		}
		state.Allocations[owner], state.BindingUIDs[owner] = a, string(b.UID)
		writeAllocationState(&cm, state)
		if err := c.Update(ctx, &cm); err != nil {
			return Allocation{}, err
		}
	}
	receipt := allocationReceipt{ClusterUID: cfg.ClusterUID, SourceUID: owner, BindingUID: string(b.UID), LedgerUID: string(cm.UID), Allocation: a}
	if b.Annotations[AllocationReceiptAnnotation] == "" {
		raw, _ := json.Marshal(receipt)
		updated := b.DeepCopy()
		updated.Annotations[AllocationReceiptAnnotation] = string(raw)
		if err := c.Update(ctx, updated); err != nil {
			return Allocation{}, err
		}
		*b = *updated
	}
	return a, nil
}
