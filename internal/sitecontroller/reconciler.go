// Package sitecontroller reconciles one locally authoritative attachment.
// No operation contacts another site's Controller, Kubernetes API or IC database.
package sitecontroller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	api "globalvpc.io/controller/api/v1alpha1"
	"globalvpc.io/controller/internal/config"
	legacy "globalvpc.io/controller/internal/controller"
	"globalvpc.io/controller/internal/ovn"
	"globalvpc.io/controller/internal/siteconfig"
	"globalvpc.io/controller/internal/sitegateway"
	"globalvpc.io/controller/internal/siteplan"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type Gateway interface {
	Ensure(context.Context, siteplan.GatewayConfig, []api.GatewayResourceRecord) (sitegateway.Result, error)
	Get(context.Context, siteplan.GatewayConfig, []api.GatewayResourceRecord) (sitegateway.Result, error)
	Delete(context.Context, siteplan.GatewayConfig, []api.GatewayResourceRecord) (sitegateway.Result, error)
	EndpointsEmpty(context.Context, siteplan.GatewayConfig, []api.GatewayResourceRecord) (sitegateway.Result, error)
}

// Network only observes local OVN state. Kube-OVN remains its sole writer.
type Network interface {
	EndpointsEmpty(context.Context, config.Cluster, string) (bool, error)
	LocalAbsent(context.Context, config.Cluster, string, string, string) (bool, error)
	RoutesReady(context.Context, config.Cluster, string, []ovn.Route) (bool, error)
}

// BFD owns explicitly tagged BFD rows and selection_fields on exact owned
// default routes. Kube-OVN owns route references, the native BFD-only router
// port, and its redundant chassis group.
type BFD interface {
	GetBFD(context.Context, config.Cluster, siteplan.GatewayConfig, []api.BFDResourceRecord) (ovn.BFDState, error)
	EnsureBFD(context.Context, config.Cluster, siteplan.GatewayConfig, []api.BFDResourceRecord) (ovn.BFDState, error)
	DeleteBFD(context.Context, config.Cluster, siteplan.GatewayConfig, []api.BFDResourceRecord) (bool, error)
	BFDRouteConflicts(context.Context, config.Cluster, siteplan.GatewayConfig, []api.BFDResourceRecord) (map[string]bool, error)
	EnsureECMPFields(context.Context, config.Cluster, siteplan.GatewayConfig, []api.BFDResourceRecord) (bool, error)
}

type Reconciler struct {
	client.Client
	Config   siteconfig.Config
	Store    legacy.ResourceStore
	Gateway  Gateway
	Network  Network
	BFD      BFD
	Interval time.Duration
}

func (r *Reconciler) interval() time.Duration {
	if r.Interval < time.Second {
		return 10 * time.Second
	}
	return r.Interval
}

func (r *Reconciler) state(ctx context.Context, v *api.SiteVpc, phase, reason, message string, ready, gateway bool) (ctrl.Result, error) {
	before := v.DeepCopy().Status
	v.Status.Phase, v.Status.ObservedGeneration = phase, v.Generation
	for _, c := range []struct {
		name  string
		value bool
	}{{"LocalReady", ready}, {"GatewayConfigured", gateway}} {
		status := metav1.ConditionFalse
		if c.value {
			status = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&v.Status.Conditions, metav1.Condition{Type: c.name, Status: status, Reason: reason, Message: message, ObservedGeneration: v.Generation})
	}
	if v.Status.GatewayDesiredCount > 1 {
		status, why, detail := metav1.ConditionFalse, "GatewayRedundancyDegraded", "Fewer than the configured gateways have both local process readiness and BFD connectivity"
		if gateway && v.Status.GatewayReadyCount == v.Status.GatewayDesiredCount {
			status, why, detail = metav1.ConditionTrue, "AllGatewaysAvailable", "Every configured gateway has local process readiness and BFD connectivity"
		}
		meta.SetStatusCondition(&v.Status.Conditions, metav1.Condition{Type: "GatewayRedundant", Status: status, Reason: why, Message: detail, ObservedGeneration: v.Generation})
	}
	if !reflect.DeepEqual(before, v.Status) {
		if err := r.Status().Update(ctx, v); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: r.interval()}, nil
}

func (r *Reconciler) blocked(ctx context.Context, v *api.SiteVpc, err error) (ctrl.Result, error) {
	return r.state(ctx, v, "Blocked", "LocalReconcileBlocked", err.Error(), false, false)
}

func (r *Reconciler) Reconcile(parent context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	var v api.SiteVpc
	if err := r.Get(ctx, req.NamespacedName, &v); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if v.Spec.SiteID != r.Config.SiteID || v.Spec.AttachmentID != r.Config.AttachmentID {
		return ctrl.Result{}, nil
	}
	if !v.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(&v, api.SiteFinalizer) {
		return ctrl.Result{}, nil
	}
	p, err := siteplan.Build(&v, r.Config)
	if err != nil {
		return r.blocked(ctx, &v, err)
	}
	if v.Status.PlanHash != "" && v.Status.PlanHash != p.Hash {
		return r.blocked(ctx, &v, fmt.Errorf("accepted local intent or registration changed; restore its accepted configuration"))
	}
	if !controllerutil.ContainsFinalizer(&v, api.SiteFinalizer) {
		controllerutil.AddFinalizer(&v, api.SiteFinalizer)
		return ctrl.Result{RequeueAfter: time.Millisecond}, r.Update(ctx, &v)
	}
	if v.Status.PlanHash == "" {
		v.Status.PlanHash = p.Hash
		if err = r.Status().Update(ctx, &v); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err = r.Store.CheckIdentity(ctx); err != nil {
		return r.blocked(ctx, &v, err)
	}
	// Check every local target before any mutation, including response-loss
	// recovery. Existing objects with another local owner's UID are never adopted.
	for _, desired := range []*unstructured.Unstructured{p.Vpc, p.Subnet, p.Transit} {
		actual, readErr := r.Store.Get(ctx, desired.GetKind(), desired.GetName())
		if apierrors.IsNotFound(readErr) {
			continue
		}
		if readErr != nil {
			return r.blocked(ctx, &v, fmt.Errorf("local ownership preflight failed"))
		}
		expected := recorded(&v, desired.GetKind(), desired.GetName())
		if actual.GetLabels()[api.OwnerLabel] != p.OwnerUID || actual.GetLabels()[api.ClusterLabel] != r.Config.Cluster.UID || (expected != "" && string(actual.GetUID()) != expected) {
			return r.blocked(ctx, &v, fmt.Errorf("local resource ownership or UID conflict"))
		}
	}
	observed, err := r.Gateway.Get(ctx, p.Gateway, v.Status.GatewayResources)
	if err != nil {
		return r.blocked(ctx, &v, err)
	}
	var bfdState ovn.BFDState
	if p.Gateway.IsHA() {
		if r.BFD == nil {
			return r.blocked(ctx, &v, errors.New("local BFD adapter is not configured"))
		}
		bfdState, err = r.BFD.GetBFD(ctx, r.Store.Config, p.Gateway, v.Status.BFDResources)
		if err != nil {
			return r.blocked(ctx, &v, err)
		}
		if !sameBFDReceipts(v.Status.BFDResources, bfdState.Records) {
			v.Status.BFDResources = bfdState.Records
			return ctrl.Result{RequeueAfter: time.Millisecond}, r.Status().Update(ctx, &v)
		}
	}
	if !v.DeletionTimestamp.IsZero() {
		if v.Status.Operation == "DeleteResources" && !observed.Absent {
			// A delayed gateway write must be cleaned up before finalization.
			v.Status.GatewayResources = observed.Resources
			v.Status.Operation = "DeleteGateway"
			return ctrl.Result{RequeueAfter: time.Millisecond}, r.Status().Update(ctx, &v)
		}
		return r.remove(ctx, &v, p)
	}
	if !sameReceipts(v.Status.GatewayResources, observed.Resources) {
		// Persist verified observations before any new mutation. This recovers
		// partial loss (for example, an absent Pod with its ConfigMap intact) and
		// committed writes whose original response was lost. Replaced UIDs fail
		// validation in the gateway backend before they reach this point.
		v.Status.GatewayResources = observed.Resources
		return ctrl.Result{RequeueAfter: time.Millisecond}, r.Status().Update(ctx, &v)
	}
	if p.Gateway.IsHA() {
		conflicts, queryErr := r.BFD.BFDRouteConflicts(ctx, r.Store.Config, p.Gateway, bfdState.Records)
		if queryErr != nil {
			return r.blocked(ctx, &v, queryErr)
		}
		// Native Kube-OVN ignores a bfdId-only diff. Omit stale references until
		// their routes are confirmed absent; never rewrite a native NB route.
		setGatewayRoutes(p, bfdState.Records, conflicts)
	}
	for _, desired := range []*unstructured.Unstructured{p.Vpc, p.Subnet, p.Transit} {
		uid, ensureErr := r.Store.Ensure(ctx, desired, recorded(&v, desired.GetKind(), desired.GetName()))
		if errors.Is(ensureErr, legacy.ErrRecordedResourceMissing) {
			forget(&v, desired.GetKind(), desired.GetName())
			return ctrl.Result{RequeueAfter: time.Millisecond}, r.Status().Update(ctx, &v)
		}
		if ensureErr != nil {
			return r.blocked(ctx, &v, ensureErr)
		}
		if recorded(&v, desired.GetKind(), desired.GetName()) != uid {
			v.Status.Resources = append(v.Status.Resources, api.ResourceRecord{ClusterRef: r.Config.Cluster.Name, Kind: desired.GetKind(), Name: desired.GetName(), UID: uid})
			if err = r.Status().Update(ctx, &v); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	for _, name := range []string{p.SubnetName, p.TransitName} {
		ready, readyErr := r.Store.Ready(ctx, "Subnet", name, p.OwnerUID, recorded(&v, "Subnet", name))
		if readyErr != nil {
			return r.blocked(ctx, &v, readyErr)
		}
		if !ready {
			return r.state(ctx, &v, "Pending", "LocalSubnetsPending", "Waiting for local Kube-OVN Subnets", false, false)
		}
	}
	if p.Gateway.IsHA() {
		bfdState, err = r.BFD.EnsureBFD(ctx, r.Store.Config, p.Gateway, v.Status.BFDResources)
		if errors.Is(err, ovn.ErrBFDPortPending) {
			return r.state(ctx, &v, "Pending", "NativeBFDPortPending", err.Error(), false, false)
		}
		if err != nil {
			return r.blocked(ctx, &v, err)
		}
		if !sameBFDReceipts(v.Status.BFDResources, bfdState.Records) {
			v.Status.BFDResources = bfdState.Records
			return ctrl.Result{RequeueAfter: time.Millisecond}, r.Status().Update(ctx, &v)
		}
	}
	result, err := r.Gateway.Ensure(ctx, p.Gateway, v.Status.GatewayResources)
	if err != nil {
		return r.blocked(ctx, &v, err)
	}
	if !sameReceipts(v.Status.GatewayResources, result.Resources) {
		v.Status.GatewayResources = result.Resources
		if err = r.Status().Update(ctx, &v); err != nil {
			return ctrl.Result{}, err
		}
	}
	if p.Gateway.IsHA() {
		readyCount := 0
		for _, id := range result.ReadyGatewayIDs {
			for _, up := range bfdState.Up {
				if id == up {
					readyCount++
				}
			}
		}
		if v.Status.GatewayReadyCount != readyCount || v.Status.GatewayDesiredCount != result.DesiredGateways {
			v.Status.GatewayReadyCount, v.Status.GatewayDesiredCount = readyCount, result.DesiredGateways
			return ctrl.Result{RequeueAfter: time.Millisecond}, r.Status().Update(ctx, &v)
		}
		if readyCount == 0 {
			return r.state(ctx, &v, "Pending", "LocalGatewayBFDPending", "Waiting for a configured local gateway with BFD connectivity", false, false)
		}
	}
	if !result.Ready {
		return r.state(ctx, &v, "Pending", "LocalGatewayPending", "Waiting for local gateway configuration; remote peers do not gate this state", false, false)
	}
	// Kube-OVN creates source-policy routes for both local Subnets. Account for
	// exactly those routes rather than mistaking them for foreign destinations.
	routes := []ovn.Route{
		{CIDR: v.Spec.CIDR, NextHopIP: v.Spec.Gateway, Policy: "src-ip"},
		{CIDR: p.Gateway.TransitCIDR, NextHopIP: p.Gateway.RouterIP, Policy: "src-ip"},
	}
	if p.Gateway.IsHA() {
		byID := map[string]string{}
		for _, record := range v.Status.BFDResources {
			byID[record.GatewayID] = record.UUID
		}
		for _, member := range p.Gateway.Gateways {
			if byID[member.ID] == "" {
				return r.state(ctx, &v, "Pending", "LocalBFDConfigurationPending", "Waiting for all local BFD identities", false, true)
			}
			routes = append(routes, ovn.Route{CIDR: "0.0.0.0/0", NextHopIP: member.IP, Policy: "dst-ip", BFD: byID[member.ID]})
		}
	} else {
		routes = append(routes, ovn.Route{CIDR: "0.0.0.0/0", NextHopIP: p.Gateway.GatewayIP, Policy: "dst-ip"})
	}
	ready, err := r.Network.RoutesReady(ctx, r.Store.Config, p.VpcName, routes)
	if err != nil {
		return r.blocked(ctx, &v, err)
	}
	if !ready {
		return r.state(ctx, &v, "Pending", "LocalRoutesPending", "Waiting for the local OVN gateway route", false, true)
	}
	if p.Gateway.IsHA() {
		ready, err = r.BFD.EnsureECMPFields(ctx, r.Store.Config, p.Gateway, v.Status.BFDResources)
		if err != nil {
			return r.blocked(ctx, &v, err)
		}
		if !ready {
			return r.state(ctx, &v, "Pending", "LocalECMPFieldsPending", "Waiting for native route flow-hash fields", false, true)
		}
	}
	return r.state(ctx, &v, "LocalReady", "LocalConfigurationApplied", "Local network and gateway configured; remote reachability is observed by the routing layer", true, true)
}

func (r *Reconciler) remove(ctx context.Context, v *api.SiteVpc, p *siteplan.Plan) (ctrl.Result, error) {
	// Recheck on every pass. Consumers must stop admission before deletion;
	// observations cannot fence a concurrent workload creator.
	empty, err := r.Network.EndpointsEmpty(ctx, r.Store.Config, p.SubnetName)
	if err != nil {
		return r.blocked(ctx, v, err)
	}
	if !empty {
		return r.state(ctx, v, "Deleting", "LocalEndpointsPresent", "Drain local workload endpoints before detaching", false, false)
	}
	result, err := r.Gateway.EndpointsEmpty(ctx, p.Gateway, v.Status.GatewayResources)
	if err != nil {
		return r.blocked(ctx, v, err)
	}
	if !result.EndpointsEmpty {
		return r.state(ctx, v, "Deleting", "LocalEndpointsPresent", "Drain local workload and transit endpoints before detaching", false, false)
	}
	if v.Status.Operation == "" {
		v.Status.Operation = "DeleteGateway"
		return ctrl.Result{RequeueAfter: time.Millisecond}, r.Status().Update(ctx, v)
	}
	if v.Status.Operation == "DeleteGateway" {
		result, err = r.Gateway.Delete(ctx, p.Gateway, v.Status.GatewayResources)
		if err != nil {
			return r.blocked(ctx, v, err)
		}
		if !result.Absent {
			return r.state(ctx, v, "Deleting", "GatewayWithdrawalPending", "Waiting for local gateway removal and route withdrawal", false, false)
		}
		v.Status.GatewayResources = nil
		v.Status.Operation = "DeleteResources"
		return ctrl.Result{RequeueAfter: time.Millisecond}, r.Status().Update(ctx, v)
	}
	if v.Status.Operation != "DeleteResources" {
		return r.blocked(ctx, v, fmt.Errorf("unrecognized durable deletion operation"))
	}
	// Verify transit OVN endpoints too, after the gateway's CNI teardown.
	empty, err = r.Network.EndpointsEmpty(ctx, r.Store.Config, p.TransitName)
	if err != nil {
		return r.blocked(ctx, v, err)
	}
	if !empty {
		return r.state(ctx, v, "Deleting", "TransitTeardownPending", "Waiting for local transit endpoint removal", false, false)
	}
	if err = r.Store.WithdrawRoutes(ctx, p.VpcName, p.OwnerUID, recorded(v, "Vpc", p.VpcName)); err != nil {
		return r.blocked(ctx, v, err)
	}
	if p.Gateway.IsHA() {
		absent, err := r.BFD.DeleteBFD(ctx, r.Store.Config, p.Gateway, v.Status.BFDResources)
		if err != nil {
			return r.blocked(ctx, v, err)
		}
		if !absent {
			return r.state(ctx, v, "Deleting", "BFDWithdrawalPending", "Waiting for native route withdrawal and owned BFD removal", false, false)
		}
		if len(v.Status.BFDResources) != 0 {
			v.Status.BFDResources = nil
			return ctrl.Result{RequeueAfter: time.Millisecond}, r.Status().Update(ctx, v)
		}
	}
	for _, item := range [][2]string{{"Subnet", p.SubnetName}, {"Subnet", p.TransitName}, {"Vpc", p.VpcName}} {
		absent, deleteErr := r.Store.Delete(ctx, item[0], item[1], p.OwnerUID, recorded(v, item[0], item[1]))
		if deleteErr != nil {
			return r.blocked(ctx, v, deleteErr)
		}
		if !absent {
			return r.state(ctx, v, "Deleting", "LocalRemovalPending", "Waiting for local Kube-OVN resource removal", false, false)
		}
	}
	absent, err := r.Network.LocalAbsent(ctx, r.Store.Config, p.VpcName, p.SubnetName, p.TransitName)
	if err != nil {
		return r.blocked(ctx, v, err)
	}
	if !absent {
		return r.state(ctx, v, "Deleting", "LocalOVNRemovalPending", "Waiting for local OVN topology removal", false, false)
	}
	controllerutil.RemoveFinalizer(v, api.SiteFinalizer)
	return ctrl.Result{}, r.Update(ctx, v)
}

func recorded(v *api.SiteVpc, kind, name string) string {
	for _, item := range v.Status.Resources {
		if item.Kind == kind && item.Name == name {
			return item.UID
		}
	}
	return ""
}

func forget(v *api.SiteVpc, kind, name string) {
	kept := v.Status.Resources[:0]
	for _, item := range v.Status.Resources {
		if item.Kind != kind || item.Name != name {
			kept = append(kept, item)
		}
	}
	v.Status.Resources = kept
}

func sameReceipts(a, b []api.GatewayResourceRecord) bool {
	return (len(a) == 0 && len(b) == 0) || reflect.DeepEqual(a, b)
}

func sameBFDReceipts(a, b []api.BFDResourceRecord) bool {
	return (len(a) == 0 && len(b) == 0) || reflect.DeepEqual(a, b)
}

func setGatewayRoutes(p *siteplan.Plan, records []api.BFDResourceRecord, conflicts map[string]bool) {
	byID := map[string]string{}
	for _, record := range records {
		byID[record.GatewayID] = record.UUID
	}
	routes := []interface{}{}
	for _, member := range p.Gateway.Gateways {
		if byID[member.ID] != "" && !conflicts[member.IP] {
			routes = append(routes, map[string]interface{}{"policy": "policyDst", "cidr": "0.0.0.0/0", "nextHopIP": member.IP, "bfdId": byID[member.ID], "ecmpMode": "bfd"})
		}
	}
	_ = unstructured.SetNestedSlice(p.Vpc.Object, routes, "spec", "staticRoutes")
}
