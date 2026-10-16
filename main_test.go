package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/glob"
	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/report"
	"github.com/5xxeu/tforphans/internal/state"
	"github.com/5xxeu/tforphans/internal/tf"
)

func TestParseFlags(t *testing.T) {
	o, err := parseFlags([]string{"adopt", "infra", "-o", "imports.tf", "-v"}, io.Discard)
	if err != nil || !o.adopt || o.out != "imports.tf" || !o.verbose || len(o.roots) != 1 || o.roots[0] != "infra" {
		t.Fatalf("adopt: %+v, %v", o, err)
	}
	o, err = parseFlags([]string{"./adopt"}, io.Discard)
	if err != nil || o.adopt || o.roots[0] != "./adopt" {
		t.Fatalf("./adopt as root: %+v, %v", o, err)
	}
	o, err = parseFlags([]string{"-arg", "region=a,b", "-arg", "zone=z", "-arg", "region=b,c", "-arg", "google_*:zone=y"}, io.Discard)
	if err != nil || fmt.Sprint(o.args) != "[{ region [a b c]} { zone [z]} {google_* zone [y]}]" {
		t.Errorf("repeated -arg names: %v, %v", o.args, err)
	}
	for _, argv := range [][]string{
		{"-format", "import"},
		{"adopt", "-format", "json"},
		{"-o", "x.tf"},
		{"-state", "-", "-state", "-"},
	} {
		if _, err := parseFlags(argv, io.Discard); err == nil {
			t.Errorf("%v: no error", argv)
		}
	}
}

func TestMaxStateSizeFlag(t *testing.T) {
	for value, want := range map[string]int64{
		"1": 1, "1024": 1024, "1K": 1 << 10, "512M": 512 << 20, "2G": 2 << 30,
		"1k": 1 << 10, "1m": 1 << 20, "1g": 1 << 30, "9223372036854775806": 1<<63 - 2,
	} {
		o, err := parseFlags([]string{"-no-config", "-max-state-size", value}, io.Discard)
		if err != nil || o.maxStateSize != want {
			t.Errorf("%s: %+v, %v", value, o, err)
		}
	}
	for _, value := range []string{"0", "-1", "0G", "-2M", "", "1.5G", "2GB", "1T", "bytes", "9223372036854775807", "9223372036854775808", "8589934592G"} {
		if _, err := parseFlags([]string{"-no-config", "-max-state-size", value}, io.Discard); err == nil || !strings.Contains(err.Error(), "max-state-size") {
			t.Errorf("%q: %v", value, err)
		}
	}
	if o, err := parseFlags([]string{"-no-config"}, io.Discard); err != nil || o.maxStateSize != state.DefaultMaxSize {
		t.Errorf("default: %+v, %v", o, err)
	}
}

func TestMaxStateSizeCallSites(t *testing.T) {
	dir := t.TempDir()
	data := `{"version":4}`
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"-no-config", "-max-state-size", "1", "-state", path, "-no-query", "-plugin", "true"}, &stdout, &stderr)
	if code != exitError || !strings.Contains(stderr.String(), "state is larger than 1 bytes; raise -max-state-size") {
		t.Errorf("extra state: exit %d, %s", code, &stderr)
	}
	if runtime.GOOS == "windows" {
		t.Skip("shell wrapper")
	}
	bin := filepath.Join(dir, "terraform")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s' '"+data+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	o := &options{terraformBin: bin, currentWS: true, maxStateSize: 1}
	if _, n, err := pullRoot(context.Background(), o, dir); err == nil || n != 0 || !strings.Contains(err.Error(), "raise -max-state-size") {
		t.Errorf("root state: %d, %v", n, err)
	}
	o.maxStateSize = int64(len(data))
	if _, n, err := pullRoot(context.Background(), o, dir); err != nil || n != 1 {
		t.Errorf("root state at boundary: %d, %v", n, err)
	}
}

func TestUserListProvider(t *testing.T) {
	var s tf.Schemas
	json.Unmarshal([]byte(`{"provider_schemas": {
	  "registry.terraform.io/hashicorp/aws": {"resource_identity_schemas": {}},
	  "registry.terraform.io/hashicorp/google-beta": {"resource_identity_schemas": {}}
	}}`), &s)
	declared := map[string]string{"registry.terraform.io/hashicorp/google-beta": "google"}
	for _, c := range []struct{ arg, typ, want string }{
		{"aws.prod", "aws_instance", "registry.terraform.io/hashicorp/aws"},
		{"", "aws_instance", "registry.terraform.io/hashicorp/aws"},
		{"google", "google_compute_network", "registry.terraform.io/hashicorp/google-beta"},
		{"", "acme_thing", ""},
	} {
		if got := userListProvider(c.arg, c.typ, declared, &s); got != c.want {
			t.Errorf("%q %s: got %q, want %q", c.arg, c.typ, got, c.want)
		}
	}
}

func TestHasImport(t *testing.T) {
	imports := []tf.Import{
		{Dir: "a", To: "vault_policy.ci", Type: "vault_policy", ID: "ci"},
		{Dir: "a", To: "fake_thing.a", Type: "fake_thing", Identity: map[string]string{"id": "x", "n": "5"}},
		{Dir: "a", To: "fake_thing.l", Type: "fake_thing", Identity: map[string]string{"ids": `["x",2]`}},
		{Dir: "a", Type: "vault_mount", ID: "kv"}, // into module.app
		{Dir: "a", To: "vault_policy.ops", Type: "vault_policy", Provider: "vault.other", ID: "ops"},
	}
	roots := map[string]bool{"a": true, "b": true}
	for _, c := range []struct {
		r    model.Resource
		want bool
	}{
		{model.Resource{Type: "vault_policy", ImportID: "ci"}, true},
		{model.Resource{Type: "vault_policy", ImportID: "ci", Source: "a"}, true},
		// Another root imports with its own provider configuration.
		{model.Resource{Type: "vault_policy", ImportID: "ci", Source: "b"}, false},
		{model.Resource{Type: "vault_policy", ImportID: "ops"}, false},
		{model.Resource{Type: "vault_policy", ImportID: "ops", Provider: "vault.other"}, true},
		{model.Resource{Type: "vault_policy", ImportID: "ci", Provider: "vault.other"}, false},
		{model.Resource{Type: "vault_mount", ImportID: "ci"}, false},
		{model.Resource{Type: "vault_mount", ImportID: "kv"}, true},
		{model.Resource{Type: "fake_thing", Identity: map[string]any{"id": "x", "n": json.Number("5")}}, true},
		{model.Resource{Type: "fake_thing", Identity: map[string]any{"id": "x"}}, false},
		{model.Resource{Type: "fake_thing", Identity: map[string]any{"ids": []any{"x", json.Number("2")}}}, true},
	} {
		if got := hasImport(c.r, imports, roots); got != c.want {
			t.Errorf("%+v: got %v", c.r, got)
		}
	}
}

func TestDedupe(t *testing.T) {
	id := map[string]any{"id": "42"}
	got := dedupe([]model.Resource{
		{Type: "foo_thing", Identity: id, ProviderSource: "registry.terraform.io/acme/foo", Source: "a"},
		{Type: "foo_thing", Identity: id, ProviderSource: "registry.terraform.io/acme/foo", Source: "b"},
		{Type: "foo_thing", Identity: id, ProviderSource: "registry.terraform.io/other/foo", Source: "a"},
		{Type: "foo_thing", Identity: id, Source: "plugin1"},
	})
	// The result without a provider could be either object: it stays apart
	// rather than hide one of them.
	if len(got) != 3 || got[0].Source != "a" || got[1].ProviderSource != "registry.terraform.io/other/foo" || got[2].Source != "plugin1" {
		t.Errorf("got %+v", got)
	}
}

// TestDedupeProviderPermutations: the result does not depend on which
// source answered first.
func TestDedupeProviderPermutations(t *testing.T) {
	id := map[string]any{"id": "42"}
	acme := model.Resource{Type: "foo_thing", Identity: id, ProviderSource: "registry.terraform.io/acme/foo", Source: "a"}
	other := model.Resource{Type: "foo_thing", Identity: id, ProviderSource: "registry.terraform.io/other/foo", Source: "b"}
	unknown := model.Resource{Type: "foo_thing", Identity: id, Source: "plugin1", Attributes: map[string]any{"labels": "x"}}
	families := func(rs []model.Resource) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.ProviderSource)
		}
		slices.Sort(out)
		return out
	}
	perms := [][]model.Resource{
		{acme, other, unknown}, {acme, unknown, other}, {other, acme, unknown},
		{other, unknown, acme}, {unknown, acme, other}, {unknown, other, acme},
	}
	for _, p := range perms {
		got := families(dedupe(slices.Clone(p)))
		if want := []string{"", "registry.terraform.io/acme/foo", "registry.terraform.io/other/foo"}; !slices.Equal(got, want) {
			t.Errorf("%v: %v", families(p), got)
		}
	}
	// With one known provider the unknown result is the same object, in
	// either order, and the known copy is kept with what the other knew.
	for _, p := range [][]model.Resource{{acme, unknown}, {unknown, acme}} {
		got := dedupe(slices.Clone(p))
		if len(got) != 1 || got[0].ProviderSource != acme.ProviderSource || got[0].Attributes["labels"] != "x" {
			t.Errorf("one known provider: %+v", got)
		}
	}
	// Results without a provider merge with each other.
	if got := dedupe([]model.Resource{unknown, {Type: "foo_thing", Identity: id, Source: "plugin2"}}); len(got) != 1 {
		t.Errorf("two unknown: %+v", got)
	}
}

// TestPluginKeysAllRequired runs a plugin against a state file: every
// identity key the plugin gives must be found in state, so a policy of
// another namespace stays an orphan.
func TestPluginKeysAllRequired(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "vault.tfstate")
	plugin := `printf '%s\n' '{"type":"vault_policy","identity":{"id":"policy","namespace":"team-b"}}'`
	for _, c := range []struct {
		attrs string
		want  int
	}{
		{`{"id": "policy"}`, exitOrphans},
		{`{"id": "policy", "namespace": "team-a"}`, exitOrphans},
		{`{"id": "policy", "namespace": "team-b"}`, 0},
	} {
		if err := os.WriteFile(statePath, []byte(`{"version": 4, "resources": [{"mode": "managed", "type": "vault_policy", "name": "p",
  "provider": "provider[\"registry.terraform.io/hashicorp/vault\"]", "instances": [{"attributes": `+c.attrs+`}]}]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), []string{"-no-query", "-state", statePath, "-plugin", plugin, "-strict"}, &stdout, &stderr); code != c.want {
			t.Errorf("state %s: exit %d, want %d\n%s%s", c.attrs, code, c.want, stdout.String(), stderr.String())
		}
	}
	// A type that cannot be written as configuration is a plugin error.
	var stdout, stderr bytes.Buffer
	bad := `printf '%s\n' '{"type":"vault policy","identity":{"id":"x"}}'`
	if code := run(context.Background(), []string{"adopt", "-no-query", "-state", statePath, "-plugin", bad}, &stdout, &stderr); code != exitError || stdout.Len() > 0 {
		t.Errorf("invalid type: exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
}

func TestDedupeKeepsAttributes(t *testing.T) {
	id := map[string]any{"id": "1"}
	withAttrs := model.Resource{Type: "t", Identity: id, Attributes: map[string]any{"labels": map[string]any{"a": "b"}}, TagName: "web"}
	plain := model.Resource{Type: "t", Identity: id}
	for _, order := range [][]model.Resource{{plain, withAttrs}, {withAttrs, plain}} {
		got := dedupe(slices.Clone(order))
		if len(got) != 1 || got[0].Attributes["labels"] == nil || got[0].TagName != "web" {
			t.Errorf("got %+v", got)
		}
	}
}

// TestDedupeUnknownLocation: objects with an empty project from two roots
// may be in two projects; they are not merged, nor are their attributes.
func TestDedupeUnknownLocation(t *testing.T) {
	id := map[string]any{"name": "node", "zone": "z1", "project": ""}
	a := model.Resource{Type: "google_compute_instance", Identity: id, Source: "a", ListAddress: "list.google_compute_instance.mine"}
	b := model.Resource{Type: "google_compute_instance", Identity: id, Source: "b", ListAddress: "list.google_compute_instance.tfo_google_google_compute_instance",
		Attributes: map[string]any{"labels": map[string]any{"goog-gke-node": ""}}}
	got := dedupe([]model.Resource{a, b})
	if len(got) != 2 || got[0].Attributes != nil {
		t.Errorf("got %+v", got)
	}
}

// A root that pins a provider -scan can list gets a hint, not a scan: the
// scan would use credentials from the environment.
func TestScanHints(t *testing.T) {
	root := t.TempDir()
	lock := `provider "registry.terraform.io/cloudflare/cloudflare" {
  version = "5.0.0"
  hashes  = ["h1:x"]
}
`
	if err := os.WriteFile(filepath.Join(root, ".terraform.lock.hcl"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
	o := &options{roots: []string{root}}
	if h := scanHints(o, nil); len(h) != 1 || !strings.Contains(h[0], "-scan cloudflare") {
		t.Errorf("hints %q", h)
	}
	o.scans = []string{"cloudflare"}
	if h := scanHints(o, nil); len(h) != 0 {
		t.Errorf("hints while scanning %q", h)
	}
	if p := runPlan(o, []string{"a", "b"}); !strings.Contains(p, "terraform query in "+root) || !strings.Contains(p, "2 more root modules") || !strings.Contains(p, "whole account") {
		t.Errorf("plan %q", p)
	}
}

// -max-roots stops a -state-roots walk that found too much before terraform
// runs anywhere.
func TestMaxRoots(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"a", "b", "c"} {
		os.MkdirAll(filepath.Join(dir, d), 0o755)
		os.WriteFile(filepath.Join(dir, d, "main.tf"), []byte("terraform {\n  backend \"s3\" {}\n}\n"), 0o644)
	}
	var stderr bytes.Buffer
	code := run(context.Background(), []string{"-no-query", "-plugin", "true", "-state-roots", dir, "-max-roots", "2", "-terraform", "/nonexistent/terraform"}, io.Discard, &stderr)
	if code != exitError || !strings.Contains(stderr.String(), "more than -max-roots 2") {
		t.Errorf("exit %d\n%s", code, stderr.String())
	}
}

// A directory under -state-roots that cannot be read may hold a root that
// manages what was found: adopt writes nothing, like for an unread state.
func TestAdoptUnreadableDir(t *testing.T) {
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	os.MkdirAll(locked, 0o755)
	if err := os.Chmod(locked, 0); err != nil {
		t.Skip(err)
	}
	defer os.Chmod(locked, 0o755)
	if _, err := os.ReadDir(locked); err == nil {
		t.Skip("permissions are not enforced here (root)")
	}
	plugin := `echo '{"type": "vault_policy", "identity": {"id": "ci"}, "import_id": "ci"}'`
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"adopt", "-no-query", "-plugin", plugin, "-state-roots", dir}, &stdout, &stderr)
	if code != exitError || stdout.Len() != 0 || !strings.Contains(stderr.String(), "not writing import blocks") {
		t.Errorf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
}

// A Terragrunt unit whose terragrunt.hcl does not parse may manage what is
// found: adopt refuses, as with any unread state.
func TestAdoptBrokenTerragruntUnit(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "app"), 0o755)
	os.WriteFile(filepath.Join(dir, "app", "terragrunt.hcl"), []byte("include \"root\" {\n"), 0o644)
	plugin := `echo '{"type": "vault_policy", "identity": {"id": "ci"}, "import_id": "ci"}'`
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"adopt", "-no-query", "-plugin", plugin, "-state-roots", dir, "-terragrunt"}, &stdout, &stderr)
	if code != exitError || stdout.Len() != 0 || !strings.Contains(stderr.String(), "not writing import blocks") || !strings.Contains(stderr.String(), "could not be parsed") {
		t.Errorf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
}

// A unit given as a root argument would be queried with terraform and its
// .tf backend, not terragrunt's: refused with -terragrunt.
func TestTerragruntUnitAsRoot(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "app"), 0o755)
	os.WriteFile(filepath.Join(dir, "app", "terragrunt.hcl"), []byte("terraform {\n  source = \"../m\"\n}\n"), 0o644)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"-no-query", "-plugin", "true", "-state-roots", dir, "-terragrunt", filepath.Join(dir, "app")}, &stdout, &stderr)
	if code != exitError || !strings.Contains(stderr.String(), "is a Terragrunt unit") {
		t.Errorf("exit %d\n%s", code, stderr.String())
	}
}

// A generated unit that another unit includes is taken for shared
// configuration, with a note; -terragrunt-unit reads it as a unit.
func TestTerragruntUnitOverride(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake terragrunt is a shell script")
	}
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "terragrunt"), []byte("#!/bin/sh\necho '{\"version\":4,\"serial\":1,\"lineage\":\"x\",\"resources\":[]}'\n"), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "app", "db"), 0o755)
	os.WriteFile(filepath.Join(dir, "app", "terragrunt.hcl"), []byte("remote_state {\n  backend = \"s3\"\n}\ngenerate \"main\" {\n  path = \"main.tf\"\n  contents = \"\"\n}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "app", "db", "terragrunt.hcl"), []byte("include \"app\" {\n  path = \"../terragrunt.hcl\"\n}\n"), 0o644)
	tool := func(args ...string) (int, string) {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), append([]string{"-no-query", "-plugin", "true", "-state-roots", dir, "-terragrunt"}, args...), &stdout, &stderr)
		return code, stderr.String()
	}
	if code, stderr := tool(); code != exitOK || !strings.Contains(stderr, "1 states read") || !strings.Contains(stderr, "note: 1 terragrunt.hcl taken for shared configuration") || !strings.Contains(stderr, filepath.Join(dir, "app")+" (by ") {
		t.Errorf("default: exit %d\n%s", code, stderr)
	}
	if code, stderr := tool("-terragrunt-unit", filepath.Join(dir, "app")); code != exitOK || !strings.Contains(stderr, "2 states read") || strings.Contains(stderr, "shared configuration") {
		t.Errorf("-terragrunt-unit: exit %d\n%s", code, stderr)
	}
	if code, stderr := tool("-terragrunt-unit", dir); code != exitError || !strings.Contains(stderr, "no terragrunt.hcl there") {
		t.Errorf("-terragrunt-unit without terragrunt.hcl: exit %d\n%s", code, stderr)
	}
	if _, err := parseFlags([]string{"-terragrunt-unit", "app"}, io.Discard); err == nil || !strings.Contains(err.Error(), "needs -terragrunt") {
		t.Errorf("without -terragrunt: %v", err)
	}
}

// TestImpliedNoQuery: -scan without ROOT_DIR in a directory without
// Terraform configuration runs no terraform query.
func TestImpliedNoQuery(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	o, err := parseFlags([]string{"-scan", "proxmox"}, io.Discard)
	if err != nil || !o.noQuery || !o.impliedNoQuery || len(o.roots) != 0 {
		t.Fatalf("no config: %+v, %v", o, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte("terraform {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o, err = parseFlags([]string{"-scan", "proxmox"}, io.Discard)
	if err != nil || o.noQuery || !slices.Equal(o.roots, []string{"."}) {
		t.Fatalf("with config: %+v, %v", o, err)
	}
}

func TestIgnoreHints(t *testing.T) {
	var rs []model.Resource
	for i := range hintMin {
		rs = append(rs, model.Resource{Type: "cloudflare_certificate_pack", ScopeName: "zone example.com", RequiredKeys: []string{"id"},
			Identity: map[string]any{"id": fmt.Sprint(i), "zone_id": "z1"}})
	}
	rs = append(rs, model.Resource{Type: "cloudflare_dns_record", ScopeName: "zone example.com", Identity: map[string]any{"id": "r", "zone_id": "z1"}})
	hints := ignoreHints(rs)
	if len(hints) != 1 || !strings.HasSuffix(hints[0], ": cloudflare_certificate_pack zone_id=z1") || !strings.HasPrefix(hints[0], "20 unmanaged cloudflare_certificate_pack in zone example.com") {
		t.Fatalf("hints %q", hints)
	}
	// The suggested line parses and hides them.
	line := hints[0][strings.LastIndex(hints[0], ": ")+2:]
	ig, err := report.ParseIgnore(strings.NewReader(line + "\n"))
	if err != nil || !ig.Match(rs[0]) || ig.Match(rs[len(rs)-1]) {
		t.Errorf("line %q: %v", line, err)
	}
	// Packs of one kind: the line names it and leaves an uploaded pack in
	// the same zone alone.
	kinds := slices.Clone(rs)
	for i := range kinds {
		kinds[i].Attributes = map[string]any{"type": "advanced"}
	}
	upload := model.Resource{Type: "cloudflare_certificate_pack", ScopeName: "zone example.com", RequiredKeys: []string{"id"},
		Identity: map[string]any{"id": "u", "zone_id": "z1"}, Attributes: map[string]any{"type": "sni_custom"}}
	kinds = append(kinds, upload)
	hints = ignoreHints(kinds)
	if len(hints) != 1 || !strings.HasSuffix(hints[0], ": cloudflare_certificate_pack zone_id=z1 attr.type=advanced") || !strings.HasPrefix(hints[0], "20 unmanaged cloudflare_certificate_pack (type advanced) in zone example.com") {
		t.Fatalf("hints by kind %q", hints)
	}
	line = hints[0][strings.LastIndex(hints[0], ": ")+2:]
	if ig, err = report.ParseIgnore(strings.NewReader(line + "\n")); err != nil || !ig.Match(kinds[0]) || ig.Match(upload) {
		t.Errorf("line %q: %v", line, err)
	}
	// A kind that would not parse as one word, or would match as a glob,
	// gives no hint.
	for _, kind := range []string{"a b", "a\u00a0b", "a\nb", "a*"} {
		for i := range kinds {
			kinds[i].Attributes = map[string]any{"type": kind}
		}
		if hints := ignoreHints(kinds); len(hints) != 0 {
			t.Errorf("unsafe kind %q: %q", kind, hints)
		}
	}

	// Different zones under one name, or no scope: no hint.
	rs[0].Identity = map[string]any{"id": "0", "zone_id": "z2"}
	if hints := ignoreHints(rs); len(hints) != 0 {
		t.Errorf("mixed scope values: %q", hints)
	}
	for i := range rs {
		rs[i].ScopeName = ""
	}
	if hints := ignoreHints(rs); len(hints) != 0 {
		t.Errorf("no scope: %q", hints)
	}
}

// TestSortBaseName: guests named alike keep their listed order whatever
// node the display name adds, so import addresses keep their suffixes.
func TestSortBaseName(t *testing.T) {
	rs := []model.Resource{
		{Type: "vm", DisplayName: "tmpl (pve2)", BaseName: "tmpl", ImportID: "pve2/100"},
		{Type: "vm", DisplayName: "tmpl (pve1)", BaseName: "tmpl", ImportID: "pve1/200"},
		{Type: "vm", DisplayName: "app (pve3)", BaseName: "app", ImportID: "pve3/300"},
	}
	sortResources(rs)
	var got []string
	for _, r := range rs {
		got = append(got, r.ImportID)
	}
	if want := []string{"pve3/300", "pve2/100", "pve1/200"}; !slices.Equal(got, want) {
		t.Errorf("order %v, want %v", got, want)
	}
}

func TestPrintFailedLongList(t *testing.T) {
	var failed []tf.Failure
	for i := range 12 {
		failed = append(failed, tf.Failure{Address: fmt.Sprintf("data.x.t%02d", i), Type: fmt.Sprintf("t%02d", i), Category: tf.FailDenied, Message: "403"})
	}
	failed = append(failed, tf.Failure{Address: "data.x.c", Type: "c", Category: tf.FailCrash, Message: "crashed"})
	var b strings.Builder
	printFailed(&b, &options{}, failed)
	out := b.String()
	if !strings.Contains(out, "permission denied (12): t00, t01, t02, t03, t04, t05, t06, t07, and 4 more\n") || !strings.Contains(out, "lack permission") || !strings.Contains(out, "crashed (1): c\n") {
		t.Errorf("output:\n%s", out)
	}
	b.Reset()
	printFailed(&b, &options{verbose: true}, failed)
	if !strings.Contains(b.String(), "t11\n") {
		t.Errorf("-v output:\n%s", b.String())
	}
}

// TestKeepAttrs: a list result keeps the attributes asked for and its kind
// for ignore hints; the output then drops the kind unless asked for.
func TestKeepAttrs(t *testing.T) {
	attrs := map[string]any{"labels": map[string]any{"a": "b"}, "type": "pd-ssd", "size": json.Number("10"), "nested": map[string]any{"type": "x"}}
	got := keepAttrs(attrs, []string{"labels"})
	if len(got) != 2 || got["type"] != "pd-ssd" || got["labels"] == nil {
		t.Errorf("kept %v", got)
	}
	if got := keepAttrs(map[string]any{"type": map[string]any{}}, nil); got != nil {
		t.Errorf("a type that is no scalar kept: %v", got)
	}
	r := model.Resource{Attributes: keepAttrs(attrs, []string{"labels"})}
	(&options{includeAttrs: []string{"labels"}}).trimAttrs(&r)
	if _, ok := r.Attributes["type"]; ok || r.Attributes["labels"] == nil {
		t.Errorf("output attributes %v", r.Attributes)
	}
}

func TestPresetHints(t *testing.T) {
	var rs []model.Resource
	for i := range presetHintMin {
		rs = append(rs, model.Resource{Type: "proxmox_virtual_environment_vm", Identity: map[string]any{"vm_id": json.Number(fmt.Sprint(9000 + i))}, Attributes: map[string]any{"template": true}})
	}
	hints := presetHints(rs, nil)
	if len(hints) != 1 || !strings.Contains(hints[0], "match -ignore-preset proxmox-templates") {
		t.Errorf("hints %q", hints)
	}
	if hints := presetHints(rs, []string{"proxmox-templates"}); len(hints) != 0 {
		t.Errorf("applied preset named: %q", hints)
	}
	if hints := presetHints(rs[1:], nil); len(hints) != 0 {
		t.Errorf("below the threshold: %q", hints)
	}
}

// TestDefaultPresets: the default presets exist and hide what they are
// for; -no-default-presets leaves them out.
func TestDefaultPresets(t *testing.T) {
	for _, name := range report.DefaultPresets {
		if _, err := report.Preset(name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	aws, err := report.Preset("aws-system")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []model.Resource{
		{Type: "aws_security_group", Identity: map[string]any{"id": "sg-1"}, Attributes: map[string]any{"name": "default"}},
		{Type: "aws_default_security_group", Identity: map[string]any{"id": "sg-1"}},
		{Type: "aws_kms_alias", Identity: map[string]any{"name": "alias/aws/ebs"}},
	} {
		if !aws.Match(r) {
			t.Errorf("%s %v not hidden", r.Type, r.Identity)
		}
	}
	for _, r := range []model.Resource{
		{Type: "aws_security_group", Identity: map[string]any{"id": "sg-2"}, Attributes: map[string]any{"name": "web"}},
		{Type: "aws_kms_alias", Identity: map[string]any{"name": "alias/app"}},
	} {
		if aws.Match(r) {
			t.Errorf("%s %v hidden", r.Type, r.Identity)
		}
	}
}

// TestLoadIgnoreDefaults: the default presets apply unless
// -no-default-presets, and a preset asked for applies either way.
func TestLoadIgnoreDefaults(t *testing.T) {
	t.Chdir(t.TempDir())
	sg := model.Resource{Type: "aws_default_security_group", Identity: map[string]any{"id": "sg-1"}}
	tmpl := model.Resource{Type: "proxmox_virtual_environment_vm", Identity: map[string]any{"vm_id": json.Number("9000")}, Attributes: map[string]any{"template": true}}
	for _, c := range []struct {
		args        []string
		sg, tmplHid bool
	}{
		{nil, true, false},
		{[]string{"-no-default-presets"}, false, false},
		{[]string{"-no-default-presets", "-ignore-preset", "aws-system,proxmox-templates"}, true, true},
		{[]string{"-ignore-preset", "proxmox-templates"}, true, true},
	} {
		o, err := parseFlags(append([]string{"-scan", "proxmox"}, c.args...), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		ig, err := loadIgnore(o)
		if err != nil {
			t.Fatal(err)
		}
		if ig.Match(sg) != c.sg || ig.Match(tmpl) != c.tmplHid {
			t.Errorf("%v: default SG hidden %v, template hidden %v", c.args, ig.Match(sg), ig.Match(tmpl))
		}
	}
}

func TestUnlistedTypes(t *testing.T) {
	counts := map[state.TypeKey]int{
		{Type: "aws_instance", Provider: "hashicorp/aws"}:                   3,
		{Type: "aws_iam_role_policy_attachment", Provider: "hashicorp/aws"}: 12,
		{Type: "aws_excluded_thing", Provider: "hashicorp/aws"}:             9,
		{Type: "google_project", Provider: "hashicorp/google"}:              1,
		{Type: "cloudflare_record", Provider: "cloudflare/cloudflare"}:      4,
		{Type: "cloudflare_page_rule", Provider: "cloudflare/cloudflare"}:   2,
		{Type: "acme_thing", Provider: "acme/acme"}:                         6,
		{Type: "acme_thing", Provider: "other/acme"}:                        7,
		{Type: "vault_policy", Provider: "hashicorp/vault"}:                 5,
		{Type: "vault_mount", Provider: "hashicorp/vault"}:                  1,
	}
	var sum report.Summary
	sum.AddListable("registry.terraform.io/hashicorp/aws", "aws_instance")
	// A scan that found nothing still asked its provider; the old name
	// cloudflare_record is covered through AltTypes, as is telmate's
	// proxmox_vm_qemu by the bpg scan while telmate's schema was read.
	sum.AddListable("cloudflare/cloudflare", "cloudflare_dns_record")
	sum.AddListable("bpg/proxmox", "proxmox_virtual_environment_vm")
	sum.AddListable("", "cloudflare_record", "proxmox_vm_qemu")
	sum.AddListable("registry.terraform.io/telmate/proxmox")
	// The same type name: only acme/acme can list it.
	sum.AddListable("registry.terraform.io/acme/acme", "acme_thing")
	sum.AddListable("registry.terraform.io/other/acme")
	// A plugin returned vault_policy objects.
	sum.AddListable("hashicorp/vault", "vault_policy")
	o := &options{exclude: glob.CompileList([]string{"aws_excluded_*"})}
	got := unlistedTypes(o, counts, sum)
	want := []report.UnlistedType{
		{Type: "aws_iam_role_policy_attachment", Provider: "hashicorp/aws", Instances: 12},
		{Type: "acme_thing", Provider: "other/acme", Instances: 7},
		{Type: "cloudflare_page_rule", Provider: "cloudflare/cloudflare", Instances: 2},
		{Type: "vault_mount", Provider: "hashicorp/vault", Instances: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
	// -types narrows the run: other types are the user's choice.
	o = &options{include: glob.CompileList([]string{"cloudflare_*"})}
	if got := unlistedTypes(o, counts, sum); len(got) != 1 || got[0].Type != "cloudflare_page_rule" {
		t.Errorf("with -types: %+v", got)
	}
	var many []report.UnlistedType
	for i := range 10 {
		many = append(many, report.UnlistedType{Type: fmt.Sprintf("t%d", i), Instances: 1})
	}
	if note := unlistedNote(many); !strings.Contains(note, "10 resource types") || !strings.Contains(note, "and 2 more") {
		t.Errorf("%s", note)
	}
}

// TestUnlistedTypesThroughPlugin: a plugin's provider counts as asked even
// when -types drops all of its results.
func TestUnlistedTypesThroughPlugin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell plugin")
	}
	statePath := filepath.Join(t.TempDir(), "s.tfstate")
	st := `{"version":4,"lineage":"l","serial":1,"resources":[
		{"mode":"managed","type":"vault_mount","name":"m","provider":"provider[\"registry.terraform.io/hashicorp/vault\"]","instances":[{"attributes":{"id":"kv"}}]},
		{"mode":"managed","type":"vault_policy","name":"p","provider":"provider[\"registry.terraform.io/hashicorp/vault\"]","instances":[{"attributes":{"id":"admin"}}]}]}`
	if err := os.WriteFile(statePath, []byte(st), 0o600); err != nil {
		t.Fatal(err)
	}
	plugin := `printf '%s\n' '{"type":"vault_policy","identity":{"id":"admin"},"provider_source":"hashicorp/vault"}'`
	// Without provider_source (vault.sh) the plugin's types count for any
	// provider, and so do alternative names.
	anon := `printf '%s\n' '{"type":"vault_policy","identity":{"id":"admin"},"alt_types":["vault_mount"]}' '{"type":"other_thing","identity":{"id":"x"},"provider_source":"hashicorp/vault"}'`
	for _, c := range []struct {
		plugin string
		args   []string
		want   string
	}{
		{plugin, nil, "vault_mount (1)"},
		{plugin, []string{"-types", "vault_mount"}, "vault_mount (1)"},
		{plugin, []string{"-types", "vault_policy"}, ""},
		{anon, nil, ""},
	} {
		args := append([]string{"-no-config", "-no-query", "-state", statePath, "-plugin", c.plugin}, c.args...)
		var stdout, stderr bytes.Buffer
		run(context.Background(), args, &stdout, &stderr)
		got := stderr.String()
		if c.want == "" && strings.Contains(got, "cannot be listed") || c.want != "" && !strings.Contains(got, "1 resource types in the states cannot be listed by their provider, so objects of them are never checked: "+c.want) {
			t.Errorf("%v:\n%s", c.args, got)
		}
	}
}

func TestNameTag(t *testing.T) {
	for _, c := range []struct {
		attrs map[string]any
		want  string
	}{
		{map[string]any{"tags": map[string]any{"Name": " web-1 "}}, "web-1"},
		{map[string]any{"tags": map[string]any{"name": "lower"}}, ""},
		{map[string]any{"tags": map[string]any{"Name": 5}}, ""},
		{map[string]any{"tags": nil}, ""},
		{nil, ""},
	} {
		if got := nameTag(c.attrs); got != c.want {
			t.Errorf("%v: %q", c.attrs, got)
		}
	}
}

func TestHasTags(t *testing.T) {
	var s tf.Schemas
	if err := json.Unmarshal([]byte(`{"provider_schemas":{"registry.terraform.io/hashicorp/aws":{"resource_schemas":{
		"aws_instance":{"block":{"attributes":{"tags":{"type":["map","string"]}}}},
		"aws_iam_role_policy":{"block":{"attributes":{"name":{"type":"string"}}}}}}}}`), &s); err != nil {
		t.Fatal(err)
	}
	if !hasTags(&s, "aws_instance") || hasTags(&s, "aws_iam_role_policy") || hasTags(&s, "aws_missing") {
		t.Error("tags attribute not recognised")
	}
}

func TestStateParallelismBounds(t *testing.T) {
	for _, v := range []string{"0", "65"} {
		if _, err := parseFlags([]string{"-no-config", "-state-parallelism", v}, io.Discard); err == nil {
			t.Errorf("-state-parallelism %s accepted", v)
		}
	}
	o, err := parseFlags([]string{"-no-config", "-state-parallelism", "16"}, io.Discard)
	if err != nil || o.stateParallelism != 16 {
		t.Fatalf("%v %+v", err, o)
	}
	if _, err := parseFlags([]string{"-no-config", "-names-from-tags"}, io.Discard); err == nil {
		t.Error("-names-from-tags accepted without adopt")
	}
}
