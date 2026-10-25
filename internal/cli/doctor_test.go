package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/report"
	"github.com/5xxeu/tforphans/internal/state"
	"github.com/5xxeu/tforphans/internal/tf"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func doctorNextArgs(t *testing.T, args, fixes []string) []string {
	t.Helper()
	o, err := parseFlags([]string{"doctor", "-no-config"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return doctorNext(commandArgs(o.doctor.flagSet, args), fixes)
}

func TestDoctorFlags(t *testing.T) {
	t.Chdir(t.TempDir())
	accepted := []string{"doctor", "-no-config", "-terraform", "./terraform", "-state", "state.json", "-state-roots", "infra", "-current-workspace", "-terragrunt", "-terragrunt-unit", "unit", "-types", "google_*", "-exclude-types", "google_project", "-arg", "zone=auto", "-no-aliases", "-scan", "cloudflare", "-scan-scope", "zone_id=abc", "-no-query", "-format", "json", "-v", "-quiet", "-timeout", "1m", "-source-timeout", "10s", "-max-state-size", "2M", "-state-parallelism", "2", "-max-roots", "5", "-offline", "-check-auth", "-plugin", "echo done", "root"}
	o, err := parseFlags(accepted, io.Discard)
	if err != nil || o.doctor == nil || !o.doctor.offline || !o.doctor.checkAuth || o.src.maxStateSize != 2<<20 || len(o.src.args) != 1 {
		t.Fatalf("options: %+v, %v", o, err)
	}
	for _, name := range []string{"refresh", "init", "allow-unpinned", "baseline", "o", "names-from-tags", "allow-incomplete", "out-dir", "adopt-root", "ignore", "ignore-preset", "strict", "limit", "print-query", "allow-http", "version"} {
		if _, err := parseFlags([]string{"doctor", "-no-config", "-" + name}, io.Discard); err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
			t.Errorf("-%s: %v", name, err)
		}
	}
	for _, args := range [][]string{{"-arg", "invalid"}, {"-max-state-size", "0"}, {"-state-parallelism", "65"}, {"-scan", "unknown"}} {
		if _, err := parseFlags(append([]string{"doctor", "-no-config"}, args...), io.Discard); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

func TestDoctorConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	config := `types: fake_thing
strict: true
baseline: not-read.json
ignore-preset: []
no-default-presets: true
`
	if err := os.WriteFile(defaultConfigFile, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	o, err := parseFlags([]string{"doctor"}, io.Discard)
	if err != nil || !o.src.include.Match("fake_thing") || o.audit.strict || o.audit.baseline != "" || !o.ignore.noDefaults {
		t.Fatalf("config: %+v, %v", o, err)
	}
	if err := os.WriteFile(defaultConfigFile, []byte("terraform: malicious\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := parseFlags([]string{"doctor"}, io.Discard); err == nil {
		t.Fatal("commandLineOnly accepted")
	}
	if _, err := parseFlags([]string{"doctor", "-no-config"}, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorNext(t *testing.T) {
	got := doctorNextArgs(t, []string{"-offline", "-check-auth=false", "-format=json", "-state", "https://u:password@host/state?token=secret", "-arg", "region=eu-north-1", "-arg", "token=secret", "-plugin", "echo secret", "root with 'quote'"}, []string{"-arg", "zone=auto", "-arg", "zone=auto"})
	want := []string{"tforphans", "-state", "https://host/state?REDACTED", "-arg", "region=eu-north-1", "-arg", "token=<redacted>", "-plugin", "<plugin command>", "-arg", "zone=auto", "--", "root with 'quote'"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%q, want %q", got, want)
	}
	d := doctorReport{Status: "ready", NextCommand: got}
	var out bytes.Buffer
	if err := d.write(&out, "text"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "secret") || strings.Contains(out.String(), "password") || !strings.Contains(out.String(), `'root with '"'"'quote'"'"''`) {
		t.Fatal(out.String())
	}
	got = doctorNextArgs(t, []string{"-state=https://u:p@host/s?secret=value", "--plugin=echo secret", "--arg=token=secret"}, nil)
	if strings.Contains(strings.Join(got, " "), "secret") || strings.Contains(strings.Join(got, " "), "u:p") {
		t.Fatal(got)
	}
}

func doctorFixture(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell wrapper")
	}
	root := t.TempDir()
	t.Chdir(root)
	if err := os.Mkdir(".terraform", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("main.tf", []byte(`terraform {}`), 0600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "terraform")
	script := `#!/bin/sh
if [ -n "$TF_CLI_ARGS" ]; then exit 9; fi
case "$1 $2" in
 "version -json") if [ -n "$DOCTOR_VERSION" ]; then printf '%s' "$DOCTOR_VERSION"; else printf '%s' '{"terraform_version":"1.16.0"}'; fi ;;
 "providers schema")
  if [ -n "$DOCTOR_SCHEMA_ERROR" ]; then printf '%s' "$DOCTOR_SCHEMA_ERROR" >&2; exit 1; fi
  if [ -n "$DOCTOR_SCHEMA_JSON" ]; then printf '%s' "$DOCTOR_SCHEMA_JSON"; exit 0; fi
  printf '%s' '{"provider_schemas":{"registry.terraform.io/hashicorp/aws":{"list_resource_schemas":{"aws_thing":{"block":{"attributes":{"region":{"type":"string","required":true}}}}}}}}' ;;
 "workspace list")
  if [ -n "$DOCTOR_WORKSPACES" ]; then printf '%s' "$DOCTOR_WORKSPACES"; exit 0; fi
  printf '* default\n  other\n' ;;
 "state pull")
  if [ "$DOCTOR_STATE_SLOW" = 1 ]; then sleep 0.2; fi
  if [ -n "$DOCTOR_STATE_ERROR" ]; then printf '%s' "$DOCTOR_STATE_ERROR" >&2; exit 1; fi
  if [ -n "$DOCTOR_STATE_JSON" ]; then printf '%s' "$DOCTOR_STATE_JSON"; exit 0; fi
  if [ "$DOCTOR_STATE_FAIL" = 1 ]; then printf 'secret state error' >&2; exit 1; fi
  if [ "$DOCTOR_STATE_BAD" = 1 ]; then printf 'invalid secret'; exit 0; fi
  printf '%s' '{"version":4,"resources":[{"mode":"managed","type":"aws_thing","name":"a","provider":"provider[\"registry.terraform.io/hashicorp/aws\"]","instances":[{"attributes":{"id":"secret"}}]}]}' ;;
 "plan "*)
  if [ "$DOCTOR_IDENTITY_PLAN_FAIL" = 1 ]; then printf 'Error: identity plan could not decode provider credentials' >&2; exit 1; fi
  exit 0 ;;
 "show -json") printf '%s' "$DOCTOR_IDENTITY_JSON" ;;
 *) printf 'forbidden invocation' >&2; exit 90 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestDoctorReport(t *testing.T) {
	bin := doctorFixture(t)
	t.Setenv("TF_CLI_ARGS", "must be removed")
	for _, tc := range []struct {
		name   string
		args   []string
		env    string
		code   int
		status string
	}{
		{"ready", []string{"-arg", "region=eu"}, "", 0, "ready"},
		{"required", nil, "", 2, "needs_configuration"},
		{"offline", []string{"-offline", "-arg", "region=eu"}, "DOCTOR_STATE_FAIL", 0, "unknown"},
		{"offline failed", []string{"-offline"}, "", 2, "unknown"},
		{"state failure", []string{"-arg", "region=eu"}, "DOCTOR_STATE_FAIL", 2, "needs_configuration"},
		{"bad state", []string{"-arg", "region=eu"}, "DOCTOR_STATE_BAD", 1, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv(tc.env, "1")
			}
			var out, stderr bytes.Buffer
			args := append([]string{"doctor", "-no-config", "-terraform", bin, "-format", "json"}, tc.args...)
			code := run(context.Background(), args, &out, &stderr)
			if code != tc.code {
				t.Fatalf("exit %d want %d: %s\n%s", code, tc.code, &out, &stderr)
			}
			var d doctorReport
			dec := json.NewDecoder(&out)
			if err := dec.Decode(&d); err != nil {
				t.Fatal(err)
			}
			if dec.Decode(new(any)) != io.EOF {
				t.Fatal("extra stdout")
			}
			if d.SchemaVersion != 1 || d.Status != tc.status {
				t.Fatalf("%+v", d)
			}
			if strings.Contains(string(mustDoctorJSON(t, d)), "secret") {
				t.Fatalf("secret in report: %+v", d)
			}
			if stderr.Len() != 0 {
				t.Fatalf("non-TTY progress: %s", &stderr)
			}
			found := false
			for _, c := range d.Checks {
				if c.ID == "states.read" && c.Total != nil && *c.Total == 2 {
					found = true
				}
				if tc.name == "required" && c.ID == "query.arguments" && c.Status == "failed" && !strings.Contains(c.Remediation, "-arg region=...") {
					t.Fatalf("missing remediation: %+v", c)
				}
			}
			if !strings.HasPrefix(tc.name, "offline") && !found {
				t.Fatal("missing state count")
			}
		})
	}
}

func mustDoctorJSON(t *testing.T, d doctorReport) []byte {
	t.Helper()
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDoctorInterrupted(t *testing.T) {
	bin := doctorFixture(t)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	var out bytes.Buffer
	if code := run(ctx, []string{"doctor", "-no-config", "-terraform", bin, "-format", "json"}, &out, io.Discard); code != 1 {
		t.Fatalf("exit %d: %s", code, &out)
	}
	if !json.Valid(out.Bytes()) {
		t.Fatal(out.String())
	}
}

func TestDoctorVersionFailures(t *testing.T) {
	bin := doctorFixture(t)
	for _, tc := range []struct {
		version string
		code    int
	}{
		{`{"terraform_version":"1.13.5"}`, 2},
		{`invalid`, 1},
		{`{}`, 1},
	} {
		t.Run(tc.version, func(t *testing.T) {
			t.Setenv("DOCTOR_VERSION", tc.version)
			var out, stderr bytes.Buffer
			code := run(context.Background(), []string{"doctor", "-no-config", "-terraform", bin, "-arg", "region=eu", "-format", "json"}, &out, &stderr)
			if code != tc.code {
				t.Fatalf("exit %d want %d: %s %s", code, tc.code, &out, &stderr)
			}
			if !strings.Contains(out.String(), `"read":2`) {
				t.Fatal("state checks did not continue: " + out.String())
			}
		})
	}
}

func TestDoctorTerragruntStatePull(t *testing.T) {
	bin := doctorFixture(t)
	unit := filepath.Join(t.TempDir(), "unit")
	if err := os.Mkdir(unit, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unit, "terragrunt.hcl"), []byte(`terraform { source = "./module" }`), 0600); err != nil {
		t.Fatal(err)
	}
	unit, err := filepath.EvalSymlinks(unit)
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "terragrunt-args")
	t.Setenv("DOCTOR_TG_LOG", log)
	script := `#!/bin/sh
printf '%s\n' "$*" > "$DOCTOR_TG_LOG"
mkdir .terragrunt-cache
printf '%s' '{"version":4,"resources":[]}'
`
	if err := os.WriteFile("terragrunt", []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", cwd+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out, stderr bytes.Buffer
	code := run(context.Background(), []string{"doctor", "-no-config", "-terraform", bin, "-no-query", "-plugin", "must not execute", "-terragrunt", "-state-roots", unit, "-format", "json"}, &out, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, &out, &stderr)
	}
	args, err := os.ReadFile(log)
	if err != nil || string(args) != "run -- state pull\n" {
		t.Fatalf("terragrunt: %s, %v", args, err)
	}
	if _, err := os.Stat(filepath.Join(unit, ".terragrunt-cache")); err != nil {
		t.Fatal(err)
	}
	var d doctorReport
	if err := json.Unmarshal(out.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	c := doctorFindCheck(t, d, "states.read", "")
	if c.Read == nil || *c.Read != 1 || c.Status != "ok" {
		t.Fatalf("states: %+v", c)
	}
	doctorAssertStateOnly(t, d, unit)
}

func doctorFindCheck(t *testing.T, d doctorReport, id, root string) doctorCheck {
	t.Helper()
	for _, c := range d.Checks {
		if c.ID == id && c.Root == root {
			return c
		}
	}
	t.Fatalf("missing check %s [%s]: %+v", id, root, d.Checks)
	return doctorCheck{}
}

func doctorAssertStateOnly(t *testing.T, d doctorReport, root string) {
	t.Helper()
	c := doctorFindCheck(t, d, "root.query", root)
	if c.Status != "skipped" || c.Message != "used only for state" {
		t.Fatalf("state-only query: %+v", c)
	}
	for _, c := range d.Checks {
		if c.Root == root && (strings.HasPrefix(c.ID, "query.") || c.ID == "providers.schema" || c.ID == "auth.providers") {
			t.Fatalf("extra query skip: %+v", c)
		}
	}
}

func TestDoctorStateErrors(t *testing.T) {
	bin := doctorFixture(t)
	for _, tc := range []struct{ message, fix string }{
		{"Module source has changed", "terraform -chdir='.' init"},
		{"Backend initialization required", "terraform -chdir='.' init"},
		{"Required plugins are not installed", "terraform -chdir='.' init"},
		{"please run terraform init", "terraform -chdir='.' init"},
		{`Please run "terraform init"`, "terraform -chdir='.' init"},
		{"Module not installed", "terraform -chdir='.' init"},
		{"403 Forbidden", "backend credentials"},
		{"HTTP 401 Unauthorized", "backend credentials"},
		{"AccessDenied", "backend credentials"},
		{"provider could not decode or parse /tmp/parse-state: unexpected output", "Check backend credentials, state source and Terraform initialization"},
	} {
		t.Run(tc.message, func(t *testing.T) {
			diagnostic := "\x1b[31mError: " + tc.message + "\x1b[0m\nsecret attribute details"
			t.Setenv("DOCTOR_STATE_ERROR", diagnostic)
			var out, stderr bytes.Buffer
			code := run(context.Background(), []string{"doctor", "-no-config", "-terraform", bin, "-arg", "region=eu", "-format", "json"}, &out, &stderr)
			if code != 2 {
				t.Fatalf("exit %d: %s %s", code, &out, &stderr)
			}
			var d doctorReport
			if err := json.Unmarshal(out.Bytes(), &d); err != nil {
				t.Fatal(err)
			}
			c := doctorFindCheck(t, d, "states.read", ".")
			// Compare with the audit's own handling of per-workspace errors.
			audit := report.NewUnreadState(".", errors.Join(inWorkspace("default", errors.New("terraform state pull: exit status 1\n"+diagnostic)), inWorkspace("other", errors.New("terraform state pull: exit status 1\n"+diagnostic))))
			if c.Message != report.Clean(audit.Error) || !strings.Contains(c.Remediation, tc.fix) {
				t.Fatalf("state: %+v, want %q and %q", c, audit.Error, tc.fix)
			}
			if strings.Contains(out.String(), "secret attribute") {
				t.Fatal(out.String())
			}
		})
	}
	for _, tc := range []struct {
		err error
		fix string
	}{
		{fmt.Errorf("state: %w", &state.TooLargeError{Max: 1024}), "-max-state-size above 1024"},
		{fmt.Errorf("root: %w", sourceTimeout{after: time.Second, err: context.DeadlineExceeded}), "-source-timeout"},
		{fmt.Errorf("root: %w", context.DeadlineExceeded), "-source-timeout"},
	} {
		if fix := doctorStateRemediation("root", tc.err, &options{}); !strings.Contains(fix, tc.fix) {
			t.Errorf("%v: %s", tc.err, fix)
		}
	}
	// Fetch redacts state URLs before the diagnostic reaches the reporter.
	var out bytes.Buffer
	code := run(context.Background(), []string{"doctor", "-no-config", "-terraform", bin, "-arg", "region=eu", "-state", "http://user:password@example.test/state?token=secret", "-format", "json"}, &out, io.Discard)
	if code != 2 || strings.Contains(out.String(), "password") || strings.Contains(out.String(), "token=secret") {
		t.Fatalf("redaction: exit %d, %s", code, &out)
	}
}

func TestDoctorOutputNoise(t *testing.T) {
	bin := doctorFixture(t)
	t.Setenv("DOCTOR_STATE_JSON", `{"version":4,"resources":[]}`)
	if err := os.WriteFile(".terraform.lock.hcl", nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, offline := range []bool{false, true} {
		t.Run(fmt.Sprint(offline), func(t *testing.T) {
			args := []string{"doctor", "-no-config", "-terraform", bin, "-arg", "region=eu", "-format", "json"}
			if offline {
				args = append(args, "-offline")
			}
			var out bytes.Buffer
			if code := run(context.Background(), args, &out, io.Discard); code != 0 {
				t.Fatalf("exit %d: %s", code, &out)
			}
			var d doctorReport
			if err := json.Unmarshal(out.Bytes(), &d); err != nil {
				t.Fatal(err)
			}
			for _, c := range d.Checks {
				if c.ID == "states.backend" || c.ID == "plugins" || !offline && c.Status == "warning" {
					t.Fatalf("noise: %+v", c)
				}
			}
			if offline && len(d.Notes) != 0 || !offline && len(d.Notes) != 1 {
				t.Fatalf("notes: %v", d.Notes)
			}
			var text bytes.Buffer
			if err := d.write(&text, "text"); err != nil {
				t.Fatal(err)
			}
			if n := strings.Count(text.String(), "State pull confirms backend access"); n != len(d.Notes) {
				t.Fatal(text.String())
			}
		})
	}
	o, err := parseFlags([]string{"doctor", "-no-config", "-terraform", bin}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var d doctorReport
	d.root(context.Background(), o, ".", false, map[state.TypeKey]bool{})
	doctorAssertStateOnly(t, d, ".")
	var out bytes.Buffer
	if code := run(context.Background(), []string{"doctor", "-no-config", "-terraform", bin, "-arg", "region=eu", "-plugin", "must not execute", "-format", "json"}, &out, io.Discard); code != 0 {
		t.Fatal(out.String())
	}
	var withPlugin doctorReport
	if err := json.Unmarshal(out.Bytes(), &withPlugin); err != nil {
		t.Fatal(err)
	}
	if c := doctorFindCheck(t, withPlugin, "plugins", ""); c.Status != "skipped" {
		t.Fatal(c)
	}
}

func TestDoctorShellArguments(t *testing.T) {
	for _, s := range []string{"tforphans", "-no-query", "region=eu-north-1", "key_vault_name=prod", "./root", "a+b,c:d@e%f"} {
		if got := shellArg(s); got != s {
			t.Errorf("%q: %q", s, got)
		}
	}
	for _, s := range []string{"", "root with spaces", "a'b", "$TOKEN", "$(id)", "a;b", "a|b", "a&b", "*", "?", "[ab]", "~/.config", "#comment", "a\nb", "x=y>z", "a\\b", "a\"b", "(x)", "{a,b}"} {
		got := shellArg(s)
		if got == s || !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
			t.Errorf("%q: %q", s, got)
		}
	}
	var out bytes.Buffer
	d := doctorReport{Status: "ready", NextCommand: []string{"tforphans", "-no-query", "root with spaces", "-arg", "project=prod"}}
	if err := d.write(&out, "text"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Next run:\n  tforphans -no-query 'root with spaces' -arg project=prod\n") {
		t.Fatal(out.String())
	}
}

func TestDoctorArgumentValues(t *testing.T) {
	for _, arg := range []string{"key_vault_name=vault-prod", "monkey_name=George", "hockey_team=team", "key_name=name", "key_id=id", "project=prod", "folder=123", "custom=a,b", "aws_*:key_vault_name=prod", "keyboard=layout", "ssh_key_name=prod", "encryption_key_version=2", "pattern=a?b#c", "filter=a:b", "name=hello world"} {
		if got := redactArg(arg); got != arg {
			t.Errorf("%q: %q", arg, got)
		}
	}
	for _, name := range []string{"token", "api_token", "SECRET", "client_secret", "password", "db_password", "credential", "credentials", "key", "api_key", "private-key", "aws_*:access_key", "google_*:private_key", "apiKey", "privateKey", "APIKey"} {
		arg := name + "=sensitive"
		if got := redactArg(arg); got != name+"=<redacted>" {
			t.Errorf("%q: %q", arg, got)
		}
	}
	want := []string{"tforphans", "-arg", "key_vault_name=prod", "--arg=custom=a?b#c", "-plugin", "<plugin command>"}
	if got := doctorNextArgs(t, []string{"-arg", "key_vault_name=prod", "--arg=custom=a?b#c", "-plugin", "credentials here"}, nil); !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}

func TestDoctorMalformedResponses(t *testing.T) {
	bin := doctorFixture(t)
	for _, tc := range []struct {
		name, env, value string
		code             int
	}{
		{"schema syntax", "DOCTOR_SCHEMA_JSON", `invalid`, 1},
		{"schema type", "DOCTOR_SCHEMA_JSON", `{"provider_schemas":[]}`, 1},
		{"schema missing", "DOCTOR_SCHEMA_JSON", `{}`, 1},
		{"schema command", "DOCTOR_SCHEMA_ERROR", "Error: cannot decode /parse/provider: unexpected output", 2},
		{"workspace format", "DOCTOR_WORKSPACES", "unexpected output", 1},
		{"state syntax", "DOCTOR_STATE_JSON", `invalid`, 1},
		{"state type", "DOCTOR_STATE_JSON", `{"version":"four"}`, 1},
		{"state version", "DOCTOR_STATE_JSON", `{"version":3}`, 1},
		{"state trailing JSON", "DOCTOR_STATE_JSON", `{"version":4} {}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.env, tc.value)
			var out bytes.Buffer
			code := run(context.Background(), []string{"doctor", "-no-config", "-terraform", bin, "-arg", "region=eu", "-format", "json"}, &out, io.Discard)
			if code != tc.code {
				t.Fatalf("exit %d want %d: %s", code, tc.code, &out)
			}
		})
	}
	_, err := state.ParseSnapshot([]byte(`{"version":4} {}`), "parse-state")
	var response *model.ResponseError
	if !errors.As(errors.Join(errors.New("another error"), fmt.Errorf("wrapped: %w", err)), &response) {
		t.Fatalf("untyped error: %v", err)
	}
	if doctorResponseError(errors.New("cannot parse, decode, or read unexpected output")) {
		t.Fatal("classified text as a response error")
	}
}

func TestDoctorImportGroups(t *testing.T) {
	files := token.NewFileSet()
	f, err := parser.ParseFile(files, "doctor.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	external := false
	previous := 0
	for _, imp := range f.Imports {
		isExternal := strings.Contains(imp.Path.Value, ".")
		line := files.Position(imp.Pos()).Line
		if !external && isExternal && line-previous < 2 {
			t.Fatal("external imports are grouped with standard library imports")
		}
		if external && !isExternal {
			t.Fatal("standard library import after external imports")
		}
		external = isExternal
		previous = line
	}
}

func doctorTestSchema(t *testing.T, sources map[string]map[string][]string) string {
	t.Helper()
	providers := map[string]any{}
	for source, types := range sources {
		lists := map[string]any{}
		for typ, required := range types {
			attrs := map[string]any{}
			for _, name := range required {
				attrs[name] = map[string]any{"type": "string", "required": true}
			}
			lists[typ] = map[string]any{"block": map[string]any{"attributes": attrs}}
		}
		providers[source] = map[string]any{"list_resource_schemas": lists}
	}
	b, err := json.Marshal(map[string]any{"provider_schemas": providers})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDoctorPlanSkipCoverage(t *testing.T) {
	bin := doctorFixture(t)
	aws := "registry.terraform.io/hashicorp/aws"
	google := "registry.terraform.io/hashicorp/google"
	beta := "registry.terraform.io/hashicorp/google-beta"
	awsConfig := `terraform {
 required_providers {
  aws = { source = "hashicorp/aws" }
 }
}`
	googleConfig := `terraform {
 required_providers {
  google = { source = "hashicorp/google" }
  beta = { source = "hashicorp/google-beta" }
 }
}`
	for _, tc := range []struct {
		name, config string
		sources      map[string]map[string][]string
		args         []string
		skips        bool
	}{
		{"required argument", awsConfig, map[string]map[string][]string{aws: {"aws_thing": {"region"}}}, nil, true},
		{"unsupported auto", awsConfig, map[string]map[string][]string{aws: {"aws_thing": {"region"}}}, []string{"-arg", "region=auto"}, true},
		{"opt-in", googleConfig, map[string]map[string][]string{google: {"google_project": nil, "google_network": nil}}, nil, true},
		{"drop twins", googleConfig, map[string]map[string][]string{google: {"google_network": nil}, beta: {"google_network": nil}}, nil, true},
		{"no aliases", awsConfig + "\nprovider \"aws\" {\n alias = \"prod\"\n}\n", map[string]map[string][]string{aws: {"aws_thing": nil}}, []string{"-no-aliases"}, true},
		{"aliases selected", awsConfig + "\nprovider \"aws\" {\n alias = \"prod\"\n}\n", map[string]map[string][]string{aws: {"aws_thing": nil}}, nil, false},
		{"multiple local names", `terraform {
 required_providers {
  aws = { source = "hashicorp/aws" }
  other = { source = "hashicorp/aws" }
 }
}`, map[string]map[string][]string{aws: {"aws_thing": nil}}, nil, true},
		{"excluded types", awsConfig, map[string]map[string][]string{aws: {"aws_thing": {"region"}}}, []string{"-exclude-types", "aws_*"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile("main.tf", []byte(tc.config), 0600); err != nil {
				t.Fatal(err)
			}
			raw := doctorTestSchema(t, tc.sources)
			t.Setenv("DOCTOR_SCHEMA_JSON", raw)
			o, err := parseFlags(append([]string{"doctor", "-no-config", "-terraform", bin}, tc.args...), io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			var schemas tf.Schemas
			if err := json.Unmarshal([]byte(raw), &schemas); err != nil {
				t.Fatal(err)
			}
			declared, err := tf.LocalNames(".")
			if err != nil {
				t.Fatal(err)
			}
			sc, err := tf.ProviderSourceConfigs(".")
			if err != nil {
				t.Fatal(err)
			}
			pc, err := tf.ProviderConfigs(".")
			if err != nil {
				t.Fatal(err)
			}
			p := tf.BuildPlan(&schemas, declared, tf.PlanOptions{Args: o.src.args, Include: o.src.include, Exclude: o.src.exclude, Configs: sc, NoAliases: o.src.noAliases, ProviderConfigs: pc})
			if (len(p.Skipped) > 0) != tc.skips {
				t.Fatalf("unexpected plan skips: %+v", p.Skipped)
			}
			var d doctorReport
			d.root(context.Background(), o, ".", true, map[state.TypeKey]bool{})
			wantCode := 0
			for _, sk := range p.Skipped {
				coverage := report.BuildCoverage(nil, nil, []tf.Skip{sk}, nil)
				gap := slices.ContainsFunc(coverage, func(c report.Coverage) bool { return c.Status == report.Incomplete })
				want := "skipped"
				if gap {
					want = "failed"
				}
				if tc.name == "no aliases" {
					want = "warning"
				}
				if want == "failed" {
					wantCode = 2
				}
				i := slices.IndexFunc(d.Checks, func(c doctorCheck) bool {
					return slices.ContainsFunc(c.Skipped, func(got tf.Skip) bool { return reflect.DeepEqual(got, sk) })
				})
				if i < 0 || d.Checks[i].Status != want {
					t.Fatalf("skip %+v: want %s, checks %+v", sk, want, d.Checks)
				}
			}
			if code := d.result(false); code != wantCode {
				t.Fatalf("exit %d want %d: %+v", code, wantCode, d.Checks)
			}
			if !tc.skips && slices.ContainsFunc(d.Checks, func(c doctorCheck) bool { return c.Status == "failed" }) {
				t.Fatalf("unexpected failure: %+v", d.Checks)
			}
			if tc.name == "no aliases" && d.result(false) != 0 {
				t.Fatalf("explicit choice failed: %+v", d.Checks)
			}
		})
	}
}

func TestDoctorAutoArgumentRemediation(t *testing.T) {
	bin := doctorFixture(t)
	// A second family proves the suggestions follow AutoSources, not type names.
	saved, exists := tf.AutoSources["fake"]
	tf.AutoSources["fake"] = tf.AutoSources["google"]
	t.Cleanup(func() {
		if exists {
			tf.AutoSources["fake"] = saved
		} else {
			delete(tf.AutoSources, "fake")
		}
	})
	for _, tc := range []struct{ source, typ, arg, value string }{
		{"hashicorp/google", "google_thing", "zone", "auto"},
		{"hashicorp/google", "google_thing", "region", "auto"},
		{"hashicorp/google-beta", "google_thing", "region", "auto"},
		{"hashicorp/aws", "aws_thing", "region", "..."},
		{"hashicorp/google", "google_thing", "project", "..."},
		{"example.com/fake/fake", "fake_thing", "region", "auto"},
	} {
		t.Run(tc.source+"/"+tc.arg, func(t *testing.T) {
			config := "terraform {\n required_providers {\n local = { source = " + fmt.Sprintf("%q", tc.source) + " }\n }\n}\n"
			if err := os.WriteFile("main.tf", []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			source := tf.NormalizeSource(tc.source)
			t.Setenv("DOCTOR_SCHEMA_JSON", doctorTestSchema(t, map[string]map[string][]string{source: {tc.typ: {tc.arg}}, "example.com/other/other": {"other_thing": nil}}))
			o, err := parseFlags([]string{"doctor", "-no-config", "-terraform", bin}, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			var d doctorReport
			d.root(context.Background(), o, ".", true, map[state.TypeKey]bool{})
			c := doctorFindCheck(t, d, "query.arguments", ".")
			if !strings.Contains(c.Remediation, "-arg "+tc.arg+"="+tc.value) || !slices.Contains(d.fixes, tc.arg+"="+tc.value) {
				t.Fatalf("advice: %+v, %v", c, d.fixes)
			}
		})
	}
}

func TestDoctorStateLimits(t *testing.T) {
	bin := doctorFixture(t)
	for _, tc := range []struct {
		name string
		args []string
		slow bool
		fix  string
	}{
		{"size", []string{"-max-state-size", "1"}, false, "-max-state-size"},
		{"timeout", []string{"-source-timeout", "50ms"}, true, "-source-timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.slow {
				t.Setenv("DOCTOR_STATE_SLOW", "1")
			}
			var out bytes.Buffer
			args := append([]string{"doctor", "-no-config", "-terraform", bin, "-arg", "region=eu", "-format", "json"}, tc.args...)
			if code := run(context.Background(), args, &out, io.Discard); code != 2 {
				t.Fatalf("exit %d: %s", code, &out)
			}
			var d doctorReport
			if err := json.Unmarshal(out.Bytes(), &d); err != nil {
				t.Fatal(err)
			}
			c := doctorFindCheck(t, d, "states.read", ".")
			if !strings.Contains(c.Remediation, tc.fix) {
				t.Fatal(c)
			}
		})
	}
}

func TestDoctorCheckAuthCommandLineOnly(t *testing.T) {
	bin := doctorFixture(t)
	marker := filepath.Join(t.TempDir(), "executed")
	t.Setenv("DOCTOR_EXECUTED", marker)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ntouch \"$DOCTOR_EXECUTED\"\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, explicit := range []bool{false, true} {
		for _, value := range []string{"true", "false"} {
			t.Run(fmt.Sprintf("explicit=%t/value=%s", explicit, value), func(t *testing.T) {
				path := defaultConfigFile
				if explicit {
					path = "doctor.yaml"
				}
				if err := os.WriteFile(path, []byte("check-auth: "+value+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Remove(path) })
				args := []string{"doctor", "-terraform", bin, "-check-auth=false"}
				if explicit {
					args = append(args, "-config", path)
				}
				var out, stderr bytes.Buffer
				code := run(context.Background(), args, &out, &stderr)
				if code != 1 || out.Len() != 0 || !strings.Contains(stderr.String(), "check-auth is accepted only on the command line: it runs a command, sends credentials or writes a file") {
					t.Fatalf("exit %d: %s %s", code, &out, &stderr)
				}
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatalf("a command ran: %v", err)
				}
			})
		}
	}
	if err := os.WriteFile(defaultConfigFile, []byte("offline: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"doctor", "-check-auth"}, {"doctor", "-config", defaultConfigFile, "-check-auth"}} {
		o, err := parseFlags(args, io.Discard)
		if err != nil || !o.doctor.offline || !o.doctor.checkAuth {
			t.Fatalf("offline config and CLI auth: %+v, %v", o, err)
		}
	}
}

func TestDoctorArgumentNameNormalization(t *testing.T) {
	for _, arg := range []string{
		"api_key =sensitive", " api_key =sensitive", "\tapiKey\t=sensitive",
		" aws_* : api_key =sensitive", "aws_*: password \t=sensitive", " type : token =sensitive",
	} {
		t.Run(arg, func(t *testing.T) {
			key, _, _ := strings.Cut(arg, "=")
			if got := redactArg(arg); got != key+"=<redacted>" {
				t.Fatalf("%q became %q", arg, got)
			}
			for _, argv := range [][]string{{"-arg", arg}, {"--arg=" + arg}} {
				d := doctorReport{NextCommand: doctorNextArgs(t, argv, nil)}
				for _, format := range []string{"text", "json"} {
					var out bytes.Buffer
					if err := d.write(&out, format); err != nil {
						t.Fatal(err)
					}
					if strings.Contains(out.String(), "sensitive") {
						t.Fatal(out.String())
					}
				}
			}
		})
	}
	for _, arg := range []string{"api_key=sensitive,auto", ":api_key=sensitive", "api_key=", "invalid sensitive"} {
		if got := redactArg(arg); got != "<redacted>" {
			t.Fatalf("invalid %q became %q", arg, got)
		}
	}
	for _, arg := range []string{" key_vault_name =prod", "type : key_name =name", " type : custom =value"} {
		if got := redactArg(arg); got != arg {
			t.Fatalf("safe %q became %q", arg, got)
		}
	}
}

func TestDoctorIdentityResponseErrors(t *testing.T) {
	bin := doctorFixture(t)
	if err := os.WriteFile("main.tf", []byte("terraform {\n required_providers {\n aws = { source = \"hashicorp/aws\" }\n }\n}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, reply string
		planFails   bool
		code        int
		status      string
	}{
		{"invalid JSON", "invalid sensitive", false, 1, "unknown"},
		{"truncated JSON", `{"prior_state":`, false, 1, "unknown"},
		{"wrong JSON type", `{"prior_state":[]}`, false, 1, "unknown"},
		{"trailing JSON", `{} {}`, false, 1, "unknown"},
		{"missing identity attributes", `{}`, false, 2, "needs_configuration"},
		{"plan failure", "", true, 2, "needs_configuration"},
		{"valid", `{"prior_state":{"values":{"root_module":{"resources":[{"mode":"data","address":"data.aws_caller_identity.tfo_id_0_0","values":{"account_id":"123"}},{"mode":"data","address":"data.aws_region.tfo_id_0_1","values":{"region":"eu"}}]}}}}`, false, 0, "ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DOCTOR_IDENTITY_JSON", tc.reply)
			if tc.planFails {
				t.Setenv("DOCTOR_IDENTITY_PLAN_FAIL", "1")
			}
			var out, stderr bytes.Buffer
			code := run(context.Background(), []string{"doctor", "-no-config", "-terraform", bin, "-arg", "region=eu", "-check-auth", "-format", "json"}, &out, &stderr)
			if code != tc.code {
				t.Fatalf("exit %d want %d: %s %s", code, tc.code, &out, &stderr)
			}
			var d doctorReport
			if err := json.Unmarshal(out.Bytes(), &d); err != nil {
				t.Fatal(err)
			}
			if d.Status != tc.status {
				t.Fatalf("status %s want %s", d.Status, tc.status)
			}
			c := doctorFindCheck(t, d, "auth.aws", ".")
			if tc.code == 1 && (c.Status != "failed" || c.Message != "could not decode identity plan" || strings.Contains(c.Remediation, "credentials")) {
				t.Fatalf("malformed response: %+v", c)
			}
			if strings.Contains(out.String(), "sensitive") {
				t.Fatal(out.String())
			}
		})
	}
}

// failOnce fails only write number n, so an error that is not checked is
// not caught by a later write.
type failOnce struct{ n int }

func (f *failOnce) Write(p []byte) (int, error) {
	f.n--
	if f.n == -1 {
		return 0, io.ErrShortWrite
	}
	return len(p), nil
}

func TestDoctorTextReportsEveryWriteError(t *testing.T) {
	d := doctorReport{Status: "ready", Checks: []doctorCheck{{ID: "terraform.version", Status: "ok", Message: "1.15"}}, NextCommand: []string{"tforphans"}}
	count := &countWrites{}
	if err := d.write(count, "text"); err != nil {
		t.Fatal(err)
	}
	for n := range count.n {
		if err := d.write(&failOnce{n: n}, "text"); err == nil {
			t.Fatalf("write %d failed silently", n)
		}
	}
}

type countWrites struct{ n int }

func (c *countWrites) Write(p []byte) (int, error) { c.n++; return len(p), nil }
