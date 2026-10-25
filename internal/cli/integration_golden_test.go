//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrationGoldenIncompleteCoverage pins the coverage of a run with
// gaps end to end: a failed listing and a type that needs an argument,
// each with its reason codes (TFORPHANS_UPDATE_GOLDEN=1 rewrites the file).
// Only summary.coverage is compared: the rest names temporary paths and
// versions.
func TestIntegrationGoldenIncompleteCoverage(t *testing.T) {
	golden, err := filepath.Abs(filepath.Join("testdata", "golden", "coverage-incomplete.json"))
	if err != nil {
		t.Fatal(err)
	}
	docs, err := os.ReadFile(filepath.Join("docs", "json.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, root := setup(t)
	write(t, filepath.Join(root, "main.tf"), "terraform {\n  required_providers {\n    fake = { source = \"example.com/fake/fake\" }\n  }\n}\n")
	t.Chdir(root)
	t.Setenv("FAKE_FAIL", "thing:Error 403: permission denied on resource")
	code, stdout, stderr := runTool("-types", "fake_thing,fake_zoned", "-format", "json")
	if code != exitOK || !strings.Contains(stderr, "coverage: INCOMPLETE") {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	var out struct {
		Summary struct {
			Complete bool            `json:"complete"`
			Coverage json.RawMessage `json:"coverage"`
		} `json:"summary"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatal(err)
	}
	if out.Summary.Complete {
		t.Fatal("summary.complete with gaps")
	}
	var coverage any
	if err := json.Unmarshal(out.Summary.Coverage, &coverage); err != nil {
		t.Fatal(err)
	}
	// Indented as written: the order of keys is part of the output.
	var buf bytes.Buffer
	if err := json.Indent(&buf, out.Summary.Coverage, "", "  "); err != nil {
		t.Fatal(err)
	}
	got := append(buf.Bytes(), '\n')
	if strings.Contains(string(got), root) {
		t.Fatalf("coverage names the temporary root:\n%s", got)
	}
	if os.Getenv("TFORPHANS_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("coverage differs from %s (TFORPHANS_UPDATE_GOLDEN=1 rewrites it):\n%s", golden, got)
	}
	for _, k := range jsonKeys(coverage, nil) {
		if !strings.Contains(string(docs), "`"+k+"`") {
			t.Errorf("key %q is not described in docs/json.md", k)
		}
	}
}
