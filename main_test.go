package main

import (
	"encoding/json"
	"fmt"
	"io"
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
	o, err = parseFlags([]string{"-arg", "region=a,b", "-arg", "zone=z", "-arg", "region=b,c"}, io.Discard)
	if err != nil || fmt.Sprint(o.args) != "[{region [a b c]} {zone [z]}]" {
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
		{To: "vault_policy.ci", ID: "ci"},
		{To: "fake_thing.a", Identity: map[string]string{"id": "x", "n": "5"}},
	}
	for _, c := range []struct {
		r    model.Resource
		want bool
	}{
		{model.Resource{Type: "vault_policy", ImportID: "ci"}, true},
		{model.Resource{Type: "vault_policy", ImportID: "ops"}, false},
		{model.Resource{Type: "vault_mount", ImportID: "ci"}, false},
		{model.Resource{Type: "fake_thing", Identity: map[string]any{"id": "x", "n": json.Number("5")}}, true},
		{model.Resource{Type: "fake_thing", Identity: map[string]any{"id": "x"}}, false},
	} {
		if got := hasImport(c.r, imports); got != c.want {
			t.Errorf("%+v: got %v", c.r, got)
		}
	}
}
