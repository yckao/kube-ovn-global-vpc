package vpcctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func nativeBuildTestArgs(t *testing.T, target string) []string {
	t.Helper()
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	goPath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return []string{"--target", target, "--repository", repo, "--go", goPath,
		"--base-image", "registry.example/kube-ovn@sha256:" + strings.Repeat("a", 64),
		"--image", "registry.example:5000/native:candidate", "--output", filepath.Join(t.TempDir(), "result")}
}

func nativeBuildTestFlag(args []string, name, value string) []string {
	copy := append([]string(nil), args...)
	for i := range copy {
		if copy[i] == name {
			copy[i+1] = value
			return copy
		}
	}
	return append(copy, name, value)
}

func nativeBuildTestValue(args []string, name string) string {
	for i := range args {
		if args[i] == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestNativeBuildDryRunHasNoCommandsOrWrites(t *testing.T) {
	for _, target := range []string{"v1.16.3", "v1.16.4"} {
		t.Run(target, func(t *testing.T) {
			args := append(nativeBuildTestArgs(t, target), "--dry-run")
			var output strings.Builder
			app := App{Out: &output, CommandRunner: func(context.Context, string, []string, string, []string, io.Writer, io.Writer) error {
				t.Fatal("dry-run executed an external command")
				return nil
			}}
			if err := app.runNativeBuildOnPlatform(context.Background(), args, Options{}, "darwin/arm64"); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(nativeBuildTestValue(args, "--output")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("dry-run created output: %v", err)
			}
			var plan map[string]any
			if err := json.Unmarshal([]byte(output.String()), &plan); err != nil {
				t.Fatal(err)
			}
			if plan["dryRun"] != true || plan["requiredHost"] != "linux/amd64" || !strings.Contains(output.String(), "--push") || !strings.Contains(output.String(), "not-run") {
				t.Fatalf("incomplete plan: %s", output.String())
			}
		})
	}
}

func TestNativeBuildHelpNeedsNoRepositoryOrToolchain(t *testing.T) {
	var output strings.Builder
	app := App{ErrOut: &output}
	if err := app.runNativeBuild(context.Background(), []string{"--help"}, Options{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "--") && !strings.Contains(output.String(), "-target") {
		t.Fatalf("missing help: %s", output.String())
	}
}

func TestNativeBuildRejectsInvalidInputsBeforeCommands(t *testing.T) {
	for _, test := range []struct{ name, flag, value, want string }{
		{"target", "--target", "v1.17", "--target"},
		{"mutable-base", "--base-image", "registry/native:latest", "--base-image"},
		{"bad-digest", "--base-image", "registry/native@sha256:" + strings.Repeat("z", 64), "--base-image"},
		{"command-in-image", "--image", "registry/native:tag;echo surprise", "--image"},
		{"port-is-not-tag", "--image", "registry:5000/native", "--image"},
		{"go-relative", "--go", "./go", "absolute"},
		{"go-missing", "--go", "/does/not/exist/go", "executable"},
		{"repository-missing", "--repository", "/does/not/exist", "source lock"},
		{"source-missing", "--source", "/does/not/exist", "--source"},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := nativeBuildTestFlag(nativeBuildTestArgs(t, "v1.16.3"), test.flag, test.value)
			app := App{CommandRunner: func(context.Context, string, []string, string, []string, io.Writer, io.Writer) error {
				t.Fatal("invalid input executed a command")
				return nil
			}}
			if err := app.runNativeBuildOnPlatform(context.Background(), args, Options{}, "linux/amd64"); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("want %q, got %v", test.want, err)
			}
		})
	}
	t.Run("existing-output", func(t *testing.T) {
		args := nativeBuildTestFlag(nativeBuildTestArgs(t, "v1.16.3"), "--output", t.TempDir())
		if _, err := parseNativeBuild(args, io.Discard); err == nil || !strings.Contains(err.Error(), "already exist") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("unsupported-host", func(t *testing.T) {
		args := nativeBuildTestArgs(t, "v1.16.3")
		if err := (&App{}).runNativeBuildOnPlatform(context.Background(), args, Options{}, "darwin/arm64"); err == nil || !strings.Contains(err.Error(), "Linux/amd64") {
			t.Fatalf("got %v", err)
		}
		if _, err := os.Lstat(nativeBuildTestValue(args, "--output")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("unsupported host wrote output")
		}
	})
}

// This fixture exercises orchestration and artifact validation, not the native
// controller or Docker. Real qualification remains explicitly not-run.
type nativeBuildFake struct {
	t       *testing.T
	c       nativeBuildConfig
	steps   []nativeBuildStep
	fail    string
	badGo   bool
	dirty   bool
	badHead bool
	badCaps bool
	badDeps bool
}

func (f *nativeBuildFake) command(_ context.Context, name string, args []string, dir string, env []string, stdout, _ io.Writer) error {
	f.t.Helper()
	f.steps = append(f.steps, nativeBuildStep{Program: name, Args: append([]string(nil), args...), Dir: dir})
	if name == "sh" || name == "bash" {
		f.t.Fatalf("unexpected shell invocation: %s %v", name, args)
	}
	for _, required := range []string{"GOTOOLCHAIN=local", "GOWORK=off", "GOENV=off", "GOFLAGS=", "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64"} {
		found := false
		for _, item := range env {
			found = found || item == required
		}
		if !found {
			f.t.Errorf("missing build environment %s", required)
		}
	}
	if f.fail != "" && strings.Contains(name+" "+strings.Join(args, " "), f.fail) {
		return errors.New("injected failure")
	}
	write := func(path, data string) {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			f.t.Fatal(err)
		}
	}
	if name == f.c.Go && reflect.DeepEqual(args, []string{"env", "GOVERSION"}) {
		version := f.c.goVersion
		if f.badGo {
			version = "go1.99.0"
		}
		fmt.Fprintln(stdout, version)
	}
	if name == "git" && len(args) > 2 && args[2] == "rev-parse" {
		commit := f.c.commit
		if f.badHead {
			commit = strings.Repeat("0", 40)
		}
		fmt.Fprintln(stdout, commit)
	}
	if name == "git" && len(args) > 2 && args[2] == "status" && f.dirty {
		fmt.Fprintln(stdout, " M go.mod")
	}
	if name == "git" && ((len(args) > 0 && args[0] == "init") || (len(args) > 0 && args[0] == "clone")) {
		source := args[len(args)-1]
		write(filepath.Join(source, "go.mod"), "module production\n")
		write(filepath.Join(source, "go.sum"), "original dependencies\n")
	}
	if name == "python3" && strings.HasSuffix(args[0], "build.py") {
		build := nativeBuildTestValue(args, "--output")
		write(filepath.Join(build, "production-source", "go.mod"), "module production\n")
		write(filepath.Join(build, "production-source", "go.sum"), "original dependencies\n")
		write(filepath.Join(build, "kube-ovn-controller"), "production binary\n")
		if f.badDeps {
			write(filepath.Join(build, "production-source", "go.mod"), "changed production dependencies\n")
		}
	}
	if name == f.c.Go && len(args) > 0 && args[0] == "build" {
		write(nativeBuildTestValue(args, "-o"), "production binary\n")
		if f.badDeps {
			write(filepath.Join(dir, "go.mod"), "changed production dependencies\n")
		}
	}
	if name == "helm" {
		fmt.Fprint(stdout, nativeBuildTestChart())
	}
	if name == "docker" && args[0] == "buildx" {
		write(nativeBuildTestValue(args, "--metadata-file"), `{"containerimage.digest":"sha256:`+strings.Repeat("b", 64)+`"}`)
	}
	if name == "docker" && args[0] == "run" {
		if nativeBuildTestValue(args, "--entrypoint") == "sha256sum" {
			hash, err := nativeBuildSHA(filepath.Join(f.c.Output, "build", "kube-ovn-controller"))
			if err != nil {
				f.t.Fatal(err)
			}
			fmt.Fprintln(stdout, hash+"  /kube-ovn/kube-ovn-controller")
		} else if !f.badCaps {
			fmt.Fprintln(stdout, "/kube-ovn/kube-ovn-controller cap_net_bind_service,cap_net_raw=eip")
		}
	}
	return nil
}

func nativeBuildTestChart() string {
	schema := nativeSchema()
	object := map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": "vpcs.kubeovn.io"},
		"spec": map[string]any{"versions": []any{map[string]any{
			"name": "v1", "served": true, "storage": true,
			"schema": map[string]any{"openAPIV3Schema": map[string]any{"properties": map[string]any{
				"spec":   map[string]any{"properties": map[string]any{"destinationRoutes": schema.Spec, "unrelated": map[string]any{"type": "string"}}},
				"status": map[string]any{"properties": map[string]any{"destinationRoutes": schema.Status}},
			}}},
		}}},
	}
	data, _ := json.Marshal(object)
	return string(data)
}

func TestNativeBuildProducesImmutableBundleAndPreservesEvidence(t *testing.T) {
	for _, target := range []string{"v1.16.3", "v1.16.4"} {
		t.Run(target, func(t *testing.T) {
			args := nativeBuildTestArgs(t, target)
			// Spaces and shell metacharacters remain literal paths, never shell code.
			source := filepath.Join(t.TempDir(), "source with spaces;$(do-not-execute)")
			if err := os.Mkdir(source, 0700); err != nil {
				t.Fatal(err)
			}
			args = append(args, "--source", source)
			c, err := parseNativeBuild(args, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			fake := nativeBuildFake{t: t, c: c}
			var output strings.Builder
			app := App{Out: &output, CommandRunner: fake.command}
			if err := app.runNativeBuildOnPlatform(context.Background(), args, Options{}, "linux/amd64"); err != nil {
				t.Fatal(err)
			}
			var bundle map[string]any
			data, err := os.ReadFile(filepath.Join(c.Output, "bundle.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &bundle); err != nil {
				t.Fatal(err)
			}
			var installBundle NativeBundle
			if err := json.Unmarshal(data, &installBundle); err != nil {
				t.Fatal(err)
			}
			if err := validateNativeBundle(installBundle); err != nil {
				t.Fatalf("build produced a bundle rejected by native install: %v", err)
			}
			wantImage := "registry.example:5000/native@sha256:" + strings.Repeat("b", 64)
			if bundle["image"] != wantImage || bundle["sourceCommit"] != c.commit || bundle["baseImage"] != c.BaseImage {
				t.Fatalf("wrong bundle: %s", data)
			}
			qualification := bundle["qualification"].(map[string]any)
			if qualification["nativeUnitTests"] != "passed" || qualification["realOVNNorthd"] != "not-run" || qualification["packetForwarding"] != "not-run" {
				t.Fatalf("misleading qualification: %v", qualification)
			}
			schema := bundle["schema"].(map[string]any)
			if len(schema) != 2 || schema["spec"].(map[string]any)["type"] != "array" {
				t.Fatalf("unexpected schema: %v", schema)
			}
			for _, step := range fake.steps {
				if step.Program == "python3" && strings.HasSuffix(step.Args[0], "apply.py") && step.Args[1] == source {
					t.Fatal("mutated user-supplied source")
				}
			}
			dockerfile, err := os.ReadFile(filepath.Join(c.Output, "image", "Dockerfile"))
			if err != nil || !strings.Contains(string(dockerfile), "setcap cap_net_bind_service,cap_net_raw=eip") {
				t.Fatalf("capabilities not restored: %s %v", dockerfile, err)
			}
		})
	}
}

func TestNativeBuildFailuresDoNotEmitInstallBundle(t *testing.T) {
	for _, test := range []struct {
		name, target, fail, want                string
		badGo, dirty, badHead, badCaps, badDeps bool
	}{
		{name: "wrong-go", target: "v1.16.3", badGo: true, want: "requires go1.26.6"},
		{name: "dirty-source", target: "v1.16.3", dirty: true, want: "not clean"},
		{name: "wrong-head", target: "v1.16.3", badHead: true, want: "pinned commit"},
		{name: "compat-tests", target: "v1.16.3", fail: "ovs.test", want: "tests failed"},
		{name: "native-tests", target: "v1.16.4", fail: "test-portable.py", want: "tests failed"},
		{name: "capabilities", target: "v1.16.3", badCaps: true, want: "getcap verification failed"},
		{name: "compat-dependencies", target: "v1.16.3", badDeps: true, want: "production go.mod changed"},
		{name: "native-dependencies", target: "v1.16.4", badDeps: true, want: "production go.mod changed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := nativeBuildTestArgs(t, test.target)
			c, err := parseNativeBuild(args, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			fake := nativeBuildFake{t: t, c: c, fail: test.fail, badGo: test.badGo, dirty: test.dirty, badHead: test.badHead, badCaps: test.badCaps, badDeps: test.badDeps}
			app := App{CommandRunner: fake.command}
			if err := app.runNativeBuildOnPlatform(context.Background(), args, Options{}, "linux/amd64"); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("want %q, got %v", test.want, err)
			}
			if _, err := os.Stat(filepath.Join(c.Output, "bundle.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed build emitted install bundle")
			}
			if !test.badCaps {
				for _, step := range fake.steps {
					if step.Program == "docker" {
						t.Fatalf("failure still reached Docker: %v", step)
					}
				}
			}
			data, err := os.ReadFile(filepath.Join(c.Output, "qualification.json"))
			if err != nil || !strings.Contains(string(data), `"status": "failed"`) {
				t.Fatalf("missing failed qualification record: %s %v", data, err)
			}
		})
	}
}

func TestNativeBuildRejectsUnreviewedPatchOrSourceLock(t *testing.T) {
	for _, mutation := range []string{"patch", "hash", "commit", "path"} {
		t.Run(mutation, func(t *testing.T) {
			args := nativeBuildTestArgs(t, "v1.16.3")
			c, err := parseNativeBuild(args, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			repo := t.TempDir()
			integration := filepath.Join(repo, "integration", "kube-ovn", "compat-v1.16.3")
			if err := os.MkdirAll(integration, 0700); err != nil {
				t.Fatal(err)
			}
			if err := nativeBuildCopy(filepath.Join(c.integration, "native.patch"), filepath.Join(integration, "native.patch")); err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "patch":
				if err := os.WriteFile(filepath.Join(integration, "native.patch"), []byte("unreviewed patch"), 0600); err != nil {
					t.Fatal(err)
				}
			case "hash":
				c.lock.Files["go.mod"] = "invalid"
			case "commit":
				c.lock.Commit = strings.Repeat("0", 40)
			case "path":
				c.lock.Files["../escape"] = strings.Repeat("0", 64)
			}
			if err := nativeBuildWriteJSON(filepath.Join(integration, "source-lock.json"), c.lock); err != nil {
				t.Fatal(err)
			}
			args = nativeBuildTestFlag(args, "--repository", repo)
			if _, err := parseNativeBuild(args, io.Discard); err == nil {
				t.Fatalf("accepted %s mutation", mutation)
			}
		})
	}
}

func TestNativeBuildExtractSchemaRejectsMissingAndDuplicateCRDs(t *testing.T) {
	for _, input := range []string{"{}", nativeBuildTestChart() + "\n---\n" + nativeBuildTestChart(), strings.Replace(nativeBuildTestChart(), `"destinationRoutes":`, `"missing":`, 1)} {
		if _, err := nativeBuildExtractSchema([]byte(input)); err == nil {
			t.Fatal("accepted incomplete/ambiguous schema")
		}
	}
}
