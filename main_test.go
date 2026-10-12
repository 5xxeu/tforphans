package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/5xxeu/tforphans/internal/model"
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
	if len(got) != 2 || got[0].Source != "a" || got[1].ProviderSource != "registry.terraform.io/other/foo" {
		t.Errorf("got %+v", got)
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
