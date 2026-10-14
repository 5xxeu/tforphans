package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/dsource"
	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/report"
)

func runReportTest(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func saveReportTest(t *testing.T, path string, rs []model.Resource, s report.Summary) {
	t.Helper()
	var buf bytes.Buffer
	if err := report.JSON(&buf, rs, nil, s); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBaselineRun(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	statePath := filepath.Join(dir, "empty.tfstate")
	if err := os.WriteFile(statePath, []byte(`{"version":4,"resources":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	base := []string{"-no-config", "-no-query", "-state", statePath, "-plugin", `printf '%s\n' '{"type":"vault_policy","identity":{"id":"known"},"display_name":"changed"}'`}
	baselinePath := filepath.Join(dir, "baseline.json")
	known := model.Resource{Type: "vault_policy", Identity: map[string]any{"id": "known"}, DisplayName: "before", ListAddress: "before", Source: "plugin1"}
	saveReportTest(t, baselinePath, []model.Resource{known}, report.Summary{})
	code, plain, stderr := runReportTest(t, append(base, "-format", "json")...)
	if code != exitOrphans {
		t.Fatalf("plain run exit %d: %s", code, stderr)
	}
	if strings.Contains(plain, `"baseline"`) || strings.Contains(plain, `"providers"`) || strings.Contains(plain, `"scans"`) {
		t.Fatalf("plain run has optional fields: %s", plain)
	}
	code, marked, stderr := runReportTest(t, append(base, "-format", "json", "-baseline", baselinePath)...)
	if code != exitOK || !strings.Contains(stderr, "baseline: 0 new, 1 known (not shown), 0 resolved, 0 not verified") {
		t.Fatalf("known run exit %d: %s", code, stderr)
	}
	var a, b map[string]any
	if err := model.DecodeJSON([]byte(plain), &a); err != nil {
		t.Fatal(err)
	}
	if err := model.DecodeJSON([]byte(marked), &b); err != nil {
		t.Fatal(err)
	}
	if b["orphans"].([]any)[0].(map[string]any)["baseline"] != "known" {
		t.Fatalf("known finding not marked: %s", marked)
	}
	delete(b, "baseline")
	delete(b["orphans"].([]any)[0].(map[string]any), "baseline")
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("baseline changed other JSON fields:\n%s\n%s", plain, marked)
	}
	// Re-encoding the report with annotations removed must reproduce every byte.
	var decoded struct {
		Orphans   []model.Resource   `json:"orphans"`
		Uncertain []report.Uncertain `json:"uncertain"`
		Summary   report.Summary     `json:"summary"`
	}
	if err := model.DecodeJSON([]byte(marked), &decoded); err != nil {
		t.Fatal(err)
	}
	decoded.Orphans[0].Baseline = ""
	var unchanged bytes.Buffer
	if err := report.JSON(&unchanged, decoded.Orphans, decoded.Uncertain, decoded.Summary); err != nil {
		t.Fatal(err)
	}
	if unchanged.String() != plain {
		t.Fatalf("unannotated JSON changed bytes:\n%s\n%s", plain, unchanged.String())
	}
	for _, format := range []string{"text", "summary"} {
		code, stdout, stderr := runReportTest(t, append(base, "-baseline", baselinePath, "-format", format)...)
		if code != exitOK || strings.Contains(stdout, "changed") || !strings.Contains(stderr, "baseline: 0 new, 1 known") {
			t.Fatalf("%s exit %d: %s%s", format, code, stdout, stderr)
		}
	}
	base[5] = `printf '%s\n' '{"type":"vault_policy","identity":{"id":"known"}}' '{"type":"vault_policy","identity":{"id":"new"},"display_name":"new-name"}'`
	code, stdout, stderr := runReportTest(t, append(base, "-baseline", baselinePath)...)
	if code != exitOrphans || !strings.Contains(stdout, "new-name") || strings.Contains(stdout, "id=known") || !strings.Contains(stderr, "baseline: 1 new, 1 known") {
		t.Fatalf("new run exit %d: %s%s", code, stdout, stderr)
	}
	base[5] = `printf '%s\n' '{"type":"vault_policy","identity":{"id":"known"}}' '{"error":"permission denied"}'`
	code, _, stderr = runReportTest(t, append(base, "-baseline", baselinePath, "-strict")...)
	if code != exitError || !strings.Contains(stderr, "baseline: 0 new, 1 known") {
		t.Fatalf("strict baseline exit %d: %s", code, stderr)
	}
	code, _, stderr = runReportTest(t, "adopt", "-baseline", baselinePath)
	if code != exitError || !strings.Contains(stderr, "-baseline is not supported with adopt") {
		t.Fatalf("adopt baseline exit %d: %s", code, stderr)
	}
}

func TestBaselinePluginOnlyCoverage(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, "baseline.json")
	old := []model.Resource{
		{Type: "foo_thing", Identity: map[string]any{"id": "A"}, ScopeName: "account x", ProviderSource: "hashicorp/foo", Source: "infra"},
		{Type: "foo_thing", Identity: map[string]any{"id": "B"}, ScopeName: "account x", ProviderSource: "hashicorp/foo", Source: "infra"},
	}
	saveReportTest(t, path, old, report.Summary{})
	for _, strict := range []bool{false, true} {
		args := []string{"-no-config", "-no-query", "-plugin", `printf '%s\n' '{"type":"foo_thing","identity":{"id":"B"},"scope":"account x"}'`, "-baseline", path, "-format", "json"}
		if strict {
			args = append(args, "-strict")
		}
		code, stdout, stderr := runReportTest(t, args...)
		if code != exitOK {
			t.Fatalf("strict %t, exit %d: %s", strict, code, stderr)
		}
		var doc struct {
			Orphans  []model.Resource `json:"orphans"`
			Summary  report.Summary   `json:"summary"`
			Baseline report.Baseline  `json:"baseline"`
		}
		if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
			t.Fatal(err)
		}
		b, s := doc.Baseline, doc.Summary
		if len(doc.Orphans) != 1 || doc.Orphans[0].Baseline != "known" || b.Known != 1 || b.New != 0 || b.Resolved != 0 || b.Unverified != 1 || b.UnverifiedFindings[0].Identity["id"] != "A" || b.UnverifiedFindings[0].Reason != "no listing evidence" {
			t.Fatalf("plugin-only comparison: %s", stdout)
		}
		if len(s.Providers) != 0 || len(s.Coverage) != 1 || s.Coverage[0].Status != report.Complete || s.Coverage[0].Scope != "account x" {
			t.Fatalf("plugin-only coverage: %+v", s)
		}
	}
}

func TestSummaryScanListings(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("TF_PLUGIN_CACHE_DIR", dir)
	bin := filepath.Join(dir, "terraform")
	script := `#!/bin/sh
case "$1" in
  init) exit 0 ;;
  providers) printf '%s\n' '{"provider_schemas":{"hashicorp/foo":{"resource_schemas":{"foo_thing":{}},"data_source_schemas":{"foo_things":{"block":{"attributes":{"result":{"type":["list",["object",{"id":"string"}]]}}}}}}}}' ;;
  apply)
    case "$PWD" in
      */tforphans-alpha-*) scan_id=alpha ;;
      */tforphans-zeta-*) scan_id=zeta ;;
      *) exit 1 ;;
    esac
    printf '{"version":4,"resources":[{"mode":"data","type":"foo_things","name":"all","instances":[{"attributes":{"result":[{"id":"%s"}]}}]}]}\n' "$scan_id" > terraform.tfstate
    ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "zeta"} {
		sources[name] = dsource.Adapter{Name: name, Provider: "hashicorp/foo", Target: "account"}
		t.Cleanup(func() { delete(sources, name) })
	}
	code, stdout, stderr := runReportTest(t, "-no-config", "-no-query", "-terraform", bin, "-scan", "zeta,alpha", "-allow-unpinned", "-format", "json")
	if code != exitOrphans {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var doc struct {
		Orphans []model.Resource `json:"orphans"`
		Summary report.Summary   `json:"summary"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(doc.Summary.Listings, []report.Listing{{Source: "-scan alpha", Provider: "hashicorp/foo", Type: "foo_thing", Scopes: []string{}, Complete: true}, {Source: "-scan zeta", Provider: "hashicorp/foo", Type: "foo_thing", Scopes: []string{}, Complete: true}}) {
		t.Fatalf("scans not recorded in sorted order: %s", stdout)
	}
	if len(doc.Orphans) != 2 {
		t.Fatalf("scan results missing: %s", stdout)
	}
	for _, r := range doc.Orphans {
		if r.Source != "-scan "+r.Identity["id"].(string) {
			t.Fatalf("scan source: %+v", r)
		}
	}
	path := filepath.Join(dir, "report.json")
	if err := os.WriteFile(path, []byte(stdout), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := report.ReadSnapshot(path, 1<<20)
	if err != nil || !reflect.DeepEqual(snapshot.Summary.Listings, doc.Summary.Listings) {
		t.Fatalf("scan snapshot: %+v, %v", snapshot, err)
	}
}

func TestUnqueriedRootCompleteness(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	var err error
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TF_DATA_DIR", ".terraform")
	good := filepath.Join(dir, "good")
	for _, name := range []string{"good", "schema-failure", "state-failure"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(dir, "terraform")
	script := `#!/bin/sh
case "$1" in
  version) printf '%s\n' '{"terraform_version":"1.14.0"}' ;;
  providers)
    case "$PWD" in
      */schema-failure) echo 'Error: Required plugins are not installed' >&2; exit 1 ;;
    esac
    printf '%s\n' '{"provider_schemas":{"registry.terraform.io/hashicorp/foo":{"list_resource_schemas":{"foo_thing":{}}}}}'
    ;;
  state)
    case "$PWD" in
      */schema-failure|*/state-failure) echo 'Error: Backend initialization required' >&2; exit 1 ;;
    esac
    printf '%s\n' '{"version":4,"resources":[]}'
    ;;
  query)
    printf '%s\n' '{"list_start":{"address":"list.foo_thing.tfo_foo_foo_thing","resource_type":"foo_thing"}}' '{"list_complete":{"address":"list.foo_thing.tfo_foo_foo_thing","resource_type":"foo_thing","total":0}}'
    ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	baselinePath := filepath.Join(dir, "baseline.json")
	for _, c := range []struct {
		name, failedRoot string
		noQuery          bool
	}{
		{name: "all queries ran"},
		{name: "schema failure", failedRoot: "schema-failure"},
		{name: "state failure before query", failedRoot: "state-failure"},
		{name: "no query state failure", failedRoot: "state-failure", noQuery: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := []string{"-no-config", "-terraform", bin, "-current-workspace", "-format", "json", good}
			if c.failedRoot != "" {
				args = append(args, filepath.Join(dir, c.failedRoot))
			}
			if c.noQuery {
				args = append(args, "-no-query", "-plugin", "true")
			}
			old := []model.Resource{{Type: "foo_thing", Identity: map[string]any{"id": "gone"}, ProviderSource: "hashicorp/foo", Source: good}}
			if c.failedRoot != "" {
				old = append(old, model.Resource{Type: "foo_thing", Identity: map[string]any{"id": "unchecked"}, ProviderSource: "hashicorp/foo", Source: filepath.Join(dir, c.failedRoot)})
			}
			saveReportTest(t, baselinePath, old, report.Summary{})
			args = append(args, "-baseline", baselinePath)
			code, stdout, stderr := runReportTest(t, args...)
			if code != exitOK {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			var doc struct {
				Summary  report.Summary  `json:"summary"`
				Baseline report.Baseline `json:"baseline"`
			}
			if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
				t.Fatal(err)
			}
			s := doc.Summary
			b := doc.Baseline
			if !c.noQuery {
				wantUnverified := len(old) - 1
				if b.Resolved != 1 || b.Unverified != wantUnverified || wantUnverified > 0 && b.UnverifiedFindings[0].Reason != "not listed by the same source" {
					t.Fatalf("same source evidence: %s", stdout)
				}
				// Saved evidence must make the same decision in diff.
				currentPath := filepath.Join(t.TempDir(), "current.json")
				if err := os.WriteFile(currentPath, []byte(stdout), 0o600); err != nil {
					t.Fatal(err)
				}
				code, diffJSON, stderr := runReportTest(t, "diff", "-format", "json", baselinePath, currentPath)
				var d report.Diff
				if err := json.Unmarshal([]byte(diffJSON), &d); err != nil {
					t.Fatal(err)
				}
				if code != exitOK || len(d.Resolved) != 1 || len(d.Unverified) != wantUnverified {
					t.Fatalf("saved same source evidence: %s%s", diffJSON, stderr)
				}
			}
			statesComplete := c.failedRoot == ""
			discoveryComplete := statesComplete || c.noQuery
			if s.StatesComplete != statesComplete || s.DiscoveryComplete != discoveryComplete || s.Complete != statesComplete || (len(s.UnreadStates) == 0) != statesComplete || len(s.UnreadStates) > 1 {
				t.Fatalf("split completeness: %s", stdout)
			}
			if !c.noQuery && (len(s.Providers) != 1 || s.Providers[0].QueriedTypes != 1 || !reflect.DeepEqual(s.Listings, []report.Listing{{Source: good, Provider: "hashicorp/foo", Type: "foo_thing", Scopes: []string{}, Complete: true}}) || len(s.Coverage) != 1 || s.Coverage[0].Status != report.Complete) {
				t.Fatalf("successful root did not list the type: %s", stdout)
			}
		})
	}
}

func TestBaselineVerboseUnverified(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	var rs []model.Resource
	for i := range 10 {
		rs = append(rs, model.Resource{Type: "thing", Identity: map[string]any{"id": fmt.Sprintf("%02d", i)}})
	}
	path := filepath.Join(dir, "baseline.json")
	saveReportTest(t, path, rs, report.Summary{})
	for _, verbose := range []bool{false, true} {
		args := []string{"-no-config", "-no-query", "-plugin", "true", "-baseline", path}
		if verbose {
			args = append(args, "-v")
		}
		code, _, stderr := runReportTest(t, args...)
		if code != exitOK || !strings.Contains(stderr, "10 not verified") || strings.Contains(stderr, "and 2 more") != verbose || strings.Contains(stderr, "id=00: no listing evidence") != verbose || strings.Contains(stderr, "id=09") {
			t.Fatalf("verbose %t, exit %d: %s", verbose, code, stderr)
		}
	}
}

func TestSplitCompleteness(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	root := filepath.Join(dir, "infra")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		args  []string
		state bool
	}{
		{"read states", nil, true},
		{"unread state", []string{"-terraform", "/nonexistent/terraform", root}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := append([]string{"-no-config", "-no-query", "-plugin", "true", "-format", "json"}, c.args...)
			code, stdout, stderr := runReportTest(t, args...)
			if code != exitOK {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			var doc struct {
				Summary report.Summary `json:"summary"`
			}
			if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
				t.Fatal(err)
			}
			s := doc.Summary
			if s.StatesComplete != c.state || s.Complete != c.state || !s.DiscoveryComplete {
				t.Fatalf("split completeness: %+v", s)
			}
		})
	}
}

func TestBaselineConfig(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("baseline.json", []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(defaultConfigFile, []byte("baseline: baseline.json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o, err := parseFlags(nil, io.Discard)
	if err != nil || o.baselinePath != "baseline.json" {
		t.Fatalf("automatic config: %+v, %v", o, err)
	}
	outside := filepath.Join(t.TempDir(), "baseline.json")
	if err := os.WriteFile(outside, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(defaultConfigFile, []byte("baseline: "+outside+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := parseFlags(nil, io.Discard); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("automatic config escaped directory: %v", err)
	}
	o, err = parseFlags([]string{"-config", defaultConfigFile}, io.Discard)
	if err != nil || o.baselinePath != outside {
		t.Fatalf("explicit config: %+v, %v", o, err)
	}
}
