package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// fakeVault answers the calls of vault.sh; VAULT_MODE breaks it:
// warn adds a warning on stderr to every answer, garbage gives text
// instead of JSON for policies, denied fails sys/mounts.
const fakeVault = `#!/bin/sh
[ "${VAULT_MODE:-}" = warn ] && echo "WARNING! VAULT_ADDR and -address unset" >&2
case "$*" in
"read -format=json sys/auth")
	echo '{"data":{"token/":{"type":"token"},"approle/":{"type":"approle"},"gh/":{"type":"github"}}}' ;;
"list -format=json auth/approle/role")
	echo '["ci"]' ;;
"read -format=json sys/mounts")
	if [ "${VAULT_MODE:-}" = denied ]; then echo "Code: 403. permission denied" >&2; exit 2; fi
	echo '{"data":{"sys/":{"type":"system"},"kv/":{"type":"kv"}}}' ;;
"policy list -format=json")
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
		{"garbage", []string{"approle", "auth/approle/role/ci", "gh", "kv"}, "list policies: unexpected output"},
		{"denied", []string{"approle", "auth/approle/role/ci", "gh", "ops"}, "read sys/mounts: Code: 403"},
	} {
		t.Setenv("VAULT_MODE", tc.mode)
		var out, errb bytes.Buffer
		code := run(context.Background(), []string{"-no-query", "-state", state, "-plugin", "sh examples/plugins/vault.sh", "-format", "json", "-strict"}, &out, &errb)
		var rep struct {
			Orphans []struct {
				Identity map[string]any `json:"identity"`
			} `json:"orphans"`
		}
		if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
			t.Fatalf("%q: exit %d: %v\n%s\n%s", tc.mode, code, err, out.String(), errb.String())
		}
		var ids []string
		for _, o := range rep.Orphans {
			ids = append(ids, o.Identity["id"].(string))
		}
		slices.Sort(ids)
		if !slices.Equal(ids, tc.ids) {
			t.Errorf("%q: ids %v, want %v", tc.mode, ids, tc.ids)
		}
		incomplete := bytes.Contains(errb.Bytes(), []byte("coverage: INCOMPLETE"))
		if tc.failure == "" && (code != exitOrphans || incomplete) || tc.failure != "" && (code != exitError || !incomplete || !bytes.Contains(errb.Bytes(), []byte(tc.failure))) {
			t.Errorf("%q: exit %d\n%s", tc.mode, code, errb.String())
		}
	}
}
