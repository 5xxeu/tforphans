package cli

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/state"
)

func TestExplanationReasonContract(t *testing.T) {
	r := model.Resource{Type: "thing", Identity: map[string]any{"id": "secret-value"}}
	for _, tc := range []struct {
		name         string
		match        state.Match
		ignored      bool
		code, action string
		keys         []string
	}{
		{name: "confirmed", match: state.Match{Managed: true, Verified: true, Address: "thing.managed"}, code: "state_match"},
		{name: "ignored uncertain", match: state.Match{Managed: true, ScopeUnknown: true}, ignored: true, code: "ignore_rule"},
		{name: "cluster", match: state.Match{Managed: true, ScopeUnknown: true}, code: "scope_unverified"},
		{name: "empty discovery", match: state.Match{Managed: true, Unknown: []string{"project"}}, code: "discovery_identity_missing", keys: []string{"project"}},
		{name: "old state", match: state.Match{Managed: true, Unconfirmed: []string{"zone", "project", "zone"}}, code: "state_identity_missing", action: "refresh_state", keys: []string{"project", "zone"}},
		{name: "mixed missing", match: state.Match{Managed: true, Unknown: []string{"zone"}, Unconfirmed: []string{"project"}}, code: "discovery_identity_missing", keys: []string{"project", "zone"}},
		{name: "scope and keys", match: state.Match{Managed: true, ScopeUnknown: true, Unconfirmed: []string{"project"}}, code: "scope_unverified", keys: []string{"project"}},
		{name: "no state", code: "no_state_instance"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, _ := json.Marshal(tc.match)
			e := explain(r, tc.match, nil, 0, "ignore:1", tc.ignored)
			if e.ReasonCode != tc.code || e.Action != tc.action || !reflect.DeepEqual(e.MissingKeys, tc.keys) {
				t.Fatalf("metadata: %+v", e.ReasonDetails)
			}
			if e.Reason == "" {
				t.Fatal("human reason lost")
			}
			b, _ := json.Marshal(e.ReasonDetails)
			if strings.Contains(string(b), "secret-value") {
				t.Fatal("identity value in reason metadata")
			}
			after, _ := json.Marshal(tc.match)
			if string(before) != string(after) {
				t.Fatal("match mutated")
			}
		})
	}
	for _, tc := range []struct {
		rejections []state.Rejection
		code       string
	}{
		{nil, "identity_not_matched"}, {[]state.Rejection{{Why: "different project"}}, "identity_conflict"},
	} {
		if got := explain(r, state.Match{}, tc.rejections, 1, "", false); got.ReasonCode != tc.code {
			t.Fatalf("unmanaged: %+v", got)
		}
	}
}
