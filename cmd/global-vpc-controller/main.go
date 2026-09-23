package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	api "globalvpc.io/controller/api/v1alpha1"
	"globalvpc.io/controller/internal/config"
	reconciler "globalvpc.io/controller/internal/controller"
	"globalvpc.io/controller/internal/ovn"
	"globalvpc.io/controller/internal/provider"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	var path, namespace, probe string
	var interval time.Duration
	flag.StringVar(&path, "config", "", "Path to administrator-owned domain JSON configuration")
	flag.StringVar(&namespace, "namespace", "global-vpc-system", "Authority namespace for the Lease and durable domain lock")
	flag.StringVar(&probe, "health-probe-bind-address", ":8081", "Health probe listener")
	flag.DurationVar(&interval, "resync-period", 15*time.Second, "Remote topology drift reconciliation interval")
	flag.Parse()
	if path == "" || interval < time.Second {
		return fmt.Errorf("--config is required and --resync-period must be at least one second")
	}
	cfg, err := config.Load(path)
	if err != nil {
		return fmt.Errorf("cannot load domain registration: %w", err)
	}
	ctrl.SetLogger(zap.New())
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = api.AddToScheme(scheme)
	authority, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("cannot load authority Kubernetes access")
	}
	authority.Timeout = 30 * time.Second
	manager, err := ctrl.NewManager(authority, ctrl.Options{Scheme: scheme, LeaderElection: true, LeaderElectionNamespace: namespace, LeaderElectionID: "global-vpc-controller-" + cfg.Domain, LeaderElectionReleaseOnCancel: false, HealthProbeBindAddress: probe, Metrics: metricsserver.Options{BindAddress: "0"}})
	if err != nil {
		return fmt.Errorf("cannot initialize controller manager")
	}
	// Authoritative GET/UPDATE and every Infra read bypass informer staleness.
	direct, err := client.New(authority, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("cannot initialize authority client")
	}
	stores := map[string]reconciler.ResourceStore{}
	for _, c := range cfg.Clusters {
		rest, err := clientcmd.BuildConfigFromFlags("", c.Kubeconfig)
		if err != nil {
			return fmt.Errorf("cannot load registered Infra access")
		}
		rest.Timeout = 25 * time.Second
		remote, err := client.New(rest, client.Options{Scheme: scheme})
		if err != nil {
			return fmt.Errorf("cannot initialize Infra client")
		}
		stores[c.Name] = reconciler.ResourceStore{Client: remote, Config: c}
	}
	providers := map[string]reconciler.IPAM{}
	for name, command := range cfg.Providers {
		providers[name] = provider.Exec{Command: command, Timeout: 30 * time.Second}
	}
	r := &reconciler.Reconciler{Client: direct, Config: cfg, Stores: stores, Providers: providers, Network: &ovn.Exec{}, Interval: interval}
	r.Gate = &reconciler.DomainGate{Authority: direct, Namespace: namespace, Config: cfg, Stores: stores}
	if err = ctrl.NewControllerManagedBy(manager).For(&api.GlobalVpc{}).WithOptions(controller.Options{MaxConcurrentReconciles: 1}).Complete(r); err != nil {
		return fmt.Errorf("cannot register GlobalVpc controller")
	}
	if err = manager.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err = manager.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	return manager.Start(ctrl.SetupSignalHandler())
}
