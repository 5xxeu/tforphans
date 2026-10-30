package diffcmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/report"
)

func runDiff(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func saveReport(t *testing.T, path string, rs []model.Resource, s report.Summary) {
	t.Helper()
	var buf bytes.Buffer
	if err := report.JSON(&buf, rs, nil, s); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRun(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	old, current := filepath.Join(dir, "old.json"), filepath.Join(dir, "new.json")
	r := func(id, scope string) model.Resource {
		return model.Resource{Type: "thing", Identity: map[string]any{"id": id}, ScopeName: scope, Source: "infra", ListAddress: "list.thing." + scope}
	}
	saveReport(t, old, []model.Resource{r("resolved", "a"), r("missing", "b")}, report.Summary{Live: 2, Orphans: 2, Complete: true, Listings: []report.Listing{{Source: "infra", Address: "list.thing.a", Type: "thing", Complete: true, Fingerprint: "same", Scope: map[string]string{"account": "a"}}, {Source: "infra", Address: "list.thing.b", Type: "thing", Complete: true, Fingerprint: "same", Scope: map[string]string{"account": "a"}}}, Coverage: []report.Coverage{{Type: "thing", Scope: "a", Status: report.Complete}, {Type: "thing", Scope: "b", Status: report.Complete}}})
	saveReport(t, current, []model.Resource{r("added", "a")}, report.Summary{Live: 1, Orphans: 1, Listings: []report.Listing{{Source: "infra", Address: "list.thing.a", Type: "thing", Complete: true, Fingerprint: "same", Scope: map[string]string{"account": "a"}}}, Coverage: []report.Coverage{{Type: "thing", Scope: "a", Status: report.Complete}}})
	code, stdout, stderr := runDiff(t, old, current, "-format", "json")
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
	code, stdout, stderr = runDiff(t, old, current)
	if code != exitOrphans || !strings.Contains(stdout, "thing id=resolved (a)") || !strings.Contains(stdout, "thing id=missing (b): not listed this run") || !strings.Contains(stdout, "orphans: 2 to 1") {
		t.Fatalf("text exit %d: %s%s", code, stdout, stderr)
	}
	code, stdout, stderr = runDiff(t, current, current)
	if code != exitOK || !strings.Contains(stdout, "added (0)") {
		t.Fatalf("unchanged exit %d: %s%s", code, stdout, stderr)
	}
	code, stdout, stderr = runDiff(t, "-h")
	if code != exitOK || stderr != "" || !strings.Contains(stdout, "Usage: tforphans diff") {
		t.Fatalf("help exit %d: %s%s", code, stdout, stderr)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"schema_version":2,"orphans":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{}, {old}, {old, current, old},
		{old, "missing.json"}, {"missing.json", current},
		{bad, current}, {old, bad}, {"-", current},
		{"-format", "summary", old, current}, {"-unknown", old, current},
	} {
		code, stdout, stderr = runDiff(t, args...)
		if code != exitError || stdout != "" || stderr == "" {
			t.Errorf("%v exit %d: %s%s", args, code, stdout, stderr)
		}
	}
}

// TestRunEndOfFlags: after -- both paths stay paths, even with a dash.
func TestRunEndOfFlags(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	for _, p := range []string{"old.json", "-new.json"} {
		if err := os.WriteFile(p, []byte(`{"schema_version":1,"orphans":[],"summary":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	code, stdout, stderr := runDiff(t, "--", "old.json", "-new.json")
	if code != exitOK || !strings.Contains(stdout, "added (0)") {
		t.Fatalf("exit %d: %s%s", code, stdout, stderr)
	}
}
