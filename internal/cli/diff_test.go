package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/report"
)

func TestDiffRun(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	old, current := filepath.Join(dir, "old.json"), filepath.Join(dir, "new.json")
	r := func(id, scope string) model.Resource {
		return model.Resource{Type: "thing", Identity: map[string]any{"id": id}, ScopeName: scope, Source: "infra", ListAddress: "list.thing." + scope}
	}
	saveReportTest(t, old, []model.Resource{r("resolved", "a"), r("missing", "b")}, report.Summary{Live: 2, Orphans: 2, Complete: true, Listings: []report.Listing{{Source: "infra", Address: "list.thing.a", Type: "thing", Complete: true, Fingerprint: "same", Scope: map[string]string{"account": "a"}}, {Source: "infra", Address: "list.thing.b", Type: "thing", Complete: true, Fingerprint: "same", Scope: map[string]string{"account": "a"}}}, Coverage: []report.Coverage{{Type: "thing", Scope: "a", Status: report.Complete}, {Type: "thing", Scope: "b", Status: report.Complete}}})
	saveReportTest(t, current, []model.Resource{r("added", "a")}, report.Summary{Live: 1, Orphans: 1, Listings: []report.Listing{{Source: "infra", Address: "list.thing.a", Type: "thing", Complete: true, Fingerprint: "same", Scope: map[string]string{"account": "a"}}}, Coverage: []report.Coverage{{Type: "thing", Scope: "a", Status: report.Complete}}})
	// A config that could run a command must not be read by diff.
	if err := os.WriteFile(defaultConfigFile, []byte("plugin: exit 99\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")
	code, stdout, stderr := runReportTest(t, "diff", old, current, "-format", "json")
	if code != exitOrphans || stderr != "" {
		t.Fatalf("exit %d: %s%s", code, stdout, stderr)
	}
	var d report.Diff
	if err := json.Unmarshal([]byte(stdout), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Added) != 1 || len(d.Resolved) != 1 || len(d.Unverified) != 1 || d.Unverified[0].Reason != "not listed this run" || len(d.Counters) != 7 || len(d.CoverageChanges) != 1 || !d.Complete.Old || d.Complete.New {
		t.Fatalf("diff JSON: %s", stdout)
	}
	code, stdout, stderr = runReportTest(t, "diff", old, current)
	if code != exitOrphans || !strings.Contains(stdout, "thing id=resolved (a)") || !strings.Contains(stdout, "thing id=missing (b): not listed this run") || !strings.Contains(stdout, "orphans: 2 to 1") {
		t.Fatalf("text exit %d: %s%s", code, stdout, stderr)
	}
	code, stdout, stderr = runReportTest(t, "diff", current, current)
	if code != exitOK || !strings.Contains(stdout, "added (0)") {
		t.Fatalf("unchanged exit %d: %s%s", code, stdout, stderr)
	}
	code, stdout, stderr = runReportTest(t, "diff", "-h")
	if code != exitOK || stdout != "" || !strings.Contains(stderr, "Usage: tforphans diff") {
		t.Fatalf("help exit %d: %s%s", code, stdout, stderr)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"schema_version":2,"orphans":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"diff"}, {"diff", old}, {"diff", old, current, old},
		{"diff", old, "missing.json"}, {"diff", "missing.json", current},
		{"diff", bad, current}, {"diff", old, bad}, {"diff", "-", current},
		{"diff", "-format", "summary", old, current}, {"diff", "-unknown", old, current},
	} {
		code, stdout, stderr = runReportTest(t, args...)
		if code != exitError || stdout != "" || stderr == "" {
			t.Errorf("%v exit %d: %s%s", args, code, stdout, stderr)
		}
	}
}
