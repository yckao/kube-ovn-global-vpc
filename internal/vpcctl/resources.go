package vpcctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/pflag"
	api "globalvpc.io/controller/api/v1alpha2"
	"globalvpc.io/controller/internal/siteconfig"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// runResources operates only on the public, project-scoped API. It never writes
// status, finalizers, NetworkBindings, native Kube-OVN resources, or OVN state.
func (a *App) runResources(ctx context.Context, args []string, opts Options) error {
	if len(args) == 0 || (args[0] != "vpc" && args[0] != "subnet") {
		return fmt.Errorf("expected vpc or subnet command")
	}
	kind := args[0]
	if len(args) == 1 || args[1] == "--help" || args[1] == "help" || args[1] == "-h" {
		return a.resourceHelp(kind)
	}
	action := args[1]
	if action != "create" && action != "list" && action != "get" && action != "delete" && action != "wait" {
		return fmt.Errorf("unknown %s command %q; expected create, list, get, delete, or wait", kind, action)
	}
	flags := pflag.NewFlagSet(kind+" "+action, pflag.ContinueOnError)
	flags.SetOutput(a.ErrOut)
	var dryRun, await bool
	var vpc, location, cidr, networkClass string
	if action == "create" {
		flags.BoolVar(&dryRun, "dry-run", false, "Print the requested resource without contacting Kubernetes")
		flags.BoolVar(&await, "wait", false, "Wait for a current-generation Ready condition")
		if kind == "vpc" {
			flags.StringVar(&networkClass, "network-class", api.DefaultNetworkClass, "Administrator-defined network class")
		} else {
			flags.StringVar(&vpc, "vpc", "", "VPC name in the selected project namespace (required)")
			flags.StringVar(&location, "location", "", "Administrator-defined Infra/DC location (required)")
			flags.StringVar(&cidr, "cidr", "", "Canonical IPv4 network prefix, /8 through /30 (required)")
		}
	}
	if action == "delete" {
		flags.BoolVar(&await, "wait", false, "Wait for controller-owned cleanup and object deletion")
	}
	if action == "list" && kind == "subnet" {
		flags.StringVar(&vpc, "vpc", "", "Filter by VPC name in the selected project namespace")
	}
	flags.Usage = func() {
		_, _ = fmt.Fprintf(a.Out, "Usage: vpcctl [global flags] %s %s", kind, action)
		if action != "list" {
			_, _ = fmt.Fprint(a.Out, " NAME")
		}
		_, _ = fmt.Fprintln(a.Out, " [flags]\n\n"+flags.FlagUsages())
	}
	if err := flags.Parse(args[2:]); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil
		}
		return err
	}
	if opts.Namespace == "" {
		return fmt.Errorf("a project namespace is required; set --namespace or a kubeconfig context namespace")
	}
	if errs := validation.IsDNS1123Label(opts.Namespace); len(errs) != 0 {
		return fmt.Errorf("invalid project namespace %q: %s", opts.Namespace, strings.Join(errs, "; "))
	}
	if opts.Output != "" && opts.Output != "table" && opts.Output != "json" && opts.Output != "yaml" {
		return fmt.Errorf("unsupported output %q; use table, json, or yaml", opts.Output)
	}
	wantArgs := 1
	if action == "list" {
		wantArgs = 0
	}
	if flags.NArg() != wantArgs {
		return fmt.Errorf("%s %s requires %d name argument(s)", kind, action, wantArgs)
	}
	name := ""
	if wantArgs == 1 {
		name = flags.Arg(0)
		if err := resourceDNSName("name", name); err != nil {
			return err
		}
	}
	if vpc != "" || flags.Changed("vpc") {
		if err := resourceDNSName("VPC reference", vpc); err != nil {
			return err
		}
	}
	obj := resourceObject(kind, opts.Namespace, name)
	if action == "create" {
		if dryRun && await {
			return fmt.Errorf("--dry-run and --wait cannot be combined")
		}
		switch typed := obj.(type) {
		case *api.VPC:
			if err := resourceDNSName("network class", networkClass); err != nil {
				return err
			}
			typed.Spec.NetworkClassRef = networkClass
		case *api.Subnet:
			if err := resourceDNSName("VPC reference (--vpc)", vpc); err != nil {
				return err
			}
			if err := resourceDNSName("location (--location)", location); err != nil {
				return err
			}
			if _, err := siteconfig.Prefix(cidr); err != nil {
				return fmt.Errorf("invalid CIDR %q: %w", cidr, err)
			}
			typed.Spec = api.SubnetSpec{VPCRef: vpc, LocationRef: location, CIDR: cidr}
		}
		if dryRun {
			format := opts.Output
			if format == "" || format == "table" {
				format = "yaml"
			}
			return resourcePrint(a.Out, obj, format)
		}
	}
	c, err := a.client(opts)
	if err != nil {
		return err
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	key := client.ObjectKeyFromObject(obj)
	switch action {
	case "create":
		if err := c.Create(ctx, obj); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("%s %s/%s already exists; create does not overwrite existing intent: %w", kind, opts.Namespace, name, err)
			}
			return fmt.Errorf("create %s %s/%s: %w", kind, opts.Namespace, name, err)
		}
		if await {
			obj, err = resourceWait(ctx, c, kind, key, obj.GetUID(), false)
			if err != nil {
				return fmt.Errorf("%s %s/%s was created but readiness was not confirmed: %w", kind, opts.Namespace, name, err)
			}
		}
		return resourcePrint(a.Out, obj, opts.Output)
	case "list":
		if kind == "vpc" {
			list := &api.VPCList{TypeMeta: resourceTypeMeta("VPCList")}
			if err := c.List(ctx, list, client.InNamespace(opts.Namespace)); err != nil {
				return fmt.Errorf("list VPCs in %s: %w", opts.Namespace, err)
			}
			sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
			return resourcePrint(a.Out, list, opts.Output)
		}
		list := &api.SubnetList{TypeMeta: resourceTypeMeta("SubnetList")}
		if err := c.List(ctx, list, client.InNamespace(opts.Namespace)); err != nil {
			return fmt.Errorf("list Subnets in %s: %w", opts.Namespace, err)
		}
		if vpc != "" {
			filtered := make([]api.Subnet, 0, len(list.Items))
			for _, s := range list.Items {
				if s.Spec.VPCRef == vpc {
					filtered = append(filtered, s)
				}
			}
			list.Items = filtered
		}
		sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
		return resourcePrint(a.Out, list, opts.Output)
	case "get", "wait", "delete":
		if err := c.Get(ctx, key, obj); err != nil {
			return fmt.Errorf("get %s %s/%s: %w", kind, opts.Namespace, name, err)
		}
		if action == "wait" {
			obj, err = resourceWait(ctx, c, kind, key, obj.GetUID(), false)
			if err != nil {
				return err
			}
		}
		if action != "delete" {
			return resourcePrint(a.Out, obj, opts.Output)
		}
		if kind == "vpc" {
			var subnets api.SubnetList
			if err := c.List(ctx, &subnets, client.InNamespace(opts.Namespace)); err != nil {
				return fmt.Errorf("check VPC Subnet dependencies: %w", err)
			}
			var remaining []string
			for _, s := range subnets.Items {
				if s.Spec.VPCRef == name {
					remaining = append(remaining, s.Name)
				}
			}
			if len(remaining) > 0 {
				sort.Strings(remaining)
				return fmt.Errorf("VPC %s/%s still has Subnets (%s); delete them and wait for cleanup before deleting the VPC", opts.Namespace, name, strings.Join(remaining, ", "))
			}
		}
		uid, rv := obj.GetUID(), obj.GetResourceVersion()
		if uid == "" || rv == "" {
			return fmt.Errorf("refusing deletion without an observed UID and resourceVersion")
		}
		if err := c.Delete(ctx, obj, client.Preconditions{UID: &uid, ResourceVersion: &rv}); err != nil {
			return fmt.Errorf("delete %s %s/%s (identity guarded): %w", kind, opts.Namespace, name, err)
		}
		if await {
			if _, err := resourceWait(ctx, c, kind, key, uid, true); err != nil {
				return fmt.Errorf("deletion requested, but cleanup was not confirmed: %w", err)
			}
		}
		if opts.Output == "json" || opts.Output == "yaml" {
			return resourcePrint(a.Out, obj, opts.Output)
		}
		state := "deletion requested"
		if await {
			state = "deleted"
		}
		_, err := fmt.Fprintf(a.Out, "%s %s/%s %s\n", kind, opts.Namespace, name, state)
		return err
	}
	return nil
}

func (a *App) resourceHelp(kind string) error {
	_, err := fmt.Fprintf(a.Out, `Usage: vpcctl [global flags] %s COMMAND

Commands:
  create NAME [flags]  Create public network intent (--dry-run prints YAML)
  list [flags]         List resources in the selected project namespace
  get NAME             Show intent, controller status, and native mappings
  wait NAME            Wait for current-generation Ready status
  delete NAME [--wait] Request controller-owned cleanup; never force finalizers

Use COMMAND --help for command flags. Put global flags before %s.
Ready reflects controller status, not an independent packet reachability test.
`, kind, kind)
	return err
}

func resourceDNSName(field, value string) error {
	if errs := validation.IsDNS1123Subdomain(value); len(errs) != 0 {
		return fmt.Errorf("invalid %s %q: %s", field, value, strings.Join(errs, "; "))
	}
	return nil
}

func resourceTypeMeta(kind string) metav1.TypeMeta {
	return metav1.TypeMeta{APIVersion: api.GroupVersion.String(), Kind: kind}
}

func resourceObject(kind, namespace, name string) client.Object {
	metadata := metav1.ObjectMeta{Name: name, Namespace: namespace}
	if kind == "vpc" {
		return &api.VPC{TypeMeta: resourceTypeMeta("VPC"), ObjectMeta: metadata}
	}
	return &api.Subnet{TypeMeta: resourceTypeMeta("Subnet"), ObjectMeta: metadata}
}

func resourceStatus(obj client.Object) (int64, []metav1.Condition) {
	switch obj := obj.(type) {
	case *api.VPC:
		return obj.Status.ObservedGeneration, obj.Status.Conditions
	case *api.Subnet:
		return obj.Status.ObservedGeneration, obj.Status.Conditions
	default:
		return 0, nil
	}
}

func resourceReadiness(obj client.Object) (string, string) {
	if !obj.GetDeletionTimestamp().IsZero() {
		return "Terminating", "CleanupPending"
	}
	observed, conditions := resourceStatus(obj)
	ready := meta.FindStatusCondition(conditions, "Ready")
	if ready == nil {
		return "Pending", "NotReported"
	}
	if observed != obj.GetGeneration() || ready.ObservedGeneration != obj.GetGeneration() {
		return "Stale", ready.Reason
	}
	if ready.Status == metav1.ConditionTrue {
		return "Ready", ready.Reason
	}
	if ready.Status == metav1.ConditionUnknown {
		return "Unknown", ready.Reason
	}
	return "Pending", ready.Reason
}

func resourceWait(ctx context.Context, c client.Client, kind string, key client.ObjectKey, uid types.UID, deleted bool) (client.Object, error) {
	if uid == "" {
		return nil, fmt.Errorf("cannot wait without an observed resource UID")
	}
	var result client.Object
	lastState := "Pending"
	err := wait.PollUntilContextCancel(ctx, 500*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		obj := resourceObject(kind, key.Namespace, key.Name)
		if err := c.Get(ctx, key, obj); err != nil {
			if deleted && apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		}
		if obj.GetUID() != uid {
			return false, fmt.Errorf("%s %s/%s was replaced (UID changed); refusing to accept a different resource", kind, key.Namespace, key.Name)
		}
		result = obj
		state, reason := resourceReadiness(obj)
		lastState = state + " (" + reason + ")"
		if deleted {
			return false, nil
		}
		if !obj.GetDeletionTimestamp().IsZero() {
			return false, fmt.Errorf("%s %s/%s is terminating", kind, key.Namespace, key.Name)
		}
		return state == "Ready", nil
	})
	if err != nil {
		return nil, fmt.Errorf("wait for %s %s/%s; last status %s: %w", kind, key.Namespace, key.Name, lastState, err)
	}
	return result, nil
}

func resourcePrint(out io.Writer, obj any, format string) error {
	// Typed clients (including the fake API) can clear GVK during decoding.
	// Restore the public API identity so rendered output remains applyable.
	switch obj := obj.(type) {
	case *api.VPC:
		obj.TypeMeta = resourceTypeMeta("VPC")
	case *api.Subnet:
		obj.TypeMeta = resourceTypeMeta("Subnet")
	case *api.VPCList:
		obj.TypeMeta = resourceTypeMeta("VPCList")
		for i := range obj.Items {
			obj.Items[i].TypeMeta = resourceTypeMeta("VPC")
		}
	case *api.SubnetList:
		obj.TypeMeta = resourceTypeMeta("SubnetList")
		for i := range obj.Items {
			obj.Items[i].TypeMeta = resourceTypeMeta("Subnet")
		}
	}
	if format == "json" || format == "yaml" {
		data, err := json.MarshalIndent(obj, "", "  ")
		if err != nil {
			return err
		}
		if format == "yaml" {
			data, err = yaml.JSONToYAML(data)
			if err != nil {
				return err
			}
		}
		_, err = fmt.Fprintln(out, strings.TrimRight(string(data), "\n"))
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	var writeErr error
	line := func(text string, values ...any) {
		if writeErr == nil {
			_, writeErr = fmt.Fprintf(tw, text, values...)
		}
	}
	vpcHeader := func() { line("NAMESPACE\tNAME\tCLASS\tREADY\tREASON\tNATIVE-VPCS\n") }
	subnetHeader := func() { line("NAMESPACE\tNAME\tVPC\tLOCATION\tCIDR\tREADY\tREASON\tNATIVE-VPC\tNATIVE-SUBNET\n") }
	vpcRow := func(v *api.VPC) {
		state, reason := resourceReadiness(v)
		class := v.Spec.NetworkClassRef
		if class == "" {
			class = api.DefaultNetworkClass
		}
		var mappings []string
		for _, b := range v.Status.Bindings {
			if b.NativeVpcName != "" {
				mappings = append(mappings, b.LocationRef+"="+b.NativeVpcName)
			}
		}
		sort.Strings(mappings)
		line("%s\t%s\t%s\t%s\t%s\t%s\n", v.Namespace, v.Name, class, state, resourceDash(reason), resourceDash(strings.Join(mappings, ",")))
	}
	subnetRow := func(s *api.Subnet) {
		state, reason := resourceReadiness(s)
		line("%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Namespace, s.Name, s.Spec.VPCRef, s.Spec.LocationRef, s.Spec.CIDR, state, resourceDash(reason), resourceDash(s.Status.NativeVpcName), resourceDash(s.Status.NativeSubnetName))
	}
	switch obj := obj.(type) {
	case *api.VPC:
		vpcHeader()
		vpcRow(obj)
	case *api.VPCList:
		vpcHeader()
		for i := range obj.Items {
			vpcRow(&obj.Items[i])
		}
	case *api.Subnet:
		subnetHeader()
		subnetRow(obj)
	case *api.SubnetList:
		subnetHeader()
		for i := range obj.Items {
			subnetRow(&obj.Items[i])
		}
	default:
		return fmt.Errorf("unsupported resource output %T", obj)
	}
	if writeErr != nil {
		return writeErr
	}
	return tw.Flush()
}

func resourceDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
