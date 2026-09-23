package vpcctl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type releaseTransport func(*http.Request) (*http.Response, error)

func (f releaseTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func releaseFixture() Release {
	b := nativeTestBundle()
	b.BinarySHA256 = strings.Repeat("c", 64)
	b.Qualification = map[string]string{"nativeUnitTests": "passed", "realOVNNorthd": "not-run", "packetForwarding": "not-run"}
	return Release{APIVersion: nativeAPIVersion, Kind: "Release", Version: "v0.1.0", SourceCommit: strings.Repeat("a", 40), ControllerImage: "ghcr.io/example/controller@sha256:" + strings.Repeat("b", 64), GatewayImage: "ghcr.io/example/gateway@sha256:" + strings.Repeat("c", 64), CLIImage: "ghcr.io/example/vpcctl@sha256:" + strings.Repeat("d", 64), NativeBundles: map[string]NativeBundle{b.Target: b}}
}
func TestReleaseDownloadChecksHashVersionAndImagePins(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tamper    bool
		version   string
		wantError bool
	}{{"valid", false, "v0.1.0", false}, {"hash mismatch", true, "v0.1.0", true}, {"version mismatch", false, "v0.2.0", true}} {
		t.Run(tc.name, func(t *testing.T) {
			r := releaseFixture()
			r.Version = tc.version
			data, _ := json.Marshal(r)
			sum := sha256.Sum256(data)
			checksums := hex.EncodeToString(sum[:]) + "  release.json\n"
			if tc.tamper {
				data = append(data, ' ')
			}
			a := App{HTTPClient: &http.Client{Transport: releaseTransport(func(req *http.Request) (*http.Response, error) {
				if req.URL.Scheme != "https" || req.URL.Host != "github.com" || req.Header.Get("Authorization") != "" {
					t.Fatal("unexpected release request")
				}
				body := string(data)
				if strings.HasSuffix(req.URL.Path, "SHA256SUMS") {
					body = checksums
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
			})}}
			_, err := a.loadRelease(context.Background(), "v0.1.0")
			if (err != nil) != tc.wantError {
				t.Fatalf("unexpected %v", err)
			}
		})
	}
}
func TestReleaseRejectsUnpublishedMutableAndUnqualifiedInputs(t *testing.T) {
	for _, change := range []func(*Release){func(r *Release) { r.ControllerImage = "repo:latest" }, func(r *Release) {
		b := r.NativeBundles["v1.16.3"]
		b.Qualification["nativeUnitTests"] = "not-run"
		r.NativeBundles["v1.16.3"] = b
	}, func(r *Release) { r.SourceCommit = "main" }} {
		r := releaseFixture()
		change(&r)
		if err := validateRelease(r); err == nil {
			t.Fatal("accepted unqualified release")
		}
	}
	a := App{HTTPClient: &http.Client{Transport: releaseTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("not found"))}, nil
	})}}
	if _, err := a.loadRelease(context.Background(), "v9.9.9"); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("unexpected %v", err)
	}
	for _, ref := range []string{"http://example.test/release.json", "https://user:password@example.test/release.json", "https://example.test/release.json?token=secret"} {
		if _, err := a.loadRelease(context.Background(), ref); err == nil {
			t.Fatal("accepted unsafe URL")
		}
	}
}
func TestReleaseLocalDescriptorNeedsNoToolsOrNetwork(t *testing.T) {
	r := releaseFixture()
	data, _ := json.Marshal(r)
	file := filepath.Join(t.TempDir(), "release.json")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	a := App{HTTPClient: &http.Client{Transport: releaseTransport(func(*http.Request) (*http.Response, error) { return nil, fmt.Errorf("network forbidden") })}}
	got, err := a.loadRelease(context.Background(), file)
	if err != nil || got.Version != r.Version {
		t.Fatalf("%+v %v", got, err)
	}
}
