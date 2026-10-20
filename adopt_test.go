package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/model"
)

// -out-dir writes a file per root module: a plugin assigned with
// -adopt-root goes to that root's file, under the addresses of its
// configuration, and one without goes to _unassigned. Nothing else is left
// next to the new directory.
func TestAdoptOutDir(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	put(t, "vault/main.tf", "resource \"vault_policy\" \"id_ci\" {\n  name = \"ci\"\n}\n")
	put(t, "empty.tfstate", `{"version": 4, "serial": 1, "lineage": "x", "resources": []}`)
	vault := `echo '{"type": "vault_policy", "identity": {"id": "ci"}, "import_id": "ci"}'`
	junos := `echo '{"type": "junos_interface", "identity": {"name": "ge-0/0/1"}, "import_id": "ge-0/0/1"}'`
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"adopt", "-no-config", "-no-query", "-state", "empty.tfstate",
		"-plugin", vault, "-plugin", junos, "-out-dir", "out", "-adopt-root", "plugin1=./vault/"}, &stdout, &stderr)
	if code != exitOK || stdout.Len() != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	got := read(t, "out/vault/imports.tf")
	if !strings.HasPrefix(got, "# Objects that none of the 1 states read manages.") || !strings.Contains(got, "to = vault_policy.id_ci_2\n") || strings.Contains(got, "junos") {
		t.Errorf("vault/imports.tf:\n%s", got)
	}
	got = read(t, "out/_unassigned/plugin2.tf")
	if !strings.Contains(got, "# Not assigned to a root module: plugin2 found") || !strings.Contains(got, "-adopt-root plugin2=DIR") || !strings.Contains(got, `id = "ge-0/0/1"`) {
		t.Errorf("_unassigned/plugin2.tf:\n%s", got)
	}
	for _, want := range []string{"Wrote 2 import blocks to 2 files under out.", "note: 1 objects of plugin2 are in " + filepath.Join("out", "_unassigned", "plugin2.tf")} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr.String())
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".out") {
			t.Errorf("left behind: %s", e.Name())
		}
	}

	// A second run never writes into the directory of the first.
	stderr.Reset()
	code = run(context.Background(), []string{"adopt", "-no-config", "-no-query", "-state", "empty.tfstate", "-plugin", vault, "-out-dir", "out"}, io.Discard, &stderr)
	if code != exitError || !strings.Contains(stderr.String(), "-out-dir out already exists") {
		t.Errorf("exit %d\n%s", code, stderr.String())
	}
}

// A root outside the current directory has no path under -out-dir: adopt
// says so before the run and leaves nothing behind.
func TestAdoptOutDirOutside(t *testing.T) {
	parent := t.TempDir()
	put(t, filepath.Join(parent, "other", "main.tf"), "")
	cwd := filepath.Join(parent, "here")
	put(t, filepath.Join(cwd, "empty.tfstate"), `{"version": 4, "serial": 1, "lineage": "x", "resources": []}`)
	t.Chdir(cwd)
	plugin := `echo '{"type": "vault_policy", "identity": {"id": "ci"}, "import_id": "ci"}'`
	var stderr bytes.Buffer
	code := run(context.Background(), []string{"adopt", "-no-config", "-no-query", "-state", "empty.tfstate", "-plugin", plugin, "-out-dir", "out", "-adopt-root", "plugin1=../other"}, io.Discard, &stderr)
	if code != exitError || !strings.Contains(stderr.String(), "outside the current directory") {
		t.Errorf("exit %d\n%s", code, stderr.String())
	}
	if entries, _ := os.ReadDir(cwd); len(entries) != 1 {
		t.Errorf("left behind: %v", entries)
	}
}

func TestAdoptOutDirFlags(t *testing.T) {
	t.Chdir(t.TempDir())
	put(t, "vault/main.tf", "")
	put(t, "file", "")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-o", "x.tf", "-out-dir", "out"}, "-o and -out-dir cannot be used together"},
		{[]string{"-plugin", "true", "-adopt-root", "plugin1=vault"}, "-adopt-root needs -out-dir"},
		{[]string{"-out-dir", "vault"}, "-out-dir vault already exists"},
		{[]string{"-out-dir", "out", "-adopt-root", "plugin1"}, "want NAME=DIR"},
		{[]string{"-out-dir", "out", "-plugin", "true", "-adopt-root", "plugin2=vault"}, "plugin2 is neither a -scan provider of this run nor one of its 1 plugins"},
		{[]string{"-out-dir", "out", "-adopt-root", "cloudflare=vault"}, "cloudflare is neither"},
		{[]string{"-out-dir", "out", "-plugin", "true", "-adopt-root", "plugin1=vault", "-adopt-root", "plugin1=file"}, "-adopt-root plugin1 is given twice"},
		{[]string{"-out-dir", "out", "-plugin", "true", "-adopt-root", "plugin1=file"}, "file is not a directory"},
		{[]string{"-out-dir", "out", "-plugin", "true", "-adopt-root", "plugin1=missing"}, "-adopt-root plugin1: stat missing"},
		{[]string{"-out-dir", "out", "-plugin", "true", "-adopt-root", "plugin1=.."}, "root module .. is outside the current directory"},
	} {
		_, err := parseFlags(append([]string{"adopt", "-no-config", "-no-query"}, c.args...), io.Discard)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want %q", c.args, err, c.want)
		}
	}
	o, err := parseFlags([]string{"adopt", "-no-config", "-scan", "cloudflare", "-out-dir", "out", "-adopt-root", "cloudflare=vault"}, io.Discard)
	if err != nil || o.adoptRoots["cloudflare"] != "vault" {
		t.Errorf("%v %v", o, err)
	}
	if _, err := parseFlags([]string{"-out-dir", "out"}, io.Discard); err == nil {
		t.Error("-out-dir accepted without adopt")
	}
}

func TestSourceName(t *testing.T) {
	for source, want := range map[string]string{"-scan cloudflare": "cloudflare", "plugin12": "plugin12", "plugin0": "", "infra": "", "pluginx": ""} {
		name, ok := sourceName(source)
		if !ok {
			name = ""
		}
		if name != want {
			t.Errorf("%s: %q, want %q", source, name, want)
		}
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func put(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A pending import block counts only in the root a source is assigned to:
// a imports both policies, plugin1 is assigned to b, where ci is not
// imported yet, plugin2 to a, where ops already is.
func TestAdoptOutDirPendingImports(t *testing.T) {
	t.Chdir(t.TempDir())
	put(t, "a/imports.tf", "import {\n  to = vault_policy.ci\n  id = \"ci\"\n}\nimport {\n  to = vault_policy.ops\n  id = \"ops\"\n}\n")
	put(t, "b/main.tf", "")
	put(t, "empty.tfstate", `{"version": 4, "serial": 1, "lineage": "x", "resources": []}`)
	ci := `echo '{"type": "vault_policy", "identity": {"id": "ci"}, "import_id": "ci"}'`
	ops := `echo '{"type": "vault_policy", "identity": {"id": "ops"}, "import_id": "ops"}'`
	var stderr bytes.Buffer
	code := run(context.Background(), []string{"adopt", "-no-config", "-no-query", "-state", "empty.tfstate", "-plugin", ci, "-plugin", ops,
		"-out-dir", "out", "-adopt-root", "plugin1=b", "-adopt-root", "plugin2=a"}, io.Discard, &stderr)
	if code != exitOK {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	if got := read(t, "out/b/imports.tf"); !strings.Contains(got, `id = "ci"`) {
		t.Errorf("b/imports.tf:\n%s", got)
	}
	if _, err := os.Stat("out/a"); err == nil || !strings.Contains(stderr.String(), "1 objects already have import blocks") {
		t.Errorf("ops written again\n%s", stderr.String())
	}
}

// A root directory named like a plugin does not take that plugin's
// objects: only terraform query results carry a list block address.
func TestAdoptTarget(t *testing.T) {
	t.Chdir(t.TempDir())
	put(t, "plugin1/main.tf", "")
	put(t, "vault/main.tf", "")
	roots := map[string]bool{"plugin1": true}
	o := &options{}
	if got := adoptTarget(o, model.Resource{Source: "plugin1"}, roots); got != "" {
		t.Errorf("plugin1 result went to %s", got)
	}
	if got := adoptTarget(o, model.Resource{Source: "plugin1", ListAddress: "list.x.y"}, roots); got != canonDir("plugin1") {
		t.Errorf("root result: %q", got)
	}
	o.adoptRoots = map[string]string{"plugin1": "vault"}
	if got := adoptTarget(o, model.Resource{Source: "plugin1"}, roots); got != canonDir("vault") {
		t.Errorf("assigned: %q", got)
	}
}

// A link is resolved before .. applies: jump/../prod is team/prod.
func TestCanonDirLink(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	put(t, "team/sub/x", "")
	put(t, "team/prod/x", "")
	if err := os.Symlink(filepath.Join("team", "sub"), "jump"); err != nil {
		t.Skip(err)
	}
	cwd, _ := realCwd()
	if got, err := relRoot(cwd, "jump"+string(filepath.Separator)+".."+string(filepath.Separator)+"prod"); err != nil || got != filepath.Join("team", "prod") {
		t.Errorf("%q %v", got, err)
	}
}
