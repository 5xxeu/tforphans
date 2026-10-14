package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/dsource"
	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/report"
)

func TestBaselineListingRun(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("TF_DATA_DIR", ".terraform")
	if err := os.Mkdir("plugin-infra", 0o700); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "terraform")
	script := `#!/bin/sh
case "$1" in
  version) printf '%s\n' '{"terraform_version":"1.14.0"}' ;;
  providers) printf '%s\n' '{"provider_schemas":{"registry.terraform.io/hashicorp/foo":{"list_resource_schemas":{"foo_thing":{"block":{"attributes":{"project":{"type":"string","optional":true},"zone":{"type":"string","optional":true}}}}}}}}' ;;
  state) printf '%s\n' '{"version":4,"resources":[]}' ;;
  query)
    if [ "$TFO_TEST_EMPTY" = 1 ]; then exit 0; fi
    if [ "$TFO_TEST_EXTRA" = 1 ]; then
      printf '%s\n' '{"list_start":{"address":"list.foo_thing.tfo_foo_foo_thing[\"project=Q\"]","resource_type":"foo_thing"}}' '{"list_complete":{"address":"list.foo_thing.tfo_foo_foo_thing[\"project=Q\"]","resource_type":"foo_thing","total":0}}'
    fi
    addr=list.foo_thing.tfo_foo_foo_thing
    if [ -n "$TFO_TEST_SCOPE" ]; then
      scope_key=project=$TFO_TEST_SCOPE
      if [ -n "$TFO_TEST_ZONE" ]; then scope_key=$scope_key,zone=$TFO_TEST_ZONE; fi
      addr="$addr[\"$scope_key\"]"
    fi
    printf '{"list_start":{"address":"%s","resource_type":"foo_thing"}}\n' "$(printf '%s' "$addr" | sed 's/"/\\"/g')"
    if [ "$TFO_TEST_FAILED" = 1 ]; then
      printf '{"diagnostic":{"severity":"error","summary":"permission denied","address":"%s"}}\n' "$(printf '%s' "$addr" | sed 's/"/\\"/g')"
      exit 1
    fi
    printf '{"list_complete":{"address":"%s","resource_type":"foo_thing","total":0}}\n' "$(printf '%s' "$addr" | sed 's/"/\\"/g')"
    ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, scope, zone, reason string
		failed, empty, extra      bool
		withoutScope              bool
	}{
		{name: "root named plugin-infra", withoutScope: true},
		{name: "deleted last object in scope", reason: "scope not listed"},
		{name: "explicit scope present", scope: "P"},
		{name: "project narrowed to one zone", scope: "P", zone: "Z", reason: "scope not listed"},
		{name: "plugin object cannot confirm another scope", scope: "Q", reason: "scope not listed"},
		{name: "strict ignored failure", failed: true, reason: "listing incomplete"},
		{name: "complete scopes sorted", scope: "P", extra: true},
		{name: "one failed instance", scope: "P", failed: true, extra: true, reason: "listing incomplete"},
		{name: "empty for_each", empty: true, reason: "no listing evidence"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("TFO_TEST_EXTRA", "0")
			if c.extra {
				t.Setenv("TFO_TEST_EXTRA", "1")
			}
			t.Setenv("TFO_TEST_ZONE", c.zone)
			t.Setenv("TFO_TEST_SCOPE", c.scope)
			t.Setenv("TFO_TEST_FAILED", "0")
			t.Setenv("TFO_TEST_EMPTY", "0")
			if c.failed {
				t.Setenv("TFO_TEST_FAILED", "1")
			}
			if c.empty {
				t.Setenv("TFO_TEST_EMPTY", "1")
			}
			path := filepath.Join(t.TempDir(), "baseline.json")
			old := model.Resource{Type: "foo_thing", Identity: map[string]any{"id": "gone"}, ScopeName: "project P", ProviderSource: "hashicorp/foo", Source: "plugin-infra"}
			if c.withoutScope {
				old.ScopeName = ""
			}
			saveReportTest(t, path, []model.Resource{old}, report.Summary{})
			args := []string{"-no-config", "-terraform", bin, "-current-workspace", "-format", "json", "-baseline", path, "-plugin", `printf '%s\n' '{"type":"foo_thing","identity":{"id":"other"},"scope":"project P"}'`, "plugin-infra"}
			if c.scope != "" {
				args = append(args, "-arg", "project="+c.scope)
			}
			if c.zone != "" {
				args = append(args, "-arg", "zone="+c.zone)
			}
			if c.failed {
				args = append(args, "-strict-ignore", "foo_thing", "-strict")
			}
			code, stdout, stderr := runReportTest(t, args...)
			if code != exitOrphans {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			var doc struct {
				Summary  report.Summary  `json:"summary"`
				Baseline report.Baseline `json:"baseline"`
			}
			if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
				t.Fatal(err)
			}
			b := doc.Baseline
			if c.reason == "" {
				if b.Resolved != 1 || b.Unverified != 0 {
					t.Fatalf("same source not resolved: %s", stdout)
				}
			} else if b.Resolved != 0 || b.Unverified != 1 || b.UnverifiedFindings[0].Reason != c.reason {
				t.Fatalf("want %q: %s", c.reason, stdout)
			}
			if strings.Contains(stdout, `"scans"`) || strings.Contains(stdout, `"queried"`) {
				t.Fatalf("global evidence retained: %s", stdout)
			}
			if c.empty {
				if len(doc.Summary.Listings) != 0 {
					t.Fatalf("empty listing has evidence: %s", stdout)
				}
				return
			}
			want := report.Listing{Source: "plugin-infra", Provider: "hashicorp/foo", Type: "foo_thing", Scopes: []string{}, Complete: !c.failed}
			if c.scope != "" {
				want.Scopes = []string{"project " + c.scope}
			}
			if c.zone != "" {
				want.Scopes = []string{"project " + c.scope + ", zone " + c.zone}
			}
			if c.extra {
				want.Scopes = []string{"project P", "project Q"}
			}
			if !reflect.DeepEqual(doc.Summary.Listings, []report.Listing{want}) {
				t.Fatalf("listing evidence: %s", stdout)
			}
		})
	}
}

func TestDiffWithoutListingEvidence(t *testing.T) {
	dir := t.TempDir()
	old, current := filepath.Join(dir, "old.json"), filepath.Join(dir, "new.json")
	saveReportTest(t, old, []model.Resource{{Type: "thing", Identity: map[string]any{"id": "gone"}, Source: "infra"}}, report.Summary{})
	// An older NEW report can have complete coverage and global query
	// evidence without tying either to the source that found the object.
	data := `{"schema_version":1,"orphans":[],"summary":{"complete":true,"providers":[{"provider":"hashicorp/foo","queried":["thing"]}],"scans":["foo"],"coverage":[{"type":"thing","status":"complete"}]}}`
	if err := os.WriteFile(current, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runReportTest(t, "diff", "-format", "json", old, current)
	var d report.Diff
	if err := json.Unmarshal([]byte(stdout), &d); err != nil {
		t.Fatal(err)
	}
	if code != exitOK || len(d.Resolved) != 0 || len(d.Unverified) != 1 || d.Unverified[0].Reason != "no listing evidence" {
		t.Fatalf("older NEW report exit %d: %s%s", code, stdout, stderr)
	}
}

func TestListingCheckScopes(t *testing.T) {
	for _, c := range []struct {
		address, scope string
	}{
		{"list.foo_thing.all", ""},
		{`list.foo_thing.all["project=P"]`, "project P"},
		{`list.foo_thing.all["zone=Z"]`, "zone Z"},
		{`list.foo_thing.all["zone=Z,project=P"]`, "project P, zone Z"},
		{`list.foo_thing.all["account_id=A,project=P"]`, "project P, account_id A"},
		{`list.foo_thing.all["custom-key"]`, "custom-key"},
	} {
		if got := listingScope(c.address); got != c.scope {
			t.Errorf("%s: scope %q, want %q", c.address, got, c.scope)
		}
	}
}

func TestUserListingScopes(t *testing.T) {
	for _, c := range []struct {
		name           string
		keyed, mixed   bool
		generatedFirst bool
		projects       []string
		resolved       bool
	}{
		{name: "config changed from P to Q", projects: []string{"Q"}},
		{name: "key P with config Q", keyed: true, projects: []string{"Q"}},
		{name: "P confirmed only by returned object", keyed: true, projects: []string{"P"}, resolved: true},
		{name: "last object deleted"},
		{name: "unscoped then scoped", mixed: true, projects: []string{"A"}, resolved: true},
		{name: "scoped then unscoped", mixed: true, generatedFirst: true, projects: []string{"A"}, resolved: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			t.Setenv("TF_DATA_DIR", ".terraform")
			if err := os.Mkdir("infra", 0o700); err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(dir, "terraform")
			script := `#!/bin/sh
case "$1" in
  version) printf '%s\n' '{"terraform_version":"1.14.0"}' ;;
  providers) printf '%s\n' '{"provider_schemas":{"registry.terraform.io/hashicorp/foo":{"list_resource_schemas":{"foo_thing":{"block":{"attributes":{"project":{"type":"string","optional":true}}}}}}}}' ;;
  state) printf '%s\n' '{"version":4,"resources":[]}' ;;
  query) cat query.json ;;
  *) exit 1 ;;
esac
`
			if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			writeConfig := func(project string) {
				t.Helper()
				key := ""
				if c.keyed {
					key = "  for_each = toset([\"project=P\"])\n"
				}
				// A tfo_ prefix alone does not make this a generated block.
				hcl := fmt.Sprintf("list \"foo_thing\" \"tfo_custom\" {\n%s  config { project = %q }\n}\n", key, project)
				if err := os.WriteFile("infra/user.tfquery.hcl", []byte(hcl), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			address := "list.foo_thing.tfo_custom"
			if c.keyed {
				address += `["project=P"]`
			}
			listing := func(address, id string, projects []string) string {
				t.Helper()
				var out strings.Builder
				enc := json.NewEncoder(&out)
				write := func(kind string, fields map[string]any) {
					fields["address"], fields["resource_type"] = address, "foo_thing"
					if err := enc.Encode(map[string]any{kind: fields}); err != nil {
						t.Fatal(err)
					}
				}
				write("list_start", map[string]any{})
				for _, project := range projects {
					write("list_resource_found", map[string]any{"identity": map[string]any{"id": id, "project": project}})
				}
				write("list_complete", map[string]any{"total": len(projects)})
				return out.String()
			}
			writeQuery := func(data string) {
				t.Helper()
				if err := os.WriteFile("infra/query.json", []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			oldProject := "P"
			if c.mixed {
				oldProject = "A"
			}
			writeConfig(oldProject)
			writeQuery(listing(address, "gone", []string{oldProject}))
			args := []string{"-no-config", "-terraform", bin, "-current-workspace", "-format", "json", "infra"}
			code, initial, stderr := runReportTest(t, args...)
			if code != exitOrphans {
				t.Fatalf("initial exit %d: %s%s", code, initial, stderr)
			}
			baselinePath := filepath.Join(dir, "baseline.json")
			if err := os.WriteFile(baselinePath, []byte(initial), 0o600); err != nil {
				t.Fatal(err)
			}
			writeConfig("Q")
			query := listing(address, "current", c.projects)
			wantScopes := []string{}
			for _, project := range c.projects {
				wantScopes = append(wantScopes, "project "+project)
			}
			if c.mixed {
				generated := listing(`list.foo_thing.tfo_foo_foo_thing["project=B"]`, "", nil)
				if c.generatedFirst {
					query = generated + query
				} else {
					query += generated
				}
				args = append(args, "-arg", "project=B")
				wantScopes = append(wantScopes, "project B")
			}
			writeQuery(query)
			code, stdout, stderr := runReportTest(t, append(args, "-baseline", baselinePath)...)
			wantCode := exitOK
			if len(c.projects) > 0 {
				wantCode = exitOrphans
			}
			if code != wantCode {
				t.Fatalf("current exit %d: %s%s", code, stdout, stderr)
			}
			var doc struct {
				Summary  report.Summary  `json:"summary"`
				Baseline report.Baseline `json:"baseline"`
			}
			if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
				t.Fatal(err)
			}
			want := []report.Listing{{Source: "infra", Provider: "hashicorp/foo", Type: "foo_thing", Scopes: wantScopes, Complete: true}}
			if !reflect.DeepEqual(doc.Summary.Listings, want) {
				t.Fatalf("listing scopes: %s", stdout)
			}
			b := doc.Baseline
			if c.resolved {
				if b.Resolved != 1 || b.Unverified != 0 {
					t.Fatalf("scope not resolved: %s", stdout)
				}
			} else if b.Resolved != 0 || b.Unverified != 1 || b.UnverifiedFindings[0].Reason != "scope not listed" {
				t.Fatalf("scope not left unverified: %s", stdout)
			}
		})
	}
}

func TestListingObjectScopes(t *testing.T) {
	var s report.Summary
	s.AddListing("infra", "hashicorp/google", "thing", "project B", false)
	addListingObjectScopes(&s, []model.Resource{
		{Source: "infra", ProviderSource: "hashicorp/google-beta", Type: "thing", Identity: map[string]any{"project": "A"}},
		{Source: "infra", ProviderSource: "registry.terraform.io/hashicorp/google", Type: "thing", ScopeName: "project A"},
		{Source: "plugin1", ProviderSource: "hashicorp/google", Type: "thing", ScopeName: "project C"},
		{Source: "elsewhere", ProviderSource: "hashicorp/google", Type: "thing", ScopeName: "project D"},
		{Source: "infra", ProviderSource: "acme/google", Type: "thing", ScopeName: "project E"},
		{Source: "infra", ProviderSource: "hashicorp/google", Type: "other", ScopeName: "project F"},
	})
	want := []report.Listing{{Source: "infra", Provider: "hashicorp/google", Type: "thing", Scopes: []string{"project A", "project B"}, Complete: false}}
	if !reflect.DeepEqual(s.Listings, want) {
		t.Fatalf("object scopes or completeness: %+v", s.Listings)
	}
}

func TestBaselineScanScopes(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("TF_PLUGIN_CACHE_DIR", dir)
	bin := filepath.Join(dir, "terraform")
	script := `#!/bin/sh
case "$1" in
  init) exit 0 ;;
  providers) printf '%s\n' '{"provider_schemas":{"hashicorp/foo":{"resource_schemas":{"foo_thing":{}},"data_source_schemas":{"foo_accounts":{"block":{"attributes":{"result":{"type":["list",["object",{"id":"string","name":"string"}]]}}}},"foo_things":{"block":{"attributes":{"account_id":{"type":"string","required":true},"result":{"type":["list",["object",{"id":"string"}]]}}}}}}}}' ;;
  apply)
    printf '{"version":4,"resources":[{"mode":"data","type":"foo_accounts","name":"all","instances":[{"attributes":{"result":[{"id":"a1","name":"A"}]}}]},{"mode":"data","type":"foo_things","name":"by_account_id","instances":[{"index_key":"a1","attributes":{"result":%s}}]}]}\n' "$TFO_TEST_SCAN_RESULT" > terraform.tfstate
    ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	sources["listingtest"] = dsource.Adapter{
		Name: "listingtest", Provider: "hashicorp/foo", Target: "account",
		Scopes: []dsource.Scope{{Arg: "account_id", Parent: "foo_accounts", Prefix: "accounts"}},
		Finish: func(resources []model.Resource, _ map[string]map[string]any) {
			for i := range resources {
				resources[i].ScopeName = "account C"
			}
		},
	}
	t.Cleanup(func() { delete(sources, "listingtest") })
	for _, c := range []struct {
		name, scope, result string
		resolved            bool
		wantScopes          []string
	}{
		{name: "explicit scan scope with no objects", scope: "account A", result: "[]", resolved: true, wantScopes: []string{"account A"}},
		{name: "returned scan object scope", scope: "account C", result: `[{"id":"current"}]`, resolved: true, wantScopes: []string{"account A", "account C"}},
		{name: "scope not scanned", scope: "account B", result: "[]", wantScopes: []string{"account A"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("TFO_TEST_SCAN_RESULT", c.result)
			path := filepath.Join(t.TempDir(), "baseline.json")
			old := model.Resource{Type: "foo_thing", Identity: map[string]any{"id": "gone"}, ScopeName: c.scope, ProviderSource: "hashicorp/foo", Source: "-scan listingtest"}
			saveReportTest(t, path, []model.Resource{old}, report.Summary{})
			code, stdout, stderr := runReportTest(t, "-no-config", "-no-query", "-terraform", bin, "-scan", "listingtest", "-scan-scope", "account_id=A", "-allow-unpinned", "-format", "json", "-baseline", path)
			wantCode := exitOK
			if c.result != "[]" {
				wantCode = exitOrphans
			}
			if code != wantCode {
				t.Fatalf("exit %d: %s%s", code, stdout, stderr)
			}
			var doc struct {
				Summary  report.Summary  `json:"summary"`
				Baseline report.Baseline `json:"baseline"`
			}
			if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
				t.Fatal(err)
			}
			want := []report.Listing{{Source: "-scan listingtest", Provider: "hashicorp/foo", Type: "foo_thing", Scopes: c.wantScopes, Complete: true}}
			if !reflect.DeepEqual(doc.Summary.Listings, want) {
				t.Fatalf("scan scopes: %s", stdout)
			}
			b := doc.Baseline
			if c.resolved {
				if b.Resolved != 1 || b.Unverified != 0 {
					t.Fatalf("scan scope not resolved: %s", stdout)
				}
			} else if b.Resolved != 0 || b.Unverified != 1 || b.UnverifiedFindings[0].Reason != "scope not listed" {
				t.Fatalf("scan scope not left unverified: %s", stdout)
			}
		})
	}
}
