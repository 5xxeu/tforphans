package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

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
	}{
		{name: "root named plugin-infra"},
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
