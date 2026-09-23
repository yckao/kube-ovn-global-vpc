// Package localcontroller realizes accepted location snapshots through native
// Kubernetes APIs. It has no OVN exec client and no access to another site's API.
package localcontroller

import (
	"context"
	"fmt"
	"net/netip"
	"reflect"
	"sort"
	"strings"
	"time"

	api "globalvpc.io/controller/api/v1alpha2"
	"globalvpc.io/controller/integration/kube-ovn/destinationroute"
	"globalvpc.io/controller/internal/localconfig"
	"globalvpc.io/controller/internal/platformplan"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const NativeOwnerLabel = "platform.globalvpc.io/binding-uid"

type GatewayResult struct {
	Gateways  []api.GatewayEndpoint
	Resources []api.ResourceRecord
	Ready     bool
	Reason    string
}
type Gateway interface {
	Ensure(context.Context, *api.NetworkBinding, Allocation) (GatewayResult, error)
	Delete(context.Context, *api.NetworkBinding) (bool, error)
}
type Reconciler struct {
	client.Client
	Config   localconfig.Config
	Gateway  Gateway
	Interval time.Duration
}

func (r *Reconciler) again() ctrl.Result {
	d := r.Interval
	if d < time.Second {
		d = 5 * time.Second
	}
	return ctrl.Result{RequeueAfter: d}
}
func (r *Reconciler) state(ctx context.Context, b *api.NetworkBinding, phase, reason, message string, ready, applied bool) (ctrl.Result, error) {
	old := b.DeepCopy().Status
	b.Status.Phase = phase
	b.Status.ObservedGeneration = b.Generation
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&b.Status.Conditions, metav1.Condition{Type: "Ready", Status: status, Reason: reason, Message: message, ObservedGeneration: b.Generation})
	if applied {
		b.Status.AppliedRevision = b.Spec.Revision
	}
	if !reflect.DeepEqual(old, b.Status) {
		if err := r.Status().Update(ctx, b); err != nil {
			return ctrl.Result{}, err
		}
	}
	return r.again(), nil
}
func (r *Reconciler) blocked(ctx context.Context, b *api.NetworkBinding, err error) (ctrl.Result, error) {
	return r.state(ctx, b, "Blocked", "LocalReconcileBlocked", err.Error(), false, false)
}
func native(kind, name string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetAPIVersion("kubeovn.io/v1")
	o.SetKind(kind)
	o.SetName(name)
	return o
}
func nativeSpec(kind, name, owner string, spec map[string]any) *unstructured.Unstructured {
	o := native(kind, name)
	o.SetLabels(map[string]string{NativeOwnerLabel: owner})
	o.Object["spec"] = spec
	return o
}
func transitName(b *api.NetworkBinding) string {
	return "pt-" + platformplan.Short(b.Spec.VPCRef.UID+"/"+b.Spec.LocationRef)
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var b api.NetworkBinding
	if err := r.Get(ctx, req.NamespacedName, &b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if b.Namespace != r.Config.Namespace || b.Spec.LocationRef != r.Config.LocationRef {
		return ctrl.Result{}, nil
	}
	if b.Annotations[SourceUIDAnnotation] == "" || b.Spec.Revision != platformplan.Revision(b.Spec) {
		return r.blocked(ctx, &b, fmt.Errorf("local snapshot is not an accepted authority revision"))
	}
	var ns corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: "kube-system"}, &ns); err != nil {
		return ctrl.Result{}, err
	}
	if string(ns.UID) != r.Config.ClusterUID {
		return r.blocked(ctx, &b, fmt.Errorf("local Infra identity changed"))
	}
	// Recover committed native creates whose status receipt was lost before any
	// endpoint check, route withdrawal, or gateway change. A tombstone can omit
	// its former subnet spec, so the binding UID inventory is authoritative here.
	if changed, err := r.recoverNativeSubnets(ctx, &b); err != nil {
		if changed {
			return ctrl.Result{}, err
		}
		return r.blocked(ctx, &b, err)
	} else if changed {
		return r.again(), nil
	}
	if b.Spec.Deleting {
		return r.remove(ctx, &b)
	}
	active := 0
	for _, s := range b.Spec.Subnets {
		if !s.Deleting {
			active++
			continue
		}
		if err := r.endpointsEmpty(ctx, s.NativeSubnetName); err != nil {
			return r.state(ctx, &b, "Pending", "SubnetInUse", err.Error(), false, false)
		}
	}
	// Authorization can retain a draining location until remote withdrawal is
	// acknowledged. Only actual remote exports require gateways; with none left,
	// routes([]) removes native BFD and a gateway rollout would wait forever for
	// unavailable siblings. Deleting exports remain in RemoteSubnets until the
	// source acknowledges native subnet removal, so this does not withdraw early
	// or depend on peer registration being complete during bootstrap.
	cross := active > 0 && len(b.Spec.RemoteSubnets) > 0
	if !cross {
		done, err := r.disconnect(ctx, &b)
		if err != nil {
			return r.blocked(ctx, &b, err)
		}
		if !done {
			return r.state(ctx, &b, "Pending", "GatewayWithdrawalPending", "Waiting for gateway and native route withdrawal", false, false)
		}
	}
	a, err := allocate(ctx, r.Client, r.Config, &b)
	if err != nil {
		return r.blocked(ctx, &b, err)
	}
	transit, _ := netip.ParsePrefix(a.TransitCIDR)
	source, _ := netip.ParseAddr(a.BFDSourceIP)
	for _, s := range b.Spec.Subnets {
		p, e := netip.ParsePrefix(s.CIDR)
		if e != nil || p.Overlaps(transit) || p.Contains(source) {
			return r.blocked(ctx, &b, fmt.Errorf("workload and allocated infrastructure ranges overlap"))
		}
	}
	for _, s := range b.Spec.RemoteSubnets {
		p, e := netip.ParsePrefix(s.CIDR)
		if e != nil || p.Overlaps(transit) || p.Contains(source) {
			return r.blocked(ctx, &b, fmt.Errorf("remote and allocated infrastructure ranges overlap"))
		}
	}
	spec := map[string]any{}
	if cross {
		spec["bfdPort"] = map[string]any{"enabled": true, "ip": a.BFDSourceIP, "nodeSelector": map[string]any{"matchLabels": stringMap(r.Config.GatewayNodeSelector)}}
	} else {
		spec["bfdPort"] = map[string]any{"enabled": false}
	}
	vpc := nativeSpec("Vpc", b.Spec.NativeVpcName, string(b.UID), spec)
	if changed, err := r.ensure(ctx, &b, vpc); err != nil {
		return r.blocked(ctx, &b, err)
	} else if changed {
		return r.again(), nil
	}
	for _, s := range b.Spec.Subnets {
		if s.Deleting {
			continue
		}
		obj := nativeSpec("Subnet", s.NativeSubnetName, string(b.UID), map[string]any{"vpc": b.Spec.NativeVpcName, "protocol": "IPv4", "cidrBlock": s.CIDR, "gateway": s.Gateway, "excludeIps": []any{s.Gateway}, "natOutgoing": false, "provider": "ovn"})
		if changed, err := r.ensure(ctx, &b, obj); err != nil {
			return r.blocked(ctx, &b, err)
		} else if changed {
			return r.again(), nil
		}
		ready, err := r.nativeReady(ctx, obj)
		if err != nil {
			return r.blocked(ctx, &b, err)
		}
		if !ready {
			return r.state(ctx, &b, "Pending", "SubnetPending", "Waiting for Kube-OVN to configure a workload subnet", false, false)
		}
	}
	if cross {
		obj := nativeSpec("Subnet", transitName(&b), string(b.UID), map[string]any{"vpc": b.Spec.NativeVpcName, "protocol": "IPv4", "cidrBlock": a.TransitCIDR, "gateway": a.RouterIP, "excludeIps": []any{a.RouterIP}, "natOutgoing": false, "provider": "ovn", "disableGatewayCheck": true})
		if changed, err := r.ensure(ctx, &b, obj); err != nil {
			return r.blocked(ctx, &b, err)
		} else if changed {
			return r.again(), nil
		}
		ready, err := r.nativeReady(ctx, obj)
		if err != nil {
			return r.blocked(ctx, &b, err)
		}
		if !ready {
			return r.state(ctx, &b, "Pending", "TransitPending", "Waiting for Kube-OVN to configure the gateway transit subnet", false, false)
		}
		if err = r.requireCapability(ctx); err != nil {
			return r.state(ctx, &b, "Pending", "NativeCapabilityRequired", err.Error(), false, false)
		}
		if r.Gateway == nil {
			return r.blocked(ctx, &b, fmt.Errorf("managed gateway backend is not configured"))
		}
		report, err := r.Gateway.Ensure(ctx, &b, a)
		if err != nil {
			return r.blocked(ctx, &b, err)
		}
		if changed, err := r.gatewayReport(ctx, &b, report); err != nil {
			return ctrl.Result{}, err
		} else if changed {
			return r.again(), nil
		}
		if len(report.Gateways) < 2 {
			return r.state(ctx, &b, "Pending", "GatewayRegistrationPending", report.Reason, false, false)
		}
		hops := []string{}
		for _, g := range report.Gateways {
			if g.TransitIP != "" {
				hops = append(hops, g.TransitIP)
			}
		}
		if len(hops) < 2 {
			return r.state(ctx, &b, "Pending", "GatewayRegistrationPending", "Waiting for two distinct local gateway addresses", false, false)
		}
		intents := []destinationroute.Intent{}
		for _, remote := range b.Spec.RemoteSubnets {
			intents = append(intents, destinationroute.Intent{CIDR: remote.CIDR, NextHops: hops, BFD: destinationroute.BFD{MinRX: 300, MinTX: 300, Multiplier: 3}, SelectionFields: []string{"ip_src", "ip_dst", "ip_proto", "tp_src", "tp_dst"}})
		}
		changed, ready, err := r.routes(ctx, &b, intents)
		if err != nil {
			return r.blocked(ctx, &b, err)
		}
		if changed {
			return r.again(), nil
		}
		if !ready {
			return r.state(ctx, &b, "Pending", "NativeRoutesPending", "Waiting for native destination-route transaction acknowledgement", false, false)
		}
		if !report.Ready {
			return r.state(ctx, &b, "Pending", "GatewayPending", report.Reason, false, false)
		}
	}
	for _, s := range b.Spec.Subnets {
		if !s.Deleting {
			continue
		}
		gone, err := r.deleteNative(ctx, &b, "Subnet", s.NativeSubnetName, true)
		if err != nil {
			return r.state(ctx, &b, "Pending", "SubnetInUse", err.Error(), false, false)
		}
		if !gone {
			return r.state(ctx, &b, "Pending", "SubnetDeleting", "Waiting for native subnet cleanup", false, false)
		}
	}
	return r.state(ctx, &b, "Ready", "LocalNetworkReady", "Native resources and required gateway configuration are ready", true, true)
}

// recoverNativeSubnets discovers native creates committed before their status
// receipt. It validates the complete inventory before recording any new UID, and
// never substitutes a new incarnation for an existing receipt.
func (r *Reconciler) recoverNativeSubnets(ctx context.Context, b *api.NetworkBinding) (bool, error) {
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion("kubeovn.io/v1")
	list.SetKind("SubnetList")
	if err := r.List(ctx, list, client.MatchingLabels{NativeOwnerLabel: string(b.UID)}); err != nil {
		return false, fmt.Errorf("cannot recover owned native subnet receipts: %w", err)
	}
	objects := map[string]*unstructured.Unstructured{}
	names := map[string]bool{transitName(b): true}
	for i := range list.Items {
		o := &list.Items[i]
		objects[o.GetName()] = o
		names[o.GetName()] = true
	}
	for _, subnet := range b.Spec.Subnets {
		names[subnet.NativeSubnetName] = true
	}
	for _, rec := range b.Status.Resources {
		if rec.APIVersion == "kubeovn.io/v1" && rec.Kind == "Subnet" {
			names[rec.Name] = true
		}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		if name != "" {
			ordered = append(ordered, name)
		}
	}
	sort.Strings(ordered)
	for _, name := range ordered {
		o := objects[name]
		if o == nil {
			o = native("Subnet", name)
			if err := r.Get(ctx, client.ObjectKey{Name: name}, o); apierrors.IsNotFound(err) {
				continue // Keep existing receipts; deletion/fenced-recovery handles absence.
			} else if err != nil {
				return false, err
			}
			objects[name] = o
		}
		if o.GetUID() == "" {
			return false, fmt.Errorf("native Subnet %s has no identity", name)
		}
		if err := guardNative(b, o); err != nil {
			return false, err
		}
	}
	changed := false
	for _, name := range ordered {
		o := objects[name]
		if o == nil {
			continue
		}
		recorded := false
		for _, rec := range b.Status.Resources {
			if rec.APIVersion == "kubeovn.io/v1" && rec.Kind == "Subnet" && rec.Name == name {
				recorded = true
				break
			}
		}
		if !recorded {
			b.Status.Resources = append(b.Status.Resources, api.ResourceRecord{APIVersion: "kubeovn.io/v1", Kind: "Subnet", Name: name, UID: string(o.GetUID())})
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	// Return changed even on an uncertain status write. The caller must stop this
	// pass so a failed or lost response cannot permit withdrawal before persistence.
	return true, r.Status().Update(ctx, b)
}

func stringMap(in map[string]string) map[string]any {
	out := map[string]any{}
	for k, v := range in {
		out[k] = v
	}
	return out
}
func (r *Reconciler) ensure(ctx context.Context, b *api.NetworkBinding, want *unstructured.Unstructured) (bool, error) {
	current := native(want.GetKind(), want.GetName())
	err := r.Get(ctx, client.ObjectKeyFromObject(current), current)
	if apierrors.IsNotFound(err) {
		for _, rec := range b.Status.Resources {
			if rec.APIVersion == "kubeovn.io/v1" && rec.Kind == want.GetKind() && rec.Name == want.GetName() {
				return false, fmt.Errorf("recorded native %s %s is missing; fenced recovery is required", want.GetKind(), want.GetName())
			}
		}
		if err = r.Create(ctx, want); err != nil {
			return false, err
		}
		current = want
	} else if err != nil {
		return false, err
	} else {
		if err = guardNative(b, current); err != nil {
			return false, err
		}
		changed := false
		fields, _, _ := unstructured.NestedMap(want.Object, "spec")
		for k, v := range fields {
			old, _, _ := unstructured.NestedFieldCopy(current.Object, "spec", k)
			if !reflect.DeepEqual(old, v) {
				if err = unstructured.SetNestedField(current.Object, v, "spec", k); err != nil {
					return false, err
				}
				changed = true
			}
		}
		if changed {
			if err = r.Update(ctx, current); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	found := false
	for _, rec := range b.Status.Resources {
		if rec.APIVersion == "kubeovn.io/v1" && rec.Kind == current.GetKind() && rec.Name == current.GetName() {
			found = true
		}
	}
	if !found {
		b.Status.Resources = append(b.Status.Resources, api.ResourceRecord{APIVersion: "kubeovn.io/v1", Kind: current.GetKind(), Name: current.GetName(), UID: string(current.GetUID())})
		return true, r.Status().Update(ctx, b)
	}
	return false, nil
}
func guardNative(b *api.NetworkBinding, o *unstructured.Unstructured) error {
	if o.GetLabels()[NativeOwnerLabel] != string(b.UID) {
		return fmt.Errorf("native %s %s has another owner", o.GetKind(), o.GetName())
	}
	for _, rec := range b.Status.Resources {
		if rec.APIVersion == "kubeovn.io/v1" && rec.Kind == o.GetKind() && rec.Name == o.GetName() && rec.UID != string(o.GetUID()) {
			return fmt.Errorf("native %s %s UID changed", o.GetKind(), o.GetName())
		}
	}
	return nil
}
func (r *Reconciler) nativeReady(ctx context.Context, obj *unstructured.Unstructured) (bool, error) {
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		return false, err
	}
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, raw := range conditions {
		c, ok := raw.(map[string]any)
		if ok && c["type"] == "Ready" && c["status"] == "True" {
			return true, nil
		}
	}
	return false, nil
}
func (r *Reconciler) requireCapability(ctx context.Context) error {
	crd := &unstructured.Unstructured{}
	crd.SetAPIVersion("apiextensions.k8s.io/v1")
	crd.SetKind("CustomResourceDefinition")
	if err := r.Get(ctx, client.ObjectKey{Name: "vpcs.kubeovn.io"}, crd); err != nil {
		return fmt.Errorf("cannot verify native Vpc destination-route API")
	}
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	for _, raw := range versions {
		v, ok := raw.(map[string]any)
		if !ok || v["served"] != true {
			continue
		}
		field, found, _ := unstructured.NestedMap(v, "schema", "openAPIV3Schema", "properties", "spec", "properties", "destinationRoutes")
		if found && field["type"] == "array" {
			return nil
		}
	}
	return fmt.Errorf("install the source-pinned Kube-OVN destination-routes.v1 extension; legacy OVN command patching is not enabled")
}
func (r *Reconciler) routes(ctx context.Context, b *api.NetworkBinding, intents []destinationroute.Intent) (bool, bool, error) {
	plan, err := destinationroute.Compile(intents)
	if err != nil {
		return false, false, err
	}
	value := make([]any, 0, len(plan.Routes))
	for _, route := range plan.Routes {
		hops, fields := []any{}, []any{}
		for _, h := range route.NextHops {
			hops = append(hops, h)
		}
		for _, f := range route.SelectionFields {
			fields = append(fields, f)
		}
		value = append(value, map[string]any{"cidr": route.CIDR, "nextHops": hops, "bfd": map[string]any{"minRX": int64(route.BFD.MinRX), "minTX": int64(route.BFD.MinTX), "multiplier": int64(route.BFD.Multiplier)}, "selectionFields": fields})
	}
	o := native("Vpc", b.Spec.NativeVpcName)
	if err = r.Get(ctx, client.ObjectKeyFromObject(o), o); err != nil {
		return false, false, err
	}
	if err = guardNative(b, o); err != nil {
		return false, false, err
	}
	old, _, _ := unstructured.NestedSlice(o.Object, "spec", "destinationRoutes")
	if len(old) != 0 || len(value) != 0 {
		if !reflect.DeepEqual(old, value) {
			if err = unstructured.SetNestedSlice(o.Object, value, "spec", "destinationRoutes"); err != nil {
				return false, false, err
			}
			return true, false, r.Update(ctx, o)
		}
	}
	status, _, _ := unstructured.NestedMap(o.Object, "status", "destinationRoutes")
	ready := status["capability"] == destinationroute.Capability && status["ready"] == true && status["appliedHash"] == plan.Hash
	generation, _, _ := unstructured.NestedInt64(o.Object, "status", "destinationRoutes", "observedGeneration")
	return false, ready && generation == o.GetGeneration(), nil
}
func (r *Reconciler) gatewayReport(ctx context.Context, b *api.NetworkBinding, report GatewayResult) (bool, error) {
	var resources []api.ResourceRecord
	for _, rec := range b.Status.Resources {
		if rec.APIVersion == "kubeovn.io/v1" {
			resources = append(resources, rec)
		}
	}
	resources = append(resources, report.Resources...)
	sort.Slice(resources, func(i, j int) bool {
		return resources[i].APIVersion+resources[i].Kind+resources[i].Name < resources[j].APIVersion+resources[j].Kind+resources[j].Name
	})
	if reflect.DeepEqual(resources, b.Status.Resources) && reflect.DeepEqual(report.Gateways, b.Status.Gateways) {
		return false, nil
	}
	b.Status.Resources = resources
	b.Status.Gateways = report.Gateways
	return true, r.Status().Update(ctx, b)
}
func (r *Reconciler) endpointsEmpty(ctx context.Context, name string) error {
	var pods corev1.PodList
	if err := r.List(ctx, &pods); err != nil {
		return err
	}
	for _, p := range pods.Items {
		for k, v := range p.Annotations {
			if strings.HasSuffix(k, "/logical_switch") && v == name {
				return fmt.Errorf("subnet still has a referencing Pod")
			}
		}
	}
	ips := &unstructured.UnstructuredList{}
	ips.SetAPIVersion("kubeovn.io/v1")
	ips.SetKind("IPList")
	if err := r.List(ctx, ips); err != nil {
		return fmt.Errorf("cannot verify native subnet IP allocation cleanup")
	}
	for _, ip := range ips.Items {
		subnet, _, _ := unstructured.NestedString(ip.Object, "spec", "subnet")
		if subnet == name {
			return fmt.Errorf("subnet still has a native IP allocation")
		}
		attached, _, _ := unstructured.NestedStringSlice(ip.Object, "spec", "attachSubnets")
		for _, s := range attached {
			if s == name {
				return fmt.Errorf("subnet still has an attached native IP allocation")
			}
		}
	}
	return nil
}
func (r *Reconciler) deleteNative(ctx context.Context, b *api.NetworkBinding, kind, name string, checkEndpoints bool) (bool, error) {
	o := native(kind, name)
	err := r.Get(ctx, client.ObjectKeyFromObject(o), o)
	if apierrors.IsNotFound(err) {
		var kept []api.ResourceRecord
		changed := false
		for _, rec := range b.Status.Resources {
			if rec.APIVersion == "kubeovn.io/v1" && rec.Kind == kind && rec.Name == name {
				changed = true
				continue
			}
			kept = append(kept, rec)
		}
		if changed {
			b.Status.Resources = kept
			return false, r.Status().Update(ctx, b)
		}
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if err = guardNative(b, o); err != nil {
		return false, err
	}
	if checkEndpoints {
		if err = r.endpointsEmpty(ctx, name); err != nil {
			return false, err
		}
	}
	if !o.GetDeletionTimestamp().IsZero() {
		return false, nil
	}
	uid, rv := o.GetUID(), o.GetResourceVersion()
	return false, r.Delete(ctx, o, client.Preconditions{UID: &uid, ResourceVersion: &rv})
}
func (r *Reconciler) remove(ctx context.Context, b *api.NetworkBinding) (ctrl.Result, error) {
	for _, rec := range b.Status.Resources {
		if rec.APIVersion == "kubeovn.io/v1" && rec.Kind == "Subnet" && rec.Name != transitName(b) {
			if err := r.endpointsEmpty(ctx, rec.Name); err != nil {
				return r.state(ctx, b, "Deleting", "SubnetInUse", err.Error(), false, false)
			}
		}
	}
	o := native("Vpc", b.Spec.NativeVpcName)
	err := r.Get(ctx, client.ObjectKeyFromObject(o), o)
	if err != nil && !apierrors.IsNotFound(err) {
		return r.blocked(ctx, b, err)
	}
	if err == nil {
		if err = guardNative(b, o); err != nil {
			return r.blocked(ctx, b, err)
		}
		status, found, _ := unstructured.NestedMap(o.Object, "status", "destinationRoutes")
		raw, _, _ := unstructured.NestedSlice(o.Object, "spec", "destinationRoutes")
		if len(raw) > 0 || found || len(b.Status.Gateways) > 0 {
			_ = status
			changed, ready, e := r.routes(ctx, b, nil)
			if e != nil {
				return r.blocked(ctx, b, e)
			}
			if changed || !ready {
				return r.state(ctx, b, "Deleting", "RouteWithdrawalPending", "Waiting for native route and BFD withdrawal acknowledgement", false, false)
			}
		}
	}
	if r.Gateway != nil {
		gone, e := r.Gateway.Delete(ctx, b)
		if e != nil {
			return r.blocked(ctx, b, e)
		}
		if !gone {
			return r.state(ctx, b, "Deleting", "GatewayCleanupPending", "Waiting for owned gateway resources to terminate", false, false)
		}
	} else if len(b.Status.Gateways) > 0 {
		return r.blocked(ctx, b, fmt.Errorf("gateway cleanup backend unavailable"))
	}
	if changed, e := r.gatewayReport(ctx, b, GatewayResult{}); e != nil {
		return ctrl.Result{}, e
	} else if changed {
		return r.again(), nil
	}
	for _, rec := range append([]api.ResourceRecord(nil), b.Status.Resources...) {
		if rec.APIVersion != "kubeovn.io/v1" || rec.Kind != "Subnet" {
			continue
		}
		gone, e := r.deleteNative(ctx, b, "Subnet", rec.Name, true)
		if e != nil {
			return r.state(ctx, b, "Deleting", "SubnetInUse", e.Error(), false, false)
		}
		if !gone {
			return r.again(), nil
		}
	}
	gone, err := r.deleteNative(ctx, b, "Vpc", b.Spec.NativeVpcName, false)
	if err != nil {
		return r.blocked(ctx, b, err)
	}
	if !gone {
		return r.again(), nil
	}
	return r.state(ctx, b, "Deleted", "CleanupComplete", "Owned native and gateway resources are absent; allocation receipts retained", false, true)
}

func (r *Reconciler) disconnect(ctx context.Context, b *api.NetworkBinding) (bool, error) {
	o := native("Vpc", b.Spec.NativeVpcName)
	err := r.Get(ctx, client.ObjectKeyFromObject(o), o)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	if err == nil {
		if err = guardNative(b, o); err != nil {
			return false, err
		}
		_, found, _ := unstructured.NestedMap(o.Object, "status", "destinationRoutes")
		raw, _, _ := unstructured.NestedSlice(o.Object, "spec", "destinationRoutes")
		if len(raw) > 0 || found || len(b.Status.Gateways) > 0 {
			changed, ready, e := r.routes(ctx, b, nil)
			if e != nil {
				return false, e
			}
			if changed || !ready {
				return false, nil
			}
		}
	}
	if r.Gateway != nil {
		gone, e := r.Gateway.Delete(ctx, b)
		if e != nil || !gone {
			return false, e
		}
		changed, e := r.gatewayReport(ctx, b, GatewayResult{})
		if e != nil || changed {
			return false, e
		}
	} else if len(b.Status.Gateways) > 0 {
		return false, fmt.Errorf("gateway cleanup backend is unavailable")
	}
	for _, rec := range b.Status.Resources {
		if rec.Kind == "Subnet" && rec.Name == transitName(b) {
			return r.deleteNative(ctx, b, "Subnet", rec.Name, true)
		}
	}
	return true, nil
}
