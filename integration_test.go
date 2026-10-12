//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegration runs the whole tool against a real terraform binary and the
// fake provider in testdata/fakeprovider, which supports list resources and
// resource identity without credentials.
func TestIntegration(t *testing.T) {
	data, root := setup(t)
	write(t, filepath.Join(root, "main.tf"), `
terraform {
  required_providers {
    fakeloc = { source = "example.com/fake/fake" }
  }
}
resource "fake_thing" "a" {
  provider = fakeloc
  name     = "managed-a"
}
`)
	terraform(t, root, "apply", "-auto-approve", "-no-color")
	// "a" is also the name of the configured fake_thing.a.
	for _, n := range []string{"orphan-1", "skip-me", "a"} {
		write(t, filepath.Join(data, "thing", n), "")
	}
	write(t, filepath.Join(root, ".tforphans-ignore"), "fake_thing:skip-*\n")

	t.Chdir(root)
	// Extra arguments from the environment must not reach terraform.
	t.Setenv("TF_CLI_ARGS", "-no-such-flag")
	code, stdout, stderr := runTool("adopt", "-exclude-types", "fake_broken")
	if code != exitOK {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "to = fake_thing.orphan_1\n  provider = fakeloc\n") || strings.Contains(stdout, "managed") || strings.Contains(stdout, "skip") {
		t.Errorf("import output:\n%s", stdout)
	}
	if !strings.Contains(stdout, "to = fake_thing.a_2\n") {
		t.Errorf("import into the configured address fake_thing.a:\n%s", stdout)
	}
	if !strings.Contains(stderr, "4 live, 1 managed, 1 ignored, 2 unmanaged") {
		t.Errorf("summary:\n%s", stderr)
	}
	if left, _ := filepath.Glob("*.tfquery.hcl"); len(left) > 0 {
		t.Errorf("query file left behind: %v", left)
	}

	// -o writes a new file and never overwrites one.
	write(t, "notes.txt", "keep")
	if code, _, _ := runTool("adopt", "-exclude-types", "fake_broken", "-o", "notes.txt"); code != exitError {
		t.Errorf("adopt -o over an existing file: exit %d", code)
	}
	if code, _, stderr := runTool("adopt", "-exclude-types", "fake_broken", "-o", "imports.tf"); code != exitOK {
		t.Fatalf("adopt -o: exit %d\n%s", code, stderr)
	}
	if got, _ := os.ReadFile("imports.tf"); string(got) != stdout {
		t.Errorf("adopt -o wrote:\n%s\nwant:\n%s", got, stdout)
	}
	// imports.tf is not applied yet: a second run writes nothing for its
	// objects.
	if code, stdout, stderr := runTool("adopt", "-exclude-types", "fake_broken"); code != exitOK || stdout != "" || !strings.Contains(stderr, "2 objects already have import blocks") {
		t.Errorf("adopt with pending imports: exit %d\n%s\n%s", code, stdout, stderr)
	}

	// The test's own terraform calls below run without the hostile value.
	t.Setenv("TF_CLI_ARGS", "")

	// Import the orphan; afterwards nothing is unmanaged.
	terraform(t, root, "plan", "-generate-config-out=generated.tf", "-no-color")
	terraform(t, root, "apply", "-auto-approve", "-no-color")
	if code, _, stderr := runTool("-exclude-types", "fake_broken"); code != exitOK {
		t.Fatalf("after import: exit %d\n%s", code, stderr)
	}

	if code, _, stderr := runTool("adopt", "-exclude-types", "fake_broken"); code != exitOK || !strings.Contains(stderr, "Nothing to adopt") {
		t.Errorf("adopt with nothing found: exit %d\n%s", code, stderr)
	}

	// A failing list block is a warning, or an error with -strict.
	code, _, stderr = runTool()
	if code != exitOK || !strings.Contains(stderr, "fake_broken not fully listed") {
		t.Errorf("failing list: exit %d\n%s", code, stderr)
	}
	if code, _, _ := runTool("-strict"); code != exitError {
		t.Errorf("-strict with failing list: exit %d", code)
	}

	// A provider crash is an error, even when another block failed on its
	// own and the run would otherwise pass with warnings.
	t.Setenv("FAKE_PANIC", "thing")
	if code, _, stderr := runTool(); code != exitError || !strings.Contains(stderr, "error:") {
		t.Errorf("provider crash: exit %d\n%s", code, stderr)
	}
}

// setup builds the fake provider, points Terraform at it and returns the
// fake data directory and an empty root module directory.
func setup(t *testing.T) (data, root string) {
	t.Helper()
	if _, err := exec.LookPath("terraform"); err != nil {
		t.Skip("terraform not in PATH")
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	data, root = filepath.Join(tmp, "data"), filepath.Join(tmp, "root")
	for _, d := range []string{bin, root, filepath.Join(data, "thing")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	build := exec.Command("go", "build", "-o", filepath.Join(bin, "terraform-provider-fake"), ".")
	build.Dir = "testdata/fakeprovider"
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake provider: %v\n%s", err, out)
	}
	rc := filepath.Join(tmp, "tf.rc")
	write(t, rc, fmt.Sprintf("provider_installation {\n  dev_overrides {\n    \"example.com/fake/fake\" = %q\n    \"example.com/fake2/fake\" = %q\n  }\n  direct {}\n}\n", bin, bin))
	t.Setenv("TF_CLI_CONFIG_FILE", rc)
	t.Setenv("FAKE_DIR", data)
	return data, root
}

// TestIntegrationModuleOnlyProvider covers a provider that only a child
// module declares: the root has no local name for it, so one is added for the
// duration of the run.
func TestIntegrationModuleOnlyProvider(t *testing.T) {
	data, root := setup(t)
	mod := filepath.Join(root, "modules", "inner")
	if err := os.MkdirAll(mod, 0o755); err != nil {
		t.Fatal(err)
	}
	// The root declares a provider of its own: the temporary declaration
	// must merge with it, a second required_providers block would be
	// rejected.
	write(t, filepath.Join(root, "main.tf"), `
terraform {
  required_providers {
    null = { source = "hashicorp/null" }
  }
}
module "inner" {
  source = "./modules/inner"
  count  = 2
  idx    = count.index
}
`)
	write(t, filepath.Join(mod, "main.tf"), `
terraform {
  required_providers {
    fake = { source = "example.com/fake/fake" }
  }
}
variable "idx" {}
resource "fake_thing" "x" {
  for_each = toset(["a", "b"])
  name     = "inner-${var.idx}-${each.key}"
}
`)
	terraform(t, root, "init", "-no-color")
	terraform(t, root, "apply", "-auto-approve", "-no-color")
	write(t, filepath.Join(data, "thing", "hand-made"), "")

	t.Chdir(root)
	code, stdout, stderr := runTool("adopt", "-types", "fake_thing")
	if code != exitOK || !strings.Contains(stderr, "5 live, 4 managed, 0 ignored, 1 unmanaged") {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "to = fake_thing.hand_made\n  # the root module does not declare example.com/fake/fake") {
		t.Errorf("import output:\n%s", stdout)
	}
	if left, _ := filepath.Glob("zz_tforphans_*"); len(left) > 0 {
		t.Errorf("temporary files left behind: %v", left)
	}

	// A run killed with SIGKILL left its provider override behind: the next
	// run removes it before reading the configuration and still works.
	_, printed, _ := runTool("-print-query", "-types", "fake_thing")
	override, _, _ := strings.Cut(strings.SplitN(printed, "\n", 2)[1], "list ")
	if !strings.Contains(override, "required_providers") {
		t.Fatalf("no provider declaration in -print-query:\n%s", printed)
	}
	write(t, "zz_tforphans_99999999_override.tf", override)
	write(t, "zz_tforphans_99999999.tfquery.hcl", "")
	if code, _, stderr := runTool("adopt", "-types", "fake_thing"); code != exitOK || !strings.Contains(stderr, "5 live, 4 managed") {
		t.Errorf("after a killed run: exit %d\n%s", code, stderr)
	}
	if left, _ := filepath.Glob("zz_tforphans_*"); len(left) > 0 {
		t.Errorf("leftovers kept: %v", left)
	}
}

// TestIntegrationStateRoots checks that states found under -state-roots,
// in every workspace, count as managed, and that a root that cannot be read
// is reported.
func TestIntegrationStateRoots(t *testing.T) {
	data, root := setup(t)
	tree := filepath.Join(filepath.Dir(root), "tree")
	other, uninit := filepath.Join(tree, "team", "other"), filepath.Join(tree, "team", "uninit")
	for _, d := range []string{other, uninit, filepath.Join(other, "modules", "m")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	const providers = `
terraform {
  required_providers {
    fake = { source = "example.com/fake/fake" }
  }
}
`
	write(t, filepath.Join(root, "main.tf"), providers+`resource "fake_thing" "mine" { name = "mine" }`)
	write(t, filepath.Join(other, "main.tf"), providers+`
variable "name" { default = "other-default" }
resource "fake_thing" "x" { name = var.name }
`)
	write(t, filepath.Join(other, "modules", "m", "main.tf"), `variable "x" {}`)
	// A root with a backend that was never initialized.
	write(t, filepath.Join(uninit, "main.tf"), "terraform {\n  backend \"local\" {}\n}\n")

	terraform(t, root, "apply", "-auto-approve", "-no-color")
	terraform(t, other, "apply", "-auto-approve", "-no-color")
	terraform(t, other, "workspace", "new", "-no-color", "prod")
	terraform(t, other, "apply", "-auto-approve", "-no-color", "-var", "name=other-prod")
	terraform(t, other, "workspace", "select", "-no-color", "default")
	write(t, filepath.Join(data, "thing", "stray"), "")

	t.Chdir(root)
	code, stdout, stderr := runTool("-types", "fake_thing", "-state-roots", tree)
	if code != exitOrphans || !strings.Contains(stderr, "4 live, 3 managed, 0 ignored, 1 unmanaged; 3 states read") {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "stray") || strings.Contains(stdout, "other-") {
		t.Errorf("orphans:\n%s", stdout)
	}
	if !strings.Contains(stderr, "uninit not read") || !strings.Contains(stderr, "pass -init") {
		t.Errorf("uninitialized root not reported:\n%s", stderr)
	}
	if code, _, _ := runTool("-types", "fake_thing", "-state-roots", tree, "-strict"); code != exitError {
		t.Errorf("-strict with an unreadable state: exit %d", code)
	}
	// The selected workspace of the other root is left alone.
	if ws, _ := os.ReadFile(filepath.Join(other, ".terraform", "environment")); strings.TrimSpace(string(ws)) != "default" {
		t.Errorf("selected workspace changed to %q", ws)
	}

	// A workspace whose state cannot be read does not lose the others.
	write(t, filepath.Join(other, "terraform.tfstate.d", "prod", "terraform.tfstate"), "not json")
	code, stdout, stderr = runTool("-types", "fake_thing", "-state-roots", tree)
	if code != exitOrphans || !strings.Contains(stderr, "4 live, 2 managed, 0 ignored, 2 unmanaged; 2 states read") || !strings.Contains(stderr, "workspace prod") {
		t.Fatalf("broken workspace: exit %d\n%s%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "other-prod") || strings.Contains(stdout, "other-default") {
		t.Errorf("orphans with a broken workspace:\n%s", stdout)
	}

	// Without the tree, the other root's resources are unmanaged.
	if _, stdout, _ := runTool("-types", "fake_thing"); !strings.Contains(stdout, "other-prod") {
		t.Errorf("expected other-prod to be unmanaged without -state-roots:\n%s", stdout)
	}
}

// pveData is what the fake Proxmox VE API serves, keyed by path.
var pveData = map[string]string{
	// pve0 runs no guests now; VM 100 was migrated from it to pve1.
	"/api2/json/nodes":                 `[{"node":"pve0","status":"online","type":"node","ssl_fingerprint":"AA:00"},{"node":"pve1","status":"online","type":"node","ssl_fingerprint":"AA:01"}]`,
	"/api2/json/nodes/pve0/qemu":       `[]`,
	"/api2/json/nodes/pve0/lxc":        `[]`,
	"/api2/json/nodes/pve1/qemu":       `[{"vmid":100,"name":"web","status":"running"},{"vmid":101,"name":"db","status":"stopped"},{"vmid":102,"name":"forgotten","status":"stopped","template":1}]`,
	"/api2/json/nodes/pve1/lxc":        `[{"vmid":200,"name":"dns","status":"running","type":"lxc"},{"vmid":201,"name":"scratch","status":"stopped","type":"lxc"}]`,
	"/api2/json/pools":                 `[{"poolid":"prod"},{"poolid":"old"}]`,
	"/api2/json/access/users":          `[{"userid":"root@pam","enable":1},{"userid":"alice@pve","enable":1}]`,
	"/api2/json/access/groups":         `[{"groupid":"admins"}]`,
	"/api2/json/access/roles":          `[{"roleid":"Administrator","special":1,"privs":"VM.Audit"},{"roleid":"Auditor","special":0,"privs":"VM.Audit"}]`,
	"/api2/json/cluster/ha/groups/":    `[{"group":"g1","type":"group","nodes":"pve1"}]`,
	"/api2/json/cluster/ha/resources/": `[{"sid":"vm:100","type":"vm"}]`,
	"/api2/json/cluster/acme/account/": `[{"name":"default"}]`,
	"/api2/json/cluster/replication":   `[{"id":"100-0","guest":100,"target":"pve2","type":"local","jobnum":0}]`,
	"/api2/json/cluster/sdn/vnets":     `[{"vnet":"vnet1","zone":"z1","type":"vnet"}]`,
	"/api2/json/cluster/backup/":       `[{"id":"backup-1","schedule":"daily","storage":"local","enabled":1}]`,
}

// TestIntegrationProxmox runs the real bpg/proxmox provider (downloaded on
// first use) against a fake Proxmox VE API and checks matching across
// bpg, telmate and deprecated type names.
func TestIntegrationProxmox(t *testing.T) {
	if _, err := exec.LookPath("terraform"); err != nil {
		t.Skip("terraform not in PATH")
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d, ok := pveData[r.URL.Path]
		if !ok {
			t.Errorf("unexpected request %s", r.URL)
			d = "[]"
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":%s}`, d)
	}))
	defer srv.Close()
	t.Setenv("PROXMOX_VE_ENDPOINT", srv.URL)
	t.Setenv("PROXMOX_VE_API_TOKEN", "test@pve!t=00000000-0000-0000-0000-000000000000")
	t.Setenv("PROXMOX_VE_INSECURE", "true")
	// Neither may redirect the scan's state or data directory.
	victim := filepath.Join(t.TempDir(), "prod.tfstate")
	t.Setenv("TF_CLI_ARGS_apply", "-state-out="+victim)
	t.Setenv("TF_DATA_DIR", filepath.Join(t.TempDir(), "data-dir"))

	inst := func(typ string, attrs map[string]any) map[string]any {
		return map[string]any{"mode": "managed", "type": typ, "name": "x", "instances": []any{map[string]any{"attributes": attrs}}}
	}
	st, _ := json.Marshal(map[string]any{"version": 4, "resources": []any{
		inst("proxmox_virtual_environment_vm", map[string]any{"id": "100", "vm_id": 100, "node_name": "pve0"}), // migrated since
		inst("proxmox_vm_qemu", map[string]any{"id": "pve1/qemu/101", "vmid": 101, "target_node": "pve1"}),     // telmate
		inst("proxmox_lxc", map[string]any{"id": "pve1/lxc/200", "vmid": 200, "target_node": "pve1"}),
		inst("proxmox_virtual_environment_pool", map[string]any{"id": "prod", "pool_id": "prod"}),
		inst("proxmox_virtual_environment_hagroup", map[string]any{"id": "g1", "group": "g1"}), // deprecated name
		// VM 102 of another cluster: its node is not one of the scanned
		// cluster's, so it does not manage the scanned VM 102.
		inst("proxmox_virtual_environment_vm", map[string]any{"id": "102", "vm_id": 102, "node_name": "elsewhere"}),
	}})
	statePath := filepath.Join(t.TempDir(), "pve.tfstate")
	write(t, statePath, string(st))

	if code, _, stderr := runTool("-no-query", "-scan", "proxmox", "-state", statePath); code != exitError || !strings.Contains(stderr, "-allow-unpinned") {
		t.Fatalf("scan without a pinned provider: exit %d\n%s", code, stderr)
	}
	code, stdout, stderr := runTool("-no-query", "-scan", "proxmox", "-state", statePath, "-format", "json", "-allow-unpinned")
	if code != exitOrphans {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	var out struct {
		Orphans []struct {
			Type        string         `json:"type"`
			DisplayName string         `json:"display_name"`
			ImportID    string         `json:"import_id"`
			Identity    map[string]any `json:"identity"`
		} `json:"orphans"`
		Summary struct {
			Live, Managed, States int
			Unverified            int `json:"unverified_matches"`
		} `json:"summary"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatal(err)
	}
	if out.Summary.States != 1 {
		t.Errorf("-state not counted: %d states", out.Summary.States)
	}
	var got []string
	for _, o := range out.Orphans {
		got = append(got, o.Type+" "+o.ImportID)
	}
	want := []string{
		"proxmox_acme_account default",
		"proxmox_backup_job backup-1",
		"proxmox_haresource vm:100",
		"proxmox_replication 100-0",
		"proxmox_sdn_vnet vnet1",
		"proxmox_virtual_environment_container pve1/201",
		"proxmox_virtual_environment_group admins",
		"proxmox_virtual_environment_pool old",
		"proxmox_virtual_environment_role Auditor",
		"proxmox_virtual_environment_user alice@pve",
		"proxmox_virtual_environment_vm pve1/102",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("orphans:\n%s\nwant:\n%s\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"), stderr)
	}
	// Nothing proves which cluster the states belong to: all five matches
	// are unverified until the user says so.
	if out.Summary.Live != 16 || out.Summary.Managed != 5 || out.Summary.Unverified != 5 {
		t.Errorf("summary %+v", out.Summary)
	}
	code, stdout, stderr = runTool("-no-query", "-scan", "proxmox", "-state", statePath, "-format", "json", "-allow-unpinned", "-proxmox-cluster-states", "-strict")
	if err := json.Unmarshal([]byte(stdout), &out); err != nil || code != exitOrphans || out.Summary.Managed != 5 || out.Summary.Unverified != 0 {
		t.Errorf("with -proxmox-cluster-states: exit %d %+v %v\n%s", code, out.Summary, err, stderr)
	}
	if _, err := os.Stat(victim); err == nil {
		t.Error("TF_CLI_ARGS_apply reached the scan: state written outside its directory")
	}

	// With a root that pins the provider under its own local name, the
	// version comes from there and import blocks name that provider.
	t.Setenv("TF_CLI_ARGS_apply", "")
	t.Setenv("TF_DATA_DIR", "")
	pinned := filepath.Join(t.TempDir(), "pinned")
	if err := os.MkdirAll(pinned, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(pinned, "versions.tf"), "terraform {\n  required_providers {\n    pve = { source = \"bpg/proxmox\" }\n  }\n}\n")
	terraform(t, pinned, "init", "-no-color")
	code, stdout, stderr = runTool("adopt", "-no-query", "-scan", "proxmox", "-state", statePath, pinned)
	if code != exitOK || !strings.Contains(stdout, "to = proxmox_virtual_environment_vm.forgotten\n  provider = pve\n  # found with the proxmox provider configured from the environment;\n") {
		t.Errorf("adopt with a pinned root: exit %d\n%s\n%s", code, stdout, stderr)
	}
}

// TestIntegrationUserListAlias checks that objects found by the user's own
// list block keep that block's provider (an alias) in their import blocks.
func TestIntegrationUserListAlias(t *testing.T) {
	data, root := setup(t)
	write(t, filepath.Join(root, "main.tf"), `
terraform {
  required_providers {
    fake = { source = "example.com/fake/fake" }
  }
}
provider "fake" {
  alias = "second"
}
`)
	write(t, filepath.Join(root, "mine.tfquery.hcl"), `
list "fake_thing" "tfo_aliased" {
  provider = fake.second
}
`)
	write(t, filepath.Join(data, "thing", "via-alias"), "")
	t.Chdir(root)
	// fake has only an aliased configuration, so no generated blocks: only
	// the user's block lists; its label starts with tfo_ like generated
	// ones, and it is still the user's.
	code, stdout, stderr := runTool("adopt")
	if code != exitOK || !strings.Contains(stdout, "to = fake_thing.via_alias\n  provider = fake.second\n") {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	write(t, "imports.tf", stdout)
	terraform(t, root, "plan", "-generate-config-out=generated.tf", "-no-color")

	// -types narrows the user's blocks too.
	if code, stdout, stderr := runTool("-types", "fake_zoned"); code != exitOK || strings.Contains(stdout, "fake_thing") {
		t.Errorf("-types did not narrow the user's block: exit %d\n%s\n%s", code, stdout, stderr)
	}

	code, _, stderr = runTool("-types", "fake_thing")
	if code != exitOrphans || !strings.Contains(stderr, "note: .: provider fake is configured only with aliases") || !strings.Contains(stderr, "1 resource types queried") {
		t.Errorf("alias-only provider: exit %d\n%s", code, stderr)
	}
}

// TestIntegrationSameTypeTwoProviders: two providers (like google and
// google-beta) list the same resource type; each gets its own list block.
func TestIntegrationSameTypeTwoProviders(t *testing.T) {
	data, root := setup(t)
	write(t, filepath.Join(root, "main.tf"), `
terraform {
  required_providers {
    fake  = { source = "example.com/fake/fake" }
    fakeb = { source = "example.com/fake2/fake" }
  }
}
`)
	write(t, filepath.Join(data, "thing", "x"), "")
	t.Chdir(root)
	// Both providers list x: for two provider sources these are two objects.
	code, stdout, stderr := runTool("-types", "fake_thing")
	if code != exitOrphans || strings.Count(stdout, "fake_thing") != 2 || !strings.Contains(stderr, "2 resource types queried") {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}

	// State of one provider does not manage the other provider's object.
	write(t, filepath.Join(root, "main.tf"), `
terraform {
  required_providers {
    fake  = { source = "example.com/fake/fake" }
    fakeb = { source = "example.com/fake2/fake" }
  }
}
resource "fake_thing" "x" {
  provider = fake
  name     = "x"
}
`)
	terraform(t, root, "apply", "-auto-approve", "-no-color")
	code, stdout, stderr = runTool("-types", "fake_thing", "-format", "json")
	if code != exitOrphans || !strings.Contains(stderr, "2 live, 1 managed, 0 ignored, 1 unmanaged") || !strings.Contains(stdout, `"provider_source": "example.com/fake2/fake"`) {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
}

func runTool(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func terraform(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("terraform", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("terraform %v: %v\n%s", args, err, tail(string(out)))
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func tail(s string) string {
	if len(s) > 2000 {
		return "..." + s[len(s)-2000:]
	}
	return s
}

// TestIntegrationGCPFeedback covers a large GCP run's findings: a backend
// warning before the workspace list, a root pinned to an older Terraform,
// attributes for ignore rules and output, and runs that query nothing.
func TestIntegrationGCPFeedback(t *testing.T) {
	data, root := setup(t)
	write(t, filepath.Join(root, "main.tf"), `
terraform {
  required_providers {
    fake = { source = "example.com/fake/fake" }
  }
}
resource "fake_thing" "a" { name = "managed-a" }
`)
	terraform(t, root, "apply", "-auto-approve", "-no-color")
	for _, n := range []string{"orphan-1", "node-1"} {
		write(t, filepath.Join(data, "thing", n), "")
	}
	t.Chdir(root)

	// A wrapper that prints a backend warning to stdout, as the S3 backend
	// does for dynamodb_table with -no-color, and logs the workspaces.
	tfPath, err := exec.LookPath("terraform")
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "calls")
	wrapper := filepath.Join(t.TempDir(), "tfwrap")
	write(t, wrapper, fmt.Sprintf(`#!/bin/sh
echo "$* ws=${TF_WORKSPACE:-}" >> %q
if [ "$1 $2" = "workspace list" ]; then
  printf '\nWarning: Deprecated Parameter\n\nThe parameter "dynamodb_table" is deprecated. Use parameter "use_lockfile"\ninstead.\n\n'
fi
exec %q "$@"
`, log, tfPath))
	os.Chmod(wrapper, 0o755)
	code, stdout, stderr := runTool("-terraform", wrapper, "-types", "fake_thing")
	if code != exitOrphans || !strings.Contains(stderr, "3 live, 1 managed") {
		t.Fatalf("with a backend warning: exit %d\n%s%s", code, stdout, stderr)
	}
	calls, _ := os.ReadFile(log)
	if strings.Contains(string(calls), "instead.") || !strings.Contains(string(calls), "state pull -no-color ws=default") {
		t.Errorf("terraform calls:\n%s", calls)
	}
	// -current-workspace does not list workspaces at all.
	os.Remove(log)
	if code, _, stderr := runTool("-terraform", wrapper, "-types", "fake_thing", "-current-workspace"); code != exitOrphans {
		t.Errorf("-current-workspace: exit %d\n%s", code, stderr)
	}
	if calls, _ := os.ReadFile(log); strings.Contains(string(calls), "workspace list") || !strings.Contains(string(calls), "state pull -no-color ws=\n") {
		t.Errorf("-current-workspace calls:\n%s", calls)
	}

	// Attributes: for -include-attrs, and for ignore rules on attributes.
	write(t, ".tforphans-ignore", "fake_thing attr.name=node-*\n")
	code, stdout, stderr = runTool("-types", "fake_thing", "-format", "json", "-include-attrs", "id")
	if code != exitOrphans || !strings.Contains(stderr, "1 ignored") {
		t.Fatalf("attributes: exit %d\n%s%s", code, stdout, stderr)
	}
	var out struct {
		Orphans []struct {
			DisplayName string         `json:"display_name"`
			Provider    *string        `json:"provider"`
			Attributes  map[string]any `json:"attributes"`
		} `json:"orphans"`
		Summary struct {
			ByType map[string]struct{ Live, Orphans int } `json:"by_type"`
		} `json:"summary"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatal(err)
	}
	for _, o := range out.Orphans {
		// id was asked for; name, which only the ignore rule looks at, is
		// not printed.
		if o.Provider == nil || o.Attributes["id"] != o.DisplayName || len(o.Attributes) != 1 {
			t.Errorf("orphan %+v", o)
		}
	}
	if bt := out.Summary.ByType["fake_thing"]; bt.Live != 3 || bt.Orphans != 1 || len(out.Orphans) != 1 {
		t.Errorf("by_type %+v", out.Summary.ByType)
	}
	code, stdout, _ = runTool("-types", "fake_thing", "-format", "summary")
	if code != exitOrphans || !strings.Contains(stdout, "fake_thing") || !strings.Contains(stdout, "UNMANAGED") {
		t.Errorf("-format summary: exit %d\n%s", code, stdout)
	}
	os.Remove(".tforphans-ignore")

	// The user's own list block without include_resource: a note.
	write(t, "mine.tfquery.hcl", "list \"fake_thing\" \"mine\" {\n  provider = fake\n}\n")
	if code, _, stderr := runTool("-types", "fake_thing", "-include-attrs", "id"); code != exitOrphans || !strings.Contains(stderr, "list.fake_thing.mine has no include_resource") {
		t.Errorf("user block without include_resource: exit %d\n%s", code, stderr)
	}
	os.Remove("mine.tfquery.hcl")

	// Nothing queried is an error, not "nothing found"; so is a user's list
	// block that runs no instance.
	if code, _, stderr := runTool("-types", "no_such_type"); code != exitError || !strings.Contains(stderr, "no resource types were queried") {
		t.Errorf("nothing queried: exit %d\n%s", code, stderr)
	}
	write(t, "empty.tfquery.hcl", "list \"fake_thing\" \"none\" {\n  provider = fake\n  for_each = {}\n}\n")
	write(t, ".backup.tfquery.hcl", "this is not { valid hcl")
	if code, _, stderr := runTool("-types", "no_such_type"); code != exitError || !strings.Contains(stderr, "no resource types were queried") {
		t.Errorf("empty user block: exit %d\n%s", code, stderr)
	}
	os.Remove("empty.tfquery.hcl")
	os.Remove(".backup.tfquery.hcl")

	// A root pinned to an older Terraform.
	write(t, filepath.Join(root, "versions.tf"), "terraform {\n  required_version = \"= 1.10.5\"\n}\n")
	if code, _, stderr := runTool("-types", "fake_thing"); code != exitError || !strings.Contains(stderr, "Unsupported Terraform Core version") {
		t.Errorf("pinned version: exit %d\n%s", code, stderr)
	}
	if code, _, stderr := runTool("-types", "fake_thing", "-ignore-required-version"); code != exitOrphans {
		t.Errorf("-ignore-required-version: exit %d\n%s", code, stderr)
	}
	if left, _ := filepath.Glob("zz_tforphans_*"); len(left) > 0 {
		t.Errorf("files left behind: %v", left)
	}
}
