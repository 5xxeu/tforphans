package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorAuthenticationScope(t *testing.T) {
	for _, tc := range []struct {
		name, include string
		code          int
		azure         bool
	}{
		{"selected AWS", "aws_*", 0, false},
		{"all providers", "*", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := doctorFixture(t)
			config := `terraform {
 required_providers {
  aws = { source = "hashicorp/aws" }
  azurerm = { source = "hashicorp/azurerm" }
 }
}
provider "aws" {}
provider "azurerm" {}
`
			if err := os.WriteFile("main.tf", []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("DOCTOR_SCHEMA_JSON", doctorTestSchema(t, map[string]map[string][]string{
				"registry.terraform.io/hashicorp/aws":     {"aws_thing": nil},
				"registry.terraform.io/hashicorp/azurerm": {"azurerm_thing": nil},
			}))
			t.Setenv("DOCTOR_IDENTITY_JSON", `{"prior_state":{"values":{"root_module":{"resources":[{"mode":"data","address":"data.aws_caller_identity.tfo_id_0_0","values":{"account_id":"123"}},{"mode":"data","address":"data.aws_region.tfo_id_0_1","values":{"region":"eu"}}]}}}}`)
			log := filepath.Join(filepath.Dir(bin), "azure-auth")
			t.Setenv("DOCTOR_AZURE_AUTH_LOG", log)
			script, err := os.ReadFile(bin)
			if err != nil {
				t.Fatal(err)
			}
			script = bytes.Replace(script, []byte(` "plan "*)`), []byte(` "plan "*)
  if grep -F 'provider = azurerm' zz_tforphans_*_identity.tf >/dev/null; then
   printf 'attempted' >> "$DOCTOR_AZURE_AUTH_LOG"
   printf 'Azure credentials unavailable' >&2
   exit 1
  fi`), 1)
			if err := os.WriteFile(bin, script, 0700); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			args := []string{"doctor", "-no-config", "-terraform", bin, "-format", "json", "-types", tc.include, "-check-auth"}
			if code := run(context.Background(), args, &out, io.Discard); code != tc.code {
				t.Fatalf("exit %d want %d: %s", code, tc.code, &out)
			}
			var d doctorReport
			if err := json.Unmarshal(out.Bytes(), &d); err != nil {
				t.Fatal(err)
			}
			if c := doctorFindCheck(t, d, "auth.aws", "."); c.Status != "ok" {
				t.Fatal(c)
			}
			if tc.azure {
				if c := doctorFindCheck(t, d, "auth.azurerm", "."); c.Status != "failed" {
					t.Fatal(c)
				}
				if _, err := os.Stat(log); err != nil {
					t.Fatal(err)
				}
			} else {
				for _, c := range d.Checks {
					if c.ID == "auth.azurerm" {
						t.Fatalf("excluded provider checked: %+v", c)
					}
				}
				if _, err := os.Stat(log); !os.IsNotExist(err) {
					t.Fatalf("excluded provider identity plan ran: %v", err)
				}
				if d.Status != "ready" {
					t.Fatalf("status %s: %s", d.Status, &out)
				}
			}
		})
	}
}

func TestDoctorUserListAuthentication(t *testing.T) {
	for _, tc := range []struct {
		name, provider                      string
		alias, noAliases, failed, checkAuth bool
		blocks, plans, code                 int
		status                              string
	}{
		{"user alias despite no-aliases", "aws.prod", true, true, false, true, 1, 1, 0, "ok"},
		{"user alias denied", "aws.prod", true, true, true, true, 1, 1, 2, "failed"},
		{"alias without user lists excluded", "aws.prod", true, true, true, true, 0, 0, 0, ""},
		{"alias already selected", "aws.prod", true, false, false, true, 2, 1, 0, "ok"},
		{"explicit default deduplicated", "aws", false, true, false, true, 2, 1, 0, "ok"},
		{"implicit default deduplicated", "", false, true, false, true, 2, 1, 0, "ok"},
		{"auth still opt-in", "aws.prod", true, true, false, false, 1, 0, 0, "skipped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := doctorFixture(t)
			config := "terraform {\n required_providers {\n aws = { source = \"hashicorp/aws\" }\n }\n}\n"
			if tc.alias {
				config += "provider \"aws\" {\n alias = \"prod\"\n}\n"
			}
			if err := os.WriteFile("main.tf", []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			var lists strings.Builder
			for i := 0; i < tc.blocks; i++ {
				fmt.Fprintf(&lists, "list \"aws_thing\" \"custom%d\" {\n", i)
				if tc.provider != "" {
					fmt.Fprintf(&lists, " provider = %s\n", tc.provider)
				}
				lists.WriteString(" config { region = \"eu\" }\n}\n")
			}
			if tc.blocks > 0 {
				if err := os.WriteFile("custom.tfquery.hcl", []byte(lists.String()), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ref := tc.provider
			if ref == "" {
				ref = "aws"
			}
			t.Setenv("DOCTOR_EXPECT_REF", ref)
			log := filepath.Join(filepath.Dir(bin), "commands")
			t.Setenv("DOCTOR_AUTH_LOG", log)
			script, err := os.ReadFile(bin)
			if err != nil {
				t.Fatal(err)
			}
			script = bytes.Replace(script, []byte(`case "$1 $2" in`), []byte(`printf '%s\n' "$*" >> "$DOCTOR_AUTH_LOG"
case "$1 $2" in`), 1)
			script = bytes.Replace(script, []byte(` "plan "*)`), []byte(` "plan "*)
  grep -F "provider = $DOCTOR_EXPECT_REF" zz_tforphans_*_identity.tf >/dev/null || exit 98`), 1)
			if err := os.WriteFile(bin, script, 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("DOCTOR_IDENTITY_JSON", `{"prior_state":{"values":{"root_module":{"resources":[{"mode":"data","address":"data.aws_caller_identity.tfo_id_0_0","values":{"account_id":"123"}},{"mode":"data","address":"data.aws_region.tfo_id_0_1","values":{"region":"eu"}}]}}}}`)
			if tc.failed {
				t.Setenv("DOCTOR_IDENTITY_PLAN_FAIL", "1")
			}
			args := []string{"doctor", "-no-config", "-terraform", bin, "-format", "json", "-arg", "region=eu", "-plugin", "must not run"}
			if tc.noAliases {
				args = append(args, "-no-aliases")
			}
			if tc.checkAuth {
				args = append(args, "-check-auth")
			}
			var out bytes.Buffer
			if code := run(context.Background(), args, &out, io.Discard); code != tc.code {
				t.Fatalf("exit %d want %d: %s", code, tc.code, &out)
			}
			var d doctorReport
			if err := json.Unmarshal(out.Bytes(), &d); err != nil {
				t.Fatal(err)
			}
			checks := 0
			for _, c := range d.Checks {
				if c.ID == "auth."+ref {
					checks++
					if c.Status != tc.status {
						t.Fatalf("auth: %+v", c)
					}
				}
			}
			want := 1
			if tc.status == "" {
				want = 0
				if c := doctorFindCheck(t, d, "auth.providers", "."); c.Status != "skipped" {
					t.Fatal(c)
				}
			}
			if checks != want {
				t.Fatalf("%d auth checks want %d: %s", checks, want, &out)
			}
			commands, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			plans := 0
			for _, cmd := range strings.Split(string(commands), "\n") {
				if strings.HasPrefix(cmd, "plan ") {
					plans++
				}
			}
			if plans != tc.plans {
				t.Fatalf("%d plans want %d: %s", plans, tc.plans, commands)
			}
		})
	}
}
