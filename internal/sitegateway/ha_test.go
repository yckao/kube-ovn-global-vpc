package sitegateway

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"globalvpc.io/controller/internal/siteconfig"
)

func TestHAGatewayResponseRequiresConsistentRegisteredAvailability(t *testing.T) {
	g := testGateway
	g.Version, g.GatewayIP = "v2", ""
	g.Gateways = []siteconfig.Gateway{{ID: "one", IP: "10.0.0.2"}, {ID: "two", IP: "10.0.0.3"}}
	g.BFD = &siteconfig.BFD{SourceIP: "10.1.0.1", MinRX: 300, MinTX: 300, Multiplier: 3}
	for _, tc := range []struct {
		name   string
		change map[string]any
		valid  bool
	}{
		{"both available", nil, true},
		{"degraded available", map[string]any{"readyGateways": 1, "readyGatewayIDs": []string{"two"}}, true},
		{"none available", map[string]any{"ready": false, "readyGateways": 0, "readyGatewayIDs": []string{}}, true},
		{"missing count", map[string]any{"readyGateways": nil}, false},
		{"missing identities", map[string]any{"readyGatewayIDs": nil}, false},
		{"foreign identity", map[string]any{"readyGatewayIDs": []string{"one", "other"}}, false},
		{"duplicate identity", map[string]any{"readyGatewayIDs": []string{"one", "one"}}, false},
		{"count mismatch", map[string]any{"readyGateways": 1}, false},
		{"wrong configured total", map[string]any{"desiredGateways": 3}, false},
		{"over configured total", map[string]any{"readyGateways": 3}, false},
		{"contradictory ready", map[string]any{"ready": false}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := map[string]any{"readyGateways": 2, "desiredGateways": 2, "readyGatewayIDs": []string{"one", "two"}}
			for k, v := range tc.change {
				response[k] = v
			}
			body, _ := json.Marshal(response)
			out, err := helper(t, "ha-response", string(body)).Get(context.Background(), g, nil)
			if tc.valid {
				if err != nil || out.DesiredGateways != 2 || len(out.ReadyGatewayIDs) != out.ReadyGateways {
					t.Fatalf("out=%+v err=%v", out, err)
				}
			} else if !errors.Is(err, ErrUnknownState) {
				t.Fatalf("invalid availability was accepted: %v", err)
			}
		})
	}
}
