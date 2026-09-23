// Package platformconfig contains administrator-owned location and network classes.
// Tenant resources never contain Kubernetes credentials or transport peer lists.
package platformconfig

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"globalvpc.io/controller/internal/siteconfig"
	"k8s.io/apimachinery/pkg/util/validation"
)

type NetworkClass struct {
	TransportProfile string `json:"transportProfile"`
}

type Location struct {
	Name             string   `json:"name"`
	Region           string   `json:"region"`
	Site             string   `json:"site"`
	DC               string   `json:"dc"`
	BindingNamespace string   `json:"bindingNamespace"`
	AllowedProjects  []string `json:"allowedProjects"`
	// CIDRPools are administrator-reserved workload ranges, not transit ranges.
	CIDRPools []string `json:"cidrPools"`
}

type Config struct {
	RegistryNamespace string                  `json:"registryNamespace,omitempty"`
	NetworkClasses    map[string]NetworkClass `json:"networkClasses"`
	Locations         []Location              `json:"locations"`
}

func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	var c Config
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, fmt.Errorf("invalid platform configuration: %w", err)
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return c, fmt.Errorf("expected one configuration object")
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	if c.RegistryNamespace != "" && len(validation.IsDNS1123Label(c.RegistryNamespace)) != 0 {
		return fmt.Errorf("invalid registry namespace")
	}
	if _, ok := c.NetworkClasses["default"]; !ok {
		return fmt.Errorf("a default network class is required")
	}
	for name, class := range c.NetworkClasses {
		if len(validation.IsDNS1123Label(name)) != 0 {
			return fmt.Errorf("invalid network class name")
		}
		switch class.TransportProfile {
		case "wireguard-bgp", "geneve-bgp", "vxlan-evpn":
		default:
			return fmt.Errorf("unsupported transport profile for %s", name)
		}
	}
	seen, namespaces := map[string]bool{}, map[string]bool{}
	for _, l := range c.Locations {
		for _, s := range []string{l.Name, l.Region, l.Site, l.DC, l.BindingNamespace} {
			if len(validation.IsDNS1123Label(s)) != 0 {
				return fmt.Errorf("location identity and namespace must be DNS labels")
			}
		}
		if seen[l.Name] || namespaces[l.BindingNamespace] {
			return fmt.Errorf("locations require unique names and binding namespaces")
		}
		seen[l.Name], namespaces[l.BindingNamespace] = true, true
		if len(l.AllowedProjects) == 0 || len(l.CIDRPools) == 0 {
			return fmt.Errorf("location %s requires explicit project grants and reserved CIDR pools", l.Name)
		}
		for _, p := range l.AllowedProjects {
			if len(validation.IsDNS1123Label(p)) != 0 {
				return fmt.Errorf("project grants must name an explicit namespace")
			}
		}
		for _, raw := range l.CIDRPools {
			if _, err := siteconfig.Prefix(raw); err != nil {
				return fmt.Errorf("location %s: %w", l.Name, err)
			}
		}
	}
	if len(c.Locations) == 0 {
		return fmt.Errorf("at least one location is required")
	}
	return nil
}

func (c Config) Location(name, project string) (Location, error) {
	for _, l := range c.Locations {
		if l.Name != name {
			continue
		}
		for _, allowed := range l.AllowedProjects {
			if allowed == project {
				return l, nil
			}
		}
		return Location{}, fmt.Errorf("project is not authorized for location %s", name)
	}
	return Location{}, fmt.Errorf("location %s is not registered", name)
}

func (l Location) AllowsCIDR(raw string) bool {
	p, err := siteconfig.Prefix(raw)
	if err != nil {
		return false
	}
	for _, rawPool := range l.CIDRPools {
		pool, err := siteconfig.Prefix(rawPool)
		if err == nil && pool.Bits() <= p.Bits() && pool.Contains(p.Addr()) {
			return true
		}
	}
	return false
}
