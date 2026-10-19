package tf

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
	got, _, _, err := FindRoots(dir, false)
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
	got, err := parseWorkspaces("  default\n* prod\n  staging\n  _blue\n  -x\n  a+b@c~d\n\n[WARN] a newer version is available\n")
	if want := []string{"default", "prod", "staging", "_blue", "-x", "a+b@c~d"}; err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, %v", got, err)
	}
	// A backend warning without its frame (-no-color) before the list:
	// "instead." on its own line is no workspace.
	got, err = parseWorkspaces(`
Warning: Deprecated Parameter

  on main.tf line 4, in terraform:
   4:     dynamodb_table = "locks"

The parameter "dynamodb_table" is deprecated. Use parameter "use_lockfile"
instead.

* default
  prod

`)
	if want := []string{"default", "prod"}; err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("after a warning: got %v, %v", got, err)
	}
	for _, out := range []string{
		"",
		"instead.\n",
		"  default\n  prod\n",   // nothing selected
		"* default\n\n* prod\n", // two lists
		"* default\n* prod\n",   // two selected
	} {
		if got, err := parseWorkspaces(out); err == nil {
			t.Errorf("%q: got %v, want an error", out, got)
		}
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
		"list.aws_instance.other": {Provider: "aws.other_account", Static: true, Limit: 5000, Configuration: `list "aws_instance" "other" {
  provider = aws.other_account
  limit    = 5000
  config {
    region = "us-east-1"
  }
}`},
		"list.aws_s3_bucket.main": {Provider: "aws", Static: true, Limit: DefaultListLimit, Configuration: `list "aws_s3_bucket" "main" {
  provider = aws
}`},
		"list.aws_vpc.dyn": {Provider: "aws", Limit: 0, Configuration: `list "aws_vpc" "dyn" {
  provider = aws
  limit    = var.n
}`},
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
import {
  to       = vault_policy.list
  provider = vault.other
  identity = { ids = ["x", 2], meta = { a = true } }
}
import {
  to = module.app.vault_policy.ci
  id = "ci"
}
import {
  for_each = toset(["a"])
  to       = module.app[each.key].module.db["x.y"].vault_mount.m[each.key]
  id       = "kv"
}
`), 0o644)
	os.WriteFile(filepath.Join(dir, "more.tf.json"), []byte(`{"import": [{"to": "vault_policy.ci", "id": "ci"}]}`), 0o644)
	got, err := ConfigImports(dir)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, im := range got {
		if im.Dir != dir {
			t.Errorf("%s: dir %q", im.Type, im.Dir)
		}
		lines = append(lines, fmt.Sprintf("%s %s provider=%s id=%s identity=%v", im.To, im.Type, im.Provider, im.ID, im.Identity))
	}
	want := `aws_instance.web aws_instance provider= id=i-1 identity=map[]
fake_thing.a fake_thing provider= id= identity=map[id:x n:5]
aws_s3_bucket.b aws_s3_bucket provider= id= identity=map[]
vault_policy.list vault_policy provider=vault.other id= identity=map[ids:["x",2] meta:{"a":true}]
 vault_policy provider= id=ci identity=map[]
 vault_mount provider= id=kv identity=map[]
vault_policy.ci vault_policy provider= id=ci identity=map[]`
	if strings.Join(lines, "\n") != want {
		t.Errorf("got:\n%s", strings.Join(lines, "\n"))
	}
}

// A directory that cannot be read is reported, not fatal.
func TestFindRootsUnreadable(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"ok", "locked"} {
		os.MkdirAll(filepath.Join(dir, d), 0o755)
		os.WriteFile(filepath.Join(dir, d, "main.tf"), []byte("terraform {\n  backend \"s3\" {}\n}\n"), 0o644)
	}
	if err := os.Chmod(filepath.Join(dir, "locked"), 0); err != nil {
		t.Skip(err)
	}
	defer os.Chmod(filepath.Join(dir, "locked"), 0o755)
	if _, err := os.ReadDir(filepath.Join(dir, "locked")); err == nil {
		t.Skip("permissions are not enforced here (root)")
	}
	roots, unread, _, err := FindRoots(dir, false)
	if err != nil || len(roots) != 1 || len(unread) != 1 || !strings.HasSuffix(unread[0].Path, "locked") {
		t.Errorf("roots %v, unread %v, err %v", roots, unread, err)
	}
}

// A configuration file that cannot be read or parsed may hold the backend
// block: its directory is unread, not silently no root. Where a backend is
// found anyway the directory is a root.
func TestFindRootsBrokenConfig(t *testing.T) {
	dir := t.TempDir()
	backend := "terraform {\n  backend \"s3\" {}\n}\n"
	files := map[string]string{
		"stray/main.tf":       "locals {\n  a = 1\n}\n}\n" + backend,
		"json/main.tf.json":   `{"terraform": {"backend": {"s3": {}}}`,
		"after/main.tf":       backend + "}\n",
		"other/main.tf":       backend,
		"other/broken.tf":     "resource \"x\" {\n",
		"module/main.tf":      "variable \"a\" {}\n",
		"link/main.tf":        "variable \"a\" {}\n",
		"subdir/root/main.tf": backend,
		"subdir/bad.tf":       "}\n",
	}
	for name, body := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
	}
	if err := os.Symlink(filepath.Join(dir, "missing.tf"), filepath.Join(dir, "link", "backend.tf")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	roots, unread, _, err := FindRoots(".", false)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"after", "other", filepath.Join("subdir", "root")}; !reflect.DeepEqual(roots, want) {
		t.Errorf("roots %v", roots)
	}
	var paths []string
	for _, u := range unread {
		paths = append(paths, u.Path)
		if !strings.HasPrefix(u.Reason, "configuration could not be read: ") || !strings.Contains(u.Reason, u.Path+string(filepath.Separator)) {
			t.Errorf("%s: %s", u.Path, u.Reason)
		}
	}
	if want := []string{"json", "link", "stray", "subdir"}; !reflect.DeepEqual(paths, want) {
		t.Errorf("unread %v", unread)
	}
}

// Parents are the terragrunt.hcl files units include; any other one is a
// unit, even with units below it or with all its configuration generated.
func TestTerragruntParents(t *testing.T) {
	dir := t.TempDir()
	gen := "remote_state {\n  backend = \"s3\"\n}\ngenerate \"main\" {\n  path = \"main.tf\"\n  contents = \"\"\n}\n"
	files := map[string]string{
		"terragrunt.hcl":             gen, // legacy parent of svc
		"svc/terragrunt.hcl":         "include \"root\" {\n  path = find_in_parent_folders()\n}\n",
		"live/app/terragrunt.hcl":    gen, // a unit with a unit below that includes root.hcl
		"live/app/db/terragrunt.hcl": "include \"root\" {\n  path = find_in_parent_folders(\"root.hcl\")\n}\n",
		"lit/terragrunt.hcl":         gen, // parent by a literal path
		"lit/u/terragrunt.hcl":       "include \"root\" {\n  path = \"../terragrunt.hcl\"\n}\n",
		"x/terragrunt.hcl":           gen, // a terraform root below does not make a parent
		"x/tools/main.tf":            "terraform {\n  backend \"s3\" {}\n}\n",
	}
	for name, body := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
	}
	t.Chdir(dir)
	got, unread, parents, err := FindRoots(".", true)
	if want := []Parent{{".", "svc"}, {"lit", filepath.Join("lit", "u")}}; !reflect.DeepEqual(parents, want) {
		t.Errorf("parents %v", parents)
	}
	want := []string{filepath.Join("lit", "u"), filepath.Join("live", "app"), filepath.Join("live", "app", "db"), "svc", "x", filepath.Join("x", "tools")}
	if err != nil || len(unread) != 0 || !reflect.DeepEqual(got, want) {
		t.Errorf("%v %v %v", got, unread, err)
	}
}

func TestTerragruntUnits(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"terragrunt.hcl":                       "remote_state {\n  backend = \"s3\"\n}\n", // legacy parent: no unit
		"root.hcl":                             "remote_state {\n  backend = \"s3\"\n}\n",
		"app/terragrunt.hcl":                   "include \"root\" {\n  path = find_in_parent_folders()\n}\n", // makes the top one a parent
		"db/terragrunt.hcl":                    "terraform {\n  source = \"../modules/db\"\n}\n",
		"modules/db/main.tf":                   "resource \"terraform_data\" \"x\" {}\n",
		"local/terragrunt.hcl":                 "remote_state {\n  backend = \"s3\"\n}\n", // fresh checkout: no backend.tf yet
		"local/main.tf":                        "resource \"terraform_data\" \"x\" {}\n",
		"app/.terragrunt-cache/x/y/backend.tf": "terraform {\n  backend \"s3\" {}\n}\n",
	}
	for name, body := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
	}
	got, _, _, err := FindRoots(dir, true)
	want := []string{filepath.Join(dir, "app"), filepath.Join(dir, "db"), filepath.Join(dir, "local")}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("units: %v, %v", got, err)
	}
	if got, _, _, _ := FindRoots(dir, false); len(got) != 0 {
		t.Errorf("without -terragrunt: %v", got)
	}
	// A unit whose terragrunt.hcl is broken is unread, not skipped.
	os.MkdirAll(filepath.Join(dir, "broken"), 0o755)
	os.WriteFile(filepath.Join(dir, "broken", "terragrunt.hcl"), []byte("include \"root\" {\n"), 0o644)
	got, unread, _, err := FindRoots(dir, true)
	if err != nil || len(got) != 3 || len(unread) != 1 || !strings.HasSuffix(unread[0].Path, "broken") || !strings.Contains(unread[0].Reason, "could not be parsed") {
		t.Errorf("broken unit: %v %v %v", got, unread, err)
	}
	// Also in a directory that is a terraform root as well (left over from
	// before the move to Terragrunt).
	os.WriteFile(filepath.Join(dir, "broken", "terraform.tfstate"), []byte("{}"), 0o644)
	got, unread, _, err = FindRoots(dir, true)
	if err != nil || len(got) != 3 || len(unread) != 1 || !strings.HasSuffix(unread[0].Path, "broken") {
		t.Errorf("broken unit in a terraform root: %v %v %v", got, unread, err)
	}
	if got, unread, _, _ := FindRoots(dir, false); len(got) != 1 || len(unread) != 0 {
		t.Errorf("without -terragrunt it is a terraform root: %v %v", got, unread)
	}
	// A unit whose configuration is all generated (fresh checkout) is a
	// unit: no units lie below it. A link to a missing file is unread.
	os.MkdirAll(filepath.Join(dir, "gen"), 0o755)
	os.WriteFile(filepath.Join(dir, "gen", "terragrunt.hcl"), []byte("remote_state {\n  backend = \"s3\"\n}\ngenerate \"main\" {\n  path = \"main.tf\"\n  contents = \"\"\n}\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "link"), 0o755)
	if err := os.Symlink(filepath.Join(dir, "missing.hcl"), filepath.Join(dir, "link", "terragrunt.hcl")); err != nil {
		t.Skip(err)
	}
	got, unread, _, err = FindRoots(dir, true)
	if err != nil || !slices.Contains(got, filepath.Join(dir, "gen")) || slices.Contains(got, dir) || len(unread) != 2 || !strings.HasSuffix(unread[1].Path, "link") {
		t.Errorf("generated unit, dangling link: %v %v %v", got, unread, err)
	}
}

// TestSharedDataDirLinks: roots named through symlinks are compared where
// the links lead; .. in TF_DATA_DIR is taken from there, as the OS does.
func TestSharedDataDirLinks(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"tree/a", "tree/b", "other"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	a, b := filepath.Join(dir, "tree", "a"), filepath.Join(dir, "tree", "b")
	linkA, linkB := filepath.Join(dir, "link-a"), filepath.Join(dir, "other", "b")
	for target, link := range map[string]string{a: linkA, b: linkB} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TF_DATA_DIR", "../shared")
	if d, x, y := SharedDataDir([]string{a, linkB}); d != filepath.Join(dir, "tree", "shared") || x != a || y != linkB {
		t.Errorf("sibling through a link: %q %q %q", d, x, y)
	}
	if d, _, _ := SharedDataDir([]string{a, linkA}); d != "" {
		t.Errorf("one root under two names: %q", d)
	}
	t.Setenv("TF_DATA_DIR", ".terraform")
	if d, _, _ := SharedDataDir([]string{a, b, linkB}); d != "" {
		t.Errorf("per-root data directory: %q", d)
	}
}

func TestUserListStatic(t *testing.T) {
	for _, c := range []struct {
		name, body string
		static     bool
	}{
		{"literal", `config { project = "p" }`, true},
		{"provider excluded", "provider = aws.prod\nconfig { project = \"p\" }", true},
		{"variable config", `config { project = var.target_project }`, false},
		{"local limit", `limit = local.limit`, false},
		{"variable function", `config { project = lower(var.target_project) }`, false},
		{"dynamic values", "for_each = var.projects\nconfig { project = each.value }", false},
		{"literal values", "for_each = { p = { project = \"p\" } }\nconfig { project = each.value.project }", true},
		{"each key", "for_each = toset([\"p\"])\nconfig { project = each.key }", true},
		{"count index", "count = 2\nconfig { index = count.index }", true},
		{"dynamic count", "count = var.n\nconfig { index = count.index }", false},
		{"dynamic include", `include_resource = local.include`, false},
		{"nested block", "config {\n filter { value = data.target.name }\n}", false},
		{"nested provider", "config {\n filter { provider = var.target }\n}", false},
		{"module", `config { project = module.target.project }`, false},
		{"workspace", `config { project = terraform.workspace }`, false},
		{"path", `config { project = path.root }`, false},
		{"resource", `config { project = google_project.target.project_id }`, false},
		{"bare each", `config { project = each }`, false},
		{"value without for_each", `config { project = each.value }`, false},
		{"invalid text", `config { project =`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			text := "list \"google_compute_instance\" \"test\" {\n" + c.body + "\n}\n"
			if err := os.WriteFile(filepath.Join(dir, "mine.tfquery.hcl"), []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			lists, err := UserLists(dir)
			if c.name == "invalid text" {
				if err == nil {
					t.Fatal("invalid configuration accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if l, ok := lists["list.google_compute_instance.test"]; !ok || l.Static != c.static {
				t.Fatalf("want Static %t, got %+v", c.static, lists)
			}
		})
	}
}
