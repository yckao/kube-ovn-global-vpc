package vpcctl

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestHelmLifecycleUsesOnlyHelmAndItsReleaseHistory(t *testing.T) {
	for _, test := range []struct{ args, expected []string }{
		{[]string{"install", "fabric", "--component", "site", "--version", "v0.1.0", "-f", "site.yaml"}, []string{"upgrade", "--install", "fabric", "oci://ghcr.io/yckao/kube-ovn-global-vpc/charts/global-vpc-site", "--create-namespace", "--version", "0.1.0", "--values", "site.yaml", "--namespace", "network", "--kube-context", "infra-a", "--kubeconfig", "config.yaml", "--wait", "--timeout", "5m0s"}},
		{[]string{"upgrade", "fabric", "--component", "native", "--version", "0.2.0"}, []string{"upgrade", "fabric", "oci://ghcr.io/yckao/kube-ovn-global-vpc/charts/kube-ovn-global-vpc-extension", "--reset-then-reuse-values", "--version", "0.2.0", "--namespace", "network", "--kube-context", "infra-a", "--kubeconfig", "config.yaml", "--wait", "--timeout", "5m0s"}},
		{[]string{"rollback", "fabric", "2"}, []string{"rollback", "fabric", "2", "--namespace", "network", "--kube-context", "infra-a", "--kubeconfig", "config.yaml", "--wait", "--timeout", "5m0s"}},
		{[]string{"history", "fabric"}, []string{"history", "fabric", "--namespace", "network", "--kube-context", "infra-a", "--kubeconfig", "config.yaml"}},
		{[]string{"uninstall", "fabric"}, []string{"uninstall", "fabric", "--keep-history", "--namespace", "network", "--kube-context", "infra-a", "--kubeconfig", "config.yaml", "--wait", "--timeout", "5m0s"}},
		{[]string{"verify", "fabric"}, []string{"test", "fabric", "--logs", "--namespace", "network", "--kube-context", "infra-a", "--kubeconfig", "config.yaml", "--timeout", "5m0s"}},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			calls := 0
			a := App{ClientFactory: func(Options) (client.Client, error) {
				t.Fatal("Helm wrapper accessed Kubernetes directly")
				return nil, nil
			}, CommandRunner: func(_ context.Context, name string, args []string, dir string, env []string, _ io.Writer, _ io.Writer) error {
				calls++
				if name != "helm" || dir != "" || len(env) != 0 || !reflect.DeepEqual(args, test.expected) {
					t.Fatalf("unexpected command %s %q", name, args)
				}
				return nil
			}}
			args := append([]string{"--kubeconfig", "config.yaml", "--context", "infra-a", "-n", "network"}, test.args...)
			if err := a.Run(context.Background(), args); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("%d commands", calls)
			}
		})
	}
}

func TestHelmPlanHidesSecretsAndDoesNotUseLocalShell(t *testing.T) {
	var output bytes.Buffer
	value := "literal=$(not-a-command);value"
	a := App{Out: &output, CommandRunner: func(_ context.Context, name string, args []string, _ string, _ []string, _ io.Writer, _ io.Writer) error {
		if name != "helm" {
			t.Fatal(name)
		}
		joined := strings.Join(args, "\n")
		if !strings.Contains(joined, "--dry-run=server\n--hide-secret") || !strings.Contains(joined, "--set-string\n"+value) {
			t.Fatalf("wrong argv %q", args)
		}
		return nil
	}}
	if err := a.Run(context.Background(), []string{"plan", "fabric", "--chart", "./chart", "--set-string", value}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), value) {
		t.Fatal("wrapper logged a potentially sensitive value")
	}
}

func TestHelmInvalidOperationsDoNotExecute(t *testing.T) {
	for _, args := range [][]string{{"install", "fabric"}, {"rollback", "fabric", "0"}, {"rollback", "fabric", "-1"}, {"uninstall", "fabric", "--no-hooks"}, {"upgrade", "fabric", "--component", "unknown", "--version", "0.1.0"}, {"install", "fabric", "--version", "latest"}, {"admin", "native", "build"}} {
		a := App{CommandRunner: func(context.Context, string, []string, string, []string, io.Writer, io.Writer) error {
			t.Fatal("invalid arguments executed a command")
			return nil
		}}
		if err := a.Run(context.Background(), args); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
}
