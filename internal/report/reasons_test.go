package report

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/tf"
)

func TestCoverageReasonContract(t *testing.T) {
	failures := []tf.Failure{
		{Type: "thing", Scope: "zone a", Category: tf.FailDenied},
		{Type: "thing", Scope: "zone a", Category: tf.FailTruncated},
	}
	skipped := []tf.Skip{
		{Type: "thing", Root: "r", Reason: "requires inputs", ReasonDetails: model.Reason("missing_argument", "zone")},
		{Type: "thing", Root: "r", Reason: "requires inputs", ReasonDetails: model.Reason("missing_argument", "project")},
		{Type: "disabled", Root: "r", Excluded: true, Reason: StrictIgnored + tf.FailAPIOff, ReasonDetails: model.Reason("api_disabled")},
	}
	cov := BuildCoverage(nil, nil, skipped, failures)
	if len(cov) != 3 {
		t.Fatalf("rows: %+v", cov)
	}
	wantActions := map[string]string{"permission_denied": "grant_permission", "limit_reached": "raise_limit", "api_disabled": "enable_api", "missing_argument": ""}
	for _, c := range cov {
		if len(c.Reasons) != len(c.ReasonDetails) {
			t.Fatalf("unpaired metadata: %+v", c)
		}
		for i, d := range c.ReasonDetails {
			if d.Reason != c.Reasons[i] {
				t.Fatalf("order: %+v", c)
			}
			action, ok := wantActions[d.ReasonCode]
			if !ok || action != d.Action {
				t.Fatalf("action: %+v", d)
			}
			if d.ReasonCode == "missing_argument" && !reflect.DeepEqual(d.MissingKeys, []string{"project", "zone"}) {
				t.Fatalf("merged keys: %+v", d)
			}
		}
	}
	complete := BuildCoverage(nil, []tf.Check{{Type: "thing"}}, nil, nil)
	data, _ := json.Marshal(complete)
	if strings.Contains(string(data), "reason") {
		t.Fatalf("complete has reasons: %s", data)
	}
}

func TestBaselineReasonContract(t *testing.T) {
	base := Listing{Source: "infra", Address: "list.thing.all", Type: "thing", Provider: "acme/test", Fingerprint: "same", Complete: true, Scope: map[string]string{"project": "p"}}
	resource := model.Resource{Type: "thing", Source: base.Source, ListAddress: base.Address, Identity: map[string]any{"id": "x"}}
	for _, tc := range []struct {
		name, code string
		change     func(*Summary, *Summary)
	}{
		{"environment", "environment_changed", func(a, b *Summary) { b.Environment = "other" }},
		{"not run", "listing_not_run", func(a, b *Summary) { b.Listings = nil }},
		{"incomplete", "listing_incomplete", func(a, b *Summary) { b.Listings[0].Complete = false }},
		{"configuration", "configuration_changed", func(a, b *Summary) { b.Listings[0].Fingerprint = "different" }},
		{"scope", "scope_changed", func(a, b *Summary) { b.Listings[0].Scope = map[string]string{"project": "other"} }},
		{"unknown scope", "scope_unverified", func(a, b *Summary) { b.Listings[0].Scope = nil }},
		{"provider", "provider_changed", func(a, b *Summary) { b.Listings[0].Provider = "other/test" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := Summary{Listings: []Listing{base}}, Summary{Listings: []Listing{base}}
			tc.change(&a, &b)
			_, got := Compare([]model.Resource{resource}, nil, a, b)
			if got.Unverified != 1 || got.UnverifiedFindings[0].ReasonCode != tc.code {
				t.Fatalf("baseline: %+v", got)
			}
			var text bytes.Buffer
			if err := PrintFindings(&text, got.UnverifiedFindings, -1); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(text.String(), "docs/reasons.md#"+tc.code) {
				t.Fatalf("no documentation link: %s", text.String())
			}
		})
	}
	a := Summary{Listings: []Listing{base}}
	_, got := CompareRun([]model.Resource{resource}, nil, a, a, nil, func(typ string) bool { return typ == "thing" })
	if got.Unverified != 1 || got.UnverifiedFindings[0].ReasonCode != "type_filtered" {
		t.Fatalf("filtered type: %+v", got)
	}
	seen := []Seen{{Resource: resource, Status: BecameUnsure, ReasonDetails: model.Reason("state_identity_missing", "project")}}
	_, got = CompareRun([]model.Resource{resource}, nil, a, a, seen, nil)
	if got.Unverified != 1 || got.UnverifiedFindings[0].Action != "refresh_state" || !reflect.DeepEqual(got.UnverifiedFindings[0].MissingKeys, []string{"project"}) {
		t.Fatalf("uncertain transition: %+v", got)
	}
}

func TestFailureReasonReference(t *testing.T) {
	docs, err := os.ReadFile("../../docs/reasons.md")
	if err != nil {
		t.Fatal(err)
	}
	for category, code := range map[string]string{
		tf.FailMissingArg: "missing_argument", tf.FailAPIOff: "api_disabled", tf.FailDenied: "permission_denied",
		tf.FailNotFound: "not_found", tf.FailTruncated: "limit_reached", tf.FailCrash: "provider_crashed",
		tf.FailRetired: "api_retired", tf.FailProvider: "provider_error", tf.FailRejected: "request_rejected",
		tf.FailOther: "listing_failed", "future category": "listing_failed",
	} {
		d := (tf.Failure{Category: category}).ReasonDetails()
		if d.ReasonCode != code {
			t.Fatalf("%s: %+v", category, d)
		}
		if !strings.Contains(string(docs), `id="`+code+`"`) {
			t.Fatalf("undocumented code: %s", code)
		}
	}
}

func TestCoverageSameTextDifferentCauses(t *testing.T) {
	skips := []tf.Skip{
		{Type: "thing", Reason: "same text", ReasonDetails: model.Reason("state_identity_missing", "project")},
		{Type: "thing", Reason: "same text", ReasonDetails: model.Reason("permission_denied")},
	}
	a := BuildCoverage(nil, nil, skips, nil)
	skips[0], skips[1] = skips[1], skips[0]
	b := BuildCoverage(nil, nil, skips, nil)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("source order changes metadata: %+v vs %+v", a, b)
	}
	if len(a) != 1 || len(a[0].Reasons) != 1 || len(a[0].ReasonDetails) != 2 {
		t.Fatalf("lost cause: %+v", a)
	}
	causes := a[0].ReasonDetails
	if causes[0].ReasonCode != "permission_denied" || causes[0].Action != "grant_permission" || len(causes[0].MissingKeys) != 0 || causes[1].ReasonCode != "state_identity_missing" || causes[1].Action != "refresh_state" || !reflect.DeepEqual(causes[1].MissingKeys, []string{"project"}) {
		t.Fatalf("mixed metadata: %+v", causes)
	}
}
