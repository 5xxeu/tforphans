package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestTerragruntParentClassification: a terragrunt.hcl that units include is
// an unclassified gap until -terragrunt-parent confirms it (or
// -terragrunt-unit reads it).
func TestTerragruntParentClassification(t *testing.T) {
	root := t.TempDir()
	for name, body := range map[string]string{
		"terragrunt.hcl":     "remote_state {\n  backend = \"local\"\n}\n",
		"app/terragrunt.hcl": "include \"root\" {\n  path = find_in_parent_folders()\n}\nterraform {\n  source = \"./m\"\n}\n",
	} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(t.TempDir())
	base := []string{"-no-config", "-no-query", "-plugin", `printf '%s\n' '{"done":true}'`, "-state-roots", root, "-terragrunt", "-format", "json"}
	summary := func(extra ...string) (code int, unclassified []string, discovery bool) {
		t.Helper()
		code, stdout, stderr := runReportTest(t, append(slices.Clone(base), extra...)...)
		var doc struct {
			Summary struct {
				Unclassified []string `json:"unclassified_terragrunt"`
				Discovery    bool     `json:"discovery_complete"`
			} `json:"summary"`
		}
		if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
			t.Fatalf("exit %d: %v\n%s\n%s", code, err, stdout, stderr)
		}
		return code, doc.Summary.Unclassified, doc.Summary.Discovery
	}
	if _, un, disc := summary(); len(un) != 1 || disc {
		t.Fatalf("unconfirmed parent: %v, discovery %v", un, disc)
	}
	parent, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, un, _ := summary("-terragrunt-parent", parent); len(un) != 0 {
		t.Fatalf("confirmed parent still unclassified: %v", un)
	}
}

func TestTerragruntParentNeedsTerragrunt(t *testing.T) {
	t.Chdir(t.TempDir())
	if code, _, stderr := runReportTest(t, "-no-config", "-terragrunt-parent", "."); code != exitError || !strings.Contains(stderr, "-terragrunt-parent needs -terragrunt") {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
}
