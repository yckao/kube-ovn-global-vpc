// platform-vpc-controller runs either the platform authority or one local operator.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	api "globalvpc.io/controller/api/v1alpha2"
	"globalvpc.io/controller/internal/localconfig"
	"globalvpc.io/controller/internal/localcontroller"
	"globalvpc.io/controller/internal/managedgateway"
	"globalvpc.io/controller/internal/platformconfig"
	"globalvpc.io/controller/internal/platformcontroller"
	"globalvpc.io/controller/internal/platformplan"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// Release packaging overrides these values with source-controlled metadata.
// A direct developer build does not claim a known clean source commit.
var (
	version    = "v0.1.0"
	commit     = "unknown"
	sourceDate = "unknown"
)

type gatewayAdapter struct{ backend *managedgateway.Reconciler }

func (g gatewayAdapter) Ensure(ctx context.Context, b *api.NetworkBinding, a localcontroller.Allocation) (localcontroller.GatewayResult, error) {
	result, err := g.backend.Ensure(ctx, b, managedgateway.Attachment{TransitCIDR: a.TransitCIDR, RouterIP: a.RouterIP, BFDSourceIP: a.BFDSourceIP})
	return localcontroller.GatewayResult{Gateways: result.Gateways, Resources: result.Resources, Ready: result.Ready, Reason: result.Reason}, err
}
func (g gatewayAdapter) Delete(ctx context.Context, b *api.NetworkBinding) (bool, error) {
	return g.backend.Delete(ctx, b)
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	var mode, path, namespace, probe string
	var interval time.Duration
	var showVersion bool
	flag.StringVar(&mode, "mode", "authority", "Controller role: authority or site")
	flag.StringVar(&path, "config", "", "Administrator-owned JSON configuration path")
	flag.StringVar(&namespace, "namespace", "global-vpc-system", "Authority leadership namespace")
	flag.StringVar(&probe, "health-probe-bind-address", ":8081", "Health listener")
	flag.DurationVar(&interval, "resync-period", 5*time.Second, "Reconciliation and synchronization interval")
	flag.BoolVar(&showVersion, "version", false, "Print version metadata and exit")
	flag.Parse()
	if showVersion {
		fmt.Printf("platform-vpc-controller %s (commit=%s, sourceDate=%s)\n", version, commit, sourceDate)
		return nil
	}
	if path == "" || interval < time.Second || (mode != "authority" && mode != "site") {
		return fmt.Errorf("require --config, --mode authority|site and resync-period >= 1s")
	}
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("cannot load Kubernetes access")
	}
	cfg.Timeout = 15 * time.Second
	ctrl.SetLogger(zap.New())
	opts := ctrl.Options{Scheme: scheme, LeaderElection: true, LeaderElectionNamespace: namespace, LeaderElectionID: "platform-vpc-authority", LeaderElectionReleaseOnCancel: false, HealthProbeBindAddress: probe, Metrics: metricsserver.Options{BindAddress: "0"}}
	var platform platformconfig.Config
	var local localconfig.Config
	if mode == "authority" {
		platform, err = platformconfig.Load(path)
	} else {
		local, err = localconfig.Load(path)
		opts.LeaderElectionNamespace = local.Namespace
		opts.LeaderElectionID = "platform-vpc-site-" + local.LocationRef
		opts.Cache = cache.Options{DefaultNamespaces: map[string]cache.Config{local.Namespace: {}}}
	}
	if err != nil {
		return err
	}
	manager, err := ctrl.NewManager(cfg, opts)
	if err != nil {
		return err
	}
	direct, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	if mode == "authority" {
		r := &platformcontroller.Reconciler{Client: direct, Config: platform, Interval: interval}
		subnets := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []ctrl.Request {
			s := o.(*api.Subnet)
			return []ctrl.Request{{NamespacedName: client.ObjectKey{Namespace: s.Namespace, Name: s.Spec.VPCRef}}}
		})
		bindings := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []ctrl.Request {
			b := o.(*api.NetworkBinding)
			if b.Labels[platformplan.OwnerLabel] == "" {
				return nil
			}
			return []ctrl.Request{{NamespacedName: client.ObjectKey{Namespace: b.Spec.VPCRef.Namespace, Name: b.Spec.VPCRef.Name}}}
		})
		if err = ctrl.NewControllerManagedBy(manager).For(&api.VPC{}).Watches(&api.Subnet{}, subnets).Watches(&api.NetworkBinding{}, bindings).WithOptions(controller.Options{MaxConcurrentReconciles: 1}).Complete(r); err != nil {
			return err
		}
	} else {
		authorityCfg, e := clientcmd.BuildConfigFromFlags("", local.AuthorityKubeconfig)
		if e != nil {
			return fmt.Errorf("cannot load authority access reference")
		}
		authorityCfg.Timeout = 5 * time.Second
		authority, e := client.New(authorityCfg, client.Options{Scheme: scheme})
		if e != nil {
			return e
		}
		gateway := &managedgateway.Reconciler{Client: direct, Namespace: local.Namespace, ClusterUID: local.ClusterUID, Image: local.GatewayImage, RuntimeDir: local.RuntimeDir, EndpointAnnotation: local.EndpointAnnotation, NodeSelector: local.GatewayNodeSelector, Replicas: local.GatewayReplicas, MTU: local.MTU, AllocationPool: local.ControlPool, PortStart: local.PortStart, PortEnd: local.PortEnd, RolloutProbe: managedgateway.NativeProbe(cfg)}
		r := &localcontroller.Reconciler{Client: direct, Config: local, Gateway: gatewayAdapter{gateway}, Interval: interval}
		if err = ctrl.NewControllerManagedBy(manager).For(&api.NetworkBinding{}).WithOptions(controller.Options{MaxConcurrentReconciles: 1}).Complete(r); err != nil {
			return err
		}
		if err = manager.Add(&localcontroller.Syncer{Authority: authority, Local: direct, Config: local, Interval: interval}); err != nil {
			return err
		}
	}
	if err = manager.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err = manager.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	return manager.Start(ctrl.SetupSignalHandler())
}
