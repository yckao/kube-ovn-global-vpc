package vpcctl

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func nativeHookFixture(t *testing.T) (client.WithWatch, *App, nativeHookOptions) {
	t.Helper()
	ns, crd, deployment := nativeTestObjects()
	set, pod := nativeTestReadyObjects(deployment)
	base := nativeTestClient(t, ns, crd, deployment, set, pod)
	nativeLifecycleOVNFixture(t, base)
	c := nativeLifecycleRolloutClient(t, base)
	a, _ := nativeTestApp(c)
	a.PodExecutor = func(_ context.Context, _ Options, _, _, _ string, _ []string, stdout, _ io.Writer) error {
		_, err := io.WriteString(stdout, `{"headings":["_uuid","external_ids"],"data":[]}`)
		return err
	}
	bundleFile := filepath.Join(t.TempDir(), "bundle.json")
	if err := nativeWritePrivateJSON(bundleFile, nativeTestBundle()); err != nil {
		t.Fatal(err)
	}
	h := nativeHookOptions{Action: "reconcile", Release: "native", Namespace: "global-vpc-system", Secret: "native-state", BundleFile: bundleFile, ControllerNamespace: deployment.Namespace, Deployment: deployment.Name, Container: "kube-ovn-controller", OVN: defaultNativeOVNOptions()}
	return c, a, h
}

func TestNativeHelmHooksInstallUpgradeRollbackUninstall(t *testing.T) {
	c, a, h := nativeHookFixture(t)
	ctx := context.Background()
	opts := Options{Timeout: time.Second}
	if err := a.nativeHook(ctx, c, opts, h); err != nil {
		t.Fatal(err)
	}
	secret, state, err := nativeLoadHookState(ctx, c, h)
	if err != nil || secret == nil || state.Pending != nil || state.Active == nil || state.Active.Previous != nil {
		t.Fatalf("installation receipt not completed: %#v %v", state, err)
	}
	if secret.Annotations["helm.sh/resource-policy"] != "keep" {
		t.Fatal("receipt must survive failed Helm deletion")
	}
	originalBundle := h.BundleFile
	newBundle := nativeTestBundle()
	newBundle.Image = "registry.example/kube-ovn@sha256:" + strings.Repeat("c", 64)
	h.BundleFile = filepath.Join(t.TempDir(), "upgrade.json")
	if err := nativeWritePrivateJSON(h.BundleFile, newBundle); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, nativeLifecycleACK(t, true)); err != nil {
		t.Fatal(err)
	}
	if err := a.nativeHook(ctx, c, opts, h); err != nil {
		t.Fatal(err)
	}
	_, state, err = nativeLoadHookState(ctx, c, h)
	if err != nil || state.Active == nil || state.Active.Bundle.Image != newBundle.Image || state.Active.Previous == nil {
		t.Fatalf("upgrade did not preserve rollback receipt: %#v %v", state, err)
	}
	// Helm's earlier revision supplies its bundle; no separate CLI revision is used.
	h.BundleFile = originalBundle
	conflictingRollback := interceptor.NewClient(c, interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
		po := &client.PatchOptions{}
		for _, option := range options {
			option.ApplyToPatch(po)
		}
		if _, deployment := object.(*appsv1.Deployment); deployment && len(po.DryRun) == 0 {
			return errors.New("injected rollback image conflict")
		}
		return c.Patch(ctx, object, patch, options...)
	}})
	if err := a.nativeHook(ctx, conflictingRollback, opts, h); err == nil || !strings.Contains(err.Error(), "image patch failed") {
		t.Fatalf("expected guarded rollback failure: %v", err)
	}
	_, state, err = nativeLoadHookState(ctx, c, h)
	if err != nil || state.Pending == nil || state.Active.Bundle.Image != newBundle.Image {
		t.Fatalf("failed rollback lost the current and pending recovery receipts: %#v %v", state, err)
	}
	// A Helm retry completes the persisted reverse transition with the same guards.
	if err := a.nativeHook(ctx, c, opts, h); err != nil {
		t.Fatal(err)
	}
	_, state, err = nativeLoadHookState(ctx, c, h)
	if err != nil || state.Pending != nil || state.Active.Bundle.Image != nativeTestBundle().Image || state.Active.Previous != nil {
		t.Fatalf("Helm rollback did not restore earlier receipt: %#v %v", state, err)
	}
	h.Action = "uninstall"
	if err := a.nativeHook(ctx, c, Options{Timeout: time.Millisecond}, h); err == nil {
		t.Fatal("uninstall with active route intent must fail")
	}
	var retained corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: h.Namespace, Name: h.Secret}, &retained); err != nil {
		t.Fatalf("failed uninstall lost its private recovery receipt: %v", err)
	}
	vpc := nativeLifecycleACK(t, true)
	if err := c.Delete(ctx, vpc); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, nativeLifecycleACK(t, false)); err != nil {
		t.Fatal(err)
	}
	if err := a.nativeHook(ctx, c, opts, h); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: h.Namespace, Name: h.Secret}, &retained); !apierrors.IsNotFound(err) {
		t.Fatalf("successful native uninstall did not remove receipt: %v", err)
	}
	if err := a.nativeHook(ctx, c, opts, h); err != nil {
		t.Fatalf("completed uninstall cannot be retried: %v", err)
	}
}

func TestNativeHelmHookPersistsReceiptBeforeMutationAndRetriesPartialInstall(t *testing.T) {
	base, a, h := nativeHookFixture(t)
	ctx := context.Background()
	failed := false
	c := interceptor.NewClient(base, interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
		po := &client.PatchOptions{}
		for _, option := range options {
			option.ApplyToPatch(po)
		}
		if len(po.DryRun) == 0 {
			secret, state, err := nativeLoadHookState(ctx, c, h)
			if err != nil || secret == nil || state.Pending == nil {
				t.Fatalf("mutation occurred without durable pending receipt: state=%#v error=%v", state, err)
			}
			if _, deployment := object.(*appsv1.Deployment); deployment && !failed {
				failed = true
				return errors.New("injected image patch conflict")
			}
		}
		return c.Patch(ctx, object, patch, options...)
	}})
	if err := a.nativeHook(ctx, c, Options{Timeout: time.Second}, h); err == nil || !strings.Contains(err.Error(), "PARTIAL INSTALL") {
		t.Fatalf("expected a resumable partial installation: %v", err)
	}
	_, state, err := nativeLoadHookState(ctx, base, h)
	if err != nil || state.Active != nil || state.Pending == nil {
		t.Fatalf("failed hook did not keep pending receipt: %#v %v", state, err)
	}
	if err := a.nativeHook(ctx, base, Options{Timeout: time.Second}, h); err != nil {
		t.Fatalf("Helm retry failed: %v", err)
	}
	_, state, err = nativeLoadHookState(ctx, base, h)
	if err != nil || state.Active == nil || state.Pending != nil {
		t.Fatalf("retry did not complete receipt: %#v %v", state, err)
	}
}

func TestNativeHelmHookRefusesForeignSecretAndControllerRetarget(t *testing.T) {
	for _, scenario := range []string{"foreign-secret", "controller-retarget"} {
		t.Run(scenario, func(t *testing.T) {
			c, a, h := nativeHookFixture(t)
			if scenario == "foreign-secret" {
				if err := c.Create(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: h.Secret, Namespace: h.Namespace}}); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := a.nativeHook(context.Background(), c, Options{Timeout: time.Second}, h); err != nil {
					t.Fatal(err)
				}
				h.Deployment = "different-controller"
			}
			if err := a.nativeHook(context.Background(), c, Options{Timeout: time.Second}, h); err == nil {
				t.Fatal("hook accepted receipt identity drift")
			}
		})
	}
}
