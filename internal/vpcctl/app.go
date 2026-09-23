// Package vpcctl implements the public resource CLI and native installation tools.
package vpcctl

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/spf13/pflag"
	api "globalvpc.io/controller/api/v1alpha2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Options selects Kubernetes access and presentation without storing credentials.
type Options struct {
	Kubeconfig string
	Context    string
	Namespace  string
	Output     string
	Timeout    time.Duration
}

// App allows API and subprocess boundaries to be tested without a live cluster.
type App struct {
	Out            io.Writer
	ErrOut         io.Writer
	Version        string
	Commit         string
	SourceDate     string
	ClientFactory  func(Options) (client.Client, error)
	CommandRunner  func(context.Context, string, []string, string, []string, io.Writer, io.Writer) error
	PodExecutor    func(context.Context, Options, string, string, string, []string, io.Writer, io.Writer) error
	TokenRequester func(context.Context, Options, string, string, time.Duration) (string, time.Time, error)
	HTTPClient     *http.Client
}

const usage = `vpcctl manages project VPCs and Subnets through the management API.

Usage:
  vpcctl [global flags] vpc create|list|get|delete|wait ...
  vpcctl [global flags] subnet create|list|get|delete|wait ...
  vpcctl [global flags] doctor
  vpcctl [global flags] install|plan|upgrade|status|history|verify|rollback|uninstall RELEASE ...
  vpcctl values --component authority|site|native --version VERSION
  vpcctl [global flags] drain --project PROJECT
  vpcctl [global flags] access issue ...
  vpcctl release inspect VERSION|FILE|URL
  vpcctl maintainer native build ...
  vpcctl version

Examples:
  vpcctl --context management -n project-demo vpc create production
  vpcctl --context management -n project-demo subnet create app-a --vpc production --location dc-a --cidr 10.60.1.0/24
  vpcctl --context management -n project-demo vpc get production
  vpcctl --context infra-a -n global-vpc-system status native-extension

Global flags precede the command. Use COMMAND --help for command-specific flags.
The namespace defaults to the selected kubeconfig context for public resources
and global-vpc-system for Helm releases. Installation uses prebuilt images.
Access issuance explicitly writes a private, location-scoped kubeconfig.
Ready is a configuration observation, not a packet-delivery guarantee.
`

// Run parses a single invocation. Calls use the caller's cancellation context.
func (a *App) Run(ctx context.Context, args []string) error {
	err := a.run(ctx, args)
	if errors.Is(err, flag.ErrHelp) || errors.Is(err, pflag.ErrHelp) {
		return nil
	}
	return err
}

func (a *App) run(ctx context.Context, args []string) error {
	if a.Out == nil {
		a.Out = io.Discard
	}
	if a.ErrOut == nil {
		a.ErrOut = io.Discard
	}
	opts := Options{Output: "table", Timeout: 5 * time.Minute}
	flags := pflag.NewFlagSet("vpcctl", pflag.ContinueOnError)
	flags.SetOutput(a.ErrOut)
	flags.SetInterspersed(false)
	flags.StringVar(&opts.Kubeconfig, "kubeconfig", "", "Kubeconfig path (defaults to KUBECONFIG or standard loading rules)")
	flags.StringVar(&opts.Context, "context", "", "Kubeconfig context; does not change the current context")
	flags.StringVarP(&opts.Namespace, "namespace", "n", "", "Project or release namespace (Helm default: global-vpc-system; public API default: selected context)")
	flags.StringVarP(&opts.Output, "output", "o", "table", "Output format: table, json, yaml")
	flags.DurationVar(&opts.Timeout, "timeout", 5*time.Minute, "Maximum wait for reconciliation or rollout")
	var help, version bool
	flags.BoolVarP(&help, "help", "h", false, "Show help")
	flags.BoolVar(&version, "version", false, "Print version without loading Kubernetes access")
	if err := flags.Parse(args); err != nil {
		return err
	}
	args = flags.Args()
	if help || len(args) == 0 && !version {
		fmt.Fprint(a.Out, usage)
		fmt.Fprintln(a.Out, "\nGlobal flags:")
		fmt.Fprint(a.Out, flags.FlagUsages())
		return nil
	}
	if version || len(args) == 1 && args[0] == "version" {
		v := a.Version
		if v == "" {
			v = "dev"
		}
		fmt.Fprintf(a.Out, "vpcctl %s (commit=%s, sourceDate=%s)\n", v, a.Commit, a.SourceDate)
		return nil
	}
	if opts.Timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	if opts.Output != "table" && opts.Output != "json" && opts.Output != "yaml" {
		return fmt.Errorf("--output must be table, json or yaml")
	}
	if args[0] == "maintainer" {
		if len(args) == 1 || hasHelp(args[1:]) && len(args) < 3 {
			fmt.Fprintln(a.Out, "Usage: vpcctl maintainer native build|plan|install|upgrade|verify|drain|rollback|uninstall|status [flags]\nMaintainer/CI tools. Source builds require Go, Git, Python, Helm and Docker/buildx. Administrators use the Helm lifecycle commands with prebuilt releases.")
			return nil
		}
		if args[1] == "native" {
			if len(args) >= 3 && args[2] == "build" {
				return a.runNativeBuild(ctx, args[3:], opts)
			}
			return a.runNative(ctx, args[2:], opts)
		}
		return fmt.Errorf("unknown maintainer command; use maintainer native --help")
	}
	if args[0] == "release" {
		return a.runRelease(ctx, args[1:])
	}
	if args[0] == "internal" {
		if len(args) < 2 {
			return fmt.Errorf("internal commands are reserved for chart hooks")
		}
		switch args[1] {
		case "native-hook":
			return a.runNativeHook(ctx, args[2:], opts)
		case "platform-hook":
			return a.runPlatformHook(ctx, args[2:], opts)
		default:
			return fmt.Errorf("unknown internal chart hook")
		}
	}
	if args[0] == "admin" {
		if len(args) < 2 || args[1] == "--help" || args[1] == "-h" {
			fmt.Fprint(a.Out, helmUsage)
			return nil
		}
		if args[1] == "platform" {
			return a.runHelm(ctx, args[2:], opts)
		}
		if args[1] != "native" {
			if args[1] == "drain" {
				return a.runDrain(ctx, args[2:], opts)
			}
			return a.runHelm(ctx, args[1:], opts)
		}
		return fmt.Errorf("native administration uses Helm releases: use install|upgrade|status|verify|rollback|uninstall RELEASE with --component native where applicable; low-level recovery tools are under maintainer native")
	}
	if args[0] == "native" {
		return fmt.Errorf("native administration uses Helm releases: use install RELEASE --component native --version VERSION; low-level recovery tools are under maintainer native")
	}
	switch args[0] {
	case "access":
		return a.runAccess(ctx, args[1:], opts)
	case "install", "plan", "upgrade", "status", "history", "verify", "rollback", "uninstall", "values":
		return a.runHelm(ctx, args, opts)
	case "drain":
		return a.runDrain(ctx, args[1:], opts)
	case "vpc", "subnet", "doctor":
		// Help should work even when the requested kubeconfig does not exist.
		resourceHelpOnly := args[0] != "doctor" && (len(args) == 1 || args[1] == "help")
		if !hasHelp(args) && !resourceHelpOnly && opts.Namespace == "" {
			var err error
			opts.Namespace, _, err = kubeAccess(opts).Namespace()
			if err != nil {
				return fmt.Errorf("cannot resolve project namespace; set -n explicitly: %w", err)
			}
		}
		if args[0] == "doctor" {
			return a.runDoctor(ctx, args[1:], opts)
		}
		return a.runResources(ctx, args, opts)
	default:
		return fmt.Errorf("unknown command %q; use vpcctl --help", args[0])
	}
}

func hasHelp(args []string) bool {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			return true
		}
	}
	return false
}

func kubeAccess(opts Options) clientcmd.ClientConfig {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = opts.Kubeconfig
	overrides := &clientcmd.ConfigOverrides{CurrentContext: opts.Context}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
}

func (a *App) client(opts Options) (client.Client, error) {
	if a.ClientFactory != nil {
		return a.ClientFactory(opts)
	}
	cfg, err := a.restConfig(opts)
	if err != nil {
		return nil, err
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{api.AddToScheme, corev1.AddToScheme, appsv1.AddToScheme} {
		if err := add(scheme); err != nil {
			return nil, err
		}
	}
	return client.New(cfg, client.Options{Scheme: scheme})
}

func (a *App) command(ctx context.Context, name string, args []string, dir string, env []string) error {
	if a.CommandRunner != nil {
		return a.CommandRunner(ctx, name, args, dir, env, a.Out, a.ErrOut)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, a.Out, a.ErrOut
	cmd.Env = append(os.Environ(), env...)
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return ctx.Err()
		}
		return fmt.Errorf("%s failed: %w", name, err)
	}
	return nil
}
