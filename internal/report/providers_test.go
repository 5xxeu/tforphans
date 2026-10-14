package report

import (
	"reflect"
	"testing"

	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/tf"
)

func TestMergeProviders(t *testing.T) {
	var a, b tf.Schemas
	for _, c := range []struct {
		s    *tf.Schemas
		data string
	}{
		{&a, `{"provider_schemas":{"registry.terraform.io/hashicorp/aws":{"resource_schemas":{"a":{},"b":{}},"list_resource_schemas":{"a":{}}},"acme/other":{"resource_schemas":{"x":{}}}}}`},
		{&b, `{"provider_schemas":{"registry.terraform.io/hashicorp/aws":{"resource_schemas":{"a":{}},"list_resource_schemas":{"a":{},"b":{}}}}}`},
	} {
		if err := model.DecodeJSON([]byte(c.data), c.s); err != nil {
			t.Fatal(err)
		}
	}
	var s Summary
	s.MergeProviders(&a, map[string][]string{"registry.terraform.io/hashicorp/aws": {"a", "a"}})
	s.MergeProviders(&b, map[string][]string{"registry.terraform.io/hashicorp/aws": {"a", "b"}})
	if len(s.Providers) != 2 {
		t.Fatalf("providers: %+v", s.Providers)
	}
	var got []int
	for _, p := range s.Providers {
		got = append(got, p.ResourceTypes, p.ListTypes, p.QueriedTypes)
	}
	if s.Providers[0].Provider != "acme/other" || s.Providers[1].Provider != "registry.terraform.io/hashicorp/aws" || !reflect.DeepEqual(got, []int{1, 0, 0, 2, 2, 2}) {
		t.Fatalf("providers: %+v", s.Providers)
	}
}
