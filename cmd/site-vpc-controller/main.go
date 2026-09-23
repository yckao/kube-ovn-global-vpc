// site-vpc-controller has one local authority and one local Infra attachment.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	api "globalvpc.io/controller/api/v1alpha1"
	"globalvpc.io/controller/internal/config"
	legacy "globalvpc.io/controller/internal/controller"
	"globalvpc.io/controller/internal/ovn"
	"globalvpc.io/controller/internal/siteconfig"
	"globalvpc.io/controller/internal/sitecontroller"
	"globalvpc.io/controller/internal/sitegateway"
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
	flag.StringVar(&path, "config", "", "Administrator-owned local attachment registration")
	flag.StringVar(&namespace, "namespace", "global-vpc-system", "Local authority namespace for the leadership Lease")
	flag.StringVar(&probe, "health-probe-bind-address", ":8081", "Health probe listener")
	flag.DurationVar(&interval, "resync-period", 10*time.Second, "Local drift reconciliation interval")
	flag.Parse()
	if path == "" || interval < time.Second {
		return fmt.Errorf("--config is required and --resync-period must be at least one second")
	}
	cfg, err := siteconfig.Load(path)
	if err != nil {
		return fmt.Errorf("cannot load local registration: %w", err)
	}
	ctrl.SetLogger(zap.New())
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = api.AddToScheme(scheme)
	authority, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("cannot load local authority access")
	}
	authority.Timeout = 25 * time.Second
	manager, err := ctrl.NewManager(authority, ctrl.Options{Scheme: scheme, LeaderElection: true, LeaderElectionNamespace: namespace,
		LeaderElectionID: "site-vpc-" + cfg.AttachmentID, LeaderElectionReleaseOnCancel: false,
		HealthProbeBindAddress: probe, Metrics: metricsserver.Options{BindAddress: "0"}})
	if err != nil {
		return fmt.Errorf("cannot initialize local manager")
	}
	direct, err := client.New(authority, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("cannot initialize local authority client")
	}
	infra, err := clientcmd.BuildConfigFromFlags("", cfg.Cluster.Kubeconfig)
	if err != nil {
		return fmt.Errorf("cannot load local Infra access")
	}
	infra.Timeout = 25 * time.Second
	local, err := client.New(infra, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("cannot initialize local Infra client")
	}
	cluster := config.Cluster{Name: cfg.Cluster.Name, UID: cfg.Cluster.UID, NBCommand: cfg.Cluster.NBCommand, SBCommand: cfg.Cluster.SBCommand}
	r := &sitecontroller.Reconciler{Client: direct, Config: cfg, Store: legacy.ResourceStore{Client: local, Config: cluster},
		Gateway: &sitegateway.Exec{Command: cfg.GatewayCommand, Timeout: 90 * time.Second}, Network: &ovn.Exec{}, BFD: &ovn.Exec{TransactionCommand: cfg.BFDTransactionCommand}, Interval: interval}
	if err = ctrl.NewControllerManagedBy(manager).For(&api.SiteVpc{}).WithOptions(controller.Options{MaxConcurrentReconciles: 1}).Complete(r); err != nil {
		return fmt.Errorf("cannot register local SiteVpc controller")
	}
	if err = manager.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err = manager.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	return manager.Start(ctrl.SetupSignalHandler())
}
