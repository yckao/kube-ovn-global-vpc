package siteplan

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	api "globalvpc.io/controller/api/v1alpha1"
	"globalvpc.io/controller/internal/siteconfig"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestSiteCRDPreservesLifecycleReceipts(t *testing.T) {
	raw, err := os.ReadFile("../../config/crd/networking.globalvpc.io_sitevpcs.yaml")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := yaml.ToJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	var crd map[string]interface{}
	if err = json.Unmarshal(encoded, &crd); err != nil {
		t.Fatal(err)
	}
	spec := crd["spec"].(map[string]interface{})
	if spec["scope"] != "Cluster" || spec["group"] != api.GroupVersion.Group {
		t.Fatal("API/CRD scope or group differs")
	}
	version := spec["versions"].([]interface{})[0].(map[string]interface{})
	if version["name"] != api.GroupVersion.Version {
		t.Fatal("API/CRD version differs")
	}
	if _, ok := version["subresources"].(map[string]interface{})["status"]; !ok {
		t.Fatal("status subresource missing")
	}
	root := version["schema"].(map[string]interface{})["openAPIV3Schema"].(map[string]interface{})
	props := root["properties"].(map[string]interface{})
	checkFields(t, "spec", reflect.TypeOf(api.SiteVpcSpec{}), props["spec"].(map[string]interface{}))
	checkFields(t, "status", reflect.TypeOf(api.SiteVpcStatus{}), props["status"].(map[string]interface{}))
	validations := props["spec"].(map[string]interface{})["x-kubernetes-validations"].([]interface{})
	if validations[0].(map[string]interface{})["rule"] != "self == oldSelf" {
		t.Fatal("first local API must reject unsafe topology/ownership updates")
	}
}

func TestSamplesBuildWithoutRemoteRegistration(t *testing.T) {
	cfg, err := siteconfig.Load("../../config/samples/site.json")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../config/samples/sitevpc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := yaml.ToJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	var v api.SiteVpc
	if err := json.Unmarshal(encoded, &v); err != nil {
		t.Fatal(err)
	}
	v.UID = "sample-server-uid"
	if _, err := Build(&v, cfg); err != nil {
		t.Fatal(err)
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
	if typ.Name() == "Time" && typ.PkgPath() == "k8s.io/apimachinery/pkg/apis/meta/v1" {
		if schema["type"] != "string" || schema["format"] != "date-time" {
			t.Errorf("%s must preserve RFC3339", path)
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
			t.Errorf("%s.%s would be silently pruned", path, tag)
			continue
		}
		checkFields(t, path+"."+tag, field.Type, child)
	}
}
