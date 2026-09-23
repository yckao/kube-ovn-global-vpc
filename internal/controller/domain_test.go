package controller

import (
	"context"
	"reflect"
	"testing"

	"globalvpc.io/controller/internal/config"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func domainDeployment() *appsv1.Deployment {
	replicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "interconnect", Name: "native-ic", UID: "ic-deployment-uid", Generation: 4, Labels: map[string]string{DomainLabel: "test-domain", "vendor": "retained"}},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "native-ic"}}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{DomainLabel: "test-domain", "app": "native-ic"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "ic", Image: "ovn:pinned"}}}}},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 4, ReadyReplicas: 1, UpdatedReplicas: 1},
	}
}

func domainGate(t *testing.T, objects ...client.Object) (*DomainGate, *mutationClient, *mutationClient) {
	t.Helper()
	scheme := resourceScheme(t)
	authority := &mutationClient{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	objects = append(objects, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "infra-uid"}})
	infra := &mutationClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&appsv1.Deployment{}, &corev1.Pod{}).WithObjects(objects...).Build()}
	c := config.Cluster{Name: "infra", UID: "infra-uid", ICDeployment: config.DeploymentReference{Namespace: "interconnect", Name: "native-ic", UID: "ic-deployment-uid"}}
	g := &DomainGate{Authority: authority, Namespace: "controller", Config: config.Config{Domain: "test-domain", Clusters: []config.Cluster{c}}, Stores: map[string]ResourceStore{"infra": {Client: infra, Config: c}}}
	return g, authority, infra
}

func requireLock(t *testing.T, g *DomainGate) {
	t.Helper()
	if acquired, err := g.Acquire(context.Background(), "tenant-uid", "accepted-plan"); err != nil || !acquired {
		t.Fatalf("acquire failed: %v %v", acquired, err)
	}
}

func TestDomainGateLockSurvivesControllerRecreationAndRejectsNewTenantUID(t *testing.T) {
	g, authority, _ := domainGate(t)
	requireLock(t, g)
	// A new Go reconciler instance must recover the persisted holder. A deleted
	// and recreated GlobalVpc has a new UID and cannot steal that transaction.
	restarted := &DomainGate{Authority: g.Authority, Namespace: g.Namespace, Config: g.Config, Stores: g.Stores}
	if holder, err := restarted.Holder(context.Background()); err != nil || holder != "tenant-uid" {
		t.Fatalf("lock lost after restart: %q %v", holder, err)
	}
	if ok, err := restarted.Acquire(context.Background(), "tenant-uid", "accepted-plan"); err != nil || !ok {
		t.Fatalf("same transaction not resumable: %v %v", ok, err)
	}
	if len(authority.writes) != 1 {
		t.Fatal("lock replay unnecessarily mutated authority")
	}
	if ok, err := restarted.Acquire(context.Background(), "recreated-tenant-uid", "accepted-plan"); err != nil || ok {
		t.Fatalf("recreated object stole lock: %v %v", ok, err)
	}
	if ok, err := restarted.Acquire(context.Background(), "tenant-uid", "changed-plan"); err == nil || ok {
		t.Fatal("holder changed accepted transaction while locked")
	}
	if err := restarted.Release(context.Background(), "recreated-tenant-uid"); err == nil {
		t.Fatal("nonholder released the transaction")
	}
	if len(authority.writes) != 1 {
		t.Fatal("competing holder attempted a write")
	}
	if err := restarted.Release(context.Background(), "tenant-uid"); err != nil {
		t.Fatal(err)
	}
	if ok, err := restarted.Acquire(context.Background(), "another-tenant", "another-plan"); err != nil || !ok {
		t.Fatalf("released lock not reusable: %v %v", ok, err)
	}
	var lock corev1.ConfigMap
	if err := authority.Get(context.Background(), types.NamespacedName{Namespace: g.Namespace, Name: g.lockName()}, &lock); err != nil {
		t.Fatal(err)
	}
	if len(lock.OwnerReferences) != 0 {
		t.Fatal("transaction lock must outlive GlobalVpc garbage collection")
	}
}

func TestDomainGateRefusesForeignLock(t *testing.T) {
	g, authority, infra := domainGate(t, domainDeployment())
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: g.lockName(), Namespace: g.Namespace, Labels: map[string]string{DomainLabel: "foreign-domain"}}, Data: map[string]string{"holder": "tenant-uid", "planHash": "accepted-plan"}}
	if err := authority.Client.Create(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Holder(context.Background()); err == nil {
		t.Fatal("foreign lock accepted")
	}
	if _, err := g.Acquire(context.Background(), "tenant-uid", "accepted-plan"); err == nil {
		t.Fatal("foreign lock acquired")
	}
	if _, err := g.SetPaused(context.Background(), "tenant-uid", true); err == nil {
		t.Fatal("foreign lock authorized deployment write")
	}
	if err := g.Release(context.Background(), "tenant-uid"); err == nil {
		t.Fatal("foreign lock released")
	}
	if len(authority.writes) != 0 || len(infra.writes) != 0 {
		t.Fatal("foreign lock caused a write")
	}
}

func TestDomainGateDeploymentIdentityAndOwnershipGuards(t *testing.T) {
	for _, mutation := range []string{"uid", "deployment-label", "template-label", "terminating", "nil-replicas", "many-replicas", "cluster-uid", "no-lock"} {
		t.Run(mutation, func(t *testing.T) {
			dep := domainDeployment()
			switch mutation {
			case "uid":
				dep.UID = "replacement-deployment"
			case "deployment-label":
				dep.Labels[DomainLabel] = "foreign-domain"
			case "template-label":
				delete(dep.Spec.Template.Labels, DomainLabel)
			case "terminating":
				now := metav1.Now()
				dep.DeletionTimestamp = &now
				dep.Finalizers = []string{"test.example/retain"}
			case "nil-replicas":
				dep.Spec.Replicas = nil
			case "many-replicas":
				replicas := int32(2)
				dep.Spec.Replicas = &replicas
			}
			g, _, infra := domainGate(t, dep)
			if mutation == "cluster-uid" {
				var ns corev1.Namespace
				if err := infra.Get(context.Background(), types.NamespacedName{Name: "kube-system"}, &ns); err != nil {
					t.Fatal(err)
				}
				ns.UID = "replacement-cluster"
				if err := infra.Client.Update(context.Background(), &ns); err != nil {
					t.Fatal(err)
				}
			}
			if mutation != "no-lock" {
				requireLock(t, g)
			}
			if _, err := g.SetPaused(context.Background(), "tenant-uid", true); err == nil {
				t.Fatal("unsafe deployment accepted")
			}
			if len(infra.writes) != 0 {
				t.Fatal("guard failed before mutation")
			}
		})
	}
}

func icReplicaSet(uid, deploymentUID string, controller bool) *appsv1.ReplicaSet {
	replicas := int32(0)
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "interconnect", Name: uid, UID: types.UID(uid), Generation: 3, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "native-ic", UID: types.UID(deploymentUID), Controller: &controller}}},
		Spec:       appsv1.ReplicaSetSpec{Replicas: &replicas},
		Status:     appsv1.ReplicaSetStatus{ObservedGeneration: 3},
	}
}
func icPod(name, rsUID string, phase corev1.PodPhase, terminating bool) *corev1.Pod {
	controller := true
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "interconnect", Name: name, UID: types.UID(name + "-uid"), Labels: map[string]string{"app": "native-ic"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rsUID, UID: types.UID(rsUID), Controller: &controller}}}, Status: corev1.PodStatus{Phase: phase}}
	if terminating {
		now := metav1.Now()
		p.DeletionTimestamp = &now
		p.Finalizers = []string{"test.example/retain"}
	}
	return p
}

func TestDomainGatePauseWaitsForOwnedProcessesIncludingTerminatingPods(t *testing.T) {
	for _, phase := range []corev1.PodPhase{corev1.PodPending, corev1.PodRunning, corev1.PodUnknown, corev1.PodSucceeded, corev1.PodFailed} {
		for _, terminating := range []bool{false, true} {
			name := string(phase)
			if terminating {
				name += "-terminating"
			}
			t.Run(name, func(t *testing.T) {
				dep := domainDeployment()
				replicas := int32(0)
				dep.Spec.Replicas = &replicas
				// Even observed zero-replica status cannot authorize a topology
				// write while a real nonterminal owned Pod is present.
				dep.Status = appsv1.DeploymentStatus{ObservedGeneration: dep.Generation}
				g, _, infra := domainGate(t, dep, icReplicaSet("owned-rs", string(dep.UID), true), icPod("owned-pod", "owned-rs", phase, terminating))
				requireLock(t, g)
				paused, err := g.SetPaused(context.Background(), "tenant-uid", true)
				want := phase == corev1.PodSucceeded || phase == corev1.PodFailed
				if err != nil || paused != want {
					t.Fatalf("phase %s terminating=%v: paused %v, want %v, error %v", phase, terminating, paused, want, err)
				}
				if len(infra.writes) != 0 {
					t.Fatal("already-paused deployment rewritten")
				}
			})
		}
	}
}

func TestDomainGatePauseRequiresObservedScaleDownWithNoPods(t *testing.T) {
	for _, state := range []string{"deployment-unobserved", "replicaset-desired-one", "replicaset-unobserved", "replicaset-unspecified", "quiescent"} {
		t.Run(state, func(t *testing.T) {
			dep := domainDeployment()
			zero := int32(0)
			dep.Spec.Replicas = &zero
			dep.Status = appsv1.DeploymentStatus{ObservedGeneration: dep.Generation}
			rs := icReplicaSet("owned-rs", string(dep.UID), true)
			switch state {
			case "deployment-unobserved":
				dep.Status.ObservedGeneration--
			case "replicaset-desired-one":
				one := int32(1)
				rs.Spec.Replicas = &one
			case "replicaset-unobserved":
				rs.Status.ObservedGeneration--
			case "replicaset-unspecified":
				rs.Spec.Replicas = nil
			}
			g, _, infra := domainGate(t, dep, rs)
			requireLock(t, g)
			paused, err := g.SetPaused(context.Background(), "tenant-uid", true)
			if err != nil || paused != (state == "quiescent") {
				t.Fatalf("empty Pod list incorrectly authorized topology changes for %s: %v %v", state, paused, err)
			}
			if len(infra.writes) != 0 {
				t.Fatal("quiescence checks mutated desired zero-replica state")
			}
		})
	}
}

func TestDomainGatePreflightsAllDeploymentsBeforeAnyReplicaWrite(t *testing.T) {
	for _, paused := range []bool{true, false} {
		for _, failure := range []string{"deployment-uid", "domain-label", "cluster-uid", "missing-client"} {
			name := "pause/" + failure
			if !paused {
				name = "resume/" + failure
			}
			t.Run(name, func(t *testing.T) {
				dep := domainDeployment()
				if !paused {
					zero := int32(0)
					dep.Spec.Replicas = &zero
				}
				g, _, first := domainGate(t, dep)
				secondDep := dep.DeepCopy()
				if failure == "deployment-uid" {
					secondDep.UID = "replacement-uid"
				}
				if failure == "domain-label" {
					secondDep.Labels[DomainLabel] = "another-domain"
				}
				secondGate, _, second := domainGate(t, secondDep)
				registration := secondGate.Config.Clusters[0]
				registration.Name = "second"
				registration.UID = "second-infra-uid"
				var ns corev1.Namespace
				if err := second.Get(context.Background(), types.NamespacedName{Name: "kube-system"}, &ns); err != nil {
					t.Fatal(err)
				}
				ns.UID = "second-infra-uid"
				if failure == "cluster-uid" {
					ns.UID = "replacement-cluster"
				}
				if err := second.Client.Update(context.Background(), &ns); err != nil {
					t.Fatal(err)
				}
				g.Config.Clusters = append(g.Config.Clusters, registration)
				if failure != "missing-client" {
					g.Stores[registration.Name] = ResourceStore{Client: second, Config: registration}
				}
				requireLock(t, g)
				if _, err := g.SetPaused(context.Background(), "tenant-uid", paused); err == nil {
					t.Fatal("invalid second Infra was accepted")
				}
				if len(first.writes) != 0 || len(second.writes) != 0 {
					t.Fatal("first Infra mutated before second Infra preflight failed")
				}
			})
		}
	}
}

func TestDomainGatePauseFollowsOwnerUIDsAndTouchesOnlyDedicatedDeployment(t *testing.T) {
	dep := domainDeployment()
	unrelated := domainDeployment()
	unrelated.Name = "shared-ovn-central"
	unrelated.UID = "shared-deployment"
	unrelated.Labels = map[string]string{"shared": "preserve"}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gateway", UID: "gateway-uid", Labels: map[string]string{"ovn.kubernetes.io/ic-gw": "true", "shared": "preserve"}}}
	g, _, infra := domainGate(t, dep, unrelated, node, icReplicaSet("foreign-rs", "foreign-deployment", true), icPod("foreign-pod", "foreign-rs", corev1.PodRunning, true), icReplicaSet("noncontroller-rs", string(dep.UID), false), icPod("noncontroller-pod", "noncontroller-rs", corev1.PodRunning, false))
	requireLock(t, g)
	if paused, err := g.SetPaused(context.Background(), "tenant-uid", true); err != nil || paused {
		t.Fatalf("first scale update should await observation: %v %v", paused, err)
	}
	// The fake client does not increment Deployment generation on spec writes.
	// Model that API behavior explicitly, then acknowledge the controller's
	// observed generation only after checking the intermediate Pending state.
	var scaled appsv1.Deployment
	if err := infra.Get(context.Background(), client.ObjectKeyFromObject(dep), &scaled); err != nil {
		t.Fatal(err)
	}
	scaled.Generation++
	if err := infra.Client.Update(context.Background(), &scaled); err != nil {
		t.Fatal(err)
	}
	if paused, err := g.SetPaused(context.Background(), "tenant-uid", true); err != nil || paused {
		t.Fatalf("unobserved scale-down must remain pending: %v %v", paused, err)
	}
	scaled.Status.ObservedGeneration = scaled.Generation
	if err := infra.Status().Update(context.Background(), &scaled); err != nil {
		t.Fatal(err)
	}
	if paused, err := g.SetPaused(context.Background(), "tenant-uid", true); err != nil || !paused {
		t.Fatalf("unrelated pods blocked domain pause: %v %v", paused, err)
	}
	if len(infra.writes) != 1 || infra.writes[0].GetUID() != dep.UID {
		t.Fatal("paused resources outside the dedicated Deployment")
	}
	var gotNode corev1.Node
	var gotUnrelated appsv1.Deployment
	var gotDep appsv1.Deployment
	if err := infra.Get(context.Background(), client.ObjectKeyFromObject(node), &gotNode); err != nil {
		t.Fatal(err)
	}
	if err := infra.Get(context.Background(), client.ObjectKeyFromObject(unrelated), &gotUnrelated); err != nil {
		t.Fatal(err)
	}
	if err := infra.Get(context.Background(), client.ObjectKeyFromObject(dep), &gotDep); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(node.Labels, gotNode.Labels) || !reflect.DeepEqual(unrelated.Spec, gotUnrelated.Spec) || !reflect.DeepEqual(dep.Spec.Template, gotDep.Spec.Template) || !reflect.DeepEqual(dep.Labels, gotDep.Labels) {
		t.Fatal("shared labels, template, or another Deployment changed")
	}
}

func TestDomainGateResumeRequiresObservedGenerationAndReadyUpdatedReplica(t *testing.T) {
	for _, state := range []string{"stale-generation", "not-ready", "not-updated", "ready", "scale-from-zero"} {
		t.Run(state, func(t *testing.T) {
			dep := domainDeployment()
			switch state {
			case "stale-generation":
				dep.Status.ObservedGeneration--
			case "not-ready":
				dep.Status.ReadyReplicas = 0
			case "not-updated":
				dep.Status.UpdatedReplicas = 0
			case "scale-from-zero":
				replicas := int32(0)
				dep.Spec.Replicas = &replicas
			}
			g, _, infra := domainGate(t, dep)
			requireLock(t, g)
			ready, err := g.SetPaused(context.Background(), "tenant-uid", false)
			if err != nil || ready != (state == "ready") {
				t.Fatalf("invalid resume result for %s: %v %v", state, ready, err)
			}
			wantWrites := 0
			if state == "scale-from-zero" {
				wantWrites = 1
			}
			if len(infra.writes) != wantWrites {
				t.Fatalf("unexpected writes: %d", len(infra.writes))
			}
		})
	}
}
