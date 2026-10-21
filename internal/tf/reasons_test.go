package tf

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestMissingArgumentReason(t *testing.T) {
	var s Schemas
	if err := json.Unmarshal([]byte(`{"provider_schemas":{"registry.terraform.io/acme/test":{"list_resource_schemas":{"test_thing":{"block":{"attributes":{"zone":{"type":"string","required":true},"project":{"type":"string","required":true}}}}}}}}`), &s); err != nil {
		t.Fatal(err)
	}
	p := BuildPlan(&s, nil, PlanOptions{})
	if len(p.Skipped) != 1 {
		t.Fatalf("plan: %+v", p)
	}
	sk := p.Skipped[0]
	if sk.ReasonCode != "missing_argument" || sk.Action != "" || !reflect.DeepEqual(sk.MissingKeys, []string{"project", "zone"}) {
		t.Fatalf("skip: %+v", sk)
	}
}
