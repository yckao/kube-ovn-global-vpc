package destinationroute

import (
	"encoding/json"
	"reflect"
	"testing"
)

func example() Intent {
	return Intent{CIDR: "10.70.0.0/24", NextHops: []string{"172.20.0.3", "172.20.0.2"}, BFD: BFD{MinRX: 100, MinTX: 100, Multiplier: 3}, SelectionFields: []string{"tp_src", "ip_src", "ip_dst", "tp_dst", "ip_proto"}}
}
func TestCanonicalHash(t *testing.T) {
	a := example()
	before, _ := json.Marshal(a)
	p, err := Compile([]Intent{a})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(a)
	if string(before) != string(after) {
		t.Fatal("input mutated")
	}
	a.NextHops = []string{"172.20.0.2", "172.20.0.3"}
	a.SelectionFields = []string{"ip_dst", "ip_src", "ip_proto", "tp_dst", "tp_src"}
	q, err := Compile([]Intent{a})
	if err != nil || p.Hash != q.Hash {
		t.Fatalf("unstable hash %v", err)
	}
	empty, _ := Compile(nil)
	if empty.Hash != "4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945" {
		t.Fatal(empty.Hash)
	}
}
func TestValidation(t *testing.T) {
	tests := map[string]func(*Intent){"default": func(i *Intent) { i.CIDR = "0.0.0.0/0" }, "host": func(i *Intent) { i.CIDR = "10.70.0.1/32" }, "hostbits": func(i *Intent) { i.CIDR = "10.70.0.1/24" }, "duplicate-hop": func(i *Intent) { i.NextHops[1] = i.NextHops[0] }, "single-hop": func(i *Intent) { i.NextHops = i.NextHops[:1] }, "timer": func(i *Intent) { i.BFD.Multiplier = 0 }, "invalid-field": func(i *Intent) { i.SelectionFields = []string{"ct_label"} }, "destination-hop": func(i *Intent) { i.NextHops[0] = "10.70.0.3" }}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			r := example()
			change(&r)
			if _, err := Compile([]Intent{r}); err == nil {
				t.Fatal("accepted invalid intent")
			}
		})
	}
	a, b := example(), example()
	b.CIDR = "10.71.0.0/24"
	b.BFD.MinTX = 200
	if _, err := Compile([]Intent{a, b}); err == nil {
		t.Fatal("accepted conflicting shared BFD timers")
	}
	b = example()
	b.CIDR = "10.70.0.0/25"
	if _, err := Compile([]Intent{a, b}); err == nil {
		t.Fatal("accepted overlapping destination")
	}
}
func TestGuardExpansion(t *testing.T) {
	for input, want := range map[string][2]string{"10.70.0.0/24": {"10.70.0.0/25", "10.70.0.128/25"}, "10.70.0.0/31": {"10.70.0.0/32", "10.70.0.1/32"}, "128.0.0.0/1": {"128.0.0.0/2", "192.0.0.0/2"}} {
		if got := Children(input); got != want {
			t.Fatalf("%s: %v", input, got)
		}
	}
}
func TestReachableGateway(t *testing.T) {
	networks := []string{"172.20.0.1/24"}
	for hop, want := range map[string]bool{"172.20.0.2": true, "172.20.0.1": false, "172.20.0.0": false, "172.20.0.255": false, "172.21.0.2": false, "invalid": false} {
		if got := ReachableGateway(hop, networks); got != want {
			t.Fatalf("%s got %v", hop, got)
		}
	}
	p, _ := Compile([]Intent{example()})
	if !reflect.DeepEqual(p.Routes[0].NextHops, []string{"172.20.0.2", "172.20.0.3"}) {
		t.Fatal(p)
	}
}
