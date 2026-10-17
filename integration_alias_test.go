//go:build integration

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrationProviderAliases: the default configuration looks at dev,
// an alias at prod. Both are listed, each orphan's import block names the
// configuration it was found with, and coverage is complete.
func TestIntegrationProviderAliases(t *testing.T) {
	data, root := setup(t)
	prod := filepath.Join(filepath.Dir(data), "prod")
	if err := os.MkdirAll(filepath.Join(prod, "thing"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "main.tf"), `
terraform {
  required_providers {
    fakeloc = { source = "example.com/fake/fake" }
  }
}
provider "fakeloc" {}
provider "fakeloc" {
  alias = "prod"
  dir   = "`+prod+`"
}
resource "fake_thing" "a" {
  provider = fakeloc
  name     = "managed-dev"
}
resource "fake_thing" "b" {
  provider = fakeloc.prod
  name     = "managed-prod"
}
`)
	terraform(t, root, "apply", "-auto-approve", "-no-color")
	write(t, filepath.Join(data, "thing", "orphan-dev"), "")
	write(t, filepath.Join(prod, "thing", "orphan-prod"), "")
	t.Chdir(root)

	code, stdout, stderr := runTool("adopt", "-no-config", "-types", "fake_thing")
	if code != exitOK || !strings.Contains(stdout, "to = fake_thing.orphan_dev\n  provider = fakeloc\n") || !strings.Contains(stdout, "to = fake_thing.orphan_prod\n  provider = fakeloc.prod\n") {
		t.Fatalf("adopt: exit %d\n%s\n%s", code, stdout, stderr)
	}
	code, _, stderr = runTool("-no-config", "-types", "fake_thing", "-strict")
	if code != exitOrphans || !strings.Contains(stderr, "4 live, 2 managed, 0 uncertain, 0 ignored, 2 unmanaged") || !strings.Contains(stderr, "coverage: complete") {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	// -no-aliases: prod is not checked, and coverage says so.
	if code, stdout, stderr := runTool("-no-config", "-types", "fake_thing", "-strict", "-no-aliases"); code != exitError || strings.Contains(stdout, "orphan-prod") || !strings.Contains(stderr, "coverage: INCOMPLETE") {
		t.Errorf("-no-aliases: exit %d\n%s\n%s", code, stdout, stderr)
	}
	if code, stdout, _ := runTool("-no-config", "-types", "fake_thing", "-print-query"); code != exitOK || !strings.Contains(stdout, `list "fake_thing" "tfo_fakeloc_prod_fake_thing"`) || !strings.Contains(stdout, "provider = fakeloc.prod") {
		t.Errorf("-print-query: exit %d\n%s", code, stdout)
	}
}

// TestIntegrationTwoLocalNames: two local names require one provider
// source; an alias declared under the second name (prod) is listed
// through that name. With aliases only, the empty default is not listed.
func TestIntegrationTwoLocalNames(t *testing.T) {
	data, root := setup(t)
	prod := filepath.Join(filepath.Dir(data), "prod")
	if err := os.MkdirAll(filepath.Join(prod, "thing"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "main.tf"), `
terraform {
  required_providers {
    fakeloc = { source = "example.com/fake/fake" }
    fakeeu  = { source = "example.com/fake/fake" }
  }
}
provider "fakeloc" {
  alias = "prod"
  dir   = "`+prod+`"
}
`)
	write(t, filepath.Join(data, "thing", "orphan-dev"), "")
	write(t, filepath.Join(prod, "thing", "orphan-prod"), "")
	t.Chdir(root)
	code, stdout, stderr := runTool("adopt", "-no-config", "-types", "fake_thing")
	if code != exitOK || strings.Contains(stdout, "orphan_dev") || !strings.Contains(stdout, "to = fake_thing.orphan_prod\n  provider = fakeloc.prod\n") {
		t.Fatalf("adopt: exit %d\n%s\n%s", code, stdout, stderr)
	}
}
