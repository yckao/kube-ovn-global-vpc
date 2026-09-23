package vpcctl

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/util/validation"
)

const helmUsage = `vpcctl uses the project's Helm charts for the installation lifecycle.

Usage:
  vpcctl [global flags] install|plan|upgrade RELEASE --component authority|site|native --version VERSION [-f values.yaml]
  vpcctl [global flags] status|history|verify|uninstall RELEASE
  vpcctl [global flags] rollback RELEASE REVISION
  vpcctl values --component authority|site|native --version VERSION
  vpcctl [global flags] drain --project PROJECT

Helm owns release history and rollback state. The CLI requires Helm 3.14+ or 4,
but no local Go, Docker, BuildKit, Python or jq. Charts contain prebuilt images.
Global --context, --kubeconfig, --namespace and --timeout precede the command.
Uninstall runs chart cleanup checks; remove workloads and drain public resources
first. CRDs and allocation records are retained. No --no-hooks shortcut is exposed.
`

var helmCharts = map[string]string{"authority": "global-vpc", "site": "global-vpc-site", "native": "kube-ovn-global-vpc-extension"}

func (a *App) runHelm(ctx context.Context, args []string, opts Options) error {
	if len(args) == 0 || args[0] == "help" || hasHelp(args[:1]) {
		fmt.Fprint(a.Out, helmUsage)
		return nil
	}
	op := args[0]
	switch op {
	case "install", "plan", "upgrade", "status", "history", "verify", "rollback", "uninstall", "values":
	default:
		return fmt.Errorf("unknown lifecycle command %q; use --help", op)
	}
	f := pflag.NewFlagSet(op, pflag.ContinueOnError)
	f.SetOutput(a.ErrOut)
	var component, chart, version string
	var values, set, setString, setFile []string
	var keepHistory bool
	chartOperation := op == "install" || op == "plan" || op == "upgrade" || op == "values"
	if chartOperation {
		f.StringVar(&component, "component", "authority", "Chart component: authority, site or native")
		f.StringVar(&chart, "chart", "", "Override the published OCI chart with a trusted chart path/reference")
		f.StringVar(&version, "version", "", "Explicit chart release version (required for published charts)")
	}
	if op == "install" || op == "plan" || op == "upgrade" {
		f.StringArrayVarP(&values, "values", "f", nil, "Helm values file (repeatable)")
		f.StringArrayVar(&set, "set", nil, "Helm value override (repeatable)")
		f.StringArrayVar(&setString, "set-string", nil, "Helm string override (repeatable)")
		f.StringArrayVar(&setFile, "set-file", nil, "Helm value loaded from a file (repeatable)")
	}
	if op == "uninstall" {
		f.BoolVar(&keepHistory, "keep-history", true, "Retain Helm release history after uninstall")
	}
	f.Usage = func() { fmt.Fprint(a.Out, helmUsage); fmt.Fprintln(a.Out); fmt.Fprint(a.Out, f.FlagUsages()) }
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	want := 1
	if op == "values" {
		want = 0
	}
	if op == "rollback" {
		want = 2
	}
	if f.NArg() != want {
		return fmt.Errorf("%s requires %d argument(s); use --help", op, want)
	}
	name := ""
	if want > 0 {
		name = f.Arg(0)
		if len(name) > 53 || len(validation.IsDNS1123Label(name)) != 0 {
			return fmt.Errorf("Helm release name must be a DNS label of at most 53 characters")
		}
	}
	if opts.Namespace == "" {
		opts.Namespace = "global-vpc-system"
	}
	if len(validation.IsDNS1123Label(opts.Namespace)) != 0 {
		return fmt.Errorf("invalid Helm release namespace")
	}
	if chartOperation {
		if _, ok := helmCharts[component]; !ok {
			return fmt.Errorf("--component must be authority, site or native")
		}
		if chart == "" {
			if version == "" {
				return fmt.Errorf("--version is required; select a published prebuilt release")
			}
			chart = "oci://ghcr.io/yckao/kube-ovn-global-vpc/charts/" + helmCharts[component]
		}
		if strings.HasPrefix(chart, "-") {
			return fmt.Errorf("invalid chart reference")
		}
		if version != "" && !releaseVersion.MatchString("v"+strings.TrimPrefix(version, "v")) {
			return fmt.Errorf("invalid chart release version")
		}
	}
	command := []string{}
	switch op {
	case "install", "plan":
		command = []string{"upgrade", "--install", name, chart, "--create-namespace"}
	case "upgrade":
		command = []string{"upgrade", name, chart, "--reset-then-reuse-values"}
	case "values":
		command = []string{"show", "values", chart}
	case "rollback":
		revision, err := strconv.Atoi(f.Arg(1))
		if err != nil || revision < 1 {
			return fmt.Errorf("rollback requires a positive Helm revision from history")
		}
		command = []string{"rollback", name, strconv.Itoa(revision)}
	case "verify":
		command = []string{"test", name, "--logs"}
	case "uninstall":
		command = []string{"uninstall", name}
		if keepHistory {
			command = append(command, "--keep-history")
		}
	default:
		command = []string{op, name}
		if opts.Output != "table" {
			command = append(command, "--output", opts.Output)
		}
	}
	if chartOperation && version != "" {
		command = append(command, "--version", strings.TrimPrefix(version, "v"))
	}
	for _, v := range values {
		command = append(command, "--values", v)
	}
	for _, v := range set {
		command = append(command, "--set", v)
	}
	for _, v := range setString {
		command = append(command, "--set-string", v)
	}
	for _, v := range setFile {
		command = append(command, "--set-file", v)
	}
	if op != "values" {
		command = append(command, "--namespace", opts.Namespace)
		if opts.Context != "" {
			command = append(command, "--kube-context", opts.Context)
		}
		if opts.Kubeconfig != "" {
			command = append(command, "--kubeconfig", opts.Kubeconfig)
		}
	}
	if op == "plan" {
		command = append(command, "--dry-run=server", "--hide-secret")
	}
	if op == "install" || op == "upgrade" || op == "rollback" || op == "uninstall" {
		command = append(command, "--wait")
	}
	if op == "install" || op == "plan" || op == "upgrade" || op == "rollback" || op == "uninstall" || op == "verify" {
		command = append(command, "--timeout", opts.Timeout.String())
	}
	if a.CommandRunner == nil {
		if _, err := exec.LookPath("helm"); err != nil {
			return fmt.Errorf("Helm 3.14+ or Helm 4 is required; install Helm, then retry (no build toolchain is needed)")
		}
	}
	// Do not log argv: --set/--set-file may reference access material. Helm owns
	// its standard output, release Secrets, history and hook execution.
	return a.command(ctx, "helm", command, "", nil)
}
