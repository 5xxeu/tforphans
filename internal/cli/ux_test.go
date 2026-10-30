package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/report"
)

// TestHelpGoesToStdout: -h and -version can be paged and captured; a flag
// error is one line and a pointer to -h, not the whole usage.
func TestHelpGoesToStdout(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, args := range [][]string{{"-h"}, {"adopt", "-h"}, {"doctor", "-h"}, {"init", "-h"}, {"diff", "-h"}, {"-version"}} {
		code, stdout, stderr := runReportTest(t, args...)
		if code != exitOK || stdout == "" || stderr != "" {
			t.Errorf("%v: exit %d\nstdout %q\nstderr %q", args, code, stdout, stderr)
		}
	}
	for _, c := range []struct {
		args []string
		hint string
	}{
		{[]string{"-bogus"}, `Run "tforphans -h" for usage.`},
		{[]string{"adopt", "-limit", "x"}, `Run "tforphans adopt -h" for usage.`},
		{[]string{"doctor", "-bogus"}, `Run "tforphans doctor -h" for usage.`},
		{[]string{"init", "-bogus"}, `Run "tforphans init -h" for usage.`},
		{[]string{"diff", "-bogus"}, `Run "tforphans diff -h" for usage.`},
	} {
		code, stdout, stderr := runReportTest(t, c.args...)
		lines := strings.Split(strings.TrimSpace(stderr), "\n")
		if code != exitError || stdout != "" || len(lines) != 2 || !strings.HasPrefix(lines[0], "error: ") || lines[1] != c.hint {
			t.Errorf("%v: exit %d\nstdout %q\nstderr %q", c.args, code, stdout, stderr)
		}
	}
}

// TestHelpGroupsEveryFlag: a new flag must get a heading in -h.
func TestHelpGroupsEveryFlag(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, cmd := range []string{"", "adopt", "doctor"} {
		args := []string{"-h"}
		if cmd != "" {
			args = []string{cmd, "-h"}
		}
		var out bytes.Buffer
		if _, err := parseFlags(args, &out); !errors.Is(err, flag.ErrHelp) {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "Other flags:") {
			t.Errorf("%q: flags without a group:\n%s", cmd, out.String())
		}
	}
}

// TestCheckRoots: a missing directory is named before terraform runs, with
// the command it was probably meant to be.
func TestCheckRoots(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("file", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{nil, "no Terraform configuration in the current directory"},
		{[]string{"doctr"}, `ROOT_DIR "doctr" does not exist; did you mean tforphans doctor?`},
		{[]string{"-quiet", "adopt"}, `ROOT_DIR "adopt" does not exist; the command goes first: tforphans adopt [flags]`},
		{[]string{"no/such/dir"}, `ROOT_DIR "no/such/dir" does not exist`},
		{[]string{"file"}, `ROOT_DIR "file" is not a directory`},
		{[]string{"-no-query", "-plugin", "true", "-state-roots", "gone"}, `-state-roots "gone" does not exist`},
		{[]string{"doctor", "nope"}, `ROOT_DIR "nope" does not exist`},
	} {
		code, stdout, stderr := runReportTest(t, append([]string{"-no-config"}, c.args...)...)
		if c.args != nil && c.args[0] == "doctor" {
			code, stdout, stderr = runReportTest(t, "doctor", "-no-config", c.args[1])
		}
		if code != exitError || stdout != "" || !strings.Contains(stderr, "error: "+c.want) {
			t.Errorf("%v: exit %d\nstdout %q\nstderr %q", c.args, code, stdout, stderr)
		}
	}
}

func TestNegativeLimits(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, f := range []string{"-max-roots=-1", "-timeout=-1s", "-source-timeout=-1s"} {
		if _, err := parseFlags([]string{"-no-config", f}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "must not be negative") {
			t.Errorf("%s: %v", f, err)
		}
	}
	if _, err := parseFlags([]string{"-no-config", "-max-roots=0", "-timeout=0"}, &bytes.Buffer{}); err != nil {
		t.Error(err)
	}
}

func TestNaturalCompare(t *testing.T) {
	got := []string{"policy-10", "policy-2", "policy-1", "a01", "a1", "b", "", "policy-02"}
	slices.SortFunc(got, naturalCompare)
	want := []string{"", "a01", "a1", "b", "policy-1", "policy-02", "policy-2", "policy-10"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	for _, p := range [][2]string{{"a01", "a1"}, {"x9", "x10"}, {"9", "a"}} {
		if naturalCompare(p[0], p[1]) != -naturalCompare(p[1], p[0]) || naturalCompare(p[0], p[1]) == 0 {
			t.Errorf("%q, %q: not antisymmetric", p[0], p[1])
		}
	}
}

func TestUnverifiedReasons(t *testing.T) {
	if got := unverifiedReasons(nil); got != "" {
		t.Errorf("none: %q", got)
	}
	fs := []report.Finding{{Reason: "listing incomplete"}, {Reason: "plugin results cannot be verified"}, {Reason: "listing incomplete"}, {}}
	if got, want := unverifiedReasons(fs), " (listing incomplete 2, no listing evidence 1, plugin results cannot be verified 1)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestNoAllClearWhenNothingChecked: a run with gaps must not print a clean
// result, and one that listed nothing prints no result at all.
func TestNoAllClearWhenNothingChecked(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("empty.tfstate", []byte(`{"version":4,"resources":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, plugin := range []string{"false", `printf '%s\n' '{"error":"denied"}'`} {
		_, stdout, _ := runReportTest(t, "-no-config", "-no-query", "-state", "empty.tfstate", "-plugin", plugin)
		if !strings.Contains(stdout, "No unmanaged resources found, but coverage is incomplete (see Coverage).") {
			t.Errorf("%s: %s", plugin, stdout)
		}
	}
	// A terraform that fails on everything: no schema, nothing listed.
	if err := os.WriteFile("main.tf", []byte("terraform {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "terraform")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runReportTest(t, "-no-config", "-terraform", bin)
	if code != exitError || stdout != "" {
		t.Errorf("exit %d\nstdout %q\nstderr %s", code, stdout, stderr)
	}
}

func TestBaselineSaysNew(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("empty.tfstate", []byte(`{"version":4,"resources":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	known := model.Resource{Type: "vault_policy", Identity: map[string]any{"id": "known"}, Source: "plugin1"}
	saveReportTest(t, "old.json", []model.Resource{known}, report.Summary{})
	code, stdout, stderr := runReportTest(t, "-no-config", "-no-query", "-state", "empty.tfstate", "-baseline", "old.json",
		"-plugin", `printf '%s\n' '{"type":"vault_policy","identity":{"id":"known"}}' '{"done":true}'`)
	if code != exitOK || !strings.Contains(stdout, "New unmanaged resources (0)") || !strings.Contains(stdout, "No new unmanaged resources found.") ||
		!strings.Contains(stderr, "0 not verified\n") {
		t.Errorf("exit %d\n%s\n%s", code, stdout, stderr)
	}
}

func TestSummaryLine(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("empty.tfstate", []byte(`{"version":4,"resources":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runReportTest(t, "-no-config", "-no-query", "-state", "empty.tfstate",
		"-plugin", `printf '%s\n' '{"type":"vault_policy","identity":{"id":"a"}}' '{"error":"denied"}'`)
	if code != exitOrphans || !strings.Contains(stderr, "0 skipped, 1 plugin run\n") || !strings.Contains(stderr, "INCOMPLETE (1 warning in Diagnostics)") {
		t.Errorf("exit %d\n%s", code, stderr)
	}
	var b bytes.Buffer
	w := report.WithColor(&b, true)
	if got := countAccent(w, report.Attention, 0); got != "0" {
		t.Errorf("zero colored: %q", got)
	}
	if got := countAccent(w, report.Attention, 3); got == "3" {
		t.Errorf("count not colored: %q", got)
	}
}

func TestAdoptExistingFile(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("empty.tfstate", []byte(`{"version":4,"resources":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("imports.tf", []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plugin := `printf '%s\n' '{"type":"vault_policy","identity":{"id":"a"},"import_id":"a"}' '{"error":"denied"}'`
	code, _, stderr := runReportTest(t, "adopt", "-no-config", "-no-query", "-state", "empty.tfstate", "-plugin", plugin, "-o", "imports.tf")
	if code != exitError || !strings.Contains(stderr, "imports.tf already exists, and adopt does not overwrite files") {
		t.Errorf("exit %d\n%s", code, stderr)
	}
	code, _, stderr = runReportTest(t, "adopt", "-no-config", "-no-query", "-state", "empty.tfstate", "-plugin", plugin, "-o", "new\x1b[31m.tf")
	if code != exitOK || strings.Contains(stderr, "\x1b[31m") || !strings.Contains(stderr, "Coverage is incomplete") {
		t.Errorf("exit %d\n%q", code, stderr)
	}
}

func TestInitNamesConfigWithoutRoot(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, d := range []string{"a", "b"} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "main.tf"), []byte("terraform {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"init", "-dry-run"}, &stdout, &stderr)
	if code != exitError || !strings.Contains(stderr.String(), "these only have configuration: a, b; pass the roots among them as DIR, e.g. tforphans init a b") {
		t.Errorf("exit %d\n%s", code, stderr.String())
	}
	// A directory named like a flag: the example must still parse.
	if err := os.Rename("a", "-scan"); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	code = run(context.Background(), []string{"init", "-dry-run"}, &stdout, &stderr)
	if code != exitError || !strings.Contains(stderr.String(), "e.g. tforphans init -- -scan b") {
		t.Errorf("exit %d\n%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"init", "-dry-run", "--", "-scan"}, &stdout, &stderr); code == exitError && strings.Contains(stderr.String(), "flag needs an argument") {
		t.Errorf("example does not parse:\n%s", stderr.String())
	}
}

func TestDoctorShowsIgnoreError(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("main.tf", []byte("terraform {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(defaultIgnoreFile, []byte("aws_instance project.id=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stdout, _ := runReportTest(t, "doctor", "-no-config", "-offline")
	if !strings.Contains(stdout, "line 1") {
		t.Errorf("no line number:\n%s", stdout)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("no space left on device") }

func TestTextWriteError(t *testing.T) {
	for _, rs := range [][]model.Resource{nil, {{Type: "t", Identity: map[string]any{"id": "1"}}}} {
		if err := report.Text(failWriter{}, rs, nil); err == nil {
			t.Errorf("%d resources: write error lost", len(rs))
		}
	}
	if err := report.Text(failWriter{}, nil, []report.Uncertain{{Resource: model.Resource{Type: "t"}}}); err == nil {
		t.Error("uncertain: write error lost")
	}
}
