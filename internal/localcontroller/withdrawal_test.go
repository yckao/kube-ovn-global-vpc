package localcontroller

import (
	"context"
	"testing"

	api "globalvpc.io/controller/api/v1alpha2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Native destination-route withdrawal also removes the BFD sessions used by
// gateway readiness. The last remote prefix must therefore enter disconnect,
// not ask these now-unready gateways to perform another configuration rollout.
func TestLastRemoteWithdrawalDisconnectsWithoutReadySibling(t *testing.T) {
	h := newLocalHarness(t, true, true)
	h.run(30, true)
	before := h.binding()
	if before.Status.AppliedRevision != before.Spec.Revision {
		t.Fatal("cross-site fixture was not ready")
	}
	beforeEnsures, beforeDeletes := h.gateway.ensures, h.gateway.deletes
	identities := map[string]string{}
	for _, rec := range before.Status.Resources {
		if rec.Kind == "Vpc" || rec.Kind == "Subnet" && rec.Name != transitName(before) {
			identities[rec.Kind+"/"+rec.Name] = rec.UID
		}
	}
	h.edit(func(s *api.NetworkBindingSpec) {
		// AuthorizedLocations still includes the draining source until its
		// public Subnet finalizer observes this remote withdrawal receipt.
		s.RemoteSubnets = nil
		s.Peers = nil
	})
	// Reproduce the native outcome: after routes([]), both local BFD sessions
	// are absent and the surviving gateway cannot satisfy a rolling-update gate.
	h.gateway.ready = false
	h.run(5, false)
	if h.gateway.deletes != beforeDeletes {
		t.Fatal("gateway was deleted before native withdrawal acknowledgement")
	}
	pending := h.binding()
	if pending.Status.AppliedRevision == pending.Spec.Revision {
		t.Fatal("withdrawal acknowledged before native route cleanup")
	}
	h.run(30, true)
	after := h.binding()
	if after.Status.AppliedRevision != after.Spec.Revision || !meta.IsStatusConditionTrue(after.Status.Conditions, "Ready") {
		t.Fatalf("last remote withdrawal is stuck waiting for a sibling without native BFD: %+v", after.Status)
	}
	if h.gateway.ensures != beforeEnsures || h.gateway.deletes == beforeDeletes || len(after.Status.Gateways) != 0 {
		t.Fatal("empty remote intent attempted gateway rollout instead of disconnect")
	}
	if len(after.Spec.AuthorizedLocations) != 2 || len(after.Spec.RemoteSubnets) != 0 {
		t.Fatal("fixture lost the draining-location authorization window")
	}
	if err := h.c.Get(context.Background(), client.ObjectKey{Name: transitName(after)}, native("Subnet", transitName(after))); !apierrors.IsNotFound(err) {
		t.Fatal("disconnected transit subnet remains", err)
	}
	for _, rec := range after.Status.Resources {
		if want, found := identities[rec.Kind+"/"+rec.Name]; found {
			if rec.UID != want {
				t.Fatal("local workload or native VPC identity changed during remote leave")
			}
			delete(identities, rec.Kind+"/"+rec.Name)
		}
	}
	if len(identities) != 0 {
		t.Fatal("remote leave removed a remaining local resource")
	}
	vpc := native("Vpc", after.Spec.NativeVpcName)
	if err := h.c.Get(context.Background(), client.ObjectKeyFromObject(vpc), vpc); err != nil {
		t.Fatal(err)
	}
	routes, _, _ := unstructured.NestedSlice(vpc.Object, "spec", "destinationRoutes")
	bfd, _, _ := unstructured.NestedBool(vpc.Object, "spec", "bfdPort", "enabled")
	if len(routes) != 0 || bfd {
		t.Fatal("local-only VPC retained cross-site route or BFD intent")
	}
	// A subsequent remote export re-enters normal discovery using the retained
	// authority allocation and native VPC, without requiring another local subnet.
	nativeUID := vpc.GetUID()
	h.edit(func(s *api.NetworkBindingSpec) {
		s.RemoteSubnets = []api.RemoteSubnet{{LocationRef: "dc-b", CIDR: "10.242.0.0/24"}}
	})
	h.gateway.ready = true
	h.run(30, true)
	rejoined := h.binding()
	if rejoined.Status.AppliedRevision != rejoined.Spec.Revision || len(rejoined.Status.Gateways) != 2 {
		t.Fatal("remote rejoin failed to restore gateway discovery")
	}
	if err := h.c.Get(context.Background(), client.ObjectKeyFromObject(vpc), vpc); err != nil || vpc.GetUID() != nativeUID {
		t.Fatal("remote rejoin replaced the native VPC", err)
	}
}

func TestRemoteExportBootstrapsGatewayBeforePeerRegistration(t *testing.T) {
	h := newLocalHarness(t, true, true)
	h.gateway.ready = false
	h.run(25, true)
	b := h.binding()
	if len(b.Spec.Peers) != 0 || len(b.Spec.RemoteSubnets) != 1 || h.gateway.ensures == 0 || h.gateway.deletes != 0 {
		t.Fatal("remote export did not bootstrap local gateways independently of peer discovery")
	}
	vpc := native("Vpc", b.Spec.NativeVpcName)
	if err := h.c.Get(context.Background(), client.ObjectKeyFromObject(vpc), vpc); err != nil {
		t.Fatal(err)
	}
	routes, _, _ := unstructured.NestedSlice(vpc.Object, "spec", "destinationRoutes")
	bfd, _, _ := unstructured.NestedBool(vpc.Object, "spec", "bfdPort", "enabled")
	if len(routes) != 1 || !bfd {
		t.Fatal("remote export lost native route/BFD intent during peer discovery")
	}
	if meta.IsStatusConditionTrue(b.Status.Conditions, "Ready") || b.Status.AppliedRevision == b.Spec.Revision {
		t.Fatal("discovery was acknowledged before gateway readiness")
	}
}
