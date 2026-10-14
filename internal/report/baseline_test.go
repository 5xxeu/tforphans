package report

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/state"
)

func TestBaselineKeys(t *testing.T) {
	base := model.Resource{Type: "thing", Identity: map[string]any{"id": "a"}, ScopeName: "account a", ProviderSource: "hashicorp/aws"}
	for _, c := range []struct {
		name   string
		change func(*model.Resource, *model.Resource)
		known  bool
	}{
		{"names and addresses", func(a, b *model.Resource) {
			a.DisplayName, a.ListAddress, a.BaseName = "before", "list.before", "old"
			b.DisplayName, b.ListAddress, b.BaseName = "after", "list.after", "new"
		}, true},
		{"scope", func(_, b *model.Resource) { b.ScopeName = "account b" }, false},
		{"type", func(_, b *model.Resource) { b.Type = "other" }, false},
		{"unknown old provider", func(a, _ *model.Resource) { a.ProviderSource = "" }, true},
		{"unknown new provider", func(_, b *model.Resource) { b.ProviderSource = "" }, true},
		{"google beta", func(a, b *model.Resource) {
			a.ProviderSource, b.ProviderSource = "registry.terraform.io/hashicorp/google", "hashicorp/google-beta"
		}, true},
		{"other provider", func(_, b *model.Resource) { b.ProviderSource = "acme/aws" }, false},
		{"import id", func(a, b *model.Resource) { a.Identity, b.Identity, a.ImportID, b.ImportID = nil, nil, "abc", "abc" }, true},
		{"different import id", func(a, b *model.Resource) { a.Identity, b.Identity, a.ImportID, b.ImportID = nil, nil, "abc", "def" }, false},
		{"identity wins", func(a, b *model.Resource) { a.ImportID, b.ImportID = "abc", "def" }, true},
		{"no identity", func(a, b *model.Resource) { a.Identity, b.Identity = nil, nil }, false},
		{"nested canonical json", func(a, b *model.Resource) {
			a.Identity, b.Identity = nil, nil
			if err := model.DecodeJSON([]byte(`{"z":[null,true,9007199254740993],"a":{"b":"x","a":1}}`), &a.Identity); err != nil {
				t.Fatal(err)
			}
			if err := model.DecodeJSON([]byte(`{"a":{"a":1,"b":"x"},"z":[null,true,9007199254740993]}`), &b.Identity); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"typed values", func(a, b *model.Resource) {
			a.Identity, b.Identity = map[string]any{"id": json.Number("1")}, map[string]any{"id": "1"}
		}, false},
		{"null retained", func(a, b *model.Resource) {
			a.Identity, b.Identity = map[string]any{"id": "a", "x": nil}, map[string]any{"id": "a"}
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, b := base, base
			c.change(&a, &b)
			marked, result := Compare([]model.Resource{a}, []model.Resource{b}, Summary{Coverage: []Coverage{{Type: "thing", Scope: "account a", Status: Complete}}})
			want := "new"
			if c.known {
				want = "known"
			}
			if marked[0].Baseline != want || (result.Known == 1) != c.known || result.New+result.Known != 1 {
				t.Fatalf("marked %v, result %+v", marked, result)
			}
			if len(a.Identity) == 0 && a.ImportID == "" && result.Resolved != 0 {
				t.Fatal("unkeyable finding resolved")
			}
			if a.Baseline != "" || b.Baseline != "" {
				t.Fatal("comparison mutated input")
			}
		})
	}
	// Unknown provider matches must not erase an indexed provider family.
	old := []model.Resource{base, base}
	old[1].ProviderSource = "acme/aws"
	current := base
	current.ProviderSource = "acme/aws"
	_, result := Compare(old, []model.Resource{current}, Summary{})
	if result.Known != 1 || result.Unverified != 1 {
		t.Fatalf("multiple provider families: %+v", result)
	}
}

func TestBaselineMissingCoverage(t *testing.T) {
	for _, c := range []struct {
		name, scope, source string
		rows                []Coverage
		plugins             int
		warnings            []string
		reason              string
	}{
		{name: "scope complete", scope: "a", rows: []Coverage{{Type: "thing", Scope: "a", Status: Complete}}},
		{name: "scope incomplete", scope: "a", rows: []Coverage{{Type: "thing", Scope: "a", Status: Incomplete}}, reason: "listing incomplete"},
		{name: "scope missing", scope: "a", rows: []Coverage{{Type: "thing", Scope: "b", Status: Complete}}, reason: "scope not listed"},
		{name: "scope type missing", scope: "a", reason: "scope not listed"},
		{name: "no scope all complete", rows: []Coverage{{Type: "thing", Scope: "a", Status: Complete}, {Type: "thing", Scope: "b", Status: Complete}}},
		{name: "no scope one incomplete", rows: []Coverage{{Type: "thing", Scope: "a", Status: Complete}, {Type: "thing", Scope: "b", Status: Incomplete}}, reason: "listing incomplete"},
		{name: "type absent", rows: []Coverage{{Type: "other", Status: Complete}}, reason: "type not listed"},
		{name: "skipped row", rows: []Coverage{{Type: "thing", Status: Skipped, Reasons: []string{"excluded"}}}, reason: "skipped: excluded"},
		{name: "skipped gap", rows: []Coverage{{Type: "thing", Status: Incomplete, Reasons: []string{"skipped: list config requires zone"}}}, reason: "skipped: list config requires zone"},
		{name: "plugin failed", source: "plugin1", plugins: 2, rows: []Coverage{{Type: "thing", Status: Complete}}, warnings: []string{"plugin2: permission denied"}, reason: "plugin failed or not run"},
		{name: "plugin not run", source: "plugin1", rows: []Coverage{{Type: "thing", Status: Complete}}, reason: "plugin failed or not run"},
		{name: "plugin succeeded", source: "plugin1", plugins: 1, rows: []Coverage{{Type: "thing", Status: Complete}}},
		{name: "plugin coverage missing", source: "plugin1", plugins: 1, reason: "type not listed"},
		{name: "unrelated warning", source: "plugin1", plugins: 1, warnings: []string{"other warning"}, rows: []Coverage{{Type: "thing", Status: Complete}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := model.Resource{Type: "thing", Identity: map[string]any{"id": "old"}, ScopeName: c.scope, Source: c.source, DisplayName: "old name", ImportID: "import", ProviderSource: "aws"}
			_, b := Compare([]model.Resource{r}, nil, Summary{Coverage: c.rows, Plugins: c.plugins, Warnings: c.warnings})
			if c.reason == "" {
				if b.Resolved != 1 || b.Unverified != 0 || !reflect.DeepEqual(b.ResolvedFindings[0], finding(r, "")) {
					t.Fatalf("resolved: %+v", b)
				}
			} else if b.Resolved != 0 || b.Unverified != 1 || b.UnverifiedFindings[0].Reason != c.reason {
				t.Fatalf("want reason %q: %+v", c.reason, b)
			}
		})
	}
}

func TestReadSnapshotErrors(t *testing.T) {
	for _, c := range []struct {
		name, data, want string
		limit            int64
	}{
		{"version", `{"schema_version":2,"orphans":[]}`, "schema_version 2", state.DefaultMaxSize},
		{"array", `[]`, "JSON object", state.DefaultMaxSize},
		{"null", `null`, "JSON object", state.DefaultMaxSize},
		{"trailing data", `{"schema_version":1,"orphans":[]} {}`, "unexpected data", state.DefaultMaxSize},
		{"too large", `{"schema_version":1,"orphans":[]}`, "larger than 10 bytes", 10},
		{"missing orphans", `{"schema_version":1}`, "orphans must be an array", state.DefaultMaxSize},
		{"null orphans", `{"schema_version":1,"orphans":null}`, "orphans must be an array", state.DefaultMaxSize},
		{"object orphans", `{"schema_version":1,"orphans":{}}`, "orphans must be an array", state.DefaultMaxSize},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "report.json")
			if err := os.WriteFile(path, []byte(c.data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadSnapshot(path, c.limit); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q: %v", c.want, err)
			}
		})
	}
	if _, err := ReadSnapshot("-", state.DefaultMaxSize); err == nil {
		t.Fatal("accepted stdin")
	}
	if _, err := ReadSnapshot(filepath.Join(t.TempDir(), "missing"), state.DefaultMaxSize); err == nil {
		t.Fatal("accepted missing file")
	}
}

func TestSnapshotResourceRoundTrip(t *testing.T) {
	var rs []model.Resource
	if err := model.DecodeJSON([]byte(`[{"type":"thing","identity":{"nested":{"b":true,"a":9007199254740993}},"scope":"a","display_name":"name","provider_source":"aws","import_id":"id","source":"plugin1"}]`), &rs); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := JSON(&buf, rs, nil, Summary{Plugins: 1}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := ReadSnapshot(path, state.DefaultMaxSize)
	if err != nil {
		t.Fatal(err)
	}
	var again bytes.Buffer
	if err := JSON(&again, s.Orphans, nil, s.Summary); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), again.Bytes()) {
		t.Fatalf("round trip changed output:\n%s\n%s", buf.String(), again.String())
	}
}

func TestBaselineUnkeyableNeverResolved(t *testing.T) {
	r := model.Resource{Type: "thing", DisplayName: "same", ScopeName: "a"}
	marked, b := Compare([]model.Resource{r}, []model.Resource{r}, Summary{Coverage: []Coverage{{Type: "thing", Scope: "a", Status: Complete}}})
	if marked[0].Baseline != "new" || b.New != 1 || b.Resolved != 0 || b.Unverified != 1 || b.UnverifiedFindings[0].Reason != "identity and import_id missing" {
		t.Fatalf("unkeyable findings: %+v", b)
	}
}
