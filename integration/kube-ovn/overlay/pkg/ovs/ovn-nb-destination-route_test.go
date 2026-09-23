package ovs

import (
	"encoding/json"
	"testing"
	"time"

	"context"
	"github.com/kubeovn/kube-ovn/pkg/destinationroute"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnnb"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
	"github.com/stretchr/testify/require"
)

func nativeDestinationFixture(t *testing.T) (*OVNNbClient, destinationroute.Plan) {
	t.Helper()
	db, err := ovnnb.FullDatabaseModel()
	require.NoError(t, err)
	_, sock := newOVSDBServer(t, "destination", db, ovnnb.Schema())
	c, err := newOvnNbClient(t, "unix:"+sock, 10)
	require.NoError(t, err)
	require.NoError(t, c.CreateLogicalRouter("tenant"))
	require.NoError(t, c.CreateLogicalRouterPort("tenant", "transit", "02:00:00:00:01:01", []string{"172.20.0.1/24"}))
	require.NoError(t, c.CreateLogicalRouterPort("tenant", "bfd@tenant", "02:00:00:00:01:02", []string{"169.254.100.1/32"}))
	require.NoError(t, c.UpdateLogicalRouterPortOptions("bfd@tenant", map[string]string{"bfd-only": "true"}))
	p, err := destinationroute.Compile([]destinationroute.Intent{{CIDR: "10.70.0.0/24", NextHops: []string{"172.20.0.2", "172.20.0.3"}, BFD: destinationroute.BFD{MinRX: 100, MinTX: 100, Multiplier: 3}, SelectionFields: []string{"ip_src", "ip_dst", "ip_proto", "tp_src", "tp_dst"}}})
	require.NoError(t, err)
	return c, p
}
func TestNativeDestinationRoutes(t *testing.T) {
	t.Run("bfd-absence-wire-and-atomic-guard", func(t *testing.T) {
		c, _ := nativeDestinationFixture(t)
		guard := destinationBFDWait("bfd@tenant", "172.20.0.2", nil)
		wire, err := json.Marshal(guard)
		require.NoError(t, err)
		var decoded map[string]any
		require.NoError(t, json.Unmarshal(wire, &decoded))
		require.Len(t, decoded["rows"], 1)
		require.Equal(t, "!=", decoded["until"])
		insert, err := c.Create(&ovnnb.BFD{LogicalPort: "bfd@tenant", DstIP: "172.20.0.2"})
		require.NoError(t, err)
		ops := append([]ovsdb.Operation{guard}, insert...)
		results, err := c.Client.Transact(context.Background(), ops...)
		require.NoError(t, err)
		_, err = ovsdb.CheckOperationResults(results, ops)
		require.NoError(t, err)
		// Reusing the stale absence snapshot must prevent an unrelated mutation.
		insert, err = c.Create(&ovnnb.BFD{LogicalPort: "bfd@tenant", DstIP: "172.20.0.9"})
		require.NoError(t, err)
		ops = append([]ovsdb.Operation{guard}, insert...)
		results, err = c.Client.Transact(context.Background(), ops...)
		require.NoError(t, err)
		_, err = ovsdb.CheckOperationResults(results, ops)
		require.Error(t, err)
		sessions, err := c.ListBFDs("bfd@tenant", "172.20.0.9")
		require.NoError(t, err)
		require.Empty(t, sessions)
	})
	t.Run("snapshot-guard-rolls-back-mutation", func(t *testing.T) {
		c, _ := nativeDestinationFixture(t)
		require.NoError(t, c.AddLogicalRouterStaticRoute("tenant", "", "dst-ip", "0.0.0.0/0", nil, nil, "172.20.0.254"))
		lr, err := c.GetLogicalRouter("tenant", false)
		require.NoError(t, err)
		empty, _ := ovsdb.NewOvsSet([]ovsdb.UUID{})
		guard := destinationWait(ovnnb.LogicalRouterTable, "_uuid", ovsdb.UUID{GoUUID: lr.UUID}, "static_routes", empty)
		next, err := c.Create(&ovnnb.BFD{LogicalPort: "bfd@tenant", DstIP: "172.20.0.9"})
		require.NoError(t, err)
		ops := append([]ovsdb.Operation{guard}, next...)
		result, err := c.Client.Transact(context.Background(), ops...)
		require.NoError(t, err)
		_, err = ovsdb.CheckOperationResults(result, ops)
		require.Error(t, err)
		sessions, err := c.FindBFD(nil)
		require.NoError(t, err)
		require.Empty(t, sessions)
	})
	t.Run("apply-idempotency-rotation-cleanup", func(t *testing.T) {
		c, p := nativeDestinationFixture(t)
		require.NoError(t, c.AddLogicalRouterStaticRoute("tenant", "", "dst-ip", "0.0.0.0/0", nil, map[string]string{"vendor": "kube-ovn"}, "172.20.0.254"))
		nativeConverge(t, c, "tenant", "uid1", "bfd@tenant", p)
		first, err := c.ListLogicalRouterStaticRoutes("tenant", nil, nil, "", nil)
		require.NoError(t, err)
		require.Len(t, first, 6)
		bfds, err := c.FindBFD(map[string]string{destinationroute.OwnerKey: "uid1"})
		require.NoError(t, err)
		require.Len(t, bfds, 2)
		for _, r := range first {
			if r.Nexthop == "discard" {
				require.Equal(t, "10.70.0.0/24", r.IPPrefix)
				require.Nil(t, r.BFD)
			} else if r.IPPrefix != "0.0.0.0/0" {
				require.NotNil(t, r.BFD)
				require.Len(t, r.SelectionFields, 5)
				require.Contains(t, []string{"10.70.0.0/25", "10.70.0.128/25"}, r.IPPrefix)
			}
		}
		nativeConverge(t, c, "tenant", "uid1", "bfd@tenant", p)
		again, err := c.ListLogicalRouterStaticRoutes("tenant", nil, nil, "", nil)
		require.NoError(t, err)
		require.ElementsMatch(t, first, again)
		require.NoError(t, c.DeleteBFD(bfds[0].UUID))
		nativeConverge(t, c, "tenant", "uid1", "bfd@tenant", p)
		newBFD, err := c.FindBFD(map[string]string{destinationroute.OwnerKey: "uid1"})
		require.NoError(t, err)
		require.Len(t, newBFD, 2)
		for _, b := range newBFD {
			require.NotEqual(t, bfds[0].UUID, b.UUID)
		}
		empty, _ := destinationroute.Compile(nil)
		nativeConverge(t, c, "tenant", "uid1", "", empty)
		left, err := c.ListLogicalRouterStaticRoutes("tenant", nil, nil, "", nil)
		require.NoError(t, err)
		require.Len(t, left, 1)
		require.Equal(t, "0.0.0.0/0", left[0].IPPrefix)
		b, err := c.FindBFD(map[string]string{destinationroute.OwnerKey: "uid1"})
		require.NoError(t, err)
		require.Empty(t, b)
		nativeConverge(t, c, "tenant", "uid1", "", empty)
	})
	t.Run("foreign-bfd-refused", func(t *testing.T) {
		c, p := nativeDestinationFixture(t)
		foreign, err := c.CreateBFD("bfd@tenant", "172.20.0.2", 100, 100, 3, map[string]string{"owner": "foreign"})
		require.NoError(t, err)
		require.ErrorContains(t, c.ReconcileDestinationRoutes("tenant", "uid1", "bfd@tenant", p), "foreign")
		b, err := c.ListBFDs("bfd@tenant", "")
		require.NoError(t, err)
		require.Len(t, b, 1)
		require.Equal(t, foreign.UUID, b[0].UUID)
		routes, err := c.ListLogicalRouterStaticRoutes("tenant", nil, nil, "", nil)
		require.NoError(t, err)
		require.Empty(t, routes)
	})
	t.Run("generated-child-collision-refused", func(t *testing.T) {
		c, p := nativeDestinationFixture(t)
		require.NoError(t, c.AddLogicalRouterStaticRoute("tenant", "", "dst-ip", "10.70.0.0/25", nil, map[string]string{"owner": "foreign"}, "172.20.0.4"))
		require.ErrorContains(t, c.ReconcileDestinationRoutes("tenant", "uid1", "bfd@tenant", p), "foreign static route")
		b, err := c.FindBFD(nil)
		require.NoError(t, err)
		require.Empty(t, b)
	})
	t.Run("replacement-uid-refused", func(t *testing.T) {
		c, p := nativeDestinationFixture(t)
		nativeConverge(t, c, "tenant", "uid1", "bfd@tenant", p)
		require.ErrorContains(t, c.ReconcileDestinationRoutes("tenant", "uid2", "bfd@tenant", p), "foreign static route")
	})
	t.Run("foreign-consumer-blocks-cleanup", func(t *testing.T) {
		c, p := nativeDestinationFixture(t)
		nativeConverge(t, c, "tenant", "uid1", "bfd@tenant", p)
		b, err := c.FindBFD(map[string]string{destinationroute.OwnerKey: "uid1"})
		require.NoError(t, err)
		require.NoError(t, c.AddLogicalRouterStaticRoute("tenant", "", "dst-ip", "10.90.0.0/24", &b[0].UUID, map[string]string{"owner": "foreign"}, b[0].DstIP))
		empty, _ := destinationroute.Compile(nil)
		require.ErrorContains(t, c.ReconcileDestinationRoutes("tenant", "uid1", "", empty), "another route consumer")
		routes, err := c.ListLogicalRouterStaticRoutes("tenant", nil, nil, "", map[string]string{destinationroute.OwnerKey: "uid1"})
		require.NoError(t, err)
		require.Len(t, routes, 5)
	})
}

// Native reconciliation retries when an OVSDB monitor snapshot races a committed
// transaction. The CAS must reject that stale snapshot before retrying.
func nativeConverge(t *testing.T, c *OVNNbClient, router, owner, port string, p destinationroute.Plan) {
	t.Helper()
	require.Eventually(t, func() bool { return c.ReconcileDestinationRoutes(router, owner, port, p) == nil }, time.Second, 10*time.Millisecond)
}
