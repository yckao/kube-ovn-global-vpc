package platformcontroller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	api "globalvpc.io/controller/api/v1alpha2"
	"globalvpc.io/controller/internal/platformplan"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// IDs share one monotonic sequence. Claims are sharded and retained, so a VPC
// or native tunnel identifier is never reused after deletion. Reservation may
// burn IDs after an uncertain write; it never guesses that they are available.
const (
	networkIDAllocatorLabel = "platform.globalvpc.io/allocator"
	networkIDSequenceName   = "network-id-sequence"
	networkIDAnchorName     = "network-id-sequence-anchor"
	networkIDClaimPrefix    = "network-id-claims-"
	maxNetworkID            = uint64(16777215)
)

func networkIDShard(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])[:2]
}

func (r *Reconciler) networkIDs(ctx context.Context, keys []string) (map[string]uint32, error) {
	ns := r.Config.RegistryNamespace
	if ns == "" {
		ns = "global-vpc-system"
	}
	groups := map[string][]string{}
	unique := map[string]bool{}
	for _, key := range keys {
		if !unique[key] {
			unique[key] = true
			shard := networkIDShard(key)
			groups[shard] = append(groups[shard], key)
		}
	}
	shards := []string{}
	for shard := range groups {
		shards = append(shards, shard)
	}
	sort.Strings(shards)

	// Every retained claim participates in the integrity check, including keys
	// unrelated to this reconcile. A damaged shard cannot silently free an ID.
	var all corev1.ConfigMapList
	if err := r.List(ctx, &all, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	objects := map[string]*corev1.ConfigMap{}
	claims := map[string]map[string]uint32{}
	used := map[uint32]bool{}
	var highest uint64
	for i := range all.Items {
		cm := &all.Items[i]
		if !strings.HasPrefix(cm.Name, networkIDClaimPrefix) && cm.Labels[networkIDAllocatorLabel] != "network-id-claims" {
			continue
		}
		shard := strings.TrimPrefix(cm.Name, networkIDClaimPrefix)
		var ids map[string]uint32
		if cm.Labels[networkIDAllocatorLabel] != "network-id-claims" || len(shard) != 2 || json.Unmarshal([]byte(cm.Data["claims.json"]), &ids) != nil || ids == nil {
			return nil, fmt.Errorf("network ID claim shard is invalid or foreign")
		}
		if _, err := hex.DecodeString(shard); err != nil {
			return nil, fmt.Errorf("network ID claim shard name is invalid")
		}
		for key, id := range ids {
			if key == "" || networkIDShard(key) != shard || id == 0 || uint64(id) > maxNetworkID {
				return nil, fmt.Errorf("invalid retained network ID claim")
			}
			if used[id] {
				return nil, fmt.Errorf("duplicate retained network ID")
			}
			used[id] = true
			if uint64(id) > highest {
				highest = uint64(id)
			}
		}
		objects[shard], claims[shard] = cm, ids
	}

	var anchor corev1.ConfigMap
	anchorKey := client.ObjectKey{Namespace: ns, Name: networkIDAnchorName}
	err := r.Get(ctx, anchorKey, &anchor)
	anchorMissing := apierrors.IsNotFound(err)
	if err != nil && !anchorMissing {
		return nil, err
	}
	if !anchorMissing && (anchor.Labels[networkIDAllocatorLabel] != networkIDAnchorName || anchor.Immutable == nil || !*anchor.Immutable || anchor.Data["sequenceUID"] == "") {
		return nil, fmt.Errorf("network ID sequence identity anchor is invalid or foreign")
	}

	// Read the sequence after the claim scan: another allocator may reserve and
	// publish claims during that scan. Reading it first would falsely diagnose
	// those newer claims as a rollback. The later CAS still fences concurrent
	// reservations and sequence replacement before this call's own mutation.
	var seq corev1.ConfigMap
	seqKey := client.ObjectKey{Namespace: ns, Name: networkIDSequenceName}
	err = r.Get(ctx, seqKey, &seq)
	if apierrors.IsNotFound(err) {
		if !anchorMissing || highest != 0 {
			return nil, fmt.Errorf("network ID sequence missing beside retained identity or claims")
		}
		seq = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: seqKey.Name, Labels: map[string]string{networkIDAllocatorLabel: networkIDSequenceName}}, Data: map[string]string{"next": "1"}}
		if err = r.Create(ctx, &seq); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	next, err := strconv.ParseUint(seq.Data["next"], 10, 32)
	if err != nil || seq.UID == "" || seq.Labels[networkIDAllocatorLabel] != networkIDSequenceName || next < 1 || next > maxNetworkID+1 {
		return nil, fmt.Errorf("network ID sequence invalid")
	}
	if next <= highest {
		return nil, fmt.Errorf("network ID sequence regressed behind retained claims")
	}
	if anchorMissing {
		// Only an unused initial sequence can acquire its identity anchor. This
		// also recovers a lost initial Create response without trusting an older
		// registry that has already issued IDs but lost its anchor.
		if next != 1 || highest != 0 {
			return nil, fmt.Errorf("network ID sequence identity anchor missing for used registry")
		}
		immutable := true
		anchor = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: anchorKey.Name, Labels: map[string]string{networkIDAllocatorLabel: networkIDAnchorName}}, Immutable: &immutable, Data: map[string]string{"sequenceUID": string(seq.UID)}}
		if err = r.Create(ctx, &anchor); err != nil {
			return nil, err
		}
	} else if anchor.Data["sequenceUID"] != string(seq.UID) {
		return nil, fmt.Errorf("network ID sequence identity changed")
	}

	result := map[string]uint32{}
	missing := []string{}
	for _, shard := range shards {
		for _, key := range groups[shard] {
			if id, ok := claims[shard][key]; ok {
				result[key] = id
			} else {
				missing = append(missing, key)
			}
		}
	}
	if len(missing) == 0 {
		return result, nil
	}
	if next+uint64(len(missing)) > maxNetworkID+1 {
		return nil, fmt.Errorf("network ID sequence exhausted")
	}
	for _, shard := range shards {
		if objects[shard] != nil {
			continue
		}
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: networkIDClaimPrefix + shard, Labels: map[string]string{networkIDAllocatorLabel: "network-id-claims"}}, Data: map[string]string{"claims.json": "{}"}}
		if err = r.Create(ctx, cm); err != nil {
			return nil, err
		}
		objects[shard], claims[shard] = cm, map[string]uint32{}
	}
	seq.Data["next"] = strconv.FormatUint(next+uint64(len(missing)), 10)
	if err = r.Update(ctx, &seq); err != nil {
		return nil, err
	}
	sort.Strings(missing)
	for i, key := range missing {
		result[key] = uint32(next) + uint32(i)
	}
	for _, shard := range shards {
		changed := false
		for _, key := range groups[shard] {
			if _, ok := claims[shard][key]; !ok {
				claims[shard][key] = result[key]
				changed = true
			}
		}
		if !changed {
			continue
		}
		b, _ := json.Marshal(claims[shard])
		if len(b) > 900000 {
			return nil, fmt.Errorf("network ID claim shard capacity exceeded")
		}
		objects[shard].Data["claims.json"] = string(b)
		if err = r.Update(ctx, objects[shard]); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (r *Reconciler) assignTransport(ctx context.Context, v *api.VPC, desired []api.NetworkBinding, existing []api.NetworkBinding) error {
	keys := []string{}
	for i := range desired {
		b := &desired[i]
		b.Spec.NetworkID = v.Status.NetworkID
		keys = append(keys, "asn/"+b.Spec.LocationRef)
		for _, local := range existing {
			if local.Spec.LocationRef != b.Spec.LocationRef {
				continue
			}
			for _, a := range local.Status.Gateways {
				for _, p := range b.Spec.Peers {
					left, right := b.Spec.LocationRef+"/"+a.ID, p.LocationRef+"/"+p.Endpoint.ID
					if right < left {
						left, right = right, left
					}
					if b.Spec.TransportProfile != "wireguard-bgp" {
						b.Spec.LinkAllocations = append(b.Spec.LinkAllocations, api.LinkAllocation{PeerA: left, PeerB: right})
						keys = append(keys, "link/"+string(v.UID)+"/"+left+"/"+right)
					}
				}
			}
		}
	}
	ids, err := r.networkIDs(ctx, keys)
	if err != nil {
		return err
	}
	for i := range desired {
		b := &desired[i]
		b.Spec.LocalASN = 4200000000 + ids["asn/"+b.Spec.LocationRef]
		for j := range b.Spec.LinkAllocations {
			link := &b.Spec.LinkAllocations[j]
			link.ControlVNI = ids["link/"+string(v.UID)+"/"+link.PeerA+"/"+link.PeerB]
		}
		sort.Slice(b.Spec.LinkAllocations, func(i, j int) bool {
			return b.Spec.LinkAllocations[i].PeerA+"/"+b.Spec.LinkAllocations[i].PeerB < b.Spec.LinkAllocations[j].PeerA+"/"+b.Spec.LinkAllocations[j].PeerB
		})
		b.Spec.Revision = platformplan.Revision(b.Spec)
	}
	return nil
}
