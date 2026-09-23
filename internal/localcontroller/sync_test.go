package localcontroller

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	api "globalvpc.io/controller/api/v1alpha2"
	"globalvpc.io/controller/internal/platformplan"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type authorityView struct {
	client.Client
	unavailable bool
	replay      *api.NetworkBindingList
}

func (c *authorityView) List(ctx context.Context, obj client.ObjectList, opts ...client.ListOption) error {
	if c.unavailable {
		return fmt.Errorf("simulated authority transport unavailable")
	}
	if c.replay != nil {
		*obj.(*api.NetworkBindingList) = *c.replay.DeepCopy()
		return nil
	}
	return c.Client.List(ctx, obj, opts...)
}
func authorityFixtureBinding() *api.NetworkBinding {
	b := localFixtureBinding(false)
	b.Namespace = "location-a"
	b.UID = "authority-binding-a"
	b.Annotations = nil
	return b
}
func getSnapshot(t *testing.T, c client.Client, name string) *api.NetworkBinding {
	t.Helper()
	b := new(api.NetworkBinding)
	if e := c.Get(context.Background(), client.ObjectKey{Namespace: "operator-system", Name: name}, b); e != nil {
		t.Fatal(e)
	}
	return b
}

func TestSyncerKeepsDurableSnapshotDuringAuthorityFailureOrOmission(t *testing.T) {
	ctx := context.Background()
	remote := authorityFixtureBinding()
	authority := &authorityView{Client: fakeLocalClient(t, remote)}
	local := fakeLocalClient(t)
	s := &Syncer{Authority: authority, Local: local, Config: localFixtureConfig()}
	if e := s.Once(ctx); e != nil {
		t.Fatal(e)
	}
	before := getSnapshot(t, local, remote.Name)
	if before.UID == remote.UID || before.Annotations[SourceUIDAnnotation] != string(remote.UID) {
		t.Fatal("source and local lifecycle identities were conflated")
	}
	authority.unavailable = true
	if e := s.Once(ctx); e == nil {
		t.Fatal("failed authority read was hidden")
	}
	if !reflect.DeepEqual(before, getSnapshot(t, local, remote.Name)) {
		t.Fatal("authority outage mutated durable local snapshot")
	}
	authority.unavailable = false
	authority.replay = &api.NetworkBindingList{}
	if e := s.Once(ctx); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(before, getSnapshot(t, local, remote.Name)) {
		t.Fatal("empty listing inferred destructive deletion")
	}
	// A new process sees the same persisted configuration without the authority.
	restarted := &Syncer{Authority: authority, Local: local, Config: localFixtureConfig()}
	authority.unavailable = true
	_ = restarted.Once(ctx)
	if getSnapshot(t, local, remote.Name).Spec.Revision != before.Spec.Revision {
		t.Fatal("process restart lost accepted snapshot")
	}
}

func TestSyncerRejectsStaleReplayBeforeChangingLocalSnapshot(t *testing.T) {
	ctx := context.Background()
	remote := authorityFixtureBinding()
	authority := &authorityView{Client: fakeLocalClient(t, remote)}
	local := fakeLocalClient(t)
	s := &Syncer{Authority: authority, Local: local, Config: localFixtureConfig()}
	if e := s.Once(ctx); e != nil {
		t.Fatal(e)
	}
	var old api.NetworkBinding
	if e := authority.Get(ctx, client.ObjectKeyFromObject(remote), &old); e != nil {
		t.Fatal(e)
	}
	next := old.DeepCopy()
	next.Spec.Subnets = append(next.Spec.Subnets, api.BindingSubnet{Name: "new", UID: "new-uid", CIDR: "10.241.2.0/24", Gateway: "10.241.2.1", NativeSubnetName: "ps-new"})
	next.Spec.Revision = platformplan.Revision(next.Spec)
	if e := authority.Update(ctx, next); e != nil {
		t.Fatal(e)
	}
	if e := s.Once(ctx); e != nil {
		t.Fatal(e)
	}
	accepted := getSnapshot(t, local, remote.Name)
	if accepted.Annotations[SourceGenerationAnnotation] != "2" {
		t.Fatal("accepted authority generation was not persisted")
	}
	for _, sameGeneration := range []bool{false, true} {
		replayed := old.DeepCopy()
		if sameGeneration {
			replayed.Generation = next.Generation
		}
		authority.replay = &api.NetworkBindingList{Items: []api.NetworkBinding{*replayed}}
		if e := s.Once(ctx); e == nil {
			t.Fatal("stale or same-generation different-spec replay accepted")
		}
		if !reflect.DeepEqual(accepted, getSnapshot(t, local, remote.Name)) {
			t.Fatal("local snapshot rolled back before reporting replay error")
		}
	}
}

func TestOneMalformedBindingDoesNotBlockOtherAuthorizedSnapshots(t *testing.T) {
	ctx := context.Background()
	good := authorityFixtureBinding()
	bad := good.DeepCopy()
	bad.Name = "aaa-invalid-binding"
	bad.UID = "bad-source"
	authority := fakeLocalClient(t, bad, good)
	local := fakeLocalClient(t)
	s := &Syncer{Authority: authority, Local: local, Config: localFixtureConfig()}
	if e := s.Once(ctx); e == nil {
		t.Fatal("malformed binding identity was ignored")
	}
	if getSnapshot(t, local, good.Name).Spec.VPCRef.UID != good.Spec.VPCRef.UID {
		t.Fatal("unrelated valid binding was blocked by another binding error")
	}
	var rejected api.NetworkBinding
	if e := local.Get(ctx, client.ObjectKey{Namespace: "operator-system", Name: bad.Name}, &rejected); !apierrors.IsNotFound(e) {
		t.Fatal("invalid binding was materialized")
	}
}

func TestSyncerRejoinRequiresTerminalOldIncarnationCleanup(t *testing.T) {
	ctx := context.Background()
	remote := authorityFixtureBinding()
	authority := fakeLocalClient(t, remote)
	local := fakeLocalClient(t)
	s := &Syncer{Authority: authority, Local: local, Config: localFixtureConfig()}
	if e := s.Once(ctx); e != nil {
		t.Fatal(e)
	}
	original := getSnapshot(t, local, remote.Name)
	if e := authority.Delete(ctx, remote); e != nil {
		t.Fatal(e)
	}
	replacement := remote.DeepCopy()
	replacement.ResourceVersion = ""
	replacement.UID = "replacement-authority"
	if e := authority.Create(ctx, replacement); e != nil {
		t.Fatal(e)
	}
	if e := s.Once(ctx); e == nil {
		t.Fatal("new source UID adopted a running old incarnation")
	}
	old := getSnapshot(t, local, remote.Name)
	old.Spec.Deleting = true
	old.Spec.Revision = platformplan.Revision(old.Spec)
	if e := local.Update(ctx, old); e != nil {
		t.Fatal(e)
	}
	old.Status.Phase = "Deleted"
	old.Status.AppliedRevision = old.Spec.Revision
	old.Status.Resources = []api.ResourceRecord{{Kind: "Vpc", Name: "still-present", UID: "native-old"}}
	if e := local.Status().Update(ctx, old); e != nil {
		t.Fatal(e)
	}
	if e := s.Once(ctx); e == nil {
		t.Fatal("terminal label bypassed retained resource cleanup")
	}
	old = getSnapshot(t, local, remote.Name)
	old.Status.Resources = nil
	old.Status.Gateways = nil
	if e := local.Status().Update(ctx, old); e != nil {
		t.Fatal(e)
	}
	if e := s.Once(ctx); e != nil {
		t.Fatal(e)
	}
	var removed api.NetworkBinding
	if e := local.Get(ctx, client.ObjectKeyFromObject(original), &removed); !apierrors.IsNotFound(e) {
		t.Fatal("old local object was overwritten instead of UID-guarded deletion")
	}
	if e := s.Once(ctx); e != nil {
		t.Fatal(e)
	}
	joined := getSnapshot(t, local, remote.Name)
	if joined.UID == original.UID || joined.Annotations[SourceUIDAnnotation] != "replacement-authority" || joined.Spec.Deleting {
		t.Fatal("fresh source did not create a new local lifecycle")
	}
}
