// Package platformcontroller manages project-scoped VPC and Subnet intent.
// It writes location snapshots in the authority API, never remote Infra APIs.
package platformcontroller

import (
	"context"
	"fmt"
	"net/netip"
	"reflect"
	"sort"
	"time"

	api "globalvpc.io/controller/api/v1alpha2"
	"globalvpc.io/controller/internal/platformconfig"
	"globalvpc.io/controller/internal/platformplan"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type Reconciler struct {
	client.Client
	Config   platformconfig.Config
	Interval time.Duration
}

func (r *Reconciler) again() ctrl.Result {
	d := r.Interval
	if d < time.Second {
		d = 5 * time.Second
	}
	return ctrl.Result{RequeueAfter: d}
}

func condition(cs *[]metav1.Condition, generation int64, kind, reason, message string, ready bool) {
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(cs, metav1.Condition{Type: kind, Status: status, Reason: reason, Message: message, ObservedGeneration: generation})
}
func (r *Reconciler) vpcState(ctx context.Context, v *api.VPC, phase, reason, message string, ready bool) (ctrl.Result, error) {
	old := v.DeepCopy().Status
	v.Status.Phase = phase
	v.Status.ObservedGeneration = v.Generation
	condition(&v.Status.Conditions, v.Generation, "Ready", reason, message, ready)
	if !reflect.DeepEqual(old, v.Status) {
		if err := r.Status().Update(ctx, v); err != nil {
			return ctrl.Result{}, err
		}
	}
	return r.again(), nil
}
func (r *Reconciler) subnetState(ctx context.Context, s *api.Subnet, phase, reason, message string, ready bool) error {
	old := s.DeepCopy().Status
	s.Status.Phase = phase
	s.Status.ObservedGeneration = s.Generation
	condition(&s.Status.Conditions, s.Generation, "Ready", reason, message, ready)
	if !reflect.DeepEqual(old, s.Status) {
		return r.Status().Update(ctx, s)
	}
	return nil
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var v api.VPC
	if err := r.Get(ctx, req.NamespacedName, &v); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		var subnets api.SubnetList
		if err = r.List(ctx, &subnets, client.InNamespace(req.Namespace)); err != nil {
			return ctrl.Result{}, err
		}
		for i := range subnets.Items {
			s := &subnets.Items[i]
			if s.Spec.VPCRef == req.Name {
				if err = r.subnetState(ctx, s, "Blocked", "VPCNotFound", "Referenced VPC does not exist", false); err != nil {
					return ctrl.Result{}, err
				}
			}
		}
		return r.again(), nil
	}
	if !controllerutil.ContainsFinalizer(&v, platformplan.Finalizer) {
		if !v.DeletionTimestamp.IsZero() {
			return ctrl.Result{}, nil
		}
		controllerutil.AddFinalizer(&v, platformplan.Finalizer)
		return r.again(), r.Update(ctx, &v)
	}
	var all api.SubnetList
	ids, err := r.networkIDs(ctx, []string{"vpc/" + string(v.UID)})
	if err != nil {
		return ctrl.Result{}, err
	}
	networkID := ids["vpc/"+string(v.UID)]
	if v.Status.NetworkID != 0 && v.Status.NetworkID != networkID {
		return r.vpcState(ctx, &v, "Blocked", "RegistryIdentityChanged", "Accepted network ID no longer matches the retained registry", false)
	}
	if v.Status.NetworkID == 0 {
		v.Status.NetworkID = networkID
		return r.again(), r.Status().Update(ctx, &v)
	}
	if err := r.List(ctx, &all, client.InNamespace(v.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	subnets := []api.Subnet{}
	for _, s := range all.Items {
		if s.Spec.VPCRef == v.Name {
			subnets = append(subnets, s)
		}
	}
	sort.Slice(subnets, func(i, j int) bool {
		if !subnets[i].CreationTimestamp.Equal(&subnets[j].CreationTimestamp) {
			return subnets[i].CreationTimestamp.Before(&subnets[j].CreationTimestamp)
		}
		return subnets[i].Name < subnets[j].Name
	})
	var list api.NetworkBindingList
	if err := r.List(ctx, &list, client.MatchingLabels{platformplan.OwnerLabel: string(v.UID)}); err != nil {
		return ctrl.Result{}, err
	}
	bindings := list.Items
	// Forget a fully acknowledged tombstone receipt before deleting its authority
	// object. A subsequent join creates a new incarnation, never resurrects one.
	for i := range bindings {
		b := &bindings[i]
		if !b.Spec.Deleting || b.Status.Phase != "Deleted" || b.Status.AppliedRevision != b.Spec.Revision || len(b.Status.Resources) > 0 || len(b.Status.Gateways) > 0 {
			continue
		}
		var refs []api.BindingReference
		found := false
		for _, ref := range v.Status.Bindings {
			if ref.Name == b.Name && ref.LocationRef == b.Spec.LocationRef {
				if ref.UID != string(b.UID) {
					return r.vpcState(ctx, &v, "Blocked", "BindingReplaced", "A cleanup binding UID changed", false)
				}
				found = true
				continue
			}
			refs = append(refs, ref)
		}
		if found {
			v.Status.Bindings = refs
			return r.again(), r.Status().Update(ctx, &v)
		}
		return r.again(), r.Delete(ctx, b, client.Preconditions{UID: &b.UID, ResourceVersion: &b.ResourceVersion})
	}
	// Admission receipts are committed with the VPC resourceVersion before any
	// native object or binding can be created. Prefix conflicts survive restarts.
	claims := append([]api.PrefixClaim(nil), v.Status.SubnetClaims...)
	accepted := []api.Subnet{}
	for i := range subnets {
		s := &subnets[i]
		if !controllerutil.ContainsFinalizer(s, platformplan.Finalizer) {
			if !s.DeletionTimestamp.IsZero() {
				continue
			}
			controllerutil.AddFinalizer(s, platformplan.Finalizer)
			return r.again(), r.Update(ctx, s)
		}
		err := platformplan.ValidateSubnet(&v, s, r.Config)
		var own *api.PrefixClaim
		for j := range claims {
			if claims[j].SubnetUID == string(s.UID) {
				own = &claims[j]
				break
			}
		}
		if own != nil && (own.CIDR != s.Spec.CIDR || own.LocationRef != s.Spec.LocationRef) {
			err = fmt.Errorf("accepted subnet identity changed")
		}
		if err == nil && own == nil {
			for _, b := range bindings {
				if b.Spec.LocationRef == s.Spec.LocationRef && b.Spec.Deleting {
					err = fmt.Errorf("location is completing a previous detach; admission will retry")
					break
				}
			}
			if !v.DeletionTimestamp.IsZero() {
				err = fmt.Errorf("VPC is deleting; new subnets cannot be admitted")
			}
			p, _ := netip.ParsePrefix(s.Spec.CIDR)
			for _, c := range claims {
				previous, _ := netip.ParsePrefix(c.CIDR)
				if p.Overlaps(previous) {
					err = fmt.Errorf("CIDR overlaps an existing or draining subnet in this VPC")
					break
				}
			}
		}
		if err != nil {
			if !s.DeletionTimestamp.IsZero() && own == nil {
				controllerutil.RemoveFinalizer(s, platformplan.Finalizer)
				return r.again(), r.Update(ctx, s)
			}
			if updateErr := r.subnetState(ctx, s, "Blocked", "AdmissionRejected", err.Error(), false); updateErr != nil {
				return ctrl.Result{}, updateErr
			}
			continue
		}
		if own == nil {
			claims = append(claims, api.PrefixClaim{SubnetUID: string(s.UID), CIDR: s.Spec.CIDR, LocationRef: s.Spec.LocationRef})
		}
		accepted = append(accepted, *s)
	}
	if !reflect.DeepEqual(claims, v.Status.SubnetClaims) {
		v.Status.SubnetClaims = claims
		return r.again(), r.Status().Update(ctx, &v)
	}
	for i := range accepted {
		s := &accepted[i]
		if s.Status.VPCUID == "" {
			s.Status.VPCUID = string(v.UID)
			s.Status.NativeVpcName = platformplan.VpcName(string(v.UID), s.Spec.LocationRef)
			s.Status.NativeSubnetName = platformplan.SubnetName(string(s.UID))
			s.Status.BindingName = platformplan.BindingName(string(v.UID), s.Spec.LocationRef)
			return r.again(), r.Status().Update(ctx, s)
		}
	}
	desired, err := platformplan.Build(&v, accepted, bindings, r.Config)
	if err != nil {
		return r.vpcState(ctx, &v, "Blocked", "PlanRejected", err.Error(), false)
	}
	if err = r.assignTransport(ctx, &v, desired, bindings); err != nil {
		return ctrl.Result{}, err
	}
	for i := range desired {
		want := &desired[i]
		var current api.NetworkBinding
		err = r.Get(ctx, client.ObjectKeyFromObject(want), &current)
		if apierrors.IsNotFound(err) {
			for _, ref := range v.Status.Bindings {
				if ref.Name == want.Name && ref.LocationRef == want.Spec.LocationRef {
					return r.vpcState(ctx, &v, "Blocked", "BindingMissing", "An accepted binding disappeared without a cleanup acknowledgement", false)
				}
			}
			if err = r.Create(ctx, want); err != nil {
				return ctrl.Result{}, err
			}
			return r.again(), nil
		}
		if err != nil {
			return ctrl.Result{}, err
		}
		if current.Labels[platformplan.OwnerLabel] != string(v.UID) || current.Spec.VPCRef != want.Spec.VPCRef || current.Spec.LocationRef != want.Spec.LocationRef {
			return r.vpcState(ctx, &v, "Blocked", "OwnershipConflict", "An internal binding has a different owner", false)
		}
		for _, ref := range v.Status.Bindings {
			if ref.Name == current.Name && ref.LocationRef == current.Spec.LocationRef && ref.UID != string(current.UID) {
				return r.vpcState(ctx, &v, "Blocked", "BindingReplaced", "An accepted internal binding UID changed", false)
			}
		}
		if current.Spec.TransportProfile != want.Spec.TransportProfile {
			return r.vpcState(ctx, &v, "Blocked", "TransportMigrationRequired", "An existing VPC cannot change transport by editing its class", false)
		}
		if !reflect.DeepEqual(current.Spec, want.Spec) {
			current.Spec = want.Spec
			if err = r.Update(ctx, &current); err != nil {
				return ctrl.Result{}, err
			}
			return r.again(), nil
		}
		*want = current
	}
	var refs []api.BindingReference
	for _, b := range desired {
		refs = append(refs, api.BindingReference{LocationRef: b.Spec.LocationRef, Name: b.Name, UID: string(b.UID), NativeVpcName: b.Spec.NativeVpcName})
	}
	if !reflect.DeepEqual(refs, v.Status.Bindings) {
		v.Status.Bindings = refs
		return r.again(), r.Status().Update(ctx, &v)
	}
	allApplied := true
	for _, b := range desired {
		if b.Status.AppliedRevision != b.Spec.Revision {
			allApplied = false
		}
	}
	for i := range accepted {
		s := &accepted[i]
		var local *api.NetworkBinding
		for j := range desired {
			if desired[j].Spec.LocationRef == s.Spec.LocationRef {
				local = &desired[j]
				break
			}
		}
		if !s.DeletionTimestamp.IsZero() {
			if allApplied && local != nil && resourceAbsent(local, s.Status.NativeSubnetName) {
				controllerutil.RemoveFinalizer(s, platformplan.Finalizer)
				if err = r.Update(ctx, s); err != nil {
					return ctrl.Result{}, err
				}
				return r.again(), nil
			}
			if err = r.subnetState(ctx, s, "Deleting", "WithdrawalPending", "Waiting for endpoint drain and all location withdrawal acknowledgements", false); err != nil {
				return ctrl.Result{}, err
			}
			continue
		}
		ready := local != nil && local.Status.AppliedRevision == local.Spec.Revision && meta.IsStatusConditionTrue(local.Status.Conditions, "Ready")
		phase, reason, message := "Pending", "LocationPending", "Waiting for local network and cross-site configuration"
		if ready {
			phase, reason, message = "Ready", "LocationReady", "Local network and required connectivity are configured"
		}
		if err = r.subnetState(ctx, s, phase, reason, message, ready); err != nil {
			return ctrl.Result{}, err
		}
	}
	// A claim can be released only after its public object is gone and no snapshot
	// still contains that incarnation. Parent registry reservations are untouched.
	var kept []api.PrefixClaim
	for _, c := range claims {
		present := false
		for _, s := range subnets {
			if string(s.UID) == c.SubnetUID {
				present = true
			}
		}
		for _, b := range desired {
			for _, s := range b.Spec.Subnets {
				if s.UID == c.SubnetUID {
					present = true
				}
			}
		}
		if present || !allApplied {
			kept = append(kept, c)
		}
	}
	if !reflect.DeepEqual(kept, v.Status.SubnetClaims) {
		v.Status.SubnetClaims = kept
		return r.again(), r.Status().Update(ctx, &v)
	}
	if !v.DeletionTimestamp.IsZero() {
		if len(subnets) > 0 {
			return r.vpcState(ctx, &v, "Deleting", "SubnetsInUse", "Delete the VPC's subnets before deleting the VPC", false)
		}
		for _, b := range desired {
			if !b.Spec.Deleting || b.Status.AppliedRevision != b.Spec.Revision || len(b.Status.Resources) > 0 || len(b.Status.Gateways) > 0 {
				return r.vpcState(ctx, &v, "Deleting", "LocationCleanupPending", "Waiting for location cleanup acknowledgements", false)
			}
		}
		for i := range desired {
			b := &desired[i]
			if err = r.Delete(ctx, b, client.Preconditions{UID: &b.UID, ResourceVersion: &b.ResourceVersion}); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
		controllerutil.RemoveFinalizer(&v, platformplan.Finalizer)
		return ctrl.Result{}, r.Update(ctx, &v)
	}
	ready := allApplied && len(accepted) == len(subnets)
	for _, b := range desired {
		if !b.Spec.Deleting && !meta.IsStatusConditionTrue(b.Status.Conditions, "Ready") {
			ready = false
		}
	}
	if ready {
		return r.vpcState(ctx, &v, "Ready", "LocationsReady", "All requested locations have accepted the current network configuration", true)
	}
	return r.vpcState(ctx, &v, "Pending", "LocationsPending", "One or more subnets or locations require attention; existing locations reconcile independently", false)
}

func resourceAbsent(b *api.NetworkBinding, name string) bool {
	for _, r := range b.Status.Resources {
		if r.Kind == "Subnet" && r.Name == name {
			return false
		}
	}
	return true
}
