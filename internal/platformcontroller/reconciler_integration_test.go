//go:build integration

package platformcontroller

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	api "globalvpc.io/controller/api/v1alpha2"
	"globalvpc.io/controller/internal/platformplan"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// The authority uses a real API server, CRDs, finalizers and durable status CAS.
// Local site application is represented only by explicit test status reports;
// no OVN database, gateway, physical NIC or cross-site dataplane is exercised.
func TestManagedAuthorityWithRealAdmissionAndOfflineLocation(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS must reference installed envtest binaries")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	e := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd")}, ErrorIfCRDPathMissing: true, ControlPlaneStartTimeout: 45 * time.Second, ControlPlaneStopTimeout: 20 * time.Second}
	cfg, err := e.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	if err = api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err = corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"project", "location-a", "location-b", "global-vpc-system"} {
		if err = c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	v := publicVPC("red")
	v.UID = ""
	if err = c.Create(ctx, v); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*api.Subnet{publicSubnet("aa-apps", "red", "dc-a", "10.241.0.0/24"), publicSubnet("db", "red", "dc-a", "10.241.1.0/24"), publicSubnet("remote", "red", "dc-b", "10.242.0.0/24"), publicSubnet("zz-overlap", "red", "dc-b", "10.241.0.128/25")} {
		s.UID = ""
		if err = c.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	h := &authorityHarness{t: t, c: c, vpc: client.ObjectKeyFromObject(v), r: &Reconciler{Client: c, Config: authorityConfig()}}
	h.run(40)
	if len(h.bindings()) != 2 || len(h.binding("dc-a").Spec.Subnets) != 2 || len(h.network().Status.SubnetClaims) != 3 {
		t.Fatal("real admission lost multiple-subnet or location snapshots")
	}
	if h.subnet("zz-overlap").Status.Phase != "Blocked" || h.subnet("zz-overlap").Status.VPCUID != "" {
		t.Fatal("real authority admitted an overlapping same-VPC CIDR")
	}
	if h.network().Status.NetworkID == 0 || h.binding("dc-a").Spec.NetworkID != h.network().Status.NetworkID || h.binding("dc-a").Spec.LocalASN == 0 {
		t.Fatal("durable transport identity was not allocated")
	}
	h.ack("dc-a")
	h.run(6)
	if !meta.IsStatusConditionTrue(h.subnet("aa-apps").Status.Conditions, "Ready") || meta.IsStatusConditionTrue(h.network().Status.Conditions, "Ready") {
		t.Fatal("offline location either blocked local application or was falsely reported ready")
	}
	blue := publicVPC("blue")
	blue.UID = ""
	if err = c.Create(ctx, blue); err != nil {
		t.Fatal(err)
	}
	bs := publicSubnet("blue-apps", "blue", "dc-a", "10.241.0.0/24")
	bs.UID = ""
	if err = c.Create(ctx, bs); err != nil {
		t.Fatal(err)
	}
	h.vpc = client.ObjectKeyFromObject(blue)
	h.run(25)
	if len(h.network().Status.SubnetClaims) != 1 || h.subnet("blue-apps").Status.VPCUID != string(blue.UID) {
		t.Fatal("address reuse in a different VPC was rejected")
	}
	h.vpc = client.ObjectKeyFromObject(v)
	if err = c.Delete(ctx, h.subnet("zz-overlap")); err != nil {
		t.Fatal(err)
	}
	h.run(5)
	if err = c.Delete(ctx, h.subnet("aa-apps")); err != nil {
		t.Fatal(err)
	}
	h.run(12)
	h.ack("dc-a")
	h.run(5)
	if !controllerutil.ContainsFinalizer(h.subnet("aa-apps"), platformplan.Finalizer) || len(h.network().Status.SubnetClaims) != 3 {
		t.Fatal("offline remote acknowledgement did not protect deletion receipts")
	}
	remote := h.binding("dc-b")
	for _, s := range remote.Spec.RemoteSubnets {
		if s.CIDR == "10.241.0.0/24" {
			t.Fatal("deleting subnet still present in remote desired routes")
		}
	}
	h.ack("dc-b")
	h.run(12)
	var removed api.Subnet
	if err = c.Get(ctx, client.ObjectKey{Namespace: "project", Name: "aa-apps"}, &removed); !apierrors.IsNotFound(err) {
		t.Fatalf("acknowledged deletion did not finalize: %v", err)
	}
	if h.subnet("db").Status.NativeVpcName != platformplan.VpcName(string(v.UID), "dc-a") {
		t.Fatal("deleting one subnet replaced its sibling VPC")
	}
	h.ack("dc-a")
	h.ack("dc-b")
	h.run(8)
	if len(h.network().Status.SubnetClaims) != 2 {
		t.Fatal("completed withdrawal retained a stale prefix claim")
	}
	if err = c.Delete(ctx, h.network()); err != nil {
		t.Fatal(err)
	}
	h.run(6)
	if !controllerutil.ContainsFinalizer(h.network(), platformplan.Finalizer) || h.network().Status.Phase != "Deleting" || h.subnet("db").DeletionTimestamp != nil {
		t.Fatal("VPC deletion bypassed independent subnet lifecycle")
	}
}
