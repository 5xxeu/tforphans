package main

import (
	"bytes"
	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/report"
	"strings"
	"testing"
)

func TestRedirectedColorDisabled(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	if colorEnabled(&bytes.Buffer{}) {
		t.Fatal("redirected output must be plain")
	}
}

func TestSummarySeparatesGapReasons(t *testing.T) {
	var b bytes.Buffer
	printSummary(&b, &options{}, report.Summary{Coverage: []report.Coverage{{
		Type: "demo_resource", Status: report.Incomplete,
		Reasons:       []string{report.GapPrefix + "missing argument"},
		ReasonDetails: []report.CoverageReason{{ReasonDetails: model.Reason("missing_argument")}},
	}}})
	text := b.String()
	header, reason := strings.Index(text, "== Diagnostics "), strings.Index(text, "  reason missing_argument:")
	if header < 0 || reason < header {
		t.Fatalf("gap reasons have no section:\n%s", text)
	}
}
