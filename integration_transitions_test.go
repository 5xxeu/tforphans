//go:build integration

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestIntegrationBaselineTransitions: orphans of a baseline that are
// imported or ignored afterwards are resolved as became_managed and
// ignored, since the same listing ran again unchanged and saw them.
func TestIntegrationBaselineTransitions(t *testing.T) {
	data, root := setup(t)
	write(t, filepath.Join(root, "main.tf"), `
terraform {
  required_providers {
    fakeloc = { source = "example.com/fake/fake" }
  }
}
resource "fake_thing" "a" {
  provider = fakeloc
  name     = "managed-a"
}
`)
	terraform(t, root, "apply", "-auto-approve", "-no-color")
	for _, n := range []string{"orphan-1", "orphan-2"} {
		write(t, filepath.Join(data, "thing", n), "")
	}
	t.Chdir(root)
	args := []string{"-no-config", "-types", "fake_thing", "-format", "json"}
	code, baseline, stderr := runTool(args...)
	if code != exitOrphans {
		t.Fatalf("baseline run: exit %d\n%s", code, stderr)
	}
	write(t, "baseline.json", baseline)

	// Import orphan-1, ignore orphan-2.
	code, imports, stderr := runTool("adopt", "-no-config", "-types", "fake_thing")
	if code != exitOK {
		t.Fatalf("adopt: exit %d\n%s", code, stderr)
	}
	var keep []string
	for _, block := range strings.SplitAfter(imports, "}\n\n") {
		if strings.Contains(block, "orphan_1") {
			keep = append(keep, block)
		}
	}
	write(t, "imports.tf", strings.Join(keep, ""))
	terraform(t, root, "plan", "-generate-config-out=generated.tf", "-no-color")
	if before116(t) {
		imported, _ := os.ReadFile("imports.tf")
		write(t, "imports.tf", regexp.MustCompile(`(?m)^  provider = .*\n`).ReplaceAllString(string(imported), ""))
		generated, _ := os.ReadFile("generated.tf")
		write(t, "generated.tf", regexp.MustCompile(`(?m)^(resource "fake_thing" .*\{\n)`).ReplaceAllString(string(generated), "${1}  provider = fakeloc\n"))
	}
	terraform(t, root, "apply", "-auto-approve", "-no-color")
	write(t, ".tforphans-ignore", "fake_thing:orphan-2\n")

	code, stdout, stderr := runTool(append(args, "-baseline", "baseline.json")...)
	if code != exitOK || !strings.Contains(stderr, "2 resolved (1 became managed, 1 ignored)") ||
		!regexp.MustCompile(`"transition": "became_managed",\s+"state_address": "fake_thing\.orphan_1"`).MatchString(stdout) ||
		!strings.Contains(stdout, `"rule": ".tforphans-ignore:1: fake_thing:orphan-2"`) {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
}
