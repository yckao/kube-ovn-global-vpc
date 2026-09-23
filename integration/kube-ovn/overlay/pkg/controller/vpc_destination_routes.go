package controller

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/destinationroute"
	"github.com/kubeovn/kube-ovn/pkg/util"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// Kept separate from the existing broad mock interface: only the patched native
// OVNNbClient implements this capability. A missing implementation fails closed.
type destinationRouteClient interface {
	ReconcileDestinationRoutes(router, owner, bfdPort string, plan destinationroute.Plan) error
}

func validateVpcDestinationRoutes(vpc *kubeovnv1.Vpc) (destinationroute.Plan, error) {
	plan, err := destinationroute.Compile(vpc.Spec.DestinationRoutes)
	if err != nil {
		return plan, err
	}
	if len(plan.Routes) == 0 {
		return plan, nil
	}
	if vpc.Name == util.DefaultVpc || vpc.Status.Default {
		return plan, fmt.Errorf("destination routes require a custom VPC")
	}
	if !vpc.Spec.BFDPort.IsEnabled() {
		return plan, fmt.Errorf("destination routes require enabled bfdPort")
	}
	for _, route := range plan.Routes {
		dst := netip.MustParsePrefix(route.CIDR)
		for _, static := range vpc.Spec.StaticRoutes {
			if static == nil {
				continue
			}
			prefix, err := netip.ParsePrefix(static.CIDR)
			if err != nil {
				if host, hostErr := netip.ParseAddr(static.CIDR); hostErr == nil {
					prefix = netip.PrefixFrom(host, host.BitLen())
					err = nil
				}
			}
			if err == nil && static.RouteTable == "" && static.Policy != kubeovnv1.PolicySrc && dst.Overlaps(prefix) && prefix.Bits() >= dst.Bits() {
				return plan, fmt.Errorf("destination conflicts with staticRoutes")
			}
		}
	}
	return plan, nil
}
func (c *Controller) reconcileVpcDestinationRoutes(vpc *kubeovnv1.Vpc, bfdPort string) error {
	plan, err := validateVpcDestinationRoutes(vpc)
	if err == nil {
		client, ok := c.OVNNbClient.(destinationRouteClient)
		if !ok {
			if vpc.Spec.DestinationRoutes == nil && vpc.Status.DestinationRoutes.Capability == "" {
				return nil
			}
			err = fmt.Errorf("native destination-route capability unavailable")
		} else {
			err = client.ReconcileDestinationRoutes(vpc.Name, string(vpc.UID), bfdPort, plan)
		}
	}
	vpc.Status.DestinationRoutes = kubeovnv1.DestinationRoutesStatus{Capability: destinationroute.Capability, ObservedGeneration: vpc.Generation, Ready: err == nil}
	if err == nil {
		vpc.Status.DestinationRoutes.AppliedHash = plan.Hash
	}
	// A failure must revoke a previous acknowledgement of the same generation.
	if err != nil {
		if _, statusErr := c.config.KubeOvnClient.KubeovnV1().Vpcs().UpdateStatus(context.Background(), vpc, metav1.UpdateOptions{}); statusErr != nil {
			return statusErr
		}
		return err
	}
	if len(plan.Routes) != 0 {
		c.addOrUpdateVpcQueue.AddAfter(vpc.Name, 30*time.Second)
	}
	return nil
}
func (c *Controller) deleteVpcDestinationRoutes(vpc *kubeovnv1.Vpc) error {
	client, ok := c.OVNNbClient.(destinationRouteClient)
	if !ok {
		if vpc.Spec.DestinationRoutes == nil && vpc.Status.DestinationRoutes.Capability == "" {
			return nil
		}
		return fmt.Errorf("native destination-route capability unavailable")
	}
	plan, _ := destinationroute.Compile(nil)
	return client.ReconcileDestinationRoutes(vpc.Name, string(vpc.UID), "", plan)
}

// BFD rows are roots and survive router deletion. Recover orphan cleanup after
// a controller crash using UID ownership, never a name-only adoption.
func (c *Controller) gcDestinationRoutes() error {
	client, ok := c.OVNNbClient.(destinationRouteClient)
	if !ok {
		return nil
	}
	vpcs, err := c.vpcsLister.List(labels.Everything())
	if err != nil {
		return err
	}
	live := map[string]bool{}
	for _, vpc := range vpcs {
		live[string(vpc.UID)] = true
	}
	sessions, err := c.OVNNbClient.FindBFD(nil)
	if err != nil {
		return err
	}
	orphan := map[string]bool{}
	for _, bfd := range sessions {
		owner := bfd.ExternalIDs[destinationroute.OwnerKey]
		if owner != "" && !live[owner] {
			orphan[owner] = true
		}
	}
	routers, err := c.OVNNbClient.ListLogicalRouter(false, nil)
	if err != nil {
		return err
	}
	// A parent discard guard can outlive all sessions if BFD rows were removed.
	// Discover orphan ownership from routes too, including same-name VPC replacement.
	for _, router := range routers {
		routes, err := c.OVNNbClient.ListLogicalRouterStaticRoutes(router.Name, nil, nil, "", nil)
		if err != nil {
			return err
		}
		for _, route := range routes {
			owner := route.ExternalIDs[destinationroute.OwnerKey]
			if owner != "" && !live[owner] {
				orphan[owner] = true
			}
		}
	}
	plan, _ := destinationroute.Compile(nil)
	for owner := range orphan {
		for _, router := range routers {
			// Reconcile is safe only if this router owns rows with this UID. BFD cleanup
			// may fail while another router still references the orphan; retry after its
			// native router GC rather than deleting a foreign reference.
			routes, err := c.OVNNbClient.ListLogicalRouterStaticRoutes(router.Name, nil, nil, "", map[string]string{destinationroute.OwnerKey: owner})
			if err != nil {
				return err
			}
			if len(routes) != 0 {
				if err := client.ReconcileDestinationRoutes(router.Name, owner, "", plan); err != nil {
					return err
				}
			}
		}
		if err := client.ReconcileDestinationRoutes("", owner, "", plan); err != nil {
			return err
		}
	}
	return nil
}
