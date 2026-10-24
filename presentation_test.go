package main

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/report"
)

func TestRedirectedColorDisabled(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	if colorEnabled(&bytes.Buffer{}) {
		t.Fatal("redirected output must be plain")
	}
}

func TestSummarySeparatesGapReasons(t *testing.T) {
	s := report.Summary{Coverage: []report.Coverage{{
		Type: "demo_resource", Status: report.Incomplete,
		Reasons:       []string{report.GapPrefix + "missing argument"},
		ReasonDetails: []report.CoverageReason{{ReasonDetails: model.Reason("missing_argument")}},
	}}}
	var b bytes.Buffer
	printSummary(&b, &options{verbose: true}, s, nil)
	text := b.String()
	header, reason := strings.Index(text, "== Diagnostics "), strings.Index(text, "  reason missing_argument:")
	if header < 0 || reason < header {
		t.Fatalf("gap reasons have no section:\n%s", text)
	}
	// Without -v the codes stay in JSON: no line of links per code.
	b.Reset()
	printSummary(&b, &options{}, s, nil)
	if strings.Contains(b.String(), "reasons.md") {
		t.Fatalf("reason links without -v:\n%s", b.String())
	}
}

func TestSummaryKeepsCoverageLast(t *testing.T) {
	var b bytes.Buffer
	details := func(w io.Writer) error {
		fmt.Fprintln(w)
		report.Section(w, "Baseline")
		_, err := fmt.Fprintln(w, "baseline: 1 new")
		return err
	}
	if err := printSummary(&b, &options{}, report.Summary{Complete: true}, details); err != nil {
		t.Fatal(err)
	}
	text := b.String()
	if strings.Contains(text, "== Diagnostics") {
		t.Fatalf("empty diagnostics have a heading:\n%s", text)
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if lines[len(lines)-1] != "coverage: complete" || strings.Index(text, "baseline: 1 new") > strings.Index(text, "== Coverage") {
		t.Fatalf("coverage is not the last line:\n%s", text)
	}
	if err := printSummary(&b, &options{}, report.Summary{}, func(io.Writer) error { return io.ErrShortWrite }); err != io.ErrShortWrite {
		t.Fatalf("details error lost: %v", err)
	}
}

func TestDiagnosticsHeadingOnFirstWrite(t *testing.T) {
	// A diagnostic no condition lists still gets the heading.
	var b bytes.Buffer
	w := report.WithColor(&b, true)
	d := report.LazySection(w, "Diagnostics")
	if b.Len() != 0 {
		t.Fatal("heading before any diagnostic")
	}
	fmt.Fprintln(d, "warning:", "x")
	fmt.Fprintln(d, "note:", "y")
	text := b.String()
	if strings.Count(text, "== Diagnostics") != 1 || !strings.HasPrefix(text, "\n") || strings.Index(text, "== Diagnostics") > strings.Index(text, "warning:") {
		t.Fatalf("%q", text)
	}
	if !strings.Contains(text, "\x1b[") || report.Accent(d, report.Attention, "z") == "z" {
		t.Fatalf("colors lost through the section: %q", text)
	}
}
