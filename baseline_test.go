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
	if strings.Contains(plain, `"baseline"`) || strings.Contains(plain, `"providers"`) {
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
		if code != exitOK || !strings.Contains(stderr, "10 not verified") || strings.Contains(stderr, "and 2 more") != verbose || strings.Contains(stderr, "id=00: type not listed") != verbose || strings.Contains(stderr, "id=09") {
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
