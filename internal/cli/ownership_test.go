package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/state"
)

func TestStrictOwnership(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell plugin")
	}
	dir := t.TempDir()
	write := func(name, lineage, identity, resources string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		data := fmt.Sprintf(`{"version":4,"lineage":%q,"serial":1,"resources":[
		{"mode":"managed","type":"aws_instance","name":"a","provider":"provider[\"registry.terraform.io/hashicorp/aws\"]","instances":[{"identity":%s,"attributes":{"id":"i-1"}}]}%s]}`, lineage, identity, resources)
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	a := write("a.tfstate", "a", `{"id":"i-1","account_id":"111"}`, "")
	b := write("b.tfstate", "b", `{"id":"i-1","account_id":"111"}`, "")
	partial := write("partial.tfstate", "partial", `{"id":"i-1"}`, "")
	unknown := write("unknown.tfstate", "", `{"id":"i-1","account_id":"111"}`, "")
	legacy := write("legacy.tfstate", "legacy", `null`, ","+`{"mode":"managed","type":"aws_instance","name":"b","provider":"provider[\"registry.terraform.io/hashicorp/aws\"]","instances":[{"attributes":{"id":"i-1"}}]}`)
	within := write("within.tfstate", "within", `{"id":"i-1"}`, ","+`{"mode":"managed","type":"aws_instance","name":"b","provider":"provider[\"registry.terraform.io/hashicorp/aws\"]","instances":[{"identity":{"id":"i-1"}}]}`)
	for _, c := range []struct {
		name                        string
		paths                       []string
		flags                       []string
		want                        int
		confirmed, possible, within int
	}{
		{"warning", []string{a, b}, nil, exitOK, 1, 0, 0},
		{"strict coverage", []string{a, b}, []string{"-strict"}, exitOK, 1, 0, 0},
		{"strict ownership", []string{a, b}, []string{"-strict-ownership"}, exitError, 1, 0, 0},
		{"possible", []string{a, partial}, []string{"-strict-ownership"}, exitOK, 0, 1, 0},
		{"unknown", []string{a, unknown}, []string{"-strict-ownership"}, exitOK, 0, 1, 0},
		{"same read twice", []string{a, a}, []string{"-strict-ownership"}, exitOK, 0, 0, 0},
		{"within", []string{within}, []string{"-strict-ownership"}, exitError, 0, 0, 1},
		{"within legacy", []string{legacy}, []string{"-strict-ownership"}, exitError, 0, 0, 1},
		{"quiet warnings", []string{a, b}, []string{"-quiet"}, exitOK, 1, 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := []string{"-no-config", "-no-query", "-plugin", `printf '%s\n' '{"done":true}'`, "-format", "json"}
			for _, path := range c.paths {
				args = append(args, "-state", path)
			}
			args = append(args, c.flags...)
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), args, &stdout, &stderr); code != c.want {
				t.Fatalf("exit %d: %s", code, &stderr)
			}
			var result struct {
				SchemaVersion int               `json:"schema_version"`
				Ownership     []state.Duplicate `json:"duplicate_ownership"`
				InState       []state.Duplicate `json:"duplicate_in_state"`
				Summary       struct {
					Confirmed int  `json:"duplicate_ownership"`
					Possible  int  `json:"possible_duplicate_ownership"`
					Within    int  `json:"duplicate_in_state"`
					Complete  bool `json:"complete"`
				} `json:"summary"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.SchemaVersion != 1 || result.Summary.Confirmed != c.confirmed || result.Summary.Possible != c.possible || result.Summary.Within != c.within || !result.Summary.Complete || len(result.Ownership) != c.confirmed+c.possible || len(result.InState) != c.within {
				t.Fatalf("JSON: %s", &stdout)
			}
			if c.confirmed+c.possible+c.within > 0 && !strings.Contains(stderr.String(), "warning: duplicate state ownership:") {
				t.Fatalf("missing warning: %s", &stderr)
			}
		})
	}
}

func TestOwnershipConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(defaultConfigFile, []byte("strict-ownership: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o, err := parseFlags(nil, io.Discard)
	if err != nil || !o.audit.strictOwnership {
		t.Fatalf("automatic config: %+v, %v", o, err)
	}
	o, err = parseFlags([]string{"-strict-ownership=false"}, io.Discard)
	if err != nil || o.audit.strictOwnership {
		t.Fatalf("override: %+v, %v", o, err)
	}
}

func TestAdoptStrictOwnershipDiagnostics(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell plugin")
	}
	dir := t.TempDir()
	write := func(name, lineage, identity string, addresses ...string) string {
		t.Helper()
		var resources []string
		for _, address := range addresses {
			resources = append(resources, fmt.Sprintf(`{"mode":"managed","type":"aws_instance","name":%q,"provider":"provider[\"registry.terraform.io/hashicorp/aws\"]","instances":[{"identity":%s,"attributes":{"id":"i-1"}}]}`, address, identity))
		}
		path := filepath.Join(dir, name+".tfstate")
		data := fmt.Sprintf(`{"version":4,"lineage":%q,"serial":1,"resources":[%s]}`, lineage, strings.Join(resources, ","))
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	a := write("a", "a", `{"id":"i-1","account_id":"111"}`, "a")
	b := write("b", "b", `{"id":"i-1","account_id":"111"}`, "b")
	partial := write("partial", "partial", `{"id":"i-1"}`, "a")
	within := write("within", "within", `{"id":"i-1"}`, "a", "b")
	legacy := write("legacy", "legacy", `null`, "a", "b")
	for _, c := range []struct {
		name    string
		paths   []string
		strict  bool
		want    int
		warning string
	}{
		{"confirmed", []string{a, b}, true, exitError, "confirmed duplicate ownership"},
		{"within", []string{within}, true, exitError, "confirmed duplicate in state"},
		{"within legacy", []string{legacy}, true, exitError, "possible duplicate in state (no identity)"},
		{"possible", []string{a, partial}, true, exitOK, "possible duplicate ownership (partial identity)"},
		{"without strict", []string{a, b}, false, exitOK, "confirmed duplicate ownership"},
		{"none", []string{a}, true, exitOK, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := []string{"adopt", "-no-config", "-no-query", "-plugin", `printf '%s\n' '{"done":true}'`}
			for _, path := range c.paths {
				args = append(args, "-state", path)
			}
			if c.strict {
				args = append(args, "-strict-ownership")
			}
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), args, &stdout, &stderr); code != c.want {
				t.Fatalf("exit %d, want %d: %s", code, c.want, &stderr)
			}
			if !strings.Contains(stderr.String(), "Nothing to adopt.") {
				t.Fatalf("expected no orphans: %s", &stderr)
			}
			if c.warning != "" {
				if !strings.Contains(stderr.String(), "warning: duplicate state ownership:") || !strings.Contains(stderr.String(), c.warning) {
					t.Fatalf("missing ownership warning: %s", &stderr)
				}
			} else if strings.Contains(stderr.String(), "duplicate state ownership") {
				t.Fatalf("unexpected ownership warning: %s", &stderr)
			}
		})
	}
}

func TestPrintOwnershipTruncation(t *testing.T) {
	var entries []state.Duplicate
	serial := int64(7)
	for i := 0; i < 10; i++ {
		entries = append(entries, state.Duplicate{Type: "aws_instance", Identity: map[string]any{"id": fmt.Sprintf("i-%02d", i)}, Confidence: "confirmed", Owners: []state.Owner{{Address: "aws_instance.a", Lineage: "a", Serial: &serial, Sources: []string{"root (workspace prod)"}}}})
	}
	entries[1].Confidence, entries[1].Reason = "possible", "same_lineage_diverged"
	var b bytes.Buffer
	printOwnership(&b, entries, nil)
	for _, want := range []string{"confirmed duplicate ownership", "possible duplicate ownership (same lineage diverged)", "serial 7", "root (workspace prod)", "i-07", "and 2 more", "-format json"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q: %s", want, &b)
		}
	}
	if strings.Contains(b.String(), "i-08") {
		t.Errorf("not truncated: %s", &b)
	}
	b.Reset()
	printOwnership(&b, nil, nil)
	if b.Len() != 0 {
		t.Fatal("empty warning")
	}
}

func TestPullOwnershipSnapshots(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell wrapper")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "terraform")
	script := `#!/bin/sh
case "$1" in
workspace) printf '* default\n  prod\n';;
state) printf '%s' '{"version":4,"lineage":"remote","serial":5}';;
plan) exit 0;;
show) printf '%s' '{"prior_state":{"values":{"root_module":{"resources":[]}}}}';;
*) exit 1;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, refresh := range []bool{false, true} {
		o := &options{src: sourceOptions{terraformBin: bin, refresh: refresh, maxStateSize: state.DefaultMaxSize}}
		snapshots, n, err := pullRoot(context.Background(), o, dir)
		if err != nil || n != 2 || len(snapshots) != 2 {
			t.Fatalf("pull: %+v, %d, %v", snapshots, n, err)
		}
		for i, ws := range []string{"default", "prod"} {
			if snapshots[i].Sources[0] != inWorkspaceName(dir, ws) {
				t.Fatalf("source: %+v", snapshots[i])
			}
			if refresh {
				if snapshots[i].Lineage != "" || snapshots[i].Serial != nil {
					t.Fatalf("refresh provenance: %+v", snapshots[i])
				}
			} else if snapshots[i].Lineage != "remote" || snapshots[i].Serial == nil || *snapshots[i].Serial != 5 {
				t.Fatalf("pull provenance: %+v", snapshots[i])
			}
		}
	}
}

func TestOwnershipPreservesFindingsAndAdopt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell plugin")
	}
	dir := t.TempDir()
	var paths []string
	for _, lineage := range []string{"a", "b"} {
		path := filepath.Join(dir, lineage+".tfstate")
		data := fmt.Sprintf(`{"version":4,"lineage":%q,"serial":1,"resources":[{"mode":"managed","type":"aws_instance","name":"managed","provider":"provider[\"registry.terraform.io/hashicorp/aws\"]","instances":[{"identity":{"id":"managed"}}]}]}`, lineage)
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	plugin := `printf '%s\n' '{"type":"aws_instance","identity":{"id":"new"}}'`
	for _, adopt := range []bool{false, true} {
		var baseline string
		for i := 1; i <= 2; i++ {
			args := []string{"-no-config", "-no-query", "-plugin", plugin}
			want := exitOrphans
			if adopt {
				args = append([]string{"adopt"}, args...)
				want = exitOK
			}
			for _, path := range paths[:i] {
				args = append(args, "-state", path)
			}
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), args, &stdout, &stderr); code != want {
				t.Fatalf("exit %d: %s", code, &stderr)
			}
			findings := stdout.String()
			if adopt {
				header := fmt.Sprintf("# Objects that none of the %d states read manages.\n\n", i)
				if !strings.HasPrefix(findings, header) {
					t.Fatalf("adopt header: %s", &stdout)
				}
				findings = strings.TrimPrefix(findings, header)
			}
			if i == 1 {
				baseline = findings
			} else if findings != baseline {
				t.Fatalf("findings changed: %s versus %s", baseline, findings)
			}
			if warned := strings.Contains(stderr.String(), "duplicate state ownership"); warned != (i > 1) {
				t.Fatalf("ownership warning with %d states: %s", i, &stderr)
			}
		}
	}
}
