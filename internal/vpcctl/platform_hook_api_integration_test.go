//go:build integration

package vpcctl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "globalvpc.io/controller/api/v1alpha2"
	"globalvpc.io/controller/internal/platformplan"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestPlatformHookAPI proves current cleanup receipt recognition against real
// CRD admission/status generation and the hook's read-only namespace RBAC. No
// controller or dataplane is started, and all reported statuses are fixtures.
func TestPlatformHookAPI(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS must reference envtest binaries")
	}
	environment := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd")}, ErrorIfCRDPathMissing: true, ControlPlaneStartTimeout: 45 * time.Second}
	cfg, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{api.AddToScheme, corev1.AddToScheme, rbacv1.AddToScheme} {
		if err = add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"operator-system", "other-site"} {
		if err = admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "site-lifecycle", Namespace: "operator-system"}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{api.GroupVersion.Group}, Resources: []string{"networkbindings"}, Verbs: []string{"get", "list"}}, {APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}}}}
	if err = admin.Create(ctx, role); err != nil {
		t.Fatal(err)
	}
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: role.Name, Namespace: role.Namespace}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name}, Subjects: []rbacv1.Subject{{Kind: "User", APIGroup: rbacv1.GroupName, Name: "lifecycle-reader"}}}
	if err = admin.Create(ctx, binding); err != nil {
		t.Fatal(err)
	}
	user, err := environment.AddUser(envtest.User{Name: "lifecycle-reader", Groups: []string{"system:authenticated"}}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := client.New(user.Config(), client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err = wait.PollUntilContextTimeout(ctx, 20*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		return reader.List(ctx, &api.NetworkBindingList{}, client.InNamespace(role.Namespace)) == nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	receipt := platformTerminalFixture()
	receipt.UID = ""
	receipt.Generation = 0
	status := receipt.Status
	receipt.Status = api.NetworkBindingStatus{}
	if err = admin.Create(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	status.ObservedGeneration = receipt.Generation
	status.Conditions[0].ObservedGeneration = receipt.Generation
	receipt.Status = status
	if err = admin.Status().Update(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	// Another site's unresolved binding cannot be observed through this hook role.
	other := receipt.DeepCopy()
	other.Namespace = "other-site"
	other.UID = ""
	other.ResourceVersion = ""
	other.Generation = 0
	other.Spec.VPCRef.UID = "another-vpc"
	other.Name = platformplan.BindingName(other.Spec.VPCRef.UID, other.Spec.LocationRef)
	other.Spec.Revision = platformplan.Revision(other.Spec)
	other.Status = api.NetworkBindingStatus{}
	if err = admin.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, err = platformRunHook(t, reader, "site", "check-empty"); err != nil {
		t.Fatalf("admitted terminal receipt blocked scoped hook: %v", err)
	}
	if err = reader.List(ctx, &api.NetworkBindingList{}, client.InNamespace("other-site")); !apierrors.IsForbidden(err) {
		t.Fatalf("site hook could read another site: %v", err)
	}
	current := &api.NetworkBinding{}
	if err = admin.Get(ctx, client.ObjectKeyFromObject(receipt), current); err != nil {
		t.Fatal(err)
	}
	if current.UID != receipt.UID || current.Status.Phase != "Deleted" {
		t.Fatal("hook changed retained receipt")
	}
	if err = reader.Delete(ctx, current); !apierrors.IsForbidden(err) {
		t.Fatalf("hook identity could delete cleanup receipts: %v", err)
	}
	current.Status.Conditions[0].ObservedGeneration = 0
	if err = admin.Status().Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	if _, err = platformRunHook(t, reader, "site", "check-empty"); err == nil || !strings.Contains(err.Error(), "still require controller cleanup") {
		t.Fatalf("stale admitted cleanup status allowed uninstall: %v", err)
	}
}
