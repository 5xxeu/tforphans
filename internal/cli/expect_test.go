package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/report"
	"github.com/5xxeu/tforphans/internal/state"
)

func TestExpectGap(t *testing.T) {
	o, err := parseFlags([]string{"-no-config", "-no-query", "-plugin", "true", "-expect", "account_id=1,2 region=a", "-expect", "projcet=p"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	sum := report.Summary{}
	sum.AddListing(report.Listing{Source: "infra", Address: "list.aws_vpc.all", Type: "aws_vpc", Complete: true, Scope: map[string]string{"account_id": "1", "region": "a"}})
	conclude(o, state.NewIndex(), nil, nil, &sum)
	if len(sum.Expected) != 3 || sum.Expected[0].Status != report.ExpectConfirmed || sum.Expected[1].Status != report.ExpectMissing {
		t.Fatalf("expected: %+v", sum.Expected)
	}
	if sum.DiscoveryComplete || sum.Complete {
		t.Fatal("a missing expected scope left coverage complete")
	}
	if !strings.Contains(strings.Join(sum.Notes, "\n"), "reports scope projcet") {
		t.Fatalf("no note on the unknown key: %q", sum.Notes)
	}
	var b bytes.Buffer
	printSummary(&b, o, sum, nil)
	text := b.String()
	for _, want := range []string{"== Expected scopes", "  MISSING account_id=2 region=a (0 listings)", "1 of 3 expected scopes confirmed", "2 of 3 expected scopes not confirmed"} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in:\n%s", want, text)
		}
	}
	// Confirmed scopes and reason links only with -v.
	if strings.Contains(text, "confirmed account_id=1") || strings.Contains(text, "reasons.md") {
		t.Errorf("confirmed rows or links without -v:\n%s", text)
	}
	if strings.Index(text, "== Expected scopes") > strings.Index(text, "== Coverage") {
		t.Errorf("expected scopes after coverage:\n%s", text)
	}
	o.verbose = true
	b.Reset()
	printSummary(&b, o, sum, nil)
	if !strings.Contains(b.String(), "  confirmed account_id=1 region=a (1 listing)") || !strings.Contains(b.String(), "reasons.md#expected_scope_missing") {
		t.Errorf("-v:\n%s", b.String())
	}
}

func TestExpectConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	// A file found on its own may set it: it only makes the check stricter.
	if err := os.WriteFile(defaultConfigFile, []byte("expect: [\"project=a,b\", \"account_id=1\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o, err := parseFlags([]string{"-no-query", "-plugin", "true"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(o.audit.expect) != 3 || o.audit.expect[2]["account_id"] != "1" {
		t.Fatalf("expect from the file: %v", o.audit.expect)
	}
	if _, err := parseFlags([]string{"-no-config", "-no-query", "-plugin", "true", "-expect", "project"}, io.Discard); err == nil || !strings.Contains(err.Error(), "KEY=VALUE") {
		t.Fatalf("bad -expect: %v", err)
	}
}
