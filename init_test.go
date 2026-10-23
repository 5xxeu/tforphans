package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/report"
	"go.yaml.in/yaml/v3"
)

func initFixture(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func invokeInit(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, err bytes.Buffer
	code := run(context.Background(), append([]string{"init"}, args...), &out, &err)
	return code, out.String(), err.String()
}

func readInitConfig(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(defaultConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := yaml.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	return config
}

func TestInitDiscoversWithoutStartingTerraform(t *testing.T) {
	t.Chdir(t.TempDir())
	// No terraform or plugin can run; state bodies and credentials must not be read.
	t.Setenv("PATH", "")
	initFixture(t, "aws/main.tf", `terraform {
 backend "s3" { bucket = "backend-secret" }
 required_providers { aws = { source = "hashicorp/aws" } }
}
provider "aws" { alias = "prod" }
module "child" { source = "./modules/child" }
`)
	initFixture(t, "aws/modules/child/main.tf", `resource "aws_s3_bucket" "x" { bucket = "child" }`)
	initFixture(t, "cf/main.tf", `terraform {
 backend "http" { password = "backend-secret" }
 required_providers { cloudflare = { source = "cloudflare/cloudflare" } }
}
provider "cloudflare" { api_token = "provider-secret" }
`)
	initFixture(t, "cf/.terraform.lock.hcl", `provider "registry.terraform.io/cloudflare/cloudflare" {
 version = "5.0.0"
 hashes = ["h1:example"]
}`)
	initFixture(t, "json/main.tf.json", `{"terraform":{"backend":{"local":{}}},"resource":{"random_id":{"example":{"byte_length":4}}}}`)
	initFixture(t, "state-only/terraform.tfstate", `state-secret`)
	for _, dir := range []string{".hidden", "vendor", "testdata"} {
		initFixture(t, dir+"/main.tf", `terraform {
 backend "local" {}
}`)
	}
	code, out, stderr := invokeInit(t)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, stderr)
	}
	for _, want := range []string{"Found 3 Terraform roots", "backend s3", "backend http", "aws.prod", "cloudflare/cloudflare 5.0.0", "State source: state-only", "Adapter available: -scan cloudflare", "tforphans doctor", "== Terraform roots ", "== Scan adapters ", "== Not checked ", "== Result "} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	raw, _ := os.ReadFile(defaultConfigFile)
	if strings.Contains(out+stderr+string(raw), "-secret") {
		t.Fatal("configuration or state values leaked")
	}
	o, err := parseFlags(nil, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(o.roots, []string{"aws", "cf", "json"}) || !slices.Equal(o.stateRoots, []string{"."}) || len(o.scans) > 0 {
		t.Fatalf("config parsed as %+v", o)
	}
	state, _ := os.ReadFile("state-only/terraform.tfstate")
	if string(state) != "state-secret" {
		t.Fatal("state changed")
	}
}

func TestInitPristineRootAndExplicitScan(t *testing.T) {
	t.Chdir(t.TempDir())
	initFixture(t, "main.tf", `terraform {
 required_providers { cloudflare = { source = "cloudflare/cloudflare" } }
}`)
	code, out, stderr := invokeInit(t, "-scan", "cloudflare")
	if code != 0 {
		t.Fatalf("%d\n%s%s", code, out, stderr)
	}
	o, err := parseFlags(nil, &bytes.Buffer{})
	if err != nil || !slices.Equal(o.roots, []string{"."}) || !slices.Equal(o.scans, []string{"cloudflare"}) {
		t.Fatalf("%+v, %v", o, err)
	}
}

func TestInitUpdatePreservesSettings(t *testing.T) {
	t.Chdir(t.TempDir())
	initFixture(t, "infra/main.tf", `terraform {
 backend "local" {}
}`)
	original := "# audit settings\nroots: [old]\nstate-roots: [old]\nscan: [cloudflare]\nstrict: true # keep this\nexclude-types: [aws_iam_*]\n"
	initFixture(t, defaultConfigFile, original)
	if err := os.Chmod(defaultConfigFile, 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, _ := invokeInit(t)
	raw, _ := os.ReadFile(defaultConfigFile)
	if code != 1 || string(raw) != original {
		t.Fatal("existing config overwritten")
	}
	code, out, stderr := invokeInit(t, "infra", "-update")
	if code != 0 {
		t.Fatalf("%d\n%s%s", code, out, stderr)
	}
	config := readInitConfig(t)
	raw, _ = os.ReadFile(defaultConfigFile)
	info, _ := os.Stat(defaultConfigFile)
	if config["strict"] != true || !strings.Contains(string(raw), "# keep this") || !strings.Contains(string(raw), "# audit settings") || info.Mode().Perm() != 0o600 {
		t.Fatalf("lost settings or permissions: %s (%v)", raw, info.Mode())
	}
	o, err := parseFlags(nil, &bytes.Buffer{})
	if err != nil || !slices.Equal(o.roots, []string{"infra"}) || !slices.Equal(o.scans, []string{"cloudflare"}) || !o.exclude.Match("aws_iam_role") {
		t.Fatalf("%+v %v", o, err)
	}
	initFixture(t, "unrelated/main.tf", `terraform {
 backend "local" {}
}`)
	code, out, stderr = invokeInit(t, "-update")
	if code != 0 {
		t.Fatalf("%d\n%s%s", code, out, stderr)
	}
	o, err = parseFlags(nil, &bytes.Buffer{})
	if err != nil || !slices.Equal(o.roots, []string{"infra"}) {
		t.Fatalf("update broadened selection: %+v %v", o, err)
	}
	code, out, stderr = invokeInit(t, "-update", "-scan=proxmox", "infra")
	if code != 0 {
		t.Fatalf("%d\n%s%s", code, out, stderr)
	}
	o, err = parseFlags(nil, &bytes.Buffer{})
	if err != nil || !slices.Equal(o.scans, []string{"proxmox"}) {
		t.Fatalf("scan update: %+v %v", o, err)
	}
}

func TestInitDryRunAndEndOfFlags(t *testing.T) {
	t.Chdir(t.TempDir())
	initFixture(t, "-update/main.tf", `terraform {
 backend "local" {}
}`)
	code, out, stderr := invokeInit(t, "-dry-run", "--", "-update")
	if code != 0 || !strings.Contains(out, "not written") {
		t.Fatalf("%d\n%s%s", code, out, stderr)
	}
	if _, err := os.Stat(defaultConfigFile); !os.IsNotExist(err) {
		t.Fatal("dry-run wrote config")
	}
	code, out, stderr = invokeInit(t, "--", "-update")
	if code != 0 {
		t.Fatalf("%d\n%s%s", code, out, stderr)
	}
	o, err := parseFlags(nil, &bytes.Buffer{})
	if err != nil || !slices.Equal(o.roots, []string{"-update"}) {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestInitDoesNotWriteIncompleteDiscovery(t *testing.T) {
	for _, kind := range []string{"empty", "broken-root", "broken-lock", "max-roots", "unknown-scan", "outside", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			parent := t.TempDir()
			inside := filepath.Join(parent, "work")
			if err := os.Mkdir(inside, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Chdir(inside)
			initFixture(t, "good/main.tf", `terraform {
 backend "local" {}
}`)
			args := []string{}
			switch kind {
			case "empty":
				if err := os.RemoveAll("good"); err != nil {
					t.Fatal(err)
				}
			case "broken-root":
				initFixture(t, "broken/main.tf", `terraform { backend `)
			case "broken-lock":
				initFixture(t, "good/.terraform.lock.hcl", `provider "x" {`)
			case "max-roots":
				initFixture(t, "other/main.tf", `terraform {
 backend "local" {}
}`)
				args = []string{"-max-roots=1"}
			case "unknown-scan":
				args = []string{"-scan=not-a-provider"}
			case "outside":
				initFixture(t, "../external/main.tf", `terraform {
 backend "local" {}
}`)
				args = []string{"../external"}
			case "cancelled":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if code := run(ctx, []string{"init"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 1 {
					t.Fatal(code)
				}
			}
			if kind != "cancelled" {
				if code, out, err := invokeInit(t, args...); code != 1 {
					t.Fatalf("%d\n%s%s", code, out, err)
				}
			}
			if _, err := os.Stat(defaultConfigFile); !os.IsNotExist(err) {
				t.Fatal("incomplete config written")
			}
		})
	}
}

func TestInitUpdateRejectsInvalidYamlAndSymlink(t *testing.T) {
	for _, original := range []string{"roots: [broken", "roots: [.]\nroots: [.]\n", "---\nroots: [.]\n---\nstrict: true\n", "- roots\n"} {
		t.Run(original, func(t *testing.T) {
			t.Chdir(t.TempDir())
			initFixture(t, "main.tf", `terraform {
 backend "local" {}
}`)
			initFixture(t, defaultConfigFile, original)
			code, out, stderr := invokeInit(t, "-update")
			if code != 1 {
				t.Fatalf("%d\n%s%s", code, out, stderr)
			}
			raw, _ := os.ReadFile(defaultConfigFile)
			if string(raw) != original {
				t.Fatal("invalid original changed")
			}
		})
	}
	t.Run("symlink", func(t *testing.T) {
		t.Chdir(t.TempDir())
		initFixture(t, "main.tf", `terraform {
 backend "local" {}
}`)
		initFixture(t, "target.yaml", "roots: [.]\n")
		if err := os.Symlink("target.yaml", defaultConfigFile); err != nil {
			t.Skip(err)
		}
		if code, _, _ := invokeInit(t, "-update"); code != 1 {
			t.Fatal(code)
		}
		raw, _ := os.ReadFile("target.yaml")
		if string(raw) != "roots: [.]\n" {
			t.Fatal("symlink target changed")
		}
	})
}

func TestInitUpdateDoesNotClobberConcurrentEdit(t *testing.T) {
	t.Chdir(t.TempDir())
	original := []byte("roots: [.]\n")
	initFixture(t, defaultConfigFile, string(original))
	info, err := os.Lstat(defaultConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	initFixture(t, defaultConfigFile, "roots: [other]\n")
	if err := writeInitConfig([]byte("roots: [new]\n"), original, info); err == nil {
		t.Fatal("concurrent edit overwritten")
	}
	raw, _ := os.ReadFile(defaultConfigFile)
	if string(raw) != "roots: [other]\n" {
		t.Fatal("concurrent edit lost")
	}
}

func TestInitUpdateKeepsAliasedSettings(t *testing.T) {
	t.Chdir(t.TempDir())
	initFixture(t, "infra/main.tf", `terraform {
 backend "local" {}
}`)
	initFixture(t, defaultConfigFile, "roots: &selected [old]\nstate-roots: *selected\nexclude-types: *selected # keep old filter\n")
	code, out, stderr := invokeInit(t, "-update", "infra")
	if code != 0 {
		t.Fatalf("%d\n%s%s", code, out, stderr)
	}
	o, err := parseFlags(nil, &bytes.Buffer{})
	if err != nil || !o.exclude.Match("old") || o.exclude.Match("infra") || !slices.Equal(o.roots, []string{"infra"}) {
		t.Fatalf("unrelated alias value changed: %+v %v", o, err)
	}
}

func TestInitColorsOnlyLabels(t *testing.T) {
	t.Chdir(t.TempDir())
	initFixture(t, "infra/main.tf", `terraform {
 backend "local" {}
}`)
	var out bytes.Buffer
	if err := prepareInit(t.Context(), initOptions{dirs: []string{"infra"}, maxRoots: 200}, report.WithColor(&out, true)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\x1b[1;36m== Terraform roots ", "\x1b[1;33mSetup needed:\x1b[0m terraform -chdir=infra init", "\x1b[32mCreated\x1b[0m .tforphans.yaml"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%q", want, out.String())
		}
	}
	raw, _ := os.ReadFile(defaultConfigFile)
	if bytes.Contains(raw, []byte("\x1b")) {
		t.Fatal("colors written to the config")
	}
}

func TestInitPreviewSpellsOutFormatCharacters(t *testing.T) {
	t.Chdir(t.TempDir())
	initFixture(t, "a\u202eb/main.tf", `terraform {
 backend "local" {}
}`)
	initFixture(t, defaultConfigFile, "# note \u202e\nroots: [old]\n")
	code, out, stderr := invokeInit(t, "-update", "-dry-run", "a\u202eb")
	if code != 0 {
		t.Fatalf("%d\n%s%s", code, out, stderr)
	}
	if strings.ContainsAny(out, "\u202e\x1b") || !strings.Contains(out, `a\u202Eb`) {
		t.Fatalf("unsafe preview: %q", out)
	}
}

func TestInitDoesNotPrintControlSequencesInPaths(t *testing.T) {
	t.Chdir(t.TempDir())
	dir := "tenant \x1b[31m"
	initFixture(t, filepath.Join(dir, "main.tf"), `terraform {
 backend "local" {}
}`)
	code, out, stderr := invokeInit(t, dir)
	if code != 0 {
		t.Fatalf("%d\n%s%s", code, out, stderr)
	}
	if strings.Contains(out+stderr, "\x1b") || !strings.Contains(out, "directory with control characters") {
		t.Fatalf("unsafe directory printed: %q %q", out, stderr)
	}
	o, err := parseFlags(nil, &bytes.Buffer{})
	if err != nil || !slices.Equal(o.roots, []string{dir}) {
		t.Fatalf("saved path changed: %+v %v", o, err)
	}
}
