package vpcctl

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "globalvpc.io/controller/api/v1alpha2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestHelpAndVersionDoNotLoadCredentials(t *testing.T) {
	for _, args := range [][]string{
		{"--help"}, {"version"}, {"--version"}, {"admin", "--help"}, {"doctor", "--help"},
		{"vpc"}, {"subnet", "help"}, {"vpc", "create", "--help"}, {"subnet", "delete", "-h"},
		{"maintainer", "native", "--help"}, {"maintainer", "native", "-h"},
		{"maintainer", "native", "build", "--help"}, {"maintainer", "native", "plan", "--help"},
		{"maintainer", "native", "install", "--help"}, {"maintainer", "native", "status", "--help"},
		{"access", "issue", "--help"},
		{"install", "--help"}, {"rollback", "--help"}, {"uninstall", "--help"}, {"values", "--help"}, {"drain", "--help"},
	} {
		var out bytes.Buffer
		a := App{Out: &out, ErrOut: &out, Version: "test", ClientFactory: func(Options) (client.Client, error) {
			t.Fatal("help or version attempted API access")
			return nil, nil
		}}
		if err := a.Run(context.Background(), append([]string{"--kubeconfig", "/does/not/exist"}, args...)); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if out.Len() == 0 {
			t.Fatalf("%v produced no help", args)
		}
	}
}

func TestGlobalArgumentsAndNamespaceSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	config := `apiVersion: v1
kind: Config
current-context: first
contexts:
- name: first
  context: {cluster: example, namespace: project-a}
- name: second
  context: {cluster: example, namespace: project-b}
clusters:
- name: example
  cluster: {server: https://api.example.invalid}
`
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{{nil, "project-a"}, {[]string{"--context", "second"}, "project-b"}, {[]string{"--context", "second", "-n", "explicit"}, "explicit"}} {
		a := App{ClientFactory: func(opts Options) (client.Client, error) {
			if opts.Namespace != tc.want {
				t.Fatalf("namespace %q, want %q", opts.Namespace, tc.want)
			}
			return nil, fmt.Errorf("expected-test-stop")
		}}
		args := append([]string{"--kubeconfig", path}, tc.args...)
		if err := a.Run(context.Background(), append(args, "doctor")); err == nil || !strings.Contains(err.Error(), "expected-test-stop") {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != config {
		t.Fatal("context selection modified kubeconfig")
	}
}

func TestInvalidGlobalsFailBeforeAPI(t *testing.T) {
	for _, args := range [][]string{{"--timeout", "0s", "doctor"}, {"-o", "xml", "doctor"}, {"unknown"}, {"admin", "other"}} {
		a := App{ClientFactory: func(Options) (client.Client, error) {
			t.Fatal("invalid arguments attempted API access")
			return nil, nil
		}}
		if err := a.Run(context.Background(), args); err == nil {
			t.Fatalf("invalid args accepted: %v", args)
		}
	}
}

func TestDoctorUsesOnlySelectedProjectAndRejectsStaleReadiness(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	object := &api.VPC{ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: "project-a", Generation: 3}, Status: api.VPCStatus{ObservedGeneration: 3, Phase: "Ready", Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, ObservedGeneration: 2, Reason: "OldStatus"}}}}
	foreign := &api.VPC{ObjectMeta: metav1.ObjectMeta{Name: "private-other-project", Namespace: "project-b"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(object, foreign).Build()
	var out bytes.Buffer
	a := App{Out: &out, ClientFactory: func(Options) (client.Client, error) { return c, nil }}
	if err := a.Run(context.Background(), []string{"-n", "project-a", "-o", "json", "doctor"}); err == nil {
		t.Fatal("stale Ready condition passed doctor")
	}
	if !strings.Contains(out.String(), "StaleCondition") || strings.Contains(out.String(), "private-other-project") {
		t.Fatalf("unexpected report: %s", out.String())
	}
}

func TestDoctorTerminationOverridesReady(t *testing.T) {
	now := metav1.NewTime(time.Now())
	r := doctorObservation("VPC", metav1.ObjectMeta{Name: "network", Generation: 1, DeletionTimestamp: &now}, "Ready", 1, []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, ObservedGeneration: 1}})
	if r.Ready || r.Reason != "Terminating" {
		t.Fatalf("incorrect observation: %+v", r)
	}
}
