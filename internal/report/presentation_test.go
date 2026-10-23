package report

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/model"
)

func TestPresentationPreservesColumns(t *testing.T) {
	resources := []model.Resource{
		{Type: "a", DisplayName: "first", Identity: map[string]any{"id": "1"}},
		{Type: "a_much_longer_type", DisplayName: "second", Identity: map[string]any{"id": "2"}},
	}
	var plain, colored bytes.Buffer
	if err := Text(&plain, resources, nil); err != nil {
		t.Fatal(err)
	}
	if err := Text(WithColor(&colored, true), resources, nil); err != nil {
		t.Fatal(err)
	}
	stripped := regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(colored.String(), "")
	if stripped != plain.String() {
		t.Fatalf("color changed layout:\n%s\nplain:\n%s", stripped, plain.String())
	}
	lines := strings.Split(plain.String(), "\n")
	first, second := -1, -1
	for _, line := range lines {
		if strings.Contains(line, "first") {
			first = strings.Index(line, "first")
		}
		if strings.Contains(line, "second") {
			second = strings.Index(line, "second")
		}
	}
	if first < 0 || first != second {
		t.Fatalf("group spacing broke columns:\n%s", plain.String())
	}
	if !strings.Contains(colored.String(), "\x1b[1;36m") {
		t.Fatal("missing heading accent")
	}
}

func TestPresentationDiagnosticsAndCoverage(t *testing.T) {
	var b bytes.Buffer
	w := WithColor(&b, true)
	for _, line := range []string{"error: bad", "warning: partial", "note: hint", "coverage: INCOMPLETE", "coverage: complete"} {
		fmt.Fprintln(w, line)
	}
	for _, code := range []string{"1;31", "1;33", "1;36", "32"} {
		if !strings.Contains(b.String(), "\x1b["+code+"m") {
			t.Errorf("missing %s", code)
		}
	}
	b.Reset()
	if err := writeTable(w, "TYPE  COVERAGE\nx     incomplete\ny     complete\n"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "\x1b[1;33mincomplete\x1b[0m") || !strings.Contains(b.String(), "\x1b[32mcomplete\x1b[0m") {
		t.Fatal(b.String())
	}
}

type presentationShortWriter struct{}

func (presentationShortWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }

func TestPresentationWriteFailure(t *testing.T) {
	if _, err := io.WriteString(WithColor(presentationShortWriter{}, true), "warning: partial"); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("got %v", err)
	}
}
