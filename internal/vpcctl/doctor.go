package vpcctl

import (
	"context"
	"encoding/json"
	"fmt"
	"text/tabwriter"

	api "globalvpc.io/controller/api/v1alpha2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

type doctorResource struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Phase  string `json:"phase"`
	Ready  bool   `json:"ready"`
	Reason string `json:"reason"`
}

type doctorReport struct {
	Namespace string           `json:"namespace"`
	APIAccess bool             `json:"apiAccess"`
	Resources []doctorResource `json:"resources"`
	Scope     string           `json:"scope"`
}

func (a *App) runDoctor(ctx context.Context, args []string, opts Options) error {
	if len(args) == 1 && hasHelp(args) {
		fmt.Fprintln(a.Out, "Usage: vpcctl [global flags] doctor\nRead public VPC/Subnet API access and current status in the selected project. No administrator permissions or writes are used.")
		return nil
	}
	if len(args) != 0 {
		return fmt.Errorf("doctor takes no arguments; use --help")
	}
	c, err := a.client(opts)
	if err != nil {
		return err
	}
	vpcs, subnets := &api.VPCList{}, &api.SubnetList{}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	if err := c.List(ctx, vpcs, client.InNamespace(opts.Namespace)); err != nil {
		return fmt.Errorf("cannot list public VPCs in project %q; check context, API installation and project RBAC: %w", opts.Namespace, err)
	}
	if err := c.List(ctx, subnets, client.InNamespace(opts.Namespace)); err != nil {
		return fmt.Errorf("cannot list public Subnets in project %q; check API installation and project RBAC: %w", opts.Namespace, err)
	}
	report := doctorReport{Namespace: opts.Namespace, APIAccess: true, Resources: []doctorResource{}, Scope: "Public API access and configuration observations only; no packet or native-controller health test."}
	ready := true
	for _, v := range vpcs.Items {
		r := doctorObservation("VPC", v.ObjectMeta, v.Status.Phase, v.Status.ObservedGeneration, v.Status.Conditions)
		report.Resources = append(report.Resources, r)
		ready = ready && r.Ready
	}
	for _, s := range subnets.Items {
		r := doctorObservation("Subnet", s.ObjectMeta, s.Status.Phase, s.Status.ObservedGeneration, s.Status.Conditions)
		report.Resources = append(report.Resources, r)
		ready = ready && r.Ready
	}
	switch opts.Output {
	case "json", "yaml":
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		if opts.Output == "yaml" {
			data, err = yaml.JSONToYAML(data)
			if err != nil {
				return err
			}
		}
		fmt.Fprintln(a.Out, string(data))
	default:
		fmt.Fprintf(a.Out, "Project %s: public VPC/Subnet API access succeeded\n", opts.Namespace)
		w := tabwriter.NewWriter(a.Out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "KIND\tNAME\tPHASE\tREADY\tREASON")
		for _, r := range report.Resources {
			fmt.Fprintf(w, "%s\t%s\t%s\t%t\t%s\n", r.Kind, r.Name, r.Phase, r.Ready, r.Reason)
		}
		if err := w.Flush(); err != nil {
			return err
		}
		if len(report.Resources) == 0 {
			fmt.Fprintln(a.Out, "No public resources exist in this project yet.")
		}
		fmt.Fprintln(a.Out, report.Scope)
	}
	if !ready {
		return fmt.Errorf("some resources have pending, stale or terminating configuration; inspect vpc/subnet get output")
	}
	return nil
}

func doctorObservation(kind string, meta metav1.ObjectMeta, phase string, observed int64, conditions []metav1.Condition) doctorResource {
	r := doctorResource{Kind: kind, Name: meta.Name, Phase: phase, Reason: "AwaitingController"}
	if !meta.DeletionTimestamp.IsZero() {
		r.Reason = "Terminating"
		return r
	}
	if observed != meta.Generation {
		r.Reason = "StaleGeneration"
		return r
	}
	for _, c := range conditions {
		if c.Type == "Ready" {
			r.Reason = c.Reason
			if c.ObservedGeneration != meta.Generation {
				r.Reason = "StaleCondition"
				return r
			}
			r.Ready = c.Status == metav1.ConditionTrue
			return r
		}
	}
	return r
}
