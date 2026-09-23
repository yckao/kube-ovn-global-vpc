package controller

import (
	"fmt"
	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	kubeovnlisters "github.com/kubeovn/kube-ovn/pkg/client/listers/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/destinationroute"
	"github.com/kubeovn/kube-ovn/pkg/ovs"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnnb"
	"github.com/kubeovn/kube-ovn/pkg/util"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"testing"
)

func TestDestinationRouteValidation(t *testing.T) {
	v := &kubeovnv1.Vpc{ObjectMeta: metav1.ObjectMeta{Name: "tenant"}, Spec: kubeovnv1.VpcSpec{BFDPort: &kubeovnv1.BFDPort{Enabled: true}, DestinationRoutes: []destinationroute.Intent{{CIDR: "10.70.0.0/24", NextHops: []string{"172.20.0.2", "172.20.0.3"}, BFD: destinationroute.BFD{MinRX: 100, MinTX: 100, Multiplier: 3}, SelectionFields: []string{"ip_src", "ip_dst"}}}}}
	_, err := validateVpcDestinationRoutes(v)
	require.NoError(t, err)
	copy := v.DeepCopy()
	copy.Spec.DestinationRoutes[0].NextHops[0] = "172.20.0.5"
	require.Equal(t, "172.20.0.2", v.Spec.DestinationRoutes[0].NextHops[0])
	v.Spec.StaticRoutes = []*kubeovnv1.StaticRoute{{CIDR: "0.0.0.0/0", NextHopIP: "172.20.0.254"}}
	_, err = validateVpcDestinationRoutes(v)
	require.NoError(t, err)
	v.Spec.StaticRoutes[0].CIDR = "10.70.0.128/25"
	_, err = validateVpcDestinationRoutes(v)
	require.ErrorContains(t, err, "conflicts")
	v.Spec.StaticRoutes = nil
	v.Name = util.DefaultVpc
	_, err = validateVpcDestinationRoutes(v)
	require.ErrorContains(t, err, "custom VPC")
	v.Name = "tenant"
	v.Spec.BFDPort.Enabled = false
	_, err = validateVpcDestinationRoutes(v)
	require.ErrorContains(t, err, "enabled bfdPort")
	v.Spec.DestinationRoutes = nil
	_, err = validateVpcDestinationRoutes(v)
	require.NoError(t, err)
}

// Collect an old guard even if its sessions disappeared before VPC replacement.
func TestDestinationRouteOrphanGuardGC(t *testing.T) {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	require.NoError(t, indexer.Add(&kubeovnv1.Vpc{ObjectMeta: metav1.ObjectMeta{Name: "tenant", UID: types.UID("new-uid")}}))
	fake := &destinationGCClient{routes: []*ovnnb.LogicalRouterStaticRoute{
		{IPPrefix: "10.70.0.0/24", Nexthop: "discard", ExternalIDs: map[string]string{destinationroute.OwnerKey: "old-uid"}},
		{IPPrefix: "10.80.0.0/24", Nexthop: "discard", ExternalIDs: map[string]string{destinationroute.OwnerKey: "new-uid"}},
	}}
	c := &Controller{OVNNbClient: fake, vpcsLister: kubeovnlisters.NewVpcLister(indexer)}
	require.NoError(t, c.gcDestinationRoutes())
	require.Equal(t, []string{"tenant:old-uid", ":old-uid"}, fake.deleted)
}

type destinationGCClient struct {
	ovs.NbClient
	routes  []*ovnnb.LogicalRouterStaticRoute
	deleted []string
}

func (f *destinationGCClient) FindBFD(map[string]string) ([]ovnnb.BFD, error) { return nil, nil }
func (f *destinationGCClient) ListLogicalRouter(bool, func(*ovnnb.LogicalRouter) bool) ([]ovnnb.LogicalRouter, error) {
	return []ovnnb.LogicalRouter{{Name: "tenant"}}, nil
}
func (f *destinationGCClient) ListLogicalRouterStaticRoutes(_ string, _ *string, _ *string, _ string, ids map[string]string) ([]*ovnnb.LogicalRouterStaticRoute, error) {
	var out []*ovnnb.LogicalRouterStaticRoute
	for _, route := range f.routes {
		if ids == nil || route.ExternalIDs[destinationroute.OwnerKey] == ids[destinationroute.OwnerKey] {
			out = append(out, route)
		}
	}
	return out, nil
}
func (f *destinationGCClient) ReconcileDestinationRoutes(router, owner, _ string, p destinationroute.Plan) error {
	if len(p.Routes) != 0 {
		return fmt.Errorf("GC must withdraw only")
	}
	f.deleted = append(f.deleted, router+":"+owner)
	return nil
}
