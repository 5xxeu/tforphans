//go:build integration

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/dsource"
)

// fakeAdapter scans the fake provider's data sources the way Cloudflare is
// scanned: fake_zones lists zones, fake_records iterates over them.
var fakeAdapter = dsource.Adapter{
	Name:     "fake",
	Provider: "example.com/fake/fake",
	Target:   "account",
	Scopes:   []dsource.Scope{{Arg: "zone_id", Parent: "fake_zones", Prefix: "zones"}},
}

// scanSetup creates zones and records and registers the fake adapter.
func scanSetup(t *testing.T) (root string) {
	t.Helper()
	data, root := setup(t)
	if err := os.MkdirAll(filepath.Join(data, "zone"), 0o755); err != nil {
		t.Fatal(err)
	}
	for zone, records := range map[string][]string{"z1": {"r1", "r2"}, "z2": {"r3"}, "z3": {"r4"}, "z4": {"r5"}} {
		write(t, filepath.Join(data, "zone", zone), "")
		dir := filepath.Join(data, "record", zone)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, r := range records {
			write(t, filepath.Join(dir, r), "")
		}
	}
	sources["fake"] = fakeAdapter
	t.Cleanup(func() { delete(sources, "fake") })
	return root
}

type scanOutput struct {
	Orphans []struct {
		Type     string         `json:"type"`
		ImportID string         `json:"import_id"`
		Identity map[string]any `json:"identity"`
	} `json:"orphans"`
	Summary struct {
		Live     int  `json:"live"`
		Complete bool `json:"complete"`
		Coverage []struct {
			Type    string   `json:"type"`
			Scope   string   `json:"scope"`
			Status  string   `json:"status"`
			Reasons []string `json:"reasons"`
			Live    int      `json:"live"`
		} `json:"coverage"`
		Failed []struct {
			Address  string `json:"address"`
			Type     string `json:"type"`
			Category string `json:"category"`
			Message  string `json:"message"`
		} `json:"failed_queries"`
	} `json:"summary"`
}

func scanJSON(t *testing.T, args ...string) (int, scanOutput, string) {
	t.Helper()
	code, stdout, stderr := runTool(append([]string{"-no-query", "-scan", "fake", "-allow-unpinned", "-format", "json"}, args...)...)
	var out scanOutput
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("exit %d, %v\n%s\n%s", code, err, stdout, stderr)
	}
	return code, out, stderr
}

func importIDs(out scanOutput) []string {
	var ids []string
	for _, o := range out.Orphans {
		ids = append(ids, o.ImportID)
	}
	slices.Sort(ids)
	return ids
}

// TestIntegrationScanCrash crashes the provider on one zone's records: that
// zone is reported as failed, every other zone and type is still listed.
func TestIntegrationScanCrash(t *testing.T) {
	root := scanSetup(t)
	t.Chdir(root)
	t.Setenv("FAKE_PANIC", "record:z3")
	code, out, stderr := scanJSON(t)
	if code != exitOrphans {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	want := []string{"z1", "z1/r1", "z1/r2", "z2", "z2/r3", "z3", "z4", "z4/r5"}
	if got := importIDs(out); !slices.Equal(got, want) {
		t.Errorf("orphans %v, want %v\n%s", got, want, stderr)
	}
	if len(out.Summary.Failed) != 1 || out.Summary.Failed[0].Address != `data.fake_records.by_zone_id["z3"]` || out.Summary.Failed[0].Category != "crashed" {
		t.Errorf("failed %+v", out.Summary.Failed)
	}
	if !strings.Contains(stderr, "crashed") {
		t.Errorf("stderr:\n%s", stderr)
	}
	// Only the crashed zone is incomplete.
	var cov []string
	for _, c := range out.Summary.Coverage {
		if c.Type == "fake_record" {
			cov = append(cov, fmt.Sprintf("%s %s %v %d", c.Scope, c.Status, c.Reasons, c.Live))
		}
	}
	if want := []string{"zone z1.name complete [] 2", "zone z2.name complete [] 1", "zone z3.name incomplete [crashed] 0", "zone z4.name complete [] 1"}; out.Summary.Complete || !slices.Equal(cov, want) {
		t.Errorf("coverage %q, want %q", cov, want)
	}
	if code, _, _ := runTool("-no-query", "-scan", "fake", "-allow-unpinned", "-strict"); code != exitError {
		t.Errorf("-strict with a crashed zone: exit %d", code)
	}
}

// TestIntegrationScanScope limits the scan to some zones, by ID or name,
// or to the zones of the states read.
func TestIntegrationScanScope(t *testing.T) {
	root := scanSetup(t)
	t.Chdir(root)
	code, out, stderr := scanJSON(t, "-scan-scope", "zone_id=z1.name,z2")
	want := []string{"z1", "z1/r1", "z1/r2", "z2", "z2/r3"}
	if got := importIDs(out); code != exitOrphans || !slices.Equal(got, want) {
		t.Errorf("exit %d, orphans %v, want %v\n%s", code, got, want, stderr)
	}

	st := `{"version": 4, "resources": [{"mode": "managed", "type": "fake_record", "name": "r", "provider": "provider[\"example.com/fake/fake\"]",
	  "instances": [{"attributes": {"id": "r5", "zone_id": "z4"}}]}]}`
	statePath := filepath.Join(t.TempDir(), "zone.tfstate")
	write(t, statePath, st)
	code, out, stderr = scanJSON(t, "-scan-scope", "state", "-state", statePath)
	if got := importIDs(out); code != exitOrphans || !slices.Equal(got, []string{"z4"}) {
		t.Errorf("-scan-scope state: exit %d, orphans %v\n%s", code, got, stderr)
	}
	var raw struct {
		Orphans []struct {
			Scope string `json:"scope"`
		} `json:"orphans"`
		Summary struct {
			ByScope map[string]struct{ Live, Managed int } `json:"by_scope"`
		} `json:"summary"`
	}
	_, stdout, _ := runTool("-no-query", "-scan", "fake", "-allow-unpinned", "-format", "json", "-scan-scope", "state", "-state", statePath)
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil || len(raw.Orphans) != 1 || raw.Orphans[0].Scope != "zone z4.name" || raw.Summary.ByScope["zone z4.name"].Managed != 1 {
		t.Errorf("scope names: %v\n%s", err, stdout)
	}
	// Without a limit and without -state-roots the scan says it covers the
	// whole account.
	if _, _, stderr := runTool("-no-query", "-scan", "fake", "-allow-unpinned"); !strings.Contains(stderr, "-scan lists whole accounts") {
		t.Errorf("no hint:\n%s", stderr)
	}
	// A zone that does not exist is a gap, not an empty zone.
	code, out, stderr = scanJSON(t, "-scan-scope", "zone_id=typo.example")
	if code != exitOK || len(out.Summary.Failed) != 1 || out.Summary.Failed[0].Category != "not found" || !strings.Contains(out.Summary.Failed[0].Address, "typo.example") {
		t.Errorf("unknown zone: exit %d %+v\n%s", code, out.Summary.Failed, stderr)
	}
	if code, _, _ := runTool("-no-query", "-scan", "fake", "-allow-unpinned", "-scan-scope", "zone_id=typo.example", "-strict"); code != exitError {
		t.Errorf("unknown zone with -strict: exit %d", code)
	}
	// No value: that scope is not read (its types are skipped), and no
	// zone is listed either.
	code, out, stderr = scanJSON(t, "-scan-scope", "zone_id=", "-v")
	if got := importIDs(out); code != exitOK || len(got) != 0 || !strings.Contains(stderr, "skipped fake_record: -scan-scope names no zone_id") {
		t.Errorf("zone_id=: exit %d, orphans %v\n%s", code, got, stderr)
	}
	if code, _, _ := runTool("-no-query", "-scan", "fake", "-allow-unpinned", "-scan-scope", "region=x"); code != exitError {
		t.Errorf("unknown scope argument: exit %d", code)
	}
}

// TestIntegrationRefresh reads identities that the stored state lacks
// through a refresh-only plan, without changing the stored state.
func TestIntegrationRefresh(t *testing.T) {
	data, root := setup(t)
	write(t, filepath.Join(root, "main.tf"), `
terraform {
  required_providers {
    fake = { source = "example.com/fake/fake" }
  }
}
resource "fake_thing" "a" { name = "a" }
`)
	terraform(t, root, "apply", "-auto-approve", "-no-color")
	write(t, filepath.Join(data, "thing", "stray"), "")
	// State as written before Terraform 1.12: no identities.
	statePath := filepath.Join(root, "terraform.tfstate")
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	for _, r := range st["resources"].([]any) {
		for _, in := range r.(map[string]any)["instances"].([]any) {
			delete(in.(map[string]any), "identity")
			delete(in.(map[string]any), "identity_schema_version")
		}
	}
	raw, _ = json.Marshal(st)
	write(t, statePath, string(raw))

	t.Chdir(root)
	code, _, stderr := runTool("-types", "fake_thing")
	if code != exitOrphans || !strings.Contains(stderr, "by identity: 0, by attributes: 1") || !strings.Contains(stderr, "-refresh reads identities") {
		t.Errorf("stored state: exit %d\n%s", code, stderr)
	}
	code, _, stderr = runTool("-types", "fake_thing", "-refresh")
	if code != exitOrphans || !strings.Contains(stderr, "by identity: 1, by attributes: 0") {
		t.Errorf("-refresh: exit %d\n%s", code, stderr)
	}
	if after, _ := os.ReadFile(statePath); string(after) != string(raw) {
		t.Error("-refresh changed the stored state")
	}
	// A plan that cannot run falls back to the stored state.
	wrapper := filepath.Join(t.TempDir(), "tf.sh")
	write(t, wrapper, "#!/bin/sh\n[ \"$1\" = plan ] && { echo 'Error: No value for required variable' >&2; exit 1; }\nexec terraform \"$@\"\n")
	if err := os.Chmod(wrapper, 0o755); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = runTool("-types", "fake_thing", "-refresh", "-terraform", wrapper)
	if code != exitOrphans || !strings.Contains(stderr, "-refresh failed, the stored state was read instead") {
		t.Errorf("-refresh fallback: exit %d\n%s", code, stderr)
	}
}

// TestIntegrationScanNoZones: with no zone listed, records were not read in
// any: coverage is incomplete and -strict fails, not a clean audit.
func TestIntegrationScanNoZones(t *testing.T) {
	root := scanSetup(t)
	t.Chdir(root)
	data := os.Getenv("FAKE_DIR")
	if err := os.RemoveAll(filepath.Join(data, "zone")); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(data, "zone"), 0o755)
	code, out, stderr := scanJSON(t, "-types", "fake_record", "-strict", "-v")
	if code != exitError || out.Summary.Complete || !strings.Contains(stderr, "no zone was listed to read it in") {
		t.Errorf("exit %d, complete %v\n%s", code, out.Summary.Complete, stderr)
	}
}
