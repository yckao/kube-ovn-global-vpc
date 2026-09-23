package ovn

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	api "globalvpc.io/controller/api/v1alpha1"
	"globalvpc.io/controller/internal/config"
	"globalvpc.io/controller/internal/siteconfig"
	"globalvpc.io/controller/internal/siteplan"
)

const bfdPortID = "44444444-4444-4444-4444-444444444444"
const bfdGroupID = "55555555-5555-5555-5555-555555555555"
const bfdChassis1 = "66666666-6666-6666-6666-666666666666"
const bfdChassis2 = "77777777-7777-7777-7777-777777777777"

func bfdFixture() (config.Cluster, siteplan.GatewayConfig, []api.BFDResourceRecord) {
	c := cfg()
	c.UID = "cluster-a"
	g := siteplan.GatewayConfig{Version: "v2", OwnerUID: "local-owner", VpcName: "vpc-a", Gateways: []siteconfig.Gateway{{ID: "gw1", IP: "10.253.30.2"}, {ID: "gw2", IP: "10.253.30.3"}}, BFD: &siteconfig.BFD{SourceIP: "10.254.120.1", MinRX: 300, MinTX: 300, Multiplier: 3}}
	return c, g, []api.BFDResourceRecord{{GatewayID: "gw1", UUID: id1}, {GatewayID: "gw2", UUID: id2}}
}

func bfdRow(id, member, ip string) []any {
	return []any{ovsUUID(id), "bfd@vpc-a", ip, ovsMap(map[string]string{ownerKey: "local-owner", bfdGatewayKey: member, bfdClusterKey: "cluster-a", bfdRouterKey: "vpc-a"}), 300, 300, 3, "up"}
}

func bfdRows() [][]any {
	return [][]any{bfdRow(id1, "gw1", "10.253.30.2"), bfdRow(id2, "gw2", "10.253.30.3")}
}

func bfdRead(rows ...[]any) step {
	return step{output: table(bfdColumns, rows...), check: requireArgs("ovn-nbctl", "--timeout=15", "--format=json", "--columns="+bfdColumns, "find", "BFD")}
}

func bfdNativePortSteps() []step {
	return []step{
		{output: table("_uuid,name,networks,options,ha_chassis_group", []any{ovsUUID(bfdPortID), "bfd@vpc-a", "10.254.120.1", ovsMap(map[string]string{"bfd-only": "true"}), ovsUUID(bfdGroupID)}), check: requireArgs("ovn-nbctl", "--timeout=15", "--format=json", "--columns=_uuid,name,networks,options,ha_chassis_group", "find", "Logical_Router_Port", `name="bfd@vpc-a"`)},
		{output: table("name,ports", []any{"vpc-a", ovsSet(ovsUUID(bfdPortID))})},
		{output: table("ha_chassis", []any{ovsSet(ovsUUID(bfdChassis1), ovsUUID(bfdChassis2))}), check: requireArgs("ovn-nbctl", "--timeout=15", "--format=json", "--columns=ha_chassis", "list", "HA_Chassis_Group", bfdGroupID)},
		{output: table("chassis_name", []any{"infra-a"}), check: requireArgs("ovn-nbctl", "--timeout=15", "--format=json", "--columns=chassis_name", "list", "HA_Chassis", bfdChassis1)},
		{output: table("chassis_name", []any{"infra-b"}), check: requireArgs("ovn-nbctl", "--timeout=15", "--format=json", "--columns=chassis_name", "list", "HA_Chassis", bfdChassis2)},
	}
}

func bfdSetCheck(id, member, ip string) func(*testing.T, []string) {
	return requireArgs("ovn-nbctl", "--timeout=15", "get", "BFD", id, "external_ids", "logical_port", "dst_ip", "--", "wait-until", "BFD", id, `external_ids:global-vpc-uid="local-owner"`, `external_ids:global-vpc-gateway-id="`+member+`"`, `external_ids:global-vpc-cluster-uid="cluster-a"`, `external_ids:global-vpc-router="vpc-a"`, `logical_port="bfd@vpc-a"`, `dst_ip="`+ip+`"`, "--", "set", "BFD", id, "min_rx=300", "min_tx=300", "detect_mult=3")
}

func TestGetBFDPreservesIdentityAndSeparatesTransientHealth(t *testing.T) {
	c, g, records := bfdFixture()
	rows := bfdRows()
	rows[1][7] = "down"
	foreign := bfdRow(id3, "foreign", "192.0.2.2")
	foreign[1], foreign[3] = "bfd@other", ovsMap(map[string]string{ownerKey: "someone-else"})
	e, _ := script(t, bfdRead(rows[1], foreign, rows[0]))
	state, err := e.GetBFD(context.Background(), c, g, records)
	if err != nil || !state.Configured || !reflect.DeepEqual(state.Records, records) || !reflect.DeepEqual(state.Up, []string{"gw1"}) {
		t.Fatalf("wrong BFD snapshot: %#v, %v", state, err)
	}
}

func TestGetBFDRejectsConflictingOrMalformedRows(t *testing.T) {
	c, g, records := bfdFixture()
	for _, tc := range []struct {
		name      string
		edit      func([]any)
		duplicate bool
	}{
		{"wrong owner", func(r []any) { r[3] = ovsMap(map[string]string{ownerKey: "foreign"}) }, false},
		{"replacement UUID", func(r []any) { r[0] = ovsUUID(id3) }, false},
		{"wrong port", func(r []any) { r[1] = "bfd@other" }, false},
		{"wrong IP", func(r []any) { r[2] = "10.253.30.4" }, false},
		{"wrong member", func(r []any) {
			r[3] = ovsMap(map[string]string{ownerKey: "local-owner", bfdGatewayKey: "gw3", bfdClusterKey: "cluster-a", bfdRouterKey: "vpc-a"})
		}, false},
		{"wrong cluster", func(r []any) {
			r[3] = ovsMap(map[string]string{ownerKey: "local-owner", bfdGatewayKey: "gw1", bfdClusterKey: "cluster-b", bfdRouterKey: "vpc-a"})
		}, false},
		{"wrong router", func(r []any) {
			r[3] = ovsMap(map[string]string{ownerKey: "local-owner", bfdGatewayKey: "gw1", bfdClusterKey: "cluster-a", bfdRouterKey: "vpc-b"})
		}, false},
		{"duplicate member", func(r []any) {}, true},
		{"unknown status", func(r []any) { r[7] = "maybe" }, false},
		{"malformed timer", func(r []any) { r[4] = "300" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := bfdRows()
			tc.edit(rows[0])
			if tc.duplicate {
				rows = append(rows, rows[0])
			}
			e, _ := script(t, bfdRead(rows...))
			if _, err := e.GetBFD(context.Background(), c, g, records); err == nil {
				t.Fatal("unsafe row accepted")
			}
		})
	}
}

func TestBFDReceiptValidationPrecedesCLI(t *testing.T) {
	c, g, records := bfdFixture()
	for _, invalid := range [][]api.BFDResourceRecord{
		{{GatewayID: "unknown", UUID: id1}}, {{GatewayID: "gw1", UUID: "not-uuid"}}, {records[0], records[0]},
	} {
		e, _ := script(t)
		if _, err := e.GetBFD(context.Background(), c, g, invalid); err == nil {
			t.Fatal("invalid receipt accepted")
		}
	}
}

func TestEnsureBFDRequiresNativeBFDPortAndRedundantChassis(t *testing.T) {
	c, g, _ := bfdFixture()
	for _, tc := range []struct {
		name    string
		stage   int
		output  []byte
		pending bool
	}{
		{"port missing", 0, table("_uuid,name,networks,options,ha_chassis_group"), true},
		{"wrong source", 0, table("_uuid,name,networks,options,ha_chassis_group", []any{ovsUUID(bfdPortID), "bfd@vpc-a", "10.254.120.2", ovsMap(map[string]string{"bfd-only": "true"}), ovsUUID(bfdGroupID)}), true},
		{"ordinary port", 0, table("_uuid,name,networks,options,ha_chassis_group", []any{ovsUUID(bfdPortID), "bfd@vpc-a", "10.254.120.1", ovsMap(map[string]string{}), ovsUUID(bfdGroupID)}), true},
		{"router missing", 1, table("name,ports"), true},
		{"wrong router membership", 1, table("name,ports", []any{"vpc-a", ovsUUID(id3)}), false},
		{"single chassis", 2, table("ha_chassis", []any{ovsUUID(bfdChassis1)}), true},
		{"duplicate chassis names", 4, table("chassis_name", []any{"infra-a"}), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := bfdNativePortSteps()[:tc.stage+1]
			steps[tc.stage].output = tc.output
			e, _ := script(t, append([]step{bfdRead()}, steps...)...)
			_, err := e.EnsureBFD(context.Background(), c, g, nil)
			if err == nil || (tc.pending && !errors.Is(err, ErrBFDPortPending)) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestEnsureBFDDoesNotReplaceRecordedMissingRow(t *testing.T) {
	c, g, records := bfdFixture()
	steps := append([]step{bfdRead(bfdRows()[1])}, bfdNativePortSteps()...)
	e, _ := script(t, steps...)
	if _, err := e.EnsureBFD(context.Background(), c, g, records); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing receipt recreated: %v", err)
	}
}

func TestEnsureBFDRecoversCommittedWriteAfterLostResponse(t *testing.T) {
	c, g, _ := bfdFixture()
	steps := append([]step{bfdRead()}, bfdNativePortSteps()...)
	steps = append(steps, step{err: errors.New("response lost"), check: requireArgs("ovn-nbctl", "--timeout=15", "create", "BFD", `logical_port="bfd@vpc-a"`, `dst_ip="10.253.30.2"`, `external_ids:global-vpc-uid="local-owner"`, `external_ids:global-vpc-gateway-id="gw1"`, `external_ids:global-vpc-cluster-uid="cluster-a"`, `external_ids:global-vpc-router="vpc-a"`, "min_rx=300", "min_tx=300", "detect_mult=3")}, bfdRead(bfdRows()[0]))
	e, s := script(t, steps...)
	if _, err := e.EnsureBFD(context.Background(), c, g, nil); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("write ambiguity hidden: %v", err)
	}
	state, err := e.GetBFD(context.Background(), c, g, nil)
	if err != nil || state.Configured || len(state.Records) != 1 || state.Records[0].UUID != id1 {
		t.Fatalf("committed row not rediscovered: %#v %v", state, err)
	}
	creates := 0
	for _, cmd := range s.calls {
		if len(cmd) > 2 && cmd[2] == "create" {
			creates++
		}
	}
	if creates != 1 {
		t.Fatal("recovery performed duplicate create")
	}
}

func TestEnsureBFDRepairsTimersWithIdentityGuard(t *testing.T) {
	c, g, records := bfdFixture()
	rows := bfdRows()
	rows[0][4] = 1000
	steps := append([]step{bfdRead(rows...)}, bfdNativePortSteps()...)
	steps = append(steps, step{check: bfdSetCheck(id1, "gw1", "10.253.30.2")}, step{check: bfdSetCheck(id2, "gw2", "10.253.30.3")}, bfdRead(bfdRows()...))
	e, _ := script(t, steps...)
	state, err := e.EnsureBFD(context.Background(), c, g, records)
	if err != nil || !state.Configured || len(state.Up) != 2 {
		t.Fatalf("timer repair did not converge: %#v %v", state, err)
	}
}

func TestDeleteBFDPreservesReferencedAndForeignRows(t *testing.T) {
	c, g, records := bfdFixture()
	for _, kind := range []string{"static", "policy"} {
		t.Run(kind, func(t *testing.T) {
			static, policy := ovsSet(), ovsSet()
			if kind == "static" {
				static = ovsUUID(id1)
			} else {
				policy = ovsUUID(id2)
			}
			e, _ := script(t, bfdRead(bfdRows()...), step{output: table("bfd", []any{static})}, step{output: table("bfd_sessions", []any{policy})})
			done, err := e.DeleteBFD(context.Background(), c, g, records)
			if err != nil || done {
				t.Fatalf("deleted referenced row: done=%v err=%v", done, err)
			}
		})
	}
	foreign := bfdRow(id3, "other", "192.0.2.5")
	foreign[1], foreign[3] = "bfd@other", ovsMap(map[string]string{ownerKey: "other"})
	steps := []step{bfdRead(append(bfdRows(), foreign)...), {output: table("bfd", []any{ovsUUID(id3)})}, {output: table("bfd_sessions")}}
	steps = append(steps, step{output: successfulBFDDeleteResponse(2), check: checkAtomicBFDDelete(c, g, records)})
	steps = append(steps, bfdRead(foreign))
	e, _ := script(t, steps...)
	e.TransactionCommand = bfdTransactionCommand()
	done, err := e.DeleteBFD(context.Background(), c, g, records)
	if err != nil || done {
		t.Fatalf("delete must wait for independent absence: %v %v", done, err)
	}
	done, err = e.DeleteBFD(context.Background(), c, g, records)
	if err != nil || !done {
		t.Fatalf("foreign BFD row blocked owned absence: %v %v", done, err)
	}
}

func bfdTransactionCommand() []string {
	return []string{"ovsdb-client", "--timeout=15", "transact", "unix:/local/nb.sock"}
}

func successfulBFDDeleteResponse(members int) []byte {
	responses := []any{}
	for i := 0; i < members; i++ {
		responses = append(responses, map[string]any{}, map[string]any{}, map[string]any{}, map[string]any{"count": 1})
	}
	encoded, _ := json.Marshal(responses)
	return encoded
}

// Check the server-side safety properties independently of the adapter's
// mutation helpers: each owned UUID is pinned and both weak-reference tables
// must be empty in the same transaction that deletes the BFD row.
func checkAtomicBFDDelete(c config.Cluster, g siteplan.GatewayConfig, records []api.BFDResourceRecord) func(*testing.T, []string) {
	return func(t *testing.T, cmd []string) {
		t.Helper()
		prefix := bfdTransactionCommand()
		if len(cmd) != len(prefix)+1 || !reflect.DeepEqual(cmd[:len(prefix)], prefix) {
			t.Fatalf("expected one raw ovsdb-client transaction: %q", cmd)
		}
		var transaction []json.RawMessage
		if err := json.Unmarshal([]byte(cmd[len(prefix)]), &transaction); err != nil || len(transaction) != 1+4*len(records) || string(transaction[0]) != `"OVN_Northbound"` {
			t.Fatalf("invalid atomic BFD transaction shape: %s", cmd[len(prefix)])
		}
		assertJSON := func(raw json.RawMessage, expected any) {
			t.Helper()
			var got, want any
			encoded, _ := json.Marshal(expected)
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encoded, &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("transaction does not enforce the required guard\ngot: %s\nwant: %s", raw, encoded)
			}
		}
		for i, record := range records {
			ip := ""
			for _, member := range g.Gateways {
				if member.ID == record.GatewayID {
					ip = member.IP
				}
			}
			id := []string{"uuid", record.UUID}
			where := []any{
				[]any{"_uuid", "==", id},
				[]any{"logical_port", "==", g.BFDPortName()},
				[]any{"dst_ip", "==", ip},
				[]any{"external_ids", "includes", []any{"map", [][]string{{"global-vpc-uid", g.OwnerUID}, {"global-vpc-gateway-id", record.GatewayID}, {"global-vpc-cluster-uid", c.UID}, {"global-vpc-router", g.VpcName}}}},
			}
			assertJSON(transaction[1+4*i], map[string]any{"op": "wait", "table": "BFD", "where": where, "columns": []string{"_uuid"}, "until": "==", "rows": []any{map[string]any{"_uuid": id}}, "timeout": 0})
			for j, ref := range [][2]string{{"Logical_Router_Static_Route", "bfd"}, {"Logical_Router_Policy", "bfd_sessions"}} {
				assertJSON(transaction[2+4*i+j], map[string]any{"op": "wait", "table": ref[0], "where": []any{[]any{ref[1], "includes", []any{"set", []any{id}}}}, "columns": []string{"_uuid"}, "until": "==", "rows": []any{}, "timeout": 0})
			}
			assertJSON(transaction[4+4*i], map[string]any{"op": "delete", "table": "BFD", "where": where})
		}
	}
}

func TestDeleteBFDRejectsUnconfirmedAtomicTransactionResults(t *testing.T) {
	c, g, records := bfdFixture()
	valid := successfulBFDDeleteResponse(2)
	for _, tc := range []struct {
		name   string
		output []byte
		err    error
	}{
		{"ownership or reference wait failed", []byte(`[{},{"error":"timed out"},null,null,null,null,null,null]`), nil},
		{"zero deleted rows", []byte(`[{},{},{},{"count":0},{},{},{},{"count":1}]`), nil},
		{"too many deleted rows", []byte(`[{},{},{},{"count":2},{},{},{},{"count":1}]`), nil},
		{"second deletion not confirmed", []byte(`[{},{},{},{"count":1},{},{},{},{}]`), nil},
		{"aborted operation", []byte(`[{},{},{},null,{},{},{},{"count":1}]`), nil},
		{"wrong response count", []byte(`[{},{},{},{"count":1}]`), nil},
		{"trailing JSON", append(append([]byte(nil), valid...), []byte(` {}`)...), nil},
		{"trailing invalid bytes", append(append([]byte(nil), valid...), []byte(` broken`)...), nil},
		{"malformed response", []byte(`{`), nil},
		{"null response", []byte(`null`), nil},
		{"lost response", nil, errors.New("connection closed after commit")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expected := append([]api.BFDResourceRecord(nil), records...)
			e, _ := script(t, bfdRead(bfdRows()...), step{output: table("bfd")}, step{output: table("bfd_sessions")}, step{output: tc.output, err: tc.err, check: checkAtomicBFDDelete(c, g, records)})
			e.TransactionCommand = bfdTransactionCommand()
			done, err := e.DeleteBFD(context.Background(), c, g, records)
			if err == nil || done || !reflect.DeepEqual(records, expected) {
				t.Fatalf("unconfirmed delete discarded durable identity: done=%v err=%v records=%v", done, err, records)
			}
		})
	}
}

func TestDeleteBFDAmbiguousCommitRecoversFromIndependentAbsence(t *testing.T) {
	c, g, records := bfdFixture()
	e, s := script(t, bfdRead(bfdRows()...), step{output: table("bfd")}, step{output: table("bfd_sessions")}, step{err: errors.New("lost committed response"), check: checkAtomicBFDDelete(c, g, records)}, bfdRead())
	e.TransactionCommand = bfdTransactionCommand()
	if done, err := e.DeleteBFD(context.Background(), c, g, records); err == nil || done || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("ambiguous commit reported as success: %v %v", done, err)
	}
	if done, err := e.DeleteBFD(context.Background(), c, g, records); err != nil || !done {
		t.Fatalf("independent absence did not complete deletion: %v %v", done, err)
	}
	transactions := 0
	for _, cmd := range s.calls {
		if cmd[0] == "ovsdb-client" {
			transactions++
		}
	}
	if transactions != 1 {
		t.Fatal("absence recovery issued another delete transaction")
	}
}

func TestDeleteBFDRequiresAtomicTransactionTransport(t *testing.T) {
	c, g, records := bfdFixture()
	e, _ := script(t, bfdRead(bfdRows()...), step{output: table("bfd")}, step{output: table("bfd_sessions")})
	if done, err := e.DeleteBFD(context.Background(), c, g, records); err == nil || done {
		t.Fatalf("missing transaction transport fell back to unsafe CLI deletion: %v %v", done, err)
	}
}

func TestOptionalBFDColumnsRoundTripOVNEncoding(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		value int
		bad   bool
	}{
		{`300`, 300, false}, {`["set",[300]]`, 300, false}, {`["set",[]]`, 0, false}, {`["set",[1,2]]`, 0, true}, {`null`, 0, true}, {`"300"`, 0, true}, {`["set",["300"]]`, 0, true},
	} {
		got, err := intValue(json.RawMessage(tc.raw))
		if (err != nil) != tc.bad || (!tc.bad && got != tc.value) {
			t.Errorf("%s: got=%d err=%v", tc.raw, got, err)
		}
	}
	c, g, records := bfdFixture()
	rows := bfdRows()
	rows[0][7] = ovsSet()
	rows[1][4] = ovsSet()
	rows[1][7] = ovsSet("init")
	e, _ := script(t, bfdRead(rows...))
	state, err := e.GetBFD(context.Background(), c, g, records)
	if err != nil || state.Configured || len(state.Up) != 0 || !reflect.DeepEqual(state.Records, records) {
		t.Fatalf("optional pending state lost receipts: %#v %v", state, err)
	}
}

func TestBFDRouteConflictsFindOnlyStaleLocalDefaults(t *testing.T) {
	c, g, records := bfdFixture()
	for _, tc := range []struct {
		name, cidr, ip, policy, routeTable string
		bfd                                any
		conflict                           bool
	}{
		{"valid", "0.0.0.0/0", "10.253.30.2", "dst-ip", "", ovsUUID(id1), false},
		{"no BFD", "0.0.0.0/0", "10.253.30.2", "dst-ip", "", ovsSet(), true},
		{"stale BFD", "0.0.0.0/0", "10.253.30.2", "dst-ip", "", ovsUUID(id3), true},
		{"local source", "10.253.30.0/29", "10.253.30.2", "src-ip", "", ovsSet(), false},
		{"other table", "0.0.0.0/0", "10.253.30.2", "dst-ip", "other", ovsSet(), false},
		{"other gateway", "0.0.0.0/0", "10.253.30.4", "dst-ip", "", ovsSet(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := script(t, step{output: table("name,static_routes", []any{"vpc-a", ovsUUID(id3)})}, step{output: table("ip_prefix,nexthop,policy,route_table,bfd", []any{tc.cidr, tc.ip, tc.policy, tc.routeTable, tc.bfd}), check: requireArgs("ovn-nbctl", "--timeout=15", "--format=json", "--columns=ip_prefix,nexthop,policy,route_table,bfd", "list", "Logical_Router_Static_Route", id3)})
			conflicts, err := e.BFDRouteConflicts(context.Background(), c, g, records)
			if err != nil || conflicts[tc.ip] != tc.conflict {
				t.Fatalf("wrong stale route decision: %v %v", conflicts, err)
			}
		})
	}
}

func TestRoutesReadyRequiresExactBFDReference(t *testing.T) {
	for _, tc := range []struct {
		name   string
		actual any
		ready  bool
	}{
		{"exact", ovsUUID(id1), true}, {"missing", ovsSet(), false}, {"replacement", ovsUUID(id2), false}, {"multiple", ovsSet(ovsUUID(id1), ovsUUID(id2)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := script(t, step{output: table("name,static_routes", []any{"vpc-a", ovsUUID(id3)})}, step{output: table("ip_prefix,nexthop,policy,route_table,bfd", []any{"0.0.0.0/0", "10.253.30.2", "dst-ip", "", tc.actual}), check: requireArgs("ovn-nbctl", "--timeout=15", "--format=json", "--columns=ip_prefix,nexthop,policy,route_table,bfd", "list", "Logical_Router_Static_Route", id3)})
			ready, err := e.RoutesReady(context.Background(), cfg(), "vpc-a", []Route{{CIDR: "0.0.0.0/0", NextHopIP: "10.253.30.2", BFD: id1}})
			if err != nil || ready != tc.ready {
				t.Fatalf("ready=%v err=%v", ready, err)
			}
		})
	}
}
