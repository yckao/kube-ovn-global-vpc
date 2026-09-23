package v1alpha2

import (
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"os"
	"reflect"
	"sigs.k8s.io/yaml"
	"strings"
	"testing"
)

func loadSchema(t *testing.T, plural string) *apiextv1.CustomResourceDefinition {
	t.Helper()
	raw, err := os.ReadFile("../../config/crd/platform.globalvpc.io_" + plural + ".yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextv1.CustomResourceDefinition
	if err = yaml.UnmarshalStrict(raw, &crd); err != nil {
		t.Fatal(err)
	}
	return &crd
}

func TestPlatformSchemasMatchAPITypes(t *testing.T) {
	for _, tc := range []struct {
		plural, kind string
		spec, status any
	}{
		{"vpcs", "VPC", VPCSpec{}, VPCStatus{}},
		{"subnets", "Subnet", SubnetSpec{}, SubnetStatus{}},
		{"networkbindings", "NetworkBinding", NetworkBindingSpec{}, NetworkBindingStatus{}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			crd := loadSchema(t, tc.plural)
			if crd.Spec.Scope != apiextv1.NamespaceScoped || crd.Spec.Group != GroupVersion.Group || crd.Spec.Names.Kind != tc.kind {
				t.Fatal("managed APIs must have the agreed names and project/location namespace boundary")
			}
			v := crd.Spec.Versions[0]
			if v.Name != GroupVersion.Version || !v.Served || !v.Storage || v.Subresources == nil || v.Subresources.Status == nil {
				t.Fatal("version or status subresource missing")
			}

			checkSchemaFields(t, "spec", reflect.TypeOf(tc.spec), v.Schema.OpenAPIV3Schema.Properties["spec"])
			checkSchemaFields(t, "status", reflect.TypeOf(tc.status), v.Schema.OpenAPIV3Schema.Properties["status"])
		})
	}
}

func checkSchemaFields(t *testing.T, path string, typ reflect.Type, schema apiextv1.JSONSchemaProps) {
	t.Helper()
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() == reflect.Slice {
		if schema.Type != "array" || schema.Items == nil || schema.Items.Schema == nil {
			t.Fatalf("%s must have structured array items", path)
		}
		checkSchemaFields(t, path+"[]", typ.Elem(), *schema.Items.Schema)
		return
	}
	if typ.Name() == "Time" && typ.PkgPath() == "k8s.io/apimachinery/pkg/apis/meta/v1" {
		if schema.Type != "string" || schema.Format != "date-time" {
			t.Errorf("%s loses timestamp format", path)
		}
		return
	}
	if typ.Kind() != reflect.Struct {
		return
	}
	seen := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		if tag == "-" {
			continue
		}
		seen[tag] = true
		s, ok := schema.Properties[tag]
		if !ok {
			t.Errorf("%s.%s would be pruned by API admission", path, tag)
			continue
		}
		checkSchemaFields(t, path+"."+tag, f.Type, s)
	}
	for k := range schema.Properties {
		if !seen[k] {
			t.Errorf("%s.%s has no typed API field", path, k)
		}
	}
}

func bindingFixture() NetworkBindingSpec {
	return NetworkBindingSpec{NetworkID: 11001, LocalASN: 64512, VPCRef: ObjectIdentity{Namespace: "project-red", Name: "red", UID: "vpc-uid"}, LocationRef: "dc-a", NetworkClassRef: "default", NativeVpcName: "pv-red-a", TransportProfile: "wireguard-bgp", Revision: "revision-1", Subnets: []BindingSubnet{{Name: "apps", UID: "subnet-uid", CIDR: "10.241.0.0/24", Gateway: "10.241.0.1", NativeSubnetName: "ps-red-apps"}}}
}

func TestAllManagedKindsRegistered(t *testing.T) {
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"VPC", "VPCList", "Subnet", "SubnetList", "NetworkBinding", "NetworkBindingList"} {
		if _, err := s.New(GroupVersion.WithKind(kind)); err != nil {
			t.Fatal(err)
		}
	}
}
