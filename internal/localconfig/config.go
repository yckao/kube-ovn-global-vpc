// Package localconfig defines one operator's local installation settings.
package localconfig

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"globalvpc.io/controller/internal/siteconfig"
	"k8s.io/apimachinery/pkg/util/validation"
)

type Config struct {
	LocationRef         string            `json:"locationRef"`
	ClusterUID          string            `json:"clusterUID"`
	Namespace           string            `json:"namespace"`
	AuthorityNamespace  string            `json:"authorityNamespace"`
	AuthorityKubeconfig string            `json:"authorityKubeconfig"`
	GatewayNodeSelector map[string]string `json:"gatewayNodeSelector"`
	// EndpointAnnotation is optional; otherwise the Node InternalIP is used.
	EndpointAnnotation  string `json:"endpointAnnotation,omitempty"`
	TransitPool         string `json:"transitPool"`
	TransitPrefixLength int    `json:"transitPrefixLength"`
	BFDSourcePool       string `json:"bfdSourcePool"`
	GatewayReplicas     int    `json:"gatewayReplicas"`
	GatewayImage        string `json:"gatewayImage"`
	MTU                 int    `json:"mtu"`
	// ControlPool is reserved exclusively for this location's gateway control and
	// health addresses. WireGuard ports are reserved per host in this namespace.
	ControlPool string `json:"controlPool"`
	PortStart   int    `json:"portStart"`
	PortEnd     int    `json:"portEnd"`
	RuntimeDir  string `json:"runtimeDir"`
}

func Load(path string) (Config, error) {
	var c Config
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, fmt.Errorf("invalid local configuration: %w", err)
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return c, fmt.Errorf("expected one configuration object")
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	for _, s := range []string{c.LocationRef, c.Namespace, c.AuthorityNamespace} {
		if len(validation.IsDNS1123Label(s)) != 0 {
			return fmt.Errorf("local identity and namespaces must be DNS labels")
		}
	}
	if c.ClusterUID == "" || c.AuthorityKubeconfig == "" {
		return fmt.Errorf("cluster UID and authority kubeconfig reference are required")
	}
	transit, err := siteconfig.Prefix(c.TransitPool)
	if err != nil {
		return fmt.Errorf("transit pool: %w", err)
	}
	bfd, err := siteconfig.Prefix(c.BFDSourcePool)
	if err != nil {
		return fmt.Errorf("BFD source pool: %w", err)
	}
	if transit.Overlaps(bfd) {
		return fmt.Errorf("transit and BFD pools must not overlap")
	}
	control, err := siteconfig.Prefix(c.ControlPool)
	if err != nil {
		return fmt.Errorf("control pool: %w", err)
	}
	if control.Overlaps(transit) || control.Overlaps(bfd) {
		return fmt.Errorf("control, transit and BFD pools must not overlap")
	}
	if c.PortStart < 1024 || c.PortEnd > 65535 || c.PortEnd < c.PortStart || c.RuntimeDir == "" {
		return fmt.Errorf("reserved WireGuard port range and runtime directory are required")
	}
	for _, port := range []int{4788, 4789, 6081, 6082} {
		if c.PortStart <= port && port <= c.PortEnd {
			return fmt.Errorf("WireGuard port range overlaps reserved native transport ports")
		}
	}
	if c.TransitPrefixLength < transit.Bits() || c.TransitPrefixLength > 29 || c.TransitPrefixLength < 16 {
		return fmt.Errorf("transit prefix length must fit pool and provide usable gateway addresses")
	}
	if c.GatewayReplicas < 2 || c.GatewayReplicas > 64 || (1<<(32-c.TransitPrefixLength))-3 < c.GatewayReplicas {
		return fmt.Errorf("require 2 through 64 gateways and sufficient transit addresses")
	}
	if len(c.GatewayNodeSelector) == 0 || c.GatewayImage == "" || c.MTU < 1280 || c.MTU > 1400 {
		return fmt.Errorf("gateway selector, image and MTU 1280 through 1400 are required")
	}
	for k, v := range c.GatewayNodeSelector {
		if len(validation.IsQualifiedName(k)) != 0 || len(validation.IsValidLabelValue(v)) != 0 {
			return fmt.Errorf("invalid gateway node selector")
		}
	}
	if c.EndpointAnnotation != "" && len(validation.IsQualifiedName(c.EndpointAnnotation)) != 0 {
		return fmt.Errorf("invalid endpoint annotation")
	}
	return nil
}
