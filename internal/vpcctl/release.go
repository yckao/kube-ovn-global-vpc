package vpcctl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
)

// Release names immutable, project-built artifacts. Native bundles retain their
// own upstream source, base-image and qualification boundaries.
type Release struct {
	APIVersion      string                  `json:"apiVersion"`
	Kind            string                  `json:"kind"`
	Version         string                  `json:"version"`
	SourceCommit    string                  `json:"sourceCommit"`
	ControllerImage string                  `json:"controllerImage"`
	GatewayImage    string                  `json:"gatewayImage"`
	CLIImage        string                  `json:"cliImage,omitempty"`
	Charts          map[string]string       `json:"charts,omitempty"`
	NativeBundles   map[string]NativeBundle `json:"nativeBundles"`
}

var releaseVersion = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[a-zA-Z0-9][a-zA-Z0-9.-]*)?$`)
var releaseCommit = regexp.MustCompile(`^[a-f0-9]{40}$`)

func validateRelease(r Release) error {
	if r.APIVersion != nativeAPIVersion || r.Kind != "Release" || !releaseVersion.MatchString(r.Version) || !releaseCommit.MatchString(r.SourceCommit) {
		return fmt.Errorf("release requires the supported apiVersion/kind, a version and an exact source commit")
	}
	if !digestImage.MatchString(r.ControllerImage) || !digestImage.MatchString(r.GatewayImage) {
		return fmt.Errorf("release controllerImage and gatewayImage must use immutable SHA256 digests")
	}
	if r.CLIImage != "" && !digestImage.MatchString(r.CLIImage) {
		return fmt.Errorf("release cliImage must use an immutable SHA256 digest")
	}
	if len(r.NativeBundles) == 0 {
		return fmt.Errorf("release has no native extension bundles")
	}
	for target, bundle := range r.NativeBundles {
		if target != bundle.Target {
			return fmt.Errorf("release native bundle key does not match its target")
		}
		if err := validateNativeBundle(bundle); err != nil {
			return fmt.Errorf("release native %s: %w", target, err)
		}
		if !digestImage.MatchString(bundle.BaseImage) || !nativeBuildHashPattern.MatchString(bundle.BinarySHA256) || bundle.Qualification["nativeUnitTests"] != "passed" {
			return fmt.Errorf("release native %s is missing its base digest, binary hash or successful unit-test record", target)
		}
	}
	return nil
}

// loadRelease accepts trusted local descriptors, or checksum-verified HTTPS
// assets. Checksums detect corruption, not an independent publisher signature.
func (a *App) loadRelease(ctx context.Context, reference string) (Release, error) {
	var result Release
	expectedVersion := ""
	if releaseVersion.MatchString(reference) {
		expectedVersion = reference
		reference = "https://github.com/yckao/kube-ovn-global-vpc/releases/download/" + reference + "/release.json"
	}
	var data []byte
	var err error
	if strings.Contains(reference, "://") {
		u, e := url.Parse(reference)
		if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return result, fmt.Errorf("release URL must be HTTPS without credentials, query or fragment")
		}
		data, err = a.releaseDownload(ctx, u.String(), 4<<20)
		if err != nil {
			return result, err
		}
		checksumURL := *u
		checksumURL.Path = path.Join(path.Dir(u.Path), "SHA256SUMS")
		checksums, err := a.releaseDownload(ctx, checksumURL.String(), 2<<20)
		if err != nil {
			return result, fmt.Errorf("release checksum download failed: %w", err)
		}
		if err := verifyReleaseChecksum(data, checksums, path.Base(u.Path)); err != nil {
			return result, err
		}
	} else {
		if reference == "" {
			return result, fmt.Errorf("--release requires a version, HTTPS descriptor URL or local release.json")
		}
		f, e := os.Open(reference)
		if e != nil {
			return result, fmt.Errorf("read release descriptor: %w", e)
		}
		defer f.Close()
		data, err = io.ReadAll(io.LimitReader(f, (4<<20)+1))
		if len(data) > 4<<20 {
			return result, fmt.Errorf("release descriptor exceeds 4 MiB")
		}
	}
	if err != nil {
		return result, err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&result); err != nil {
		return result, fmt.Errorf("invalid release descriptor: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return result, fmt.Errorf("release descriptor must contain exactly one object")
	}
	if err := validateRelease(result); err != nil {
		return result, err
	}
	if expectedVersion != "" && result.Version != expectedVersion {
		return result, fmt.Errorf("downloaded release version differs from the requested version")
	}
	return result, nil
}

func (a *App) releaseDownload(ctx context.Context, location string, maximum int64) ([]byte, error) {
	c := a.HTTPClient
	if c == nil {
		c = &http.Client{Timeout: 45 * time.Second}
	}
	copyClient := *c
	previousRedirect := copyClient.CheckRedirect
	copyClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || req.URL.User != nil {
			return fmt.Errorf("insecure release redirect refused")
		}
		if len(via) >= 10 {
			return fmt.Errorf("too many release redirects")
		}
		if previousRedirect != nil {
			return previousRedirect(req, via)
		}
		return nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid release download URL")
	}
	request.Header.Set("User-Agent", "vpcctl")
	response, err := copyClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("release download failed; check network access and release availability")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release asset unavailable (HTTP %d); use a published version or a trusted local release.json", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maximum+1))
	if err != nil {
		return nil, fmt.Errorf("read release asset: %w", err)
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("release asset exceeds size limit")
	}
	return data, nil
}

func verifyReleaseChecksum(data, checksums []byte, filename string) error {
	var expected string
	for _, line := range strings.Split(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == filename {
			if expected != "" {
				return fmt.Errorf("duplicate release checksum entry")
			}
			expected = fields[0]
		}
	}
	actual := sha256.Sum256(data)
	if expected == "" || expected != hex.EncodeToString(actual[:]) {
		return fmt.Errorf("release descriptor SHA256 verification failed")
	}
	return nil
}

func (a *App) runRelease(ctx context.Context, args []string) error {
	if len(args) == 0 || hasHelp(args) {
		fmt.Fprintln(a.Out, "Usage: vpcctl release inspect VERSION|FILE|HTTPS_URL\nDownload and validate the prebuilt release descriptor. No cluster access or build tools are required.")
		return nil
	}
	if len(args) != 2 || args[0] != "inspect" {
		return fmt.Errorf("use release inspect VERSION|FILE|HTTPS_URL")
	}
	r, err := a.loadRelease(ctx, args[1])
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(a.Out, string(data))
	return err
}
