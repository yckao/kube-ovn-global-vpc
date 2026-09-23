package controller

import (
	"context"
	stderrors "errors"
	"fmt"
	"reflect"

	api "globalvpc.io/controller/api/v1alpha1"
	"globalvpc.io/controller/internal/config"
	corev1 "k8s.io/api/core/v1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var ErrRecordedResourceMissing = stderrors.New("recorded resource is absent; persist rediscovery intent before recreating")

// ResourceStore uses uncached reads and optimistic updates against an Infra API.
// Controller-owned spec fields are reconciled; vendor-defaulted fields survive.
type ResourceStore struct {
	Client client.Client
	Config config.Cluster
}

func (s ResourceStore) CheckIdentity(ctx context.Context) error {
	var ns corev1.Namespace
	if err := s.Client.Get(ctx, types.NamespacedName{Name: "kube-system"}, &ns); err != nil {
		return fmt.Errorf("Infra identity query failed")
	}
	if string(ns.UID) != s.Config.UID {
		return fmt.Errorf("Infra cluster UID mismatch")
	}
	return nil
}
func object(kind, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("kubeovn.io/v1")
	u.SetKind(kind)
	u.SetName(name)
	return u
}
func (s ResourceStore) Get(ctx context.Context, kind, name string) (*unstructured.Unstructured, error) {
	o := object(kind, name)
	err := s.Client.Get(ctx, types.NamespacedName{Name: name}, o)
	return o, err
}
func owned(o *unstructured.Unstructured, owner, cluster, expected string) error {
	if o.GetLabels()[api.OwnerLabel] != owner || o.GetLabels()[api.ClusterLabel] != cluster {
		return fmt.Errorf("resource ownership conflict")
	}
	if expected != "" && string(o.GetUID()) != expected {
		return fmt.Errorf("resource UID changed")
	}
	return nil
}
func (s ResourceStore) Ensure(ctx context.Context, desired *unstructured.Unstructured, expected string) (string, error) {
	if err := s.CheckIdentity(ctx); err != nil {
		return "", err
	}
	o, err := s.Get(ctx, desired.GetKind(), desired.GetName())
	if errors.IsNotFound(err) {
		if expected != "" {
			return "", ErrRecordedResourceMissing
		}
		// A recorded object disappearing is safe to recreate; an existing replacement
		// is not. Its new UID is durably recorded before proceeding to OVN mutations.
		o = desired.DeepCopy()
		if err = s.Client.Create(ctx, o); err != nil {
			return "", fmt.Errorf("resource creation failed; retry discovery")
		}
		return string(o.GetUID()), nil
	}
	if err != nil {
		return "", fmt.Errorf("resource query failed")
	}
	if err = owned(o, desired.GetLabels()[api.OwnerLabel], s.Config.UID, expected); err != nil {
		return "", err
	}
	if !o.GetDeletionTimestamp().IsZero() {
		return "", fmt.Errorf("resource is terminating")
	}
	spec, _, err := unstructured.NestedMap(desired.Object, "spec")
	if err != nil {
		return "", err
	}
	before := o.DeepCopy()
	current, _, _ := unstructured.NestedMap(o.Object, "spec")
	if current == nil {
		current = map[string]interface{}{}
	}
	for k, v := range spec {
		current[k] = v
	}
	if err = unstructured.SetNestedMap(o.Object, current, "spec"); err != nil {
		return "", err
	}
	if !reflect.DeepEqual(before.Object, o.Object) {
		if err = s.Client.Update(ctx, o); err != nil {
			return "", fmt.Errorf("resource update failed; retry discovery")
		}
	}
	return string(o.GetUID()), nil
}
func (s ResourceStore) Delete(ctx context.Context, kind, name, owner, expected string) (bool, error) {
	if err := s.CheckIdentity(ctx); err != nil {
		return false, err
	}
	o, err := s.Get(ctx, kind, name)
	if errors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("resource deletion query failed")
	}
	if err = owned(o, owner, s.Config.UID, expected); err != nil {
		return false, err
	}
	if o.GetDeletionTimestamp().IsZero() {
		uid, rv := o.GetUID(), o.GetResourceVersion()
		err = s.Client.Delete(ctx, o, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}})
		if err != nil && !errors.IsNotFound(err) {
			return false, fmt.Errorf("resource deletion failed; retry discovery")
		}
	}
	return false, nil
}
func (s ResourceStore) Ready(ctx context.Context, kind, name, owner, expected string) (bool, error) {
	if err := s.CheckIdentity(ctx); err != nil {
		return false, err
	}
	o, err := s.Get(ctx, kind, name)
	if err != nil {
		return false, fmt.Errorf("resource readiness query failed")
	}
	if err = owned(o, owner, s.Config.UID, expected); err != nil {
		return false, err
	}
	if !o.GetDeletionTimestamp().IsZero() {
		return false, nil
	}
	conditions, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	for _, c := range conditions {
		m, ok := c.(map[string]interface{})
		if ok && m["type"] == "Ready" && m["status"] == "True" {
			return true, nil
		}
	}
	return false, nil
}
func (s ResourceStore) WithdrawRoutes(ctx context.Context, name, owner, expected string) error {
	if err := s.CheckIdentity(ctx); err != nil {
		return err
	}
	o, err := s.Get(ctx, "Vpc", name)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("route withdrawal query failed")
	}
	if err = owned(o, owner, s.Config.UID, expected); err != nil {
		return err
	}
	routes, _, _ := unstructured.NestedSlice(o.Object, "spec", "staticRoutes")
	if len(routes) == 0 {
		return nil
	}
	_ = unstructured.SetNestedSlice(o.Object, []interface{}{}, "spec", "staticRoutes")
	if err = s.Client.Update(ctx, o); err != nil {
		return fmt.Errorf("route withdrawal failed")
	}
	return nil
}
