package report

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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
			marked, result := Compare([]model.Resource{a}, []model.Resource{b}, Summary{}, Summary{Coverage: []Coverage{{Type: "thing", Scope: "account a", Status: Complete}}})
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
	_, result := Compare(old, []model.Resource{current}, Summary{}, Summary{})
	if result.Known != 1 || result.Unverified != 1 {
		t.Fatalf("multiple provider families: %+v", result)
	}
}

func TestBaselineMissingListing(t *testing.T) {
	base := Listing{Source: "infra", Address: `list.thing.all["Z2"]`, Provider: "hashicorp/foo", Type: "thing", Complete: true, Fingerprint: "same"}
	for _, c := range []struct {
		name, reason string
		change       func(*model.Resource, *Summary, *Summary)
	}{
		{"same complete listing", "", func(_ *model.Resource, _, _ *Summary) {}},
		{"missing finding address", "no listing evidence", func(r *model.Resource, _, _ *Summary) { r.ListAddress = "" }},
		{"older baseline without listings", "no listing evidence", func(_ *model.Resource, old, _ *Summary) { old.Listings = nil }},
		{"old listing has no fingerprint", "no listing evidence", func(_ *model.Resource, old, _ *Summary) { old.Listings[0].Fingerprint = "" }},
		{"wrong old address", "no listing evidence", func(_ *model.Resource, old, _ *Summary) { old.Listings[0].Address = "list.thing.other" }},
		{"wrong old source", "no listing evidence", func(_ *model.Resource, old, _ *Summary) { old.Listings[0].Source = "elsewhere" }},
		{"older current without listings", "not listed this run", func(_ *model.Resource, _, now *Summary) { now.Listings = nil }},
		{"other source", "not listed this run", func(_ *model.Resource, _, now *Summary) { now.Listings[0].Source = "elsewhere" }},
		{"for_each instance missing", "not listed this run", func(_ *model.Resource, _, now *Summary) { now.Listings[0].Address = `list.thing.all["Z1"]` }},
		{"changed configuration", "listing configuration changed", func(_ *model.Resource, _, now *Summary) { now.Listings[0].Fingerprint = "changed" }},
		{"incompatible provider", "listed by another provider", func(_ *model.Resource, _, now *Summary) { now.Listings[0].Provider = "acme/foo" }},
		{"compatible provider family", "", func(_ *model.Resource, old, now *Summary) {
			old.Listings[0].Provider = "hashicorp/google-beta"
			now.Listings[0].Provider = "registry.terraform.io/hashicorp/google"
		}},
		{"unknown provider", "", func(_ *model.Resource, _, now *Summary) { now.Listings[0].Provider = "" }},
		{"incomplete", "listing incomplete", func(_ *model.Resource, _, now *Summary) { now.Listings[0].Complete = false }},
		// A root whose results were dropped (it timed out after its query)
		// proves nothing, even with listings it recorded before.
		{"failed source", "listing incomplete", func(_ *model.Resource, _, now *Summary) {
			now.FailSource("infra", errors.New("gave up after 1s (-source-timeout)"))
		}},
		{"incomplete before fingerprint", "listing incomplete", func(_ *model.Resource, _, now *Summary) {
			now.Listings[0].Complete = false
			now.Listings[0].Fingerprint = "changed"
		}},
		{"provider before incomplete", "listed by another provider", func(_ *model.Resource, _, now *Summary) {
			now.Listings[0].Complete = false
			now.Listings[0].Provider = "acme/foo"
		}},
		{"plugin", "plugin results cannot be verified", func(r *model.Resource, old, now *Summary) {
			r.Source = "plugin1"
			old.Listings[0].Source = r.Source
			now.Listings[0].Source = r.Source
		}},
		{"plugin without evidence", "plugin results cannot be verified", func(r *model.Resource, old, now *Summary) {
			r.Source = "plugin123"
			old.Listings = nil
			now.Listings = nil
		}},
		{"root plugin-infra", "", func(r *model.Resource, old, now *Summary) {
			r.Source = "plugin-infra"
			old.Listings[0].Source = r.Source
			now.Listings[0].Source = r.Source
		}},
		{"root plugin", "", func(r *model.Resource, old, now *Summary) {
			r.Source = "plugin"
			old.Listings[0].Source = r.Source
			now.Listings[0].Source = r.Source
		}},
		{"scan same scope", "", func(r *model.Resource, old, now *Summary) {
			r.Source = "-scan cloudflare"
			old.Listings[0].Source = r.Source
			now.Listings[0].Source = r.Source
		}},
		{"coverage does not decide resolution", "", func(_ *model.Resource, _, now *Summary) {
			now.Coverage = []Coverage{{Type: "thing", Status: Incomplete, Reasons: []string{"skipped: excluded"}}}
		}},
		{"unrelated listing failure", "", func(_ *model.Resource, _, now *Summary) {
			now.Listings = append(now.Listings, Listing{Source: "infra", Address: "list.thing.other", Provider: "acme/foo", Type: "thing"})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := model.Resource{Type: "thing", Identity: map[string]any{"id": "gone"}, Source: base.Source, ListAddress: base.Address, ScopeName: "project P", ProviderSource: base.Provider}
			old, now := Summary{Listings: []Listing{base}}, Summary{Listings: []Listing{base}}
			c.change(&r, &old, &now)
			_, b := Compare([]model.Resource{r}, nil, old, now)
			if c.reason == "" {
				if b.Resolved != 1 || b.Unverified != 0 || !reflect.DeepEqual(b.ResolvedFindings[0], finding(r, "")) {
					t.Fatalf("resolved: %+v", b)
				}
			} else if b.Resolved != 0 || b.Unverified != 1 || b.UnverifiedFindings[0].Reason != c.reason {
				t.Fatalf("want %q: %+v", c.reason, b)
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
			if _, err := ReadSnapshot(context.Background(), path, c.limit); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q: %v", c.want, err)
			}
		})
	}
	if _, err := ReadSnapshot(context.Background(), "-", state.DefaultMaxSize); err == nil {
		t.Fatal("accepted stdin")
	}
	if _, err := ReadSnapshot(context.Background(), filepath.Join(t.TempDir(), "missing"), state.DefaultMaxSize); err == nil {
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
	s, err := ReadSnapshot(context.Background(), path, state.DefaultMaxSize)
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
	marked, b := Compare([]model.Resource{r}, []model.Resource{r}, Summary{}, Summary{Coverage: []Coverage{{Type: "thing", Scope: "a", Status: Complete}}})
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
	_, b := Compare(old, current, Summary{}, Summary{})
	if d := time.Since(start); d > 30*time.Second { // a guard against quadratic work, see the benchmarks
		t.Fatalf("%d repeated keys took %s", n, d)
	}
	if b.New != n || b.Known != 0 {
		t.Fatalf("new %d, known %d", b.New, b.Known)
	}
}

func BenchmarkBaseline(b *testing.B) {
	for _, n := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			old := make([]model.Resource, n)
			current := make([]model.Resource, n)
			for i := range n {
				old[i] = model.Resource{Type: "thing", Identity: map[string]any{"id": fmt.Sprint(i)}, ProviderSource: "acme/thing"}
				current[i] = model.Resource{Type: "thing", Identity: map[string]any{"id": fmt.Sprint(i + n/2)}, ProviderSource: "acme/thing"}
			}
			b.ReportAllocs()
			for b.Loop() {
				Compare(old, current, Summary{}, Summary{})
			}
		})
	}
}
