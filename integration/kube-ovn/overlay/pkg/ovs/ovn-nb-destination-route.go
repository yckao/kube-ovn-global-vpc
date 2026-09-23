package ovs

import (
	"context"
	"fmt"
	"net/netip"
	"reflect"
	"slices"

	"github.com/kubeovn/kube-ovn/pkg/destinationroute"
	ovsclient "github.com/kubeovn/kube-ovn/pkg/ovsdb/client"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnnb"
	"github.com/kubeovn/kube-ovn/pkg/util"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
)

// ReconcileDestinationRoutes is called only by the native VPC controller under
// its VPC key lock. It owns marked rows, never ordinary static/default routes.
// Changes to routes and BFD references commit in one guarded OVSDB transaction.
func (c *OVNNbClient) ReconcileDestinationRoutes(router, owner, bfdPort string, plan destinationroute.Plan) error {
	if owner == "" {
		return fmt.Errorf("destination routes require VPC UID")
	}
	lr, err := c.GetLogicalRouter(router, true)
	if err != nil {
		return err
	}
	if lr == nil && len(plan.Routes) != 0 {
		return fmt.Errorf("destination router absent")
	}
	var current []*ovnnb.LogicalRouterStaticRoute
	if lr != nil {
		current, err = c.ListLogicalRouterStaticRoutes(router, nil, nil, "", nil)
		if err != nil {
			return err
		}
	}
	sessions, err := c.FindBFD(nil)
	if err != nil {
		return err
	}
	var networks []string
	bfdFound := false
	if lr != nil && len(plan.Routes) != 0 {
		for _, id := range lr.Ports {
			port, err := c.GetLogicalRouterPortByUUID(id)
			if err != nil {
				return err
			}
			if port.Name == bfdPort {
				bfdFound = port.Options["bfd-only"] == "true"
				continue
			}
			if port.Options["bfd-only"] != "true" {
				networks = append(networks, port.Networks...)
			}
		}
		if !bfdFound {
			return fmt.Errorf("dedicated native BFD port is not ready")
		}
		for hop := range plan.Sessions {
			if !destinationroute.ReachableGateway(hop, networks) {
				return fmt.Errorf("destination gateway is not directly connected")
			}
		}
		for _, route := range plan.Routes {
			dst := netip.MustParsePrefix(route.CIDR)
			for _, raw := range networks {
				connected, err := netip.ParsePrefix(raw)
				if err == nil && connected.Overlaps(dst) {
					return fmt.Errorf("destination overlaps a connected network")
				}
			}
			for _, old := range current {
				if old.ExternalIDs[destinationroute.OwnerKey] == owner {
					continue
				}
				prefix, err := netip.ParsePrefix(old.IPPrefix)
				if err != nil {
					if host, hostErr := netip.ParseAddr(old.IPPrefix); hostErr == nil {
						prefix = netip.PrefixFrom(host, host.BitLen())
						err = nil
					}
				}
				if err != nil {
					continue
				}
				// Permit unrelated covering default/aggregate routes. Foreign routes at or
				// within the destination could collide or bypass the discard guard.
				if old.RouteTable == "" && (old.Policy == nil || *old.Policy == "dst-ip") && dst.Overlaps(prefix) && prefix.Bits() >= dst.Bits() {
					return fmt.Errorf("destination conflicts with a foreign static route")
				}
			}
		}
	}
	var ops []ovsdb.Operation
	if lr != nil {
		uuids := make([]ovsdb.UUID, 0, len(lr.StaticRoutes))
		for _, id := range lr.StaticRoutes {
			uuids = append(uuids, ovsdb.UUID{GoUUID: id})
		}
		refs, _ := ovsdb.NewOvsSet(uuids)
		ops = append(ops, destinationWait(ovnnb.LogicalRouterTable, "_uuid", ovsdb.UUID{GoUUID: lr.UUID}, "static_routes", refs))
	}
	byHop := map[string]*ovnnb.BFD{}
	for i := range sessions {
		b := &sessions[i]
		if b.LogicalPort != bfdPort {
			continue
		}
		if _, wanted := plan.Sessions[b.DstIP]; !wanted {
			continue
		}
		if b.ExternalIDs[destinationroute.OwnerKey] != owner || byHop[b.DstIP] != nil {
			return fmt.Errorf("destination BFD tuple is foreign or duplicated")
		}
		byHop[b.DstIP] = b
	}
	wantedBFD := map[string]bool{}
	for hop, timers := range plan.Sessions {
		old := byHop[hop]
		ops = append(ops, destinationBFDWait(bfdPort, hop, old))
		minRX, minTX, mult := timers.MinRX, timers.MinTX, timers.Multiplier
		if old == nil {
			b := &ovnnb.BFD{UUID: ovsclient.NamedUUID(), LogicalPort: bfdPort, DstIP: hop, MinRx: &minRX, MinTx: &minTX, DetectMult: &mult, ExternalIDs: map[string]string{"vendor": util.CniTypeName, destinationroute.OwnerKey: owner}}
			next, err := c.Create(b)
			if err != nil {
				return err
			}
			ops = append(ops, next...)
			byHop[hop] = b
			wantedBFD[b.UUID] = true
		} else {
			wantedBFD[old.UUID] = true
			if !reflect.DeepEqual(old.MinRx, &minRX) || !reflect.DeepEqual(old.MinTx, &minTX) || !reflect.DeepEqual(old.DetectMult, &mult) {
				ids, _ := ovsdb.NewOvsMap(old.ExternalIDs)
				ops = append(ops, destinationWait(ovnnb.BFDTable, "_uuid", ovsdb.UUID{GoUUID: old.UUID}, "external_ids", ids))
				old.MinRx, old.MinTx, old.DetectMult = &minRX, &minTX, &mult
				next, err := c.Where(old).Update(old, &old.MinRx, &old.MinTx, &old.DetectMult)
				if err != nil {
					return err
				}
				ops = append(ops, next...)
			}
		}
	}
	desired := map[string]*ovnnb.LogicalRouterStaticRoute{}
	policy := "dst-ip"
	for _, intent := range plan.Routes {
		ids := map[string]string{"vendor": util.CniTypeName, destinationroute.OwnerKey: owner, destinationroute.RouteKey: intent.CIDR}
		guard := &ovnnb.LogicalRouterStaticRoute{Policy: &policy, IPPrefix: intent.CIDR, Nexthop: "discard", ExternalIDs: ids}
		desired[destinationRouteKey(guard)] = guard
		for _, child := range destinationroute.Children(intent.CIDR) {
			for _, hop := range intent.NextHops {
				bfd := byHop[hop].UUID
				route := &ovnnb.LogicalRouterStaticRoute{Policy: &policy, IPPrefix: child, Nexthop: hop, BFD: &bfd, ExternalIDs: ids, SelectionFields: intent.SelectionFields, Options: map[string]string{util.StaticRouteBfdEcmp: "true"}}
				desired[destinationRouteKey(route)] = route
			}
		}
	}
	var delIDs, addIDs []string
	for _, old := range current {
		if old.ExternalIDs[destinationroute.OwnerKey] != owner {
			continue
		}
		key := destinationRouteKey(old)
		want := desired[key]
		if want != nil && destinationRouteEqual(old, want) {
			delete(desired, key)
			continue
		}
		ids, _ := ovsdb.NewOvsMap(old.ExternalIDs)
		ops = append(ops, destinationWait(ovnnb.LogicalRouterStaticRouteTable, "_uuid", ovsdb.UUID{GoUUID: old.UUID}, "external_ids", ids))
		delIDs = append(delIDs, old.UUID)
	}
	for _, route := range desired {
		route.UUID = ovsclient.NamedUUID()
		next, err := c.Create(route)
		if err != nil {
			return err
		}
		ops = append(ops, next...)
		addIDs = append(addIDs, route.UUID)
	}
	if len(delIDs) != 0 {
		next, err := c.LogicalRouterUpdateStaticRouteOp(router, delIDs, ovsdb.MutateOperationDelete)
		if err != nil {
			return err
		}
		ops = append(ops, next...)
	}
	if len(addIDs) != 0 {
		next, err := c.LogicalRouterUpdateStaticRouteOp(router, addIDs, ovsdb.MutateOperationInsert)
		if err != nil {
			return err
		}
		ops = append(ops, next...)
	}
	// Refuse to remove any BFD session that another consumer has referenced.
	ctx, cancel := context.WithTimeout(context.Background(), c.Timeout)
	defer cancel()
	var allRoutes []ovnnb.LogicalRouterStaticRoute
	var allPolicies []ovnnb.LogicalRouterPolicy
	if err := c.List(ctx, &allRoutes); err != nil {
		return err
	}
	if err := c.List(ctx, &allPolicies); err != nil {
		return err
	}
	for _, session := range sessions {
		if session.ExternalIDs[destinationroute.OwnerKey] != owner || wantedBFD[session.UUID] {
			continue
		}
		for _, r := range allRoutes {
			if r.BFD != nil && *r.BFD == session.UUID && !slices.Contains(delIDs, r.UUID) {
				return fmt.Errorf("obsolete destination BFD has another route consumer")
			}
		}
		for _, r := range allPolicies {
			if slices.Contains(r.BFDSessions, session.UUID) {
				return fmt.Errorf("obsolete destination BFD has a policy consumer")
			}
		}
		ids, _ := ovsdb.NewOvsMap(session.ExternalIDs)
		ops = append(ops, destinationWait(ovnnb.BFDTable, "_uuid", ovsdb.UUID{GoUUID: session.UUID}, "external_ids", ids))
		next, err := c.Where(&session).Delete()
		if err != nil {
			return err
		}
		ops = append(ops, next...)
	}
	// Pure wait operations mean the desired rows are already present.
	mutated := false
	for _, op := range ops {
		if op.Op != ovsdb.OperationWait {
			mutated = true
			break
		}
	}
	if !mutated {
		return nil
	}
	return c.Transact("vpc-destination-routes", ops)
}

// The NB schema makes (logical_port, dst_ip) unique. With at most one
// matching row, comparing against its nonempty tuple with until != is an
// atomic absence guard. Empty Rows is omitted by the pinned libovsdb JSON
// encoder and rejected by real ovsdb-server, even if a non-nil empty slice is used.
func destinationBFDWait(port, hop string, existing *ovnnb.BFD) ovsdb.Operation {
	zero := 0
	op := ovsdb.Operation{Op: ovsdb.OperationWait, Table: ovnnb.BFDTable,
		Where:   []ovsdb.Condition{ovsdb.NewCondition("logical_port", ovsdb.ConditionEqual, port), ovsdb.NewCondition("dst_ip", ovsdb.ConditionEqual, hop)},
		Columns: []string{"logical_port", "dst_ip"}, Until: string(ovsdb.WaitConditionNotEqual),
		Rows: []ovsdb.Row{{"logical_port": port, "dst_ip": hop}}, Timeout: &zero}
	if existing != nil {
		ids, _ := ovsdb.NewOvsMap(existing.ExternalIDs)
		op.Columns = []string{"_uuid", "external_ids"}
		op.Until = string(ovsdb.WaitConditionEqual)
		op.Rows = []ovsdb.Row{{"_uuid": ovsdb.UUID{GoUUID: existing.UUID}, "external_ids": ids}}
	}
	return op
}

func destinationWait(table, key string, value any, column string, expected any) ovsdb.Operation {
	zero := 0
	return ovsdb.Operation{Op: ovsdb.OperationWait, Table: table, Where: []ovsdb.Condition{ovsdb.NewCondition(key, ovsdb.ConditionEqual, value)}, Columns: []string{column}, Until: string(ovsdb.WaitConditionEqual), Rows: []ovsdb.Row{{column: expected}}, Timeout: &zero}
}
func destinationRouteKey(r *ovnnb.LogicalRouterStaticRoute) string {
	return r.IPPrefix + "|" + r.Nexthop
}
func destinationRouteEqual(a, b *ovnnb.LogicalRouterStaticRoute) bool {
	x, y := *a, *b
	x.UUID = ""
	y.UUID = ""
	x.SelectionFields = slices.Clone(x.SelectionFields)
	y.SelectionFields = slices.Clone(y.SelectionFields)
	slices.Sort(x.SelectionFields)
	slices.Sort(y.SelectionFields)
	if len(x.Options) == 0 {
		x.Options = nil
	}
	if len(y.Options) == 0 {
		y.Options = nil
	}
	if len(x.SelectionFields) == 0 {
		x.SelectionFields = nil
	}
	if len(y.SelectionFields) == 0 {
		y.SelectionFields = nil
	}
	return reflect.DeepEqual(x, y)
}
