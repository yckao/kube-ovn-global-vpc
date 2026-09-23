package vpcctl

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// nativeHookState is kept in Kubernetes, never on the administrator's machine.
// Pending is written before touching native objects, allowing failed Helm hooks
// to resume or unwind using the same guarded plan.
type nativeHookState struct {
	APIVersion string      `json:"apiVersion"`
	Kind       string      `json:"kind"`
	Release    string      `json:"release"`
	Active     *nativePlan `json:"active,omitempty"`
	Pending    *nativePlan `json:"pending,omitempty"`
}

type nativeHookOptions struct {
	Action, Release, Namespace, Secret, BundleFile string
	ControllerNamespace, Deployment, Container     string
	OVN                                            nativeOVNOptions
}

func (a *App) runNativeHook(ctx context.Context, args []string, opts Options) error {
	fs := flag.NewFlagSet("internal native-hook", flag.ContinueOnError)
	fs.SetOutput(a.ErrOut)
	h := nativeHookOptions{ControllerNamespace: "kube-system", Deployment: "kube-ovn-controller", Container: "kube-ovn-controller", OVN: defaultNativeOVNOptions()}
	fs.StringVar(&h.Action, "action", "", "Helm hook action: reconcile, uninstall, or verify")
	fs.StringVar(&h.Release, "release", "", "Owning Helm release name")
	fs.StringVar(&h.Namespace, "release-namespace", "", "Helm release and receipt namespace")
	fs.StringVar(&h.Secret, "state-secret", "", "Private Kubernetes receipt Secret")
	fs.StringVar(&h.BundleFile, "bundle", "", "Mounted native bundle JSON for this Helm revision")
	fs.StringVar(&h.ControllerNamespace, "controller-namespace", h.ControllerNamespace, "Existing native controller namespace")
	fs.StringVar(&h.Deployment, "controller-deployment", h.Deployment, "Existing native controller Deployment")
	fs.StringVar(&h.Container, "controller-container", h.Container, "Existing native controller container")
	fs.StringVar(&h.OVN.Namespace, "ovn-namespace", h.OVN.Namespace, "OVN NB client namespace")
	fs.StringVar(&h.OVN.Deployment, "ovn-deployment", h.OVN.Deployment, "OVN NB client Deployment")
	fs.StringVar(&h.OVN.Container, "ovn-container", h.OVN.Container, "OVN NB client container")
	fs.StringVar(&h.OVN.Database, "ovn-db", h.OVN.Database, "OVN NB endpoint inside the client container")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || (h.Action != "reconcile" && h.Action != "uninstall" && h.Action != "verify") {
		return fmt.Errorf("internal native-hook requires --action reconcile|uninstall|verify and no positional arguments")
	}
	if len(validation.IsDNS1123Subdomain(h.Release)) != 0 || len(h.Release) > 53 || len(validation.IsDNS1123Label(h.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(h.Secret)) != 0 {
		return fmt.Errorf("native hook release, release namespace, and state Secret names must be valid Kubernetes names")
	}
	if h.Action == "reconcile" && h.BundleFile == "" {
		return fmt.Errorf("native reconcile hook requires --bundle")
	}
	c, err := a.client(opts)
	if err != nil {
		return err
	}
	return a.nativeHook(ctx, c, opts, h)
}

func nativeLoadHookState(ctx context.Context, c client.Client, h nativeHookOptions) (*corev1.Secret, nativeHookState, error) {
	state := nativeHookState{APIVersion: nativeAPIVersion, Kind: "NativeHelmState", Release: h.Release}
	var secret corev1.Secret
	err := c.Get(ctx, client.ObjectKey{Namespace: h.Namespace, Name: h.Secret}, &secret)
	if apierrors.IsNotFound(err) {
		return nil, state, nil
	}
	if err != nil {
		return nil, state, err
	}
	if secret.Labels["globalvpc.io/native-receipt"] != "true" || secret.Annotations["meta.helm.sh/release-name"] != h.Release || secret.Annotations["meta.helm.sh/release-namespace"] != h.Namespace {
		return nil, state, fmt.Errorf("native receipt Secret exists but does not belong to this Helm release")
	}
	data := secret.Data["state.json"]
	if len(data) == 0 || len(data) > 900<<10 {
		return nil, state, fmt.Errorf("native receipt Secret has missing or oversized state")
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, state, fmt.Errorf("invalid native receipt Secret: %w", err)
	}
	if state.APIVersion != nativeAPIVersion || state.Kind != "NativeHelmState" || state.Release != h.Release {
		return nil, state, fmt.Errorf("native receipt Secret has incompatible identity")
	}
	for _, plan := range []*nativePlan{state.Active, state.Pending} {
		if plan == nil {
			continue
		}
		if err := validateNativePlan(*plan); err != nil {
			return nil, state, fmt.Errorf("native receipt Secret contains an invalid plan: %w", err)
		}
		if plan.Namespace != h.ControllerNamespace || plan.Deployment != h.Deployment || plan.Container != h.Container {
			return nil, state, fmt.Errorf("native controller identity differs from the release receipt; a Helm upgrade cannot retarget an installed extension")
		}
	}
	return &secret, state, nil
}

func nativeSaveHookState(ctx context.Context, c client.Client, h nativeHookOptions, secret **corev1.Secret, state nativeHookState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(data) > 900<<10 {
		return fmt.Errorf("native receipt chain is too large for a Kubernetes Secret; refusing mutation")
	}
	if *secret == nil {
		object := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: h.Secret, Namespace: h.Namespace, Labels: map[string]string{"globalvpc.io/native-receipt": "true", "app.kubernetes.io/managed-by": "Helm"}, Annotations: map[string]string{"meta.helm.sh/release-name": h.Release, "meta.helm.sh/release-namespace": h.Namespace, "helm.sh/resource-policy": "keep"}}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"state.json": data}}
		if err := c.Create(ctx, object); err != nil {
			return fmt.Errorf("persist native receipt before mutation: %w", err)
		}
		*secret = object
		return nil
	}
	object := (*secret).DeepCopy()
	object.Data = map[string][]byte{"state.json": data}
	if err := c.Update(ctx, object); err != nil {
		return fmt.Errorf("persist native receipt (another hook may have changed it): %w", err)
	}
	*secret = object
	return nil
}

func nativeReceiptHasImage(p *nativePlan, image string) bool {
	for p != nil {
		if p.Bundle.Image == image {
			return true
		}
		p = p.Previous
	}
	return false
}

func (a *App) nativeHook(ctx context.Context, c client.Client, opts Options, h nativeHookOptions) error {
	secret, state, err := nativeLoadHookState(ctx, c, h)
	if err != nil {
		return err
	}
	if h.Action == "uninstall" {
		plan := state.Pending
		if plan == nil {
			plan = state.Active
		}
		if plan == nil {
			crd := nativeCRD()
			if err := c.Get(ctx, client.ObjectKeyFromObject(crd), crd); err != nil {
				return err
			}
			props, err := nativeCRDProperties(crd)
			if err != nil {
				return err
			}
			for _, section := range []string{"spec", "status"} {
				if _, exists, _ := unstructured.NestedFieldNoCopy(props, section, "properties", "destinationRoutes"); exists {
					return fmt.Errorf("extension schema exists but no installation receipt is available; refusing to guess the original native image")
				}
			}
			fmt.Fprintln(a.Out, "No native extension receipt or schema is installed.")
		} else if err := a.restoreNative(ctx, c, *plan, opts, h.OVN, true, true); err != nil {
			return err
		}
		if secret != nil {
			if len(secret.Finalizers) != 0 {
				return fmt.Errorf("native restoration completed but receipt Secret has finalizers; they were not removed")
			}
			if err := c.Delete(ctx, secret, client.Preconditions{UID: &secret.UID, ResourceVersion: &secret.ResourceVersion}); err != nil {
				return fmt.Errorf("native restoration completed but deleting the receipt failed: %w", err)
			}
		}
		return nil
	}
	if h.Action == "verify" {
		if state.Pending != nil || state.Active == nil {
			return fmt.Errorf("native Helm installation has no completed receipt; retry the failed Helm operation first")
		}
		return a.nativeVerify(ctx, c, state.Active, h.ControllerNamespace, h.Deployment, h.Container, opts)
	}
	var bundle NativeBundle
	if err := nativeReadJSON(h.BundleFile, &bundle); err != nil {
		return err
	}
	if err := validateNativeBundle(bundle); err != nil {
		return err
	}
	// A failed previous upgrade can be retried or reversed by Helm rollback.
	// Persisted Pending always identifies the exact possible partial mutation.
	if state.Pending != nil {
		pending := state.Pending
		if pending.Bundle.Image == bundle.Image {
			if err := a.installNativePlan(ctx, c, *pending, opts, true); err != nil {
				return err
			}
			state.Active, state.Pending = pending, nil
			if err := nativeSaveHookState(ctx, c, h, &secret, state); err != nil {
				return err
			}
		} else if nativeReceiptHasImage(pending.Previous, bundle.Image) {
			if err := a.restoreNative(ctx, c, *pending, opts, h.OVN, true, false); err != nil {
				return err
			}
			state.Active, state.Pending = pending.Previous, nil
			if err := nativeSaveHookState(ctx, c, h, &secret, state); err != nil {
				return err
			}
		} else {
			return fmt.Errorf("a different native revision is partially applied; retry that Helm revision or roll back to a recorded previous image before changing targets")
		}
	}
	// Helm rollback chooses its revision's bundle, not a separate CLI history.
	// Repeated images can occur after rollback; choose the nearest matching receipt.
	for state.Active != nil && state.Active.Bundle.Image != bundle.Image && nativeReceiptHasImage(state.Active.Previous, bundle.Image) {
		state.Pending = state.Active
		if err := nativeSaveHookState(ctx, c, h, &secret, state); err != nil {
			return err
		}
		if err := a.restoreNative(ctx, c, *state.Pending, opts, h.OVN, true, false); err != nil {
			return err
		}
		state.Active, state.Pending = state.Pending.Previous, nil
		if err := nativeSaveHookState(ctx, c, h, &secret, state); err != nil {
			return err
		}
	}
	if state.Active != nil && state.Active.Bundle.Image == bundle.Image {
		if bundle.Target != state.Active.Bundle.Target || bundle.SourceCommit != state.Active.Bundle.SourceCommit || nativeImageReference(bundle.BaseImage) != nativeImageReference(state.Active.Bundle.BaseImage) {
			return fmt.Errorf("Helm revision bundle identity conflicts with the recorded immutable image")
		}
		return a.nativeVerify(ctx, c, state.Active, h.ControllerNamespace, h.Deployment, h.Container, opts)
	}
	var plan nativePlan
	if state.Active == nil {
		plan, err = makeNativePlan(ctx, c, opts.Context, h.ControllerNamespace, h.Deployment, h.Container, bundle)
	} else {
		plan, err = makeNativeUpgradePlan(ctx, c, opts, *state.Active, bundle)
	}
	if err != nil {
		return err
	}
	if len(plan.InstallBlockers) != 0 {
		return fmt.Errorf("native Helm installation blocked: %s", strings.Join(plan.InstallBlockers, "; "))
	}
	state.Pending = &plan
	if err := nativeSaveHookState(ctx, c, h, &secret, state); err != nil {
		return err
	}
	if err := a.installNativePlan(ctx, c, plan, opts, true); err != nil {
		return err
	}
	state.Active, state.Pending = &plan, nil
	if err := nativeSaveHookState(ctx, c, h, &secret, state); err != nil {
		return err
	}
	return a.nativeVerify(ctx, c, state.Active, h.ControllerNamespace, h.Deployment, h.Container, opts)
}
