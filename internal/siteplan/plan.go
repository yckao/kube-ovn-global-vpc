// Package siteplan renders only one locally owned attachment. Planning never
// contacts another Controller, remote API, shared IC database, or IPAM service.
package siteplan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	api "globalvpc.io/controller/api/v1alpha1"
	"globalvpc.io/controller/internal/siteconfig"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
)

// GatewayConfig is the local routing agent contract. The administrator selects
// its backend and peers outside tenant intent. The agent advertises only CIDR,
// reaches it through RouterIP, and keeps healthy forwarding after controller loss.
type GatewayConfig struct {
	Version           string               `json:"version"`
	GlobalVpcID       string               `json:"globalVpcID"`
	OwnerUID          string               `json:"ownerUID"`
	SiteID            string               `json:"siteID"`
	AttachmentID      string               `json:"attachmentID"`
	GatewayRevision   string               `json:"gatewayRevision"`
	DelegatedPrefixes []string             `json:"delegatedPrefixes"`
	CIDR              string               `json:"cidr"`
	TransitCIDR       string               `json:"transitCIDR"`
	RouterIP          string               `json:"routerIP"`
	GatewayIP         string               `json:"gatewayIP,omitempty"`
	VpcName           string               `json:"vpcName"`
	SubnetName        string               `json:"subnetName"`
	TransitName       string               `json:"transitName"`
	Gateways          []siteconfig.Gateway `json:"gateways,omitempty"`
	BFD               *siteconfig.BFD      `json:"bfd,omitempty"`
}

func (g GatewayConfig) IsHA() bool { return g.Version == "v2" }

// Members returns a copy so runtime reconciliation cannot mutate plan identity.
func (g GatewayConfig) Members() []siteconfig.Gateway {
	if !g.IsHA() {
		return []siteconfig.Gateway{{ID: "default", IP: g.GatewayIP}}
	}
	return append([]siteconfig.Gateway(nil), g.Gateways...)
}

// BFDPortName is created and owned by Kube-OVN through Vpc.spec.bfdPort.
func (g GatewayConfig) BFDPortName() string { return "bfd@" + g.VpcName }

type Plan struct {
	OwnerUID, GlobalVpcID, SiteID, AttachmentID, Hash string
	VpcName, SubnetName, TransitName                  string
	Vpc, Subnet, Transit                              *unstructured.Unstructured
	Gateway                                           GatewayConfig
}

func Build(v *api.SiteVpc, cfg siteconfig.Config) (*Plan, error) {
	if v == nil {
		return nil, fmt.Errorf("SiteVpc is required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid local registration: %w", err)
	}
	uid := string(v.UID)
	if uid == "" || len(validation.IsValidLabelValue(uid)) != 0 {
		return nil, fmt.Errorf("SiteVpc must have a valid server-assigned UID")
	}
	if v.Spec.SiteID != cfg.SiteID || v.Spec.AttachmentID != cfg.AttachmentID {
		return nil, fmt.Errorf("SiteVpc does not target this local attachment")
	}
	var grant *siteconfig.Grant
	for _, g := range cfg.Grants {
		if g.GlobalVpcID == v.Spec.GlobalVpcID {
			copy := g
			copy.DelegatedPrefixes = append([]string(nil), g.DelegatedPrefixes...)
			sort.Strings(copy.DelegatedPrefixes)
			copy.Gateways = append([]siteconfig.Gateway(nil), g.Gateways...)
			sort.Slice(copy.Gateways, func(i, j int) bool { return copy.Gateways[i].ID < copy.Gateways[j].ID })
			copy.BFD = canonicalBFD(g.BFD)
			grant = &copy
			break
		}
	}
	if grant == nil {
		return nil, fmt.Errorf("globalVpcID has no local administrator grant")
	}
	prefix, err := siteconfig.Prefix(v.Spec.CIDR)
	if err != nil {
		return nil, fmt.Errorf("cidr: %w", err)
	}
	allowed := false
	for _, raw := range grant.DelegatedPrefixes {
		parent, _ := siteconfig.Prefix(raw)
		if parent.Bits() <= prefix.Bits() && parent.Contains(prefix.Addr()) {
			allowed = true
		}
	}
	if !allowed {
		return nil, fmt.Errorf("cidr is outside this attachment's delegated prefixes")
	}
	if _, err = siteconfig.Host(v.Spec.Gateway, prefix); err != nil {
		return nil, fmt.Errorf("gateway: %w", err)
	}
	key := shortHash(v.Spec.GlobalVpcID + "\x00" + cfg.SiteID + "\x00" + cfg.AttachmentID)
	p := &Plan{OwnerUID: uid, GlobalVpcID: v.Spec.GlobalVpcID, SiteID: cfg.SiteID, AttachmentID: cfg.AttachmentID,
		VpcName: "sv-" + key, SubnetName: "ss-" + key, TransitName: "st-" + key}
	labels := map[string]interface{}{api.OwnerLabel: uid, api.ClusterLabel: cfg.Cluster.UID, api.GlobalIDLabel: shortHash(v.Spec.GlobalVpcID), api.AttachmentLabel: cfg.AttachmentID}
	p.Vpc = resource("Vpc", p.VpcName, labels, map[string]interface{}{"staticRoutes": []interface{}{map[string]interface{}{"policy": "policyDst", "cidr": "0.0.0.0/0", "nextHopIP": grant.GatewayIP}}})
	p.Subnet = subnet(p.SubnetName, p.VpcName, prefix.String(), v.Spec.Gateway, []interface{}{v.Spec.Gateway}, labels)
	transit, _ := siteconfig.Prefix(grant.TransitCIDR)
	// Leave only the gateway's explicit CNI address allocatable. Workload admission
	// must restrict access to this dedicated transit Subnet; labels are not ACLs.
	p.Transit = subnet(p.TransitName, p.VpcName, transit.String(), grant.RouterIP, transitExclusions(transit, grant.GatewayIP), labels)
	p.Gateway = GatewayConfig{Version: "v1", GlobalVpcID: p.GlobalVpcID, OwnerUID: uid, SiteID: cfg.SiteID, AttachmentID: cfg.AttachmentID,
		GatewayRevision: cfg.GatewayRevision, DelegatedPrefixes: append([]string(nil), grant.DelegatedPrefixes...), CIDR: prefix.String(), TransitCIDR: transit.String(),
		RouterIP: grant.RouterIP, GatewayIP: grant.GatewayIP, VpcName: p.VpcName, SubnetName: p.SubnetName, TransitName: p.TransitName}
	planVersion := "site-v1"
	if len(grant.Gateways) != 0 {
		planVersion = "site-v2"
		p.Gateway.Version, p.Gateway.Gateways, p.Gateway.BFD = "v2", grant.Gateways, grant.BFD
		bfdPort := map[string]interface{}{"enabled": true, "ip": grant.BFD.SourceIP}
		if grant.BFD.NodeSelector != nil {
			selector, err := runtime.DefaultUnstructuredConverter.ToUnstructured(grant.BFD.NodeSelector)
			if err != nil {
				return nil, fmt.Errorf("encode native BFD node selector: %w", err)
			}
			bfdPort["nodeSelector"] = selector
		}
		// The reconciler persists controller-owned BFD UUIDs before rendering
		// routes. Starting with no default prevents unmonitored HA next hops.
		p.Vpc = resource("Vpc", p.VpcName, labels, map[string]interface{}{"staticRoutes": []interface{}{}, "bfdPort": bfdPort})
		ips := make([]string, 0, len(grant.Gateways))
		for _, member := range grant.Gateways {
			ips = append(ips, member.IP)
		}
		p.Transit = subnet(p.TransitName, p.VpcName, transit.String(), grant.RouterIP, transitExclusions(transit, ips...), labels)
	}
	// Only the selected tenant grant participates. An unrelated tenant/site joining
	// must not freeze reconciliation of an already accepted local attachment.
	identity := struct {
		Version         string
		OwnerUID        string
		Spec            api.SiteVpcSpec
		Cluster         siteconfig.Cluster
		GatewayCommand  []string
		GatewayRevision string
		Grant           siteconfig.Grant
	}{planVersion, uid, v.Spec, cfg.Cluster, cfg.GatewayCommand, cfg.GatewayRevision, *grant}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return nil, fmt.Errorf("encode local plan: %w", err)
	}
	digest := sha256.Sum256(encoded)
	p.Hash = hex.EncodeToString(digest[:])
	return p, nil
}

func shortHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:8])
}

func canonicalBFD(b *siteconfig.BFD) *siteconfig.BFD {
	if b == nil {
		return nil
	}
	out := *b
	out.NodeSelector = b.NodeSelector.DeepCopy()
	if out.NodeSelector != nil {
		for i := range out.NodeSelector.MatchExpressions {
			sort.Strings(out.NodeSelector.MatchExpressions[i].Values)
		}
		sort.Slice(out.NodeSelector.MatchExpressions, func(i, j int) bool {
			a, z := out.NodeSelector.MatchExpressions[i], out.NodeSelector.MatchExpressions[j]
			return a.Key+"\x00"+string(a.Operator)+"\x00"+strings.Join(a.Values, "\x00") < z.Key+"\x00"+string(z.Operator)+"\x00"+strings.Join(z.Values, "\x00")
		})
	}
	return &out
}

func transitExclusions(p netip.Prefix, gateways ...string) []interface{} {
	addresses := make([]netip.Addr, 0, len(gateways))
	for _, gateway := range gateways {
		g, _ := netip.ParseAddr(gateway)
		addresses = append(addresses, g)
	}
	sort.Slice(addresses, func(i, j int) bool { return addresses[i].Less(addresses[j]) })
	first, last := p.Addr().Next(), siteconfig.LastAddress(p).Prev()
	out := []interface{}{}
	appendRange := func(start, end netip.Addr) {
		if start == end {
			out = append(out, start.String())
		} else {
			out = append(out, start.String()+".."+end.String())
		}
	}
	for _, g := range addresses {
		if first.Less(g) {
			appendRange(first, g.Prev())
		}
		first = g.Next()
	}
	if first.Compare(last) <= 0 {
		appendRange(first, last)
	}
	return out
}

func resource(kind, name string, labels, spec map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "kubeovn.io/v1", "kind": kind,
		"metadata": map[string]interface{}{"name": name, "labels": labels}, "spec": spec}}
}

func subnet(name, vpc, cidr, gateway string, excluded []interface{}, labels map[string]interface{}) *unstructured.Unstructured {
	return resource("Subnet", name, labels, map[string]interface{}{"vpc": vpc, "cidrBlock": cidr, "gateway": gateway, "excludeIps": excluded,
		"provider": "ovn", "protocol": "IPv4", "natOutgoing": false, "enableDHCP": false, "enableLb": false,
		"disableGatewayCheck": true, "disableInterConnection": true})
}
