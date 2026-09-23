// Package platformplan compiles managed VPC/Subnet intent into per-location snapshots.
package platformplan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	api "globalvpc.io/controller/api/v1alpha2"
	"globalvpc.io/controller/internal/platformconfig"
	"globalvpc.io/controller/internal/siteconfig"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const OwnerLabel = "platform.globalvpc.io/vpc-uid"
const LocationLabel = "platform.globalvpc.io/location"
const ProjectLabel = "platform.globalvpc.io/project"
const Finalizer = "platform.globalvpc.io/managed-cleanup"

func Short(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:])[:20]
}
func BindingName(uid, location string) string { return "nb-" + Short(uid+"/"+location) }
func VpcName(uid, location string) string     { return "pv-" + Short(uid+"/"+location) }
func SubnetName(uid string) string            { return "ps-" + Short(uid) }

func ValidateSubnet(v *api.VPC, s *api.Subnet, cfg platformconfig.Config) error {
	if s.Namespace != v.Namespace || s.Spec.VPCRef != v.Name {
		return fmt.Errorf("subnet must reference a VPC in its project")
	}
	if s.Status.VPCUID != "" && s.Status.VPCUID != string(v.UID) {
		return fmt.Errorf("VPC identity changed; subnet cannot adopt a same-name replacement")
	}
	l, err := cfg.Location(s.Spec.LocationRef, s.Namespace)
	if err != nil {
		return err
	}
	if _, err = siteconfig.Prefix(s.Spec.CIDR); err != nil {
		return err
	}
	if !l.AllowsCIDR(s.Spec.CIDR) {
		return fmt.Errorf("CIDR is outside this location's reserved workload pools")
	}
	return nil
}

// Revision excludes status: BFD changes must not rewrite transport configuration.
func Revision(spec api.NetworkBindingSpec) string {
	spec.Revision = ""
	spec.Peers = append([]api.PeerGateway(nil), spec.Peers...)
	for i := range spec.Peers {
		spec.Peers[i].Endpoint.Ready = false
	}
	b, _ := json.Marshal(spec)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Build includes terminating subnets until the local reconciler acknowledges their
// removal. Existing locations receive explicit tombstones; API absence never means delete.
func Build(v *api.VPC, subnets []api.Subnet, existing []api.NetworkBinding, cfg platformconfig.Config) ([]api.NetworkBinding, error) {
	className := v.Spec.NetworkClassRef
	if className == "" {
		className = "default"
	}
	class, ok := cfg.NetworkClasses[className]
	if !ok {
		return nil, fmt.Errorf("network class %s is not registered", className)
	}
	byLocation := map[string][]api.BindingSubnet{}
	locations := map[string]platformconfig.Location{}
	for _, b := range existing {
		if b.Spec.VPCRef.UID != string(v.UID) || b.Spec.VPCRef.Namespace != v.Namespace {
			return nil, fmt.Errorf("foreign binding identity")
		}
		locations[b.Spec.LocationRef] = platformconfig.Location{Name: b.Spec.LocationRef, BindingNamespace: b.Namespace}
	}
	for _, s := range subnets {
		if s.Status.VPCUID != string(v.UID) {
			continue
		}
		l, err := cfg.Location(s.Spec.LocationRef, s.Namespace)
		if err != nil {
			return nil, err
		}
		locations[l.Name] = l
		p, err := siteconfig.Prefix(s.Spec.CIDR)
		if err != nil {
			return nil, err
		}
		byLocation[l.Name] = append(byLocation[l.Name], api.BindingSubnet{Name: s.Name, UID: string(s.UID), CIDR: s.Spec.CIDR, Gateway: p.Addr().Next().String(), NativeSubnetName: SubnetName(string(s.UID)), Deleting: !s.DeletionTimestamp.IsZero()})
	}
	names := make([]string, 0, len(locations))
	for name := range locations {
		names = append(names, name)
	}
	sort.Strings(names)
	authorized := []string{}
	for _, name := range names {
		if len(byLocation[name]) > 0 {
			authorized = append(authorized, name)
		}
	}
	result := []api.NetworkBinding{}
	for _, name := range names {
		l := locations[name]
		local := byLocation[name]
		sort.Slice(local, func(i, j int) bool { return local[i].UID < local[j].UID })
		b := api.NetworkBinding{TypeMeta: metav1.TypeMeta{APIVersion: api.GroupVersion.String(), Kind: "NetworkBinding"}, ObjectMeta: metav1.ObjectMeta{Name: BindingName(string(v.UID), name), Namespace: l.BindingNamespace, Labels: map[string]string{OwnerLabel: string(v.UID), LocationLabel: name, ProjectLabel: v.Namespace}}, Spec: api.NetworkBindingSpec{VPCRef: api.ObjectIdentity{Name: v.Name, Namespace: v.Namespace, UID: string(v.UID)}, LocationRef: name, NetworkClassRef: className, NativeVpcName: VpcName(string(v.UID), name), TransportProfile: class.TransportProfile, Subnets: local, AuthorizedLocations: authorized, Deleting: len(local) == 0}}
		for _, remote := range names {
			if remote == name {
				continue
			}
			for _, s := range byLocation[remote] {
				if !s.Deleting || !withdrawn(existing, remote, s.NativeSubnetName) {
					b.Spec.RemoteSubnets = append(b.Spec.RemoteSubnets, api.RemoteSubnet{LocationRef: remote, CIDR: s.CIDR})
				}
			}
		}
		for _, remote := range existing {
			if remote.Spec.LocationRef == name || len(byLocation[remote.Spec.LocationRef]) == 0 {
				continue
			}
			seenIDs, seenNodes := map[string]bool{}, map[string]bool{}
			for _, g := range remote.Status.Gateways {
				// Registration is authenticated by per-location status RBAC; still reject malformed inventories.
				ip, err := netip.ParseAddr(g.EndpointIP)
				transit, e2 := netip.ParseAddr(g.TransitIP)
				if err != nil || e2 != nil || !ip.Is4() || !ip.IsGlobalUnicast() || !transit.Is4() || g.ID == "" || g.NodeUID == "" || g.NodeName == "" || seenIDs[g.ID] || seenNodes[g.NodeUID] {
					return nil, fmt.Errorf("invalid gateway registration at %s", remote.Spec.LocationRef)
				}
				seenIDs[g.ID], seenNodes[g.NodeUID] = true, true
				g.Ready = false
				var links []api.GatewayLink
				for _, link := range g.Links {
					if strings.HasPrefix(link.PeerID, name+"/") {
						links = append(links, link)
					}
				}
				g.Links = links
				b.Spec.Peers = append(b.Spec.Peers, api.PeerGateway{LocationRef: remote.Spec.LocationRef, Endpoint: g})
			}
		}
		sort.Slice(b.Spec.RemoteSubnets, func(i, j int) bool {
			a, c := b.Spec.RemoteSubnets[i], b.Spec.RemoteSubnets[j]
			return a.LocationRef+"/"+a.CIDR < c.LocationRef+"/"+c.CIDR
		})
		sort.Slice(b.Spec.Peers, func(i, j int) bool {
			a, c := b.Spec.Peers[i], b.Spec.Peers[j]
			return a.LocationRef+"/"+a.Endpoint.ID < c.LocationRef+"/"+c.Endpoint.ID
		})
		b.Spec.Revision = Revision(b.Spec)
		result = append(result, b)
	}
	return result, nil
}

func withdrawn(bindings []api.NetworkBinding, location, name string) bool {
	for _, b := range bindings {
		if b.Spec.LocationRef != location || b.Status.AppliedRevision != b.Spec.Revision {
			continue
		}
		for _, r := range b.Status.Resources {
			if r.Kind == "Subnet" && r.Name == name {
				return false
			}
		}
		return true
	}
	return false
}
