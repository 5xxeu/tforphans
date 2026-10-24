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
	if !strings.Contains(plain.String(), "\n\n  a_much") {
		t.Fatalf("group separator is not an empty line:\n%q", plain.String())
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
	fmt.Fprintln(w, "coverage: complete for what can be listed; not checked: 1 types")
	if !strings.HasPrefix(b.String(), "\x1b[1;33mcoverage: complete\x1b[0m") {
		t.Fatalf("qualified coverage marked as complete: %q", b.String())
	}
	b.Reset()
	if err := writeTable(w, "TYPE  COVERAGE\nx     incomplete\ny     complete\n", true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "\x1b[1;33mincomplete\x1b[0m") || !strings.Contains(b.String(), "\x1b[32mcomplete\x1b[0m") {
		t.Fatal(b.String())
	}
	// Findings may end with these words or start with "total ": data, not labels.
	b.Reset()
	if err := writeTable(w, "SCOPE        SOURCE\ntotal cost   complete\n", false); err != nil {
		t.Fatal(err)
	}
	if strings.Count(b.String(), "\x1b[") != 2 {
		t.Fatalf("data rows were styled: %q", b.String())
	}
}

type presentationShortWriter struct{}

func (presentationShortWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }

func TestPresentationWriteFailure(t *testing.T) {
	if _, err := io.WriteString(WithColor(presentationShortWriter{}, true), "warning: partial"); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("got %v", err)
	}
}

type presentationLimitedWriter struct {
	limit int
	err   error
}

func (w presentationLimitedWriter) Write(p []byte) (int, error) { return min(len(p), w.limit), w.err }

func TestPresentationReportsConsumedInput(t *testing.T) {
	for _, tc := range []struct {
		name, text  string
		limit, want int
		failure     error
	}{
		{"plain partial", "hello", 2, 2, nil},
		{"inside opening color", "warning: partial", 3, 0, nil},
		{"inside label", "warning: partial", 10, 3, nil},
		{"inside reset", "warning: partial", 17, 8, nil},
		{"inside message", "warning: partial", 22, 11, io.ErrClosedPipe},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, err := WithColor(presentationLimitedWriter{tc.limit, tc.failure}, true).Write([]byte(tc.text))
			wantErr := tc.failure
			if wantErr == nil {
				wantErr = io.ErrShortWrite
			}
			if n != tc.want || !errors.Is(err, wantErr) {
				t.Fatalf("got %d, %v; want %d, %v", n, err, tc.want, wantErr)
			}
		})
	}
}

// TestOrphanTableScopes: one heading per scope names the shared zone_id,
// which its rows leave out; rows of another scope keep their own keys.
func TestOrphanTableScopes(t *testing.T) {
	resources := []model.Resource{
		{Type: "cloudflare_dns_record", DisplayName: "A a.example.com", Source: "-scan cloudflare", ScopeName: "zone example.com",
			Identity: map[string]any{"id": "r1", "zone_id": "z1"}},
		{Type: "cloudflare_dns_record", DisplayName: "A b.example.com", Source: "-scan cloudflare", ScopeName: "zone example.com",
			Identity: map[string]any{"id": "r2", "zone_id": "z1"}},
		{Type: "cloudflare_zone", DisplayName: "example.org", Source: "plugin1", ScopeName: "zone example.org",
			Identity: map[string]any{"id": "z2", "zone_id": "z2"}},
	}
	var b bytes.Buffer
	if err := Text(&b, resources, nil); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"zone example.com (zone_id=z1): 2\n", "zone example.org (zone_id=z2): 1\n", "SOURCE", "id=r1 "} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "zone_id=z1,") || strings.Contains(out, "id=r1, zone_id") || strings.Contains(out, "All found by") {
		t.Errorf("shared key or source repeated:\n%s", out)
	}
}
