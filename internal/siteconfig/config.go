// Package siteconfig defines administrator-owned, locally durable grants.
// It contains no peer Controller, remote Kubernetes API, or shared IC endpoint.
package siteconfig

import (
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metavalidation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// Cluster is exactly one locally managed Infra attachment. A site with several
// Infra clusters runs separate attachment controllers with distinct identities.
type Cluster struct {
	Name       string   `json:"name"`
	UID        string   `json:"uid"`
	Kubeconfig string   `json:"kubeconfig"`
	NBCommand  []string `json:"nbCommand"`
	SBCommand  []string `json:"sbCommand"`
}

// Grant is provisioned by an administrator before disconnected operation. A
// delegated prefix is never reclaimed merely because a remote site times out.
type Grant struct {
	GlobalVpcID       string    `json:"globalVpcID"`
	DelegatedPrefixes []string  `json:"delegatedPrefixes"`
	TransitCIDR       string    `json:"transitCIDR"`
	RouterIP          string    `json:"routerIP"`
	GatewayIP         string    `json:"gatewayIP,omitempty"`
	Gateways          []Gateway `json:"gateways,omitempty"`
	BFD               *BFD      `json:"bfd,omitempty"`
}

// Gateway is a stable administrator-owned local forwarding member. Placement,
// credentials, and WAN peers remain in the separately registered backend.
type Gateway struct {
	ID string `json:"id"`
	IP string `json:"ip"`
}

// BFD configures the native Kube-OVN BFD-only logical router port and the
// controller-owned BFD sessions that observe local gateway members. Timers are
// milliseconds; nodeSelector selects the native BFD port's HA chassis only.
type BFD struct {
	SourceIP     string                `json:"sourceIP"`
	MinRX        int                   `json:"minRX"`
	MinTX        int                   `json:"minTX"`
	Multiplier   int                   `json:"multiplier"`
	NodeSelector *metav1.LabelSelector `json:"nodeSelector,omitempty"`
}

type Config struct {
	SiteID          string   `json:"siteID"`
	AttachmentID    string   `json:"attachmentID"`
	Cluster         Cluster  `json:"cluster"`
	GatewayCommand  []string `json:"gatewayCommand"`
	GatewayRevision string   `json:"gatewayRevision"`
	Grants          []Grant  `json:"grants"`
	// BFDTransactionCommand is runtime access to the same local NB database,
	// excluded from tenant plan identity. It carries atomic reference guards
	// when deleting already owned BFD UUIDs; it cannot retarget ownership.
	BFDTransactionCommand []string `json:"bfdTransactionCommand,omitempty"`
}

func Load(path string) (Config, error) {
	var cfg Config
	f, err := os.Open(path)
	if err != nil {
		return cfg, err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if err = d.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("invalid site configuration")
	}
	if err = d.Decode(new(interface{})); err != io.EOF {
		return cfg, fmt.Errorf("site configuration must contain exactly one JSON object")
	}
	return cfg, cfg.Validate()
}

func (c Config) Validate() error {
	if len(validation.IsDNS1123Label(c.SiteID)) != 0 || len(validation.IsDNS1123Label(c.AttachmentID)) != 0 {
		return fmt.Errorf("siteID and attachmentID must be DNS labels")
	}
	if len(validation.IsDNS1123Subdomain(c.Cluster.Name)) != 0 || c.Cluster.UID == "" || len(validation.IsValidLabelValue(c.Cluster.UID)) != 0 || c.Cluster.Kubeconfig == "" {
		return fmt.Errorf("local cluster name, UID and kubeconfig are required")
	}
	if !command(c.Cluster.NBCommand) || !command(c.Cluster.SBCommand) || !command(c.GatewayCommand) {
		return fmt.Errorf("local NB, SB and gateway commands are required")
	}
	if len(validation.IsDNS1123Label(c.GatewayRevision)) != 0 {
		return fmt.Errorf("gatewayRevision must be a DNS label matching the local backend registration")
	}
	if len(c.Grants) == 0 {
		return fmt.Errorf("at least one local tenant grant is required")
	}
	ids := map[string]bool{}
	for _, g := range c.Grants {
		if len(validation.IsDNS1123Subdomain(g.GlobalVpcID)) != 0 || ids[g.GlobalVpcID] {
			return fmt.Errorf("grant globalVpcID must be a unique DNS subdomain")
		}
		ids[g.GlobalVpcID] = true
		transit, err := Prefix(g.TransitCIDR)
		if err != nil {
			return fmt.Errorf("grant transitCIDR: %w", err)
		}
		if _, err = Host(g.RouterIP, transit); err != nil {
			return fmt.Errorf("grant routerIP: %w", err)
		}
		if err = g.validateGateways(transit); err != nil {
			return err
		}
		if len(g.Gateways) != 0 && !command(c.BFDTransactionCommand) {
			return fmt.Errorf("HA requires a local BFD transaction command")
		}
		if len(g.DelegatedPrefixes) == 0 {
			return fmt.Errorf("grant requires delegated prefixes")
		}
		seen := []netip.Prefix{transit}
		for _, raw := range g.DelegatedPrefixes {
			p, err := Prefix(raw)
			if err != nil {
				return fmt.Errorf("delegated prefix: %w", err)
			}
			for _, previous := range seen {
				if p.Overlaps(previous) {
					return fmt.Errorf("delegated prefixes and local transit must not overlap within a tenant")
				}
			}
			seen = append(seen, p)
		}
		if g.BFD != nil {
			source, _ := netip.ParseAddr(g.BFD.SourceIP)
			for _, p := range seen {
				if p.Contains(source) {
					return fmt.Errorf("BFD sourceIP must be outside local delegated prefixes and transit")
				}
			}
		}
	}
	return nil
}

func (g Grant) validateGateways(transit netip.Prefix) error {
	if len(g.Gateways) == 0 {
		if g.BFD != nil {
			return fmt.Errorf("BFD requires HA gateways")
		}
		if _, err := Host(g.GatewayIP, transit); err != nil {
			return fmt.Errorf("grant gatewayIP: %w", err)
		}
		if g.RouterIP == g.GatewayIP {
			return fmt.Errorf("grant routerIP and gatewayIP must differ")
		}
		return nil
	}
	if g.GatewayIP != "" || len(g.Gateways) < 2 || len(g.Gateways) > 8 || g.BFD == nil {
		return fmt.Errorf("HA requires 2 through 8 gateways and BFD, without legacy gatewayIP")
	}
	ids, ips := map[string]bool{}, map[string]bool{g.RouterIP: true}
	for _, member := range g.Gateways {
		if len(validation.IsDNS1123Label(member.ID)) != 0 || ids[member.ID] {
			return fmt.Errorf("gateway IDs must be unique DNS labels")
		}
		if _, err := Host(member.IP, transit); err != nil || ips[member.IP] {
			return fmt.Errorf("gateway IPs must be unique usable transit hosts distinct from routerIP")
		}
		ids[member.ID], ips[member.IP] = true, true
	}
	b := g.BFD
	a, err := netip.ParseAddr(b.SourceIP)
	if err != nil || !a.Is4() || a.String() != b.SourceIP || !a.IsGlobalUnicast() || a.IsLoopback() || a.IsLinkLocalUnicast() {
		return fmt.Errorf("BFD sourceIP must be a canonical unicast IPv4 address outside loopback and link-local ranges")
	}
	if b.MinRX < 100 || b.MinRX > 60000 || b.MinTX < 100 || b.MinTX > 60000 || b.Multiplier < 2 || b.Multiplier > 255 {
		return fmt.Errorf("BFD minRX/minTX must be 100 through 60000 milliseconds and multiplier 2 through 255")
	}
	if errors := metavalidation.ValidateLabelSelector(b.NodeSelector, metavalidation.LabelSelectorValidationOptions{}, field.NewPath("bfd", "nodeSelector")); len(errors) != 0 {
		return fmt.Errorf("BFD nodeSelector is invalid: %v", errors.ToAggregate())
	}
	return nil
}

func command(argv []string) bool { return len(argv) > 0 && argv[0] != "" }

// Prefix intentionally shares the routed alpha's conservative IPv4 boundary.
func Prefix(raw string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(raw)
	if err != nil || !p.Addr().Is4() || p.Bits() < 8 || p.Bits() > 30 || p.Masked().String() != raw {
		return netip.Prefix{}, fmt.Errorf("must be a canonical IPv4 network from /8 through /30")
	}
	if !p.Addr().IsGlobalUnicast() || !LastAddress(p).IsGlobalUnicast() || p.Addr().IsLoopback() || p.Addr().IsLinkLocalUnicast() {
		return netip.Prefix{}, fmt.Errorf("must be a unicast subnet outside loopback and link-local ranges")
	}
	return p, nil
}

func Host(raw string, p netip.Prefix) (netip.Addr, error) {
	a, err := netip.ParseAddr(raw)
	if err != nil || !a.Is4() || a.String() != raw || !p.Contains(a) || a == p.Addr() || a == LastAddress(p) || !a.IsGlobalUnicast() {
		return netip.Addr{}, fmt.Errorf("must be a usable IPv4 host in its subnet, excluding network and broadcast")
	}
	return a, nil
}

func LastAddress(p netip.Prefix) netip.Addr {
	b := p.Addr().As4()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v |= ^uint32(0) >> p.Bits()
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}
