package report

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/model"
)

func TestDiff(t *testing.T) {
	r := func(id, scope string) model.Resource {
		return model.Resource{Type: "thing", Identity: map[string]any{"id": id}, ScopeName: scope}
	}
	old := Snapshot{
		Orphans: []model.Resource{r("known", "a"), r("resolved", "a"), r("unverified", "b")},
		Summary: Summary{Live: 4, Managed: 1, Uncertain: 2, Ignored: 3, Orphans: 3, States: 1, Queried: 2, Complete: true,
			Coverage: []Coverage{{Type: "thing", Scope: "a", Status: Complete}, {Type: "thing", Scope: "b", Status: Complete}, {Type: "removed", Status: Skipped}}},
	}
	current := Snapshot{
		Orphans: []model.Resource{r("known", "a"), r("added", "a")},
		Summary: Summary{Live: 5, Managed: 1, Uncertain: 0, Ignored: 4, Orphans: 2, States: 2, Queried: 3,
			Listings: []Listing{{Provider: "hashicorp/foo", Type: "thing", Scopes: []string{"a", "b"}, Complete: true}},
			Coverage: []Coverage{{Type: "thing", Scope: "a", Status: Complete}, {Type: "thing", Scope: "b", Status: Incomplete}, {Type: "introduced", Status: Complete}}},
	}
	d := CompareSnapshots(old, current)
	if len(d.Added) != 1 || d.Added[0].IdentityString() != "id=added" || len(d.Resolved) != 1 || d.Resolved[0].Identity["id"] != "resolved" ||
		len(d.Unverified) != 1 || d.Unverified[0].Reason != "listing incomplete" || d.Complete != (CompleteChange{true, false}) {
		t.Fatalf("findings: %+v", d)
	}
	wantCounts := map[string]CounterChange{
		"live": {4, 5}, "managed": {1, 1}, "uncertain": {2, 0}, "ignored": {3, 4}, "orphans": {3, 2}, "states": {1, 2}, "queried_types": {2, 3},
	}
	if !reflect.DeepEqual(d.Counters, wantCounts) {
		t.Fatalf("counters: %+v", d.Counters)
	}
	wantCoverage := []CoverageChange{{"introduced", "", "absent", Complete}, {"removed", "", Skipped, "absent"}, {"thing", "b", Complete, Incomplete}}
	if !reflect.DeepEqual(d.CoverageChanges, wantCoverage) {
		t.Fatalf("coverage: %+v", d.CoverageChanges)
	}
	var text, js bytes.Buffer
	if err := d.Text(&text, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"added (1):", "thing id=added (a)", "resolved (1):", "not verified (1):", "listing incomplete", "live: 4 to 5", "complete: true to false"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text missing %q: %s", want, text.String())
		}
	}
	if strings.Contains(text.String(), "managed: 1 to 1") {
		t.Fatal("text includes unchanged counter")
	}
	if err := d.JSON(&js); err != nil {
		t.Fatal(err)
	}
	var decoded Diff
	if err := model.DecodeJSON(js.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Counters) != 7 || !reflect.DeepEqual(decoded.CoverageChanges, wantCoverage) {
		t.Fatalf("JSON: %s", js.String())
	}
}

func TestDiffTextLimits(t *testing.T) {
	var old, current Snapshot
	for i := range 10 {
		typ := fmt.Sprintf("thing_%02d", i)
		current.Summary.AddListing("", "hashicorp/foo", typ, "", true)
		old.Orphans = append(old.Orphans, model.Resource{Type: typ, Identity: map[string]any{"id": "old"}})
		current.Orphans = append(current.Orphans, model.Resource{Type: typ, Identity: map[string]any{"id": "new"}})
		current.Summary.Coverage = append(current.Summary.Coverage, Coverage{Type: typ, Status: Complete})
	}
	d := CompareSnapshots(old, current)
	if len(d.Added) != 10 || len(d.Resolved) != 10 || len(d.Unverified) != 0 {
		t.Fatalf("findings: %+v", d)
	}
	for _, verbose := range []bool{false, true} {
		var buf bytes.Buffer
		if err := d.Text(&buf, verbose); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(buf.String(), "thing_09") != verbose || strings.Contains(buf.String(), "and 2 more") == verbose {
			t.Fatalf("verbose=%t: %s", verbose, buf.String())
		}
	}
}
