package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"unicode"

	"github.com/5xxeu/tforphans/internal/report"
)

func TestDoctorDiscovery(t *testing.T) {
	bin := doctorFixture(t)
	t.Setenv("DOCTOR_STATE_JSON", `{"version":4,"resources":[]}`)
	for _, tc := range []struct {
		name, source string
		types        map[string][]string
		args         []string
		userList     bool
		count, code  int
		remediation  string
	}{
		{"cloudflare has no list resources", "cloudflare/cloudflare", nil, nil, false, 0, 2, "-scan cloudflare"},
		{"provider without adapter", "hashicorp/random", nil, nil, false, 0, 2, "-plugin COMMAND"},
		{"all excluded", "hashicorp/aws", map[string][]string{"aws_thing": nil}, []string{"-exclude-types", "aws_*"}, false, 0, 2, "-exclude-types"},
		{"no included types", "hashicorp/aws", map[string][]string{"aws_thing": nil}, []string{"-types", "google_*"}, false, 0, 2, "-types"},
		{"generated query", "hashicorp/aws", map[string][]string{"aws_thing": nil}, nil, false, 1, 0, ""},
		{"filtered generated query", "hashicorp/aws", map[string][]string{"aws_thing": nil, "aws_other": nil}, []string{"-types", "aws_*", "-exclude-types", "aws_other"}, false, 1, 0, ""},
		{"user list despite exclusion", "hashicorp/aws", map[string][]string{"aws_thing": nil}, []string{"-exclude-types", "aws_*"}, true, 1, 0, ""},
		{"user list despite inclusion", "hashicorp/aws", map[string][]string{"aws_thing": nil}, []string{"-types", "google_*"}, true, 1, 0, ""},
		{"user and generated lists", "hashicorp/aws", map[string][]string{"aws_thing": nil}, nil, true, 2, 0, ""},
		{"no query with plugin", "hashicorp/aws", map[string][]string{"aws_thing": nil}, []string{"-no-query", "-plugin", "must not execute"}, false, 1, 0, ""},
		{"no query with scan", "cloudflare/cloudflare", nil, []string{"-no-query", "-scan", "cloudflare"}, false, 1, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fmt.Sprintf("terraform {\n required_providers {\n provider = { source = %q }\n }\n}\n", tc.source)
			if err := os.WriteFile("main.tf", []byte(cfg), 0600); err != nil {
				t.Fatal(err)
			}
			source := "registry.terraform.io/" + tc.source
			t.Setenv("DOCTOR_SCHEMA_JSON", doctorTestSchema(t, map[string]map[string][]string{source: tc.types}))
			terraformBin := bin
			if tc.name == "no query with scan" {
				terraformBin = doctorScanFixture(t)
			}
			lock := fmt.Sprintf("provider %q {\n version = \"1.0.0\"\n hashes = [\"h1:test\"]\n}\n", source)
			if err := os.WriteFile(".terraform.lock.hcl", []byte(lock), 0600); err != nil {
				t.Fatal(err)
			}
			if tc.userList {
				block := "list \"aws_thing\" \"custom\" {\n provider = provider\n}\n"
				if err := os.WriteFile("custom.tfquery.hcl", []byte(block), 0600); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Remove("custom.tfquery.hcl") })
			}
			args := append([]string{"doctor", "-no-config", "-terraform", terraformBin, "-format", "json", "."}, tc.args...)
			var out, stderr bytes.Buffer
			code := run(context.Background(), args, &out, &stderr)
			if code != tc.code {
				t.Fatalf("exit %d want %d: %s %s", code, tc.code, &out, &stderr)
			}
			var d doctorReport
			if err := json.Unmarshal(out.Bytes(), &d); err != nil {
				t.Fatal(err)
			}
			c := doctorFindCheck(t, d, "discovery", "")
			want := "ok"
			if tc.count == 0 {
				want = "failed"
			}
			if c.Status != want || c.Count == nil || *c.Count != tc.count || !strings.Contains(c.Remediation, tc.remediation) {
				t.Fatalf("discovery: %+v", c)
			}
			if tc.name == "cloudflare has no list resources" && !strings.Contains(strings.Join(d.NextCommand, " "), "-scan cloudflare") {
				t.Fatal(d.NextCommand)
			}
			if tc.count == 0 {
				out.Reset()
				stderr.Reset()
				auditArgs := append([]string{"-no-config", "-terraform", terraformBin, "-format", "json", "."}, tc.args...)
				if code := run(context.Background(), auditArgs, &out, &stderr); code != 1 || !strings.Contains(stderr.String(), "no resource types were queried") {
					t.Fatalf("audit exit %d: %s %s", code, &out, &stderr)
				}
			}
		})
	}
}

func TestDoctorDiscoveryMultipleRoots(t *testing.T) {
	bin := doctorFixture(t)
	t.Setenv("DOCTOR_STATE_JSON", `{"version":4,"resources":[]}`)
	t.Setenv("DOCTOR_SCHEMA_JSON", doctorTestSchema(t, map[string]map[string][]string{"registry.terraform.io/hashicorp/aws": {"aws_thing": nil}}))
	other := filepath.Join(t.TempDir(), "other")
	if err := os.MkdirAll(filepath.Join(other, ".terraform"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "main.tf"), []byte("terraform {}"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := run(context.Background(), []string{"doctor", "-no-config", "-terraform", bin, "-format", "json", ".", other}, &out, io.Discard); code != 0 {
		t.Fatalf("exit %d: %s", code, &out)
	}
	var d doctorReport
	if err := json.Unmarshal(out.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	c := doctorFindCheck(t, d, "discovery", "")
	if c.Count == nil || *c.Count != 2 || c.Status != "ok" {
		t.Fatal(c)
	}
}

func TestDiscoveryPredicate(t *testing.T) {
	for _, tc := range []struct {
		ran, plugins            int
		printQuery, wantNothing bool
	}{
		{0, 0, false, true}, {1, 0, false, false}, {0, 1, false, false}, {2, 1, false, false}, {0, 0, true, false},
	} {
		o := &options{plugins: make(listFlag, tc.plugins), printQuery: tc.printQuery}
		if got := nothingChecked(o, report.Summary{Ran: tc.ran}); got != tc.wantNothing {
			t.Fatalf("%+v: %t", tc, got)
		}
	}
}

func TestOfflineConfigSharedWithAudit(t *testing.T) {
	bin := doctorFixture(t)
	if err := os.WriteFile(defaultConfigFile, []byte("offline: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	plugin := `printf '%s\n' '{"done":true}'`
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			args := []string{"-no-query", "-plugin", plugin}
			if explicit {
				args = append(args, "-config", defaultConfigFile)
			}
			for _, adopt := range []bool{false, true} {
				auditArgs := append([]string{}, args...)
				if adopt {
					auditArgs = append([]string{"adopt"}, auditArgs...)
				}
				var out, stderr bytes.Buffer
				if code := run(context.Background(), auditArgs, &out, &stderr); code != 0 {
					t.Fatalf("audit exit %d: %s %s", code, &out, &stderr)
				}
			}
			var out, stderr bytes.Buffer
			doctorArgs := append([]string{"doctor", "-terraform", bin, "-format", "json"}, args...)
			if code := run(context.Background(), doctorArgs, &out, &stderr); code != 0 {
				t.Fatalf("doctor exit %d: %s %s", code, &out, &stderr)
			}
			var d doctorReport
			if err := json.Unmarshal(out.Bytes(), &d); err != nil {
				t.Fatal(err)
			}
			if d.Status != "unknown" {
				t.Fatal(d.Status)
			}
			if _, err := parseFlags(d.NextCommand[1:], io.Discard); err != nil {
				t.Fatalf("next command rejected shared config: %v", err)
			}
		})
	}
}

func TestDoctorTerminalControlCharacters(t *testing.T) {
	bin := doctorFixture(t)
	root := filepath.Join(t.TempDir(), "root-\x1b]52;c;owned\a")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.tf"), []byte("terraform {}"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"text", "json"} {
		var out, stderr bytes.Buffer
		code := run(context.Background(), []string{"doctor", "-no-config", "-terraform", bin, "-offline", "-format", format, root}, &out, &stderr)
		if code != 2 {
			t.Fatalf("exit %d: %s %s", code, &out, &stderr)
		}
		if strings.ContainsFunc(out.String(), func(r rune) bool { return unicode.IsControl(r) && r != '\n' }) {
			t.Fatalf("raw control in %s output: %q", format, out.String())
		}
		if format == "json" {
			var d doctorReport
			if err := json.Unmarshal(out.Bytes(), &d); err != nil {
				t.Fatal(err)
			}
			c := doctorFindCheck(t, d, "root.initialized", report.Clean(root))
			if c.Remediation != "terraform -chdir="+shellQuote(root)+" init" {
				t.Fatalf("JSON changed remediation: %q", c.Remediation)
			}
			if !strings.Contains(strings.Join(d.NextCommand, " "), "<argument with control characters>") || strings.Contains(strings.Join(d.NextCommand, " "), "owned") {
				t.Fatalf("unsafe command: %q", d.NextCommand)
			}
			if !strings.Contains(strings.Join(d.Notes, " "), "placeholder") {
				t.Fatal(d.Notes)
			}
		} else {
			_, next, ok := strings.Cut(out.String(), "Next run:\n")
			if !ok || strings.Contains(next, "owned") || !strings.Contains(next, "<argument with control characters>") {
				t.Fatalf("unsafe command: %q", next)
			}
			if !strings.Contains(out.String(), "placeholder") {
				t.Fatal(out.String())
			}
		}
	}
	raw := doctorReport{NextCommand: []string{"tforphans", "-arg", "region=eu\nmalicious"}}
	original := append([]string{}, raw.NextCommand...)
	var out bytes.Buffer
	if err := raw.write(&out, "text"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(raw.NextCommand, original) {
		t.Fatal("formatting mutated command arguments")
	}
}

func TestDoctorCommandWhitespace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell quoting")
	}
	for _, value := range []string{"root  directory", "  leading", "trailing  ", "a  ' b"} {
		quoted := shellQuote(value)
		if report.Clean(quoted) != quoted {
			t.Fatalf("cleaning changes quoted argument: %q", quoted)
		}
		out, err := exec.Command("sh", "-c", "printf '%s' "+report.Clean(quoted)).Output()
		if err != nil || string(out) != value {
			t.Fatalf("%q round trip: %q, %v", value, out, err)
		}
	}
}
