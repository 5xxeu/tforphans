//go:build integration

package main

import (
	"context"
	"encoding/json"
	"github.com/5xxeu/tforphans/internal/tf"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIntegrationDoctor(t *testing.T) {
	_, root := setup(t)
	write(t, filepath.Join(root, "main.tf"), `
terraform {
 required_providers {
  fake = { source = "example.com/fake/fake" }
 }
}
`)
	// dev_overrides supplies the installed provider without network init.
	if err := os.Mkdir(filepath.Join(root, ".terraform"), 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "terraform.tfstate"), `{"version":4,"serial":1,"lineage":"doctor","resources":[]}`)
	t.Chdir(root)
	if _, err := (tf.Runner{Bin: "terraform", Dir: root}).ProvidersSchema(context.Background()); err != nil {
		t.Fatalf("provider schema prerequisite: %v", err)
	}
	for _, tc := range []struct {
		name   string
		args   []string
		code   int
		status string
	}{
		{"ready", []string{"-types", "fake_thing"}, 0, "ready"},
		{"required argument", []string{"-types", "fake_zoned"}, 2, "needs_configuration"},
		{"supplied argument", []string{"-types", "fake_zoned", "-arg", "zone=a"}, 0, "ready"},
		{"unread state", []string{"-types", "fake_thing", "-state", "missing.tfstate"}, 2, "needs_configuration"},
		{"offline", []string{"-types", "fake_thing", "-offline", "-check-auth", "-state", "missing.tfstate"}, 0, "unknown"},
		{"uninitialized", []string{"-types", "fake_thing"}, 2, "needs_configuration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "uninitialized" {
				if err := os.Remove(filepath.Join(root, ".terraform")); err != nil {
					t.Fatal(err)
				}
			}
			code, out, stderr := runTool(append([]string{"doctor", "-no-config", "-format", "json"}, tc.args...)...)
			if code != tc.code {
				t.Fatalf("exit %d want %d: %s\n%s", code, tc.code, out, stderr)
			}
			var d doctorReport
			if err := json.Unmarshal([]byte(out), &d); err != nil {
				t.Fatal(err)
			}
			if d.Status != tc.status {
				t.Fatalf("status %s want %s", d.Status, tc.status)
			}
			if tc.name == "required argument" && !strings.Contains(out, "-arg zone=...") {
				t.Fatal(out)
			}
			if left, _ := filepath.Glob("zz_tforphans_*"); len(left) > 0 {
				t.Fatalf("temporary files left: %v", left)
			}
		})
	}
}
