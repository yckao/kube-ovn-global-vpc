package ovn

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"globalvpc.io/controller/internal/config"
)

const id1 = "11111111-1111-1111-1111-111111111111"
const id2 = "22222222-2222-2222-2222-222222222222"
const id3 = "33333333-3333-3333-3333-333333333333"

func ovsMap(m map[string]string) any {
	pairs := [][]string{}
	for k, v := range m {
		pairs = append(pairs, []string{k, v})
	}
	return []any{"map", pairs}
}
func ovsUUID(s string) any { return []string{"uuid", s} }
func ovsSet(values ...any) any {
	if values == nil {
		values = []any{}
	}
	return []any{"set", values}
}
func table(columns string, rows ...[]any) []byte {
	if rows == nil {
		rows = [][]any{}
	}
	b, _ := json.Marshal(map[string]any{"headings": strings.Split(columns, ","), "data": rows})
	return b
}
func transitRow(id, owner string) []byte {
	return table("_uuid,name,external_ids", []any{ovsUUID(id), "ts", ovsMap(map[string]string{ownerKey: owner})})
}

type step struct {
	output []byte
	err    error
	check  func(*testing.T, []string)
}
type scripted struct {
	t     *testing.T
	steps []step
	calls [][]string
}

func (s *scripted) Run(ctx context.Context, command []string) ([]byte, error) {
	s.t.Helper()
	if _, ok := ctx.Deadline(); !ok {
		s.t.Fatal("command lacks deadline")
	}
	s.calls = append(s.calls, append([]string{}, command...))
	if len(s.steps) == 0 {
		s.t.Fatalf("unexpected command %q", command)
	}
	item := s.steps[0]
	s.steps = s.steps[1:]
	if item.check != nil {
		item.check(s.t, command)
	}
	return item.output, item.err
}
func script(t *testing.T, steps ...step) (*Exec, *scripted) {
	s := &scripted{t: t, steps: steps}
	t.Cleanup(func() {
		if len(s.steps) != 0 {
			t.Errorf("%d unconsumed command responses", len(s.steps))
		}
	})
	return &Exec{Runner: s}, s
}
func requireArgs(want ...string) func(*testing.T, []string) {
	return func(t *testing.T, got []string) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("command mismatch\ngot:  %q\nwant: %q", got, want)
		}
	}
}
func cfg() config.Cluster {
	return config.Cluster{NBCommand: []string{"ovn-nbctl"}, SBCommand: []string{"ovn-sbctl"}, AZName: "az-a", GatewayChassis: []string{"gw-a"}}
}

func TestCheckPrerequisitesNeverMutates(t *testing.T) {
	for _, tc := range []struct {
		name, az, interconn, remote string
		bad                         bool
	}{
		{"ready", "az-a", "true", "false", false}, {"wrong AZ", "az-b", "true", "false", true},
		{"not interconnection", "az-a", "false", "false", true}, {"remote chassis", "az-a", "true", "true", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := []step{{output: table("name", []any{tc.az})}}
			if tc.az == "az-a" {
				steps = append(steps, step{output: table("name,other_config", []any{"gw-a", ovsMap(map[string]string{"is-interconn": tc.interconn, "is-remote": tc.remote})})})
			}
			e, s := script(t, steps...)
			err := e.Check(context.Background(), cfg())
			if (err != nil) != tc.bad {
				t.Fatalf("error=%v, bad=%v", err, tc.bad)
			}
			for _, command := range s.calls {
				if !strings.Contains(strings.Join(command, " "), " find ") {
					t.Fatalf("unexpected mutation %v", command)
				}
			}
		})
	}
}

func TestMarkOwnershipAndIdempotency(t *testing.T) {
	for _, tc := range []struct {
		name       string
		marker     map[string]string
		write, bad bool
	}{
		{"new", map[string]string{"preserve": "native"}, true, false},
		{"replay", map[string]string{"interconn-ts": "ts"}, false, false},
		{"conflict", map[string]string{"interconn-ts": "other"}, false, true},
		{"empty marker is owned", map[string]string{"interconn-ts": ""}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := []step{{output: table("_uuid,name,other_config", []any{ovsUUID(id1), "ts", ovsMap(tc.marker)})}}
			if !tc.bad {
				marked := table("_uuid")
				if tc.marker["interconn-ts"] == "ts" {
					marked = table("_uuid", []any{ovsUUID(id1)})
				}
				steps = append(steps, step{output: marked})
			}
			if tc.write {
				steps = append(steps, step{check: requireArgs("ovn-nbctl", "--timeout=15", "get", "Logical_Switch", id1, "name", "other_config", "--", "wait-until", "Logical_Switch", id1, `name="ts"`, "other_config:interconn-ts{=}[]", "--", "set", "Logical_Switch", id1, `other_config:interconn-ts="ts"`)})
			}
			e, _ := script(t, steps...)
			if err := e.Mark(context.Background(), cfg(), "ts"); (err != nil) != tc.bad {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestMarkRejectsDuplicateMarkerOnDifferentSwitch(t *testing.T) {
	e, _ := script(t, step{output: table("_uuid,name,other_config", []any{ovsUUID(id1), "ts", ovsMap(nil)})}, step{output: table("_uuid", []any{ovsUUID(id2)})})
	if err := e.Mark(context.Background(), cfg(), "ts"); err == nil {
		t.Fatal("accepted duplicate marker")
	}
}

func TestRoutesReadyRequiresNativeDestinationRoutes(t *testing.T) {
	for _, tc := range []struct {
		name, cidr, next, routeTable string
		policy                       any
		ready                        bool
	}{
		{"ready", "10.1.0.0/24", "10.255.0.2", "", "dst-ip", true},
		{"default destination", "10.1.0.0/24", "10.255.0.2", "", ovsSet(), true},
		{"wrong prefix", "10.2.0.0/24", "10.255.0.2", "", "dst-ip", false},
		{"wrong hop", "10.1.0.0/24", "10.255.0.3", "", "dst-ip", false},
		{"source policy", "10.1.0.0/24", "10.255.0.2", "", "src-ip", false},
		{"other table", "10.1.0.0/24", "10.255.0.2", "other", "dst-ip", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := script(t, step{output: table("name,static_routes", []any{"vpc", ovsUUID(id1)})}, step{output: table("ip_prefix,nexthop,policy,route_table", []any{tc.cidr, tc.next, tc.policy, tc.routeTable})})
			ready, err := e.RoutesReady(context.Background(), cfg(), "vpc", []Route{{CIDR: "10.1.0.0/24", NextHopIP: "10.255.0.2"}})
			if err != nil || ready != tc.ready {
				t.Fatalf("ready=%v err=%v", ready, err)
			}
		})
	}
	for _, refs := range []any{ovsSet(), ovsSet(ovsUUID(id1), ovsUUID(id2))} {
		e, _ := script(t, step{output: table("name,static_routes", []any{"vpc", refs})})
		ready, err := e.RoutesReady(context.Background(), cfg(), "vpc", []Route{{CIDR: "10.1.0.0/24", NextHopIP: "10.255.0.2"}})
		if err != nil || ready {
			t.Fatalf("missing/extra route accepted: ready=%v err=%v", ready, err)
		}
	}
}

// Kube-OVN v1.16.4 installs source routes for both attached subnets in addition
// to the explicit destination default route. Readiness must recognize precisely
// these routes without accepting arbitrary additional source-policy entries.
func TestRoutesReadyAcceptsExactKubeOVNLocalSourceRouteSet(t *testing.T) {
	want := []Route{
		{CIDR: "10.253.10.0/30", NextHopIP: "10.253.10.1", Policy: "src-ip"},
		{CIDR: "0.0.0.0/0", NextHopIP: "10.253.10.2"},
		{CIDR: "10.252.10.0/27", NextHopIP: "10.252.10.1", Policy: "src-ip"},
	}
	for _, tc := range []struct {
		name, cidr, nextHop, routeTable string
		policy                          any
		ready                           bool
	}{
		{"exact local routes", "10.253.10.0/30", "10.253.10.1", "", "src-ip", true},
		{"source policy encoded as set", "10.253.10.0/30", "10.253.10.1", "", ovsSet("src-ip"), true},
		{"unexpected source prefix", "10.253.11.0/30", "10.253.10.1", "", "src-ip", false},
		{"wrong source next hop", "10.253.10.0/30", "10.253.10.2", "", "src-ip", false},
		{"destination policy is not source policy", "10.253.10.0/30", "10.253.10.1", "", "dst-ip", false},
		{"omitted policy means destination", "10.253.10.0/30", "10.253.10.1", "", ovsSet(), false},
		{"source route in another table", "10.253.10.0/30", "10.253.10.1", "tenant-other", "src-ip", false},
		{"unknown source policy", "10.253.10.0/30", "10.253.10.1", "", "unexpected", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, s := script(t,
				step{output: table("name,static_routes", []any{"vpc", ovsSet(ovsUUID(id1), ovsUUID(id2), ovsUUID(id3))})},
				step{output: table("ip_prefix,nexthop,policy,route_table", []any{"0.0.0.0/0", "10.253.10.2", ovsSet(), ""}), check: requireArgs("ovn-nbctl", "--timeout=15", "--format=json", "--columns=ip_prefix,nexthop,policy,route_table", "list", "Logical_Router_Static_Route", id1)},
				step{output: table("ip_prefix,nexthop,policy,route_table", []any{"10.252.10.0/27", "10.252.10.1", "src-ip", ""}), check: requireArgs("ovn-nbctl", "--timeout=15", "--format=json", "--columns=ip_prefix,nexthop,policy,route_table", "list", "Logical_Router_Static_Route", id2)},
				step{output: table("ip_prefix,nexthop,policy,route_table", []any{tc.cidr, tc.nextHop, tc.policy, tc.routeTable}), check: requireArgs("ovn-nbctl", "--timeout=15", "--format=json", "--columns=ip_prefix,nexthop,policy,route_table", "list", "Logical_Router_Static_Route", id3)},
			)
			ready, err := e.RoutesReady(context.Background(), cfg(), "vpc", want)
			if err != nil || ready != tc.ready {
				t.Fatalf("ready=%v, want=%v, err=%v", ready, tc.ready, err)
			}
			for _, argv := range s.calls {
				joined := strings.Join(argv, " ")
				if !strings.Contains(joined, " find ") && !strings.Contains(joined, " list ") {
					t.Fatal("route observation mutated OVN")
				}
			}
		})
	}
	for _, refs := range []any{ovsSet(ovsUUID(id1), ovsUUID(id2)), ovsSet(ovsUUID(id1), ovsUUID(id2), ovsUUID(id3), ovsUUID("44444444-4444-4444-4444-444444444444"))} {
		e, _ := script(t, step{output: table("name,static_routes", []any{"vpc", refs})})
		if ready, err := e.RoutesReady(context.Background(), cfg(), "vpc", want); err != nil || ready {
			t.Fatalf("missing/additional route accepted: ready=%v, err=%v", ready, err)
		}
	}
}

func TestGatewaySetOnlyOwnsPortReferences(t *testing.T) {
	c := cfg()
	c.GatewayChassis = []string{"gw-a", "gw-b"}
	e, _ := script(t,
		step{output: table("_uuid,name,gateway_chassis", []any{ovsUUID(id1), "a-ts", ovsUUID(id2)})},
		step{output: table("chassis_name,priority", []any{"obsolete", 100})},
		step{check: requireArgs("ovn-nbctl", "--timeout=15", "get", "Logical_Router_Port", id1, "name", "gateway_chassis", "--", "wait-until", "Logical_Router_Port", id1, `name="a-ts"`, "gateway_chassis=["+id2+"]", "--", "clear", "Logical_Router_Port", id1, "gateway_chassis",
			"--", "--id=@gw0", "create", "Gateway_Chassis", `name="a-ts-gw-a"`, `chassis_name="gw-a"`, "priority=100", "--", "add", "Logical_Router_Port", id1, "gateway_chassis", "@gw0",
			"--", "--id=@gw1", "create", "Gateway_Chassis", `name="a-ts-gw-b"`, `chassis_name="gw-b"`, "priority=99", "--", "add", "Logical_Router_Port", id1, "gateway_chassis", "@gw1")},
	)
	if err := e.Gateways(context.Background(), c, "a-ts"); err != nil {
		t.Fatal(err)
	}
}
func TestGatewayReplayMakesNoWrite(t *testing.T) {
	e, _ := script(t, step{output: table("_uuid,name,gateway_chassis", []any{ovsUUID(id1), "a-ts", ovsUUID(id2)})}, step{output: table("chassis_name,priority", []any{"gw-a", 100}), check: requireArgs("ovn-nbctl", "--timeout=15", "--format=json", "--columns=chassis_name,priority", "list", "Gateway_Chassis", id2)})
	if err := e.Gateways(context.Background(), cfg(), "a-ts"); err != nil {
		t.Fatal(err)
	}
}

func TestTransitCreationIsAtomicAndRecoversLostResponse(t *testing.T) {
	e, _ := script(t,
		step{output: table("_uuid,name,external_ids")},
		step{err: errors.New("connection failed secret=must-not-leak"), check: requireArgs("ovn-ic-nbctl", "--timeout=15", "--id=@ts", "create", "Transit_Switch", `name="ts"`, `external_ids:global-vpc-uid="owner"`)},
		step{output: transitRow(id1, "owner")},
		step{output: transitRow(id1, "owner")},
	)
	id, err := e.EnsureTransit(context.Background(), []string{"ovn-ic-nbctl"}, "ts", "owner", "")
	if err != nil || id != id1 {
		t.Fatalf("id=%s err=%v", id, err)
	}
	if _, err = e.EnsureTransit(context.Background(), []string{"ovn-ic-nbctl"}, "ts", "owner", id1); err != nil {
		t.Fatal(err)
	}
}

func TestTransitRefusesAdoptionReplacementAndDuplicates(t *testing.T) {
	for _, tc := range []struct {
		name     string
		output   []byte
		expected string
		missing  bool
	}{
		{"foreign owner", transitRow(id1, "foreign"), "", false},
		{"unowned", transitRow(id1, ""), "", false},
		{"replacement", transitRow(id2, "owner"), id1, false},
		{"missing", table("_uuid,name,external_ids"), id1, true},
		{"duplicates", table("_uuid,name,external_ids", []any{ovsUUID(id1), "ts", ovsMap(map[string]string{ownerKey: "owner"})}, []any{ovsUUID(id2), "ts", ovsMap(map[string]string{ownerKey: "owner"})}), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := script(t, step{output: tc.output})
			_, err := e.EnsureTransit(context.Background(), []string{"ic"}, "ts", "owner", tc.expected)
			if err == nil {
				t.Fatal("unsafe identity accepted")
			}
			if tc.missing && !errors.Is(err, ErrTransitMissing) {
				t.Fatalf("missing sentinel: %v", err)
			}
		})
	}
}

func TestInspectTransitIsReadOnlyAndAllowsRecordedAbsence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output []byte
		want   string
		bad    bool
	}{
		{"recorded row", transitRow(id1, "owner"), id1, false},
		{"recorded absence", table("_uuid,name,external_ids"), "", false},
		{"replaced row", transitRow(id2, "owner"), "", true},
		{"foreign owner", transitRow(id1, "other"), "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := script(t, step{output: tc.output, check: requireArgs("ic", "--timeout=15", "--format=json", "--columns=_uuid,name,external_ids", "find", "Transit_Switch", `name="ts"`)})
			got, err := e.InspectTransit(context.Background(), []string{"ic"}, "ts", "owner", id1)
			if got != tc.want || (err != nil) != tc.bad {
				t.Fatalf("got=%q err=%v", got, err)
			}
		})
	}
}

func TestDeleteTransitUsesVerifiedUUIDAndOwnerGuard(t *testing.T) {
	e, _ := script(t, step{output: transitRow(id1, "owner")}, step{check: requireArgs("ic", "--timeout=15", "get", "Transit_Switch", id1, "name", "external_ids", "--", "wait-until", "Transit_Switch", id1, `name="ts"`, `external_ids:global-vpc-uid="owner"`, "--", "destroy", "Transit_Switch", id1)}, step{output: table("_uuid,name,external_ids")})
	for range 2 {
		if err := e.DeleteTransit(context.Background(), []string{"ic"}, "ts", "owner", id1); err != nil {
			t.Fatal(err)
		}
	}
}
func TestDeleteTransitRefusesReplacement(t *testing.T) {
	e, _ := script(t, step{output: transitRow(id2, "owner")})
	if err := e.DeleteTransit(context.Background(), []string{"ic"}, "ts", "owner", id1); err == nil {
		t.Fatal("replacement deleted")
	}
}
func TestDeleteMutationFailureIsNotSuccess(t *testing.T) {
	e, _ := script(t, step{output: transitRow(id1, "owner")}, step{err: errors.New("guard failed auth-token-secret")})
	err := e.DeleteTransit(context.Background(), []string{"ic"}, "ts", "owner", id1)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func switchRow(refs any, marked bool) []byte {
	other := map[string]string{}
	if marked {
		other = map[string]string{"interconn-ts": "ts", "requested-tnl-key": "16711683"}
	}
	return table("_uuid,name,ports,other_config", []any{ovsUUID(id1), "ts", refs, ovsMap(other)})
}
func portRow(name, typ string, options map[string]string, addresses any) []byte {
	return table("name,type,options,addresses", []any{name, typ, ovsMap(options), addresses})
}
func TestSwitchPortUUIDLookupUsesListRecordSyntax(t *testing.T) {
	e, _ := script(t,
		step{output: switchRow(ovsUUID(id2), false)},
		step{output: portRow("port", "router", nil, ovsSet()), check: requireArgs("ovn-nbctl", "--timeout=15", "--format=json", "--columns=name,type,options,addresses", "list", "Logical_Switch_Port", id2)},
	)
	if empty, err := e.EndpointsEmpty(context.Background(), cfg(), "ts"); err != nil || !empty {
		t.Fatalf("empty=%v, err=%v", empty, err)
	}
}

func TestEndpointCheckFailsClosed(t *testing.T) {
	for _, typ := range []string{"", "external", "localnet", "unknown", "router", "remote"} {
		t.Run("type_"+typ, func(t *testing.T) {
			e, _ := script(t, step{output: switchRow(ovsUUID(id2), false)}, step{output: portRow("port", typ, nil, ovsSet())})
			empty, err := e.EndpointsEmpty(context.Background(), cfg(), "ts")
			if err != nil {
				t.Fatal(err)
			}
			if empty != (typ == "router" || typ == "remote") {
				t.Fatalf("unsafe empty=%v", empty)
			}
		})
	}
	e, _ := script(t, step{output: table("_uuid,name,ports,other_config")})
	empty, err := e.EndpointsEmpty(context.Background(), cfg(), "ts")
	if !empty || err != nil {
		t.Fatalf("absent switch: %v %v", empty, err)
	}
}

func TestConnectedRequiresNativeRemotePortAndKeys(t *testing.T) {
	for _, tc := range []struct {
		name, remoteName, remoteType, key string
		addresses                         any
		ready                             bool
	}{
		{"ready", "ts-b", "remote", "12", "00:00:00:00:00:02 10.0.0.2", true},
		{"wrong remote identity", "ts-c", "remote", "12", "00:00:00:00:00:02 10.0.0.2", false},
		{"endpoint cannot stand in for remote", "ts-b", "", "12", "00:00:00:00:00:02 10.0.0.2", false},
		{"key absent", "ts-b", "remote", "", "00:00:00:00:00:02 10.0.0.2", false},
		{"address absent", "ts-b", "remote", "12", ovsSet(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := script(t, step{output: switchRow(ovsSet(ovsUUID(id2), ovsUUID(id3)), true)},
				step{output: portRow("ts-a", "router", map[string]string{"router-port": "a-ts", "requested-tnl-key": "11"}, "router")},
				step{output: portRow(tc.remoteName, tc.remoteType, map[string]string{"requested-tnl-key": tc.key}, tc.addresses)})
			ready, err := e.Connected(context.Background(), cfg(), "ts", "a-ts", []string{"b-ts"})
			if err != nil || ready != tc.ready {
				t.Fatalf("ready=%v err=%v", ready, err)
			}
		})
	}
}

func TestLocalAbsentChecksAllNativeNames(t *testing.T) {
	steps := []step{}
	for _, item := range [][2]string{{"Logical_Router", "vpc"}, {"Logical_Switch", "subnet"}, {"Logical_Switch", "ts"}, {"Logical_Router_Port", "vpc-subnet"}, {"Logical_Router_Port", "vpc-ts"}, {"Logical_Switch_Port", "subnet-vpc"}, {"Logical_Switch_Port", "ts-vpc"}} {
		steps = append(steps, step{output: table("_uuid"), check: requireArgs("ovn-nbctl", "--timeout=15", "--format=json", "--columns=_uuid", "find", item[0], condition("name", item[1]))})
	}
	e, _ := script(t, steps...)
	absent, err := e.LocalAbsent(context.Background(), cfg(), "vpc", "subnet", "ts")
	if err != nil || !absent {
		t.Fatalf("absent=%v err=%v", absent, err)
	}
}

func TestMalformedResponsesFailClosed(t *testing.T) {
	for _, s := range []string{
		`{`, `{"headings":["name"],"data":null}`, `{"headings":["name"],"data":[[]]}`,
		`{"headings":["wrong"],"data":[["az-a"]]}`, `{"headings":["name"],"data":[[4]]}`,
		`{"headings":["name"],"data":[["az-a"]]} {}`,
	} {
		t.Run(s, func(t *testing.T) {
			e, _ := script(t, step{output: []byte(s)})
			if err := e.Check(context.Background(), cfg()); err == nil {
				t.Fatal("accepted malformed response")
			}
		})
	}
	for _, s := range []string{`null`, `["map",null]`, `["map",[["x","y"],["x","z"]]]`, `["map",[["x",7]]]`} {
		if _, err := mapValue(json.RawMessage(s)); err == nil {
			t.Fatalf("accepted malformed map %s", s)
		}
	}
	for _, s := range []string{`null`, `["uuid","not-a-uuid"]`, `["set",[["uuid","not-a-uuid"]]]`} {
		if _, err := setValue(json.RawMessage(s), true); err == nil {
			t.Fatalf("accepted malformed set %s", s)
		}
	}
}

func TestArgumentsRemainSeparateAndErrorsSanitized(t *testing.T) {
	prefix := []string{"kubectl", "--kubeconfig=/credential/path", "exec", "pod", "--", "ovn-ic-nbctl"}
	name := `ts";$(leak)`
	e, _ := script(t, step{err: errors.New("sensitive stderr /credential/path"), check: func(t *testing.T, args []string) {
		if !reflect.DeepEqual(args[:len(prefix)], prefix) || args[len(args)-1] != condition("name", name) {
			t.Fatalf("unsafe argument rewriting: %q", args)
		}
	}})
	_, err := e.EnsureTransit(context.Background(), prefix, name, "owner", "")
	if err == nil || err.Error() != "OVN command failed" {
		t.Fatalf("error leaks details: %v", err)
	}
}

func TestProcessRunnerBoundsOutputAndHonorsCancellation(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GVPC_OVN_TEST_CHILD", "output")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = (processRunner{}).Run(ctx, []string{self, "-test.run=^TestOVNHelperProcess$", "--"})
	if err == nil || !strings.Contains(err.Error(), "output limit") {
		t.Fatalf("unbounded output: %v", err)
	}
	t.Setenv("GVPC_OVN_TEST_CHILD", "wait")
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel2()
	_, err = (processRunner{}).Run(ctx2, []string{self, "-test.run=^TestOVNHelperProcess$", "--"})
	if err == nil || ctx2.Err() == nil {
		t.Fatalf("cancellation ignored: %v", err)
	}
}
func TestOVNHelperProcess(t *testing.T) {
	switch os.Getenv("GVPC_OVN_TEST_CHILD") {
	case "output":
		_, _ = io.Copy(os.Stdout, strings.NewReader(strings.Repeat("x", maxOutput+65536)))
		os.Exit(0)
	case "wait":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
}

func TestBoundedBufferCannotBypassLimitViaReaderFrom(t *testing.T) {
	b := &boundedBuffer{limit: 8}
	_, _ = io.Copy(b, io.LimitReader(strings.NewReader(strings.Repeat("x", 32)), 32))
	if b.buffer.Len() != 8 || !b.overflow {
		t.Fatalf("overflow guard bypass: len=%d overflow=%v", b.buffer.Len(), b.overflow)
	}
}
