package dsource

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCheckAuth(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell wrapper")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "terraform")
	log := filepath.Join(dir, "commands")
	t.Setenv("DOCTOR_LOG", log)
	t.Setenv("TF_CLI_ARGS", "must be removed")
	t.Setenv("TF_DATA_DIR", "must be isolated")
	t.Setenv("TF_WORKSPACE", "must be isolated")
	script := `#!/bin/sh
if [ -n "$TF_CLI_ARGS$TF_DATA_DIR$TF_WORKSPACE" ]; then exit 99; fi
printf '%s\n' "$*" >> "$DOCTOR_LOG"
case "$1" in
 init) exit 0 ;;
 plan)
  grep 'data "proxmox_virtual_environment_version" "probe"' main.tf >/dev/null || exit 98
  if [ "$DOCTOR_FAIL" = 1 ]; then printf 'HTTP 401' >&2; exit 1; fi
  exit 0 ;;
 *) exit 97 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	lock := `provider "registry.terraform.io/bpg/proxmox" {
 version = "0.100.0"
 hashes = ["h1:test"]
}`
	if err := os.WriteFile(filepath.Join(dir, ".terraform.lock.hcl"), []byte(lock), 0600); err != nil {
		t.Fatal(err)
	}
	opt := Options{TerraformBin: bin, VersionRoot: dir}
	if err := CheckAuth(context.Background(), Proxmox, opt); err != nil {
		t.Fatal(err)
	}
	commands, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	text := string(commands)
	if !strings.Contains(text, "-lockfile=readonly") || !strings.Contains(text, "plan -refresh=false -lock=false") || strings.Contains(text, "apply") || strings.Contains(text, "query") {
		t.Fatal(text)
	}
	t.Setenv("DOCTOR_FAIL", "1")
	if err := CheckAuth(context.Background(), Proxmox, opt); err == nil {
		t.Fatal("probe failure accepted")
	}
	if err := CheckAuth(context.Background(), Cloudflare, opt); err == nil {
		t.Fatal("missing probe accepted")
	}
	if err := CheckAuth(context.Background(), Proxmox, Options{}); err == nil {
		t.Fatal("unpinned probe accepted")
	}
}
