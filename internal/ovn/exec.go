// Package ovn implements the narrow OVN fields owned by Global VPC.
// Kube-OVN owns routers, switches, ports and routes; native ovn-ic owns tunnel
// keys, remote ports, and interconnection southbound state.
package ovn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"globalvpc.io/controller/internal/config"
)

const ownerKey = "global-vpc-uid"
const maxOutput = 4 << 20

// ErrTransitMissing requires coordinated native-IC pause before any recreation.
var ErrTransitMissing = errors.New("previous transit switch is missing; refusing replacement")

// Runner is a test seam. Command prefixes come exclusively from administrator
// registration, never from GlobalVpc spec fields. Implementations must bound
// output and honor context cancellation.
type Runner interface {
	Run(context.Context, []string) ([]byte, error)
}
type RunnerFunc func(context.Context, []string) ([]byte, error)

func (f RunnerFunc) Run(ctx context.Context, command []string) ([]byte, error) {
	return f(ctx, command)
}

type Exec struct {
	Timeout time.Duration
	Runner  Runner
	// TransactionCommand is an administrator-owned ovsdb-client transact argv
	// prefix targeting the same local NB database. Only guarded BFD deletion uses
	// it; a JSON transaction is appended as one argument, without a shell.
	TransactionCommand []string
}

type boundedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
	cancel   context.CancelFunc
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if n > b.limit-b.buffer.Len() {
		b.overflow = true
		p = p[:b.limit-b.buffer.Len()]
		if b.cancel != nil {
			b.cancel()
		}
	}
	_, _ = b.buffer.Write(p)
	return n, nil
}

type processRunner struct{}

func (processRunner) Run(ctx context.Context, command []string) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	out := &boundedBuffer{limit: maxOutput, cancel: cancel}
	cmd.Stdout, cmd.Stderr = out, io.Discard
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if out.overflow {
		return nil, errors.New("OVN output limit exceeded")
	}
	return out.buffer.Bytes(), err
}

func (e *Exec) run(ctx context.Context, prefix []string, args ...string) ([]byte, error) {
	if len(prefix) == 0 || prefix[0] == "" {
		return nil, errors.New("OVN command is not configured")
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := append([]string{}, prefix...)
	command = append(command, "--timeout="+strconv.Itoa(int(math.Ceil(timeout.Seconds()))))
	command = append(command, args...)
	runner := e.Runner
	if runner == nil {
		runner = processRunner{}
	}
	out, err := runner.Run(ctx, command)
	// Arguments, child errors, and stderr can contain TLS material or credentials.
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("OVN command cancelled: %w", ctx.Err())
		}
		return nil, errors.New("OVN command failed")
	}
	if len(out) > maxOutput {
		return nil, errors.New("OVN output limit exceeded")
	}
	return out, nil
}

type row map[string]json.RawMessage

func (e *Exec) query(ctx context.Context, command []string, columns, table string, conditions ...string) ([]row, error) {
	args := []string{"--format=json", "--columns=" + columns, "find", table}
	if len(conditions) == 1 && strings.HasPrefix(conditions[0], "_uuid=") {
		// _uuid is a synthetic column, not a valid find condition in ovn-nbctl.
		// Read referenced rows through the CLI's UUID record selector instead.
		id := strings.TrimPrefix(conditions[0], "_uuid=")
		if !uuidPattern.MatchString(id) {
			return nil, errors.New("invalid OVN record UUID")
		}
		args = []string{"--format=json", "--columns=" + columns, "list", table, id}
	} else {
		args = append(args, conditions...)
	}
	out, err := e.run(ctx, command, args...)
	if err != nil {
		return nil, err
	}
	var data struct {
		Headings []string            `json:"headings"`
		Data     [][]json.RawMessage `json:"data"`
	}
	d := json.NewDecoder(bytes.NewReader(out))
	if d.Decode(&data) != nil {
		return nil, errors.New("invalid OVN JSON response")
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid trailing OVN JSON response")
	}
	want := strings.Split(columns, ",")
	if len(data.Headings) != len(want) || data.Data == nil {
		return nil, errors.New("invalid OVN table response")
	}
	seen := map[string]bool{}
	for _, h := range data.Headings {
		if seen[h] {
			return nil, errors.New("duplicate OVN heading")
		}
		seen[h] = true
	}
	for _, h := range want {
		if !seen[h] {
			return nil, errors.New("missing OVN heading")
		}
	}
	result := make([]row, 0, len(data.Data))
	for _, values := range data.Data {
		if len(values) != len(data.Headings) {
			return nil, errors.New("invalid OVN row")
		}
		r := row{}
		for i, h := range data.Headings {
			r[h] = values[i]
		}
		result = append(result, r)
	}
	return result, nil
}
func stringValue(v json.RawMessage) (string, error) {
	var s *string
	if json.Unmarshal(v, &s) != nil || s == nil {
		return "", errors.New("invalid OVN string")
	}
	return *s, nil
}
func mapValue(v json.RawMessage) (map[string]string, error) {
	var tagged []json.RawMessage
	if json.Unmarshal(v, &tagged) != nil || len(tagged) != 2 {
		return nil, errors.New("invalid OVN map")
	}
	tag, _ := stringValue(tagged[0])
	if tag != "map" {
		return nil, errors.New("invalid OVN map tag")
	}
	var pairs [][]json.RawMessage
	if json.Unmarshal(tagged[1], &pairs) != nil || pairs == nil {
		return nil, errors.New("invalid OVN map entries")
	}
	m := map[string]string{}
	for _, pair := range pairs {
		if len(pair) != 2 {
			return nil, errors.New("invalid OVN map pair")
		}
		k, e1 := stringValue(pair[0])
		v, e2 := stringValue(pair[1])
		if e1 != nil || e2 != nil {
			return nil, errors.New("invalid OVN map strings")
		}
		if _, ok := m[k]; ok {
			return nil, errors.New("duplicate OVN map key")
		}
		m[k] = v
	}
	return m, nil
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func uuidValue(v json.RawMessage) (string, error) {
	var tagged []string
	if json.Unmarshal(v, &tagged) != nil || len(tagged) != 2 || tagged[0] != "uuid" || !uuidPattern.MatchString(tagged[1]) {
		return "", errors.New("invalid OVN UUID")
	}
	return tagged[1], nil
}
func setValue(v json.RawMessage, uuid bool) ([]string, error) {
	var tagged []json.RawMessage
	if json.Unmarshal(v, &tagged) == nil && len(tagged) == 2 {
		tag, _ := stringValue(tagged[0])
		if tag == "set" {
			var values []json.RawMessage
			if json.Unmarshal(tagged[1], &values) != nil || values == nil {
				return nil, errors.New("invalid OVN set")
			}
			result := []string{}
			seen := map[string]bool{}
			for _, value := range values {
				var s string
				var err error
				if uuid {
					s, err = uuidValue(value)
				} else {
					s, err = stringValue(value)
				}
				if err != nil {
					return nil, err
				}
				if seen[s] {
					return nil, errors.New("duplicate OVN set member")
				}
				seen[s] = true
				result = append(result, s)
			}
			return result, nil
		}
	}
	var s string
	var err error
	if uuid {
		s, err = uuidValue(v)
	} else {
		s, err = stringValue(v)
	}
	if err != nil {
		return nil, err
	}
	return []string{s}, nil
}
func single(rows []row) (row, error) {
	if len(rows) != 1 {
		return nil, errors.New("expected exactly one OVN row")
	}
	return rows[0], nil
}

func namedSingle(rows []row, want string) (row, error) {
	r, err := single(rows)
	if err != nil {
		return nil, err
	}
	name, err := stringValue(r["name"])
	if err != nil {
		return nil, err
	}
	if name != want {
		return nil, errors.New("OVN row name does not match query")
	}
	return r, nil
}
func condition(column, value string) string { return column + "=" + strconv.Quote(value) }
func uuidSet(values []string) string        { return "[" + strings.Join(values, ",") + "]" }

// Check validates shared prerequisites without taking ownership of them.
func (e *Exec) Check(ctx context.Context, cfg config.Cluster) error {
	rows, err := e.query(ctx, cfg.NBCommand, "name", "NB_Global")
	if err != nil {
		return err
	}
	r, err := single(rows)
	if err != nil {
		return err
	}
	name, err := stringValue(r["name"])
	if err != nil {
		return err
	}
	if name != cfg.AZName || name == "" {
		return errors.New("NB availability zone does not match registration")
	}
	if len(cfg.GatewayChassis) == 0 || len(cfg.GatewayChassis) > 100 {
		return errors.New("registration requires 1-100 gateway chassis")
	}
	seen := map[string]bool{}
	for _, chassis := range cfg.GatewayChassis {
		if chassis == "" || seen[chassis] {
			return errors.New("invalid gateway chassis registration")
		}
		seen[chassis] = true
		rows, err := e.query(ctx, cfg.SBCommand, "name,other_config", "Chassis", condition("name", chassis))
		if err != nil {
			return err
		}
		r, err := single(rows)
		if err != nil {
			return err
		}
		name, err := stringValue(r["name"])
		if err != nil {
			return err
		}
		other, err := mapValue(r["other_config"])
		if err != nil {
			return err
		}
		if name != chassis || other["is-interconn"] != "true" || other["is-remote"] == "true" {
			return errors.New("registered chassis is not a local interconnection gateway")
		}
	}
	return nil
}

// Mark must only be called during a coordinated native-IC pause when the IC
// transit row does not exist: native IC deletes orphan marked switches.
func (e *Exec) Mark(ctx context.Context, cfg config.Cluster, transitSwitch string) error {
	rows, err := e.query(ctx, cfg.NBCommand, "_uuid,name,other_config", "Logical_Switch", condition("name", transitSwitch))
	if err != nil {
		return err
	}
	r, err := namedSingle(rows, transitSwitch)
	if err != nil {
		return err
	}
	id, err := uuidValue(r["_uuid"])
	if err != nil {
		return err
	}
	other, err := mapValue(r["other_config"])
	if err != nil {
		return err
	}
	value, marked := other["interconn-ts"]
	if marked && value != transitSwitch {
		return errors.New("logical switch already has a different interconnection marker")
	}
	markers, err := e.query(ctx, cfg.NBCommand, "_uuid", "Logical_Switch", condition("other_config:interconn-ts", transitSwitch))
	if err != nil {
		return err
	}
	if len(markers) > 1 {
		return errors.New("duplicate transit switch markers")
	}
	if len(markers) == 1 {
		markerID, err := uuidValue(markers[0]["_uuid"])
		if err != nil {
			return err
		}
		if markerID != id {
			return errors.New("transit marker belongs to another logical switch")
		}
	}
	if marked {
		return nil
	}
	_, err = e.run(ctx, cfg.NBCommand, "get", "Logical_Switch", id, "name", "other_config", "--", "wait-until", "Logical_Switch", id, condition("name", transitSwitch), "other_config:interconn-ts{=}[]", "--", "set", "Logical_Switch", id, condition("other_config:interconn-ts", transitSwitch))
	return err
}

// Gateways exclusively owns gateway_chassis on this per-VPC router port. It
// replaces that reference set atomically and leaves shared chassis untouched.
func (e *Exec) Gateways(ctx context.Context, cfg config.Cluster, routerPort string) error {
	if len(cfg.GatewayChassis) == 0 || len(cfg.GatewayChassis) > 100 {
		return errors.New("registration requires 1-100 gateway chassis")
	}
	want := map[string]int{}
	for i, c := range cfg.GatewayChassis {
		if c == "" {
			return errors.New("empty gateway chassis")
		}
		if _, ok := want[c]; ok {
			return errors.New("duplicate gateway chassis")
		}
		want[c] = 100 - i
	}
	rows, err := e.query(ctx, cfg.NBCommand, "_uuid,name,gateway_chassis", "Logical_Router_Port", condition("name", routerPort))
	if err != nil {
		return err
	}
	r, err := namedSingle(rows, routerPort)
	if err != nil {
		return err
	}
	id, err := uuidValue(r["_uuid"])
	if err != nil {
		return err
	}
	refs, err := setValue(r["gateway_chassis"], true)
	if err != nil {
		return err
	}
	current := map[string]int{}
	duplicate := false
	for _, ref := range refs {
		rows, err := e.query(ctx, cfg.NBCommand, "chassis_name,priority", "Gateway_Chassis", "_uuid="+ref)
		if err != nil {
			return err
		}
		g, err := single(rows)
		if err != nil {
			return err
		}
		name, err := stringValue(g["chassis_name"])
		if err != nil {
			return err
		}
		var priority int
		if json.Unmarshal(g["priority"], &priority) != nil {
			return errors.New("invalid gateway priority")
		}
		if _, ok := current[name]; ok {
			duplicate = true
		}
		current[name] = priority
	}
	equal := !duplicate && len(current) == len(want)
	for name, priority := range want {
		if got, ok := current[name]; !ok || got != priority {
			equal = false
		}
	}
	if equal {
		return nil
	}
	args := []string{"get", "Logical_Router_Port", id, "name", "gateway_chassis", "--", "wait-until", "Logical_Router_Port", id, condition("name", routerPort), "gateway_chassis=" + uuidSet(refs), "--", "clear", "Logical_Router_Port", id, "gateway_chassis"}
	for i, chassis := range cfg.GatewayChassis {
		ref := "@gw" + strconv.Itoa(i)
		args = append(args, "--", "--id="+ref, "create", "Gateway_Chassis", condition("name", routerPort+"-"+chassis), condition("chassis_name", chassis), "priority="+strconv.Itoa(100-i), "--", "add", "Logical_Router_Port", id, "gateway_chassis", ref)
	}
	_, err = e.run(ctx, cfg.NBCommand, args...)
	return err
}

func (e *Exec) transit(ctx context.Context, command []string, name, ownerUID, expectedUUID string) (string, error) {
	if name == "" || ownerUID == "" || (expectedUUID != "" && !uuidPattern.MatchString(expectedUUID)) {
		return "", errors.New("invalid transit identity")
	}
	rows, err := e.query(ctx, command, "_uuid,name,external_ids", "Transit_Switch", condition("name", name))
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", nil
	}
	r, err := namedSingle(rows, name)
	if err != nil {
		return "", err
	}
	id, err := uuidValue(r["_uuid"])
	if err != nil {
		return "", err
	}
	ids, err := mapValue(r["external_ids"])
	if err != nil {
		return "", err
	}
	if ids[ownerKey] != ownerUID {
		return "", errors.New("transit switch ownership conflict")
	}
	if expectedUUID != "" && id != expectedUUID {
		return "", errors.New("transit switch UUID changed")
	}
	return id, nil
}

// InspectTransit validates any existing row without creating or adopting it.
// An absent row returns an empty UUID so the reconciler can stage recovery.
func (e *Exec) InspectTransit(ctx context.Context, command []string, name, ownerUID, expectedUUID string) (string, error) {
	return e.transit(ctx, command, name, ownerUID, expectedUUID)
}

// EnsureTransit tags ownership in the creation transaction. A lost create
// response is recoverable by the name/UID pair; no existing row is adopted.
func (e *Exec) EnsureTransit(ctx context.Context, command []string, name, ownerUID, expectedUUID string) (string, error) {
	id, err := e.transit(ctx, command, name, ownerUID, expectedUUID)
	if err != nil || id != "" {
		return id, err
	}
	if expectedUUID != "" {
		return "", ErrTransitMissing
	}
	_, createErr := e.run(ctx, command, "--id=@ts", "create", "Transit_Switch", condition("name", name), condition("external_ids:"+ownerKey, ownerUID))
	// Always re-read, including timeout/connection loss. The schema's unique
	// name index prevents a concurrent creator from inserting a duplicate.
	id, err = e.transit(ctx, command, name, ownerUID, "")
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil
	}
	if createErr != nil {
		return "", createErr
	}
	return "", errors.New("transit creation did not produce an owned row")
}

// DeleteTransit guards both the exact row UUID and owner in the same OVSDB
// transaction as destroy, so an observed row can never authorize its replacement.
func (e *Exec) DeleteTransit(ctx context.Context, command []string, name, ownerUID, expectedUUID string) error {
	id, err := e.transit(ctx, command, name, ownerUID, expectedUUID)
	if err != nil || id == "" {
		return err
	}
	// get adds transaction verification; wait-until alone merely checks the
	// IDL snapshot and cannot guard a concurrent external_ids change.
	_, err = e.run(ctx, command, "get", "Transit_Switch", id, "name", "external_ids", "--", "wait-until", "Transit_Switch", id, condition("name", name), condition("external_ids:"+ownerKey, ownerUID), "--", "destroy", "Transit_Switch", id)
	return err
}

func (e *Exec) switchPorts(ctx context.Context, cfg config.Cluster, name string) (row, []row, error) {
	rows, err := e.query(ctx, cfg.NBCommand, "_uuid,name,ports,other_config", "Logical_Switch", condition("name", name))
	if err != nil {
		return nil, nil, err
	}
	if len(rows) == 0 {
		return nil, nil, nil
	}
	ls, err := namedSingle(rows, name)
	if err != nil {
		return nil, nil, err
	}
	refs, err := setValue(ls["ports"], true)
	if err != nil {
		return nil, nil, err
	}
	ports := make([]row, 0, len(refs))
	for _, ref := range refs {
		rows, err := e.query(ctx, cfg.NBCommand, "name,type,options,addresses", "Logical_Switch_Port", "_uuid="+ref)
		if err != nil {
			return nil, nil, err
		}
		port, err := single(rows)
		if err != nil {
			return nil, nil, err
		}
		ports = append(ports, port)
	}
	return ls, ports, nil
}

// EndpointsEmpty permits only router and native remote ports. All other types,
// including localnet/external/unknown types, conservatively block deletion.
func (e *Exec) EndpointsEmpty(ctx context.Context, cfg config.Cluster, subnetName string) (bool, error) {
	_, ports, err := e.switchPorts(ctx, cfg, subnetName)
	if err != nil {
		return false, err
	}
	for _, port := range ports {
		typ, err := stringValue(port["type"])
		if err != nil {
			return false, err
		}
		if typ != "router" && typ != "remote" {
			return false, nil
		}
	}
	return true, nil
}

func (e *Exec) LocalAbsent(ctx context.Context, cfg config.Cluster, vpc, subnet, transit string) (bool, error) {
	queries := [][2]string{{"Logical_Router", vpc}, {"Logical_Switch", subnet}, {"Logical_Switch", transit}, {"Logical_Router_Port", vpc + "-" + subnet}, {"Logical_Router_Port", vpc + "-" + transit}, {"Logical_Switch_Port", subnet + "-" + vpc}, {"Logical_Switch_Port", transit + "-" + vpc}}
	for _, q := range queries {
		rows, err := e.query(ctx, cfg.NBCommand, "_uuid", q[0], condition("name", q[1]))
		if err != nil {
			return false, err
		}
		if len(rows) > 0 {
			return false, nil
		}
	}
	return true, nil
}

// Route is a Kube-OVN-owned route that must converge in the main table in NB.
// An empty Policy means dst-ip for compatibility with the OVN-IC planner.
type Route struct{ CIDR, NextHopIP, Policy, BFD string }

// RoutesReady confirms the exact expected route set on the tenant router.
// Kube-OVN's implicit subnet src-ip routes must be enumerated by callers that
// expect them. Unregistered routes, policies or tables prevent readiness.
func (e *Exec) RoutesReady(ctx context.Context, cfg config.Cluster, router string, routes []Route) (bool, error) {
	rows, err := e.query(ctx, cfg.NBCommand, "name,static_routes", "Logical_Router", condition("name", router))
	if err != nil {
		return false, err
	}
	if len(rows) == 0 {
		return false, nil
	}
	r, err := namedSingle(rows, router)
	if err != nil {
		return false, err
	}
	refs, err := setValue(r["static_routes"], true)
	if err != nil {
		return false, err
	}
	if len(refs) != len(routes) {
		return false, nil
	}
	want := map[Route]int{}
	checkBFD := false
	for _, route := range routes {
		if route.Policy == "" {
			route.Policy = "dst-ip"
		}
		if route.CIDR == "" || route.NextHopIP == "" || (route.Policy != "dst-ip" && route.Policy != "src-ip") {
			return false, errors.New("invalid expected route")
		}
		want[route]++
		checkBFD = checkBFD || route.BFD != ""
	}
	for _, ref := range refs {
		columns := "ip_prefix,nexthop,policy,route_table"
		if checkBFD {
			columns += ",bfd"
		}
		rows, err := e.query(ctx, cfg.NBCommand, columns, "Logical_Router_Static_Route", "_uuid="+ref)
		if err != nil {
			return false, err
		}
		row, err := single(rows)
		if err != nil {
			return false, err
		}
		cidr, err := stringValue(row["ip_prefix"])
		if err != nil {
			return false, err
		}
		nextHop, err := stringValue(row["nexthop"])
		if err != nil {
			return false, err
		}
		policy, err := setValue(row["policy"], false)
		if err != nil {
			return false, err
		}
		routeTable, err := stringValue(row["route_table"])
		if err != nil {
			return false, err
		}
		if len(policy) > 1 || (len(policy) == 1 && policy[0] != "dst-ip" && policy[0] != "src-ip") || routeTable != "" {
			return false, nil
		}
		actualPolicy := "dst-ip"
		if len(policy) == 1 {
			actualPolicy = policy[0]
		}
		key := Route{CIDR: cidr, NextHopIP: nextHop, Policy: actualPolicy}
		if checkBFD {
			bfd, err := setValue(row["bfd"], true)
			if err != nil {
				return false, err
			}
			if len(bfd) > 1 {
				return false, nil
			}
			if len(bfd) == 1 {
				key.BFD = bfd[0]
			}
		}
		if want[key] == 0 {
			return false, nil
		}
		want[key]--
	}
	return true, nil
}

func positiveKey(s string, maximum uint64) bool {
	n, err := strconv.ParseUint(s, 10, 32)
	return err == nil && n > 0 && n <= maximum
}

// Connected verifies NB convergence generated by native IC. It does not prove
// that gateway tunnels or tenant packets work. Peer router ports use Kube-OVN's
// <vpc>-<subnet> naming and remote LSPs preserve <subnet>-<vpc> names.
func (e *Exec) Connected(ctx context.Context, cfg config.Cluster, ts, routerPort string, peerRouterPorts []string) (bool, error) {
	ls, ports, err := e.switchPorts(ctx, cfg, ts)
	if err != nil {
		return false, err
	}
	if ls == nil {
		return false, nil
	}
	other, err := mapValue(ls["other_config"])
	if err != nil {
		return false, err
	}
	if other["interconn-ts"] != ts || !positiveKey(other["requested-tnl-key"], (1<<24)-1) {
		return false, nil
	}
	expected := map[string]bool{}
	for _, peer := range peerRouterPorts {
		if !strings.HasSuffix(peer, "-"+ts) || peer == "-"+ts {
			return false, errors.New("invalid peer router-port identity")
		}
		expected[ts+"-"+strings.TrimSuffix(peer, "-"+ts)] = false
	}
	local := false
	for _, port := range ports {
		name, err := stringValue(port["name"])
		if err != nil {
			return false, err
		}
		typ, err := stringValue(port["type"])
		if err != nil {
			return false, err
		}
		options, err := mapValue(port["options"])
		if err != nil {
			return false, err
		}
		if typ == "router" && options["router-port"] == routerPort {
			local = positiveKey(options["requested-tnl-key"], 32767)
		}
		if _, ok := expected[name]; ok {
			addresses, err := setValue(port["addresses"], false)
			if err != nil {
				return false, err
			}
			expected[name] = typ == "remote" && positiveKey(options["requested-tnl-key"], 32767) && len(addresses) > 0 && addresses[0] != ""
		}
	}
	if !local {
		return false, nil
	}
	for _, ready := range expected {
		if !ready {
			return false, nil
		}
	}
	return true, nil
}
