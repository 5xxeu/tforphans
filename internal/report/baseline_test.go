package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/state"
	"github.com/5xxeu/tforphans/internal/tf"
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
		reason              string
	}{
		{name: "scope complete", scope: "a", rows: []Coverage{{Type: "thing", Scope: "a", Status: Complete}}},
		{name: "scope incomplete", scope: "a", rows: []Coverage{{Type: "thing", Scope: "a", Status: Incomplete}}, reason: "listing incomplete"},
		{name: "scope missing", scope: "a", rows: []Coverage{{Type: "thing", Scope: "b", Status: Complete}}, reason: "scope not listed"},
		{name: "scope type missing", scope: "a", reason: "scope not listed"},
		{name: "no scope all complete", rows: []Coverage{{Type: "thing", Scope: "a", Status: Complete}, {Type: "thing", Scope: "b", Status: Complete}}},
		{name: "no scope one incomplete", rows: []Coverage{{Type: "thing", Scope: "a", Status: Complete}, {Type: "thing", Scope: "b", Status: Incomplete}}, reason: "listing incomplete"},
		{name: "type absent", rows: []Coverage{{Type: "other", Status: Complete}}, reason: "not listed by the same source"},
		{name: "skipped row", rows: []Coverage{{Type: "thing", Status: Skipped, Reasons: []string{"excluded"}}}, reason: "skipped: excluded"},
		{name: "skipped gap", rows: []Coverage{{Type: "thing", Status: Incomplete, Reasons: []string{"skipped: list config requires zone"}}}, reason: "skipped: list config requires zone"},
		{name: "plugin", source: "plugin1", rows: []Coverage{{Type: "thing", Status: Complete}}, reason: "plugin results cannot be verified"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := model.Resource{Type: "thing", Identity: map[string]any{"id": "old"}, ScopeName: c.scope, Source: c.source, DisplayName: "old name", ImportID: "import", ProviderSource: "aws"}
			s := Summary{Coverage: c.rows, Listings: []Listing{{Source: "other", Type: "other", Complete: true}}}
			// Explicit check scopes are separate from the coverage rows.
			if c.name != "type absent" {
				l := Listing{Source: c.source, Provider: "hashicorp/aws", Type: "thing", Complete: true}
				if c.scope != "" {
					l.Scopes = []string{"b"}
					if c.name == "scope complete" || c.name == "scope incomplete" {
						l.Scopes = []string{"a"}
					}
				}
				s.Listings = append(s.Listings, l)
			}
			_, b := Compare([]model.Resource{r}, nil, s)
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

func TestBaselineMissingSource(t *testing.T) {
	listed := Listing{Source: "infra", Provider: "hashicorp/foo", Type: "thing", Complete: true}
	other := func(source, provider, typ string) Listing {
		return Listing{Source: source, Provider: provider, Type: typ, Complete: true}
	}
	for _, c := range []struct {
		name, source, provider, scope string
		listings                      []Listing
		reason                        string
	}{
		{name: "missing listings", reason: "no listing evidence"},
		{name: "other source", listings: []Listing{listed}, reason: "not listed by the same source"},
		{name: "other type", source: "infra", listings: []Listing{other("infra", "hashicorp/foo", "other")}, reason: "not listed by the same source"},
		{name: "compatible provider", source: "infra", provider: "foo", listings: []Listing{listed}},
		{name: "compatible family", source: "infra", provider: "hashicorp/google-beta", listings: []Listing{other("infra", "registry.terraform.io/hashicorp/google", "thing")}},
		{name: "incompatible provider", source: "infra", provider: "acme/foo", listings: []Listing{listed}, reason: "type listed by another provider"},
		{name: "mixed providers", source: "infra", provider: "hashicorp/foo", listings: []Listing{listed, other("infra", "acme/foo", "thing")}, reason: "type listed by another provider"},
		{name: "unrelated incompatible source", source: "infra", provider: "hashicorp/foo", listings: []Listing{listed, other("elsewhere", "acme/foo", "thing")}},
		{name: "unrelated incompatible type", source: "infra", provider: "hashicorp/foo", listings: []Listing{listed, other("infra", "acme/foo", "other")}},
		{name: "unknown finding provider", source: "infra", listings: []Listing{listed, other("infra", "acme/foo", "thing")}},
		{name: "unknown listing provider", source: "infra", provider: "foo", listings: []Listing{other("infra", "", "thing")}},
		{name: "incomplete listing", source: "infra", listings: []Listing{{Source: "infra", Type: "thing"}}, reason: "listing incomplete"},
		{name: "explicit scope present", source: "infra", scope: "project P", listings: []Listing{{Source: "infra", Type: "thing", Scopes: []string{"project P"}, Complete: true}}},
		{name: "explicit scope absent", source: "infra", scope: "project P", listings: []Listing{{Source: "infra", Type: "thing", Scopes: []string{"project Q"}, Complete: true}}, reason: "scope not listed"},
		{name: "no explicit scopes", source: "infra", scope: "project P", listings: []Listing{listed}, reason: "scope not listed"},
		{name: "finding without scope", source: "infra", listings: []Listing{{Source: "infra", Type: "thing", Scopes: []string{"project Q"}, Complete: true}}},
		{name: "scan ran", source: "-scan cloudflare", listings: []Listing{other("-scan cloudflare", "cloudflare/cloudflare", "thing")}},
		{name: "another scan ran", source: "-scan cloudflare", listings: []Listing{other("-scan proxmox", "bpg/proxmox", "thing")}, reason: "not listed by the same source"},
		{name: "provider query cannot replace scan", source: "-scan cloudflare", listings: []Listing{listed}, reason: "not listed by the same source"},
		{name: "root named like scan", source: "cloudflare", listings: []Listing{other("cloudflare", "cloudflare/cloudflare", "thing")}},
		{name: "plugin", source: "plugin1", listings: []Listing{other("plugin1", "", "thing")}, reason: "plugin results cannot be verified"},
		{name: "plugin without evidence", source: "plugin123", reason: "plugin results cannot be verified"},
		{name: "root plugin-infra", source: "plugin-infra", listings: []Listing{other("plugin-infra", "hashicorp/foo", "thing")}},
		{name: "root plugin", source: "plugin", listings: []Listing{other("plugin", "hashicorp/foo", "thing")}},
		{name: "root plugin1-infra", source: "plugin1-infra", listings: []Listing{other("plugin1-infra", "hashicorp/foo", "thing")}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := model.Resource{Type: "thing", Identity: map[string]any{"id": "old"}, Source: c.source, ProviderSource: c.provider, ScopeName: c.scope}
			_, b := Compare([]model.Resource{r}, nil, Summary{Coverage: []Coverage{{Type: "thing", Scope: c.scope, Status: Complete}}, Listings: c.listings})
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

func TestBaselineProviderScopeMixing(t *testing.T) {
	r := model.Resource{Type: "foo_thing", Identity: map[string]any{"id": "old"}, ScopeName: "B", ProviderSource: "hashicorp/foo", Source: "root-b"}
	s := Summary{
		Listings: []Listing{{Source: "root-a", Provider: "hashicorp/foo", Type: "foo_thing", Scopes: []string{"A"}, Complete: true}, {Source: "root-b", Provider: "acme/foo", Type: "foo_thing", Scopes: []string{"B"}, Complete: true}},
		Coverage: BuildCoverage(nil, []tf.Check{
			{Type: "foo_thing", Scope: "A", Root: "root-a", Provider: "hashicorp/foo"},
			{Type: "foo_thing", Scope: "B", Root: "root-b", Provider: "acme/foo"},
		}, nil, nil),
	}
	_, b := Compare([]model.Resource{r}, nil, s)
	if b.Resolved != 0 || b.Unverified != 1 || b.UnverifiedFindings[0].Reason != "type listed by another provider" {
		t.Fatalf("provider listing from another scope resolved finding: %+v", b)
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
	if err := JSON(&buf, rs, nil, Summary{}); err != nil {
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

func TestBaselineRepeatedKeysScale(t *testing.T) {
	const n = 20000
	old := make([]model.Resource, n)
	current := make([]model.Resource, n)
	for i := range n {
		old[i] = model.Resource{Type: "thing", Identity: map[string]any{"id": "x"}, ProviderSource: fmt.Sprintf("acme%d/thing", i)}
		current[i] = model.Resource{Type: "thing", Identity: map[string]any{"id": "x"}, ProviderSource: fmt.Sprintf("other%d/thing", i)}
	}
	start := time.Now()
	_, b := Compare(old, current, Summary{})
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("%d repeated keys took %s", n, d)
	}
	if b.New != n || b.Known != 0 {
		t.Fatalf("new %d, known %d", b.New, b.Known)
	}
}
