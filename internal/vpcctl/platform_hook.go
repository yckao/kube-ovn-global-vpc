package vpcctl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/pflag"
	api "globalvpc.io/controller/api/v1alpha2"
	"globalvpc.io/controller/internal/platformplan"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Chart hooks implement server-side resource guards. Helm remains the sole
// deployment lifecycle store; this helper neither adopts nor deletes workloads.
func (a *App) runPlatformHook(ctx context.Context, args []string, opts Options) error {
	f := pflag.NewFlagSet("platform-hook", pflag.ContinueOnError)
	f.SetOutput(a.ErrOut)
	var action, role, namespace, deployment, configMap, desiredConfig string
	f.StringVar(&action, "action", "", "check-empty, check-config or verify")
	f.StringVar(&role, "role", "", "authority or site")
	f.StringVar(&namespace, "namespace", opts.Namespace, "Chart release namespace")
	f.StringVar(&deployment, "deployment", "", "Chart controller Deployment name")
	f.StringVar(&configMap, "config-map", "", "Current chart configuration ConfigMap")
	f.StringVar(&desiredConfig, "desired-config", "", "Mounted proposed configuration JSON")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || (action != "check-empty" && action != "verify" && action != "check-config") || (role != "authority" && role != "site") || len(validation.IsDNS1123Label(namespace)) != 0 {
		return fmt.Errorf("invalid platform hook action, role or namespace")
	}
	c, err := a.client(opts)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	if action == "check-config" {
		if configMap == "" || desiredConfig == "" {
			return fmt.Errorf("check-config requires --config-map and --desired-config")
		}
		current := &corev1.ConfigMap{}
		if err = c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: configMap}, current); err != nil {
			return err
		}
		key := "platform.json"
		if role == "site" {
			key = "site.json"
		}
		data, err := os.ReadFile(desiredConfig)
		if err != nil {
			return err
		}
		if len(data) > 1<<20 {
			return fmt.Errorf("proposed configuration is too large")
		}
		var before, after map[string]any
		if err = json.Unmarshal([]byte(current.Data[key]), &before); err != nil {
			return fmt.Errorf("current chart configuration is invalid")
		}
		if err = json.Unmarshal(data, &after); err != nil {
			return fmt.Errorf("proposed chart configuration is invalid")
		}
		if before == nil || after == nil {
			return fmt.Errorf("chart configuration must be an object")
		}
		if role == "site" {
			delete(before, "gatewayImage")
			delete(after, "gatewayImage")
		}
		if nativeJSONEqual(before, after) {
			fmt.Fprintln(a.Out, "Existing allocation configuration is unchanged.")
			return nil
		}
		bindings := &api.NetworkBindingList{}
		listOptions := []client.ListOption{}
		if role == "site" {
			listOptions = append(listOptions, client.InNamespace(namespace))
		}
		if err = c.List(ctx, bindings, listOptions...); err != nil {
			return err
		}
		if len(bindings.Items) != 0 {
			if role == "site" && platformPendingBindings(bindings.Items, true) == 0 {
				return fmt.Errorf("configuration change blocked by retained allocation receipts; completed cleanup does not migrate location identities or reserved pools")
			}
			return fmt.Errorf("configuration change blocked while NetworkBindings exist; allocation or location changes require a separately reviewed migration")
		}
		if role == "authority" {
			v, s := &api.VPCList{}, &api.SubnetList{}
			if err = c.List(ctx, v); err != nil {
				return err
			}
			if err = c.List(ctx, s); err != nil {
				return err
			}
			if len(v.Items)+len(s.Items) != 0 {
				return fmt.Errorf("configuration change blocked while public resources exist; drain first")
			}
		}
		fmt.Fprintln(a.Out, "No managed intent remains. Configuration change allowed; retained allocation receipts are not migrated or released.")
		return nil
	}
	if action == "check-empty" {
		if role == "authority" {
			v, s := &api.VPCList{}, &api.SubnetList{}
			if err = c.List(ctx, v); err != nil {
				return err
			}
			if err = c.List(ctx, s); err != nil {
				return err
			}
			if len(v.Items)+len(s.Items) > 0 {
				return fmt.Errorf("uninstall blocked: %d VPCs and %d Subnets remain; remove workloads, then use vpcctl drain for their projects", len(v.Items), len(s.Items))
			}
		}
		bindings := &api.NetworkBindingList{}
		listOpts := []client.ListOption{}
		if role == "site" {
			listOpts = append(listOpts, client.InNamespace(namespace))
		}
		if err = c.List(ctx, bindings, listOpts...); err != nil {
			return err
		}
		if pending := platformPendingBindings(bindings.Items, role == "site"); pending > 0 {
			return fmt.Errorf("uninstall blocked: %d NetworkBindings still require controller cleanup", pending)
		}
		if role == "site" {
			pods := &corev1.PodList{}
			if err = c.List(ctx, pods, client.InNamespace(namespace), client.MatchingLabels{"platform.globalvpc.io/component": "gateway"}); err != nil {
				return err
			}
			if len(pods.Items) > 0 {
				return fmt.Errorf("uninstall blocked: %d managed gateway Pods remain", len(pods.Items))
			}
		}
		fmt.Fprintf(a.Out, "Controller cleanup is complete; Helm may uninstall this component. Retaining %d acknowledged local cleanup snapshots, allocation receipts and CRDs. Stop new intent until uninstall completes.\n", len(bindings.Items))
		return nil
	}
	if deployment == "" {
		return fmt.Errorf("verify requires --deployment")
	}
	d := &appsv1.Deployment{}
	if err = c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: deployment}, d); err != nil {
		return err
	}
	i, err := nativeContainerIndex(d, "controller")
	if err != nil {
		return err
	}
	ready, reason, err := nativeRolloutReady(ctx, c, d, "controller", d.Spec.Template.Spec.Containers[i].Image)
	if err != nil {
		return err
	}
	if !ready {
		return fmt.Errorf("controller verification failed: %s", reason)
	}
	if role == "authority" {
		v, s := &api.VPCList{}, &api.SubnetList{}
		if err = c.List(ctx, v); err != nil {
			return err
		}
		if err = c.List(ctx, s); err != nil {
			return err
		}
		for _, o := range v.Items {
			r := doctorObservation("VPC", o.ObjectMeta, o.Status.Phase, o.Status.ObservedGeneration, o.Status.Conditions)
			if !r.Ready {
				return fmt.Errorf("VPC %s/%s is not currently Ready: %s", o.Namespace, o.Name, r.Reason)
			}
		}
		for _, o := range s.Items {
			r := doctorObservation("Subnet", o.ObjectMeta, o.Status.Phase, o.Status.ObservedGeneration, o.Status.Conditions)
			if !r.Ready {
				return fmt.Errorf("Subnet %s/%s is not currently Ready: %s", o.Namespace, o.Name, r.Reason)
			}
		}
	} else {
		bindings := &api.NetworkBindingList{}
		if err = c.List(ctx, bindings, client.InNamespace(namespace)); err != nil {
			return err
		}
		for _, binding := range bindings.Items {
			if platformCompletedBinding(binding) {
				fmt.Fprintf(a.Out, "NetworkBinding %s/%s: retained acknowledged cleanup snapshot.\n", binding.Namespace, binding.Name)
				continue
			}
			r := doctorObservation("NetworkBinding", binding.ObjectMeta, binding.Status.Phase, binding.Status.ObservedGeneration, binding.Status.Conditions)
			if !r.Ready || binding.Status.AppliedRevision != binding.Spec.Revision || binding.Spec.Deleting {
				return fmt.Errorf("NetworkBinding %s/%s has unconfirmed current configuration: %s", binding.Namespace, binding.Name, r.Reason)
			}
		}
	}
	fmt.Fprintln(a.Out, "Controller rollout and available configuration observations verified. This does not establish tenant packet forwarding.")
	return nil
}

// The site syncer deliberately retains terminal snapshots when their authority
// objects disappear. A Deleted phase alone is insufficient: require the current
// accepted intent, matching status generation/hash, and explicit cleanup result.
// These snapshots and their allocation receipts remain untouched by Helm hooks.
func platformCompletedBinding(binding api.NetworkBinding) bool {
	if binding.UID == "" || binding.Generation < 1 || binding.Annotations["platform.globalvpc.io/source-binding-uid"] == "" ||
		binding.Spec.VPCRef.UID == "" || binding.Spec.LocationRef == "" || binding.Name != platformplan.BindingName(binding.Spec.VPCRef.UID, binding.Spec.LocationRef) ||
		!binding.Spec.Deleting || binding.Status.Phase != "Deleted" || binding.Spec.Revision != platformplan.Revision(binding.Spec) ||
		binding.Status.AppliedRevision != binding.Spec.Revision || binding.Status.ObservedGeneration != binding.Generation ||
		len(binding.Status.Resources) != 0 || len(binding.Status.Gateways) != 0 || len(binding.Finalizers) != 0 || binding.DeletionTimestamp != nil {
		return false
	}
	readyCount := 0
	for _, condition := range binding.Status.Conditions {
		if condition.Type != "Ready" {
			continue
		}
		readyCount++
		if condition.Status != metav1.ConditionFalse || condition.Reason != "CleanupComplete" || condition.ObservedGeneration != binding.Generation {
			return false
		}
	}
	return readyCount == 1
}

func platformPendingBindings(bindings []api.NetworkBinding, allowLocalReceipts bool) int {
	pending := 0
	for _, binding := range bindings {
		if !allowLocalReceipts || !platformCompletedBinding(binding) {
			pending++
		}
	}
	return pending
}

// Drain is an explicit data operation on public resources, separate from Helm's
// deployment lifecycle. It never deletes guest workloads or strips finalizers.
func (a *App) runDrain(ctx context.Context, args []string, opts Options) error {
	f := pflag.NewFlagSet("drain", pflag.ContinueOnError)
	f.SetOutput(a.ErrOut)
	var projects []string
	f.StringArrayVar(&projects, "project", nil, "Project namespace to drain; repeat for several projects (defaults to -n)")
	f.Usage = func() {
		fmt.Fprintln(a.Out, "Usage: vpcctl [global flags] drain --project PROJECT [--project PROJECT]\nRemove guest workloads first. This command deletes all public Subnets then VPCs in the explicitly selected projects, waiting for normal controller cleanup. No finalizers are removed.")
	}
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("drain takes only --project options")
	}
	if len(projects) == 0 && opts.Namespace != "" {
		projects = []string{opts.Namespace}
	}
	if len(projects) == 0 {
		return fmt.Errorf("select a project with --project or global -n")
	}
	seen := map[string]bool{}
	for _, p := range projects {
		if len(validation.IsDNS1123Label(p)) != 0 || seen[p] {
			return fmt.Errorf("projects must be distinct valid namespaces")
		}
		seen[p] = true
	}
	c, err := a.client(opts)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	for _, kind := range []string{"subnet", "vpc"} {
		var selected []client.Object
		for _, namespace := range projects {
			if kind == "subnet" {
				l := &api.SubnetList{}
				if err = c.List(ctx, l, client.InNamespace(namespace)); err != nil {
					return err
				}
				for i := range l.Items {
					selected = append(selected, &l.Items[i])
				}
			} else {
				l := &api.VPCList{}
				if err = c.List(ctx, l, client.InNamespace(namespace)); err != nil {
					return err
				}
				for i := range l.Items {
					selected = append(selected, &l.Items[i])
				}
			}
		}
		for _, o := range selected {
			if err = ctx.Err(); err != nil {
				return err
			}
			if o.GetDeletionTimestamp() != nil {
				continue
			}
			if kind == "vpc" {
				children := &api.SubnetList{}
				if err = c.List(ctx, children, client.InNamespace(o.GetNamespace())); err != nil {
					return err
				}
				for _, s := range children.Items {
					if s.Spec.VPCRef == o.GetName() {
						return fmt.Errorf("new Subnet %s/%s appeared during drain; stop new intent before retrying", s.Namespace, s.Name)
					}
				}
			}
			uid, rv := o.GetUID(), o.GetResourceVersion()
			if err = c.Delete(ctx, o, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}}); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
		for _, o := range selected {
			if _, err = resourceWait(ctx, c, kind, client.ObjectKeyFromObject(o), types.UID(o.GetUID()), true); err != nil {
				return fmt.Errorf("%s %s/%s cleanup remains pending; keep controllers running and remove attached workloads: %w", kind, o.GetNamespace(), o.GetName(), err)
			}
		}
	}
	for _, namespace := range projects {
		v, s := &api.VPCList{}, &api.SubnetList{}
		if err = c.List(ctx, v, client.InNamespace(namespace)); err != nil {
			return err
		}
		if err = c.List(ctx, s, client.InNamespace(namespace)); err != nil {
			return err
		}
		if len(v.Items)+len(s.Items) > 0 {
			return fmt.Errorf("new public resources appeared during drain in %s; stop new intent before retrying", namespace)
		}
	}
	fmt.Fprintf(a.Out, "Drained public resources in %s. Retained allocation receipts; Helm uninstall hooks independently check remaining bindings and gateways.\n", strings.Join(projects, ", "))
	return nil
}
