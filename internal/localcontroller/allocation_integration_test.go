//go:build integration

package localcontroller

import (
	"context"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Use a real API server for UID, resourceVersion and immutable ConfigMap
// semantics; fake clients intentionally do not model ConfigMap immutability.
func TestAPIAllocationIdentityAndLostResponseRecovery(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS must reference installed envtest binaries")
	}
	ctx := context.Background()
	server := startControlServer(t, false)
	ensureNamespace(t, server.c, "operator-system")
	b := localFixtureBinding(true)
	b.UID = ""
	if err := server.c.Create(ctx, b); err != nil {
		t.Fatal(err)
	}
	uncertain := &lostAllocationResponse{Client: server.c, operation: "update", target: allocationLedgerName}
	if _, err := allocate(ctx, uncertain, localFixtureConfig(), b); err == nil || !uncertain.fired {
		t.Fatal("expected lost committed reservation response")
	}
	if err := server.c.Get(ctx, client.ObjectKeyFromObject(b), b); err != nil {
		t.Fatal(err)
	}
	if b.Annotations[AllocationReceiptAnnotation] != "" {
		t.Fatal("receipt preceded confirmed retained reservation")
	}
	a, err := allocate(ctx, server.c, localFixtureConfig(), b)
	if err != nil {
		t.Fatal(err)
	}
	if a.TransitCIDR != "10.211.0.0/29" || a.BFDSourceIP != "10.251.0.1" {
		t.Fatalf("lost response caused a second reservation: %+v", a)
	}
	if b.Generation != 1 {
		t.Fatal("local runtime receipt changed authority snapshot generation")
	}
	pinned := allocationCM(t, server.c, allocationIdentityName)
	pinned.Data["ledgerUID"] = "replacement-ledger"
	if err := server.c.Update(ctx, pinned); !apierrors.IsInvalid(err) {
		t.Fatalf("identity ConfigMap was not immutable: %v", err)
	}
	ledger := allocationCM(t, server.c, allocationLedgerName)
	oldUID := ledger.UID
	if err := server.c.Delete(ctx, ledger); err != nil {
		t.Fatal(err)
	}
	if _, err := allocate(ctx, server.c, localFixtureConfig(), b); err == nil {
		t.Fatal("lost ledger was treated as a fresh install")
	}
	var missing corev1.ConfigMap
	if err := server.c.Get(ctx, client.ObjectKeyFromObject(ledger), &missing); !apierrors.IsNotFound(err) {
		t.Fatal("allocator recreated missing ledger")
	}
	ledger.UID = ""
	ledger.ResourceVersion = ""
	ledger.ManagedFields = nil
	if err := server.c.Create(ctx, ledger); err != nil {
		t.Fatal(err)
	}
	if ledger.UID == oldUID {
		t.Fatal("API did not assign a fresh ledger identity")
	}
	if _, err := allocate(ctx, server.c, localFixtureConfig(), b); err == nil {
		t.Fatal("restored content bypassed ledger UID fencing")
	}
	if got := allocationCM(t, server.c, allocationLedgerName); got.UID != ledger.UID {
		t.Fatal("rejected allocation rewrote replaced ledger")
	}
}
