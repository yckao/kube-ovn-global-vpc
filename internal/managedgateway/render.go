package managedgateway

import (
	"encoding/base64"
	"fmt"
	"net/netip"
	"sort"

	api "globalvpc.io/controller/api/v1alpha2"
)

func ipv4(s string) bool {
	ip, e := netip.ParseAddr(s)
	return e == nil && ip.Is4() && ip.String() == s
}
func findLink(links []api.GatewayLink, id string) (api.GatewayLink, bool) {
	for _, l := range links {
		if l.PeerID == id {
			return l, true
		}
	}
	return api.GatewayLink{}, false
}
func (r *Reconciler) render(b *api.NetworkBinding, a Attachment, l ledger, m member) (map[string]any, bool, error) {
	cidrs := []string{}
	for _, s := range b.Spec.Subnets {
		if !s.Deleting {
			cidrs = append(cidrs, s.CIDR)
		}
	}
	sort.Strings(cidrs)
	// A binding without local subnets is retired by the native local reconciler.
	if len(cidrs) == 0 {
		return nil, false, nil
	}
	gateway := map[string]any{"version": "v2", "ownerUID": string(b.UID), "globalVpcID": b.Spec.NativeVpcName, "siteID": b.Spec.LocationRef, "attachmentID": b.Name, "gatewayID": m.Endpoint.ID, "vpcName": b.Spec.NativeVpcName, "transitName": "pt-" + Short(b.Spec.VPCRef.UID+"/"+b.Spec.LocationRef), "transitCIDR": a.TransitCIDR, "cidr": cidrs[0], "cidrs": cidrs, "routerIP": a.RouterIP, "gatewayIP": m.Endpoint.TransitIP, "bfd": map[string]any{"sourceIP": a.BFDSourceIP, "minRX": 300, "minTX": 300, "multiplier": 3}}
	siblings := []map[string]any{}
	for _, s := range l.Members {
		if s.Endpoint.ID != m.Endpoint.ID {
			siblings = append(siblings, map[string]any{"id": s.Endpoint.ID, "ip": s.Endpoint.TransitIP})
		}
	}
	transport := map[string]any{"profile": b.Spec.TransportProfile, "localASN": b.Spec.LocalASN, "routerID": m.Endpoint.HealthIP, "mtu": r.MTU, "siblings": siblings}
	links := []map[string]any{}
	localID := peerID(b.Spec.LocationRef, m.Endpoint.ID)
	seen := map[string]bool{}
	locations := map[string]bool{}
	authorized := map[string]bool{}
	for _, v := range b.Spec.AuthorizedLocations {
		authorized[v] = true
	}
	peers := append([]api.PeerGateway(nil), b.Spec.Peers...)
	sort.Slice(peers, func(i, j int) bool {
		return peerID(peers[i].LocationRef, peers[i].Endpoint.ID) < peerID(peers[j].LocationRef, peers[j].Endpoint.ID)
	})
	for _, p := range peers {
		id := peerID(p.LocationRef, p.Endpoint.ID)
		if seen[id] || p.LocationRef == b.Spec.LocationRef || !authorized[p.LocationRef] {
			return nil, false, fmt.Errorf("duplicate or unauthorized remote gateway")
		}
		seen[id] = true
		prefixes := []string{}
		for _, s := range b.Spec.RemoteSubnets {
			if s.LocationRef == p.LocationRef {
				prefixes = append(prefixes, s.CIDR)
			}
		}
		if len(prefixes) == 0 {
			continue
		}
		sort.Strings(prefixes)
		if !ipv4(p.Endpoint.EndpointIP) || !ipv4(p.Endpoint.HealthIP) || p.Endpoint.LocalASN < 4200000000 || p.Endpoint.LocalASN == b.Spec.LocalASN {
			return nil, false, fmt.Errorf("invalid remote endpoint identity")
		}
		local, ok := findLink(m.Endpoint.Links, id)
		if !ok {
			return nil, false, nil
		}
		remote, ok := findLink(p.Endpoint.Links, localID)
		if !ok {
			return nil, false, nil
		}
		if !ipv4(local.TunnelIP) || !ipv4(remote.TunnelIP) || local.TunnelIP == remote.TunnelIP || local.ListenPort < 1024 || local.ListenPort > 65535 || remote.ListenPort < 1024 || remote.ListenPort > 65535 {
			return nil, false, fmt.Errorf("invalid reciprocal link allocation")
		}
		link := map[string]any{"id": Short(localID + "/" + id), "listenPort": local.ListenPort, "localTunnelIP": local.TunnelIP, "tunnelIP": remote.TunnelIP, "endpoint": fmt.Sprintf("%s:%d", p.Endpoint.EndpointIP, remote.ListenPort), "asn": p.Endpoint.LocalASN, "remoteSiteID": p.LocationRef, "remoteAttachmentID": p.LocationRef, "delegatedPrefixes": prefixes}
		if b.Spec.TransportProfile == "wireguard-bgp" {
			key, e := base64.StdEncoding.DecodeString(p.Endpoint.PublicKey)
			if e != nil || len(key) != 32 {
				return nil, false, fmt.Errorf("invalid public WireGuard key")
			}
			link["publicKey"] = p.Endpoint.PublicKey
		} else {
			if local.ListenPort != remote.ListenPort || local.ControlVNI == 0 || local.ControlVNI > 16777215 || local.ControlVNI != remote.ControlVNI || local.ControlVNI == b.Spec.NetworkID {
				return nil, false, fmt.Errorf("native peer allocations disagree")
			}
			link["vni"] = local.ControlVNI
			if b.Spec.TransportProfile == "vxlan-evpn" {
				link["remoteHealthIP"] = p.Endpoint.HealthIP
			}
		}
		links = append(links, link)
		locations[p.LocationRef] = true
	}
	for _, s := range b.Spec.RemoteSubnets {
		if !locations[s.LocationRef] {
			return nil, false, nil
		}
		p, e := netip.ParsePrefix(s.CIDR)
		if e != nil || !p.Addr().Is4() || p != p.Masked() {
			return nil, false, fmt.Errorf("invalid remote subnet")
		}
		for _, local := range cidrs {
			q, _ := netip.ParsePrefix(local)
			if p.Overlaps(q) {
				return nil, false, fmt.Errorf("overlapping subnet ownership within a VPC")
			}
		}
	}
	transport["links"] = links
	switch b.Spec.TransportProfile {
	case "wireguard-bgp":
		transport["privateKeySecretName"] = m.SecretName
	case "geneve-bgp":
		transport["trustedUnderlay"] = true
		transport["underlayIP"] = m.Endpoint.EndpointIP
	case "vxlan-evpn":
		if b.Spec.NetworkID == 0 || b.Spec.NetworkID > 16777215 {
			return nil, false, fmt.Errorf("native tenant VNI required")
		}
		transport["trustedUnderlay"] = true
		transport["localVtepIP"] = m.Endpoint.EndpointIP
		transport["healthIP"] = m.Endpoint.HealthIP
		transport["vni"] = b.Spec.NetworkID
		transport["vxlanPort"] = 4789
		transport["routeDistinguisher"] = m.Endpoint.HealthIP + ":1"
		transport["routeTarget"] = fmt.Sprintf("64512:%d", b.Spec.NetworkID)
	default:
		return nil, false, fmt.Errorf("unknown transport profile")
	}
	return map[string]any{"managedVersion": "v1", "gateway": gateway, "transport": transport}, true, nil
}
