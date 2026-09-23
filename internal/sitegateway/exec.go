// Package sitegateway invokes one administrator-selected, local gateway backend.
// It never discovers peer Controllers or calls remote cluster APIs.
package sitegateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	api "globalvpc.io/controller/api/v1alpha1"
	"globalvpc.io/controller/internal/siteplan"
)

const (
	protocolVersion = "v1"
	defaultTimeout  = 30 * time.Second
	maximumTimeout  = 5 * time.Minute
	maxOutputBytes  = 1 << 20
)

// ErrUnknownState requires retaining receipts and retrying the same owner and
// revision. An interrupted subprocess may already have committed its operation.
var ErrUnknownState = errors.New("gateway state is unknown; preserve ownership receipts and retry the same identity")

// Exec accepts its argv only from local administrator registration. It never
// invokes a shell. Nonpositive timeouts default to 30 seconds; the cap is 5 minutes.
type Exec struct {
	Command []string
	Timeout time.Duration
}

// Result contains only verified protocol fields. Resources are local Kubernetes
// receipts, not permission to act on arbitrary objects returned by the backend.
type Result struct {
	Version         string                      `json:"version"`
	OwnerUID        string                      `json:"ownerUID"`
	GlobalVpcID     string                      `json:"globalVpcID"`
	GatewayRevision string                      `json:"gatewayRevision"`
	Ready           bool                        `json:"ready"`
	Absent          bool                        `json:"absent"`
	EndpointsEmpty  bool                        `json:"endpointsEmpty"`
	ReadyGateways   int                         `json:"readyGateways,omitempty"`
	DesiredGateways int                         `json:"desiredGateways,omitempty"`
	ReadyGatewayIDs []string                    `json:"readyGatewayIDs,omitempty"`
	Resources       []api.GatewayResourceRecord `json:"resources,omitempty"`
}

type request struct {
	Version           string                      `json:"version"`
	Operation         string                      `json:"operation"`
	Gateway           siteplan.GatewayConfig      `json:"gateway"`
	ExpectedResources []api.GatewayResourceRecord `json:"expectedResources,omitempty"`
}

type response struct {
	Version         string                      `json:"version"`
	OwnerUID        string                      `json:"ownerUID"`
	GlobalVpcID     string                      `json:"globalVpcID"`
	GatewayRevision string                      `json:"gatewayRevision"`
	Ready           *bool                       `json:"ready"`
	Absent          *bool                       `json:"absent"`
	EndpointsEmpty  *bool                       `json:"endpointsEmpty"`
	ReadyGateways   *int                        `json:"readyGateways,omitempty"`
	DesiredGateways *int                        `json:"desiredGateways,omitempty"`
	ReadyGatewayIDs *[]string                   `json:"readyGatewayIDs,omitempty"`
	Resources       []api.GatewayResourceRecord `json:"resources,omitempty"`
}

func (e Exec) Ensure(ctx context.Context, gateway siteplan.GatewayConfig, expected []api.GatewayResourceRecord) (Result, error) {
	return e.invoke(ctx, "Ensure", gateway, expected)
}

func (e Exec) Get(ctx context.Context, gateway siteplan.GatewayConfig, expected []api.GatewayResourceRecord) (Result, error) {
	return e.invoke(ctx, "Get", gateway, expected)
}

func (e Exec) Delete(ctx context.Context, gateway siteplan.GatewayConfig, expected []api.GatewayResourceRecord) (Result, error) {
	return e.invoke(ctx, "Delete", gateway, expected)
}

func (e Exec) EndpointsEmpty(ctx context.Context, gateway siteplan.GatewayConfig, expected []api.GatewayResourceRecord) (Result, error) {
	return e.invoke(ctx, "EndpointsEmpty", gateway, expected)
}

func (e Exec) invoke(ctx context.Context, operation string, gateway siteplan.GatewayConfig, expected []api.GatewayResourceRecord) (Result, error) {
	if len(e.Command) == 0 || strings.TrimSpace(e.Command[0]) == "" {
		return Result{}, errors.New("local gateway command is not configured")
	}
	if (gateway.Version != protocolVersion && gateway.Version != "v2") || strings.TrimSpace(gateway.OwnerUID) == "" ||
		strings.TrimSpace(gateway.GlobalVpcID) == "" || strings.TrimSpace(gateway.GatewayRevision) == "" {
		return Result{}, errors.New("local gateway requires a supported version, owner, global VPC identity and revision")
	}
	if gateway.Version == "v2" && (len(gateway.Gateways) < 2 || len(gateway.Gateways) > 8 || gateway.BFD == nil || gateway.GatewayIP != "") {
		return Result{}, errors.New("multi-gateway configuration requires members and BFD")
	}
	if !validRecords(expected) {
		return Result{}, errors.New("local gateway expected resource receipts are invalid")
	}
	body, err := json.Marshal(request{Version: protocolVersion, Operation: operation, Gateway: gateway, ExpectedResources: expected})
	if err != nil || len(body) > maxOutputBytes {
		return Result{}, errors.New("local gateway request cannot be encoded within the size limit")
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if timeout > maximumTimeout {
		timeout = maximumTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stdout := &limitedOutput{cancel: cancel}
	stderr := &limitedOutput{cancel: cancel, discard: true}
	cmd := exec.CommandContext(ctx, e.Command[0], e.Command[1:]...)
	cmd.Stdin = bytes.NewReader(body)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// Bound descriptors retained by descendants after the executable exits.
	cmd.WaitDelay = time.Second
	err = cmd.Run()
	// Never return subprocess error text, output, command arguments or paths.
	if stdout.exceeded || stderr.exceeded {
		return Result{}, unknown("local gateway output exceeded the size limit")
	}
	if ctx.Err() != nil {
		return Result{}, unknown("local gateway deadline or cancellation interrupted the request")
	}
	if err != nil {
		return Result{}, unknown("local gateway execution failed")
	}
	var out response
	if err = decodeResponse(stdout.buffer.Bytes(), &out); err != nil {
		return Result{}, unknown("local gateway returned an invalid response")
	}
	if out.Version != protocolVersion || out.OwnerUID != gateway.OwnerUID || out.GlobalVpcID != gateway.GlobalVpcID || out.GatewayRevision != gateway.GatewayRevision {
		return Result{}, unknown("local gateway response version, identity or revision does not match")
	}
	if out.Ready == nil || out.Absent == nil || out.EndpointsEmpty == nil ||
		(*out.Ready && *out.Absent) || (*out.Absent && len(out.Resources) != 0) {
		return Result{}, unknown("local gateway response state is incomplete or contradictory")
	}
	if !validRecords(out.Resources) || !matchesExpected(out.Resources, expected) {
		return Result{}, unknown("local gateway response resource receipts do not match")
	}
	readyCount, desiredCount := 0, 1
	if *out.Ready {
		readyCount = 1
	}
	if gateway.Version == "v2" {
		if out.ReadyGateways == nil || out.DesiredGateways == nil ||
			*out.DesiredGateways != len(gateway.Gateways) || *out.DesiredGateways < 2 ||
			*out.ReadyGateways < 0 || *out.ReadyGateways > *out.DesiredGateways ||
			*out.Ready != (*out.ReadyGateways > 0) || (*out.Absent && *out.ReadyGateways != 0) {
			return Result{}, unknown("local gateway availability counts are incomplete or contradictory")
		}
		if out.ReadyGatewayIDs == nil || len(*out.ReadyGatewayIDs) != *out.ReadyGateways {
			return Result{}, unknown("local gateway available identities are incomplete")
		}
		members := map[string]bool{}
		for _, member := range gateway.Gateways {
			members[member.ID] = true
		}
		for _, id := range *out.ReadyGatewayIDs {
			if !members[id] {
				return Result{}, unknown("local gateway available identities are invalid")
			}
			delete(members, id)
		}
		readyCount, desiredCount = *out.ReadyGateways, *out.DesiredGateways
	}
	var readyIDs []string
	if out.ReadyGatewayIDs != nil {
		readyIDs = append([]string(nil), (*out.ReadyGatewayIDs)...)
	}
	return Result{Version: out.Version, OwnerUID: out.OwnerUID, GlobalVpcID: out.GlobalVpcID, GatewayRevision: out.GatewayRevision,
		Ready: *out.Ready, Absent: *out.Absent, EndpointsEmpty: *out.EndpointsEmpty, ReadyGateways: readyCount,
		DesiredGateways: desiredCount, ReadyGatewayIDs: readyIDs, Resources: out.Resources}, nil
}

func unknown(reason string) error { return fmt.Errorf("%s: %w", reason, ErrUnknownState) }

func recordKey(r api.GatewayResourceRecord) string {
	return r.APIVersion + "\x00" + r.Kind + "\x00" + r.Namespace + "\x00" + r.Name
}

func validRecords(records []api.GatewayResourceRecord) bool {
	seen := map[string]bool{}
	for _, r := range records {
		key := recordKey(r)
		if r.APIVersion != "v1" || (r.Kind != "Pod" && r.Kind != "ConfigMap") ||
			strings.TrimSpace(r.Namespace) == "" || strings.TrimSpace(r.Name) == "" || strings.TrimSpace(r.UID) == "" || seen[key] {
			return false
		}
		seen[key] = true
	}
	return true
}

// Existing receipt keys pin UIDs. Additional receipts permit rediscovery when
// Ensure created an object but its response was lost. The administrator-selected
// backend must restrict all records to its deterministic local object names and
// verify owner/revision before returning them. The reconciler must persist an
// observed absence before clearing a receipt and requesting recreation.
func matchesExpected(actual, expected []api.GatewayResourceRecord) bool {
	if len(expected) == 0 {
		return true
	}
	byKey := make(map[string]string, len(expected))
	for _, r := range expected {
		byKey[recordKey(r)] = r.UID
	}
	for _, r := range actual {
		if uid, exists := byKey[recordKey(r)]; exists && uid != r.UID {
			return false
		}
	}
	return true
}

func decodeResponse(data []byte, out *response) error {
	data = bytes.TrimSpace(data)
	if len(data) < 2 || data[0] != '{' || data[len(data)-1] != '}' {
		return errors.New("expected one object")
	}
	// encoding/json otherwise accepts repeated fields and case-insensitive keys.
	// Reject duplicates first, then decode a strict schema using canonical keys.
	d := json.NewDecoder(bytes.NewReader(data))
	if err := uniqueJSON(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("extra output")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return err
	}
	for key := range top {
		switch key {
		case "version", "ownerUID", "globalVpcID", "gatewayRevision", "ready", "absent", "endpointsEmpty", "resources", "readyGateways", "desiredGateways", "readyGatewayIDs":
		default:
			return errors.New("unknown field")
		}
	}
	if raw, present := top["resources"]; present {
		var records []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &records); err != nil {
			return err
		}
		for _, record := range records {
			for key := range record {
				switch key {
				case "apiVersion", "kind", "namespace", "name", "uid":
				default:
					return errors.New("unknown resource field")
				}
			}
		}
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(out)
}

func uniqueJSON(d *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("excessive nesting")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delim {
	case '{':
		keys := map[string]bool{}
		for d.More() {
			keyToken, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			// Case folding also rejects ambiguous aliases inside resource records.
			key = strings.ToLower(key)
			if !ok || keys[key] {
				return errors.New("duplicate key")
			}
			keys[key] = true
			if err := uniqueJSON(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := uniqueJSON(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected delimiter")
	}
	_, err = d.Token()
	return err
}

type limitedOutput struct {
	buffer   bytes.Buffer
	count    int
	cancel   context.CancelFunc
	exceeded bool
	discard  bool
}

func (b *limitedOutput) Write(p []byte) (int, error) {
	remaining := maxOutputBytes - b.count
	if len(p) > remaining {
		b.exceeded = true
		b.cancel()
		p = p[:remaining]
	}
	b.count += len(p)
	if !b.discard {
		_, _ = b.buffer.Write(p)
	}
	if b.exceeded {
		return len(p), errors.New("gateway output size limit exceeded")
	}
	return len(p), nil
}
