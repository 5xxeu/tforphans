package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestCoverageLevelAndStrictUnlisted: types in the states that their
// provider cannot list make coverage partial, and -strict-unlisted fails.
func TestCoverageLevelAndStrictUnlisted(t *testing.T) {
	t.Chdir(t.TempDir())
	const withMount = `{"version":4,"resources":[{"mode":"managed","type":"vault_mount","name":"m","provider":"provider[\"registry.terraform.io/hashicorp/vault\"]","instances":[{"attributes":{"path":"secret"}}]}]}`
	if err := os.WriteFile("mount.tfstate", []byte(withMount), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("empty.tfstate", []byte(`{"version":4,"resources":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plugin := `printf '%s\n' '{"type":"vault_policy","identity":{"id":"ci"},"provider_source":"hashicorp/vault"}' '{"done":true}'`
	level := func(args ...string) (int, string, string) {
		t.Helper()
		code, stdout, stderr := runReportTest(t, append([]string{"-no-config", "-no-query", "-plugin", plugin, "-format", "json"}, args...)...)
		var doc struct {
			Summary struct {
				Level string `json:"coverage_level"`
			} `json:"summary"`
		}
		if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
			t.Fatalf("exit %d: %v\n%s\n%s", code, err, stdout, stderr)
		}
		return code, doc.Summary.Level, stderr
	}
	if code, lvl, stderr := level("-state", "empty.tfstate"); lvl != "full" || code != exitOrphans {
		t.Fatalf("empty state: exit %d, level %q\n%s", code, lvl, stderr)
	}
	if code, lvl, stderr := level("-state", "mount.tfstate"); lvl != "partial" || code != exitOrphans {
		t.Fatalf("vault_mount in the state: exit %d, level %q\n%s", code, lvl, stderr)
	}
	code, _, stderr := level("-state", "mount.tfstate", "-strict-unlisted")
	if code != exitError || !strings.Contains(stderr, "-strict-unlisted: 1 resource types") {
		t.Fatalf("-strict-unlisted: exit %d\n%s", code, stderr)
	}
}
