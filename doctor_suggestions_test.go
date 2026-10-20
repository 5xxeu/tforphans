package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/tf"
)

func TestDoctorURLCredentials(t *testing.T) {
	for _, tc := range []struct{ arg, want string }{
		{"endpoint=https://one.example/a,https://user:password@two.example/b", "endpoint=https://one.example/a,https://two.example/b"},
		{"type:endpoint=https://user:password@one.example/a,https://other:private@two.example/b", "type:endpoint=https://one.example/a,https://two.example/b"},
		{"endpoint=prefix https://user:password@two.example/b suffix", "endpoint=prefix https://two.example/b suffix"},
		{"endpoint=https://one.example/a?signature=private,https://user:password@two.example/b?signature=private", "endpoint=https://one.example/a?REDACTED,https://two.example/b?REDACTED"},
		{"endpoint=https://one.example/ahttps://user:password@two.example/b", "endpoint=https://one.example/ahttps://two.example/b"},
		{"endpoint=https://user:pa'ssword@two.example/b", "endpoint=https://two.example/b"},
		{"endpoint=https://user:pa@ssword@two.example/b", "endpoint=https://two.example/b"},
	} {
		t.Run(tc.arg, func(t *testing.T) {
			if got := doctorArg(tc.arg); got != tc.want {
				t.Fatalf("arg: %q, want %q", got, tc.want)
			}
			for _, args := range [][]string{{"-arg", tc.arg}, {"--arg=" + tc.arg}} {
				command := doctorNextArgs(t, args, nil)
				for _, format := range []string{"text", "json"} {
					var out bytes.Buffer
					if err := (doctorReport{NextCommand: command}).write(&out, format); err != nil {
						t.Fatal(err)
					}
					for _, secret := range []string{"password", "private", "user@", "other@"} {
						if strings.Contains(out.String(), secret) {
							t.Fatalf("credential in %s: %s", format, &out)
						}
					}
				}
			}
		})
	}
	for _, arg := range []string{
		"https://one.example/a,https://user:password@two.example/b",
		"root/https://user:password@two.example/b",
		"prefix=https://user:password@two.example/b",
		"path=https://user:password@one.example/a|https://other:private@two.example/b",
		"prefix HTTPS://user:password@two.example suffix",
		"https://user@two.example/b",
		"https://user:p%40ssword@two.example/b",
		"https://user:pa'ssword@two.example/b",
		"https://user:pa@ssword@two.example/b",
	} {
		t.Run(arg, func(t *testing.T) {
			// Cover positional arguments, both state flag forms, and direct report writers.
			commands := [][]string{
				doctorNextArgs(t, []string{arg}, nil),
				doctorNextArgs(t, []string{"-state", arg}, nil),
				doctorNextArgs(t, []string{"--state=" + arg}, nil),
				{"tforphans", arg},
			}
			for _, command := range commands {
				for _, format := range []string{"text", "json"} {
					var out bytes.Buffer
					if err := (doctorReport{NextCommand: command}).write(&out, format); err != nil {
						t.Fatal(err)
					}
					for _, secret := range []string{"password", "private", "p%40ssword", "ssword", "user@", "other@"} {
						if strings.Contains(out.String(), secret) {
							t.Fatalf("credential in %s: %s", format, &out)
						}
					}
				}
			}
		})
	}
}

func TestDoctorArgumentFixesAcrossRoots(t *testing.T) {
	bin := doctorFixture(t)
	script, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	script = []byte(strings.Replace(string(script), `"providers schema")`, `"providers schema")
  if [ -f doctor-schema.json ]; then cat doctor-schema.json; exit 0; fi`, 1))
	if err := os.WriteFile(bin, script, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCTOR_STATE_JSON", `{"version":4,"resources":[]}`)
	google := "registry.terraform.io/hashicorp/google"
	other := "example.com/other/other"
	type rootSpec struct {
		providers map[string]string
		schemas   map[string]map[string][]string
	}
	for _, tc := range []struct {
		name  string
		roots []rootSpec
		want  []string
	}{
		{"mixed providers in one root", []rootSpec{{
			providers: map[string]string{"google": google, "other": other},
			schemas:   map[string]map[string][]string{google: {"google_widget": {"zone", "region"}}, other: {"other_widget": {"zone", "region"}}},
		}}, []string{"google_widget:region=auto", "google_widget:zone=auto", "other_widget:region=...", "other_widget:zone=..."}},
		{"mixed providers across roots", []rootSpec{
			{providers: map[string]string{"google": google}, schemas: map[string]map[string][]string{google: {"google_widget": {"zone"}}}},
			{providers: map[string]string{"other": other}, schemas: map[string]map[string][]string{other: {"other_widget": {"zone"}}}},
		}, []string{"google_widget:zone=auto", "other_widget:zone=..."}},
		{"same value across types and roots", []rootSpec{
			{providers: map[string]string{"google": google}, schemas: map[string]map[string][]string{google: {"google_a": {"zone"}, "google_b": {"zone"}}}},
			{providers: map[string]string{"google": google}, schemas: map[string]map[string][]string{google: {"google_c": {"zone"}}}},
		}, []string{"zone=auto"}},
		{"same literal across roots", []rootSpec{
			{providers: map[string]string{"other": other}, schemas: map[string]map[string][]string{other: {"other_a": {"zone"}}}},
			{providers: map[string]string{"other": other}, schemas: map[string]map[string][]string{other: {"other_b": {"zone"}}}},
		}, []string{"zone=..."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			roots := []string{}
			for i, spec := range tc.roots {
				root := filepath.Join(base, string(rune('a'+i)))
				if err := os.MkdirAll(filepath.Join(root, ".terraform"), 0700); err != nil {
					t.Fatal(err)
				}
				config := "terraform {\n required_providers {\n"
				names := []string{}
				for name := range spec.providers {
					names = append(names, name)
				}
				slices.Sort(names)
				for _, name := range names {
					config += "  " + name + " = { source = " + tf.HCLString(spec.providers[name]) + " }\n"
				}
				config += " }\n}\n"
				if err := os.WriteFile(filepath.Join(root, "main.tf"), []byte(config), 0600); err != nil {
					t.Fatal(err)
				}
				raw := doctorTestSchema(t, spec.schemas)
				if err := os.WriteFile(filepath.Join(root, "doctor-schema.json"), []byte(raw), 0600); err != nil {
					t.Fatal(err)
				}
				root, err = filepath.EvalSymlinks(root)
				if err != nil {
					t.Fatal(err)
				}
				roots = append(roots, root)
			}
			args := append([]string{"doctor", "-no-config", "-terraform", bin, "-format", "json"}, roots...)
			var out, stderr bytes.Buffer
			if code := run(context.Background(), args, &out, &stderr); code != 2 {
				t.Fatalf("exit %d: %s %s", code, &out, &stderr)
			}
			var d doctorReport
			if err := json.Unmarshal(out.Bytes(), &d); err != nil {
				t.Fatal(err)
			}
			next, err := parseFlags(d.NextCommand[1:], io.Discard)
			if err != nil {
				t.Fatalf("invalid next command: %v", err)
			}
			actual := []string{}
			for _, arg := range next.args {
				value := arg.Name + "=" + strings.Join(arg.Values, ",")
				if arg.Types != "" {
					value = arg.Types + ":" + value
				}
				actual = append(actual, value)
			}
			slices.Sort(actual)
			slices.Sort(tc.want)
			if !reflect.DeepEqual(actual, tc.want) {
				t.Fatalf("args %v, want %v", actual, tc.want)
			}
			for i, root := range roots {
				raw := doctorTestSchema(t, tc.roots[i].schemas)
				var schemas tf.Schemas
				if err := json.Unmarshal([]byte(raw), &schemas); err != nil {
					t.Fatal(err)
				}
				declared, err := tf.LocalNames(root)
				if err != nil {
					t.Fatal(err)
				}
				configs, err := tf.ProviderSourceConfigs(root)
				if err != nil {
					t.Fatal(err)
				}
				providerConfigs, err := tf.ProviderConfigs(root)
				if err != nil {
					t.Fatal(err)
				}
				plan := tf.BuildPlan(&schemas, declared, tf.PlanOptions{Args: next.args, Configs: configs, ProviderConfigs: providerConfigs})
				if len(plan.Skipped) != 0 || len(plan.Queries) == 0 {
					t.Fatalf("suggested args do not plan root %s: %+v", root, plan)
				}
			}
			for _, c := range d.Checks {
				if c.ID != "query.arguments" || c.Status != "failed" {
					continue
				}
				_, typ, _ := strings.Cut(c.Message, ":")
				for _, arg := range tc.want {
					parsed, err := tf.ParseArg(arg)
					if err != nil {
						t.Fatal(err)
					}
					if parsed.Types != "" && parsed.Types != typ {
						continue
					}
					if !strings.Contains(c.Message, parsed.Name) {
						continue
					}
					if !strings.Contains(c.Remediation, "-arg "+arg) {
						t.Fatalf("stale remediation: %+v, want %s", c, arg)
					}
				}
			}
		})
	}
}

func TestDoctorArgumentFixesKeepOtherFlags(t *testing.T) {
	d := doctorReport{fixes: []string{"-scan", "cloudflare"}}
	d.add("query.arguments", "failed", "list config requires zone", "", "")
	d.argFixes = append(d.argFixes, doctorArgFix{check: 0, typ: "google_widget", name: "zone", value: "auto"})
	d.updateArgumentFixes()
	d.updateArgumentFixes()
	if !reflect.DeepEqual(d.fixes, []string{"-scan", "cloudflare", "-arg", "zone=auto"}) {
		t.Fatal(d.fixes)
	}
}

func TestDoctorNextPreservesNonURLs(t *testing.T) {
	args := []string{"-types", "aws_?3_bucket", "-exclude-types=aws_?3_*", "root?name", "-state", "https://user:password@host/state?token=secret", "-state=https://user:password@host/other?signature=secret"}
	want := []string{"tforphans", "-types", "aws_?3_bucket", "-exclude-types=aws_?3_*", "root?name", "-state", "https://host/state?REDACTED", "-state=https://host/other?REDACTED"}
	got := doctorNextArgs(t, args, nil)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%q want %q", got, want)
	}
	for _, format := range []string{"text", "json"} {
		var out bytes.Buffer
		if err := (doctorReport{NextCommand: got}).write(&out, format); err != nil {
			t.Fatal(err)
		}
		for _, value := range []string{"aws_?3_bucket", "aws_?3_*", "root?name", "https://host/state?REDACTED", "https://host/other?REDACTED"} {
			if !strings.Contains(out.String(), value) {
				t.Fatalf("%s lost %q: %s", format, value, &out)
			}
		}
		for _, secret := range []string{"password", "token", "signature", "secret"} {
			if strings.Contains(out.String(), secret) {
				t.Fatalf("%s leaked %q: %s", format, secret, &out)
			}
		}
	}
}

func TestDoctorNextUnicodeSpace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "root\u00a0directory")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	unsafe := "root\x1b]52;control"
	command := doctorNextArgs(t, []string{path, unsafe}, nil)
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			var out bytes.Buffer
			if err := (doctorReport{NextCommand: command}).write(&out, format); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "\x1b") {
				t.Fatalf("raw control character: %q", out.String())
			}
			if format == "text" {
				_, next, ok := strings.Cut(out.String(), "Next run:\n  ")
				want := "tforphans '" + path + "' '<argument with control characters>'\n"
				if !ok || next != want {
					t.Fatalf("command bytes %q want %q", next, want)
				}
			} else {
				var d doctorReport
				if err := json.Unmarshal(out.Bytes(), &d); err != nil {
					t.Fatal(err)
				}
				want := []string{"tforphans", path, "<argument with control characters>"}
				if !reflect.DeepEqual(d.NextCommand, want) {
					t.Fatalf("%q want %q", d.NextCommand, want)
				}
				if len(d.Notes) != 1 {
					t.Fatalf("missing control-character note: %+v", d)
				}
			}
		})
	}
}
