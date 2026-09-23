//go:build integration && scale

package platformcontroller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	api "globalvpc.io/controller/api/v1alpha2"
	"globalvpc.io/controller/internal/platformconfig"
	"globalvpc.io/controller/internal/platformplan"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

type measuredAPI struct {
	next         http.RoundTripper
	mu           sync.Mutex
	requests     int
	milliseconds float64
}

func (m *measuredAPI) RoundTrip(r *http.Request) (*http.Response, error) {
	start := time.Now()
	response, err := m.next.RoundTrip(r)
	m.mu.Lock()
	m.requests++
	m.milliseconds += float64(time.Since(start).Microseconds()) / 1000
	m.mu.Unlock()
	return response, err
}
func (m *measuredAPI) counts() (int, float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.requests, m.milliseconds
}

type scaleCaseEvidence struct {
	Locations                  int     `json:"participatingLocations"`
	Registered                 int     `json:"registeredLocations"`
	Gateways                   int     `json:"gatewaysPerLocation"`
	Scope                      string  `json:"scope"`
	Accepted                   bool    `json:"accepted"`
	Bindings                   int     `json:"bindings"`
	PeersPerBinding            int     `json:"peersPerBinding"`
	LinkReceiptsPerBinding     int     `json:"projectedLinkReceiptsPerBinding"`
	MaxSpecBytes               int     `json:"maxSerializedSpecBytes"`
	MaxStatusBytes             int     `json:"maxSerializedStatusBytes"`
	MaxObjectBytes             int     `json:"maxSerializedObjectBytes"`
	MaxManagedFieldsBytes      int     `json:"maxManagedFieldsBytes"`
	Reconciles                 int     `json:"reconciles"`
	MaxReconcileMS             float64 `json:"maxReconcileMilliseconds"`
	ElapsedSeconds             float64 `json:"elapsedSeconds"`
	APIRequests                int     `json:"apiRequests"`
	APIRequestMS               float64 `json:"aggregateAPIRequestMilliseconds"`
	SampledGoHeapBytes         uint64  `json:"sampledGoHeapBytes"`
	OfflineDidNotBlockCreation bool    `json:"offlineLocationDidNotBlockCreation"`
	NativeProjectionAdmission  string  `json:"nativeProjectionAdmission"`
	NativeLinkAllocations      int     `json:"nativeLinkAllocations"`
	NativeProjectionBytes      int     `json:"nativeProjectionBytes"`
	Error                      string  `json:"error,omitempty"`
}

func scaleGateways(b *api.NetworkBinding, sites, members int) []api.GatewayEndpoint {
	var location int
	_, _ = fmt.Sscanf(b.Spec.LocationRef, "loc-%02d", &location)
	values := []api.GatewayEndpoint{}
	for member := 0; member < members; member++ {
		g := api.GatewayEndpoint{ID: fmt.Sprintf("g-%02d", member), NodeName: fmt.Sprintf("node-%02d-%02d", location, member), NodeUID: fmt.Sprintf("node-uid-%02d-%02d", location, member), EndpointIP: fmt.Sprintf("192.0.%d.%d", location+2, member+10), TransitIP: fmt.Sprintf("10.252.%d.%d", location, member+2), HealthIP: fmt.Sprintf("10.251.%d.%d", location, member+1), LocalASN: b.Spec.LocalASN, Ready: true}
		// A valid 32-byte public key, not a credential or a live peer registration.
		g.PublicKey = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
		for remote := 0; remote < sites; remote++ {
			if remote == location {
				continue
			}
			for rm := 0; rm < members; rm++ {
				n := remote*members + rm
				g.Links = append(g.Links, api.GatewayLink{PeerID: fmt.Sprintf("loc-%02d/g-%02d", remote, rm), ListenPort: int32(32000 + n), TunnelIP: fmt.Sprintf("10.%d.%d.%d", 64+location, (member*sites*members+n)/250, (member*sites*members+n)%250+1)})
			}
		}
		values = append(values, g)
	}
	return values
}
func scaleBindings(ctx context.Context, c client.Client, v *api.VPC) ([]api.NetworkBinding, error) {
	var list api.NetworkBindingList
	err := c.List(ctx, &list, client.MatchingLabels{platformplan.OwnerLabel: string(v.UID)})
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Spec.LocationRef < list.Items[j].Spec.LocationRef })
	return list.Items, err
}
func scaleCase(ctx context.Context, c client.Client, meter *measuredAPI, config platformconfig.Config, sites, members int) (result scaleCaseEvidence) {
	result = scaleCaseEvidence{Locations: sites, Registered: 50, Gateways: members, Scope: "Real authority reconciliation and CRD storage; simulated gateway/native status only; no dataplane", NativeProjectionAdmission: "not-run"}
	start := time.Now()
	startN, startMS := meter.counts()
	defer func() {
		n, ms := meter.counts()
		result.APIRequests = n - startN
		result.APIRequestMS = ms - startMS
		result.ElapsedSeconds = time.Since(start).Seconds()
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		result.SampledGoHeapBytes = mem.HeapAlloc
	}()
	fail := func(err error) scaleCaseEvidence { result.Error = err.Error(); return result }
	name := fmt.Sprintf("scale-%02d-g%02d", sites, members)
	v := &api.VPC{ObjectMeta: metav1.ObjectMeta{Namespace: "scale-project", Name: name}}
	if err := c.Create(ctx, v); err != nil {
		return fail(err)
	}
	for i := 0; i < sites; i++ {
		subnet := &api.Subnet{ObjectMeta: metav1.ObjectMeta{Namespace: v.Namespace, Name: fmt.Sprintf("%s-sub-%02d", name, i)}, Spec: api.SubnetSpec{VPCRef: v.Name, LocationRef: fmt.Sprintf("loc-%02d", i), CIDR: fmt.Sprintf("10.240.%d.0/24", i)}}
		if err := c.Create(ctx, subnet); err != nil {
			return fail(err)
		}
	}
	r := &Reconciler{Client: c, Config: config}
	reconcile := func() error {
		start := time.Now()
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(v)})
		ms := float64(time.Since(start).Microseconds()) / 1000
		result.Reconciles++
		if ms > result.MaxReconcileMS {
			result.MaxReconcileMS = ms
		}
		return err
	}
	// Each public Subnet finalizer/status and Binding create is a durable stage.
	for i := 0; i < 3*sites+10; i++ {
		if err := reconcile(); err != nil {
			return fail(err)
		}
	}
	bindings, err := scaleBindings(ctx, c, v)
	if err != nil {
		return fail(err)
	}
	result.Bindings = len(bindings)
	if len(bindings) != sites {
		return fail(fmt.Errorf("got %d bindings, expected %d", len(bindings), sites))
	}
	// No site has reported yet. Every desired location must still be materialized.
	if err := c.Get(ctx, client.ObjectKeyFromObject(v), v); err != nil {
		return fail(err)
	}
	result.OfflineDidNotBlockCreation = !meta.IsStatusConditionTrue(v.Status.Conditions, "Ready")
	if !result.OfflineDidNotBlockCreation {
		return fail(fmt.Errorf("unacknowledged sites unexpectedly Ready"))
	}
	for i := range bindings {
		b := &bindings[i]
		b.Status.Gateways = scaleGateways(b, sites, members)
		b.Status.ObservedGeneration = b.Generation
		b.Status.Phase = "Pending"
		if err := c.Status().Update(ctx, b); err != nil {
			return fail(fmt.Errorf("gateway status admission: %w", err))
		}
	}
	// Authority projects only reciprocal links relevant to the destination site.
	for i := 0; i < sites+3; i++ {
		if err := reconcile(); err != nil {
			return fail(err)
		}
	}
	bindings, err = scaleBindings(ctx, c, v)
	if err != nil {
		return fail(err)
	}
	for i := range bindings {
		b := &bindings[i]
		if len(b.Spec.AuthorizedLocations) != sites || len(b.Spec.Peers) != (sites-1)*members || len(b.Spec.RemoteSubnets) != sites-1 {
			return fail(fmt.Errorf("sparse membership/peer projection differs from requested sites"))
		}
		links := 0
		for _, peer := range b.Spec.Peers {
			if len(peer.Endpoint.Links) != members {
				return fail(fmt.Errorf("irrelevant remote link receipts were projected"))
			}
			links += len(peer.Endpoint.Links)
		}
		result.PeersPerBinding = len(b.Spec.Peers)
		result.LinkReceiptsPerBinding = links
		b.Status.AppliedRevision = b.Spec.Revision
		b.Status.ObservedGeneration = b.Generation
		b.Status.Phase = "Ready"
		b.Status.Resources = []api.ResourceRecord{{APIVersion: "kubeovn.io/v1", Kind: "Vpc", Name: b.Spec.NativeVpcName, UID: "simulated-native-vpc"}}
		for _, s := range b.Spec.Subnets {
			b.Status.Resources = append(b.Status.Resources, api.ResourceRecord{APIVersion: "kubeovn.io/v1", Kind: "Subnet", Name: s.NativeSubnetName, UID: "simulated-" + s.UID})
		}
		meta.SetStatusCondition(&b.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, ObservedGeneration: b.Generation, Reason: "ScaleStatusSimulator", Message: "API-only status simulator; no native or transport execution"})
		if err := c.Status().Update(ctx, b); err != nil {
			return fail(fmt.Errorf("full status admission: %w", err))
		}
	}
	for i := 0; i < 3; i++ {
		if err := reconcile(); err != nil {
			return fail(err)
		}
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(v), v); err != nil {
		return fail(err)
	}
	if !meta.IsStatusConditionTrue(v.Status.Conditions, "Ready") {
		return fail(fmt.Errorf("authority failed to converge acknowledged API-only locations"))
	}
	bindings, err = scaleBindings(ctx, c, v)
	if err != nil {
		return fail(err)
	}
	for _, b := range bindings {
		spec, _ := json.Marshal(b.Spec)
		status, _ := json.Marshal(b.Status)
		obj, _ := json.Marshal(b)
		fields, _ := json.Marshal(b.ManagedFields)
		result.MaxSpecBytes = max(result.MaxSpecBytes, len(spec))
		result.MaxStatusBytes = max(result.MaxStatusBytes, len(status))
		result.MaxObjectBytes = max(result.MaxObjectBytes, len(obj))
		result.MaxManagedFieldsBytes = max(result.MaxManagedFieldsBytes, len(fields))
	}
	// Independent schema-only native projection: this does not execute the
	// authority's native VNI allocator or claim native runtime convergence.
	native := bindings[0].DeepCopy()
	native.Name = name + "-native-schema"
	native.UID = ""
	native.ResourceVersion = ""
	native.ManagedFields = nil
	native.OwnerReferences = nil
	native.Labels = nil
	native.Spec.TransportProfile = "geneve-bgp"
	native.Spec.NetworkID = 16700000
	native.Status = api.NetworkBindingStatus{}
	for local := 0; local < members; local++ {
		for remote := 1; remote < sites; remote++ {
			for member := 0; member < members; member++ {
				native.Spec.LinkAllocations = append(native.Spec.LinkAllocations, api.LinkAllocation{PeerA: fmt.Sprintf("loc-00/g-%02d", local), PeerB: fmt.Sprintf("loc-%02d/g-%02d", remote, member), ControlVNI: uint32(1000 + len(native.Spec.LinkAllocations))})
			}
		}
	}
	native.Spec.Revision = platformplan.Revision(native.Spec)
	body, _ := json.Marshal(native)
	result.NativeProjectionBytes = len(body)
	result.NativeLinkAllocations = len(native.Spec.LinkAllocations)
	err = c.Create(ctx, native)
	if len(native.Spec.LinkAllocations) > 4096 {
		if !apierrors.IsInvalid(err) {
			return fail(fmt.Errorf("native maxItems boundary was not rejected as invalid: %v", err))
		}
		result.NativeProjectionAdmission = "rejected: native linkAllocations exceeds 4096 (expected schema boundary)"
	} else if err != nil {
		return fail(fmt.Errorf("native projection admission: %w", err))
	} else {
		result.NativeProjectionAdmission = "accepted: schema-only, not native allocator/runtime"
	}
	result.Accepted = true
	return result
}

func TestManagedFiftyLocationAPIScale(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS must reference envtest binaries")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	e := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd")}, ErrorIfCRDPathMissing: true, ControlPlaneStartTimeout: 45 * time.Second, ControlPlaneStopTimeout: 20 * time.Second}
	cfg, err := e.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.Stop(); err != nil {
			t.Error(err)
		}
	})
	meter := &measuredAPI{}
	cfg.WrapTransport = func(next http.RoundTripper) http.RoundTripper { meter.next = next; return meter }
	scheme := k8sruntime.NewScheme()
	_ = api.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	config := platformconfig.Config{NetworkClasses: map[string]platformconfig.NetworkClass{"default": {TransportProfile: "wireguard-bgp"}}}
	for _, name := range []string{"scale-project", "global-vpc-system"} {
		if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 50; i++ {
		name := fmt.Sprintf("loc-%02d", i)
		ns := "scale-" + name
		config.Locations = append(config.Locations, platformconfig.Location{Name: name, Region: "test-region", Site: name, DC: name, BindingNamespace: ns, AllowedProjects: []string{"scale-project"}, CIDRPools: []string{"10.240.0.0/16"}})
		if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	results := []scaleCaseEvidence{}
	defer func() {
		report := map[string]any{"schemaVersion": 1, "scope": "Disposable actual Kubernetes API; 50 logical locations; real authority and planner; simulated local/native/gateway acknowledgements; no Lab mutation or packet traffic", "observedAt": time.Now().UTC().Format(time.RFC3339), "cases": results}
		body, _ := json.MarshalIndent(report, "", "  ")
		t.Log(string(body))
		if path := os.Getenv("GVPC_SCALE_EVIDENCE"); path != "" {
			if err := os.WriteFile(path, append(body, '\n'), 0600); err != nil {
				t.Error(err)
			}
		}
	}()
	for _, scenario := range [][2]int{{2, 2}, {3, 8}, {50, 2}, {50, 8}, {50, 10}} {
		t.Logf("starting API-only locations=%d gateways=%d", scenario[0], scenario[1])
		result := scaleCase(ctx, c, meter, config, scenario[0], scenario[1])
		results = append(results, result)
		if !result.Accepted {
			t.Errorf("API-only scale %d/%d: %s", scenario[0], scenario[1], result.Error)
			break
		}
		t.Logf("accepted locations=%d gateways=%d spec=%d status=%d full=%d bytes elapsed=%.2fs", scenario[0], scenario[1], result.MaxSpecBytes, result.MaxStatusBytes, result.MaxObjectBytes, result.ElapsedSeconds)
	}
}
