// Package destinationroute validates and canonicalizes native destination-route intent.
// This package is copied unchanged into the pinned Kube-OVN integration patch.
package destinationroute

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
)

const Capability = "destination-routes.v1"
const OwnerKey = "kube-ovn.io/destination-vpc-uid"
const RouteKey = "kube-ovn.io/destination-route"

type BFD struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3600000
	MinRX int `json:"minRX"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3600000
	MinTX int `json:"minTX"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=255
	Multiplier int `json:"multiplier"`
}
type Intent struct {
	// +kubebuilder:validation:MaxLength=18
	CIDR string `json:"cidr"`
	// +kubebuilder:validation:MinItems=2
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=15
	// +listType=set
	NextHops []string `json:"nextHops"`
	BFD      BFD      `json:"bfd"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=7
	// +kubebuilder:validation:items:Enum=eth_src;eth_dst;ip_src;ip_dst;ip_proto;tp_src;tp_dst
	// +listType=set
	SelectionFields []string `json:"selectionFields"`
}

// DeepCopyInto also permits native Kubernetes code generation for the imported
// API intent type without sharing backing arrays between informer snapshots.
func (in *Intent) DeepCopyInto(out *Intent) {
	*out = *in
	if in.NextHops != nil {
		out.NextHops = append([]string{}, in.NextHops...)
	}
	if in.SelectionFields != nil {
		out.SelectionFields = append([]string{}, in.SelectionFields...)
	}
}
func (in *Intent) DeepCopy() *Intent {
	if in == nil {
		return nil
	}
	out := new(Intent)
	in.DeepCopyInto(out)
	return out
}

type Plan struct {
	Routes   []Intent
	Sessions map[string]BFD
	Hash     string
}

// Compile's hash is SHA256 of JSON for the normalized non-null route array.
// Routes sort by CIDR, next hops and fields sort lexically; object field order is
// cidr,nextHops,bfd,selectionFields; BFD field order is minRX,minTX,multiplier.
func Compile(input []Intent) (Plan, error) {
	p := Plan{Routes: make([]Intent, 0, len(input)), Sessions: map[string]BFD{}}
	if len(input) > 256 {
		return p, fmt.Errorf("at most 256 destination prefixes are supported")
	}
	prefixes := []netip.Prefix{}
	allowed := map[string]bool{"eth_src": true, "eth_dst": true, "ip_proto": true, "ip_src": true, "ip_dst": true, "tp_src": true, "tp_dst": true}
	for _, r := range input {
		prefix, err := netip.ParsePrefix(r.CIDR)
		if err != nil || !prefix.Addr().Is4() || prefix.Bits() == 0 || prefix.Bits() == 32 || prefix.Masked().String() != r.CIDR || !prefix.Addr().IsGlobalUnicast() {
			return p, fmt.Errorf("destination must be a canonical non-default unicast IPv4 prefix with length 1-31")
		}
		for _, other := range prefixes {
			if prefix.Overlaps(other) {
				return p, fmt.Errorf("destination prefixes overlap")
			}
		}
		prefixes = append(prefixes, prefix)
		if len(r.NextHops) < 2 || len(r.NextHops) > 64 {
			return p, fmt.Errorf("destination requires 2-64 gateway next hops")
		}
		if r.BFD.MinRX < 1 || r.BFD.MinRX > 3600000 || r.BFD.MinTX < 1 || r.BFD.MinTX > 3600000 || r.BFD.Multiplier < 1 || r.BFD.Multiplier > 255 {
			return p, fmt.Errorf("invalid BFD timers")
		}
		hops := map[string]bool{}
		for _, hop := range r.NextHops {
			a, err := netip.ParseAddr(hop)
			if err != nil || !a.Is4() || !a.IsGlobalUnicast() || a.String() != hop || prefix.Contains(a) || hops[hop] {
				return p, fmt.Errorf("invalid, duplicated, or destination-overlapping next hop")
			}
			hops[hop] = true
			if timers, ok := p.Sessions[hop]; ok && timers != r.BFD {
				return p, fmt.Errorf("shared next hop has conflicting BFD timers")
			}
			p.Sessions[hop] = r.BFD
		}
		if len(r.SelectionFields) == 0 || len(r.SelectionFields) > 7 {
			return p, fmt.Errorf("explicit ECMP selection fields are required")
		}
		fields := map[string]bool{}
		for _, field := range r.SelectionFields {
			if !allowed[field] || fields[field] {
				return p, fmt.Errorf("invalid or duplicate ECMP selection field")
			}
			fields[field] = true
		}
		r.NextHops = append([]string(nil), r.NextHops...)
		sort.Strings(r.NextHops)
		r.SelectionFields = append([]string(nil), r.SelectionFields...)
		sort.Strings(r.SelectionFields)
		p.Routes = append(p.Routes, r)
	}
	sort.Slice(p.Routes, func(i, j int) bool { return p.Routes[i].CIDR < p.Routes[j].CIDR })
	data, err := json.Marshal(p.Routes)
	if err != nil {
		return p, err
	}
	sum := sha256.Sum256(data)
	p.Hash = hex.EncodeToString(sum[:])
	return p, nil
}

// ReachableGateway ensures a next hop is on an ordinary connected router port,
// rather than relying on a default/static route or the dedicated BFD-only port.
func ReachableGateway(hop string, networks []string) bool {
	a, err := netip.ParseAddr(hop)
	if err != nil {
		return false
	}
	for _, raw := range networks {
		p, err := netip.ParsePrefix(raw)
		if err == nil && p.Contains(a) && a != p.Addr() && a != p.Masked().Addr() {
			// IPv4 subnet broadcast is not a gateway address (except RFC3021 /31).
			if p.Addr().Is4() && p.Bits() < 31 {
				b := p.Masked().Addr().As4()
				n := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
				n |= uint32(1)<<(32-p.Bits()) - 1
				if a == netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}) {
					continue
				}
			}
			return true
		}
	}
	return false
}

// Children makes more-specific ECMP routes outrank the parent discard guard.
// When all BFD sessions are down, only the requested destination is discarded.
func Children(cidr string) [2]string {
	p := netip.MustParsePrefix(cidr)
	b := p.Addr().As4()
	n := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	n |= uint32(1) << (31 - p.Bits())
	second := netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
	return [2]string{netip.PrefixFrom(p.Addr(), p.Bits()+1).String(), netip.PrefixFrom(second, p.Bits()+1).String()}
}
