package provider

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
	"time"

	v1alpha1 "globalvpc.io/controller/api/v1alpha1"
)

const secret = "test-provider-secret-must-never-leak"

var testClaim = v1alpha1.PrefixClaim{ClaimUID: "vpc-uid/infra-uid", VpcUID: "vpc-uid", ScopeRef: "vrf:42", CIDR: "10.24.1.0/24"}

func helper(t *testing.T, mode string, args ...string) Exec {
	t.Helper()
	t.Setenv("GLOBALVPC_PROVIDER_TEST", "1")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := append([]string{binary, "-test.run=^TestProviderProcess$", "--", mode}, args...)
	return Exec{Command: command, Timeout: 3 * time.Second}
}

func TestEnsureSuccess(t *testing.T) {
	for _, claim := range []v1alpha1.PrefixClaim{testClaim, {ClaimUID: "ipv6-claim", VpcUID: "ipv6-vpc", ScopeRef: "vrf:43", CIDR: "2001:db8::/64"}} {
		t.Run(claim.CIDR, func(t *testing.T) {
			p := helper(t, "success")
			allocation, err := p.Ensure(context.Background(), claim)
			if err != nil {
				t.Fatal(err)
			}
			want := v1alpha1.Allocation{PrefixClaim: claim, AllocationID: "prefix-42", ReleasePolicy: "Retain"}
			if !reflect.DeepEqual(allocation, want) {
				t.Fatalf("allocation = %#v, want %#v", allocation, want)
			}
		})
	}
}

func TestLostResponseRediscovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "committed-claim.json")
	p := helper(t, "commit-then-exit", path)
	_, err := p.Ensure(context.Background(), testClaim)
	if !errors.Is(err, ErrUnknownState) {
		t.Fatalf("expected unknown allocation state, got %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("provider never committed its claim: %v", err)
	}
	p = helper(t, "rediscover", path)
	allocation, err := p.Ensure(context.Background(), testClaim)
	if err != nil || allocation.AllocationID != "prefix-42" || allocation.PrefixClaim != testClaim {
		t.Fatalf("cannot rediscover the committed claim: %#v, %v", allocation, err)
	}
}

func TestEnsureRejectsUntrustedResponses(t *testing.T) {
	for _, mode := range []string{
		"malformed", "trailing-json", "null", "array", "wrong-version", "missing-ok", "null-ok", "string-ok", "rejected",
		"missing-result", "null-result", "array-result", "string-result", "wrong-claim", "wrong-vpc", "wrong-scope",
		"wrong-cidr", "noncanonical-cidr", "missing-identity", "typed-identity", "empty-allocation", "null-allocation", "typed-allocation", "wrong-policy", "missing-policy",
	} {
		t.Run(mode, func(t *testing.T) {
			_, err := helper(t, mode).Ensure(context.Background(), testClaim)
			if err == nil || !errors.Is(err, ErrUnknownState) {
				t.Fatalf("expected sanitized unknown-state error, got %v", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("provider response leaked in error: %v", err)
			}
		})
	}
}

func TestTimeoutAndCancellation(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		p := helper(t, "sleep")
		p.Timeout = 60 * time.Millisecond
		start := time.Now()
		_, err := p.Ensure(context.Background(), testClaim)
		if !errors.Is(err, ErrUnknownState) || time.Since(start) > 2*time.Second {
			t.Fatalf("provider timeout not bounded: %v, elapsed %s", err, time.Since(start))
		}
	})
	t.Run("cancelled-parent", func(t *testing.T) {
		p := helper(t, "sleep")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		start := time.Now()
		_, err := p.Ensure(ctx, testClaim)
		if !errors.Is(err, ErrUnknownState) || time.Since(start) > time.Second {
			t.Fatalf("parent cancellation not honored: %v", err)
		}
	})
}

func TestOversizedResponse(t *testing.T) {
	p := helper(t, "oversized")
	start := time.Now()
	_, err := p.Ensure(context.Background(), testClaim)
	if !errors.Is(err, ErrUnknownState) || !strings.Contains(err.Error(), "size limit") || time.Since(start) > 2*time.Second {
		t.Fatalf("oversized output was not terminated promptly: %v", err)
	}
	output := &limitedOutput{cancel: func() {}}
	_, _ = output.Write(make([]byte, maxOutputBytes+1))
	if output.Len() != maxOutputBytes || !output.exceeded {
		t.Fatalf("output exceeded retained byte limit: %d", output.Len())
	}
}

func TestErrorsDoNotExposeSecrets(t *testing.T) {
	for _, mode := range []string{"crash", "rejected", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			_, err := helper(t, mode, secret).Ensure(context.Background(), testClaim)
			if err == nil || strings.Contains(err.Error(), secret) {
				t.Fatalf("unsanitized subprocess error: %v", err)
			}
		})
	}
	p := Exec{Command: []string{filepath.Join(t.TempDir(), secret)}}
	_, err := p.Ensure(context.Background(), testClaim)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("executable path leaked in error: %v", err)
	}
}

func TestNoShellExpansion(t *testing.T) {
	// The helper validates the exact argument, including shell metacharacters.
	p := helper(t, "literal-argument", "$(printf unexpected); `printf unexpected` *")
	if _, err := p.Ensure(context.Background(), testClaim); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidInputNeverExecutes(t *testing.T) {
	for _, change := range []func(*v1alpha1.PrefixClaim){
		func(c *v1alpha1.PrefixClaim) { c.ClaimUID = "" },
		func(c *v1alpha1.PrefixClaim) { c.VpcUID = " " },
		func(c *v1alpha1.PrefixClaim) { c.ScopeRef = "" },
		func(c *v1alpha1.PrefixClaim) { c.CIDR = "10.24.1.1/24" },
		func(c *v1alpha1.PrefixClaim) { c.CIDR = "not-a-cidr" },
		func(c *v1alpha1.PrefixClaim) { c.CIDR = "2001:0db8:0:0::/64" },
	} {
		claim := testClaim
		change(&claim)
		p := Exec{Command: []string{filepath.Join(t.TempDir(), "does-not-exist")}}
		_, err := p.Ensure(context.Background(), claim)
		if err == nil || errors.Is(err, ErrUnknownState) {
			t.Fatalf("invalid claim should fail before execution: %v", err)
		}
	}
	for _, command := range [][]string{nil, {}, {""}, {" "}} {
		if _, err := (Exec{Command: command}).Ensure(context.Background(), testClaim); err == nil {
			t.Fatal("empty command accepted")
		}
	}
}

// TestProviderProcess is only executed in child test binaries. It checks the
// request contract before simulating an external, potentially faulty provider.
func TestProviderProcess(t *testing.T) {
	if os.Getenv("GLOBALVPC_PROVIDER_TEST") != "1" {
		return
	}
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		os.Exit(10)
	}
	args := os.Args[separator+1:]
	var req request
	if json.NewDecoder(os.Stdin).Decode(&req) != nil || req.APIVersion != apiVersion || req.Operation != "EnsurePrefix" || !validClaim(req.PrefixClaim) {
		os.Exit(11)
	}
	allocation := map[string]any{"claimUID": req.ClaimUID, "vpcUID": req.VpcUID, "scopeRef": req.ScopeRef, "cidr": req.CIDR, "allocationID": "prefix-42", "releasePolicy": "Retain"}
	response := map[string]any{"apiVersion": apiVersion, "ok": true, "result": allocation}
	switch args[0] {
	case "success":
	case "literal-argument":
		if len(args) != 2 || args[1] != "$(printf unexpected); `printf unexpected` *" {
			os.Exit(12)
		}
	case "commit-then-exit":
		body, _ := json.Marshal(req)
		if len(args) != 2 || os.WriteFile(args[1], body, 0600) != nil {
			os.Exit(13)
		}
		os.Exit(2)
	case "rediscover":
		body, err := os.ReadFile(args[1])
		var committed request
		if err != nil || json.Unmarshal(body, &committed) != nil || committed != req {
			os.Exit(14)
		}
	case "crash":
		fmt.Fprint(os.Stderr, secret)
		os.Exit(2)
	case "sleep":
		time.Sleep(10 * time.Second)
		os.Exit(15)
	case "oversized":
		_, _ = io.CopyN(os.Stdout, endlessReader{}, maxOutputBytes*4)
		time.Sleep(10 * time.Second)
		os.Exit(16)
	case "malformed":
		fmt.Fprint(os.Stdout, "{invalid:"+secret)
		os.Exit(0)
	case "trailing-json":
		fmt.Fprint(os.Stdout, "{} {}")
		os.Exit(0)
	case "null", "array":
		if args[0] == "null" {
			fmt.Fprint(os.Stdout, "null")
		} else {
			fmt.Fprint(os.Stdout, "[]")
		}
		os.Exit(0)
	case "wrong-version":
		response["apiVersion"] = secret
	case "missing-ok":
		delete(response, "ok")
	case "null-ok":
		response["ok"] = nil
	case "string-ok":
		response["ok"] = "true"
	case "rejected":
		response["ok"] = false
		response["error"] = secret
	case "missing-result":
		delete(response, "result")
	case "null-result":
		response["result"] = nil
	case "array-result":
		response["result"] = []string{secret}
	case "string-result":
		response["result"] = secret
	case "wrong-claim":
		allocation["claimUID"] = secret
	case "wrong-vpc":
		allocation["vpcUID"] = secret
	case "wrong-scope":
		allocation["scopeRef"] = secret
	case "wrong-cidr":
		allocation["cidr"] = "10.24.2.0/24"
	case "noncanonical-cidr":
		allocation["cidr"] = "10.24.1.1/24"
	case "missing-identity":
		delete(allocation, "scopeRef")
	case "typed-identity":
		allocation["vpcUID"] = 123
	case "empty-allocation":
		allocation["allocationID"] = " "
	case "null-allocation":
		allocation["allocationID"] = nil
	case "typed-allocation":
		allocation["allocationID"] = 123
	case "wrong-policy":
		allocation["releasePolicy"] = "Delete"
	case "missing-policy":
		delete(allocation, "releasePolicy")
	default:
		os.Exit(17)
	}
	if json.NewEncoder(os.Stdout).Encode(response) != nil {
		os.Exit(18)
	}
	os.Exit(0)
}

type endlessReader struct{}

func (endlessReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}
