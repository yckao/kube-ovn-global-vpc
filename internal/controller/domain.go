package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"globalvpc.io/controller/internal/config"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const DomainLabel = "networking.globalvpc.io/domain"

// DomainGate serializes topology changes across reconciles and process restarts.
// Its durable lock is separate from controller leader election. A holder must
// finish or undo its IC transaction before another tenant can change topology.
type DomainGate struct {
	Authority client.Client
	Namespace string
	Config    config.Config
	Stores    map[string]ResourceStore
}

func (g *DomainGate) lockName() string {
	h := sha256.Sum256([]byte(g.Config.Domain))
	return "gvpc-domain-" + hex.EncodeToString(h[:8])
}
func (g *DomainGate) lock(ctx context.Context) (*corev1.ConfigMap, error) {
	var cm corev1.ConfigMap
	err := g.Authority.Get(ctx, types.NamespacedName{Namespace: g.Namespace, Name: g.lockName()}, &cm)
	return &cm, err
}
func (g *DomainGate) Holder(ctx context.Context) (string, error) {
	cm, err := g.lock(ctx)
	if errors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("domain lock query failed")
	}
	if cm.Labels[DomainLabel] != g.Config.Domain {
		return "", fmt.Errorf("domain lock ownership conflict")
	}
	return cm.Data["holder"], nil
}
func (g *DomainGate) Acquire(ctx context.Context, uid, hash string) (bool, error) {
	cm, err := g.lock(ctx)
	if errors.IsNotFound(err) {
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: g.lockName(), Namespace: g.Namespace, Labels: map[string]string{DomainLabel: g.Config.Domain}}, Data: map[string]string{"holder": uid, "planHash": hash}}
		if err = g.Authority.Create(ctx, cm); err != nil {
			return false, fmt.Errorf("domain lock creation failed")
		}
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("domain lock query failed")
	}
	if cm.Labels[DomainLabel] != g.Config.Domain {
		return false, fmt.Errorf("domain lock ownership conflict")
	}
	if cm.Data["holder"] != "" && cm.Data["holder"] != uid {
		return false, nil
	}
	if cm.Data["holder"] == uid {
		if cm.Data["planHash"] != hash {
			return false, fmt.Errorf("locked domain configuration changed")
		}
		return true, nil
	}
	cm.Data = map[string]string{"holder": uid, "planHash": hash}
	if err = g.Authority.Update(ctx, cm); err != nil {
		return false, fmt.Errorf("domain lock update failed")
	}
	return true, nil
}
func (g *DomainGate) deployment(ctx context.Context, c config.Cluster) (*appsv1.Deployment, error) {
	store, ok := g.Stores[c.Name]
	if !ok || store.Client == nil {
		return nil, fmt.Errorf("native IC Infra client is not configured")
	}
	if err := store.CheckIdentity(ctx); err != nil {
		return nil, err
	}
	var dep appsv1.Deployment
	if err := store.Client.Get(ctx, types.NamespacedName{Namespace: c.ICDeployment.Namespace, Name: c.ICDeployment.Name}, &dep); err != nil {
		return nil, fmt.Errorf("native IC Deployment query failed")
	}
	if string(dep.UID) != c.ICDeployment.UID || dep.Labels[DomainLabel] != g.Config.Domain || !dep.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("native IC Deployment ownership conflict")
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas < 0 || *dep.Spec.Replicas > 1 {
		return nil, fmt.Errorf("native IC Deployment must have zero or one replica")
	}
	// Only explicitly dedicated deployments may be paused. Never infer ownership
	// from an image or mutate a shared CNI/central deployment.
	if dep.Spec.Template.Labels[DomainLabel] != g.Config.Domain {
		return nil, fmt.Errorf("native IC Pod template ownership conflict")
	}
	return &dep, nil
}
func (g *DomainGate) SetPaused(ctx context.Context, uid string, paused bool) (bool, error) {
	holder, err := g.Holder(ctx)
	if err != nil {
		return false, err
	}
	if holder != uid {
		return false, fmt.Errorf("domain topology lock not held")
	}
	// Validate every registered target before changing replicas anywhere. A
	// foreign or replaced deployment in one Infra must not partially pause the
	// otherwise valid targets in this domain.
	deployments := make([]*appsv1.Deployment, len(g.Config.Clusters))
	for i, c := range g.Config.Clusters {
		dep, err := g.deployment(ctx, c)
		if err != nil {
			return false, err
		}
		deployments[i] = dep
	}
	all := true
	for i, c := range g.Config.Clusters {
		dep := deployments[i]
		desired := int32(1)
		if paused {
			desired = 0
		}
		if *dep.Spec.Replicas != desired {
			dep.Spec.Replicas = &desired
			if err = g.Stores[c.Name].Client.Update(ctx, dep); err != nil {
				return false, fmt.Errorf("native IC replica update failed")
			}
			all = false
		}
		if paused {
			// An empty Pod list is insufficient while the Deployment or an owned
			// ReplicaSet can still launch a native IC writer from stale desired
			// state. Wait for both controllers to observe the zero-replica spec.
			if dep.Status.ObservedGeneration < dep.Generation {
				all = false
			}
			// Terminating pods can still write OVN. Follow owner UIDs, not a
			// selector that could include another workload.
			var sets appsv1.ReplicaSetList
			if err = g.Stores[c.Name].Client.List(ctx, &sets, client.InNamespace(dep.Namespace)); err != nil {
				return false, fmt.Errorf("native IC ReplicaSet query failed")
			}
			ids := map[types.UID]bool{}
			for _, rs := range sets.Items {
				for _, r := range rs.OwnerReferences {
					if r.UID == dep.UID && r.Controller != nil && *r.Controller {
						ids[rs.UID] = true
						if rs.Spec.Replicas == nil || *rs.Spec.Replicas != 0 || rs.Status.ObservedGeneration < rs.Generation {
							all = false
						}
					}
				}
			}
			var pods corev1.PodList
			if err = g.Stores[c.Name].Client.List(ctx, &pods, client.InNamespace(dep.Namespace)); err != nil {
				return false, fmt.Errorf("native IC Pod query failed")
			}
			for _, pod := range pods.Items {
				for _, r := range pod.OwnerReferences {
					if ids[r.UID] && pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
						all = false
					}
				}
			}
		} else if dep.Status.ObservedGeneration < dep.Generation || dep.Status.ReadyReplicas != 1 || dep.Status.UpdatedReplicas != 1 {
			all = false
		}
	}
	return all, nil
}
func (g *DomainGate) Release(ctx context.Context, uid string) error {
	cm, err := g.lock(ctx)
	if err != nil {
		return fmt.Errorf("domain release query failed")
	}
	if cm.Labels[DomainLabel] != g.Config.Domain || cm.Data["holder"] != uid {
		return fmt.Errorf("domain release ownership conflict")
	}
	cm.Data = map[string]string{"holder": "", "planHash": ""}
	if err = g.Authority.Update(ctx, cm); err != nil {
		return fmt.Errorf("domain release failed")
	}
	return nil
}
