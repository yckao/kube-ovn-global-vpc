package localcontroller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"time"

	api "globalvpc.io/controller/api/v1alpha2"
	"globalvpc.io/controller/internal/localconfig"
	"globalvpc.io/controller/internal/platformplan"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const SourceUIDAnnotation = "platform.globalvpc.io/source-binding-uid"
const SourceGenerationAnnotation = "platform.globalvpc.io/source-generation"

// Syncer only contacts the authority. Local reconciliation continues from its
// own API's durable snapshots while the authority is unreachable.
type Syncer struct {
	Authority client.Client
	Local     client.Client
	Config    localconfig.Config
	Interval  time.Duration
}

func (s *Syncer) NeedLeaderElection() bool { return true }
func (s *Syncer) Start(ctx context.Context) error {
	d := s.Interval
	if d < time.Second {
		d = 5 * time.Second
	}
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		call, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := s.Once(call)
		cancel()
		if err != nil {
			log.FromContext(ctx).Error(err, "Platform synchronization pending; retaining accepted local snapshots")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
func (s *Syncer) Once(ctx context.Context) error {
	var list api.NetworkBindingList
	if err := s.Authority.List(ctx, &list, client.InNamespace(s.Config.AuthorityNamespace)); err != nil {
		return fmt.Errorf("cannot read authorized location snapshots")
	}
	var failures []error
	for i := range list.Items {
		if err := s.binding(ctx, &list.Items[i]); err != nil {
			failures = append(failures, fmt.Errorf("binding %s: %w", list.Items[i].Name, err))
		}
	}
	// Omission is never a delete instruction. A failing VPC also cannot prevent
	// unrelated snapshots in this location from being synchronized.
	return errors.Join(failures...)
}
func (s *Syncer) binding(ctx context.Context, remote *api.NetworkBinding) error {
	if remote.Spec.LocationRef != s.Config.LocationRef {
		return fmt.Errorf("authority snapshot belongs to another location")
	}
	if remote.Name != platformplan.BindingName(remote.Spec.VPCRef.UID, s.Config.LocationRef) || remote.Spec.Revision != platformplan.Revision(remote.Spec) {
		return fmt.Errorf("authority snapshot identity or revision is invalid")
	}
	key := client.ObjectKey{Namespace: s.Config.Namespace, Name: remote.Name}
	var local api.NetworkBinding
	err := s.Local.Get(ctx, key, &local)
	if apierrors.IsNotFound(err) {
		local = api.NetworkBinding{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, Labels: map[string]string{api.LocationLabel: s.Config.LocationRef}, Annotations: map[string]string{SourceUIDAnnotation: string(remote.UID), SourceGenerationAnnotation: strconv.FormatInt(remote.Generation, 10)}}, Spec: remote.DeepCopy().Spec}
		return s.Local.Create(ctx, &local)
	}
	if err != nil {
		return err
	}
	if local.Annotations[SourceUIDAnnotation] != string(remote.UID) {
		if local.Spec.Deleting && local.Status.Phase == "Deleted" && local.Status.AppliedRevision == local.Spec.Revision && len(local.Status.Resources) == 0 && len(local.Status.Gateways) == 0 {
			return s.Local.Delete(ctx, &local, client.Preconditions{UID: &local.UID, ResourceVersion: &local.ResourceVersion})
		}
		return fmt.Errorf("previous local binding incarnation has not completed cleanup")
	}
	if local.Spec.VPCRef != remote.Spec.VPCRef || local.Spec.LocationRef != s.Config.LocationRef {
		return fmt.Errorf("local snapshot belongs to another authority identity")
	}
	generation, err := strconv.ParseInt(local.Annotations[SourceGenerationAnnotation], 10, 64)
	if err != nil || remote.Generation < generation {
		return fmt.Errorf("authority generation is older than the accepted local snapshot")
	}
	same := reflect.DeepEqual(local.Spec, remote.Spec)
	if remote.Generation == generation && !same {
		return fmt.Errorf("authority changed snapshot without advancing generation")
	}
	if remote.Generation > generation {
		if local.Spec.Deleting && !remote.Spec.Deleting {
			return fmt.Errorf("cannot resurrect a deleting local snapshot")
		}
		local.Spec = remote.DeepCopy().Spec
		local.Annotations[SourceGenerationAnnotation] = strconv.FormatInt(remote.Generation, 10)
		if err = s.Local.Update(ctx, &local); err != nil {
			return err
		}
	}
	status := local.DeepCopy().Status
	status.ObservedGeneration = 0
	if status.AppliedRevision == remote.Spec.Revision {
		status.ObservedGeneration = remote.Generation
	}
	for j := range status.Conditions {
		status.Conditions[j].ObservedGeneration = status.ObservedGeneration
	}
	if !reflect.DeepEqual(remote.Status, status) {
		remote.Status = status
		return s.Authority.Status().Update(ctx, remote)
	}
	return nil
}
