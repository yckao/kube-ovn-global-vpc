package sitegateway

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

	api "globalvpc.io/controller/api/v1alpha1"
	"globalvpc.io/controller/internal/siteplan"
)

const fixtureSecret = "synthetic-gateway-secret-must-never-leak"

var testGateway = siteplan.GatewayConfig{Version: "v1", OwnerUID: "owner-uid", GlobalVpcID: "tenant-one", GatewayRevision: "revision-one",
	SiteID: "site-a", AttachmentID: "infra-a", CIDR: "10.20.1.0/24", DelegatedPrefixes: []string{"10.20.0.0/16"},
	TransitCIDR: "169.254.20.0/30", RouterIP: "169.254.20.1", GatewayIP: "169.254.20.2", VpcName: "vpc-one", SubnetName: "subnet-one", TransitName: "transit-one"}

var testRecords = []api.GatewayResourceRecord{
	{APIVersion: "v1", Kind: "ConfigMap", Namespace: "gateway-system", Name: "gateway-config", UID: "config-uid"},
	{APIVersion: "v1", Kind: "Pod", Namespace: "gateway-system", Name: "gateway-pod", UID: "pod-uid"},
}

func helper(t *testing.T, mode string, args ...string) Exec {
	t.Helper()
	t.Setenv("GLOBALVPC_GATEWAY_TEST", "1")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Exec{Command: append([]string{binary, "-test.run=^TestGatewayProcess$", "--", mode}, args...), Timeout: 3 * time.Second}
}

func TestProtocolOperations(t *testing.T) {
	e := helper(t, "success")
	for _, operation := range []struct {
		name string
		call func(context.Context, siteplan.GatewayConfig, []api.GatewayResourceRecord) (Result, error)
	}{
		{"Ensure", e.Ensure}, {"Get", e.Get}, {"Delete", e.Delete}, {"EndpointsEmpty", e.EndpointsEmpty},
	} {
		t.Run(operation.name, func(t *testing.T) {
			result, err := operation.call(context.Background(), testGateway, testRecords)
			if err != nil {
				t.Fatal(err)
			}
			if result.OwnerUID != testGateway.OwnerUID || result.GlobalVpcID != testGateway.GlobalVpcID || result.GatewayRevision != testGateway.GatewayRevision {
				t.Fatalf("wrong response identity: %#v", result)
			}
			if operation.name == "Delete" {
				if !result.Absent || len(result.Resources) != 0 {
					t.Fatalf("delete did not report absence: %#v", result)
				}
			} else if !reflect.DeepEqual(result.Resources, testRecords) || !result.Ready || !result.EndpointsEmpty {
				t.Fatalf("wrong ready result: %#v", result)
			}
		})
	}
}

func TestRejectUntrustedResponses(t *testing.T) {
	for _, mode := range []string{"malformed", "trailing-json", "trailing-log", "null", "array", "wrong-version", "wrong-owner", "wrong-global-id", "wrong-revision",
		"missing-ready", "null-ready", "typed-ready", "missing-absent", "missing-endpoints", "contradictory", "absent-resources",
		"duplicate-key", "case-alias", "unknown-field", "bad-resource-kind", "bad-resource-api", "missing-resource-namespace", "missing-resource-name", "missing-resource-uid",
		"duplicate-resource", "duplicate-resource-key", "unknown-resource-field", "resource-case-alias", "null-resource", "replaced-resource", "excessive-depth"} {
		t.Run(mode, func(t *testing.T) {
			_, err := helper(t, mode).Ensure(context.Background(), testGateway, testRecords)
			if err == nil || !errors.Is(err, ErrUnknownState) {
				t.Fatalf("expected unknown-state error, got %v", err)
			}
			if strings.Contains(err.Error(), fixtureSecret) {
				t.Fatalf("backend output leaked: %v", err)
			}
		})
	}
}

func TestLostResponseRetryPreservesIdentityAndReceipts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "committed-gateway.json")
	e := helper(t, "commit-then-exit", path)
	_, err := e.Ensure(context.Background(), testGateway, testRecords)
	if !errors.Is(err, ErrUnknownState) {
		t.Fatalf("expected unknown state after committed response loss: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("backend did not commit: %v", err)
	}
	e = helper(t, "rediscover", path)
	result, err := e.Ensure(context.Background(), testGateway, testRecords)
	if err != nil || !result.Ready || !reflect.DeepEqual(result.Resources, testRecords) {
		t.Fatalf("same-identity retry did not rediscover resources: %#v, %v", result, err)
	}
}

func TestPartialDeletionRetainsSurvivingReceipt(t *testing.T) {
	result, err := helper(t, "partial-delete").Delete(context.Background(), testGateway, testRecords)
	if err != nil || result.Absent || len(result.Resources) != 1 || result.Resources[0] != testRecords[0] {
		t.Fatalf("partial deletion should report the remaining original receipt: %#v, %v", result, err)
	}
}

func TestInitialEnsureAcceptsNewReceipts(t *testing.T) {
	result, err := helper(t, "success").Ensure(context.Background(), testGateway, nil)
	if err != nil || !reflect.DeepEqual(result.Resources, testRecords) {
		t.Fatalf("initial resources rejected: %#v, %v", result, err)
	}
}

func TestPartialRecoveryAcceptsNewReceiptsWhilePinningExistingUIDs(t *testing.T) {
	// The Pod was confirmed missing and its old receipt was durably removed.
	// Ensure may recreate it. If that response is lost, Get must rediscover the
	// newly committed Pod against the same ConfigMap-only receipt ledger.
	for _, operation := range []string{"Ensure", "Get"} {
		t.Run(operation, func(t *testing.T) {
			e := helper(t, "success")
			var result Result
			var err error
			if operation == "Ensure" {
				result, err = e.Ensure(context.Background(), testGateway, testRecords[:1])
			} else {
				result, err = e.Get(context.Background(), testGateway, testRecords[:1])
			}
			if err != nil || !reflect.DeepEqual(result.Resources, testRecords) {
				t.Fatalf("newly committed gateway receipt was not rediscovered: %#v, %v", result, err)
			}
			if _, err := helper(t, "replaced-resource").Get(context.Background(), testGateway, testRecords[:1]); !errors.Is(err, ErrUnknownState) {
				t.Fatalf("existing ConfigMap UID protection was lost: %v", err)
			}
		})
	}
}

func TestTimeoutCancellationAndOutputBounds(t *testing.T) {
	for _, mode := range []string{"sleep", "oversized-stdout", "oversized-stderr"} {
		t.Run(mode, func(t *testing.T) {
			e := helper(t, mode)
			if mode == "sleep" {
				e.Timeout = 60 * time.Millisecond
			}
			start := time.Now()
			_, err := e.Get(context.Background(), testGateway, nil)
			if !errors.Is(err, ErrUnknownState) || time.Since(start) > 2*time.Second {
				t.Fatalf("execution was not bounded: %v, elapsed %s", err, time.Since(start))
			}
			if strings.HasPrefix(mode, "oversized") && !strings.Contains(err.Error(), "size limit") {
				t.Fatalf("expected output size error: %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := helper(t, "sleep").Get(ctx, testGateway, nil); !errors.Is(err, ErrUnknownState) {
		t.Fatalf("parent cancellation not honored: %v", err)
	}
	for _, discard := range []bool{false, true} {
		cancelled := false
		out := &limitedOutput{cancel: func() { cancelled = true }, discard: discard}
		_, err := out.Write(make([]byte, maxOutputBytes+100))
		if err == nil || !cancelled || !out.exceeded || out.count != maxOutputBytes || out.buffer.Len() > maxOutputBytes || (discard && out.buffer.Len() != 0) {
			t.Fatalf("output bound failed: %#v", out)
		}
	}
}

func TestErrorsDoNotExposeBackendSecrets(t *testing.T) {
	for _, mode := range []string{"crash", "malformed", "wrong-revision", "unknown-field"} {
		_, err := helper(t, mode, fixtureSecret).Get(context.Background(), testGateway, nil)
		if err == nil || strings.Contains(err.Error(), fixtureSecret) {
			t.Fatalf("unsanitized error: %v", err)
		}
	}
	e := Exec{Command: []string{filepath.Join(t.TempDir(), fixtureSecret)}}
	if _, err := e.Get(context.Background(), testGateway, nil); err == nil || strings.Contains(err.Error(), fixtureSecret) {
		t.Fatalf("executable path leaked: %v", err)
	}
}

func TestNoShellExpansion(t *testing.T) {
	e := helper(t, "literal-argument", "$(printf unexpected); `printf unexpected` *")
	if _, err := e.Ensure(context.Background(), testGateway, nil); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidInputsNeverExecute(t *testing.T) {
	for _, change := range []func(*siteplan.GatewayConfig){
		func(g *siteplan.GatewayConfig) { g.Version = "v2" },
		func(g *siteplan.GatewayConfig) { g.OwnerUID = "" },
		func(g *siteplan.GatewayConfig) { g.GlobalVpcID = " " },
		func(g *siteplan.GatewayConfig) { g.GatewayRevision = "" },
	} {
		g := testGateway
		change(&g)
		e := Exec{Command: []string{filepath.Join(t.TempDir(), "does-not-exist")}}
		if _, err := e.Ensure(context.Background(), g, nil); err == nil || errors.Is(err, ErrUnknownState) {
			t.Fatalf("invalid gateway should fail before execution: %v", err)
		}
	}
	for _, command := range [][]string{nil, {}, {""}, {" "}} {
		if _, err := (Exec{Command: command}).Ensure(context.Background(), testGateway, nil); err == nil || errors.Is(err, ErrUnknownState) {
			t.Fatalf("invalid command accepted: %v", err)
		}
	}
	e := Exec{Command: []string{filepath.Join(t.TempDir(), "does-not-exist")}}
	if _, err := e.Delete(context.Background(), testGateway, []api.GatewayResourceRecord{{Kind: "Secret"}}); err == nil || errors.Is(err, ErrUnknownState) {
		t.Fatalf("invalid expected receipt should fail before execution: %v", err)
	}
	g := testGateway
	g.DelegatedPrefixes = []string{strings.Repeat("x", maxOutputBytes)}
	if _, err := e.Ensure(context.Background(), g, nil); err == nil || errors.Is(err, ErrUnknownState) {
		t.Fatalf("oversized request should fail before execution: %v", err)
	}
}

// The test subprocess checks the exact protocol before simulating a potentially
// faulty backend. It has no Kubernetes API access or real gateway side effects.
func TestGatewayProcess(t *testing.T) {
	if os.Getenv("GLOBALVPC_GATEWAY_TEST") != "1" {
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
	if json.NewDecoder(os.Stdin).Decode(&req) != nil || req.Version != protocolVersion || (args[0] != "ha-response" && !reflect.DeepEqual(req.Gateway, testGateway)) || !validRecords(req.ExpectedResources) {
		os.Exit(11)
	}
	if req.Operation != "Ensure" && req.Operation != "Get" && req.Operation != "Delete" && req.Operation != "EndpointsEmpty" {
		os.Exit(12)
	}
	records := make([]map[string]any, len(testRecords))
	for i, r := range testRecords {
		records[i] = map[string]any{"apiVersion": r.APIVersion, "kind": r.Kind, "namespace": r.Namespace, "name": r.Name, "uid": r.UID}
	}
	out := map[string]any{"version": "v1", "ownerUID": req.Gateway.OwnerUID, "globalVpcID": req.Gateway.GlobalVpcID, "gatewayRevision": req.Gateway.GatewayRevision,
		"ready": true, "absent": false, "endpointsEmpty": true, "resources": records}
	if req.Operation == "Delete" && args[0] != "partial-delete" {
		out["ready"], out["absent"] = false, true
		delete(out, "resources")
	}
	switch args[0] {
	case "success":
	case "ha-response":
		if len(args) != 2 {
			os.Exit(21)
		}
		var extra map[string]any
		if json.Unmarshal([]byte(args[1]), &extra) != nil {
			os.Exit(22)
		}
		for key, value := range extra {
			out[key] = value
		}
	case "literal-argument":
		if len(args) != 2 || args[1] != "$(printf unexpected); `printf unexpected` *" {
			os.Exit(13)
		}
	case "commit-then-exit":
		body, _ := json.Marshal(req)
		if len(args) != 2 || os.WriteFile(args[1], body, 0600) != nil {
			os.Exit(14)
		}
		os.Exit(2)
	case "rediscover":
		body, err := os.ReadFile(args[1])
		var committed request
		if err != nil || json.Unmarshal(body, &committed) != nil || !reflect.DeepEqual(committed, req) {
			os.Exit(15)
		}
	case "partial-delete":
		out["ready"] = false
		out["resources"] = records[:1]
	case "crash":
		fmt.Fprint(os.Stderr, fixtureSecret)
		os.Exit(2)
	case "sleep":
		time.Sleep(10 * time.Second)
		os.Exit(16)
	case "oversized-stdout", "oversized-stderr":
		dst := os.Stdout
		if args[0] == "oversized-stderr" {
			dst = os.Stderr
		}
		_, _ = io.CopyN(dst, endlessReader{}, maxOutputBytes*4)
		time.Sleep(10 * time.Second)
		os.Exit(17)
	case "malformed":
		fmt.Fprint(os.Stdout, "{invalid:"+fixtureSecret)
		os.Exit(0)
	case "trailing-json", "trailing-log", "duplicate-key", "case-alias", "duplicate-resource-key":
		body, _ := json.Marshal(out)
		s := string(body)
		switch args[0] {
		case "trailing-json":
			s += " {}"
		case "trailing-log":
			s += fixtureSecret
		case "duplicate-key":
			s = strings.Replace(s, `"version":"v1"`, `"version":"v1","version":"v1"`, 1)
		case "case-alias":
			s = strings.Replace(s, `"version":"v1"`, `"Version":"v1"`, 1)
		case "duplicate-resource-key":
			s = strings.Replace(s, `"uid":"config-uid"`, `"uid":"config-uid","uid":"replacement"`, 1)
		}
		fmt.Fprint(os.Stdout, s)
		os.Exit(0)
	case "null", "array":
		if args[0] == "null" {
			fmt.Fprint(os.Stdout, "null")
		} else {
			fmt.Fprint(os.Stdout, "[]")
		}
		os.Exit(0)
	case "wrong-version":
		out["version"] = fixtureSecret
	case "wrong-owner":
		out["ownerUID"] = fixtureSecret
	case "wrong-global-id":
		out["globalVpcID"] = fixtureSecret
	case "wrong-revision":
		out["gatewayRevision"] = fixtureSecret
	case "missing-ready":
		delete(out, "ready")
	case "null-ready":
		out["ready"] = nil
	case "typed-ready":
		out["ready"] = "true"
	case "missing-absent":
		delete(out, "absent")
	case "missing-endpoints":
		delete(out, "endpointsEmpty")
	case "contradictory":
		out["absent"] = true
	case "absent-resources":
		out["ready"], out["absent"] = false, true
	case "unknown-field":
		out["error"] = fixtureSecret
	case "bad-resource-kind":
		records[0]["kind"] = "Secret"
	case "bad-resource-api":
		records[0]["apiVersion"] = "apps/v1"
	case "missing-resource-namespace":
		delete(records[0], "namespace")
	case "missing-resource-name":
		delete(records[0], "name")
	case "missing-resource-uid":
		delete(records[0], "uid")
	case "duplicate-resource":
		out["resources"] = append(records, records[0])
	case "unknown-resource-field":
		records[0]["error"] = fixtureSecret
	case "resource-case-alias":
		records[0]["Namespace"] = records[0]["namespace"]
		delete(records[0], "namespace")
	case "null-resource":
		out["resources"] = []any{nil}
	case "replaced-resource":
		records[0]["uid"] = "replacement"
	case "excessive-depth":
		fmt.Fprint(os.Stdout, `{"resources":`+strings.Repeat("[", 70)+"0"+strings.Repeat("]", 70)+"}")
		os.Exit(0)
	default:
		os.Exit(18)
	}
	if json.NewEncoder(os.Stdout).Encode(out) != nil {
		os.Exit(19)
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
