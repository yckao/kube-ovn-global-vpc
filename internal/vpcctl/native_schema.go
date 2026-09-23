package vpcctl

import "encoding/json"

// nativeSchema is deliberately pinned with the patch. A bundle cannot substitute
// a weaker CRD contract while claiming to contain the supported extension.
func nativeSchema() NativeSchema {
	var s NativeSchema
	if err := json.Unmarshal([]byte(nativeSchemaJSON), &s); err != nil {
		panic(err)
	}
	return s
}

const nativeSchemaJSON = `{
  "spec": {
    "description":"Native destination-scoped BFD ECMP with a discard guard.",
    "type":"array", "maxItems":256, "x-kubernetes-list-type":"map", "x-kubernetes-list-map-keys":["cidr"],
    "items":{"type":"object","required":["cidr","nextHops","bfd","selectionFields"],"properties":{
      "cidr":{"type":"string","maxLength":18},
      "nextHops":{"type":"array","minItems":2,"maxItems":64,"x-kubernetes-list-type":"set","items":{"type":"string","maxLength":15}},
      "selectionFields":{"type":"array","minItems":1,"maxItems":7,"x-kubernetes-list-type":"set","items":{"type":"string","enum":["eth_src","eth_dst","ip_src","ip_dst","ip_proto","tp_src","tp_dst"]}},
      "bfd":{"type":"object","required":["minRX","minTX","multiplier"],"properties":{
        "minRX":{"type":"integer","minimum":1,"maximum":3600000},
        "minTX":{"type":"integer","minimum":1,"maximum":3600000},
        "multiplier":{"type":"integer","minimum":1,"maximum":255}
      }}
    }}
  },
  "status": {
    "description":"Configuration acknowledgement, not dataplane health.", "type":"object", "properties":{
      "capability":{"type":"string"}, "observedGeneration":{"type":"integer","format":"int64"},
      "appliedHash":{"type":"string"}, "ready":{"type":"boolean"}
    }
  }
}`
