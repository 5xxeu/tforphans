//go:build integration

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrationAdoptOutDir: what terraform query finds in a root goes to
// that root's file under -out-dir, next to a plugin assigned to it, and
// nothing else is written.
func TestIntegrationAdoptOutDir(t *testing.T) {
	data, root := setup(t)
	write(t, filepath.Join(root, "main.tf"), `
terraform {
  required_providers {
    fake = { source = "example.com/fake/fake" }
  }
}
resource "fake_thing" "a" {
  name = "managed"
}
`)
	terraform(t, root, "apply", "-auto-approve", "-no-color")
	write(t, filepath.Join(data, "thing", "orphan"), "")
	parent := filepath.Dir(root)
	t.Chdir(parent)

	plugin := `echo '{"type": "vault_policy", "identity": {"id": "ci"}, "import_id": "ci"}'`
	code, stdout, stderr := runTool("adopt", "-no-config", "-types", "fake_thing,vault_policy", "-plugin", plugin, "-out-dir", "out", "-adopt-root", "plugin1=root", "root")
	if code != exitOK || stdout != "" {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	got, err := os.ReadFile(filepath.Join("out", "root", "imports.tf"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Found by plugin1\n", "# Found by root\n", "to = fake_thing.orphan\n", "to = vault_policy.id_ci\n"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("root/imports.tf lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(string(got), "managed") {
		t.Errorf("managed object written:\n%s", got)
	}
	entries, _ := os.ReadDir("out")
	if len(entries) != 1 {
		t.Errorf("out holds %v", entries)
	}
}

// TestIntegrationAdoptOutDirScan: -scan objects assigned to a root use that
// root's local name of the provider, and a root that does not declare it
// says so in the block.
func TestIntegrationAdoptOutDirScan(t *testing.T) {
	root := scanSetup(t)
	parent := filepath.Dir(root)
	if err := os.MkdirAll(filepath.Join(parent, "named"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(parent, "named", "main.tf"), "terraform {\n  required_providers {\n    fk = { source = \"example.com/fake/fake\" }\n  }\n}\n")
	t.Chdir(parent)

	if code, stdout, stderr := runTool("adopt", "-no-config", "-no-query", "-scan", "fake", "-allow-unpinned", "-out-dir", "out", "-adopt-root", "fake=named"); code != exitOK || stdout != "" {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	got, err := os.ReadFile(filepath.Join("out", "named", "imports.tf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "  provider = fk\n") || strings.Contains(string(got), "does not declare") {
		t.Errorf("named/imports.tf:\n%s", got)
	}

	if code, stdout, stderr := runTool("adopt", "-no-config", "-no-query", "-scan", "fake", "-allow-unpinned", "-out-dir", "out2", "-adopt-root", "fake=root"); code != exitOK || stdout != "" {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	got, err = os.ReadFile(filepath.Join("out2", "root", "imports.tf"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "\n  provider =") || !strings.Contains(string(got), "the root module does not declare example.com/fake/fake") {
		t.Errorf("root/imports.tf:\n%s", got)
	}
}
