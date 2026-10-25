package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestDoctorURLQueryRedaction(t *testing.T) {
	for _, query := range []string{`filter=a,b&token=SECRET`, `filter=a,"b'&token=SECRET`, `filter=a%2Cb&token=SECRET`, `filter=a,b&token=SECRET more text`, `filter=a&token=SECREThttps://other/url`} {
		url := "https://user:password@backend/state?" + query
		for _, args := range [][]string{{"-state", url}, {"-state=" + url}, {url}, {"-arg", "endpoint=" + url}, {"-arg=endpoint=" + url}, {"prefix " + url}} {
			command := doctorNextArgs(t, args, nil)
			for _, format := range []string{"text", "json"} {
				var out bytes.Buffer
				if err := (doctorReport{NextCommand: command}).write(&out, format); err != nil {
					t.Fatal(err)
				}
				for _, secret := range []string{"SECRET", "password", "token=", "filter="} {
					if strings.Contains(out.String(), secret) {
						t.Fatalf("%s %q leaks %q: %s", format, args, secret, &out)
					}
				}
				if !strings.Contains(out.String(), "https://backend/state?REDACTED") {
					t.Fatalf("%s %q: %s", format, args, &out)
				}
			}
		}
	}
	// Queries between embedded URLs must be redacted independently.
	got := redactArgURLs(`prefix=https://one/state?filter=a,b&token=SECRET,https://user:password@two/state?filter="a,b"&token=SECRET`)
	if strings.Contains(got, "SECRET") || strings.Contains(got, "password") || strings.Count(got, "REDACTED") != 2 {
		t.Fatal(got)
	}
}

func TestDoctorUserListCoverage(t *testing.T) {
	for _, tc := range []struct {
		name, config, provider, typ string
		code                        int
		covered                     bool
	}{
		{"explicit default", "", "aws", "aws_thing", 0, true},
		{"implicit default", "", "", "aws_thing", 0, true},
		{"other type", "", "aws", "aws_other", 2, false},
		{"other provider", "", "other", "aws_thing", 2, false},
		{"other alias", "\nprovider \"aws\" {\n alias = \"prod\"\n}\n", "aws", "aws_thing", 2, false},
		{"alias only", "\nprovider \"aws\" {\n alias = \"prod\"\n}\n", "aws.prod", "aws_thing", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := doctorFixture(t)
			config := "terraform {\n required_providers {\n aws = { source = \"hashicorp/aws\" }\n }\n}\n" + tc.config
			if err := os.WriteFile("main.tf", []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			block := "list \"" + tc.typ + "\" \"custom\" {\n config { region = \"eu\" }\n"
			if tc.provider != "" {
				block += " provider = " + tc.provider + "\n"
			}
			block += "}\n"
			if err := os.WriteFile("custom.tfquery.hcl", []byte(block), 0600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if code := run(context.Background(), []string{"doctor", "-no-config", "-terraform", bin, "-format", "json"}, &out, io.Discard); code != tc.code {
				t.Fatalf("exit %d want %d: %s", code, tc.code, &out)
			}
			var d doctorReport
			if err := json.Unmarshal(out.Bytes(), &d); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, c := range d.Checks {
				if c.ID == "query.arguments" && strings.Contains(c.Message, "covered by user list block list."+tc.typ+".custom") {
					found = true
					if c.Status != "skipped" || c.Remediation != "" {
						t.Fatalf("covered skip: %+v", c)
					}
				}
			}
			if found != tc.covered {
				t.Fatalf("coverage=%t want %t: %s", found, tc.covered, &out)
			}
			if tc.covered && slices.Contains(d.NextCommand, "-arg") {
				t.Fatalf("unneeded argument: %q", d.NextCommand)
			}
		})
	}
}

func TestDoctorStateOnlyWithoutInitialization(t *testing.T) {
	bin := doctorFixture(t)
	if err := os.Remove(".terraform"); err != nil {
		t.Fatal(err)
	}
	// A local backend without providers can serve its stored state without init.
	if err := os.WriteFile("main.tf", []byte("terraform {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCTOR_STATE_JSON", `{"version":4,"resources":[]}`)
	var out bytes.Buffer
	args := []string{"doctor", "-no-config", "-terraform", bin, "-no-query", "-plugin", "must not run", "-format", "json", "."}
	if code := run(context.Background(), args, &out, io.Discard); code != 0 {
		t.Fatalf("exit %d: %s", code, &out)
	}
	var d doctorReport
	if err := json.Unmarshal(out.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	c := doctorFindCheck(t, d, "root.initialized", ".")
	if c.Status != "warning" || d.Status != "ready" {
		t.Fatalf("initialization: %+v, status %s", c, d.Status)
	}
	if c := doctorFindCheck(t, d, "states.read", ""); c.Status != "ok" {
		t.Fatal(c)
	}
}

// TestDoctorTextGroupsProblems: without -v only problems and warnings are
// listed, problems in dependency order, the same check in several roots
// once with <root> in its fix; the next command checks again.
func TestDoctorTextGroupsProblems(t *testing.T) {
	d := doctorReport{Status: "needs_configuration", NextCommand: []string{"tforphans", "-state-roots", "stacks"}}
	d.add("config", "ok", "command line and config parsed", "", "")
	d.add("discovery", "failed", "0 list queries, 0 scan blocks, 0 plugins selected", "", "Add -scan cloudflare")
	d.add("root.initialized", "warning", "Terraform data directory is missing: .terraform", "stacks/a", "terraform -chdir="+shellQuote("stacks/a")+" init")
	d.add("root.initialized", "warning", "Terraform data directory is missing: .terraform", "stacks/b", "terraform -chdir="+shellQuote("stacks/b")+" init")
	d.add("states.read", "failed", "0 of 1 states read", "", "")
	var b bytes.Buffer
	if err := d.write(&b, "text"); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"Result: BLOCKED (2 problems, 2 warnings; 1 passed, 0 skipped; -v lists every check)",
		"[2 roots: stacks/a, stacks/b]\n      Fix: terraform -chdir=<root> init\n",
		"then check again:\n  tforphans doctor -state-roots stacks\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if states, discovery := strings.Index(out, "states.read"), strings.Index(out, "discovery"); states < 0 || discovery < states {
		t.Errorf("problems not in dependency order:\n%s", out)
	}
	if strings.Contains(out, "OK ") || strings.Count(out, "root.initialized") != 1 {
		t.Errorf("passed or duplicate checks listed:\n%s", out)
	}
	d.recheck = []string{"-check-auth"}
	b.Reset()
	if err := d.write(&b, "text"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "  tforphans doctor -check-auth -state-roots stacks\n") {
		t.Errorf("recheck lost -check-auth:\n%s", &b)
	}
	d.verbose = true
	b.Reset()
	if err := d.write(&b, "text"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "OK    config") || strings.Count(b.String(), "root.initialized") != 2 {
		t.Errorf("verbose:\n%s", &b)
	}
}
