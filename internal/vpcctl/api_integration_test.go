//go:build integration

package vpcctl

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "globalvpc.io/controller/api/v1alpha2"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestCLIProjectAPI exercises real API admission, project RBAC and finalizers.
// It does not start the platform/native controllers or claim packet forwarding.
func TestCLIProjectAPI(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS must reference installed envtest binaries")
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{api.AddToScheme, corev1.AddToScheme, rbacv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cli-project", "other-project"} {
		if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: "cli-project", Name: "project-editor"}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{api.GroupVersion.Group}, Resources: []string{"vpcs", "subnets"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}}}}
	if err := admin.Create(ctx, role); err != nil {
		t.Fatal(err)
	}
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: role.Namespace, Name: role.Name}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name}, Subjects: []rbacv1.Subject{{Kind: "User", Name: "cli-user", APIGroup: rbacv1.GroupName}}}
	if err := admin.Create(ctx, binding); err != nil {
		t.Fatal(err)
	}
	user, err := environment.AddUser(envtest.User{Name: "cli-user", Groups: []string{"system:authenticated"}}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	project, err := client.New(user.Config(), client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		err := project.List(ctx, &api.VPCList{}, client.InNamespace(role.Namespace))
		return err == nil, nil
	}); err != nil {
		t.Fatal("project RBAC did not converge:", err)
	}
	var output bytes.Buffer
	app := App{Out: &output, ClientFactory: func(Options) (client.Client, error) { return project, nil }}
	run := func(args ...string) error {
		output.Reset()
		return app.Run(ctx, append([]string{"-n", role.Namespace}, args...))
	}
	if err := run("vpc", "create", "production"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Pending") {
		t.Fatal("unreconciled create did not report Pending:", output.String())
	}
	if err := run("subnet", "create", "apps", "--vpc", "production", "--location", "dc-a", "--cidr", "10.60.1.0/24"); err != nil {
		t.Fatal(err)
	}
	if err := run("vpc", "delete", "production"); err == nil || !strings.Contains(err.Error(), "Subnets") {
		t.Fatalf("VPC deletion ignored live Subnet: %v", err)
	}
	if err := run("-o", "json", "subnet", "get", "apps"); err != nil || !strings.Contains(output.String(), `"cidr": "10.60.1.0/24"`) {
		t.Fatalf("CLI did not round-trip admitted resource: %v %s", err, output.String())
	}
	if err := app.Run(ctx, []string{"-n", "other-project", "vpc", "list"}); err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("project client could read another project: %v", err)
	}
	if err := project.List(ctx, &api.NetworkBindingList{}, client.InNamespace(role.Namespace)); !apierrors.IsForbidden(err) {
		t.Fatalf("tenant could access internal bindings: %v", err)
	}
	vpc := &api.VPC{}
	if err := admin.Get(ctx, client.ObjectKey{Namespace: role.Namespace, Name: "production"}, vpc); err != nil {
		t.Fatal(err)
	}
	vpc.Status = api.VPCStatus{ObservedGeneration: vpc.Generation, Phase: "Ready", Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, ObservedGeneration: vpc.Generation, Reason: "FixtureApplied", Message: "API test fixture only", LastTransitionTime: metav1.Now()}}}
	if err := project.Status().Update(ctx, vpc); !apierrors.IsForbidden(err) {
		t.Fatalf("tenant could forge readiness: %v", err)
	}
	if err := admin.Status().Update(ctx, vpc); err != nil {
		t.Fatal(err)
	}
	if err := run("vpc", "wait", "production"); err != nil {
		t.Fatalf("current readiness did not satisfy CLI: %v", err)
	}
	subnet := &api.Subnet{}
	key := client.ObjectKey{Namespace: role.Namespace, Name: "apps"}
	if err := admin.Get(ctx, key, subnet); err != nil {
		t.Fatal(err)
	}
	subnet.Finalizers = []string{api.Finalizer}
	if err := admin.Update(ctx, subnet); err != nil {
		t.Fatal(err)
	}
	if err := run("--timeout", "100ms", "subnet", "delete", "apps", "--wait"); err == nil {
		t.Fatal("CLI reported deletion while a finalizer still held the Subnet")
	}
	if err := admin.Get(ctx, key, subnet); err != nil || subnet.DeletionTimestamp.IsZero() || len(subnet.Finalizers) != 1 {
		t.Fatalf("CLI bypassed finalizer: %v %+v", err, subnet)
	}
	// Only the administrator fixture simulates completion of owner cleanup.
	subnet.Finalizers = nil
	if err := admin.Update(ctx, subnet); err != nil {
		t.Fatal(err)
	}
	if err := run("vpc", "delete", "production", "--wait"); err != nil {
		t.Fatal(err)
	}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(vpc), &api.VPC{}); !apierrors.IsNotFound(err) {
		t.Fatalf("VPC still present after guarded deletion: %v", err)
	}
}
