// Package plan validates immutable tenant intent and renders its owned resources.
// Planning is side-effect free: address reservation must succeed before applying it.
package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"globalvpc.io/controller/api/v1alpha1"
	"globalvpc.io/controller/internal/config"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
)

type Plan struct {
	UID           string
	Hash          string
	TransitSwitch string
	Claims        []v1alpha1.PrefixClaim
	Attachments   []Site
}

type Site struct {
	Cluster     config.Cluster
	Intent      v1alpha1.Attachment
	VpcName     string
	SubnetName  string
	TransitName string
	Vpc         *unstructured.Unstructured
	Subnet      *unstructured.Unstructured
	Transit     *unstructured.Unstructured
}

// Build rejects the whole request before returning any actionable plan. This
// first version requires all OVN deployments in the domain to participate: native
// ovn-ic replicates switches domain-wide, including to unattached deployments.
func Build(vpc *v1alpha1.GlobalVpc, cfg config.Config) (*Plan, error) {
	if vpc == nil {
		return nil, fmt.Errorf("GlobalVpc is required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid domain registration: %w", err)
	}
	uid := string(vpc.UID)
	if uid == "" || len(validation.IsValidLabelValue(uid)) != 0 {
		return nil, fmt.Errorf("GlobalVpc must have a valid server-assigned UID")
	}
	if vpc.Spec.DomainRef != cfg.Domain {
		return nil, fmt.Errorf("domainRef does not match this controller's registered domain")
	}
	providerCommand, ok := cfg.Providers[vpc.Spec.IPAM.ProviderRef]
	if !ok {
		return nil, fmt.Errorf("ipam.providerRef is not registered")
	}
	if strings.TrimSpace(vpc.Spec.IPAM.ScopeRef) == "" || strings.TrimSpace(vpc.Spec.IPAM.ScopeRef) != vpc.Spec.IPAM.ScopeRef || len(vpc.Spec.IPAM.ScopeRef) > 253 {
		return nil, fmt.Errorf("ipam.scopeRef must be a nonempty reference of at most 253 characters without surrounding whitespace")
	}
	if len(vpc.Spec.Attachments) != len(cfg.Clusters) {
		return nil, fmt.Errorf("attachments must include every cluster registered in the domain; sparse membership is unsupported")
	}
	transit, err := prefix(vpc.Spec.TransitCIDR)
	if err != nil {
		return nil, fmt.Errorf("transitCIDR: %w", err)
	}

	// Copy every mutable slice before sorting so Build never changes its inputs.
	clusters := make(map[string]config.Cluster, len(cfg.Clusters))
	registered := make([]config.Cluster, 0, len(cfg.Clusters))
	for _, cluster := range cfg.Clusters {
		if len(validation.IsDNS1123Subdomain(cluster.Name)) != 0 || len(validation.IsValidLabelValue(cluster.UID)) != 0 {
			return nil, fmt.Errorf("cluster registration has an invalid name or UID label")
		}
		cluster.GatewayChassis = append([]string(nil), cluster.GatewayChassis...)
		cluster.NBCommand = append([]string(nil), cluster.NBCommand...)
		cluster.SBCommand = append([]string(nil), cluster.SBCommand...)
		sort.Strings(cluster.GatewayChassis)
		for i, chassis := range cluster.GatewayChassis {
			if strings.TrimSpace(chassis) == "" || (i > 0 && chassis == cluster.GatewayChassis[i-1]) {
				return nil, fmt.Errorf("cluster %q has an empty or duplicate gateway chassis", cluster.Name)
			}
		}
		clusters[cluster.Name] = cluster
		registered = append(registered, cluster)
	}
	sort.Slice(registered, func(i, j int) bool { return registered[i].Name < registered[j].Name })
	intents := append([]v1alpha1.Attachment(nil), vpc.Spec.Attachments...)
	sort.Slice(intents, func(i, j int) bool { return intents[i].ClusterRef < intents[j].ClusterRef })
	seenClusters, seenTransitIPs := map[string]bool{}, map[netip.Addr]bool{}
	prefixes := []netip.Prefix{transit}
	for _, intent := range intents {
		if _, ok := clusters[intent.ClusterRef]; !ok {
			return nil, fmt.Errorf("attachment clusterRef %q is not registered", intent.ClusterRef)
		}
		if seenClusters[intent.ClusterRef] {
			return nil, fmt.Errorf("duplicate attachment clusterRef %q", intent.ClusterRef)
		}
		seenClusters[intent.ClusterRef] = true
		subnet, err := prefix(intent.CIDR)
		if err != nil {
			return nil, fmt.Errorf("attachment %q cidr: %w", intent.ClusterRef, err)
		}
		for _, other := range prefixes {
			if subnet.Overlaps(other) {
				return nil, fmt.Errorf("attachment %q overlaps another prefix in this GlobalVpc", intent.ClusterRef)
			}
		}
		prefixes = append(prefixes, subnet)
		if _, err := host(intent.Gateway, subnet); err != nil {
			return nil, fmt.Errorf("attachment %q gateway: %w", intent.ClusterRef, err)
		}
		address, err := host(intent.TransitIP, transit)
		if err != nil {
			return nil, fmt.Errorf("attachment %q transitIP: %w", intent.ClusterRef, err)
		}
		if seenTransitIPs[address] {
			return nil, fmt.Errorf("duplicate transitIP")
		}
		seenTransitIPs[address] = true
	}

	tenantKey := shortHash(uid)
	p := &Plan{UID: uid, TransitSwitch: "gt-" + tenantKey}
	p.Claims = append(p.Claims, claim(uid, "transit", vpc.Spec.IPAM.ScopeRef, transit.String()))
	for _, intent := range intents {
		cluster := clusters[intent.ClusterRef]
		key := tenantKey + "-" + shortHash(cluster.UID)
		site := Site{Cluster: cluster, Intent: intent, VpcName: "gv-" + key, SubnetName: "gs-" + key, TransitName: p.TransitSwitch}
		labels := map[string]interface{}{v1alpha1.OwnerLabel: uid, v1alpha1.ClusterLabel: cluster.UID}
		routes := make([]interface{}, 0, len(intents)-1)
		for _, remote := range intents {
			if remote.ClusterRef != intent.ClusterRef {
				routes = append(routes, map[string]interface{}{"policy": "policyDst", "cidr": remote.CIDR, "nextHopIP": remote.TransitIP})
			}
		}
		site.Vpc = resource("Vpc", site.VpcName, labels, map[string]interface{}{"staticRoutes": routes})
		site.Subnet = subnetResource(site.SubnetName, site.VpcName, intent.CIDR, intent.Gateway, []interface{}{intent.Gateway}, labels)
		// Reserve every usable transit address, including future gateway addresses.
		// A tenant endpoint must never be allocated on the shared transit network.
		excluded := transit.Addr().Next().String() + ".." + lastAddress(transit).Prev().String()
		site.Transit = subnetResource(site.TransitName, site.VpcName, transit.String(), intent.TransitIP, []interface{}{excluded}, labels)
		p.Attachments = append(p.Attachments, site)
		p.Claims = append(p.Claims, claim(uid, "cluster/"+cluster.UID, vpc.Spec.IPAM.ScopeRef, intent.CIDR))
	}
	// Binding the plan to connection/authority registration prevents configuration
	// changes from silently retargeting resources already accepted by a controller.
	identity := struct {
		Version         string
		UID             string
		Domain          string
		IPAM            v1alpha1.IPAMReference
		TransitCIDR     string
		Attachments     []v1alpha1.Attachment
		Clusters        []config.Cluster
		ICNBCommand     []string
		ProviderCommand []string
		Resources       []Site
	}{"v1", uid, cfg.Domain, vpc.Spec.IPAM, transit.String(), intents, registered, cfg.ICNBCommand, providerCommand, p.Attachments}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return nil, fmt.Errorf("encode plan: %w", err)
	}
	digest := sha256.Sum256(encoded)
	p.Hash = hex.EncodeToString(digest[:])
	return p, nil
}

func shortHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:8])
}

func claim(uid, attachment, scope, cidr string) v1alpha1.PrefixClaim {
	return v1alpha1.PrefixClaim{ClaimUID: uid + "/" + attachment, VpcUID: uid, ScopeRef: scope, CIDR: cidr}
}

func prefix(raw string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(raw)
	if err != nil || !p.Addr().Is4() || p.Bits() < 8 || p.Bits() > 30 || p.Masked().String() != raw {
		return netip.Prefix{}, fmt.Errorf("must be a canonical IPv4 network from /8 through /30")
	}
	if !p.Addr().IsGlobalUnicast() || !lastAddress(p).IsGlobalUnicast() || p.Addr().IsLoopback() || p.Addr().IsLinkLocalUnicast() {
		return netip.Prefix{}, fmt.Errorf("must be a unicast subnet outside loopback and link-local ranges")
	}
	return p, nil
}

func host(raw string, p netip.Prefix) (netip.Addr, error) {
	a, err := netip.ParseAddr(raw)
	if err != nil || !a.Is4() || a.String() != raw || !p.Contains(a) || a == p.Addr() || a == lastAddress(p) || !a.IsGlobalUnicast() {
		return netip.Addr{}, fmt.Errorf("must be a usable IPv4 host in its subnet, excluding network and broadcast")
	}
	return a, nil
}

func lastAddress(p netip.Prefix) netip.Addr {
	b := p.Addr().As4()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v |= ^uint32(0) >> p.Bits()
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

func resource(kind, name string, labels, spec map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kubeovn.io/v1", "kind": kind,
		"metadata": map[string]interface{}{"name": name, "labels": labels}, "spec": spec,
	}}
}

func subnetResource(name, vpc, cidr, gateway string, excluded []interface{}, labels map[string]interface{}) *unstructured.Unstructured {
	return resource("Subnet", name, labels, map[string]interface{}{
		"vpc": vpc, "cidrBlock": cidr, "gateway": gateway, "excludeIps": excluded,
		"provider": "ovn", "protocol": "IPv4", "natOutgoing": false,
		"enableDHCP": false, "enableLb": false,
		"disableGatewayCheck": true, "disableInterConnection": true,
	})
}
