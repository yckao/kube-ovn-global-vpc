package vpcctl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

// Native builds deliberately use the reviewed integration scripts rather than
// maintaining another copy of the upstream patch or test fixture adjustments.
const nativeBuildPatchSHA = "d338f05a6c4b7fa538637ee78a155a4ab2361d3b2e67ef665f359feaad5d9912"

var nativeBuildDigestPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[a-f0-9]{64}$`)
var nativeBuildTagPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*:[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$`)
var nativeBuildHashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type nativeBuildConfig struct {
	Target, Repository, Source, Go, BaseImage, Image, Output string
	DryRun                                                   bool
	integration, commit, goVersion, patchSHA                 string
	lock                                                     nativeBuildLock
}

type nativeBuildLock struct {
	Repository string            `json:"repository"`
	Commit     string            `json:"commit"`
	Tag        string            `json:"tag"`
	Files      map[string]string `json:"files"`
}

type nativeBuildStep struct {
	Program string   `json:"program"`
	Args    []string `json:"args"`
	Dir     string   `json:"directory,omitempty"`
}

func (a *App) runNativeBuild(ctx context.Context, args []string, opts Options) error {
	return a.runNativeBuildOnPlatform(ctx, args, opts, runtime.GOOS+"/"+runtime.GOARCH)
}

// The platform parameter allows command orchestration tests on a developer's
// workstation. The public command always supplies the actual runtime platform.
func (a *App) runNativeBuildOnPlatform(ctx context.Context, args []string, _ Options, platform string) error {
	c, err := parseNativeBuild(args, a.ErrOut)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if c.DryRun {
		return json.NewEncoder(nativeBuildWriter(a.Out)).Encode(c.plan())
	}
	if platform != "linux/amd64" {
		return fmt.Errorf("native build requires a Linux/amd64 build host to execute the native tests (current: %s); use --dry-run to inspect the plan", platform)
	}
	if err := os.Mkdir(c.Output, 0700); err != nil {
		return fmt.Errorf("create new build output: %w", err)
	}
	b := nativeBuildExecution{app: a, output: c.Output, env: c.environment()}
	evidence := map[string]any{
		"target": c.Target, "sourceCommit": c.commit, "patchSHA256": c.patchSHA,
		"platform": "linux/amd64", "status": "failed", "nativeUnitTests": "not-run",
		"realOVNNorthd": "not-run", "packetForwarding": "not-run", "deployed": false,
	}
	// Preserve a failed build's logs and explicit incomplete evidence for diagnosis.
	defer func() { _ = nativeBuildWriteJSON(filepath.Join(c.Output, "qualification.json"), evidence) }()
	version, err := b.run(ctx, c.Go, []string{"env", "GOVERSION"}, "")
	if err != nil {
		return err
	}
	if strings.TrimSpace(version) != c.goVersion {
		return fmt.Errorf("%s requires %s; %s reports %q", c.Target, c.goVersion, c.Go, strings.TrimSpace(version))
	}
	evidence["goVersion"] = c.goVersion
	source := filepath.Join(c.Output, "upstream")
	if c.Source != "" {
		if err := b.checkSource(ctx, c.Source, c.commit); err != nil {
			return err
		}
		if _, err := b.run(ctx, "git", []string{"clone", "--no-hardlinks", "--no-checkout", "--", c.Source, source}, ""); err != nil {
			return err
		}
	} else {
		for _, step := range []nativeBuildStep{
			{Program: "git", Args: []string{"init", source}},
			{Program: "git", Args: []string{"-C", source, "remote", "add", "origin", c.lock.Repository}},
			{Program: "git", Args: []string{"-C", source, "fetch", "--depth", "1", "origin", c.commit}},
		} {
			if _, err := b.run(ctx, step.Program, step.Args, step.Dir); err != nil {
				return err
			}
		}
	}
	if _, err := b.run(ctx, "git", []string{"-C", source, "checkout", "--detach", c.commit}, ""); err != nil {
		return err
	}
	if err := b.checkSource(ctx, source, c.commit); err != nil {
		return err
	}
	apply := filepath.Join(c.integration, "scripts", "apply.py")
	if _, err := b.run(ctx, "python3", []string{apply, source, "--check"}, ""); err != nil {
		return err
	}
	dependencies := map[string]string{}
	for _, name := range []string{"go.mod", "go.sum"} {
		dependencies[name], err = nativeBuildSHA(filepath.Join(source, name))
		if err != nil {
			return err
		}
	}
	// Both source-locked Python builders copy the libovsdb replacement from
	// its extracted module directory. A cold `go list -m -json` has no Dir until
	// the exact locked module has been downloaded, so populate it explicitly.
	if _, err := b.run(ctx, c.Go, []string{"mod", "download", "github.com/ovn-kubernetes/libovsdb"}, source); err != nil {
		return fmt.Errorf("fetch pinned native test dependency before build: %w", err)
	}
	build := filepath.Join(c.Output, "build")
	binary := filepath.Join(build, "kube-ovn-controller")
	patched := source
	if c.Target == "v1.16.3" {
		if _, err := b.run(ctx, "python3", []string{filepath.Join(c.integration, "scripts", "build.py"), source, "--go", c.Go, "--output", build}, ""); err != nil {
			return err
		}
		patched = filepath.Join(build, "production-source")
		tmp := filepath.Join(build, "test-tmp")
		if err := os.Mkdir(tmp, 0700); err != nil {
			return err
		}
		b.env = append(b.env, "TMPDIR="+tmp)
		for _, test := range []struct{ name, pattern string }{
			{"ovs", "^TestNativeDestinationRoutes$"},
			{"controller", "^TestDestinationRoute(Validation|OrphanGuardGC)$"},
			{"destinationroute", ".*"},
		} {
			if _, err := b.run(ctx, filepath.Join(build, test.name+".test"), []string{"-test.run", test.pattern, "-test.v"}, build); err != nil {
				return fmt.Errorf("native %s tests failed; no image was pushed: %w", test.name, err)
			}
		}
	} else {
		if err := os.Mkdir(build, 0700); err != nil {
			return err
		}
		if _, err := b.run(ctx, "python3", []string{apply, source}, ""); err != nil {
			return err
		}
		// --full uses Linux test code and isolates all test-only dependency fixes.
		if _, err := b.run(ctx, "python3", []string{filepath.Join(c.integration, "scripts", "test-portable.py"), source, "--go", c.Go, "--full"}, ""); err != nil {
			return fmt.Errorf("native tests failed; no image was pushed: %w", err)
		}
		flags := "-w -s -extldflags '-z now' -X github.com/kubeovn/kube-ovn/versions.COMMIT=git-" + c.commit + "+globalvpc-" + c.patchSHA[:12] +
			" -X github.com/kubeovn/kube-ovn/versions.VERSION=" + c.Target + "-globalvpc.v1" +
			" -X github.com/kubeovn/kube-ovn/versions.BUILDDATE=" + time.Now().UTC().Format("2006-01-02_15:04:05")
		if _, err := b.run(ctx, c.Go, []string{"build", "-mod=readonly", "-trimpath", "-buildmode=pie", "-ldflags", flags, "-o", binary, "./cmd/controller"}, patched); err != nil {
			return err
		}
	}
	evidence["nativeUnitTests"] = "passed"
	for name, before := range dependencies {
		after, err := nativeBuildSHA(filepath.Join(patched, name))
		if err != nil {
			return err
		}
		if after != before {
			return fmt.Errorf("production %s changed during build; refusing to package modified dependencies", name)
		}
	}
	evidence["productionDependencySHA256"] = dependencies
	binarySHA, err := nativeBuildSHA(binary)
	if err != nil {
		return err
	}
	evidence["binarySHA256"] = binarySHA
	if _, err := b.run(ctx, c.Go, []string{"version", "-m", binary}, ""); err != nil {
		return err
	}
	rendered, err := b.run(ctx, "helm", []string{"template", "native-schema", filepath.Join(patched, "charts", "kube-ovn")}, "")
	if err != nil {
		return err
	}
	schema, err := nativeBuildExtractSchema([]byte(rendered))
	if err != nil {
		return err
	}
	if !nativeJSONEqual(schema, nativeSchema()) {
		return errors.New("rendered destinationRoutes schema differs from the reviewed patch; refusing to publish an incompatible bundle")
	}
	imageDir := filepath.Join(c.Output, "image")
	if err := os.Mkdir(imageDir, 0700); err != nil {
		return err
	}
	if err := nativeBuildCopy(binary, filepath.Join(imageDir, "kube-ovn-controller")); err != nil {
		return err
	}
	dockerfile := fmt.Sprintf("ARG NATIVE_BASE\nFROM ${NATIVE_BASE}\n"+
		"LABEL org.opencontainers.image.source=\"https://github.com/yckao/kube-ovn-global-vpc\" \\\n"+
		"      org.opencontainers.image.version=%q \\\n"+
		"      org.opencontainers.image.revision=%q \\\n"+
		"      io.globalvpc.native.upstream.revision=%q \\\n"+
		"      io.globalvpc.native.patch.sha256=%q\n"+
		"COPY --chown=0:0 --chmod=0755 kube-ovn-controller /kube-ovn/kube-ovn-controller\n"+
		"RUN setcap cap_net_bind_service,cap_net_raw=eip /kube-ovn/kube-ovn-controller && getcap /kube-ovn/kube-ovn-controller\n",
		c.Target+"-globalvpc.v1", c.commit+"+globalvpc-"+c.patchSHA[:12], c.commit, c.patchSHA)
	if err := os.WriteFile(filepath.Join(imageDir, "Dockerfile"), []byte(dockerfile), 0600); err != nil {
		return err
	}
	metadata := filepath.Join(imageDir, "build-metadata.json")
	if _, err := b.run(ctx, "docker", []string{"buildx", "build", "--platform", "linux/amd64", "--provenance=mode=max", "--sbom=true", "--build-arg", "NATIVE_BASE=" + c.BaseImage, "--metadata-file", metadata, "-t", c.Image, "--push", imageDir}, ""); err != nil {
		return err
	}
	data, err := os.ReadFile(metadata)
	if err != nil {
		return err
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(data, &meta); err != nil {
		return fmt.Errorf("read image build metadata: %w", err)
	}
	var digest string
	if err := json.Unmarshal(meta["containerimage.digest"], &digest); err != nil || !strings.HasPrefix(digest, "sha256:") || !nativeBuildHashPattern.MatchString(strings.TrimPrefix(digest, "sha256:")) {
		return errors.New("image build did not return a valid SHA256 image digest")
	}
	image := c.Image[:strings.LastIndex(c.Image, ":")] + "@" + digest
	evidence["image"] = image
	for _, check := range []struct{ tool, expected string }{{"sha256sum", binarySHA}, {"getcap", "cap_net_bind_service,cap_net_raw=eip"}} {
		output, err := b.run(ctx, "docker", []string{"run", "--rm", "--platform", "linux/amd64", "--network", "none", "--entrypoint", check.tool, image, "/kube-ovn/kube-ovn-controller"}, "")
		if err != nil {
			return fmt.Errorf("image was pushed but verification failed; no install bundle was emitted: %w", err)
		}
		fields := strings.Fields(output)
		if (check.tool == "sha256sum" && (len(fields) != 2 || fields[0] != check.expected || fields[1] != "/kube-ovn/kube-ovn-controller")) ||
			(check.tool == "getcap" && (len(fields) != 2 || fields[0] != "/kube-ovn/kube-ovn-controller" || fields[1] != check.expected)) {
			return fmt.Errorf("image was pushed but %s verification failed; no install bundle was emitted", check.tool)
		}
	}
	for _, name := range []string{"source-lock.json", "native.patch"} {
		if err := nativeBuildCopy(filepath.Join(c.integration, name), filepath.Join(c.Output, name)); err != nil {
			return err
		}
	}
	bundle := map[string]any{
		"apiVersion": "vpcctl.globalvpc.io/v1alpha1", "kind": "NativeBundle", "target": c.Target,
		"sourceCommit": c.commit, "patchSHA256": c.patchSHA, "image": image, "baseImage": c.BaseImage,
		"platform": "linux/amd64", "binarySHA256": binarySHA, "schema": schema,
		"qualification": map[string]string{"nativeUnitTests": "passed", "realOVNNorthd": "not-run", "packetForwarding": "not-run"},
	}
	if err := nativeBuildWriteJSON(filepath.Join(c.Output, "bundle.json"), bundle); err != nil {
		return err
	}
	evidence["status"] = "built"
	fmt.Fprintf(nativeBuildWriter(a.Out), "Native bundle: %s\nImage: %s\nNative unit tests passed. Real OVN/northd and packet forwarding qualification have not been run.\n", filepath.Join(c.Output, "bundle.json"), image)
	return nil
}

func parseNativeBuild(args []string, stderr io.Writer) (nativeBuildConfig, error) {
	c := nativeBuildConfig{}
	fs := flag.NewFlagSet("vpcctl admin native build", flag.ContinueOnError)
	fs.SetOutput(nativeBuildWriter(stderr))
	fs.StringVar(&c.Target, "target", "", "Reviewed source target: v1.16.3 or v1.16.4 (required)")
	fs.StringVar(&c.Repository, "repository", ".", "Full global-vpc project source checkout")
	fs.StringVar(&c.Source, "source", "", "Clean pinned upstream Git checkout; otherwise fetch the locked commit")
	fs.StringVar(&c.Go, "go", "", "Absolute path to the matching Go binary (otherwise use PATH)")
	fs.StringVar(&c.BaseImage, "base-image", "", "Matching original native distribution image pinned by sha256 digest (required)")
	fs.StringVar(&c.Image, "image", "", "Registry image tag to build and push (required)")
	fs.StringVar(&c.Output, "output", "", "New output directory for source, logs, image metadata and bundle.json (required)")
	fs.BoolVar(&c.DryRun, "dry-run", false, "Print a local build plan without commands, network access, or writes")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() != 0 {
		return c, fmt.Errorf("unexpected native build argument: %s", fs.Arg(0))
	}
	switch c.Target {
	case "v1.16.3":
		c.commit, c.goVersion = "98af25ffae49193a8dc16bbc39bd8ca4110ec367", "go1.26.6"
	case "v1.16.4":
		c.commit, c.goVersion = "a9296ef2a37c6519bc0ecb798082ce139c72f8eb", "go1.27.1"
	default:
		return c, errors.New("--target must be v1.16.3 or v1.16.4")
	}
	if !nativeBuildDigestPattern.MatchString(c.BaseImage) {
		return c, errors.New("--base-image must be a native distribution image pinned as NAME@sha256:<64 lowercase hex digits>")
	}
	if !nativeBuildTagPattern.MatchString(c.Image) || strings.LastIndex(c.Image, ":") < strings.LastIndex(c.Image, "/") {
		return c, errors.New("--image must be a registry image name with an explicit tag")
	}
	if c.Output == "" {
		return c, errors.New("--output is required and must name a new directory")
	}
	var err error
	for _, path := range []*string{&c.Repository, &c.Output, &c.Source} {
		if *path != "" {
			*path, err = filepath.Abs(*path)
			if err != nil {
				return c, err
			}
		}
	}
	if _, err := os.Lstat(c.Output); !errors.Is(err, os.ErrNotExist) {
		return c, fmt.Errorf("--output must not already exist: %s", c.Output)
	}
	if st, err := os.Stat(filepath.Dir(c.Output)); err != nil || !st.IsDir() {
		return c, errors.New("--output parent directory must exist")
	}
	if c.Source != "" {
		if st, err := os.Stat(c.Source); err != nil || !st.IsDir() {
			return c, errors.New("--source must be an existing clean upstream Git checkout")
		}
	}
	if c.Go != "" && !filepath.IsAbs(c.Go) {
		return c, errors.New("--go must be an absolute executable path")
	}
	if c.Go == "" {
		c.Go, err = exec.LookPath("go")
		if err != nil {
			return c, fmt.Errorf("Go toolchain not found; provide --go /absolute/path/to/%s/bin/go", c.goVersion)
		}
		c.Go, err = filepath.Abs(c.Go)
		if err != nil {
			return c, err
		}
	}
	if st, err := os.Stat(c.Go); err != nil || st.IsDir() || st.Mode()&0111 == 0 {
		return c, fmt.Errorf("--go is not an executable file: %s", c.Go)
	}
	c.integration = filepath.Join(c.Repository, "integration", "kube-ovn")
	if c.Target == "v1.16.3" {
		c.integration = filepath.Join(c.integration, "compat-v1.16.3")
	}
	lock, err := os.ReadFile(filepath.Join(c.integration, "source-lock.json"))
	if err != nil {
		return c, fmt.Errorf("read source lock from --repository: %w", err)
	}
	if err := json.Unmarshal(lock, &c.lock); err != nil {
		return c, fmt.Errorf("invalid source lock: %w", err)
	}
	if c.lock.Commit != c.commit || c.lock.Tag != c.Target || c.lock.Repository != "https://github.com/kubeovn/kube-ovn" || len(c.lock.Files) == 0 {
		return c, errors.New("source lock does not match the reviewed upstream target")
	}
	for path, hash := range c.lock.Files {
		if !filepath.IsLocal(path) || !nativeBuildHashPattern.MatchString(hash) {
			return c, fmt.Errorf("invalid locked source path or SHA256: %s", path)
		}
	}
	c.patchSHA, err = nativeBuildSHA(filepath.Join(c.integration, "native.patch"))
	if err != nil {
		return c, err
	}
	if c.patchSHA != nativeBuildPatchSHA {
		return c, errors.New("native patch differs from the reviewed patch; review and update the vpcctl source contract first")
	}
	return c, nil
}

func (c nativeBuildConfig) environment() []string {
	return []string{"GOTOOLCHAIN=local", "GOWORK=off", "GOENV=off", "GOFLAGS=", "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64", "PATH=" + filepath.Dir(c.Go) + string(os.PathListSeparator) + os.Getenv("PATH")}
}

func (c nativeBuildConfig) plan() map[string]any {
	source, build := filepath.Join(c.Output, "upstream"), filepath.Join(c.Output, "build")
	steps := []nativeBuildStep{{Program: c.Go, Args: []string{"env", "GOVERSION"}}}
	if c.Source != "" {
		steps = append(steps,
			nativeBuildStep{Program: "git", Args: []string{"-C", c.Source, "rev-parse", "HEAD"}},
			nativeBuildStep{Program: "git", Args: []string{"-C", c.Source, "status", "--porcelain", "--untracked-files=all"}},
			nativeBuildStep{Program: "git", Args: []string{"clone", "--no-hardlinks", "--no-checkout", "--", c.Source, source}})
	} else {
		steps = append(steps,
			nativeBuildStep{Program: "git", Args: []string{"init", source}},
			nativeBuildStep{Program: "git", Args: []string{"-C", source, "remote", "add", "origin", c.lock.Repository}},
			nativeBuildStep{Program: "git", Args: []string{"-C", source, "fetch", "--depth", "1", "origin", c.commit}})
	}
	steps = append(steps,
		nativeBuildStep{Program: "git", Args: []string{"-C", source, "checkout", "--detach", c.commit}},
		nativeBuildStep{Program: "git", Args: []string{"-C", source, "rev-parse", "HEAD"}},
		nativeBuildStep{Program: "git", Args: []string{"-C", source, "status", "--porcelain", "--untracked-files=all"}},
		nativeBuildStep{Program: "python3", Args: []string{filepath.Join(c.integration, "scripts", "apply.py"), source, "--check"}},
		nativeBuildStep{Program: c.Go, Args: []string{"mod", "download", "github.com/ovn-kubernetes/libovsdb"}, Dir: source})
	patched := source
	if c.Target == "v1.16.3" {
		patched = filepath.Join(build, "production-source")
		steps = append(steps, nativeBuildStep{Program: "python3", Args: []string{filepath.Join(c.integration, "scripts", "build.py"), source, "--go", c.Go, "--output", build}})
		for _, test := range []struct{ name, pattern string }{{"ovs", "^TestNativeDestinationRoutes$"}, {"controller", "^TestDestinationRoute(Validation|OrphanGuardGC)$"}, {"destinationroute", ".*"}} {
			steps = append(steps, nativeBuildStep{Program: filepath.Join(build, test.name+".test"), Args: []string{"-test.run", test.pattern, "-test.v"}, Dir: build})
		}
	} else {
		steps = append(steps,
			nativeBuildStep{Program: "python3", Args: []string{filepath.Join(c.integration, "scripts", "apply.py"), source}},
			nativeBuildStep{Program: "python3", Args: []string{filepath.Join(c.integration, "scripts", "test-portable.py"), source, "--go", c.Go, "--full"}},
			nativeBuildStep{Program: c.Go, Args: []string{"build", "-mod=readonly", "-trimpath", "-buildmode=pie", "-ldflags", "<pinned source, patch and build timestamp metadata>", "-o", filepath.Join(build, "kube-ovn-controller"), "./cmd/controller"}, Dir: patched})
	}
	steps = append(steps,
		nativeBuildStep{Program: c.Go, Args: []string{"version", "-m", filepath.Join(build, "kube-ovn-controller")}},
		nativeBuildStep{Program: "helm", Args: []string{"template", "native-schema", filepath.Join(patched, "charts", "kube-ovn")}},
		nativeBuildStep{Program: "docker", Args: []string{"buildx", "build", "--platform", "linux/amd64", "--provenance=mode=max", "--sbom=true", "--build-arg", "NATIVE_BASE=" + c.BaseImage, "--metadata-file", filepath.Join(c.Output, "image", "build-metadata.json"), "-t", c.Image, "--push", filepath.Join(c.Output, "image")}})
	for _, tool := range []string{"sha256sum", "getcap"} {
		steps = append(steps, nativeBuildStep{Program: "docker", Args: []string{"run", "--rm", "--platform", "linux/amd64", "--network", "none", "--entrypoint", tool, "<image repository>@<returned SHA256 digest>", "/kube-ovn/kube-ovn-controller"}})
	}
	return map[string]any{
		"dryRun": true, "target": c.Target, "sourceCommit": c.commit, "patchSHA256": c.patchSHA,
		"requiredHost": "linux/amd64", "requiredGo": c.goVersion, "baseImage": c.BaseImage, "imageTagToPush": c.Image,
		"output": c.Output, "environment": c.environment(), "steps": steps,
		"localActions":  []string{"Create a new private output directory and retain command logs", "Verify clean Git source, exact source commit and locked original hashes", "Verify production go.mod/go.sum remain unchanged", "Extract only spec/status destinationRoutes from the rendered VPC CRD", "Write image Dockerfile with controller file capabilities", "Verify the pushed image's binary SHA256 and file capabilities", "Write source lock, patch, qualification.json and self-contained bundle.json"},
		"qualification": map[string]string{"nativeUnitTests": "pending execution", "realOVNNorthd": "not-run; environment-specific qualification required", "packetForwarding": "not-run"},
	}
}

type nativeBuildExecution struct {
	app    *App
	output string
	env    []string
	seq    int
}

func (b *nativeBuildExecution) run(ctx context.Context, name string, args []string, dir string) (string, error) {
	b.seq++
	log, err := os.OpenFile(filepath.Join(b.output, fmt.Sprintf("%02d-%s.log", b.seq, filepath.Base(name))), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	defer log.Close()
	var captured bytes.Buffer
	app := *b.app
	app.Out = io.MultiWriter(&captured, log)
	app.ErrOut = io.MultiWriter(nativeBuildWriter(b.app.ErrOut), log)
	fmt.Fprintf(nativeBuildWriter(b.app.ErrOut), "Native build: %s %s\n", name, strings.Join(args, " "))
	if err := app.command(ctx, name, args, dir, b.env); err != nil {
		return captured.String(), fmt.Errorf("%s failed (see %s): %w", name, log.Name(), err)
	}
	return captured.String(), nil
}

func (b *nativeBuildExecution) checkSource(ctx context.Context, source, commit string) error {
	head, err := b.run(ctx, "git", []string{"-C", source, "rev-parse", "HEAD"}, "")
	if err != nil {
		return err
	}
	if strings.TrimSpace(head) != commit {
		return fmt.Errorf("source HEAD does not match pinned commit %s", commit)
	}
	status, err := b.run(ctx, "git", []string{"-C", source, "status", "--porcelain", "--untracked-files=all"}, "")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) != "" {
		return errors.New("source checkout is not clean; use a clean pinned checkout")
	}
	return nil
}

func nativeBuildExtractSchema(data []byte) (map[string]any, error) {
	decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var found map[string]any
	for {
		var object map[string]any
		if err := decoder.Decode(&object); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("decode rendered chart: %w", err)
		}
		metadata, _ := object["metadata"].(map[string]any)
		if object["kind"] != "CustomResourceDefinition" || metadata["name"] != "vpcs.kubeovn.io" {
			continue
		}
		if found != nil {
			return nil, errors.New("rendered chart contains duplicate VPC CRDs")
		}
		found = object
	}
	if found == nil {
		return nil, errors.New("rendered chart has no VPC CRD")
	}
	spec, _ := found["spec"].(map[string]any)
	versions, _ := spec["versions"].([]any)
	if len(versions) != 1 {
		return nil, errors.New("rendered VPC CRD must have exactly one served/storage v1")
	}
	version, _ := versions[0].(map[string]any)
	if version["name"] != "v1" || version["served"] != true || version["storage"] != true {
		return nil, errors.New("rendered VPC CRD must have exactly one served/storage v1")
	}
	result := map[string]any{}
	for _, section := range []string{"spec", "status"} {
		var node any = version
		for _, key := range []string{"schema", "openAPIV3Schema", "properties", section, "properties", "destinationRoutes"} {
			object, _ := node.(map[string]any)
			node = object[key]
		}
		property, ok := node.(map[string]any)
		if !ok || len(property) == 0 {
			return nil, fmt.Errorf("rendered VPC CRD lacks %s.destinationRoutes schema", section)
		}
		result[section] = property
	}
	return result, nil
}

func nativeBuildSHA(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func nativeBuildCopy(source, destination string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(destination, data, 0600)
}

func nativeBuildWriteJSON(path string, data any) error {
	encoded, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0600)
}

func nativeBuildWriter(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}
