package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/plugin"
)

// fakeVault answers the calls of vault.sh; VAULT_MODE breaks it:
// warn adds a warning on stderr to every answer, garbage gives text
// instead of JSON for policies, denied fails sys/mounts, empty finds no objects.
const fakeVault = `#!/bin/sh
[ "${VAULT_MODE:-}" = warn ] && echo "WARNING! VAULT_ADDR and -address unset" >&2
case "$*" in
"read -format=json sys/auth")
	if [ "${VAULT_MODE:-}" = empty ]; then echo '{"data":{"token/":{"type":"token"}}}'; exit 0; fi
	echo '{"data":{"token/":{"type":"token"},"approle/":{"type":"approle"},"gh/":{"type":"github"}}}' ;;
"list -format=json auth/approle/role")
	echo '["ci"]' ;;
"read -format=json sys/mounts")
	if [ "${VAULT_MODE:-}" = empty ]; then echo '{"data":{}}'; exit 0; fi
	if [ "${VAULT_MODE:-}" = denied ]; then echo "Code: 403. permission denied" >&2; exit 2; fi
	echo '{"data":{"sys/":{"type":"system"},"kv/":{"type":"kv"}}}' ;;
"policy list -format=json")
	if [ "${VAULT_MODE:-}" = empty ]; then echo '["default","root"]'; exit 0; fi
	if [ "${VAULT_MODE:-}" = garbage ]; then echo 'not json'; exit 0; fi
	echo '["default","root","ops"]' ;;
*)
	echo "unexpected: $*" >&2; exit 2 ;;
esac
`

// TestVaultPlugin: vault.sh against a fake vault CLI. A warning on stderr
// changes nothing; output jq cannot read, or a failed call, is an error
// line, so coverage is incomplete instead of an empty Vault looking
// checked.
func TestVaultPlugin(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not in PATH")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vault"), []byte(fakeVault), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	state := filepath.Join(dir, "terraform.tfstate")
	if err := os.WriteFile(state, []byte(`{"version":4,"lineage":"l","serial":1,"resources":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	all := []string{"approle", "auth/approle/role/ci", "gh", "kv", "ops"}
	for _, tc := range []struct {
		mode    string
		ids     []string
		failure string
	}{
		{"", all, ""},
		{"warn", all, ""},
		{"empty", nil, ""},
		{"garbage", []string{"approle", "auth/approle/role/ci", "gh", "kv"}, "list policies: unexpected output"},
		{"denied", []string{"approle", "auth/approle/role/ci", "gh", "ops"}, "read sys/mounts: Code: 403"},
	} {
		t.Setenv("VAULT_MODE", tc.mode)
		t.Setenv("VAULT_ADDR", "https://tok:secret@Vault.Example.com:8200/v1?x=1")
		var out, errb bytes.Buffer
		code := run(context.Background(), []string{"-no-query", "-state", state, "-plugin", "sh ../../examples/plugins/vault.sh", "-format", "json", "-strict"}, &out, &errb)
		var rep struct {
			Summary struct {
				Complete bool `json:"complete"`
			} `json:"summary"`
			Orphans []struct {
				Identity map[string]any `json:"identity"`
				Target   string         `json:"target"`
			} `json:"orphans"`
		}
		if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
			t.Fatalf("%q: exit %d: %v\n%s\n%s", tc.mode, code, err, out.String(), errb.String())
		}
		var ids []string
		for _, o := range rep.Orphans {
			ids = append(ids, o.Identity["id"].(string))
			// Host and port only: no scheme, user or path of VAULT_ADDR.
			if o.Target != "vault.example.com:8200" {
				t.Errorf("%q: target %q", tc.mode, o.Target)
			}
		}
		slices.Sort(ids)
		if !slices.Equal(ids, tc.ids) {
			t.Errorf("%q: ids %v, want %v", tc.mode, ids, tc.ids)
		}
		wantCode := exitOrphans
		if tc.mode == "empty" {
			wantCode = exitOK
		}
		if rep.Summary.Complete != (tc.failure == "") {
			t.Errorf("%q: coverage: %s", tc.mode, &out)
		}
		incomplete := bytes.Contains(errb.Bytes(), []byte("coverage: INCOMPLETE"))
		if tc.failure == "" && (code != wantCode || incomplete) || tc.failure != "" && (code != exitError || !incomplete || !bytes.Contains(errb.Bytes(), []byte(tc.failure))) {
			t.Errorf("%q: exit %d\n%s", tc.mode, code, errb.String())
		}
	}
}

func TestVaultPluginDependencies(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not in PATH")
	}
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq not in PATH")
	}
	utilities := make(map[string]string)
	for _, name := range []string{"mktemp", "cat", "rm"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		utilities[name] = path
	}
	for _, c := range []struct {
		name, jq, mode, message string
		vault                   bool
	}{
		{"missing jq", "", "", "jq not found in PATH", true},
		{"missing vault", "#!/bin/sh\nexit 0\n", "", "vault not found in PATH", false},
		{"broken jq", "#!/bin/sh\nexit 126\n", "", "", true},
		{"resource failure", "#!/bin/sh\nif [ \"$1\" = -nc ] && [ \"$3\" = t ]; then exit 126; fi\nexec \"$VAULT_TEST_JQ\" \"$@\"\n", "", "", true},
		{"warning failure", "#!/bin/sh\nif [ \"$1\" = -nc ] && [ \"$3\" = e ]; then exit 126; fi\nexec \"$VAULT_TEST_JQ\" \"$@\"\n", "denied", "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, path := range utilities {
				if err := os.Symlink(path, filepath.Join(dir, name)); err != nil {
					t.Fatal(err)
				}
			}
			if c.vault {
				if err := os.WriteFile(filepath.Join(dir, "vault"), []byte(fakeVault), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if c.jq != "" {
				if err := os.WriteFile(filepath.Join(dir, "jq"), []byte(c.jq), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", dir)
			t.Setenv("VAULT_MODE", c.mode)
			t.Setenv("VAULT_TEST_JQ", jq)
			var out, errb bytes.Buffer
			cmd := exec.Command(sh, "../../examples/plugins/vault.sh")
			cmd.Stdout, cmd.Stderr = &out, &errb
			if err := cmd.Run(); err == nil {
				t.Fatalf("successful exit: %s", &out)
			} else if _, ok := err.(*exec.ExitError); !ok {
				t.Fatal(err)
			}
			if bytes.Contains(out.Bytes(), []byte(`"done"`)) {
				t.Errorf("done after failure: %s", &out)
			}
			if c.message != "" && (out.Len() != 0 || !bytes.Contains(errb.Bytes(), []byte(c.message))) {
				t.Errorf("stdout: %s\nstderr: %s", &out, &errb)
			}
		})
	}
}

// TestVaultPluginTargetIPv6: a bracketed IPv6 address keeps its host and
// port as the target.
func TestVaultPluginTargetIPv6(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not in PATH")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vault"), []byte(fakeVault), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("VAULT_ADDR", "https://[FD00::1]:8200/")
	cmd := exec.Command("sh", "../../examples/plugins/vault.sh")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"target":"[fd00::1]:8200"`) {
		t.Errorf("no IPv6 target in:\n%s", out)
	}
}

// TestVaultPluginTargetNamespace: the namespace joins the target, case and
// slashes intact (hex), and no namespace changes nothing.
func TestVaultPluginTargetNamespace(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not in PATH")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vault"), []byte(fakeVault), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("VAULT_ADDR", "https://vault.example:8200")
	for ns, want := range map[string]string{
		"":       `"target":"vault.example:8200"`,
		"/":      `"target":"vault.example:8200"`,
		"Admin/": `"target":"vault.example:8200_ns_41646d696e"`,
		"/a/B":   `"target":"vault.example:8200_ns_612f42"`,
		"admin":  `"target":"vault.example:8200_ns_61646d696e"`,
		"ADMIN":  `"target":"vault.example:8200_ns_41444d494e"`,
	} {
		t.Setenv("VAULT_NAMESPACE", ns)
		out, err := exec.Command("sh", "../../examples/plugins/vault.sh").Output()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), want) {
			t.Errorf("namespace %q: want %s in\n%s", ns, want, out)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if _, _, _, err := plugin.Parse([]byte(line), "plugin1"); err != nil {
				t.Errorf("namespace %q: %v: %s", ns, err, line)
			}
		}
	}
}

// The longest target tforphans accepts is 260 characters: a namespace that
// fills it exactly is kept, one more byte leaves the target empty.
func TestVaultPluginTargetNamespaceLength(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not in PATH")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vault"), []byte(fakeVault), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("VAULT_ADDR", "https://ab")
	for n, wantTarget := range map[int]bool{127: true, 128: false} {
		t.Setenv("VAULT_NAMESPACE", strings.Repeat("n", n))
		out, err := exec.Command("sh", "../../examples/plugins/vault.sh").Output()
		if err != nil {
			t.Fatal(err)
		}
		line, _, _ := strings.Cut(string(out), "\n")
		res, _, _, err := plugin.Parse([]byte(line), "plugin1")
		if err != nil || len(res) != 1 {
			t.Fatalf("%d: %v %s", n, err, out)
		}
		if got := res[0].Target != ""; got != wantTarget || (got && len(res[0].Target) != 260) {
			t.Errorf("namespace of %d bytes: target %q", n, res[0].Target)
		}
	}
}
