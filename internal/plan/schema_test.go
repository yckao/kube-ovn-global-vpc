package plan

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"globalvpc.io/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// Check every serialized API field rather than a fixed field list. A new status
// receipt must not appear to persist while the API server silently prunes it.
func TestCRDSchemaPreservesAPIFields(t *testing.T) {
	raw, err := os.ReadFile("../../config/crd/networking.globalvpc.io_globalvpcs.yaml")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := yaml.ToJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	var crd map[string]interface{}
	if err := json.Unmarshal(encoded, &crd); err != nil {
		t.Fatal(err)
	}
	spec := crd["spec"].(map[string]interface{})
	if spec["scope"] != "Cluster" || spec["group"] != v1alpha1.GroupVersion.Group {
		t.Fatal("CRD scope/group differs from API contract")
	}
	version := spec["versions"].([]interface{})[0].(map[string]interface{})
	if version["name"] != v1alpha1.GroupVersion.Version {
		t.Fatal("CRD version differs from API contract")
	}
	if _, exists := version["subresources"].(map[string]interface{})["status"]; !exists {
		t.Fatal("status subresource is required")
	}
	schema := version["schema"].(map[string]interface{})["openAPIV3Schema"].(map[string]interface{})
	properties := schema["properties"].(map[string]interface{})
	checkFields(t, "spec", reflect.TypeOf(v1alpha1.GlobalVpcSpec{}), properties["spec"].(map[string]interface{}))
	checkFields(t, "status", reflect.TypeOf(v1alpha1.GlobalVpcStatus{}), properties["status"].(map[string]interface{}))
	intent := properties["spec"].(map[string]interface{})
	validations := intent["x-kubernetes-validations"].([]interface{})
	if validations[0].(map[string]interface{})["rule"] != "self == oldSelf" {
		t.Fatal("first controller version must reject topology updates")
	}
	attachments := intent["properties"].(map[string]interface{})["attachments"].(map[string]interface{})
	if attachments["x-kubernetes-list-type"] != "map" || !reflect.DeepEqual(attachments["x-kubernetes-list-map-keys"], []interface{}{"clusterRef"}) {
		t.Fatal("attachments must be keyed by clusterRef")
	}
}

func checkFields(t *testing.T, path string, typ reflect.Type, schema map[string]interface{}) {
	t.Helper()
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() == reflect.Slice {
		if schema["type"] != "array" {
			t.Errorf("%s must be an array", path)
			return
		}
		checkFields(t, path+"[]", typ.Elem(), schema["items"].(map[string]interface{}))
		return
	}
	// metav1.Time serializes as RFC3339 rather than its internal struct fields.
	if typ.Name() == "Time" && typ.PkgPath() == "k8s.io/apimachinery/pkg/apis/meta/v1" {
		if schema["type"] != "string" || schema["format"] != "date-time" {
			t.Errorf("%s must be an RFC3339 string", path)
		}
		return
	}
	if typ.Kind() != reflect.Struct {
		return
	}
	properties, ok := schema["properties"].(map[string]interface{})
	if !ok {
		t.Errorf("%s has no structural properties", path)
		return
	}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		tag := strings.Split(field.Tag.Get("json"), ",")[0]
		if tag == "-" {
			continue
		}
		if tag == "" && field.Anonymous {
			checkFields(t, path, field.Type, schema)
			continue
		}
		child, ok := properties[tag].(map[string]interface{})
		if !ok {
			t.Errorf("%s.%s would be pruned from the API", path, tag)
			continue
		}
		checkFields(t, path+"."+tag, field.Type, child)
	}
}
