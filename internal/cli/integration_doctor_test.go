//go:build integration

package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/tf"
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
		{"uninitialized", []string{"-types", "fake_thing"}, 0, "ready"},
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
			if tc.name == "uninitialized" {
				if c := doctorFindCheck(t, d, "root.initialized", "."); c.Status != "warning" {
					t.Fatalf("missing initialization warning: %+v", c)
				}
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

func TestIntegrationDoctorScanPlan(t *testing.T) {
	root := scanSetup(t)
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	plugins := filepath.Join(root, ".terraform", "providers")
	installed := filepath.Join(plugins, fakeAdapter.Provider, "1.0.0", runtime.GOOS+"_"+runtime.GOARCH)
	if err := os.MkdirAll(installed, 0700); err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(filepath.Join(filepath.Dir(root), "bin", "terraform-provider-fake"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installed, "terraform-provider-fake_v1.0.0"), binary, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "main.tf"), `terraform {
 required_providers {
  fake = { source = "example.com/fake/fake", version = "1.0.0" }
 }
}`)
	write(t, filepath.Join(root, "terraform.tfstate"), `{"version":4,"serial":1,"lineage":"doctor-scan","resources":[]}`)
	// Like a real root: the provider comes from .terraform, not dev_overrides,
	// which terraform 1.15 rejects with -lockfile=readonly.
	rc := filepath.Join(t.TempDir(), "tf.rc")
	write(t, rc, "")
	t.Setenv("TF_CLI_CONFIG_FILE", rc)
	t.Chdir(root)
	r := tf.Runner{Bin: "terraform", Dir: root}
	if _, _, err := r.Exec(context.Background(), "providers", "lock", "-fs-mirror="+plugins); err != nil {
		t.Fatalf("local lock prerequisite: %v", err)
	}
	if _, err := r.ProvidersSchema(context.Background()); err != nil {
		t.Fatalf("provider schema prerequisite: %v", err)
	}
	// Only schema inspection and backend reads are allowed.
	terraform, err := exec.LookPath("terraform")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(t.TempDir(), "terraform")
	write(t, wrapper, "#!/bin/sh\ncase \"$1\" in plan|apply|query) exit 99 ;; esac\nexec "+shellQuote(terraform)+" \"$@\"\n")
	if err := os.Chmod(wrapper, 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name         string
		args         []string
		blocks, code int
		status       string
	}{
		{"selected", nil, 2, 0, "ready"},
		{"all excluded", []string{"-exclude-types", "*"}, 0, 2, "needs_configuration"},
		{"parent only", []string{"-types", "fake_record", "-scan-scope", "zone_id="}, 0, 2, "needs_configuration"},
		{"state without zones", []string{"-types", "fake_record", "-scan-scope", "state"}, 0, 2, "needs_configuration"},
		{"parent with listing", []string{"-types", "fake_record"}, 1, 0, "ready"},
		{"no matching types", []string{"-types", "cloudflare_*"}, 0, 2, "needs_configuration"},
		{"offline selected", []string{"-offline"}, 2, 0, "unknown"},
		{"offline excluded", []string{"-offline", "-exclude-types", "*"}, 0, 2, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"doctor", "-no-config", "-terraform", wrapper, "-format", "json", "-no-query", "-scan", "fake", "-state-roots", root}, tc.args...)
			code, out, stderr := runTool(args...)
			if code != tc.code {
				t.Fatalf("exit %d want %d: %s\n%s", code, tc.code, out, stderr)
			}
			var d doctorReport
			if err := json.Unmarshal([]byte(out), &d); err != nil {
				t.Fatal(err)
			}
			if d.Status != tc.status {
				t.Fatalf("status: %s", out)
			}
			plan := doctorFindCheck(t, d, "scan.fake.plan", root)
			if plan.Status != "ok" || plan.Count == nil || *plan.Count != tc.blocks {
				t.Fatalf("plan: %+v", plan)
			}
			discovery := doctorFindCheck(t, d, "discovery", "")
			if discovery.Count == nil || *discovery.Count != tc.blocks || tc.blocks == 0 && discovery.Status != "failed" {
				t.Fatalf("discovery: %+v", discovery)
			}
		})
	}
}

func TestIntegrationDoctorStateOnlyUninitialized(t *testing.T) {
	if _, err := exec.LookPath("terraform"); err != nil {
		t.Skip("terraform not in PATH")
	}
	root := t.TempDir()
	t.Chdir(root)
	write(t, "main.tf", "terraform {}\n")
	write(t, "terraform.tfstate", `{"version":4,"serial":1,"lineage":"doctor-local","resources":[]}`)
	code, out, stderr := runTool("doctor", "-no-config", "-no-query", "-plugin", "echo unused", "-format", "json", ".")
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, out, stderr)
	}
	var d doctorReport
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		t.Fatal(err)
	}
	if d.Status != "ready" {
		t.Fatal(out)
	}
	if c := doctorFindCheck(t, d, "root.initialized", "."); c.Status != "warning" {
		t.Fatal(c)
	}
	if _, err := os.Stat(".terraform"); !os.IsNotExist(err) {
		t.Fatalf("Terraform data directory was created: %v", err)
	}
}

func TestIntegrationDoctorStateOnlyWithoutConfiguration(t *testing.T) {
	if _, err := exec.LookPath("terraform"); err != nil {
		t.Skip("terraform not in PATH")
	}
	t.Chdir(t.TempDir())
	write(t, "terraform.tfstate", `{"version":4,"serial":1,"lineage":"doctor-state-only","resources":[]}`)
	code, out, stderr := runTool("doctor", "-no-config", "-no-query", "-plugin", "echo unused", "-format", "json", ".")
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, out, stderr)
	}
	var d doctorReport
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		t.Fatal(err)
	}
	if d.Status != "ready" {
		t.Fatal(out)
	}
	if c := doctorFindCheck(t, d, "root.config", "."); c.Status != "warning" {
		t.Fatal(c)
	}
	if c := doctorFindCheck(t, d, "states.read", ""); c.Status != "ok" {
		t.Fatal(c)
	}
}
