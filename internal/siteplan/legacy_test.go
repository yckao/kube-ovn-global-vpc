package siteplan

import (
	"encoding/json"
	"testing"
)

func TestLegacyWireCompatibility(t *testing.T) {
	v, cfg := fixture()
	p, err := Build(v, cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(p.Gateway)
	if err != nil {
		t.Fatal(err)
	}
	const hash = "1f922865a5bfeb14bac336ef72b522ad72dcb6118790044f68aca0e1d31ce36f"
	const wire = `{"version":"v1","globalVpcID":"tenant-red","ownerUID":"local-red-uid","siteID":"site-a","attachmentID":"infra-a","gatewayRevision":"registration-v1","delegatedPrefixes":["10.241.0.0/16","172.31.0.0/16"],"cidr":"10.241.1.0/24","transitCIDR":"10.240.255.0/29","routerIP":"10.240.255.1","gatewayIP":"10.240.255.2","vpcName":"sv-b34fe1f86d5ec6d6","subnetName":"ss-b34fe1f86d5ec6d6","transitName":"st-b34fe1f86d5ec6d6"}`
	if p.Hash != hash || string(b) != wire {
		t.Fatalf("legacy plan hash or gateway wire contract changed: hash=%s wire=%s", p.Hash, b)
	}
}
