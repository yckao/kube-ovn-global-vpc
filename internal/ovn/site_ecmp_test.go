package ovn

import (
	"context"
	"strings"
	"testing"
)

const selectionColumns = "ip_prefix,nexthop,policy,route_table,bfd,selection_fields,external_ids"

func selectionRead(ip, bfd, marker string, fields any) step {
	ids := map[string]string{"vendor": "kube-ovn"}
	if marker != "" {
		ids[ecmpOwnerKey] = marker
	}
	return step{output: table(selectionColumns, []any{"0.0.0.0/0", ip, ovsSet(), "", ovsUUID(bfd), fields, ovsMap(ids)})}
}

func selectionRouter() step {
	return step{output: table("_uuid,name,static_routes", []any{ovsUUID(bfdGroupID), "vpc-a", ovsSet(ovsUUID(id3), ovsUUID(bfdPortID))})}
}

func TestFlowHashFieldsAreGuardedAndNativeOwnershipIsPreserved(t *testing.T) {
	c, g, records := bfdFixture()
	check := func(route, bfd, ip string) func(*testing.T, []string) {
		return func(t *testing.T, args []string) {
			joined := strings.Join(args, " ")
			for _, want := range []string{"get Logical_Router " + bfdGroupID + " name static_routes",
				"get Logical_Router_Static_Route " + route + " ip_prefix nexthop policy route_table bfd external_ids selection_fields",
				"get BFD " + bfd + " external_ids logical_port dst_ip",
				"wait-until Logical_Router " + bfdGroupID, "static_routes{>=}" + route,
				"wait-until Logical_Router_Static_Route " + route, "bfd=" + bfd, `nexthop="` + ip + `"`, "policy=[]",
				"selection_fields=[]", `external_ids={"vendor"="kube-ovn"}`, "wait-until BFD " + bfd, `external_ids:global-vpc-uid="local-owner"`,
				"set Logical_Router_Static_Route " + route, "selection_fields=ip_dst,ip_proto,ip_src,tp_dst,tp_src"} {
				if !strings.Contains(joined, want) {
					t.Fatalf("missing atomic ownership guard %q in %q", want, joined)
				}
			}
			for _, forbidden := range []string{" create ", " destroy ", " lr-route-add ", "vendor=", ecmpOwnerKey + "=[]"} {
				if strings.Contains(joined, forbidden) {
					t.Fatalf("native route ownership was broadened: %q", joined)
				}
			}
		}
	}
	e, _ := script(t, selectionRouter(), selectionRead(g.Gateways[0].IP, id1, "", ovsSet()), selectionRead(g.Gateways[1].IP, id2, "", ovsSet()),
		step{check: check(id3, id1, g.Gateways[0].IP)}, step{check: check(bfdPortID, id2, g.Gateways[1].IP)})
	ready, err := e.EnsureECMPFields(context.Background(), c, g, records)
	if err != nil || ready {
		t.Fatalf("a new write needs an observed confirmation: ready=%v err=%v", ready, err)
	}
	fields := ovsSet("tp_src", "ip_src", "ip_dst", "tp_dst", "ip_proto")
	e, _ = script(t, selectionRouter(), selectionRead(g.Gateways[0].IP, id1, g.OwnerUID, fields), selectionRead(g.Gateways[1].IP, id2, g.OwnerUID, fields))
	if ready, err = e.EnsureECMPFields(context.Background(), c, g, records); err != nil || !ready {
		t.Fatalf("confirmed fields not ready: %v %v", ready, err)
	}
}

func TestFlowHashOwnershipAndBFDConflictCannotCausePartialWrites(t *testing.T) {
	c, g, records := bfdFixture()
	for _, tc := range []struct {
		name, bfd, owner string
		fields           any
		failure          bool
	}{
		{"foreign owner", id2, "someone-else", ovsSet(), true},
		{"unregistered nonempty fields", id2, "", ovsSet("ip_src"), true},
		{"stale BFD receipt", bfdChassis1, "", ovsSet(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, s := script(t, selectionRouter(), selectionRead(g.Gateways[0].IP, id1, "", ovsSet()), selectionRead(g.Gateways[1].IP, tc.bfd, tc.owner, tc.fields))
			ready, err := e.EnsureECMPFields(context.Background(), c, g, records)
			if ready || (err != nil) != tc.failure {
				t.Fatalf("ready=%v err=%v", ready, err)
			}
			for _, args := range s.calls {
				if strings.Contains(strings.Join(args, " "), " set ") {
					t.Fatal("partial update before all ownership checks")
				}
			}
		})
	}
}

func TestExactMapGuardHandlesAbsentAndEscapedMetadata(t *testing.T) {
	cases := []struct {
		name   string
		values map[string]string
		want   string
	}{
		{"nil map", nil, `external_ids={}`},
		{"empty map", map[string]string{}, `external_ids={}`},
		{"stable complete map", map[string]string{"vendor": "kube-ovn", "a": "value"}, `external_ids={"a"="value","vendor"="kube-ovn"}`},
		{"empty existing value", map[string]string{"owner": ""}, `external_ids={"owner"=""}`},
		{"quotes and separators", map[string]string{"a\"b": "value,=}[next]"}, `external_ids={"a\"b"="value,=}[next]"}`},
		{"backslash and newline", map[string]string{"path": "one\\two\nthree\tfour"}, `external_ids={"path"="one\\two\nthree\tfour"}`},
		{"JSON control escapes", map[string]string{"control": "\a\x1b"}, `external_ids={"control"="\u0007\u001b"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exactMapGuard("external_ids", tc.values); got != tc.want {
				t.Fatalf("guard = %q; want %q", got, tc.want)
			}
		})
	}
}
