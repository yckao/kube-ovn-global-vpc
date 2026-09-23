// Package config contains administrator-controlled connection and domain registration.
package config

import (
	"encoding/json"
	"fmt"
	"io"
	"k8s.io/apimachinery/pkg/util/validation"
	"os"
)

type Cluster struct {
	Name           string              `json:"name"`
	UID            string              `json:"uid"`
	Region         string              `json:"region"`
	Site           string              `json:"site"`
	DC             string              `json:"dc"`
	Kubeconfig     string              `json:"kubeconfig"`
	NBCommand      []string            `json:"nbCommand"`
	SBCommand      []string            `json:"sbCommand"`
	GatewayChassis []string            `json:"gatewayChassis"`
	AZName         string              `json:"azName"`
	ICDeployment   DeploymentReference `json:"icDeployment"`
}
type DeploymentReference struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}
type Config struct {
	Domain      string              `json:"domain"`
	Clusters    []Cluster           `json:"clusters"`
	ICNBCommand []string            `json:"icNBCommand"`
	Providers   map[string][]string `json:"providers"`
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
		return cfg, fmt.Errorf("invalid controller configuration")
	}
	if err = d.Decode(new(interface{})); err != io.EOF {
		return cfg, fmt.Errorf("controller configuration must contain exactly one JSON object")
	}
	return cfg, cfg.Validate()
}
func (c Config) Validate() error {
	if len(validation.IsDNS1123Label(c.Domain)) != 0 {
		return fmt.Errorf("domain must be a DNS label of at most 63 characters")
	}
	if c.Domain == "" || len(c.Clusters) < 2 || len(c.Clusters) > 32 || len(c.ICNBCommand) == 0 || len(c.Providers) == 0 {
		return fmt.Errorf("domain, 2-32 clusters, IC command and providers are required")
	}
	names, uids, azs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, x := range c.Clusters {
		if x.Name == "" || x.UID == "" || x.Region == "" || x.Site == "" || x.DC == "" || x.Kubeconfig == "" || x.AZName == "" || len(x.NBCommand) == 0 || len(x.SBCommand) == 0 || len(x.GatewayChassis) == 0 {
			return fmt.Errorf("incomplete cluster registration")
		}
		if names[x.Name] || uids[x.UID] || azs[x.AZName] {
			return fmt.Errorf("duplicate cluster name, UID or availability zone")
		}
		if x.ICDeployment.Namespace == "" || x.ICDeployment.Name == "" || x.ICDeployment.UID == "" {
			return fmt.Errorf("dedicated native IC Deployment identity is required")
		}
		names[x.Name], uids[x.UID], azs[x.AZName] = true, true, true
	}
	for k, v := range c.Providers {
		if k == "" || len(v) == 0 {
			return fmt.Errorf("invalid provider command")
		}
	}
	return nil
}
