// Package provider implements the executable external IPAM protocol.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os/exec"
	"strings"
	"time"

	v1alpha1 "globalvpc.io/controller/api/v1alpha1"
)

const (
	apiVersion     = "ipam.globalvpc.io/v1alpha1"
	defaultTimeout = 30 * time.Second
	maximumTimeout = 5 * time.Minute
	maxOutputBytes = 1 << 20
)

// ErrUnknownState means the provider may have committed the claim. Callers must
// preserve existing allocations and retry Ensure with the same claim identity.
var ErrUnknownState = errors.New("allocation state is unknown; preserve allocations and retry the same claim")

// Exec invokes a configured argument array without a shell. Credentials belong
// in provider-local configuration and are never accepted from its response.
// A zero or negative Timeout uses 30 seconds; values above 5 minutes are capped.
type Exec struct {
	Command []string
	Timeout time.Duration
}

type request struct {
	APIVersion string `json:"apiVersion"`
	Operation  string `json:"operation"`
	v1alpha1.PrefixClaim
}

type response struct {
	APIVersion string          `json:"apiVersion"`
	OK         *bool           `json:"ok"`
	Result     json.RawMessage `json:"result"`
}

// Ensure reserves an explicit, retained prefix. A lost response never triggers
// ReleasePrefix: a subsequent call uses the same immutable claim to rediscover
// the provider's committed allocation.
func (p Exec) Ensure(ctx context.Context, claim v1alpha1.PrefixClaim) (v1alpha1.Allocation, error) {
	if len(p.Command) == 0 || strings.TrimSpace(p.Command[0]) == "" {
		return v1alpha1.Allocation{}, errors.New("IPAM provider command is not configured")
	}
	if !validClaim(claim) {
		return v1alpha1.Allocation{}, errors.New("IPAM claim requires nonempty identities and a canonical network CIDR")
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if timeout > maximumTimeout {
		timeout = maximumTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, err := json.Marshal(request{APIVersion: apiVersion, Operation: "EnsurePrefix", PrefixClaim: claim})
	if err != nil {
		return v1alpha1.Allocation{}, errors.New("cannot encode IPAM claim")
	}
	output := &limitedOutput{cancel: cancel}
	cmd := exec.CommandContext(ctx, p.Command[0], p.Command[1:]...)
	cmd.Stdin = bytes.NewReader(body)
	cmd.Stdout = output
	// Provider stderr and process errors may contain credentials, command
	// arguments or environment values. Never propagate any of their text.
	cmd.Stderr = io.Discard
	// Also bound waiting for pipe descriptors retained by provider descendants.
	cmd.WaitDelay = time.Second
	err = cmd.Run()
	if output.exceeded {
		return v1alpha1.Allocation{}, unknown("IPAM provider response exceeded the size limit")
	}
	if ctx.Err() != nil {
		return v1alpha1.Allocation{}, unknown("IPAM provider deadline or cancellation interrupted the request")
	}
	if err != nil {
		return v1alpha1.Allocation{}, unknown("IPAM provider execution failed")
	}
	var result response
	if !isObject(output.Bytes()) || json.Unmarshal(output.Bytes(), &result) != nil {
		return v1alpha1.Allocation{}, unknown("IPAM provider returned an invalid response")
	}
	if result.APIVersion != apiVersion {
		return v1alpha1.Allocation{}, unknown("IPAM provider returned an unsupported response version")
	}
	if result.OK == nil || !*result.OK {
		return v1alpha1.Allocation{}, unknown("IPAM provider did not confirm the allocation")
	}
	var allocation v1alpha1.Allocation
	if !isObject(result.Result) || json.Unmarshal(result.Result, &allocation) != nil {
		return v1alpha1.Allocation{}, unknown("IPAM provider returned an invalid allocation")
	}
	if !validClaim(allocation.PrefixClaim) || allocation.PrefixClaim != claim ||
		strings.TrimSpace(allocation.AllocationID) == "" || allocation.ReleasePolicy != "Retain" {
		return v1alpha1.Allocation{}, unknown("IPAM provider allocation does not match the requested retained claim")
	}
	return allocation, nil
}

func unknown(reason string) error {
	return fmt.Errorf("%s: %w", reason, ErrUnknownState)
}

func validClaim(claim v1alpha1.PrefixClaim) bool {
	if strings.TrimSpace(claim.ClaimUID) == "" || strings.TrimSpace(claim.VpcUID) == "" || strings.TrimSpace(claim.ScopeRef) == "" {
		return false
	}
	prefix, err := netip.ParsePrefix(claim.CIDR)
	return err == nil && prefix == prefix.Masked() && prefix.String() == claim.CIDR
}

func isObject(data []byte) bool {
	data = bytes.TrimSpace(data)
	return len(data) > 1 && data[0] == '{' && data[len(data)-1] == '}'
}

// limitedOutput retains at most maxOutputBytes, terminating the provider as
// soon as the response exceeds that bound instead of buffering untrusted data.
type limitedOutput struct {
	buffer   bytes.Buffer
	cancel   context.CancelFunc
	exceeded bool
}

func (b *limitedOutput) Bytes() []byte { return b.buffer.Bytes() }
func (b *limitedOutput) Len() int      { return b.buffer.Len() }

func (b *limitedOutput) Write(data []byte) (int, error) {
	remaining := maxOutputBytes - b.Len()
	if len(data) > remaining {
		_, _ = b.buffer.Write(data[:remaining])
		b.exceeded = true
		b.cancel()
		return remaining, errors.New("IPAM response size limit exceeded")
	}
	return b.buffer.Write(data)
}
