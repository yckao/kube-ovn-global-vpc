package managedgateway

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProbeConfigBoundedWithoutLeakingValues(t *testing.T) {
	for _, value := range []map[string]any{nil, {"secret": make(chan int)}, {"secret": strings.Repeat("sensitive", maxProbeConfigBytes)}} {
		if _, err := probeConfig(value); err == nil || strings.Contains(err.Error(), "sensitive") {
			t.Fatalf("invalid configuration accepted or exposed: %v", err)
		}
	}
	want := map[string]any{"gateway": map[string]any{"gatewayID": "sibling-2"}}
	data, err := probeConfig(want)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err = json.Unmarshal(data, &got); err != nil || got["gateway"].(map[string]any)["gatewayID"] != "sibling-2" {
		t.Fatal("sibling configuration not preserved")
	}
}
