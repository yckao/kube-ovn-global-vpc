package sitecontroller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	api "globalvpc.io/controller/api/v1alpha1"
	"globalvpc.io/controller/internal/config"
	"globalvpc.io/controller/internal/ovn"
	"globalvpc.io/controller/internal/siteconfig"
	"globalvpc.io/controller/internal/sitegateway"
	"globalvpc.io/controller/internal/siteplan"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type haHarness struct {
	*harness
	gw  *haGatewayDouble
	bfd *haBFDDouble
}

func newHAHarness(t *testing.T) *haHarness {
	h := newHarness(t)
	h.r.Config.BFDTransactionCommand = []string{"local-ovsdb-client", "transact", "unix:/local/nb.sock"}
	g := &h.r.Config.Grants[0]
	g.GatewayIP = ""
	g.TransitCIDR = "10.240.255.0/29"
	g.Gateways = []siteconfig.Gateway{{ID: "gw1", IP: "10.240.255.2"}, {ID: "gw2", IP: "10.240.255.3"}}
	g.BFD = &siteconfig.BFD{SourceIP: "10.254.120.1", MinRX: 300, MinTX: 300, Multiplier: 3}
	var err error
	h.p, err = siteplan.Build(h.current(), h.r.Config)
	if err != nil {
		t.Fatal(err)
	}
	ha := &haHarness{harness: h}
	ha.gw = &haGatewayDouble{gatewayDouble: h.gateway, readyIDs: []string{"gw1", "gw2"}}
	ha.bfd = &haBFDDouble{h: ha, up: []string{"gw1", "gw2"}, foreign: []api.BFDResourceRecord{{GatewayID: "other", UUID: "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"}}}
	h.r.Gateway, h.r.BFD = ha.gw, ha.bfd
	return ha
}

func (h *haHarness) bootstrap() {
	h.reconcile()
	h.reconcile()
	h.readySubnets()
}

func (h *haHarness) convergeHA() {
	h.bootstrap()
	for i := 0; i < 12; i++ {
		h.reconcile()
		v := h.current()
		if v.Status.Phase == "LocalReady" && meta.IsStatusConditionTrue(v.Status.Conditions, "LocalReady") {
			return
		}
	}
	h.t.Fatalf("HA did not become ready: %+v", h.current().Status)
}

func (h *haHarness) defaultRoutes() []interface{} {
	h.t.Helper()
	vpc := object("Vpc", h.p.VpcName)
	if err := h.infra.Get(context.Background(), client.ObjectKeyFromObject(vpc), vpc); err != nil {
		h.t.Fatal(err)
	}
	routes, _, err := unstructured.NestedSlice(vpc.Object, "spec", "staticRoutes")
	if err != nil {
		h.t.Fatal(err)
	}
	return routes
}

type haGatewayDouble struct {
	*gatewayDouble
	readyIDs []string
}

func (g *haGatewayDouble) snapshot(c siteplan.GatewayConfig) sitegateway.Result {
	r := g.result(c)
	r.Version = "v2"
	r.DesiredGateways = len(c.Gateways)
	for _, id := range g.readyIDs {
		for _, record := range r.Resources {
			if record.Kind == "Pod" && record.Name == c.VpcName+"-"+id {
				r.ReadyGatewayIDs = append(r.ReadyGatewayIDs, id)
			}
		}
	}
	r.ReadyGateways = len(r.ReadyGatewayIDs)
	r.Ready = g.ready && r.ReadyGateways > 0
	return r
}

func (g *haGatewayDouble) Get(_ context.Context, c siteplan.GatewayConfig, expected []api.GatewayResourceRecord) (sitegateway.Result, error) {
	if g.getErr != nil {
		return sitegateway.Result{}, g.getErr
	}
	if err := g.check(c, expected, false); err != nil {
		return sitegateway.Result{}, err
	}
	return g.snapshot(c), nil
}

func (g *haGatewayDouble) Ensure(_ context.Context, c siteplan.GatewayConfig, expected []api.GatewayResourceRecord) (sitegateway.Result, error) {
	g.h.durable()
	g.ensureCalls++
	if !sameReceipts(expected, g.h.current().Status.GatewayResources) {
		g.h.t.Fatal("gateway mutation before receipt persistence")
	}
	if g.ensureErr != nil {
		return sitegateway.Result{}, g.ensureErr
	}
	if err := g.check(c, expected, true); err != nil {
		return sitegateway.Result{}, err
	}
	for _, member := range c.Gateways {
		for _, kind := range []string{"Pod", "ConfigMap"} {
			name := c.VpcName + "-" + member.ID
			found := false
			for _, r := range g.records[c.OwnerUID] {
				found = found || (r.Name == name && r.Kind == kind)
			}
			if !found {
				g.serial++
				g.mutations++
				g.records[c.OwnerUID] = append(g.records[c.OwnerUID], api.GatewayResourceRecord{APIVersion: "v1", Kind: kind, Namespace: "gateway-system", Name: name, UID: fmt.Sprintf("ha-gateway-%d", g.serial)})
			}
		}
	}
	if g.loseEnsure {
		g.loseEnsure = false
		return sitegateway.Result{}, sitegateway.ErrUnknownState
	}
	return g.snapshot(c), nil
}

func (g *haGatewayDouble) Delete(ctx context.Context, c siteplan.GatewayConfig, expected []api.GatewayResourceRecord) (sitegateway.Result, error) {
	_, err := g.gatewayDouble.Delete(ctx, c, expected)
	if err != nil {
		return sitegateway.Result{}, err
	}
	return g.snapshot(c), nil
}

func (g *haGatewayDouble) EndpointsEmpty(_ context.Context, c siteplan.GatewayConfig, expected []api.GatewayResourceRecord) (sitegateway.Result, error) {
	if err := g.check(c, expected, false); err != nil {
		return sitegateway.Result{}, err
	}
	return g.snapshot(c), nil
}

type haBFDDouble struct {
	h                                               *haHarness
	records, foreign                                []api.BFDResourceRecord
	up                                              []string
	getErr, ensureErr, deleteErr                    error
	loseEnsure, holdReferences                      bool
	ensureCalls, deleteCalls, mutations, fieldCalls int
	conflicts                                       map[string]bool
}

func (b *haBFDDouble) check(expected []api.BFDResourceRecord) error {
	for _, old := range expected {
		for _, r := range b.records {
			if old.GatewayID == r.GatewayID && old.UUID != r.UUID {
				return errors.New("BFD resource UUID changed")
			}
		}
	}
	return nil
}

func (b *haBFDDouble) snapshot() ovn.BFDState {
	up := []string{}
	for _, id := range b.up {
		for _, r := range b.records {
			if id == r.GatewayID {
				up = append(up, id)
			}
		}
	}
	return ovn.BFDState{Records: append([]api.BFDResourceRecord(nil), b.records...), Up: up, Configured: len(b.records) == 2}
}

func (b *haBFDDouble) GetBFD(_ context.Context, _ config.Cluster, _ siteplan.GatewayConfig, expected []api.BFDResourceRecord) (ovn.BFDState, error) {
	if b.getErr != nil {
		return ovn.BFDState{}, b.getErr
	}
	if err := b.check(expected); err != nil {
		return ovn.BFDState{}, err
	}
	return b.snapshot(), nil
}

func (b *haBFDDouble) EnsureBFD(_ context.Context, _ config.Cluster, g siteplan.GatewayConfig, expected []api.BFDResourceRecord) (ovn.BFDState, error) {
	b.h.durable()
	b.ensureCalls++
	if !sameBFDReceipts(expected, b.h.current().Status.BFDResources) {
		b.h.t.Fatal("BFD mutation before receipt persistence")
	}
	if b.ensureErr != nil {
		return ovn.BFDState{}, b.ensureErr
	}
	if err := b.check(expected); err != nil {
		return ovn.BFDState{}, err
	}
	for _, old := range expected {
		found := false
		for _, actual := range b.records {
			found = found || old.GatewayID == actual.GatewayID
		}
		if !found {
			return ovn.BFDState{}, errors.New("missing BFD must be durably forgotten")
		}
	}
	for i, member := range g.Gateways {
		found := false
		for _, r := range b.records {
			found = found || r.GatewayID == member.ID
		}
		if !found {
			b.mutations++
			b.records = append(b.records, api.BFDResourceRecord{GatewayID: member.ID, UUID: fmt.Sprintf("%08d-1111-1111-1111-111111111111", i+1)})
		}
	}
	if b.loseEnsure {
		b.loseEnsure = false
		return ovn.BFDState{}, errors.New("BFD write outcome unknown")
	}
	return b.snapshot(), nil
}

func (b *haBFDDouble) BFDRouteConflicts(context.Context, config.Cluster, siteplan.GatewayConfig, []api.BFDResourceRecord) (map[string]bool, error) {
	return b.conflicts, nil
}

func (b *haBFDDouble) EnsureECMPFields(context.Context, config.Cluster, siteplan.GatewayConfig, []api.BFDResourceRecord) (bool, error) {
	b.fieldCalls++
	return true, nil
}

func (b *haBFDDouble) DeleteBFD(_ context.Context, _ config.Cluster, g siteplan.GatewayConfig, expected []api.BFDResourceRecord) (bool, error) {
	b.h.durable()
	b.deleteCalls++
	if b.deleteErr != nil {
		return false, b.deleteErr
	}
	if err := b.check(expected); err != nil {
		return false, err
	}
	if len(b.records) == 0 {
		return true, nil
	}
	if len(b.h.gw.records[g.OwnerUID]) != 0 {
		b.h.t.Fatal("BFD deletion before gateway removal")
	}
	if !b.h.network.endpoints || !b.h.gw.endpoints {
		b.h.t.Fatal("BFD deletion before endpoint drain")
	}
	if len(b.h.defaultRoutes()) != 0 {
		b.h.t.Fatal("BFD deletion before native Vpc route withdrawal")
	}
	if b.h.current().Status.Operation != "DeleteResources" {
		b.h.t.Fatal("BFD deletion before durable cleanup stage")
	}
	if b.holdReferences {
		return false, nil
	}
	if len(b.records) == 0 {
		return true, nil
	}
	b.mutations += len(b.records)
	b.records = nil
	return false, nil
}

func TestHARecordsBFDIdentityBeforePublishingRoutesOrGatewayMutation(t *testing.T) {
	h := newHAHarness(t)
	h.bootstrap()
	if len(h.defaultRoutes()) != 0 {
		t.Fatal("unmonitored initial default routes")
	}
	h.reconcile()
	if len(h.current().Status.BFDResources) != 2 || h.bfd.mutations != 2 || h.gw.mutations != 0 || len(h.defaultRoutes()) != 0 {
		t.Fatal("BFD allocation was not durably separated from dependent operations")
	}
	h.reconcile()
	if len(h.defaultRoutes()) != 2 || h.gw.mutations != 4 {
		t.Fatal("durable BFD receipts did not unlock routes and gateways")
	}
	for _, raw := range h.defaultRoutes() {
		r := raw.(map[string]interface{})
		if r["bfdId"] == "" {
			t.Fatal("route lacks BFD identity")
		}
	}
}

func TestHABFDWriteResponseLossRediscoversBeforeMutation(t *testing.T) {
	h := newHAHarness(t)
	h.bootstrap()
	h.bfd.loseEnsure = true
	h.reconcile()
	if len(h.current().Status.BFDResources) != 0 || len(h.bfd.records) != 2 || h.current().Status.Phase != "Blocked" {
		t.Fatal("response loss did not preserve unknown state")
	}
	writes, ensures := h.infra.writes, h.bfd.ensureCalls
	h.reconcile()
	if len(h.current().Status.BFDResources) != 2 || h.infra.writes != writes || h.bfd.ensureCalls != ensures || h.gw.ensureCalls != 0 {
		t.Fatal("recovered receipts were not persisted before mutation")
	}
	for i := 0; i < 5; i++ {
		h.reconcile()
	}
	if h.bfd.mutations != 2 || !meta.IsStatusConditionTrue(h.current().Status.Conditions, "LocalReady") {
		t.Fatal("write recovery created replacement BFD rows or failed to recover")
	}
}

func TestHABlockedObservationsNeverImplyAbsenceOrMutate(t *testing.T) {
	for _, kind := range []string{"BFD UUID changed", "BFD read unknown", "gateway read unknown", "gateway UID changed"} {
		t.Run(kind, func(t *testing.T) {
			h := newHAHarness(t)
			h.convergeHA()
			v := h.current()
			beforeBFD := append([]api.BFDResourceRecord(nil), v.Status.BFDResources...)
			beforeGW := append([]api.GatewayResourceRecord(nil), v.Status.GatewayResources...)
			switch kind {
			case "BFD UUID changed":
				h.bfd.records[0].UUID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
			case "BFD read unknown":
				h.bfd.getErr = errors.New("BFD DB unavailable")
			case "gateway read unknown":
				h.gw.getErr = sitegateway.ErrUnknownState
			case "gateway UID changed":
				h.gw.records[h.p.OwnerUID][0].UID = "replacement"
			}
			writes, bfdMut, gwMut := h.infra.writes, h.bfd.mutations, h.gw.mutations
			h.reconcile()
			v = h.current()
			if v.Status.Phase != "Blocked" || !controllerutil.ContainsFinalizer(v, api.SiteFinalizer) || !reflect.DeepEqual(v.Status.BFDResources, beforeBFD) || !reflect.DeepEqual(v.Status.GatewayResources, beforeGW) {
				t.Fatal("unknown/replaced resource was treated as absence")
			}
			if h.infra.writes != writes || h.bfd.mutations != bfdMut || h.gw.mutations != gwMut {
				t.Fatal("mutated after failed ownership preflight")
			}
		})
	}
}

func TestHAGatewayReadinessRequiresIntersectionAndReportsDegradation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pod, bfd  []string
		ready     int
		redundant bool
	}{
		{"both", []string{"gw1", "gw2"}, []string{"gw1", "gw2"}, 2, true},
		{"one survives", []string{"gw2"}, []string{"gw2"}, 1, false},
		{"different healthy IDs", []string{"gw1"}, []string{"gw2"}, 0, false},
		{"no BFD connectivity", []string{"gw1", "gw2"}, nil, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHAHarness(t)
			h.convergeHA()
			h.gw.readyIDs, h.bfd.up = tc.pod, tc.bfd
			h.reconcile()
			h.reconcile()
			v := h.current()
			if v.Status.GatewayReadyCount != tc.ready || v.Status.GatewayDesiredCount != 2 || meta.IsStatusConditionTrue(v.Status.Conditions, "LocalReady") != (tc.ready > 0) || meta.IsStatusConditionTrue(v.Status.Conditions, "GatewayRedundant") != tc.redundant {
				t.Fatalf("incorrect availability: %+v", v.Status)
			}
			if len(h.defaultRoutes()) != 2 {
				t.Fatal("health transition rewrote native ECMP next hops")
			}
		})
	}
}

func TestHAStaleBFDRouteWithdrawsOnlyConflictingNativeIntent(t *testing.T) {
	h := newHAHarness(t)
	h.convergeHA()
	h.bfd.conflicts = map[string]bool{h.p.Gateway.Gateways[0].IP: true}
	mutations := h.bfd.mutations
	h.network.routes = false
	h.reconcile()
	routes := h.defaultRoutes()
	if len(routes) != 1 || routes[0].(map[string]interface{})["nextHopIP"] != h.p.Gateway.Gateways[1].IP || h.bfd.mutations != mutations || meta.IsStatusConditionTrue(h.current().Status.Conditions, "LocalReady") {
		t.Fatal("stale BFD route was not withdrawn through local Vpc intent")
	}
	h.bfd.conflicts = nil
	h.network.routes = true
	h.reconcile()
	if len(h.defaultRoutes()) != 2 {
		t.Fatal("route was not restored after confirmed conflict absence")
	}
}

func TestHADeleteWaitsForDrainGatewayWithdrawalAndBFDReferences(t *testing.T) {
	h := newHAHarness(t)
	h.convergeHA()
	foreignBFD := append([]api.BFDResourceRecord(nil), h.bfd.foreign...)
	h.gw.records["other-owner"] = []api.GatewayResourceRecord{{APIVersion: "v1", Kind: "Pod", Namespace: "gateway-system", Name: "other", UID: "other-uid"}}
	foreignGateway := append([]api.GatewayResourceRecord(nil), h.gw.records["other-owner"]...)
	h.deleting()
	h.network.endpoints = false
	h.reconcile()
	if h.gw.deleteCalls != 0 || h.bfd.deleteCalls != 0 || h.infra.deletes != 0 {
		t.Fatal("deletion began before OVN endpoint drain")
	}
	h.network.endpoints = true
	h.gw.endpoints = false
	h.reconcile()
	if h.gw.deleteCalls != 0 || h.bfd.deleteCalls != 0 || h.infra.deletes != 0 {
		t.Fatal("deletion began before gateway endpoint drain")
	}
	h.gw.endpoints = true
	h.reconcile()
	if h.current().Status.Operation != "DeleteGateway" || h.bfd.deleteCalls != 0 {
		t.Fatal("gateway deletion stage not durable")
	}
	h.gw.deleteErr = sitegateway.ErrUnknownState
	h.reconcile()
	if h.current().Status.Operation != "DeleteGateway" || h.bfd.deleteCalls != 0 || h.infra.deletes != 0 {
		t.Fatal("unknown gateway delete advanced lifecycle")
	}
	h.gw.deleteErr = nil
	h.reconcile()
	if h.current().Status.Operation != "DeleteResources" || h.bfd.deleteCalls != 0 {
		t.Fatal("gateway absence not persisted before network cleanup")
	}
	h.bfd.holdReferences = true
	h.reconcile()
	if len(h.defaultRoutes()) != 0 || h.bfd.deleteCalls != 1 || h.infra.deletes != 0 || !controllerutil.ContainsFinalizer(h.current(), api.SiteFinalizer) {
		t.Fatal("BFD refs did not hold local resource deletion")
	}
	h.bfd.deleteErr = errors.New("BFD database unavailable")
	h.reconcile()
	if h.infra.deletes != 0 || !controllerutil.ContainsFinalizer(h.current(), api.SiteFinalizer) {
		t.Fatal("unknown BFD state allowed Vpc deletion")
	}
	h.bfd.deleteErr = nil
	h.bfd.holdReferences = false
	h.finishDeletion()
	if !reflect.DeepEqual(h.bfd.foreign, foreignBFD) || !reflect.DeepEqual(h.gw.records["other-owner"], foreignGateway) {
		t.Fatal("local cleanup touched another owner")
	}
	for _, kindName := range [][2]string{{"Vpc", h.p.VpcName}, {"Subnet", h.p.SubnetName}, {"Subnet", h.p.TransitName}} {
		o := object(kindName[0], kindName[1])
		if err := h.infra.Get(context.Background(), client.ObjectKeyFromObject(o), o); !apierrors.IsNotFound(err) {
			t.Fatalf("owned native object survived: %s %v", kindName, err)
		}
	}
}
