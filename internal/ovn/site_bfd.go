package ovn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	api "globalvpc.io/controller/api/v1alpha1"
	"globalvpc.io/controller/internal/config"
	"globalvpc.io/controller/internal/siteplan"
)

const bfdGatewayKey = "global-vpc-gateway-id"
const bfdClusterKey = "global-vpc-cluster-uid"
const bfdRouterKey = "global-vpc-router"
const bfdColumns = "_uuid,logical_port,dst_ip,external_ids,min_rx,min_tx,detect_mult,status"

// ErrBFDPortPending means the native Kube-OVN BFD-only port or its redundant
// chassis group has not converged yet. This adapter never writes either object.
var ErrBFDPortPending = errors.New("waiting for native BFD port with at least two chassis")

// BFDState separates durable identities from a transient local health sample.
// Only dedicated BFD rows belong to this adapter. Kube-OVN owns the router,
// BFD-only LRP, HA chassis group, transit ports and static route references.
type BFDState struct {
	Records    []api.BFDResourceRecord
	Up         []string
	Configured bool
}

func bfdExpected(g siteplan.GatewayConfig, records []api.BFDResourceRecord) (map[string]string, error) {
	if g.Version != "v2" || g.BFD == nil || len(g.Gateways) < 2 || g.OwnerUID == "" {
		return nil, errors.New("multi-gateway BFD configuration is required")
	}
	members := map[string]bool{}
	for _, member := range g.Gateways {
		members[member.ID] = true
	}
	expected := map[string]string{}
	for _, record := range records {
		if !members[record.GatewayID] || !uuidPattern.MatchString(record.UUID) || expected[record.GatewayID] != "" {
			return nil, errors.New("invalid BFD identity receipt")
		}
		expected[record.GatewayID] = record.UUID
	}
	return expected, nil
}

func intValue(raw json.RawMessage) (int, error) {
	var value int
	if json.Unmarshal(raw, &value) == nil && string(raw) != "null" {
		return value, nil
	}
	var tagged []json.RawMessage
	if json.Unmarshal(raw, &tagged) != nil || len(tagged) != 2 || string(tagged[0]) != `"set"` {
		return 0, errors.New("invalid OVN optional integer")
	}
	var values []json.RawMessage
	if json.Unmarshal(tagged[1], &values) != nil || values == nil || len(values) > 1 {
		return 0, errors.New("invalid OVN optional integer set")
	}
	if len(values) == 0 {
		return 0, nil
	}
	if json.Unmarshal(values[0], &value) != nil || string(values[0]) == "null" {
		return 0, errors.New("invalid OVN integer")
	}
	return value, nil
}

// GetBFD fails closed on duplicates, ownership changes and same-name UUID
// replacements. A confirmed missing row is distinct from an unreadable database.
func (e *Exec) GetBFD(ctx context.Context, cfg config.Cluster, g siteplan.GatewayConfig, records []api.BFDResourceRecord) (BFDState, error) {
	expected, err := bfdExpected(g, records)
	if err != nil {
		return BFDState{}, err
	}
	rows, err := e.query(ctx, cfg.NBCommand, bfdColumns, "BFD")
	if err != nil {
		return BFDState{}, err
	}
	byID, byIP := map[string]string{}, map[string]string{}
	for _, member := range g.Gateways {
		byID[member.ID], byIP[member.IP] = member.IP, member.ID
	}
	state := BFDState{Configured: true}
	seen := map[string]bool{}
	for _, r := range rows {
		id, err := uuidValue(r["_uuid"])
		if err != nil {
			return BFDState{}, err
		}
		ids, err := mapValue(r["external_ids"])
		if err != nil {
			return BFDState{}, err
		}
		port, err := stringValue(r["logical_port"])
		if err != nil {
			return BFDState{}, err
		}
		ip, err := stringValue(r["dst_ip"])
		if err != nil {
			return BFDState{}, err
		}
		recorded := false
		for _, uuid := range expected {
			recorded = recorded || uuid == id
		}
		if ids[ownerKey] != g.OwnerUID {
			if recorded || (port == g.BFDPortName() && byIP[ip] != "") {
				return BFDState{}, errors.New("BFD ownership conflict")
			}
			continue
		}
		member := ids[bfdGatewayKey]
		if byID[member] != ip || byID[member] == "" || port != g.BFDPortName() ||
			ids[bfdClusterKey] != cfg.UID || ids[bfdRouterKey] != g.VpcName || seen[member] ||
			(expected[member] != "" && expected[member] != id) {
			return BFDState{}, errors.New("BFD identity or local ownership changed")
		}
		seen[member] = true
		configured := true
		for field, wanted := range map[string]int{"min_rx": g.BFD.MinRX, "min_tx": g.BFD.MinTX, "detect_mult": g.BFD.Multiplier} {
			actual, err := intValue(r[field])
			if err != nil {
				return BFDState{}, err
			}
			configured = configured && actual == wanted
		}
		statuses, err := setValue(r["status"], false)
		if err != nil {
			return BFDState{}, err
		}
		status := ""
		if len(statuses) > 1 {
			return BFDState{}, errors.New("multiple BFD statuses")
		}
		if len(statuses) == 1 {
			status = statuses[0]
		}
		if status != "" && status != "up" && status != "down" && status != "init" && status != "admin_down" {
			return BFDState{}, errors.New("unknown BFD status")
		}
		state.Configured = state.Configured && configured
		state.Records = append(state.Records, api.BFDResourceRecord{GatewayID: member, UUID: id})
		if configured && status == "up" {
			state.Up = append(state.Up, member)
		}
	}
	sort.Slice(state.Records, func(i, j int) bool { return state.Records[i].GatewayID < state.Records[j].GatewayID })
	sort.Strings(state.Up)
	state.Configured = state.Configured && len(state.Records) == len(g.Gateways)
	return state, nil
}

func (e *Exec) bfdPortReady(ctx context.Context, cfg config.Cluster, g siteplan.GatewayConfig) error {
	rows, err := e.query(ctx, cfg.NBCommand, "_uuid,name,networks,options,ha_chassis_group", "Logical_Router_Port", condition("name", g.BFDPortName()))
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return ErrBFDPortPending
	}
	r, err := namedSingle(rows, g.BFDPortName())
	if err != nil {
		return err
	}
	portID, err := uuidValue(r["_uuid"])
	if err != nil {
		return err
	}
	networks, err := setValue(r["networks"], false)
	if err != nil {
		return err
	}
	options, err := mapValue(r["options"])
	if err != nil {
		return err
	}
	groups, err := setValue(r["ha_chassis_group"], true)
	if err != nil {
		return err
	}
	if len(networks) != 1 || (networks[0] != g.BFD.SourceIP && networks[0] != g.BFD.SourceIP+"/32") || options["bfd-only"] != "true" || len(groups) != 1 {
		return ErrBFDPortPending
	}
	rows, err = e.query(ctx, cfg.NBCommand, "name,ports", "Logical_Router", condition("name", g.VpcName))
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return ErrBFDPortPending
	}
	r, err = namedSingle(rows, g.VpcName)
	if err != nil {
		return err
	}
	ports, err := setValue(r["ports"], true)
	if err != nil {
		return err
	}
	if !contains(ports, portID) {
		return errors.New("BFD port is not attached to the owned router")
	}
	rows, err = e.query(ctx, cfg.NBCommand, "ha_chassis", "HA_Chassis_Group", "_uuid="+groups[0])
	if err != nil {
		return err
	}
	r, err = single(rows)
	if err != nil {
		return err
	}
	chassis, err := setValue(r["ha_chassis"], true)
	if err != nil {
		return err
	}
	if len(chassis) < 2 {
		return ErrBFDPortPending
	}
	names := map[string]bool{}
	for _, id := range chassis {
		rows, err := e.query(ctx, cfg.NBCommand, "chassis_name", "HA_Chassis", "_uuid="+id)
		if err != nil {
			return err
		}
		r, err := single(rows)
		if err != nil {
			return err
		}
		name, err := stringValue(r["chassis_name"])
		if err != nil {
			return err
		}
		if name != "" {
			names[name] = true
		}
	}
	if len(names) < 2 {
		return ErrBFDPortPending
	}
	return nil
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func bfdGuard(id string, cfg config.Cluster, g siteplan.GatewayConfig, member, ip string) []string {
	// get adds IDL transaction verification. wait-until alone checks only the
	// snapshot and would not protect against a concurrent ownership change.
	return []string{"get", "BFD", id, "external_ids", "logical_port", "dst_ip", "--",
		"wait-until", "BFD", id, condition("external_ids:"+ownerKey, g.OwnerUID),
		condition("external_ids:"+bfdGatewayKey, member), condition("external_ids:"+bfdClusterKey, cfg.UID),
		condition("external_ids:"+bfdRouterKey, g.VpcName), condition("logical_port", g.BFDPortName()), condition("dst_ip", ip)}
}

// EnsureBFD performs no health-based route rewrite. OVN handles state changes
// after references converge, including when the Site Controller is stopped.
func (e *Exec) EnsureBFD(ctx context.Context, cfg config.Cluster, g siteplan.GatewayConfig, records []api.BFDResourceRecord) (BFDState, error) {
	state, err := e.GetBFD(ctx, cfg, g, records)
	if err != nil {
		return state, err
	}
	if err = e.bfdPortReady(ctx, cfg, g); err != nil {
		return state, err
	}
	byID := map[string]string{}
	for _, record := range state.Records {
		byID[record.GatewayID] = record.UUID
	}
	expected, _ := bfdExpected(g, records)
	for _, member := range g.Gateways {
		id := byID[member.ID]
		if id == "" && expected[member.ID] != "" {
			return state, errors.New("persist confirmed missing BFD receipt before recreation")
		}
		args := []string{}
		if id == "" {
			args = []string{"create", "BFD", condition("logical_port", g.BFDPortName()), condition("dst_ip", member.IP),
				condition("external_ids:"+ownerKey, g.OwnerUID), condition("external_ids:"+bfdGatewayKey, member.ID),
				condition("external_ids:"+bfdClusterKey, cfg.UID), condition("external_ids:"+bfdRouterKey, g.VpcName)}
		} else if !state.Configured {
			args = append(bfdGuard(id, cfg, g, member.ID, member.IP), "--", "set", "BFD", id)
		} else {
			continue
		}
		args = append(args, "min_rx="+strconv.Itoa(g.BFD.MinRX), "min_tx="+strconv.Itoa(g.BFD.MinTX), "detect_mult="+strconv.Itoa(g.BFD.Multiplier))
		if _, err = e.run(ctx, cfg.NBCommand, args...); err != nil {
			return state, fmt.Errorf("BFD write outcome is unknown; retry discovery: %w", err)
		}
	}
	return e.GetBFD(ctx, cfg, g, records)
}

// DeleteBFD waits for every route reference to disappear. It does not delete or
// alter a route owned by another reconciler as a shortcut for cleanup.
func (e *Exec) DeleteBFD(ctx context.Context, cfg config.Cluster, g siteplan.GatewayConfig, records []api.BFDResourceRecord) (bool, error) {
	state, err := e.GetBFD(ctx, cfg, g, records)
	if err != nil {
		return false, err
	}
	if len(state.Records) == 0 {
		return true, nil
	}
	refs := map[string]bool{}
	for _, query := range [][2]string{{"Logical_Router_Static_Route", "bfd"}, {"Logical_Router_Policy", "bfd_sessions"}} {
		rows, err := e.query(ctx, cfg.NBCommand, query[1], query[0])
		if err != nil {
			return false, err
		}
		for _, r := range rows {
			ids, err := setValue(r[query[1]], true)
			if err != nil {
				return false, err
			}
			for _, id := range ids {
				refs[id] = true
			}
		}
	}
	byID := map[string]string{}
	for _, member := range g.Gateways {
		byID[member.ID] = member.IP
	}
	for _, record := range state.Records {
		if refs[record.UUID] {
			return false, nil
		}
	}
	// All references are weak in the NB schema. Rechecking their absence in
	// the same server-side transaction prevents a concurrent reference insertion
	// from being silently stripped by destroy. CLI snapshot checks cannot do so.
	ops := []any{"OVN_Northbound"}
	deleteIndexes := []int{}
	for _, record := range state.Records {
		uuid := []any{"uuid", record.UUID}
		where := []any{[]any{"_uuid", "==", uuid},
			[]any{"logical_port", "==", g.BFDPortName()}, []any{"dst_ip", "==", byID[record.GatewayID]},
			[]any{"external_ids", "includes", []any{"map", [][]string{{ownerKey, g.OwnerUID}, {bfdGatewayKey, record.GatewayID}, {bfdClusterKey, cfg.UID}, {bfdRouterKey, g.VpcName}}}}}
		ops = append(ops, map[string]any{"op": "wait", "table": "BFD", "where": where, "columns": []string{"_uuid"}, "until": "==", "rows": []any{map[string]any{"_uuid": uuid}}, "timeout": 0})
		for _, table := range [][2]string{{"Logical_Router_Static_Route", "bfd"}, {"Logical_Router_Policy", "bfd_sessions"}} {
			ops = append(ops, map[string]any{"op": "wait", "table": table[0], "where": []any{[]any{table[1], "includes", []any{"set", []any{uuid}}}}, "columns": []string{"_uuid"}, "until": "==", "rows": []any{}, "timeout": 0})
		}
		deleteIndexes = append(deleteIndexes, len(ops)-1)
		ops = append(ops, map[string]any{"op": "delete", "table": "BFD", "where": where})
	}
	if err = e.bfdDeleteTransaction(ctx, ops, deleteIndexes); err != nil {
		return false, err
	}
	return false, nil // Confirm committed absence with a separate observation.
}

func (e *Exec) bfdDeleteTransaction(ctx context.Context, ops []any, deleteIndexes []int) error {
	if len(e.TransactionCommand) == 0 || strings.TrimSpace(e.TransactionCommand[0]) == "" {
		return errors.New("local BFD transaction command is not configured")
	}
	body, err := json.Marshal(ops)
	if err != nil || len(body) > maxOutput {
		return errors.New("invalid BFD transaction")
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	runner := e.Runner
	if runner == nil {
		runner = processRunner{}
	}
	command := append(append([]string{}, e.TransactionCommand...), string(body))
	out, err := runner.Run(ctx, command)
	if err != nil || ctx.Err() != nil || len(out) > maxOutput {
		return errors.New("BFD deletion outcome unknown; preserve receipts and retry discovery")
	}
	var responses []map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(out))
	if decoder.Decode(&responses) != nil || len(responses) != len(ops)-1 {
		return errors.New("invalid BFD transaction response; preserve receipts")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("trailing BFD transaction response; preserve receipts")
	}
	for _, response := range responses {
		if response == nil {
			return errors.New("BFD transaction aborted; preserve receipts")
		}
		if _, failed := response["error"]; failed {
			return errors.New("BFD transaction precondition failed; preserve receipts")
		}
	}
	for _, index := range deleteIndexes {
		var count int
		if json.Unmarshal(responses[index]["count"], &count) != nil || count != 1 {
			return errors.New("BFD deletion was not confirmed; preserve receipts")
		}
	}
	return nil
}

// BFDRouteConflicts identifies existing routes whose BFD identity is stale.
// Kube-OVN 1.16.4's static-route diff ignores bfdId-only changes, so the caller
// must withdraw these routes through the Vpc API and observe absence before
// reintroducing them. Otherwise a recovered BFD row can leave an untracked route.
func (e *Exec) BFDRouteConflicts(ctx context.Context, cfg config.Cluster, g siteplan.GatewayConfig, records []api.BFDResourceRecord) (map[string]bool, error) {
	byID := map[string]string{}
	for _, record := range records {
		byID[record.GatewayID] = record.UUID
	}
	want := map[string]string{}
	for _, member := range g.Gateways {
		want[member.IP] = byID[member.ID]
	}
	conflicts := map[string]bool{}
	rows, err := e.query(ctx, cfg.NBCommand, "name,static_routes", "Logical_Router", condition("name", g.VpcName))
	if err != nil || len(rows) == 0 {
		return conflicts, err
	}
	r, err := namedSingle(rows, g.VpcName)
	if err != nil {
		return nil, err
	}
	refs, err := setValue(r["static_routes"], true)
	if err != nil {
		return nil, err
	}
	for _, ref := range refs {
		rows, err := e.query(ctx, cfg.NBCommand, "ip_prefix,nexthop,policy,route_table,bfd", "Logical_Router_Static_Route", "_uuid="+ref)
		if err != nil {
			return nil, err
		}
		r, err := single(rows)
		if err != nil {
			return nil, err
		}
		cidr, err := stringValue(r["ip_prefix"])
		if err != nil {
			return nil, err
		}
		ip, err := stringValue(r["nexthop"])
		if err != nil {
			return nil, err
		}
		policy, err := setValue(r["policy"], false)
		if err != nil {
			return nil, err
		}
		table, err := stringValue(r["route_table"])
		if err != nil {
			return nil, err
		}
		if _, known := want[ip]; !known || cidr != "0.0.0.0/0" || table != "" || (len(policy) != 0 && strings.Join(policy, ",") != "dst-ip") {
			continue
		}
		bfd, err := setValue(r["bfd"], true)
		if err != nil {
			return nil, err
		}
		if len(bfd) != 1 || bfd[0] != want[ip] || want[ip] == "" {
			conflicts[ip] = true
		}
	}
	return conflicts, nil
}
