package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/state"
)

func TestOwnershipJSON(t *testing.T) {
	serial := int64(0)
	confirmed := state.Duplicate{Type: "aws_instance", Provider: "aws", Identity: map[string]any{"id": "i-1"}, Confidence: "confirmed", Owners: []state.Owner{
		{Address: "aws_instance.a", Lineage: "a", Serial: &serial, Sources: []string{"checkout-a", "checkout-b"}},
		{Address: "aws_instance.b", Lineage: "b", Serial: &serial, Sources: []string{"b"}},
	}}
	possible := state.Duplicate{Type: "aws_instance", Provider: "aws", ID: "i-2", Confidence: "possible", Reason: "lineage_unknown", Owners: []state.Owner{
		{Address: "aws_instance.a", Sources: []string{"-"}},
		{Address: "aws_instance.b", Lineage: "b", Serial: &serial, Sources: []string{"b"}},
	}}
	s := Summary{DuplicateOwnership: 1, PossibleDuplicateOwnership: 1, DuplicateInState: 1, Ownership: []state.Duplicate{confirmed, possible}, InState: []state.Duplicate{confirmed}}
	var b bytes.Buffer
	if err := JSON(&b, nil, nil, s); err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if string(got["schema_version"]) != "1" {
		t.Fatal("schema changed")
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(got["duplicate_ownership"], &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0]["reason"] != nil || entries[0]["id"] != nil || entries[1]["identity"] != nil || string(entries[1]["id"]) != `"i-2"` {
		t.Fatalf("fields: %s", &b)
	}
	var owners []map[string]json.RawMessage
	if err := json.Unmarshal(entries[1]["owners"], &owners); err != nil {
		t.Fatal(err)
	}
	if owners[0]["serial"] != nil || owners[0]["lineage"] != nil || string(owners[1]["serial"]) != "0" {
		t.Fatalf("unknown metadata: %s", &b)
	}
	var summary map[string]json.RawMessage
	if err := json.Unmarshal(got["summary"], &summary); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"duplicate_ownership", "possible_duplicate_ownership", "duplicate_in_state"} {
		if string(summary[name]) != "1" {
			t.Fatalf("%s: %s", name, &b)
		}
	}
	b.Reset()
	if err := JSON(&b, nil, nil, Summary{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), `"duplicate_ownership": [`) || strings.Contains(b.String(), `"duplicate_in_state": [`) {
		t.Fatal("empty optional arrays not omitted")
	}
}
