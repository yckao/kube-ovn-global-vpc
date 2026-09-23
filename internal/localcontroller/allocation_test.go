package localcontroller

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	api "globalvpc.io/controller/api/v1alpha2"
	"globalvpc.io/controller/internal/platformplan"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func allocationCM(t *testing.T, c client.Client, name string) *corev1.ConfigMap {
	t.Helper()
	cm := new(corev1.ConfigMap)
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: localFixtureConfig().Namespace, Name: name}, cm); err != nil {
		t.Fatal(err)
	}
	return cm
}
func allocateTest(t *testing.T, c client.Client, name string) Allocation {
	t.Helper()
	a, err := allocate(context.Background(), c, localFixtureConfig(), getSnapshot(t, c, name))
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func addAllocationBinding(t *testing.T, c client.Client, id string) *api.NetworkBinding {
	t.Helper()
	b := localFixtureBinding(true)
	b.Name = "binding-" + id
	b.UID = types.UID("local-" + id)
	b.Annotations[SourceUIDAnnotation] = "source-" + id
	if err := c.Create(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	return b
}
func readTestState(t *testing.T, c client.Client) allocatorState {
	t.Helper()
	state, err := readAllocationState(allocationCM(t, c, allocationLedgerName), localFixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestAllocationRetainsIdentityAndAddressesAcrossRestartAndRejoin(t *testing.T) {
	ctx := context.Background()
	b := localFixtureBinding(true)
	c := fakeLocalClient(t, b)
	original := allocateTest(t, c, b.Name)
	identity := allocationCM(t, c, allocationIdentityName)
	ledger := allocationCM(t, c, allocationLedgerName)
	if identity.Immutable == nil || !*identity.Immutable || identity.Data["ledgerUID"] != string(ledger.UID) {
		t.Fatal("ledger identity is not pinned by an immutable anchor")
	}
	recorded := getSnapshot(t, c, b.Name)
	if recorded.Annotations[AllocationReceiptAnnotation] == "" {
		t.Fatal("allocation exposed before a durable binding receipt")
	}
	// A fresh caller recovers from API state alone, with no process-local cache.
	if got := allocateTest(t, c, b.Name); got != original {
		t.Fatal("restart changed allocation")
	}
	if err := c.Delete(ctx, recorded); err != nil {
		t.Fatal(err)
	}
	replacement := localFixtureBinding(true)
	replacement.UID = "local-rejoined"
	replacement.Annotations[SourceUIDAnnotation] = "authority-rejoined"
	if err := c.Create(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	next := allocateTest(t, c, replacement.Name)
	if next.TransitCIDR == original.TransitCIDR || next.BFDSourceIP == original.BFDSourceIP {
		t.Fatal("rejoin reused potentially live isolated runtime addresses")
	}
	if len(readTestState(t, c).Allocations) != 2 {
		t.Fatal("deleted binding reservation was garbage collected")
	}
}

func TestAllocationMissingOrReplacedLedgerFailsClosedForOldAndNewOwners(t *testing.T) {
	for _, damage := range []string{"missing", "replaced", "identity-missing"} {
		t.Run(damage, func(t *testing.T) {
			ctx := context.Background()
			b := localFixtureBinding(true)
			c := fakeLocalClient(t, b)
			allocateTest(t, c, b.Name)
			old := allocationCM(t, c, allocationLedgerName)
			if damage == "identity-missing" {
				if err := c.Delete(ctx, allocationCM(t, c, allocationIdentityName)); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := c.Delete(ctx, old); err != nil {
					t.Fatal(err)
				}
				if damage == "replaced" {
					replacement := old.DeepCopy()
					replacement.UID = "replacement-ledger"
					replacement.ResourceVersion = ""
					if err := c.Create(ctx, replacement); err != nil {
						t.Fatal(err)
					}
				}
			}
			fresh := addAllocationBinding(t, c, "new")
			for _, name := range []string{b.Name, fresh.Name} {
				if a, err := allocate(ctx, c, localFixtureConfig(), getSnapshot(t, c, name)); err == nil || a != (Allocation{}) {
					t.Fatalf("%s ledger exposed allocation for %s: %+v, %v", damage, name, a, err)
				}
			}
			if damage == "missing" {
				var cm corev1.ConfigMap
				if err := c.Get(ctx, client.ObjectKeyFromObject(old), &cm); !apierrors.IsNotFound(err) {
					t.Fatal("missing damaged ledger was recreated")
				}
			}
		})
	}
}

func TestAllocationValidatesAllRetainedRecordsAndReceipts(t *testing.T) {
	cases := map[string]func(*corev1.ConfigMap, *allocatorState){
		"checksum": func(cm *corev1.ConfigMap, _ *allocatorState) { cm.Data["state.json"] += " " },
		"router": func(_ *corev1.ConfigMap, s *allocatorState) {
			a := s.Allocations["source-other"]
			a.RouterIP = "10.211.0.99"
			s.Allocations["source-other"] = a
		},
		"transit-duplicate": func(_ *corev1.ConfigMap, s *allocatorState) {
			a := s.Allocations["source-other"]
			a.TransitCIDR = s.Allocations["authority-binding-a"].TransitCIDR
			a.RouterIP = s.Allocations["authority-binding-a"].RouterIP
			s.Allocations["source-other"] = a
		},
		"bfd-duplicate": func(_ *corev1.ConfigMap, s *allocatorState) {
			a := s.Allocations["source-other"]
			a.BFDSourceIP = s.Allocations["authority-binding-a"].BFDSourceIP
			s.Allocations["source-other"] = a
		},
		"owner-removed": func(_ *corev1.ConfigMap, s *allocatorState) {
			delete(s.Allocations, "source-other")
			delete(s.BindingUIDs, "source-other")
		},
		"noncanonical": func(_ *corev1.ConfigMap, s *allocatorState) {
			a := s.Allocations["source-other"]
			a.TransitCIDR = "10.211.0.9/29"
			s.Allocations["source-other"] = a
		},
		"foreign-cluster": func(_ *corev1.ConfigMap, s *allocatorState) { s.ClusterUID = "another-cluster" },
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			b := localFixtureBinding(true)
			c := fakeLocalClient(t, b)
			allocateTest(t, c, b.Name)
			other := addAllocationBinding(t, c, "other")
			allocateTest(t, c, other.Name)
			cm := allocationCM(t, c, allocationLedgerName)
			state := readTestState(t, c)
			damage(cm, &state)
			if name != "checksum" {
				writeAllocationState(cm, state)
			}
			if err := c.Update(ctx, cm); err != nil {
				t.Fatal(err)
			}
			fresh := addAllocationBinding(t, c, "new")
			// Damage to an unrelated owner cannot be ignored by an existing or new one.
			for _, target := range []string{b.Name, fresh.Name} {
				if _, err := allocate(ctx, c, localFixtureConfig(), getSnapshot(t, c, target)); err == nil {
					t.Fatal("corrupt retained allocation was accepted")
				}
			}
		})
	}
}

func TestAllocationRejectsBindingUIDReplacementForExistingSource(t *testing.T) {
	ctx := context.Background()
	b := localFixtureBinding(true)
	c := fakeLocalClient(t, b)
	allocateTest(t, c, b.Name)
	if err := c.Delete(ctx, getSnapshot(t, c, b.Name)); err != nil {
		t.Fatal(err)
	}
	replacement := localFixtureBinding(true)
	replacement.UID = "unexpected-replacement"
	if err := c.Create(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if _, err := allocate(ctx, c, localFixtureConfig(), getSnapshot(t, c, b.Name)); err == nil {
		t.Fatal("a new local lifecycle adopted an old source's allocation")
	}
	if len(readTestState(t, c).Allocations) != 1 {
		t.Fatal("rejected replacement changed reservations")
	}
}

// lostAllocationResponse commits an API write but drops its response. A fresh
// allocation call must recover that write from durable state before returning.
type lostAllocationResponse struct {
	client.Client
	operation, target string
	fired             bool
}

func (c *lostAllocationResponse) Create(ctx context.Context, o client.Object, opts ...client.CreateOption) error {
	if err := c.Client.Create(ctx, o, opts...); err != nil {
		return err
	}
	if !c.fired && c.operation == "create" && o.GetName() == c.target {
		c.fired = true
		return fmt.Errorf("simulated committed create response loss")
	}
	return nil
}
func (c *lostAllocationResponse) Update(ctx context.Context, o client.Object, opts ...client.UpdateOption) error {
	if err := c.Client.Update(ctx, o, opts...); err != nil {
		return err
	}
	if !c.fired && c.operation == "update" && o.GetName() == c.target {
		c.fired = true
		return fmt.Errorf("simulated committed update response loss")
	}
	return nil
}
func TestAllocationRecoversUncertainWritesWithoutAdditionalReservation(t *testing.T) {
	for _, step := range []string{"ledger-create", "anchor-create", "reservation-update", "receipt-update"} {
		t.Run(step, func(t *testing.T) {
			ctx := context.Background()
			b := localFixtureBinding(true)
			base := fakeLocalClient(t, b)
			c := &lostAllocationResponse{Client: base}
			switch step {
			case "ledger-create":
				c.operation, c.target = "create", allocationLedgerName
			case "anchor-create":
				c.operation, c.target = "create", allocationIdentityName
			case "reservation-update":
				c.operation, c.target = "update", allocationLedgerName
			case "receipt-update":
				c.operation, c.target = "update", b.Name
			}
			first := getSnapshot(t, c, b.Name)
			if a, err := allocate(ctx, c, localFixtureConfig(), first); err == nil || a != (Allocation{}) || !c.fired {
				t.Fatalf("uncertain write leaked allocation: %+v %v", a, err)
			}
			if first.Annotations[AllocationReceiptAnnotation] != "" {
				t.Fatal("failed receipt write polluted caller's in-memory acknowledgment")
			}
			recovered := allocateTest(t, c, b.Name)
			if recovered.TransitCIDR != "10.211.0.0/29" || recovered.BFDSourceIP != "10.251.0.1" {
				t.Fatalf("retry skipped uncertain reservation: %+v", recovered)
			}
			state := readTestState(t, c)
			if len(state.Allocations) != 1 {
				t.Fatal("uncertain write allocated twice")
			}
			var receipt allocationReceipt
			if err := json.Unmarshal([]byte(getSnapshot(t, c, b.Name).Annotations[AllocationReceiptAnnotation]), &receipt); err != nil || receipt.Allocation != recovered {
				t.Fatal("returned allocation has no matching durable receipt")
			}
		})
	}
}

func TestAllocationConcurrentReservationsDoNotReuseAddresses(t *testing.T) {
	ctx := context.Background()
	first := localFixtureBinding(true)
	c := fakeLocalClient(t, first)
	initial := allocateTest(t, c, first.Name)
	const count = 12
	names := make([]string, count)
	for i := range names {
		names[i] = addAllocationBinding(t, c, fmt.Sprintf("parallel-%d", i)).Name
	}
	results := make(chan Allocation, count)
	errors := make(chan error, count)
	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			for attempt := 0; attempt < 100; attempt++ {
				var b api.NetworkBinding
				if err := c.Get(ctx, client.ObjectKey{Namespace: localFixtureConfig().Namespace, Name: name}, &b); err != nil {
					errors <- err
					return
				}
				a, err := allocate(ctx, c, localFixtureConfig(), &b)
				if apierrors.IsConflict(err) {
					continue
				}
				if err != nil {
					errors <- err
					return
				}
				results <- a
				return
			}
			errors <- fmt.Errorf("CAS retries exhausted for %s", name)
		}(name)
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	transit, bfd := map[string]bool{initial.TransitCIDR: true}, map[string]bool{initial.BFDSourceIP: true}
	completed := 0
	for a := range results {
		completed++
		if transit[a.TransitCIDR] || bfd[a.BFDSourceIP] {
			t.Errorf("concurrent allocation reused addresses: %+v", a)
		}
		transit[a.TransitCIDR], bfd[a.BFDSourceIP] = true, true
	}
	if completed != count || len(readTestState(t, c).Allocations) != count+1 {
		t.Fatal("concurrent reservations did not complete with one retained claim each")
	}
}

func TestAllocationDoesNotBootstrapOverRuntimeEvidence(t *testing.T) {
	b := localFixtureBinding(true)
	b.Status.Resources = []api.ResourceRecord{{APIVersion: "kubeovn.io/v1", Kind: "Vpc", Name: "previous", UID: "native-old"}}
	c := fakeLocalClient(t, b)
	if _, err := allocate(context.Background(), c, localFixtureConfig(), getSnapshot(t, c, b.Name)); err == nil {
		t.Fatal("missing ledger and identity were treated as bootstrap beside native receipts")
	}
	var maps corev1.ConfigMapList
	if err := c.List(context.Background(), &maps); err != nil {
		t.Fatal(err)
	}
	if len(maps.Items) != 0 {
		t.Fatal("damage detection created replacement allocator state")
	}
}

func TestSyncerPreservesLocalAllocationReceiptOnAuthorityUpdate(t *testing.T) {
	ctx := context.Background()
	remote := authorityFixtureBinding()
	authority := fakeLocalClient(t, remote)
	local := fakeLocalClient(t)
	s := &Syncer{Authority: authority, Local: local, Config: localFixtureConfig()}
	if err := s.Once(ctx); err != nil {
		t.Fatal(err)
	}
	before := allocateTest(t, local, remote.Name)
	receipt := getSnapshot(t, local, remote.Name).Annotations[AllocationReceiptAnnotation]
	if err := authority.Get(ctx, client.ObjectKeyFromObject(remote), remote); err != nil {
		t.Fatal(err)
	}
	remote.Spec.Subnets = append(remote.Spec.Subnets, api.BindingSubnet{Name: "new", UID: "new", CIDR: "10.241.3.0/24", Gateway: "10.241.3.1", NativeSubnetName: "ps-new"})
	remote.Spec.Revision = platformplan.Revision(remote.Spec)
	if err := authority.Update(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := s.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if got := getSnapshot(t, local, remote.Name).Annotations[AllocationReceiptAnnotation]; got != receipt {
		t.Fatal("authority update overwrote local allocation receipt")
	}
	if got := allocateTest(t, local, remote.Name); got != before {
		t.Fatal("authority update changed accepted local allocation")
	}
}
