//go:build unix

package main

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestGoldenJSON runs the binary on a fixed state and plugin output and
// compares the JSON report with testdata/golden (TFORPHANS_UPDATE_GOLDEN=1
// rewrites it). The report is also checked on its own: totals add up, big
// numbers stay exact, and every key is described in docs/json.md.
func TestGoldenJSON(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"state.json", "plugin.jsonl", "plugin-later.jsonl", "ignore"} {
		data, err := os.ReadFile(filepath.Join("testdata", "golden", f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	docs, err := os.ReadFile(filepath.Join("docs", "json.md"))
	if err != nil {
		t.Fatal(err)
	}
	base := []string{"-no-config", "-no-query", "-state", "state.json", "-ignore", "ignore", "-format", "json"}
	runs := []struct {
		golden string
		args   []string
		code   int
	}{
		{"report.json", []string{"-plugin", "cat plugin.jsonl", "-plugin", "exit 3"}, exitError},
		// The earlier report (its golden file) as the baseline of a later
		// run.
		{"report-baseline.json", []string{"-plugin", "cat plugin-later.jsonl", "-baseline", "report.json"}, exitOrphans},
		{"report-explain.json", []string{"-plugin", "cat plugin.jsonl", "-explain"}, exitOrphans},
	}
	for _, r := range runs {
		t.Run(r.golden, func(t *testing.T) {
			if old, err := os.ReadFile(filepath.Join("testdata", "golden", "report.json")); err != nil {
				t.Fatal(err)
			} else if err := os.WriteFile(filepath.Join(dir, "report.json"), old, 0o600); err != nil {
				t.Fatal(err)
			}
			code, stdout, stderr := runBinary(t, dir, append(slices.Clone(base), r.args...)...)
			if code != r.code {
				t.Fatalf("exit %d\n%s", code, stderr)
			}
			path := filepath.Join("testdata", "golden", r.golden)
			if os.Getenv("TFORPHANS_UPDATE_GOLDEN") == "1" {
				if err := os.WriteFile(path, []byte(stdout), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if stdout != string(want) {
				t.Errorf("report differs from %s (TFORPHANS_UPDATE_GOLDEN=1 rewrites it):\n%s", path, stdout)
			}
			checkReport(t, []byte(stdout), string(docs))
		})
	}
}

// checkReport checks a JSON report without the golden file.
func checkReport(t *testing.T, data []byte, docs string) {
	t.Helper()
	if !bytes.Contains(data, []byte(`"n": 12345678901234567890`)) {
		t.Error("big identity number not exact")
	}
	var rep struct {
		Orphans   []json.RawMessage `json:"orphans"`
		Uncertain []json.RawMessage `json:"uncertain"`
		Summary   struct {
			Live, Managed, Uncertain, Ignored, Orphans int
			ByType                                     map[string]struct{ Live, Managed, Uncertain, Ignored, Orphans int } `json:"by_type"`
			Coverage                                   []struct {
				Type                                       string
				Live, Managed, Uncertain, Ignored, Orphans int
			}
		}
	}
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatal(err)
	}
	s := rep.Summary
	if s.Live != s.Managed+s.Uncertain+s.Ignored+s.Orphans || s.Orphans != len(rep.Orphans) || s.Uncertain != len(rep.Uncertain) {
		t.Errorf("totals do not add up: %+v, %d orphans, %d uncertain listed", s, len(rep.Orphans), len(rep.Uncertain))
	}
	var sum [5]int
	for typ, c := range s.ByType {
		if c.Live != c.Managed+c.Uncertain+c.Ignored+c.Orphans {
			t.Errorf("by_type %s does not add up: %+v", typ, c)
		}
		sum[0], sum[1], sum[2], sum[3], sum[4] = sum[0]+c.Live, sum[1]+c.Managed, sum[2]+c.Uncertain, sum[3]+c.Ignored, sum[4]+c.Orphans
	}
	if sum != [5]int{s.Live, s.Managed, s.Uncertain, s.Ignored, s.Orphans} {
		t.Errorf("by_type sums %v, summary %+v", sum, s)
	}
	// These reports have no scopes: one coverage row per type, every type.
	var rows []string
	for _, c := range s.Coverage {
		rows = append(rows, c.Type)
	}
	slices.Sort(rows)
	if types := slices.Sorted(maps.Keys(s.ByType)); !slices.Equal(rows, types) {
		t.Errorf("coverage rows %v, by_type %v", rows, types)
	}
	for _, c := range s.Coverage {
		if b := s.ByType[c.Type]; b.Live != c.Live || b.Managed != c.Managed || b.Uncertain != c.Uncertain || b.Ignored != c.Ignored || b.Orphans != c.Orphans {
			t.Errorf("coverage %s %+v, by_type %+v", c.Type, c, b)
		}
	}
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	for _, k := range jsonKeys(doc, nil) {
		// As `key`, as a key of a JSON example, or in [{a, b}].
		inBraces := regexp.MustCompile(`[{,]\s*` + regexp.QuoteMeta(k) + `\s*[,}]`)
		if !strings.Contains(docs, "`"+k+"`") && !strings.Contains(docs, `"`+k+`":`) && !inBraces.MatchString(docs) {
			t.Errorf("key %q is not described in docs/json.md", k)
		}
	}
}

// jsonKeys collects object keys, except inside maps keyed by data (identity
// keys, type names, scopes).
func jsonKeys(v any, keys []string) []string {
	switch v := v.(type) {
	case map[string]any:
		for k, e := range v {
			if !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
			switch k {
			case "identity", "attributes", "by_type", "by_scope":
				continue
			}
			keys = jsonKeys(e, keys)
		}
	case []any:
		for _, e := range v {
			keys = jsonKeys(e, keys)
		}
	}
	return keys
}
