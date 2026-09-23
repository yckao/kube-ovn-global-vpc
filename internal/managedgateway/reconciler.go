// Package managedgateway materializes location-local gateway identities and Pods.
// It does not write OVN, host routes, switches, or another location's API.
package managedgateway

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	api "globalvpc.io/controller/api/v1alpha2"
	core "k8s.io/api/core/v1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const componentLabel = "platform.globalvpc.io/component"
const configHash = "platform.globalvpc.io/config-hash"
const memberLabel = "platform.globalvpc.io/gateway-id"
const ledgerData = "ledger.json"

type Reconciler struct {
	Client                                                       client.Client
	Namespace, ClusterUID, Image, RuntimeDir, EndpointAnnotation string
	NodeSelector                                                 map[string]string
	Replicas, MTU                                                int
	AllocationPool                                               string
	PortStart, PortEnd                                           int
	RolloutProbe                                                 func(context.Context, *core.Pod, map[string]any) (bool, error)
}
type Attachment struct {
	TransitCIDR, RouterIP, BFDSourceIP string
	MemberIPs                          []string
}
type Result struct {
	Gateways  []api.GatewayEndpoint
	Resources []api.ResourceRecord
	Ready     bool
	Reason    string
}
type member struct {
	Endpoint   api.GatewayEndpoint `json:"endpoint"`
	SecretName string              `json:"secretName,omitempty"`
}
type ledger struct {
	OwnerUID, VPCUID, Location, TransitCIDR, Profile string
	Members                                          []member
}
type allocationRegistry struct {
	ClusterUID string
	Addresses  map[string]string
	Ports      map[string]int
}

func Short(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:])[:20] }
func labels(b *api.NetworkBinding) map[string]string {
	return map[string]string{api.OwnerLabel: string(b.UID), api.VPCUIDLabel: b.Spec.VPCRef.UID, api.LocationLabel: b.Spec.LocationRef, componentLabel: "gateway"}
}
func ref(b *api.NetworkBinding) metav1.OwnerReference {
	yes := true
	return metav1.OwnerReference{APIVersion: api.GroupVersion.String(), Kind: "NetworkBinding", Name: b.Name, UID: b.UID, Controller: &yes, BlockOwnerDeletion: &yes}
}
func owned(o client.Object, b *api.NetworkBinding) bool {
	if o.GetLabels()[api.OwnerLabel] != string(b.UID) {
		return false
	}
	for _, v := range o.GetOwnerReferences() {
		if v.UID == b.UID && v.Kind == "NetworkBinding" && v.APIVersion == api.GroupVersion.String() && v.Name == b.Name && v.Controller != nil && *v.Controller {
			return true
		}
	}
	return false
}
func metadata(name string, b *api.NetworkBinding, ns string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels(b), OwnerReferences: []metav1.OwnerReference{ref(b)}}
}
func ledgerName(b *api.NetworkBinding) string { return "pg-" + Short(string(b.UID)) }
func peerID(location, id string) string       { return location + "/" + id }
func (r *Reconciler) validate(b *api.NetworkBinding, a Attachment) error {
	if r.Client == nil || r.Namespace == "" || r.ClusterUID == "" || r.Image == "" || r.RuntimeDir == "" || b.Namespace != r.Namespace || b.UID == "" || b.Spec.VPCRef.UID == "" {
		return fmt.Errorf("incomplete local gateway identity or configuration")
	}
	if r.Replicas < 2 || r.Replicas > 64 || len(r.NodeSelector) == 0 || r.MTU < 1280 || r.MTU > 1400 || r.PortStart < 1024 || r.PortEnd > 65535 || r.PortEnd < r.PortStart {
		return fmt.Errorf("invalid managed gateway settings")
	}
	if b.Spec.LocalASN < 4200000000 || b.Spec.LocalASN > 4294967294 {
		return fmt.Errorf("durable private location ASN is required")
	}
	p, e := netip.ParsePrefix(a.TransitCIDR)
	if e != nil || !p.Addr().Is4() || p != p.Masked() {
		return fmt.Errorf("invalid transit prefix")
	}
	if p.Bits() < 16 || p.Bits() > 29 || r.Replicas > (1<<(32-p.Bits()))-3 || a.RouterIP != p.Addr().Next().String() || len(a.MemberIPs) != 0 {
		return fmt.Errorf("transit allocation must fit durable members and use its first host as router")
	}
	for _, v := range []string{a.RouterIP, a.BFDSourceIP} {
		ip, e := netip.ParseAddr(v)
		if e != nil || !ip.Is4() {
			return fmt.Errorf("invalid attachment address")
		}
	}
	pool, e := netip.ParsePrefix(r.AllocationPool)
	if e != nil || !pool.Addr().Is4() || pool != pool.Masked() || pool.Overlaps(p) || pool.Contains(netip.MustParseAddr(a.BFDSourceIP)) || p.Contains(netip.MustParseAddr(a.BFDSourceIP)) {
		return fmt.Errorf("invalid reserved control pool")
	}
	for _, s := range b.Spec.RemoteSubnets {
		q, e := netip.ParsePrefix(s.CIDR)
		if e != nil || !q.Addr().Is4() || q != q.Masked() || q.Overlaps(p) || q.Overlaps(pool) {
			return fmt.Errorf("remote subnet overlaps infrastructure or is invalid")
		}
	}
	for _, s := range b.Spec.Subnets {
		q, e := netip.ParsePrefix(s.CIDR)
		if e != nil || !q.Addr().Is4() || q != q.Masked() || q.Overlaps(p) || q.Overlaps(pool) {
			return fmt.Errorf("local subnet overlaps infrastructure or is invalid")
		}
	}
	return nil
}

// Ensure first publishes durable public membership, then creates immutable
// runtime generations once both endpoints' public link receipts are available.
func (r *Reconciler) Ensure(ctx context.Context, b *api.NetworkBinding, a Attachment) (Result, error) {
	result := Result{Reason: "Initializing"}
	if err := r.validate(b, a); err != nil {
		return result, err
	}
	if _, _, err := r.registry(ctx, false, b); err != nil {
		return result, err
	}
	l, err := r.members(ctx, b, a)
	if err != nil {
		return result, err
	}
	if err = r.validateLedger(b, a, l); err != nil {
		return result, err
	}
	// Check every incarnation before writing any member resource.
	for _, m := range l.Members {
		var n core.Node
		if err = r.Client.Get(ctx, types.NamespacedName{Name: m.Endpoint.NodeName}, &n); err != nil {
			return result, fmt.Errorf("NodeIdentityUnavailable: %w", err)
		}
		if string(n.UID) != m.Endpoint.NodeUID || r.nodeEndpoint(&n) != m.Endpoint.EndpointIP {
			return result, fmt.Errorf("NodeIdentityChanged: explicit fenced replacement is required")
		}
	}
	for i := range l.Members {
		m := &l.Members[i]
		if b.Spec.TransportProfile == "wireguard-bgp" {
			if err = r.ensureKey(ctx, b, m); err != nil {
				return result, err
			}
		}
		if err = r.linkClaims(ctx, b, m); err != nil {
			return result, err
		}
	}
	if err = r.saveLedger(ctx, b, l); err != nil {
		return result, err
	}
	for _, m := range l.Members {
		result.Gateways = append(result.Gateways, m.Endpoint)
	}
	configs := make([]map[string]any, len(l.Members))
	waiting := false
	for i, m := range l.Members {
		var complete bool
		configs[i], complete, err = r.render(b, a, l, m)
		if err != nil {
			return result, err
		}
		waiting = waiting || !complete
	}
	// Read all existing Pods before any generation replacement. A failed sibling
	// blocks updates; a missing/terminating node is never force-deleted or reused.
	pods := make([]*core.Pod, len(l.Members))
	ready := make([]bool, len(l.Members))
	for i, m := range l.Members {
		var p core.Pod
		err = r.Client.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: r.podName(b, m)}, &p)
		if errors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return result, err
		}
		if !owned(&p, b) {
			return result, fmt.Errorf("foreign gateway Pod")
		}
		if err = r.verifyPod(ctx, b, m, &p); err != nil {
			return result, err
		}
		pods[i] = &p
		ready[i] = podReady(&p)
		result.Gateways[i].Ready = ready[i]
	}
	if waiting {
		result.Reason = "DiscoveringPeerAllocations"
		return r.withResources(ctx, b, result)
	}
	changed := false
	for i, m := range l.Members {
		cm, p, err := r.objects(b, m, configs[i])
		if err != nil {
			return result, err
		}
		if err = r.ensureConfig(ctx, b, cm); err != nil {
			return result, err
		}
		old := pods[i]
		if old == nil {
			if err = r.Client.Create(ctx, p); err != nil && !errors.IsAlreadyExists(err) {
				return result, err
			}
			changed = true
			continue
		}
		if old.DeletionTimestamp != nil {
			result.Reason = "WaitingForGatewayTermination"
			changed = true
			continue
		}
		if old.Annotations[configHash] == p.Annotations[configHash] {
			continue
		}
		siblingReady := false
		for j, v := range ready {
			if i != j && v {
				siblingReady = true
			}
		}
		if !siblingReady {
			result.Reason = "UpdateBlockedByUnavailableSibling"
			return r.withResources(ctx, b, result)
		}
		// At most one old generation is retired per pass; its replacement must be
		// Ready before any other member may be replaced.
		for j, other := range pods {
			if j != i && (other == nil || other.DeletionTimestamp != nil || !ready[j]) {
				result.Reason = "WaitingForSiblingRecovery"
				return r.withResources(ctx, b, result)
			}
		}
		if r.RolloutProbe == nil {
			result.Reason = "UpdateBlockedWithoutNativeProbe"
			return r.withResources(ctx, b, result)
		}
		for j, sibling := range pods {
			if j == i {
				continue
			}
			// Each survivor is checked against its own desired link allocations.
			// Withdrawn delegations must not pin an otherwise safe old generation.
			healthy, probeErr := r.RolloutProbe(ctx, sibling, configs[j])
			if probeErr != nil {
				return result, probeErr
			}
			if !healthy {
				result.Reason = "WaitingForSiblingNativeConvergence"
				return r.withResources(ctx, b, result)
			}
		}
		if err = r.Client.Delete(ctx, old, client.Preconditions{UID: &old.UID, ResourceVersion: &old.ResourceVersion}); err != nil && !errors.IsNotFound(err) {
			return result, err
		}
		result.Gateways[i].Ready = false
		result.Reason = "RollingGatewayGeneration"
		return r.withResources(ctx, b, result)
	}
	result.Ready = !changed
	for _, v := range result.Gateways {
		result.Ready = result.Ready && v.Ready
	}
	if result.Ready {
		result.Reason = "Ready"
	} else if result.Reason == "Initializing" {
		result.Reason = "WaitingForGatewayHealth"
	}
	if err = r.pruneConfigs(ctx, b); err != nil {
		return result, err
	}
	return r.withResources(ctx, b, result)
}
func podReady(p *core.Pod) bool {
	if p.DeletionTimestamp != nil {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == core.PodReady {
			return c.Status == core.ConditionTrue
		}
	}
	return false
}

func (r *Reconciler) verifyPod(ctx context.Context, b *api.NetworkBinding, m member, p *core.Pod) error {
	if p.Spec.NodeName != m.Endpoint.NodeName || p.Spec.HostNetwork || !p.Spec.HostPID || p.Spec.AutomountServiceAccountToken == nil || *p.Spec.AutomountServiceAccountToken || len(p.Spec.Containers) != 1 || len(p.Spec.InitContainers) != 0 {
		return fmt.Errorf("gateway Pod isolation or placement changed")
	}
	c := p.Spec.Containers[0]
	if c.Name != "gateway" || !reflect.DeepEqual(c.Command, []string{"python3", "/app/managed.py"}) || len(c.Args) != 0 || c.SecurityContext == nil || c.SecurityContext.Privileged == nil || !*c.SecurityContext.Privileged || c.ReadinessProbe == nil || c.ReadinessProbe.Exec == nil || !reflect.DeepEqual(c.ReadinessProbe.Exec.Command, []string{"python3", "/app/managed.py", "--ready"}) {
		return fmt.Errorf("gateway Pod runtime command changed")
	}
	wantVolumes := 1
	if b.Spec.TransportProfile == "wireguard-bgp" {
		wantVolumes = 2
	}
	if len(p.Spec.Volumes) != wantVolumes || len(c.VolumeMounts) != wantVolumes+1 || p.Spec.Volumes[0].Name != "runtime" || p.Spec.Volumes[0].ConfigMap == nil {
		return fmt.Errorf("gateway Pod runtime volume changed")
	}
	if wantVolumes == 2 && (p.Spec.Volumes[1].Name != "keys" || p.Spec.Volumes[1].Secret == nil || p.Spec.Volumes[1].Secret.SecretName != m.SecretName) {
		return fmt.Errorf("gateway Pod key reference changed")
	}
	for i, mount := range c.VolumeMounts {
		path, name := "/app", "runtime"
		if i == 1 {
			path = "/config"
		}
		if i == 2 {
			path, name = "/keys", "keys"
		}
		if mount.Name != name || mount.MountPath != path || !mount.ReadOnly || mount.SubPath != "" || mount.SubPathExpr != "" {
			return fmt.Errorf("gateway Pod runtime mount changed")
		}
	}
	var cm core.ConfigMap
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: p.Spec.Volumes[0].ConfigMap.Name}, &cm); err != nil {
		return err
	}
	if !owned(&cm, b) || cm.Immutable == nil || !*cm.Immutable {
		return fmt.Errorf("gateway accepted configuration ownership changed")
	}
	body, _ := json.Marshal(struct {
		Data  map[string]string
		Image string
	}{cm.Data, c.Image})
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != p.Annotations[configHash] {
		return fmt.Errorf("gateway accepted image or configuration changed")
	}
	if p.Annotations["ovn.kubernetes.io/logical_switch"] != "pt-"+Short(b.Spec.VPCRef.UID+"/"+b.Spec.LocationRef) || p.Annotations["ovn.kubernetes.io/ip_address"] != m.Endpoint.TransitIP || p.Annotations["ovn.kubernetes.io/port_security"] != "false" {
		return fmt.Errorf("gateway native network attachment changed")
	}
	return nil
}
func (r *Reconciler) podName(b *api.NetworkBinding, m member) string {
	return ledgerName(b) + "-" + m.Endpoint.ID
}

func (r *Reconciler) validateLedger(b *api.NetworkBinding, a Attachment, l ledger) error {
	pool, _ := netip.ParsePrefix(r.AllocationPool)
	transit, _ := netip.ParsePrefix(a.TransitCIDR)
	ids, nodes, ips := map[string]bool{}, map[string]bool{}, map[string]bool{a.RouterIP: true, a.BFDSourceIP: true}
	for _, m := range l.Members {
		g := m.Endpoint
		ip, e := netip.ParseAddr(g.TransitIP)
		health, h := netip.ParseAddr(g.HealthIP)
		if g.ID == "" || g.NodeUID == "" || ids[g.ID] || nodes[g.NodeUID] || e != nil || h != nil || !transit.Contains(ip) || !pool.Contains(health) || ips[g.TransitIP] || ips[g.HealthIP] || g.LocalASN != b.Spec.LocalASN || m.SecretName != ledgerName(b)+"-"+g.ID+"-key" {
			return fmt.Errorf("gateway ledger identity or reserved allocation changed")
		}
		ids[g.ID], nodes[g.NodeUID], ips[g.TransitIP], ips[g.HealthIP] = true, true, true, true
		peers := map[string]bool{}
		for _, link := range g.Links {
			ip, e := netip.ParseAddr(link.TunnelIP)
			if e != nil || !pool.Contains(ip) || ips[link.TunnelIP] || peers[link.PeerID] || link.PeerID == "" {
				return fmt.Errorf("gateway ledger contains conflicting control addresses")
			}
			ips[link.TunnelIP], peers[link.PeerID] = true, true
		}
	}
	return nil
}
func (r *Reconciler) nodeEndpoint(n *core.Node) string {
	if r.EndpointAnnotation != "" {
		return n.Annotations[r.EndpointAnnotation]
	}
	for _, a := range n.Status.Addresses {
		if a.Type == core.NodeInternalIP {
			ip, e := netip.ParseAddr(a.Address)
			if e == nil && ip.Is4() {
				return ip.String()
			}
		}
	}
	return ""
}
func eligible(n *core.Node) bool {
	if n.Spec.Unschedulable {
		return false
	}
	for _, t := range n.Spec.Taints {
		if t.Effect == core.TaintEffectNoSchedule || t.Effect == core.TaintEffectNoExecute {
			return false
		}
	}
	for _, c := range n.Status.Conditions {
		if c.Type == core.NodeReady {
			return c.Status == core.ConditionTrue
		}
	}
	return false
}
func (r *Reconciler) members(ctx context.Context, b *api.NetworkBinding, a Attachment) (ledger, error) {
	var cm core.ConfigMap
	key := types.NamespacedName{Namespace: r.Namespace, Name: ledgerName(b)}
	err := r.Client.Get(ctx, key, &cm)
	if err == nil {
		var l ledger
		if !owned(&cm, b) {
			return l, fmt.Errorf("foreign gateway ledger")
		}
		if err = json.Unmarshal([]byte(cm.Data[ledgerData]), &l); err != nil {
			return l, fmt.Errorf("invalid gateway ledger")
		}
		if l.OwnerUID != string(b.UID) || l.VPCUID != b.Spec.VPCRef.UID || l.Location != b.Spec.LocationRef || l.TransitCIDR != a.TransitCIDR || l.Profile != b.Spec.TransportProfile || len(l.Members) != r.Replicas {
			return l, fmt.Errorf("gateway allocation is immutable; replacement requires explicit retirement")
		}
		return l, nil
	}
	if !errors.IsNotFound(err) {
		return ledger{}, err
	}
	resources, err := r.resources(ctx, b)
	if err != nil {
		return ledger{}, err
	}
	if len(resources) != 0 {
		return ledger{}, fmt.Errorf("GatewayLedgerLost: preserve existing incarnations for explicit recovery")
	}
	var nodes core.NodeList
	if err = r.Client.List(ctx, &nodes, client.MatchingLabels(r.NodeSelector)); err != nil {
		return ledger{}, err
	}
	sort.Slice(nodes.Items, func(i, j int) bool { return nodes.Items[i].Name < nodes.Items[j].Name })
	selected := []core.Node{}
	for _, n := range nodes.Items {
		if eligible(&n) && ipv4(r.nodeEndpoint(&n)) && n.UID != "" {
			selected = append(selected, n)
		}
	}
	if len(selected) < r.Replicas {
		return ledger{}, fmt.Errorf("InsufficientGatewayNodes: need %d distinct ready selected nodes", r.Replicas)
	}
	l := ledger{OwnerUID: string(b.UID), VPCUID: b.Spec.VPCRef.UID, Location: b.Spec.LocationRef, TransitCIDR: a.TransitCIDR, Profile: b.Spec.TransportProfile}
	prefix, _ := netip.ParsePrefix(a.TransitCIDR)
	ip := prefix.Addr().Next().Next()
	for _, n := range selected {
		if !eligible(&n) {
			continue
		}
		endpoint, e := netip.ParseAddr(r.nodeEndpoint(&n))
		if e != nil || !endpoint.Is4() || n.UID == "" {
			continue
		}
		if !prefix.Contains(ip.Next()) {
			return l, fmt.Errorf("transit pool lacks member addresses")
		}
		raw := make([]byte, 8)
		if _, e = rand.Read(raw); e != nil {
			return l, e
		}
		id := "g" + hex.EncodeToString(raw)
		health, e := r.address(ctx, string(b.UID)+"/"+id+"/health")
		if e != nil {
			return l, e
		}
		l.Members = append(l.Members, member{Endpoint: api.GatewayEndpoint{ID: id, NodeName: n.Name, NodeUID: string(n.UID), EndpointIP: endpoint.String(), TransitIP: ip.String(), LocalASN: b.Spec.LocalASN, HealthIP: health}, SecretName: ledgerName(b) + "-" + id + "-key"})
		ip = ip.Next()
		if len(l.Members) == r.Replicas {
			break
		}
	}
	if len(l.Members) != r.Replicas {
		return l, fmt.Errorf("InsufficientGatewayNodes: need %d distinct ready selected nodes", r.Replicas)
	}
	body, _ := json.Marshal(l)
	cm = core.ConfigMap{ObjectMeta: metadata(key.Name, b, r.Namespace), Data: map[string]string{ledgerData: string(body)}}
	if err = r.Client.Create(ctx, &cm); errors.IsAlreadyExists(err) {
		return r.members(ctx, b, a)
	}
	return l, err
}
func (r *Reconciler) saveLedger(ctx context.Context, b *api.NetworkBinding, l ledger) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cm core.ConfigMap
		if err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: ledgerName(b)}, &cm); err != nil {
			return err
		}
		if !owned(&cm, b) {
			return fmt.Errorf("foreign gateway ledger")
		}
		body, _ := json.Marshal(l)
		if cm.Data[ledgerData] == string(body) {
			return nil
		}
		cm.Data[ledgerData] = string(body)
		return r.Client.Update(ctx, &cm)
	})
}
func (r *Reconciler) ensureKey(ctx context.Context, b *api.NetworkBinding, m *member) error {
	var s core.Secret
	key := types.NamespacedName{Namespace: r.Namespace, Name: m.SecretName}
	err := r.Client.Get(ctx, key, &s)
	if errors.IsNotFound(err) {
		if m.Endpoint.PublicKey != "" {
			return fmt.Errorf("GatewayKeyLost: published identity cannot be regenerated")
		}
		raw := make([]byte, 32)
		if _, err = rand.Read(raw); err != nil {
			return err
		}
		raw[0] &= 248
		raw[31] &= 127
		raw[31] |= 64
		private, err := ecdh.X25519().NewPrivateKey(raw)
		if err != nil {
			return err
		}
		s = core.Secret{ObjectMeta: metadata(key.Name, b, r.Namespace), Type: core.SecretTypeOpaque, Immutable: ptr(true), Data: map[string][]byte{"privateKey": []byte(base64.StdEncoding.EncodeToString(raw)), "publicKey": []byte(base64.StdEncoding.EncodeToString(private.PublicKey().Bytes()))}}
		if err = r.Client.Create(ctx, &s); errors.IsAlreadyExists(err) {
			return r.ensureKey(ctx, b, m)
		} else if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if !owned(&s, b) || s.Immutable == nil || !*s.Immutable {
		return fmt.Errorf("foreign or mutable gateway key")
	}
	raw, err := base64.StdEncoding.DecodeString(string(s.Data["privateKey"]))
	if err != nil || len(raw) != 32 {
		return fmt.Errorf("invalid private key encoding")
	}
	private, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return fmt.Errorf("invalid private key")
	}
	pub := base64.StdEncoding.EncodeToString(private.PublicKey().Bytes())
	if pub != string(s.Data["publicKey"]) {
		return fmt.Errorf("gateway key pair mismatch")
	}
	if m.Endpoint.PublicKey != "" && m.Endpoint.PublicKey != pub {
		return fmt.Errorf("gateway public key changed")
	}
	m.Endpoint.PublicKey = pub
	return nil
}
func ptr[T any](v T) *T { return &v }
func (r *Reconciler) address(ctx context.Context, key string) (string, error) {
	return r.allocate(ctx, key, false, "")
}
func (r *Reconciler) linkClaims(ctx context.Context, b *api.NetworkBinding, m *member) error {
	existing := map[string]api.GatewayLink{}
	for _, l := range m.Endpoint.Links {
		existing[l.PeerID] = l
	}
	local := peerID(b.Spec.LocationRef, m.Endpoint.ID)
	for _, p := range b.Spec.Peers {
		remote := peerID(p.LocationRef, p.Endpoint.ID)
		if p.LocationRef == b.Spec.LocationRef || p.Endpoint.ID == "" {
			return fmt.Errorf("invalid remote peer identity")
		}
		if _, ok := existing[remote]; ok {
			continue
		}
		key := string(b.UID) + "/" + m.Endpoint.ID + "/" + remote
		ip, err := r.address(ctx, key)
		if err != nil {
			return err
		}
		link := api.GatewayLink{PeerID: remote, TunnelIP: ip}
		switch b.Spec.TransportProfile {
		case "wireguard-bgp":
			port, err := r.allocate(ctx, key, true, m.Endpoint.NodeUID)
			if err != nil {
				return err
			}
			v, _ := strconv.Atoi(port)
			link.ListenPort = int32(v)
		case "geneve-bgp", "vxlan-evpn":
			link.ListenPort = 6082
			if b.Spec.TransportProfile == "vxlan-evpn" {
				link.ListenPort = 4788
			}
			for _, pair := range b.Spec.LinkAllocations {
				if (pair.PeerA == local && pair.PeerB == remote) || (pair.PeerB == local && pair.PeerA == remote) {
					link.ControlVNI = pair.ControlVNI
				}
			}
			if link.ControlVNI == 0 {
				continue
			}
		default:
			return fmt.Errorf("unknown transport profile")
		}
		existing[remote] = link
	}
	m.Endpoint.Links = nil
	for _, v := range existing {
		m.Endpoint.Links = append(m.Endpoint.Links, v)
	}
	sort.Slice(m.Endpoint.Links, func(i, j int) bool { return m.Endpoint.Links[i].PeerID < m.Endpoint.Links[j].PeerID })
	return nil
}

func record(kind string, o client.Object) api.ResourceRecord {
	return api.ResourceRecord{APIVersion: "v1", Kind: kind, Namespace: o.GetNamespace(), Name: o.GetName(), UID: string(o.GetUID())}
}
func (r *Reconciler) resources(ctx context.Context, b *api.NetworkBinding) ([]api.ResourceRecord, error) {
	result := []api.ResourceRecord{}
	var pods core.PodList
	var cms core.ConfigMapList
	var secrets core.SecretList
	// Pods and configuration are inventoried in this operator namespace so
	// ownership-label drift cannot turn a retained object into false absence.
	if err := r.Client.List(ctx, &pods, client.InNamespace(r.Namespace)); err != nil {
		return nil, err
	}
	if err := r.Client.List(ctx, &cms, client.InNamespace(r.Namespace)); err != nil {
		return nil, err
	}
	suspect := func(o client.Object) bool {
		if o.GetLabels()[api.OwnerLabel] == string(b.UID) || o.GetName() == ledgerName(b) || strings.HasPrefix(o.GetName(), ledgerName(b)+"-") {
			return true
		}
		for _, owner := range o.GetOwnerReferences() {
			if owner.UID == b.UID {
				return true
			}
		}
		return false
	}
	secretNames := map[string]bool{}
	for _, rr := range b.Status.Resources {
		if rr.Kind == "Secret" && rr.Namespace == r.Namespace {
			secretNames[rr.Name] = true
		}
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if !suspect(p) {
			continue
		}
		if !owned(p, b) {
			return nil, fmt.Errorf("gateway Pod ownership changed")
		}
		result = append(result, record("Pod", p))
		for _, v := range p.Spec.Volumes {
			if v.Secret != nil {
				secretNames[v.Secret.SecretName] = true
			}
		}
	}
	for i := range cms.Items {
		cm := &cms.Items[i]
		if !suspect(cm) {
			continue
		}
		if !owned(cm, b) {
			return nil, fmt.Errorf("gateway ConfigMap ownership changed")
		}
		result = append(result, record("ConfigMap", cm))
		if cm.Name == ledgerName(b) {
			var l ledger
			if json.Unmarshal([]byte(cm.Data[ledgerData]), &l) != nil {
				return nil, fmt.Errorf("invalid gateway ledger")
			}
			if l.Profile == "wireguard-bgp" {
				for _, m := range l.Members {
					secretNames[m.SecretName] = true
				}
			}
		}
	}
	// Secret data is read only for this binding's label or exact durable names.
	if err := r.Client.List(ctx, &secrets, client.InNamespace(r.Namespace), client.MatchingLabels{api.OwnerLabel: string(b.UID)}); err != nil {
		return nil, err
	}
	found := map[string]bool{}
	for i := range secrets.Items {
		s := &secrets.Items[i]
		if !owned(s, b) {
			return nil, fmt.Errorf("gateway Secret ownership changed")
		}
		result = append(result, record("Secret", s))
		found[s.Name] = true
	}
	for name := range secretNames {
		if name == "" || found[name] {
			continue
		}
		var secret core.Secret
		err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: name}, &secret)
		if errors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !owned(&secret, b) {
			return nil, fmt.Errorf("gateway Secret ownership changed")
		}
		result = append(result, record("Secret", &secret))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Kind+result[i].Name < result[j].Kind+result[j].Name })
	return result, nil
}
func (r *Reconciler) withResources(ctx context.Context, b *api.NetworkBinding, result Result) (Result, error) {
	resources, err := r.resources(ctx, b)
	result.Resources = resources
	return result, err
}

// Delete is called only after local native routes have been withdrawn. It waits
// for real Pod absence before removing configuration or key material. Allocation
// receipts are retained and are not silently handed to another incarnation.
func (r *Reconciler) Delete(ctx context.Context, b *api.NetworkBinding) (bool, error) {
	resources, err := r.resources(ctx, b)
	if err != nil {
		return false, err
	}
	hasPods := false
	for _, rr := range resources {
		if rr.Kind != "Pod" {
			continue
		}
		hasPods = true
		var p core.Pod
		if err = r.Client.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: rr.Name}, &p); errors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		uid := types.UID(rr.UID)
		if p.UID != uid || !owned(&p, b) {
			return false, fmt.Errorf("gateway Pod incarnation changed")
		}
		if p.DeletionTimestamp == nil {
			if err = r.Client.Delete(ctx, &p, client.Preconditions{UID: &uid, ResourceVersion: &p.ResourceVersion}); err != nil && !errors.IsNotFound(err) {
				return false, err
			}
		}
	}
	if hasPods {
		return false, nil
	}
	for _, rr := range resources {
		if rr.Name == ledgerName(b) && rr.Kind == "ConfigMap" {
			continue
		}
		if err = r.deleteRecord(ctx, b, rr); err != nil {
			return false, err
		}
	}
	remaining, err := r.resources(ctx, b)
	if err != nil {
		return false, err
	}
	for _, rr := range remaining {
		if rr.Kind != "ConfigMap" || rr.Name != ledgerName(b) {
			return false, nil
		}
		if err = r.deleteRecord(ctx, b, rr); err != nil {
			return false, err
		}
	}
	remaining, err = r.resources(ctx, b)
	return len(remaining) == 0, err
}
func (r *Reconciler) deleteRecord(ctx context.Context, b *api.NetworkBinding, rr api.ResourceRecord) error {
	var o client.Object
	switch rr.Kind {
	case "ConfigMap":
		o = &core.ConfigMap{}
	case "Secret":
		o = &core.Secret{}
	default:
		return fmt.Errorf("unsupported gateway resource")
	}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: rr.Name}, o); errors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	uid := types.UID(rr.UID)
	if o.GetUID() != uid || !owned(o, b) {
		return fmt.Errorf("gateway resource incarnation changed")
	}
	err := r.Client.Delete(ctx, o, client.Preconditions{UID: &uid, ResourceVersion: ptr(o.GetResourceVersion())})
	return client.IgnoreNotFound(err)
}
func (r *Reconciler) ensureConfig(ctx context.Context, b *api.NetworkBinding, want *core.ConfigMap) error {
	var actual core.ConfigMap
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(want), &actual)
	if errors.IsNotFound(err) {
		return r.Client.Create(ctx, want)
	}
	if err != nil {
		return err
	}
	if !owned(&actual, b) || !reflect.DeepEqual(actual.Data, want.Data) || actual.Immutable == nil || !*actual.Immutable {
		return fmt.Errorf("foreign or modified runtime ConfigMap")
	}
	return nil
}
func (r *Reconciler) pruneConfigs(ctx context.Context, b *api.NetworkBinding) error {
	var pods core.PodList
	var cms core.ConfigMapList
	opts := []client.ListOption{client.InNamespace(r.Namespace), client.MatchingLabels{api.OwnerLabel: string(b.UID)}}
	if err := r.Client.List(ctx, &pods, opts...); err != nil {
		return err
	}
	if err := r.Client.List(ctx, &cms, opts...); err != nil {
		return err
	}
	used := map[string]bool{ledgerName(b): true}
	for _, p := range pods.Items {
		for _, v := range p.Spec.Volumes {
			if v.ConfigMap != nil {
				used[v.ConfigMap.Name] = true
			}
		}
	}
	for i := range cms.Items {
		cm := &cms.Items[i]
		if !used[cm.Name] {
			if err := r.deleteRecord(ctx, b, record("ConfigMap", cm)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Reconciler) objects(b *api.NetworkBinding, m member, config map[string]any) (*core.ConfigMap, *core.Pod, error) {
	data := map[string]string{}
	for _, name := range []string{"gateway.py", "overlay.py", "evpn.py", "managed.py"} {
		raw, err := os.ReadFile(filepath.Join(r.RuntimeDir, name))
		if err != nil {
			return nil, nil, fmt.Errorf("read gateway runtime %s: %w", name, err)
		}
		data[name] = string(raw)
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return nil, nil, err
	}
	data["gateway.json"] = string(raw)
	hashBody, _ := json.Marshal(struct {
		Data  map[string]string
		Image string
	}{data, r.Image})
	sum := sha256.Sum256(hashBody)
	hash := hex.EncodeToString(sum[:])
	name := r.podName(b, m)
	cm := &core.ConfigMap{ObjectMeta: metadata(name+"-"+hash[:10], b, r.Namespace), Immutable: ptr(true), Data: data}
	p := &core.Pod{ObjectMeta: metadata(name, b, r.Namespace)}
	p.Labels[memberLabel] = m.Endpoint.ID
	p.Annotations = map[string]string{configHash: hash, "ovn.kubernetes.io/logical_switch": "pt-" + Short(b.Spec.VPCRef.UID+"/"+b.Spec.LocationRef), "ovn.kubernetes.io/ip_address": m.Endpoint.TransitIP, "ovn.kubernetes.io/port_security": "false"}
	p.Spec = core.PodSpec{NodeName: m.Endpoint.NodeName, HostPID: true, AutomountServiceAccountToken: ptr(false), TerminationGracePeriodSeconds: ptr(int64(15)), RestartPolicy: core.RestartPolicyAlways, Containers: []core.Container{{Name: "gateway", Image: r.Image, ImagePullPolicy: core.PullIfNotPresent, Command: []string{"python3", "/app/managed.py"}, SecurityContext: &core.SecurityContext{Privileged: ptr(true)}, Resources: core.ResourceRequirements{Requests: core.ResourceList{core.ResourceCPU: resource.MustParse("25m"), core.ResourceMemory: resource.MustParse("64Mi")}, Limits: core.ResourceList{core.ResourceMemory: resource.MustParse("192Mi")}}, ReadinessProbe: &core.Probe{ProbeHandler: core.ProbeHandler{Exec: &core.ExecAction{Command: []string{"python3", "/app/managed.py", "--ready"}}}, PeriodSeconds: 2, TimeoutSeconds: 2, FailureThreshold: 2}, VolumeMounts: []core.VolumeMount{{Name: "runtime", MountPath: "/app", ReadOnly: true}, {Name: "runtime", MountPath: "/config", ReadOnly: true}}}}, Volumes: []core.Volume{{Name: "runtime", VolumeSource: core.VolumeSource{ConfigMap: &core.ConfigMapVolumeSource{LocalObjectReference: core.LocalObjectReference{Name: cm.Name}}}}}}
	if b.Spec.TransportProfile == "wireguard-bgp" {
		p.Spec.Volumes = append(p.Spec.Volumes, core.Volume{Name: "keys", VolumeSource: core.VolumeSource{Secret: &core.SecretVolumeSource{SecretName: m.SecretName, DefaultMode: ptr(int32(256))}}})
		p.Spec.Containers[0].VolumeMounts = append(p.Spec.Containers[0].VolumeMounts, core.VolumeMount{Name: "keys", MountPath: "/keys", ReadOnly: true})
	}
	return cm, p, nil
}
