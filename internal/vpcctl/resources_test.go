package vpcctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	api "globalvpc.io/controller/api/v1alpha2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"
)

func resourceTestClient(t *testing.T, objects ...client.Object) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&api.VPC{}, &api.Subnet{}, &api.NetworkBinding{}).
		WithObjects(objects...).Build()
}

func resourceTestApp(c client.Client) (*App, *bytes.Buffer) {
	out := new(bytes.Buffer)
	return &App{Out: out, ErrOut: new(bytes.Buffer), ClientFactory: func(Options) (client.Client, error) { return c, nil }}, out
}

func resourceTestOptions() Options {
	return Options{Namespace: "project-red", Timeout: time.Second}
}

func resourceTestVPC(name, namespace string) *api.VPC {
	return &api.VPC{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(namespace + "-" + name), Generation: 2}, Spec: api.VPCSpec{NetworkClassRef: "default"}}
}

func resourceTestSubnet(name, namespace, vpc string) *api.Subnet {
	return &api.Subnet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(namespace + "-" + name), Generation: 2}, Spec: api.SubnetSpec{VPCRef: vpc, LocationRef: "dc-a", CIDR: "10.241.0.0/24"}}
}

func resourceTestReady(generation int64) []metav1.Condition {
	return []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, ObservedGeneration: generation, Reason: "LocationsReady", LastTransitionTime: metav1.Now()}}
}

func TestResourcesAreProjectScoped(t *testing.T) {
	c := resourceTestClient(t,
		resourceTestVPC("red", "project-red"), resourceTestVPC("blue", "project-blue"),
		resourceTestSubnet("red-apps", "project-red", "red"), resourceTestSubnet("other-apps", "project-red", "other"), resourceTestSubnet("blue-apps", "project-blue", "red"))
	a, out := resourceTestApp(c)
	opts := resourceTestOptions()
	opts.Output = "json"
	if err := a.runResources(context.Background(), []string{"vpc", "list"}, opts); err != nil {
		t.Fatal(err)
	}
	var vpcs api.VPCList
	if err := json.Unmarshal(out.Bytes(), &vpcs); err != nil {
		t.Fatal(err)
	}
	if len(vpcs.Items) != 1 || vpcs.Items[0].Name != "red" || vpcs.APIVersion != api.GroupVersion.String() {
		t.Fatalf("unexpected scoped list: %+v", vpcs)
	}
	out.Reset()
	if err := a.runResources(context.Background(), []string{"subnet", "list", "--vpc", "red"}, opts); err != nil {
		t.Fatal(err)
	}
	var subnets api.SubnetList
	if err := json.Unmarshal(out.Bytes(), &subnets); err != nil {
		t.Fatal(err)
	}
	if len(subnets.Items) != 1 || subnets.Items[0].Name != "red-apps" {
		t.Fatalf("unexpected scoped/filter list: %+v", subnets.Items)
	}
	if err := a.runResources(context.Background(), []string{"vpc", "get", "blue"}, opts); !apierrors.IsNotFound(err) {
		t.Fatalf("cross-project get should not find blue VPC: %v", err)
	}
}

func TestResourceDryRunIsOfflineAndHasPublicIntent(t *testing.T) {
	for _, format := range []string{"table", "yaml", "json"} {
		t.Run(format, func(t *testing.T) {
			a, out := resourceTestApp(nil)
			a.ClientFactory = func(Options) (client.Client, error) {
				t.Fatal("dry-run must not contact Kubernetes or construct an API client")
				return nil, nil
			}
			opts := resourceTestOptions()
			opts.Output = format
			if err := a.runResources(context.Background(), []string{"subnet", "create", "apps", "--vpc", "red", "--location", "dc-a", "--cidr", "10.241.0.0/24", "--dry-run"}, opts); err != nil {
				t.Fatal(err)
			}
			var subnet api.Subnet
			if err := yaml.Unmarshal(out.Bytes(), &subnet); err != nil {
				t.Fatal(err)
			}
			if subnet.Kind != "Subnet" || subnet.APIVersion != api.GroupVersion.String() || subnet.Namespace != "project-red" || subnet.Spec.VPCRef != "red" || subnet.Spec.CIDR != "10.241.0.0/24" {
				t.Fatalf("unexpected dry-run: %s", out.String())
			}
		})
	}
}

func TestResourcesRejectInvalidInputBeforeAPIAccess(t *testing.T) {
	base := []string{"subnet", "create", "apps", "--vpc", "red", "--location", "dc-a", "--cidr"}
	var cases [][]string
	for _, cidr := range []string{"10.241.0.1/24", "10.241.0.0/31", "10.0.0.0/7", "2001:db8::/64", "invalid", "224.0.0.0/24", "0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16"} {
		cases = append(cases, append(append([]string(nil), base...), cidr))
	}
	cases = append(cases,
		[]string{"vpc", "create", "INVALID"},
		[]string{"vpc", "create", "red", "--network-class", ""},
		[]string{"vpc", "create", "red", "--dry-run", "--wait"},
		[]string{"vpc", "create", "red", "--unknown"},
		[]string{"vpc", "get", "red", "extra"},
		[]string{"vpc", "delete", "--all"},
		[]string{"subnet", "list", "--vpc", ""},
		[]string{"subnet", "create", "apps", "--cidr", "10.241.0.0/24"},
	)
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			a, _ := resourceTestApp(nil)
			a.ClientFactory = func(Options) (client.Client, error) { t.Fatal("invalid input contacted API"); return nil, nil }
			if err := a.runResources(context.Background(), args, resourceTestOptions()); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestResourceCreateDoesNotOverwriteExistingIntent(t *testing.T) {
	v := resourceTestVPC("red", "project-red")
	v.Spec.NetworkClassRef = "restricted"
	v.Labels = map[string]string{"keep": "true"}
	v.Finalizers = []string{api.Finalizer}
	v.Status.ObservedGeneration = 2
	v.Status.Conditions = resourceTestReady(2)
	c := resourceTestClient(t, v)
	a, _ := resourceTestApp(c)
	err := a.runResources(context.Background(), []string{"vpc", "create", "red"}, resourceTestOptions())
	if !apierrors.IsAlreadyExists(err) || !strings.Contains(err.Error(), "does not overwrite") {
		t.Fatalf("expected explicit create conflict, got %v", err)
	}
	var retained api.VPC
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(v), &retained); err != nil {
		t.Fatal(err)
	}
	if retained.Spec.NetworkClassRef != "restricted" || retained.Labels["keep"] != "true" || len(retained.Finalizers) != 1 || len(retained.Status.Conditions) != 1 {
		t.Fatalf("existing intent, metadata, or status changed: %+v", retained)
	}
}

func TestResourceCreateAndWaitUsesPublicIntentAndControllerStatus(t *testing.T) {
	for _, kind := range []string{"vpc", "subnet"} {
		t.Run(kind, func(t *testing.T) {
			c := resourceTestClient(t)
			wrapped := interceptor.NewClient(c, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, options ...client.CreateOption) error {
				if obj.GetNamespace() != "project-red" || obj.GetName() != "example" || len(obj.GetFinalizers()) != 0 || len(obj.GetOwnerReferences()) != 0 {
					t.Fatalf("unexpected public intent metadata: %+v", obj)
				}
				obj.SetUID("server-created-uid")
				obj.SetGeneration(1)
				if err := c.Create(ctx, obj, options...); err != nil {
					return err
				}
				// Simulate the controller's status acknowledgement through the
				// status subresource after the API has accepted public intent.
				observed := obj.DeepCopyObject().(client.Object)
				switch typed := observed.(type) {
				case *api.VPC:
					if typed.Spec.NetworkClassRef != "default" || len(typed.Status.Conditions) > 0 {
						t.Fatalf("unexpected VPC create body: %+v", typed)
					}
					typed.Status.ObservedGeneration = 1
					typed.Status.Conditions = resourceTestReady(1)
				case *api.Subnet:
					if typed.Spec != (api.SubnetSpec{VPCRef: "red", LocationRef: "dc-a", CIDR: "10.241.0.0/24"}) || len(typed.Status.Conditions) > 0 {
						t.Fatalf("unexpected Subnet create body: %+v", typed)
					}
					typed.Status.ObservedGeneration = 1
					typed.Status.Conditions = resourceTestReady(1)
				default:
					t.Fatalf("CLI created a non-public resource: %T", obj)
				}
				return c.Status().Update(ctx, observed)
			}})
			a, out := resourceTestApp(wrapped)
			args := []string{kind, "create", "example", "--wait"}
			if kind == "subnet" {
				args = append(args, "--vpc", "red", "--location", "dc-a", "--cidr", "10.241.0.0/24")
			}
			if err := a.runResources(context.Background(), args, resourceTestOptions()); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "Ready") || !strings.Contains(out.String(), "project-red") {
				t.Fatalf("missing acknowledgement/project: %s", out.String())
			}
		})
	}
}

func TestResourceWaitRequiresBothCurrentGenerationReceipts(t *testing.T) {
	for _, tt := range []struct {
		name      string
		observed  int64
		condition int64
		ready     bool
	}{
		{"current", 2, 2, true},
		{"stale-status", 1, 2, false},
		{"stale-condition", 2, 1, false},
		{"future-status", 3, 2, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			v := resourceTestVPC("red", "project-red")
			v.Status.ObservedGeneration = tt.observed
			v.Status.Conditions = resourceTestReady(tt.condition)
			a, out := resourceTestApp(resourceTestClient(t, v))
			opts := resourceTestOptions()
			opts.Timeout = 15 * time.Millisecond
			err := a.runResources(context.Background(), []string{"vpc", "wait", "red"}, opts)
			if tt.ready {
				if err != nil || !strings.Contains(out.String(), "Ready") {
					t.Fatalf("current readiness rejected: %v; %s", err, out.String())
				}
			} else if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "Stale") {
				t.Fatalf("stale receipt accepted or unhelpful timeout: %v", err)
			}
		})
	}
}

func TestResourceWaitPinsUIDAndHonorsCancellation(t *testing.T) {
	v := resourceTestVPC("red", "project-red")
	v.Status.ObservedGeneration = 2
	v.Status.Conditions = resourceTestReady(2)
	c := resourceTestClient(t, v)
	gets := 0
	wrapped := interceptor.NewClient(c, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if err := c.Get(ctx, key, obj, opts...); err != nil {
			return err
		}
		gets++
		if gets > 1 {
			obj.SetUID("replacement-uid")
		}
		return nil
	}})
	a, _ := resourceTestApp(wrapped)
	err := a.runResources(context.Background(), []string{"vpc", "wait", "red"}, resourceTestOptions())
	if err == nil || !strings.Contains(err.Error(), "UID changed") {
		t.Fatalf("same-name replacement accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = resourceWait(ctx, c, "vpc", client.ObjectKeyFromObject(v), v.UID, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
}

func TestVPCDeleteRefusesRemainingAndDrainingSubnets(t *testing.T) {
	for _, terminating := range []bool{false, true} {
		v := resourceTestVPC("red", "project-red")
		s := resourceTestSubnet("apps", "project-red", "red")
		if terminating {
			now := metav1.Now()
			s.DeletionTimestamp = &now
			s.Finalizers = []string{api.Finalizer}
		}
		c := resourceTestClient(t, v, s)
		a, _ := resourceTestApp(c)
		err := a.runResources(context.Background(), []string{"vpc", "delete", "red", "--wait"}, resourceTestOptions())
		if err == nil || !strings.Contains(err.Error(), "still has Subnets (apps)") {
			t.Fatalf("dependent subnet accepted, terminating=%v: %v", terminating, err)
		}
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(v), &api.VPC{}); err != nil {
			t.Fatalf("VPC was deleted: %v", err)
		}
	}
}

func TestResourceDeleteUsesIdentityAndVersionPreconditions(t *testing.T) {
	for _, concurrentUpdate := range []bool{false, true} {
		s := resourceTestSubnet("apps", "project-red", "red")
		c := resourceTestClient(t, s)
		guarded := false
		wrapped := interceptor.NewClient(c, interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, options ...client.DeleteOption) error {
			opts := (&client.DeleteOptions{}).ApplyOptions(options)
			if opts.Preconditions == nil || opts.Preconditions.UID == nil || opts.Preconditions.ResourceVersion == nil || *opts.Preconditions.UID != s.UID || *opts.Preconditions.ResourceVersion == "" {
				t.Fatalf("missing delete identity preconditions: %+v", opts)
			}
			guarded = true
			if concurrentUpdate {
				var changed api.Subnet
				if err := c.Get(ctx, client.ObjectKeyFromObject(s), &changed); err != nil {
					return err
				}
				changed.Labels = map[string]string{"new": "value"}
				if err := c.Update(ctx, &changed); err != nil {
					return err
				}
			}
			return c.Delete(ctx, obj, options...)
		}})
		a, out := resourceTestApp(wrapped)
		err := a.runResources(context.Background(), []string{"subnet", "delete", "apps", "--wait"}, resourceTestOptions())
		if !guarded {
			t.Fatal("delete did not carry preconditions")
		}
		if concurrentUpdate {
			if !apierrors.IsConflict(err) {
				t.Fatalf("concurrent change was not guarded: %v", err)
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(s), &api.Subnet{}); err != nil {
				t.Fatalf("concurrently changed subnet was deleted: %v", err)
			}
		} else if err != nil || !strings.Contains(out.String(), "deleted") {
			t.Fatalf("guarded deletion failed: %v, %s", err, out.String())
		}
	}
}

func TestResourceDeleteWaitDoesNotStripFinalizersOrAcceptReplacement(t *testing.T) {
	s := resourceTestSubnet("apps", "project-red", "red")
	s.Finalizers = []string{api.Finalizer}
	c := resourceTestClient(t, s)
	a, _ := resourceTestApp(c)
	opts := resourceTestOptions()
	opts.Timeout = 15 * time.Millisecond
	err := a.runResources(context.Background(), []string{"subnet", "delete", "apps", "--wait"}, opts)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pending cleanup should time out: %v", err)
	}
	var remaining api.Subnet
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(s), &remaining); err != nil {
		t.Fatal(err)
	}
	if len(remaining.Finalizers) != 1 || remaining.DeletionTimestamp.IsZero() {
		t.Fatalf("cleanup/finalizer ownership changed: %+v", remaining)
	}
	_, err = resourceWait(context.Background(), c, "subnet", client.ObjectKeyFromObject(s), "previous-uid", true)
	if err == nil || !strings.Contains(err.Error(), "UID changed") {
		t.Fatalf("deletion wait accepted replacement: %v", err)
	}
}

func TestResourceStatusOutputPreservesMappingsAndStaleness(t *testing.T) {
	s := resourceTestSubnet("apps", "project-red", "red")
	s.Status = api.SubnetStatus{ObservedGeneration: 1, NativeVpcName: "pv-red-a", NativeSubnetName: "ps-red-apps", Conditions: resourceTestReady(1)}
	a, out := resourceTestApp(resourceTestClient(t, s))
	opts := resourceTestOptions()
	if err := a.runResources(context.Background(), []string{"subnet", "get", "apps"}, opts); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Stale", "LocationsReady", "pv-red-a", "ps-red-apps"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("table misses %s: %s", want, out.String())
		}
	}
	out.Reset()
	opts.Output = "json"
	if err := a.runResources(context.Background(), []string{"subnet", "get", "apps"}, opts); err != nil {
		t.Fatal(err)
	}
	var got api.Subnet
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.ObservedGeneration != 1 || got.Status.NativeSubnetName != "ps-red-apps" || got.Status.Conditions[0].ObservedGeneration != 1 {
		t.Fatalf("JSON discarded original status: %s", out.String())
	}
}
