// Package controller reconciles one authoritative GlobalVpc API into a registered
// OVN-IC domain. It deliberately does not provision compute or a global HA store.
package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	api "globalvpc.io/controller/api/v1alpha1"
	"globalvpc.io/controller/internal/config"
	"globalvpc.io/controller/internal/ovn"
	"globalvpc.io/controller/internal/plan"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type IPAM interface {
	Ensure(context.Context, api.PrefixClaim) (api.Allocation, error)
}
type Network interface {
	InspectTransit(context.Context, []string, string, string, string) (string, error)
	Check(context.Context, config.Cluster) error
	Mark(context.Context, config.Cluster, string) error
	Gateways(context.Context, config.Cluster, string) error
	EnsureTransit(context.Context, []string, string, string, string) (string, error)
	DeleteTransit(context.Context, []string, string, string, string) error
	EndpointsEmpty(context.Context, config.Cluster, string) (bool, error)
	LocalAbsent(context.Context, config.Cluster, string, string, string) (bool, error)
	Connected(context.Context, config.Cluster, string, string, []string) (bool, error)
	RoutesReady(context.Context, config.Cluster, string, []ovn.Route) (bool, error)
}
type Gate interface {
	Holder(context.Context) (string, error)
	Acquire(context.Context, string, string) (bool, error)
	SetPaused(context.Context, string, bool) (bool, error)
	Release(context.Context, string) error
}
type Reconciler struct {
	client.Client
	Config    config.Config
	Stores    map[string]ResourceStore
	Providers map[string]IPAM
	Network   Network
	Gate      Gate
	Interval  time.Duration
}

func (r *Reconciler) interval() time.Duration {
	if r.Interval <= 0 {
		return 10 * time.Second
	}
	return r.Interval
}
func (r *Reconciler) save(ctx context.Context, v *api.GlobalVpc) error {
	return r.Status().Update(ctx, v)
}
func (r *Reconciler) state(ctx context.Context, v *api.GlobalVpc, phase, reason, message string, ready bool) (ctrl.Result, error) {
	before := v.Status
	before.Conditions = append([]metav1.Condition(nil), v.Status.Conditions...)
	v.Status.Phase = phase
	v.Status.ObservedGeneration = v.Generation
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&v.Status.Conditions, metav1.Condition{Type: "Ready", Status: status, Reason: reason, Message: message, ObservedGeneration: v.Generation})
	if !reflect.DeepEqual(before, v.Status) {
		if err := r.save(ctx, v); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: r.interval()}, nil
}
func (r *Reconciler) failure(ctx context.Context, v *api.GlobalVpc, err error) (ctrl.Result, error) {
	// The adapters expose only sanitized operational diagnostics.
	return r.state(ctx, v, "Blocked", "ReconcileBlocked", err.Error(), false)
}
func (r *Reconciler) Reconcile(parent context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	var v api.GlobalVpc
	if err := r.Get(ctx, req.NamespacedName, &v); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if v.Spec.DomainRef != r.Config.Domain {
		return ctrl.Result{}, nil
	}
	if v.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(&v, api.Finalizer) {
		controllerutil.AddFinalizer(&v, api.Finalizer)
		return ctrl.Result{RequeueAfter: time.Millisecond}, r.Update(ctx, &v)
	}
	p, err := plan.Build(&v, r.Config)
	if err != nil {
		return r.failure(ctx, &v, err)
	}
	if v.Status.PlanHash != "" && v.Status.PlanHash != p.Hash {
		return r.failure(ctx, &v, fmt.Errorf("accepted intent or domain registration changed; restore the accepted configuration"))
	}
	if v.Status.PlanHash == "" {
		v.Status.PlanHash = p.Hash
		for _, s := range p.Attachments {
			v.Status.Attachments = append(v.Status.Attachments, api.AttachmentStatus{ClusterRef: s.Cluster.Name, VpcName: s.VpcName, SubnetName: s.SubnetName, TransitSwitchName: s.TransitName})
		}
		if err = r.save(ctx, &v); err != nil {
			return ctrl.Result{}, err
		}
	}
	holder, err := r.Gate.Holder(ctx)
	if err != nil {
		return r.failure(ctx, &v, err)
	}
	if holder != "" && holder != p.UID {
		return r.state(ctx, &v, "Pending", "DomainBusy", "Another VPC is completing a domain topology operation", false)
	}
	if _, err = r.Network.InspectTransit(ctx, r.Config.ICNBCommand, p.TransitSwitch, p.UID, v.Status.TransitSwitchUID); err != nil {
		return r.failure(ctx, &v, err)
	}
	// Verify all API identities before the first external write. Every resource
	// mutation repeats the identity check against an uncached remote client.
	for _, s := range p.Attachments {
		store, ok := r.Stores[s.Cluster.Name]
		if !ok {
			return r.failure(ctx, &v, fmt.Errorf("Infra client is not configured"))
		}
		if err = store.CheckIdentity(ctx); err != nil {
			return r.failure(ctx, &v, err)
		}
		for _, desired := range []*unstructured.Unstructured{s.Vpc, s.Subnet, s.Transit} {
			current, readErr := store.Get(ctx, desired.GetKind(), desired.GetName())
			if apierrors.IsNotFound(readErr) {
				continue
			}
			if readErr != nil {
				return r.failure(ctx, &v, fmt.Errorf("owned resource preflight query failed"))
			}
			if err = owned(current, p.UID, s.Cluster.UID, recorded(&v, s.Cluster.Name, desired.GetKind(), desired.GetName())); err != nil {
				return r.failure(ctx, &v, err)
			}
		}
	}
	if !v.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&v, api.Finalizer) {
			return ctrl.Result{}, nil
		}
		return r.remove(ctx, &v, p)
	}
	if v.Status.Operation == "Resume" {
		return r.resume(ctx, &v, false)
	}
	for _, s := range p.Attachments {
		if err = r.Network.Check(ctx, s.Cluster); err != nil {
			return r.failure(ctx, &v, err)
		}
	}
	for _, claim := range p.Claims {
		found := false
		for _, allocation := range v.Status.Allocations {
			if allocation.ClaimUID == claim.ClaimUID {
				if allocation.PrefixClaim != claim || allocation.AllocationID == "" || allocation.ReleasePolicy != "Retain" {
					return r.failure(ctx, &v, fmt.Errorf("persisted IPAM receipt does not match the accepted claim"))
				}
				found = true
				break
			}
		}
		if found {
			continue
		}
		provider, ok := r.Providers[v.Spec.IPAM.ProviderRef]
		if !ok {
			return r.failure(ctx, &v, fmt.Errorf("IPAM provider is not configured"))
		}
		allocation, err := provider.Ensure(ctx, claim)
		if err != nil {
			return r.failure(ctx, &v, err)
		}
		if allocation.PrefixClaim != claim || allocation.AllocationID == "" || allocation.ReleasePolicy != "Retain" {
			return r.failure(ctx, &v, fmt.Errorf("IPAM receipt does not match the accepted claim"))
		}
		v.Status.Allocations = append(v.Status.Allocations, allocation)
		if err = r.save(ctx, &v); err != nil {
			return ctrl.Result{}, err
		}
	}
	for _, s := range p.Attachments {
		for _, desired := range []*unstructured.Unstructured{s.Vpc, s.Subnet, s.Transit} {
			uid, err := r.Stores[s.Cluster.Name].Ensure(ctx, desired, recorded(&v, s.Cluster.Name, desired.GetKind(), desired.GetName()))
			if errors.Is(err, ErrRecordedResourceMissing) {
				updateRecord(&v, s.Cluster.Name, desired.GetKind(), desired.GetName(), "")
				if err = r.save(ctx, &v); err != nil {
					return ctrl.Result{}, err
				}
				return r.state(ctx, &v, "Pending", "ResourceRecreationPending", "Recorded resource absence persisted; recreating on the next pass", false)
			}
			if err != nil {
				return r.failure(ctx, &v, err)
			}
			if updateRecord(&v, s.Cluster.Name, desired.GetKind(), desired.GetName(), uid) {
				if err = r.save(ctx, &v); err != nil {
					return ctrl.Result{}, err
				}
			}
		}
	}
	for _, s := range p.Attachments {
		for _, name := range []string{s.SubnetName, s.TransitName} {
			ready, err := r.Stores[s.Cluster.Name].Ready(ctx, "Subnet", name, p.UID, recorded(&v, s.Cluster.Name, "Subnet", name))
			if err != nil {
				return r.failure(ctx, &v, err)
			}
			if !ready {
				return r.state(ctx, &v, "Pending", "LocalNetworkPending", "Waiting for Kube-OVN Subnets", false)
			}
		}
	}
	bootstrap := v.Status.TransitSwitchUID == "" || holder == p.UID
	if !bootstrap {
		_, err = r.Network.EnsureTransit(ctx, r.Config.ICNBCommand, p.TransitSwitch, p.UID, v.Status.TransitSwitchUID)
		if errors.Is(err, ovn.ErrTransitMissing) {
			bootstrap = true
		} else if err != nil {
			return r.failure(ctx, &v, err)
		}
	}
	if bootstrap {
		locked, err := r.Gate.Acquire(ctx, p.UID, p.Hash)
		if err != nil {
			return r.failure(ctx, &v, err)
		}
		if !locked {
			return r.state(ctx, &v, "Pending", "DomainBusy", "Waiting for domain topology lock", false)
		}
		paused, err := r.Gate.SetPaused(ctx, p.UID, true)
		if err != nil {
			return r.failure(ctx, &v, err)
		}
		if !paused {
			return r.state(ctx, &v, "Pending", "QuiescingIC", "Waiting for all dedicated native IC processes to stop", false)
		}
		for _, s := range p.Attachments {
			if err = r.Network.Mark(ctx, s.Cluster, p.TransitSwitch); err != nil {
				return r.failure(ctx, &v, err)
			}
		}
		// Empty expected UUID permits lost-response discovery by ownership. If a
		// recorded row still exists its UUID must continue to match.
		uid, err := r.Network.EnsureTransit(ctx, r.Config.ICNBCommand, p.TransitSwitch, p.UID, v.Status.TransitSwitchUID)
		if errors.Is(err, ovn.ErrTransitMissing) {
			// Persist observed absence before allowing replacement, so a lost
			// creation response cannot strand a new row behind an old UUID.
			v.Status.TransitSwitchUID = ""
			if err = r.save(ctx, &v); err != nil {
				return ctrl.Result{}, err
			}
			return r.state(ctx, &v, "Pending", "TransitRecreationPending", "Transit absence persisted; recreating while native IC remains paused", false)
		}
		if err != nil {
			return r.failure(ctx, &v, err)
		}
		v.Status.TransitSwitchUID = uid
		for _, s := range p.Attachments {
			if err = r.Network.Gateways(ctx, s.Cluster, s.VpcName+"-"+p.TransitSwitch); err != nil {
				return r.failure(ctx, &v, err)
			}
		}
		v.Status.Operation = "Resume"
		if err = r.save(ctx, &v); err != nil {
			return ctrl.Result{}, err
		}
		return r.resume(ctx, &v, false)
	}
	for _, s := range p.Attachments {
		if err = r.Network.Mark(ctx, s.Cluster, p.TransitSwitch); err != nil {
			return r.failure(ctx, &v, err)
		}
		if err = r.Network.Gateways(ctx, s.Cluster, s.VpcName+"-"+p.TransitSwitch); err != nil {
			return r.failure(ctx, &v, err)
		}
		peers := []string{}
		for _, other := range p.Attachments {
			if other.Cluster.Name != s.Cluster.Name {
				peers = append(peers, other.VpcName+"-"+p.TransitSwitch)
			}
		}
		ready, err := r.Network.Connected(ctx, s.Cluster, p.TransitSwitch, s.VpcName+"-"+p.TransitSwitch, peers)
		if err != nil {
			return r.failure(ctx, &v, err)
		}
		if !ready {
			return r.state(ctx, &v, "Pending", "InterconnectPending", "Waiting for native IC tunnel keys and remote ports", false)
		}
		routes := []ovn.Route{}
		for _, other := range p.Attachments {
			if other.Cluster.Name != s.Cluster.Name {
				routes = append(routes, ovn.Route{CIDR: other.Intent.CIDR, NextHopIP: other.Intent.TransitIP})
			}
		}
		ready, err = r.Network.RoutesReady(ctx, s.Cluster, s.VpcName, routes)
		if err != nil {
			return r.failure(ctx, &v, err)
		}
		if !ready {
			return r.state(ctx, &v, "Pending", "RoutesPending", "Waiting for Kube-OVN static route convergence", false)
		}
	}
	return r.state(ctx, &v, "Ready", "ConfigurationConverged", "Subnets report Ready; transit ports and static routes converged; dataplane probes are separate", true)
}
func recorded(v *api.GlobalVpc, cluster, kind, name string) string {
	for _, x := range v.Status.Resources {
		if x.ClusterRef == cluster && x.Kind == kind && x.Name == name {
			return x.UID
		}
	}
	return ""
}
func updateRecord(v *api.GlobalVpc, cluster, kind, name, uid string) bool {
	for i, x := range v.Status.Resources {
		if x.ClusterRef == cluster && x.Kind == kind && x.Name == name {
			if x.UID == uid {
				return false
			}
			v.Status.Resources[i].UID = uid
			return true
		}
	}
	v.Status.Resources = append(v.Status.Resources, api.ResourceRecord{ClusterRef: cluster, Kind: kind, Name: name, UID: uid})
	return true
}
func (r *Reconciler) resume(ctx context.Context, v *api.GlobalVpc, deleting bool) (ctrl.Result, error) {
	holder, err := r.Gate.Holder(ctx)
	if err != nil {
		return r.failure(ctx, v, err)
	}
	if holder != "" {
		ready, err := r.Gate.SetPaused(ctx, string(v.UID), false)
		if err != nil {
			return r.failure(ctx, v, err)
		}
		if !ready {
			return r.state(ctx, v, "Pending", "ResumingIC", "Waiting for dedicated native IC Deployments to resume", false)
		}
		if err = r.Gate.Release(ctx, string(v.UID)); err != nil {
			return r.failure(ctx, v, err)
		}
	}
	if deleting {
		controllerutil.RemoveFinalizer(v, api.Finalizer)
		return ctrl.Result{}, r.Update(ctx, v)
	}
	v.Status.Operation = ""
	if err = r.save(ctx, v); err != nil {
		return ctrl.Result{}, err
	}
	return r.state(ctx, v, "Pending", "InterconnectPending", "Native IC resumed; waiting for topology convergence", false)
}
func (r *Reconciler) remove(ctx context.Context, v *api.GlobalVpc, p *plan.Plan) (ctrl.Result, error) {
	if v.Status.Operation == "Resume" {
		return r.resume(ctx, v, false)
	}
	if v.Status.Operation == "DeleteResume" {
		return r.resume(ctx, v, true)
	}
	// Never remove consumer workloads or bypass their lifecycle. A tenant subnet
	// containing endpoints keeps its VPC finalizer and existing dataplane intact.
	for _, s := range p.Attachments {
		for _, name := range []string{s.SubnetName, s.TransitName} {
			empty, err := r.Network.EndpointsEmpty(ctx, s.Cluster, name)
			if err != nil {
				return r.failure(ctx, v, err)
			}
			if !empty {
				return r.state(ctx, v, "Deleting", "ConsumersPresent", "Drain all tenant subnet endpoints before deletion", false)
			}
		}
	}
	locked, err := r.Gate.Acquire(ctx, p.UID, p.Hash)
	if err != nil {
		return r.failure(ctx, v, err)
	}
	if !locked {
		return r.state(ctx, v, "Deleting", "DomainBusy", "Waiting for domain topology lock", false)
	}
	paused, err := r.Gate.SetPaused(ctx, p.UID, true)
	if err != nil {
		return r.failure(ctx, v, err)
	}
	if !paused {
		return r.state(ctx, v, "Deleting", "QuiescingIC", "Waiting for native IC processes to stop", false)
	}
	// Draining can span several reconciles. Check again after the stop barrier
	// before withdrawing anything; resume the domain if a consumer appeared.
	for _, s := range p.Attachments {
		for _, name := range []string{s.SubnetName, s.TransitName} {
			empty, checkErr := r.Network.EndpointsEmpty(ctx, s.Cluster, name)
			if checkErr != nil {
				return r.failure(ctx, v, checkErr)
			}
			if !empty {
				v.Status.Operation = "Resume"
				if err = r.save(ctx, v); err != nil {
					return ctrl.Result{}, err
				}
				return r.resume(ctx, v, false)
			}
		}
	}
	for _, s := range p.Attachments {
		if err = r.Stores[s.Cluster.Name].WithdrawRoutes(ctx, s.VpcName, p.UID, recorded(v, s.Cluster.Name, "Vpc", s.VpcName)); err != nil {
			return r.failure(ctx, v, err)
		}
	}
	if err = r.Network.DeleteTransit(ctx, r.Config.ICNBCommand, p.TransitSwitch, p.UID, v.Status.TransitSwitchUID); err != nil {
		return r.failure(ctx, v, err)
	}
	for _, kind := range []string{"Subnet", "Vpc"} {
		all := true
		for _, s := range p.Attachments {
			names := []string{s.VpcName}
			if kind == "Subnet" {
				names = []string{s.SubnetName, s.TransitName}
			}
			for _, name := range names {
				gone, err := r.Stores[s.Cluster.Name].Delete(ctx, kind, name, p.UID, recorded(v, s.Cluster.Name, kind, name))
				if err != nil {
					return r.failure(ctx, v, err)
				}
				all = all && gone
			}
		}
		if !all {
			return r.state(ctx, v, "Deleting", "ResourcesDeleting", "Waiting for owned Kube-OVN resources to disappear", false)
		}
	}
	for _, s := range p.Attachments {
		gone, err := r.Network.LocalAbsent(ctx, s.Cluster, s.VpcName, s.SubnetName, s.TransitName)
		if err != nil {
			return r.failure(ctx, v, err)
		}
		if !gone {
			return r.state(ctx, v, "Deleting", "OVNCleanupPending", "Waiting for Kube-OVN to remove local OVN objects", false)
		}
	}
	v.Status.Operation = "DeleteResume"
	if err = r.save(ctx, v); err != nil {
		return ctrl.Result{}, err
	}
	return r.resume(ctx, v, true)
}
