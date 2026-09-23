package ovn

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	api "globalvpc.io/controller/api/v1alpha1"
	"globalvpc.io/controller/internal/config"
	"globalvpc.io/controller/internal/siteplan"
)

const ecmpOwnerKey = "global-vpc-ecmp-fields-owner"

var ecmpFields = []string{"ip_dst", "ip_proto", "ip_src", "tp_dst", "tp_src"}

func exactMapGuard(column string, values map[string]string) string {
	entries := make([]string, 0, len(values))
	for key, value := range values {
		encodedKey, _ := json.Marshal(key)
		encodedValue, _ := json.Marshal(value)
		entries = append(entries, string(encodedKey)+"="+string(encodedValue))
	}
	sort.Strings(entries)
	return column + "={" + strings.Join(entries, ",") + "}"
}

// EnsureECMPFields owns just selection_fields and its ownership marker on the
// native default routes. Kube-OVN 1.16.4 does not render this field and its OVN
// build defaults an unspecified SELECT hash to source IP only. Explicit fields
// allow flows from one workload to use multiple gateways. This version-specific
// ownership boundary must be revalidated before upgrading the local CNI.
func (e *Exec) EnsureECMPFields(ctx context.Context, cfg config.Cluster, g siteplan.GatewayConfig, records []api.BFDResourceRecord) (bool, error) {
	expected, err := bfdExpected(g, records)
	if err != nil {
		return false, err
	}
	if len(expected) != len(g.Gateways) {
		return false, nil
	}
	want := map[string]string{}
	for _, member := range g.Gateways {
		want[member.IP] = expected[member.ID]
	}
	rows, err := e.query(ctx, cfg.NBCommand, "_uuid,name,static_routes", "Logical_Router", condition("name", g.VpcName))
	if err != nil || len(rows) == 0 {
		return false, err
	}
	router, err := namedSingle(rows, g.VpcName)
	if err != nil {
		return false, err
	}
	routerID, err := uuidValue(router["_uuid"])
	if err != nil {
		return false, err
	}
	refs, err := setValue(router["static_routes"], true)
	if err != nil {
		return false, err
	}
	type target struct {
		id, ip, bfd, idsGuard, policyGuard, fieldsGuard string
		changed                                         bool
	}
	targets := []target{}
	seen := map[string]bool{}
	for _, ref := range refs {
		rows, err := e.query(ctx, cfg.NBCommand, "ip_prefix,nexthop,policy,route_table,bfd,selection_fields,external_ids", "Logical_Router_Static_Route", "_uuid="+ref)
		if err != nil {
			return false, err
		}
		r, err := single(rows)
		if err != nil {
			return false, err
		}
		prefix, err := stringValue(r["ip_prefix"])
		if err != nil {
			return false, err
		}
		ip, err := stringValue(r["nexthop"])
		if err != nil {
			return false, err
		}
		if prefix != "0.0.0.0/0" || want[ip] == "" {
			continue
		}
		policy, err := setValue(r["policy"], false)
		if err != nil {
			return false, err
		}
		table, err := stringValue(r["route_table"])
		if err != nil {
			return false, err
		}
		bfd, err := setValue(r["bfd"], true)
		if err != nil {
			return false, err
		}
		if table != "" || (len(policy) != 0 && strings.Join(policy, ",") != "dst-ip") || len(bfd) != 1 || bfd[0] != want[ip] {
			return false, nil
		}
		if seen[ip] {
			return false, errors.New("duplicate native gateway route")
		}
		seen[ip] = true
		fields, err := setValue(r["selection_fields"], false)
		if err != nil {
			return false, err
		}
		sort.Strings(fields)
		ids, err := mapValue(r["external_ids"])
		if err != nil {
			return false, err
		}
		owner := ids[ecmpOwnerKey]
		if owner != "" && owner != g.OwnerUID {
			return false, errors.New("ECMP selection field ownership conflict")
		}
		correct := strings.Join(fields, ",") == strings.Join(ecmpFields, ",")
		if owner == "" && len(fields) != 0 && !correct {
			return false, errors.New("unregistered native ECMP selection fields")
		}
		policyGuard := "policy=[]"
		if len(policy) != 0 {
			policyGuard = condition("policy", "dst-ip")
		}
		fieldsGuard := "selection_fields=[]"
		if len(fields) != 0 {
			fieldsGuard = "selection_fields=" + strings.Join(fields, ",")
		}
		targets = append(targets, target{id: ref, ip: ip, bfd: bfd[0], idsGuard: exactMapGuard("external_ids", ids), policyGuard: policyGuard, fieldsGuard: fieldsGuard, changed: !correct || owner == ""})
	}
	if len(targets) != len(want) {
		return false, nil
	}
	changed := false
	for _, t := range targets {
		if !t.changed {
			continue
		}
		// Missing map keys do not match key=[] in ovn-nbctl wait-until. Guard
		// the complete observed map instead, including absence of our marker.
		// Membership, exact BFD identity and ownership are checked in the same
		// OVSDB transaction as the narrow field update. No native route is adopted
		// merely by matching a name or next-hop address.
		args := []string{"get", "Logical_Router", routerID, "name", "static_routes",
			"--", "get", "Logical_Router_Static_Route", t.id, "ip_prefix", "nexthop", "policy", "route_table", "bfd", "external_ids", "selection_fields",
			"--", "get", "BFD", t.bfd, "external_ids", "logical_port", "dst_ip",
			"--", "wait-until", "Logical_Router", routerID, condition("name", g.VpcName), "static_routes{>=}" + t.id,
			"--", "wait-until", "Logical_Router_Static_Route", t.id, condition("ip_prefix", "0.0.0.0/0"), condition("nexthop", t.ip),
			"bfd=" + t.bfd, condition("route_table", ""), t.policyGuard, t.fieldsGuard, t.idsGuard,
			"--", "wait-until", "BFD", t.bfd, condition("external_ids:"+ownerKey, g.OwnerUID), condition("external_ids:"+bfdClusterKey, cfg.UID), condition("logical_port", g.BFDPortName()), condition("dst_ip", t.ip),
			"--", "set", "Logical_Router_Static_Route", t.id, "selection_fields=" + strings.Join(ecmpFields, ","), condition("external_ids:"+ecmpOwnerKey, g.OwnerUID)}
		if _, err := e.run(ctx, cfg.NBCommand, args...); err != nil {
			return false, err
		}
		changed = true
	}
	return !changed, nil // Observe the persisted fields before reporting Ready.
}
