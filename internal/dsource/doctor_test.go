package dsource

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/glob"
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

func TestInspectPlan(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell wrapper")
	}
	root := t.TempDir()
	plugins := filepath.Join(root, ".terraform", "providers", Cloudflare.Provider)
	if err := os.MkdirAll(plugins, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".terraform.lock.hcl"), []byte(`provider "registry.terraform.io/cloudflare/cloudflare" {
 version = "1.0.0"
 hashes = ["h1:test"]
}`), 0600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "terraform")
	script := `#!/bin/sh
[ -z "$TF_CLI_ARGS$TF_DATA_DIR$TF_WORKSPACE" ] || exit 99
case "$1 $2" in
 "init "*)
  case " $* " in *" -plugin-dir="*) ;; *) exit 98 ;; esac
  case " $* " in *" -lockfile=readonly "*) ;; *) exit 98 ;; esac
  [ -f .terraform.lock.hcl ] && [ -f versions.tf ] || exit 97 ;;
 "providers schema") printf '%s' "$DOCTOR_SCHEMA" ;;
 *) exit 96 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCTOR_SCHEMA", schemaJSON)
	t.Setenv("TF_CLI_ARGS", "must be removed")
	t.Setenv("TF_DATA_DIR", filepath.Join(root, ".terraform"))
	t.Setenv("TF_WORKSPACE", "must be removed")
	a := Cloudflare
	a.Context = []string{"cloudflare_accounts"}
	for _, exclude := range []string{"", "*"} {
		opt := Options{TerraformBin: bin, VersionRoot: root, Exclude: glob.CompileList([]string{exclude})}
		got, err := InspectPlan(context.Background(), a, opt)
		if err != nil {
			t.Fatal(err)
		}
		s := testSchema(t)
		want := buildPlan(s, a.prepared(s), opt)
		count := 0
		if len(want.listings) > 0 {
			count = len(want.blocks())
		}
		if got.Blocks != count || !reflect.DeepEqual(got.Skipped, want.skipped) {
			t.Fatalf("plan: %+v, want %d blocks and %+v", got, count, want.skipped)
		}
		if exclude == "*" && got.Blocks != 0 {
			t.Fatal("context-only plan counted as discovery")
		}
	}
	if err := os.RemoveAll(plugins); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectPlan(context.Background(), a, Options{TerraformBin: "must not run", VersionRoot: root}); !errors.Is(err, ErrProviderNotInstalled) {
		t.Fatalf("missing provider: %v", err)
	}
}
