package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const doctorScanSchema = `{"provider_schemas":{"registry.terraform.io/cloudflare/cloudflare":{
 "resource_schemas":{"cloudflare_zone":{"block":{}},"cloudflare_thing":{"block":{}}},
 "data_source_schemas":{"cloudflare_zones":{"block":{"attributes":{"result":{"nested_type":{"attributes":{"id":{}}}}}}}}
}}}`

func doctorScanFixture(t *testing.T) string {
	t.Helper()
	bin := doctorFixture(t)
	script, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	script = bytes.Replace(script, []byte(`case "$1 $2" in`), []byte(`printf '%s\n' "$*" >> "$DOCTOR_SCAN_LOG"
case "$1 $2" in
 "init "*)
  [ -z "$TF_DATA_DIR$TF_WORKSPACE$TF_CLI_ARGS" ] || exit 98
  case " $* " in *" -plugin-dir="*) ;; *) exit 97 ;; esac
  if [ "$DOCTOR_INIT_FAIL" = 1 ]; then printf 'Error: provider installation failed' >&2; exit 1; fi
  exit 0 ;;`), 1)
	if err := os.WriteFile(bin, script, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".terraform.lock.hcl", []byte(`provider "registry.terraform.io/cloudflare/cloudflare" {
 version = "1.0.0"
 hashes = ["h1:test"]
}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(".terraform", "providers", "registry.terraform.io/cloudflare/cloudflare"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("terraform.tfstate", []byte(`{"version":4,"resources":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCTOR_SCAN_LOG", filepath.Join(filepath.Dir(bin), "commands"))
	t.Setenv("DOCTOR_SCHEMA_JSON", doctorScanSchema)
	return bin
}

func TestDoctorScanDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name         string
		args         []string
		prepare      func(*testing.T)
		code, blocks int
		plan, status string
	}{
		{"selected", nil, nil, 0, 1, "ok", "ready"},
		{"excluded", []string{"-exclude-types", "*"}, nil, 2, 0, "ok", "needs_configuration"},
		{"include matches nothing", []string{"-types", "aws_*"}, nil, 2, 0, "ok", "needs_configuration"},
		{"offline selected", []string{"-offline"}, nil, 0, 1, "ok", "unknown"},
		{"offline excluded", []string{"-offline", "-exclude-types", "*"}, nil, 2, 0, "ok", "unknown"},
		{"no pin", nil, func(t *testing.T) {
			if err := os.Remove(".terraform.lock.hcl"); err != nil {
				t.Fatal(err)
			}
		}, 2, 0, "skipped", "needs_configuration"},
		{"not installed", nil, func(t *testing.T) {
			if err := os.RemoveAll(".terraform/providers"); err != nil {
				t.Fatal(err)
			}
		}, 2, 0, "failed", "needs_configuration"},
		{"offline not installed", []string{"-offline"}, func(t *testing.T) {
			if err := os.RemoveAll(".terraform/providers"); err != nil {
				t.Fatal(err)
			}
		}, 2, 0, "skipped", "unknown"},
		{"offline unavailable with plugin", []string{"-offline", "-plugin", "must not execute"}, func(t *testing.T) {
			if err := os.RemoveAll(".terraform/providers"); err != nil {
				t.Fatal(err)
			}
		}, 0, 0, "skipped", "unknown"},
		{"init failure", nil, func(t *testing.T) { t.Setenv("DOCTOR_INIT_FAIL", "1") }, 2, 0, "failed", "needs_configuration"},
		{"malformed schema", nil, func(t *testing.T) { t.Setenv("DOCTOR_SCHEMA_JSON", "invalid") }, 1, 0, "failed", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := doctorScanFixture(t)
			if tc.prepare != nil {
				tc.prepare(t)
			}
			args := append([]string{"doctor", "-no-config", "-terraform", bin, "-format", "json", "-no-query", "-scan", "cloudflare", "-state-roots", "."}, tc.args...)
			var out, stderr bytes.Buffer
			code := run(context.Background(), args, &out, &stderr)
			if code != tc.code {
				t.Fatalf("exit %d want %d: %s\n%s", code, tc.code, &out, &stderr)
			}
			var d doctorReport
			if err := json.Unmarshal(out.Bytes(), &d); err != nil {
				t.Fatal(err)
			}
			if d.Status != tc.status {
				t.Fatalf("status: %s", &out)
			}
			var plan *doctorCheck
			for i := range d.Checks {
				if d.Checks[i].ID == "scan.cloudflare.plan" {
					plan = &d.Checks[i]
					break
				}
			}
			if plan == nil || plan.Status != tc.plan {
				t.Fatalf("plan: %+v, report %s", plan, &out)
			}
			if tc.plan == "ok" && (plan.Count == nil || *plan.Count != tc.blocks) {
				t.Fatalf("blocks: %+v", plan)
			}
			if tc.name == "selected" && !slices.ContainsFunc(d.Checks, func(c doctorCheck) bool {
				return c.ID == "scan.cloudflare.plan" && slices.Contains(c.Types, "cloudflare_thing") && c.Status == "skipped"
			}) {
				t.Fatalf("skipped: %+v", plan)
			}
			discovery := doctorFindCheck(t, d, "discovery", "")
			count := tc.blocks
			if slices.Contains(tc.args, "-plugin") {
				count++
			}
			if discovery.Count == nil || *discovery.Count != count {
				t.Fatalf("discovery: %+v", discovery)
			}
			if count == 0 && discovery.Status != "failed" {
				t.Fatalf("unconfirmed scan counted: %+v", discovery)
			}
			commands, err := os.ReadFile(os.Getenv("DOCTOR_SCAN_LOG"))
			if err != nil {
				t.Fatal(err)
			}
			for _, command := range strings.Split(string(commands), "\n") {
				if strings.HasPrefix(command, "apply ") || strings.HasPrefix(command, "plan ") || strings.HasPrefix(command, "query ") {
					t.Fatalf("cloud read: %s", command)
				}
				if (tc.plan == "skipped" || tc.name == "not installed") && (strings.HasPrefix(command, "init ") || strings.HasPrefix(command, "providers schema")) {
					t.Fatalf("unavailable provider was started: %s", command)
				}
			}
			if tc.plan == "failed" && !strings.Contains(plan.Remediation, "init") {
				t.Fatalf("remediation: %+v", plan)
			}
		})
	}
}

const doctorScopedScanSchema = `{"provider_schemas":{"registry.terraform.io/cloudflare/cloudflare":{
 "resource_schemas":{"cloudflare_zone":{"block":{}},"cloudflare_dns_record":{"block":{}},"cloudflare_ruleset":{"block":{}}},
 "data_source_schemas":{
  "cloudflare_accounts":{"block":{"attributes":{"result":{"nested_type":{"attributes":{"id":{}}}}}}},
  "cloudflare_zones":{"block":{"attributes":{"result":{"nested_type":{"attributes":{"id":{}}}}}}},
  "cloudflare_dns_records":{"block":{"attributes":{"zone_id":{"required":true},"result":{"nested_type":{"attributes":{"id":{}}}}}}},
  "cloudflare_rulesets":{"block":{"attributes":{"zone_id":{"optional":true},"account_id":{"optional":true},"result":{"nested_type":{"attributes":{"id":{}}}}}}}
 }
}}}`

func TestDoctorScanScopeDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		args                   []string
		stateType, stateAttr   string
		listings, blocks, code int
		skipped, offline       bool
	}{
		{"parent only", []string{"-types", "cloudflare_dns_record", "-scan-scope", "zone_id="}, "", "", 0, 1, 2, false, false},
		{"parent supports selected listing", []string{"-types", "cloudflare_dns_record"}, "", "", 1, 2, 0, false, false},
		{"hidden blocks", []string{"-types", "cloudflare_ruleset", "-scan-scope", "zone_id=", "-scan-scope", "account_id="}, "", "", 0, 4, 2, false, false},
		{"state without zones", []string{"-types", "cloudflare_dns_record", "-scan-scope", "state"}, "", "", 0, 1, 2, false, false},
		{"state with child zone_id", []string{"-types", "cloudflare_dns_record", "-scan-scope", "state"}, "cloudflare_dns_record", "zone_id", 1, 2, 0, false, false},
		{"state with parent id", []string{"-types", "cloudflare_dns_record", "-scan-scope", "state"}, "cloudflare_zone", "id", 1, 2, 0, false, false},
		{"explicit scope wins", []string{"-types", "cloudflare_dns_record", "-scan-scope", "state", "-scan-scope", "zone_id="}, "cloudflare_zone", "id", 0, 1, 2, false, false},
		{"offline state scope", []string{"-types", "cloudflare_dns_record", "-scan-scope", "state", "-offline"}, "cloudflare_zone", "id", 0, 0, 2, true, true},
		{"offline state scope with plugin", []string{"-types", "cloudflare_dns_record", "-scan-scope", "state", "-offline", "-plugin", "must not run"}, "", "", 0, 0, 0, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := doctorScanFixture(t)
			t.Setenv("DOCTOR_SCHEMA_JSON", doctorScopedScanSchema)
			raw := `{"version":4,"resources":[]}`
			if tc.stateType != "" {
				raw = `{"version":4,"resources":[{"mode":"managed","type":"` + tc.stateType + `","name":"a","instances":[{"attributes":{"` + tc.stateAttr + `":"zone-secret-value"}}]}]}`
			}
			t.Setenv("DOCTOR_STATE_JSON", raw)
			args := append([]string{"doctor", "-no-config", "-terraform", bin, "-format", "json", "-no-query", "-scan", "cloudflare", "-state-roots", "."}, tc.args...)
			var out bytes.Buffer
			if code := run(context.Background(), args, &out, &bytes.Buffer{}); code != tc.code {
				t.Fatalf("exit %d want %d: %s", code, tc.code, &out)
			}
			var d doctorReport
			if err := json.Unmarshal(out.Bytes(), &d); err != nil {
				t.Fatal(err)
			}
			i := slices.IndexFunc(d.Checks, func(c doctorCheck) bool { return c.ID == "scan.cloudflare.plan" })
			if i < 0 {
				t.Fatal("missing scan plan")
			}
			plan := d.Checks[i]
			if tc.skipped {
				if plan.Status != "skipped" || !strings.Contains(plan.Message, "state") {
					t.Fatalf("offline plan: %+v", plan)
				}
				commands, err := os.ReadFile(os.Getenv("DOCTOR_SCAN_LOG"))
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(commands), "init ") || strings.Contains(string(commands), "providers schema") {
					t.Fatalf("offline state plan ran: %s", commands)
				}
			} else if plan.Status != "ok" || plan.Count == nil || *plan.Count != tc.listings || plan.Blocks == nil || *plan.Blocks != tc.blocks {
				t.Fatalf("plan: %+v", plan)
			}
			discovery := doctorFindCheck(t, d, "discovery", "")
			count := tc.listings
			if slices.Contains(tc.args, "-plugin") {
				count++
			}
			if discovery.Count == nil || *discovery.Count != count || (discovery.Status == "ok") != (count > 0) {
				t.Fatalf("discovery: %+v", discovery)
			}
			if tc.offline && d.Status != "unknown" {
				t.Fatalf("offline result: %+v", d)
			}
			if strings.Contains(out.String(), "zone-secret-value") {
				t.Fatalf("state attribute leaked: %s", &out)
			}
		})
	}
}
