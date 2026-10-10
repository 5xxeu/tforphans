package tf

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestFindRoots(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"s3/main.tf":               "terraform {\n  backend \"s3\" {\n    bucket = \"b\"\n  }\n}\n",
		"s3/modules/child/main.tf": "variable \"x\" {}\n",
		"cloud/versions.tf":        "terraform {\n  cloud {\n    organization = \"o\"\n  }\n}\n",
		"json/main.tf.json":        `{"terraform": {"backend": {"gcs": {"bucket": "b"}}}}`,
		"local/main.tf":            "resource \"null_resource\" \"x\" {}\n",
		"local/terraform.tfstate":  "{}",
		"ws/main.tf":               "resource \"null_resource\" \"x\" {}\n",
		"ws/terraform.tfstate.d/prod/terraform.tfstate": "{}",
		"plain/main.tf":                   "terraform {\n  required_version = \">= 1.14\"\n}\n",
		"broken/main.tf":                  "terraform {",
		"s3/.terraform/modules/m/main.tf": "terraform {\n  backend \"s3\" {}\n}\n",
		".hidden/main.tf":                 "terraform {\n  backend \"s3\" {}\n}\n",
		"vendor/kubespray/main.tf":        "terraform {\n  backend \"s3\" {}\n}\n",
		"s3/testdata/fixture/main.tf":     "terraform {\n  backend \"s3\" {}\n}\n",
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := FindRoots(dir)
	if err != nil {
		t.Fatal(err)
	}
	var rel []string
	for _, g := range got {
		r, _ := filepath.Rel(dir, g)
		rel = append(rel, r)
	}
	want := []string{"cloud", "json", "local", "s3", "ws"}
	if !reflect.DeepEqual(rel, want) {
		t.Errorf("FindRoots = %v, want %v", rel, want)
	}
}

func TestParseWorkspaces(t *testing.T) {
	got := parseWorkspaces("  default\n* prod\n\n  staging\n  _blue\n  -x\n  a+b@c~d\n[WARN] a newer version is available\n")
	if want := []string{"default", "prod", "staging", "_blue", "-x", "a+b@c~d"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}

func TestUserListProviders(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "mine.tfquery.hcl"), []byte(`
list "aws_instance" "other" {
  provider = aws.other_account
  limit    = 5000
  config {
    region = "us-east-1"
  }
}
list "aws_s3_bucket" "main" {
  provider = aws
}
list "aws_vpc" "dyn" {
  provider = aws
  limit    = var.n
}
`), 0o644)
	os.WriteFile(filepath.Join(dir, "zz_tforphans_1.tfquery.hcl"), []byte(`list "aws_vpc" "tfo_aws_vpc" { provider = aws }`), 0o644)
	got, err := UserLists(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]UserList{
		"list.aws_instance.other": {Provider: "aws.other_account", Limit: 5000},
		"list.aws_s3_bucket.main": {Provider: "aws", Limit: DefaultListLimit},
		"list.aws_vpc.dyn":        {Provider: "aws", Limit: 0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}

func TestDataDir(t *testing.T) {
	t.Setenv("TF_DATA_DIR", "")
	if got := DataDir("/r"); got != "/r/.terraform" {
		t.Errorf("default %s", got)
	}
	t.Setenv("TF_DATA_DIR", "tfdata")
	if got := DataDir("/r"); got != "/r/tfdata" {
		t.Errorf("relative %s", got)
	}
	t.Setenv("TF_DATA_DIR", "/abs/data")
	if got := DataDir("/r"); got != "/abs/data" {
		t.Errorf("absolute %s", got)
	}
}

func TestAliasOnly(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
provider "aws" {
  alias = "prod"
}
provider "aws" {
  alias = "dev"
}
provider "google" {}
provider "google" {
  alias = "other"
}
`), 0o644)
	got, err := AliasOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || strings.Join(got["aws"], ",") != "dev,prod" {
		t.Errorf("got %v", got)
	}
}

func TestConfigImports(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "imports.tf"), []byte(`
import {
  to = aws_instance.web
  id = "i-1"
}
import {
  to       = fake_thing.a["x"]
  identity = { id = "x", n = 5 }
}
import {
  for_each = toset(["a"])
  to       = aws_s3_bucket.b[each.key]
  id       = each.key
}
`), 0o644)
	os.WriteFile(filepath.Join(dir, "more.tf.json"), []byte(`{"import": [{"to": "vault_policy.ci", "id": "ci"}]}`), 0o644)
	got, err := ConfigImports(dir)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, im := range got {
		lines = append(lines, fmt.Sprintf("%s id=%s identity=%v", im.To, im.ID, im.Identity))
	}
	want := "aws_instance.web id=i-1 identity=map[]\nfake_thing.a id= identity=map[id:x n:5]\naws_s3_bucket.b id= identity=map[]\nvault_policy.ci id=ci identity=map[]"
	if strings.Join(lines, "\n") != want {
		t.Errorf("got:\n%s", strings.Join(lines, "\n"))
	}
}
