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
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/report"
	"github.com/5xxeu/tforphans/internal/tf"
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
	if !strings.Contains(stderr, "4 live, 1 managed, 0 uncertain, 1 ignored, 2 unmanaged") {
		t.Errorf("summary:\n%s", stderr)
	}
	if left, _ := filepath.Glob("*.tfquery.hcl"); len(left) > 0 {
		t.Errorf("query file left behind: %v", left)
	}
	// The fake provider tags each object "Tagged <name>".
	if code, stdout, stderr := runTool("adopt", "-exclude-types", "fake_broken", "-names-from-tags"); code != exitOK || !strings.Contains(stdout, "to = fake_thing.tagged_orphan_1\n") || strings.Contains(stdout, "Tagged") {
		t.Errorf("-names-from-tags: exit %d\n%s\n%s", code, stdout, stderr)
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
	if before116(t) {
		// What the adopt hint asks for with older Terraform: the provider
		// moves from the import blocks into the generated resources.
		imports, _ := os.ReadFile("imports.tf")
		write(t, "imports.tf", regexp.MustCompile(`(?m)^  provider = .*\n`).ReplaceAllString(string(imports), ""))
		generated, _ := os.ReadFile("generated.tf")
		write(t, "generated.tf", regexp.MustCompile(`(?m)^(resource "fake_thing" .*\{\n)`).ReplaceAllString(string(generated), "${1}  provider = fakeloc\n"))
	}
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
	if code != exitOK || !strings.Contains(stderr, "5 live, 4 managed, 0 uncertain, 0 ignored, 1 unmanaged") {
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
	_, override, _ := strings.Cut(printed, "# file zz_tforphans_<pid>_override.tf\n")
	override, _, _ = strings.Cut(override, "\n# file ")
	if !strings.Contains(override, "required_providers") || !strings.Contains(printed, "# file zz_tforphans_<pid>.tfquery.hcl\n") {
		t.Fatalf("no provider declaration in -print-query:\n%s", printed)
	}
	write(t, "zz_tforphans_99999999_override.tf", "# tforphans temporary file, run 99999999-test\n"+override)
	write(t, "zz_tforphans_99999999.tfquery.hcl", "# tforphans temporary file, run 99999999-test\n")
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
	if code != exitOrphans || !strings.Contains(stderr, "4 live, 3 managed, 0 uncertain, 0 ignored, 1 unmanaged; 3 states read") {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "stray") || strings.Contains(stdout, "other-") {
		t.Errorf("orphans:\n%s", stdout)
	}
	if !strings.Contains(stderr, "uninit: not initialized") || !strings.Contains(stderr, "pass -init") {
		t.Errorf("uninitialized root not reported:\n%s", stderr)
	}
	if code, _, _ := runTool("-types", "fake_thing", "-state-roots", tree, "-strict"); code != exitError {
		t.Errorf("-strict with an unreadable state: exit %d", code)
	}
	if !strings.Contains(stderr, "coverage: INCOMPLETE (1 states not read: ") {
		t.Errorf("coverage verdict:\n%s", stderr)
	}
	// adopt would import the objects of the unread state a second time.
	if code, stdout, stderr := runTool("adopt", "-types", "fake_thing", "-state-roots", tree); code != exitError || stdout != "" || !strings.Contains(stderr, "-allow-incomplete") {
		t.Errorf("adopt with an unread state: exit %d\n%s%s", code, stdout, stderr)
	}
	code, stdout, stderr = runTool("adopt", "-types", "fake_thing", "-state-roots", tree, "-allow-incomplete")
	if code != exitOK || !strings.Contains(stdout, "# Objects that none of the 3 states read manages.") || !strings.Contains(stdout, "to = fake_thing.stray") {
		t.Errorf("adopt -allow-incomplete: exit %d\n%s%s", code, stdout, stderr)
	}
	// The selected workspace of the other root is left alone.
	if ws, _ := os.ReadFile(filepath.Join(other, ".terraform", "environment")); strings.TrimSpace(string(ws)) != "default" {
		t.Errorf("selected workspace changed to %q", ws)
	}

	// A workspace whose state cannot be read does not lose the others.
	write(t, filepath.Join(other, "terraform.tfstate.d", "prod", "terraform.tfstate"), "not json")
	code, stdout, stderr = runTool("-types", "fake_thing", "-state-roots", tree)
	if code != exitOrphans || !strings.Contains(stderr, "4 live, 2 managed, 0 uncertain, 0 ignored, 2 unmanaged; 2 states read") || !strings.Contains(stderr, "workspace prod") {
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
	"/api2/json/version":               `{"version":"8.2.4","release":"8.2","repoid":"faa83925c9641325"}`,
}

// TestIntegrationProxmox runs the real bpg/proxmox provider (downloaded on
// first use) against a fake Proxmox VE API and checks matching across
// bpg, telmate and deprecated type names.
func TestIntegrationProxmox(t *testing.T) {
	if _, err := exec.LookPath("terraform"); err != nil {
		t.Skip("terraform not in PATH")
	}
	// fail maps paths ("" for all) to a status line Proxmox VE would send:
	// it puts the error into the reason phrase.
	var mu sync.Mutex
	fail := map[string]string{}
	requests := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		status, failed := fail[r.URL.Path]
		if all, ok := fail[""]; ok {
			status, failed = all, true
		}
		mu.Unlock()
		if failed {
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			fmt.Fprintf(buf, "HTTP/1.1 %s\r\nContent-Type: application/json\r\nContent-Length: 13\r\nConnection: close\r\n\r\n{\"data\":null}", status)
			buf.Flush()
			return
		}
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
			Uncertain             int `json:"uncertain"`
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
	// are uncertain until the user says so.
	if out.Summary.Live != 16 || out.Summary.Managed != 0 || out.Summary.Uncertain != 5 {
		t.Errorf("summary %+v", out.Summary)
	}
	code, stdout, stderr = runTool("-no-query", "-scan", "proxmox", "-state", statePath, "-format", "json", "-allow-unpinned", "-proxmox-cluster-states", "-strict")
	if err := json.Unmarshal([]byte(stdout), &out); err != nil || code != exitOrphans || out.Summary.Managed != 5 || out.Summary.Uncertain != 0 {
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

	// Proxmox VE 9 answers HA groups with 500: skipped, not a gap.
	mu.Lock()
	fail["/api2/json/cluster/ha/groups/"] = "500 ha groups have been migrated to rules"
	mu.Unlock()
	code, _, stderr = runTool("-no-query", "-scan", "proxmox", "-state", statePath, "-format", "json", "-allow-unpinned", "-proxmox-cluster-states", "-strict", "-v")
	if code != exitOrphans || !strings.Contains(stderr, "coverage: complete") || !strings.Contains(stderr, "HA rules instead of HA groups") {
		t.Errorf("PVE 9 HA groups: exit %d\n%s", code, stderr)
	}
	// Rejected credentials: one request, then exit 1.
	mu.Lock()
	fail[""] = "401 invalid token value!"
	requests = 0
	mu.Unlock()
	code, _, stderr = runTool("-no-query", "-scan", "proxmox", "-state", statePath, "-allow-unpinned")
	mu.Lock()
	n := requests
	mu.Unlock()
	if code != exitError || !strings.Contains(stderr, "authentication failed") || n > 2 {
		t.Errorf("401: exit %d, %d requests\n%s", code, n, stderr)
	}
	// With a user and password the login (a ticket) fails when the
	// provider is configured, before any data source.
	t.Setenv("PROXMOX_VE_API_TOKEN", "")
	t.Setenv("PROXMOX_VE_USERNAME", "root@pam")
	t.Setenv("PROXMOX_VE_PASSWORD", "wrong")
	mu.Lock()
	fail[""] = "401 authentication failure"
	requests = 0
	mu.Unlock()
	code, _, stderr = runTool("-no-query", "-scan", "proxmox", "-state", statePath, "-allow-unpinned")
	mu.Lock()
	n = requests
	mu.Unlock()
	if code != exitError || !strings.Contains(stderr, "authentication failed") || n > 2 {
		t.Errorf("401 for a password: exit %d, %d requests\n%s", code, n, stderr)
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
	// fake has only an aliased configuration: generated blocks list through
	// it, next to the user's block, whose label starts with tfo_ like
	// generated ones and which is still the user's.
	code, stdout, stderr := runTool("adopt")
	if code != exitOK || !strings.Contains(stdout, "to = fake_thing.via_alias\n  provider = fake.second\n") {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	// The user's block returns no objects, so no Name tag: a note says why.
	if _, _, tagErr := runTool("adopt", "-names-from-tags"); !strings.Contains(tagErr, "list.fake_thing.tfo_aliased has no include_resource = true") || !strings.Contains(tagErr, "-names-from-tags") {
		t.Errorf("-names-from-tags with a user block:\n%s", tagErr)
	}
	write(t, "imports.tf", stdout)
	terraform(t, root, "plan", "-generate-config-out=generated.tf", "-no-color")

	// -types narrows the user's blocks too.
	if code, stdout, stderr := runTool("-types", "fake_zoned"); code != exitOK || strings.Contains(stdout, "fake_thing") {
		t.Errorf("-types did not narrow the user's block: exit %d\n%s\n%s", code, stdout, stderr)
	}

	code, _, stderr = runTool("-types", "fake_thing")
	if code != exitOrphans || !strings.Contains(stderr, "note: .: provider fake is configured only with aliases, so its list-capable types were queried through fake.second") || !strings.Contains(stderr, "2 resource types queried") {
		t.Errorf("alias-only provider: exit %d\n%s", code, stderr)
	}
	// fake_thing is listed; fake_zoned, skipped for the alias (it needs a
	// zone), is a gap that -strict does not pass.
	if code, _, stderr := runTool("-types", "fake_thing", "-strict"); code != exitOrphans || !strings.Contains(stderr, "coverage: complete") {
		t.Errorf("alias-only provider, type covered by the user's block: exit %d\n%s", code, stderr)
	}
	if code, _, stderr := runTool("-types", "fake_thing,fake_zoned", "-strict"); code != exitError || !strings.Contains(stderr, "coverage: INCOMPLETE (1 types skipped that need an argument") {
		t.Errorf("alias-only provider, type not covered: exit %d\n%s", code, stderr)
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
	if code != exitOrphans || !strings.Contains(stderr, "2 live, 1 managed, 0 uncertain, 0 ignored, 1 unmanaged") || !strings.Contains(stdout, `"provider_source": "example.com/fake2/fake"`) {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
}

func runTool(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// before116 reports a Terraform older than 1.16 on PATH.
func before116(t *testing.T) bool {
	t.Helper()
	out, err := exec.Command("terraform", "version", "-json").Output()
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Version string `json:"terraform_version"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatal(err)
	}
	var major, minor int
	fmt.Sscanf(v.Version, "%d.%d", &major, &minor)
	return major == 1 && minor < 16
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

// TestIntegrationTwoRootsCut: a listing cut short in one of two roots makes
// every row of its type incomplete, also those with objects it returned.
func TestIntegrationTwoRootsCut(t *testing.T) {
	data, root := setup(t)
	second := filepath.Join(filepath.Dir(root), "second")
	if err := os.MkdirAll(second, 0o755); err != nil {
		t.Fatal(err)
	}
	conf := "terraform {\n  required_providers {\n    fake = { source = \"example.com/fake/fake\" }\n  }\n}\n"
	write(t, filepath.Join(root, "main.tf"), conf)
	write(t, filepath.Join(second, "main.tf"), conf)
	for _, n := range []string{"a", "b", "c"} {
		write(t, filepath.Join(data, "thing", n), "")
	}
	t.Chdir(filepath.Dir(root))
	code, out, stderr := runTool("-types", "fake_thing", "-limit", "2", "-format", "json", "root", "second")
	if code != exitOrphans {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	var rep struct {
		Summary report.Summary `json:"summary"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	var rows []string
	for _, c := range rep.Summary.Coverage {
		if c.Type == "fake_thing" {
			rows = append(rows, fmt.Sprintf("%q %s %d", c.Scope, c.Status, c.Live))
		}
	}
	if rep.Summary.Complete || len(rows) == 0 || slices.ContainsFunc(rows, func(r string) bool { return !strings.Contains(r, " incomplete ") || strings.Contains(r, "root ") }) {
		t.Errorf("coverage %v, complete %v\n%s", rows, rep.Summary.Complete, stderr)
	}
}

// TestIntegrationUninitializedRoot: a root that needs terraform init does
// not stop the run; the others are still queried, and the gap shows.
func TestIntegrationUninitializedRoot(t *testing.T) {
	data, root := setup(t)
	broken := filepath.Join(filepath.Dir(root), "broken")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "main.tf"), "terraform {\n  required_providers {\n    fake = { source = \"example.com/fake/fake\" }\n  }\n}\n")
	write(t, filepath.Join(broken, "main.tf"), "terraform {\n  required_providers {\n    null = { source = \"hashicorp/null\" }\n  }\n}\n")
	// Providers from dev_overrides need no init, a backend does: the
	// schema may work and reading the state fail.
	backend := filepath.Join(filepath.Dir(root), "backend")
	if err := os.MkdirAll(backend, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(backend, "main.tf"), "terraform {\n  backend \"local\" {\n    path = \"other.tfstate\"\n  }\n  required_providers {\n    fake = { source = \"example.com/fake/fake\" }\n  }\n}\n")
	write(t, filepath.Join(data, "thing", "a"), "")
	t.Chdir(filepath.Dir(root))
	code, stdout, stderr := runTool("-types", "fake_thing", "broken", "backend", "root")
	if code != exitOrphans || !strings.Contains(stdout, "fake_thing") || !strings.Contains(stderr, "broken: not initialized") ||
		!strings.Contains(stderr, "backend: not initialized") || !strings.Contains(stderr, "coverage: INCOMPLETE") {
		t.Errorf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	if code, _, _ := runTool("-types", "fake_thing", "-strict", "broken", "backend", "root"); code != exitError {
		t.Errorf("-strict with an uninitialized root: exit %d", code)
	}
}

// TestIntegrationRootThroughLink: jump/../root, with jump a link, is the
// root the system takes it for, also for the temporary query files.
func TestIntegrationRootThroughLink(t *testing.T) {
	data, root := setup(t)
	tmp := filepath.Dir(root)
	work := filepath.Join(tmp, "work")
	for _, d := range []string{work, filepath.Join(tmp, "sub")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(tmp, "sub"), filepath.Join(work, "jump")); err != nil {
		t.Skip(err)
	}
	write(t, filepath.Join(root, "main.tf"), "terraform {\n  required_providers {\n    fake = { source = \"example.com/fake/fake\" }\n  }\n}\n")
	write(t, filepath.Join(data, "thing", "a"), "")
	t.Chdir(work)
	if code, stdout, stderr := runTool("-types", "fake_thing", "jump/../root"); code != exitOrphans || !strings.Contains(stdout, "fake_thing") {
		t.Errorf("exit %d\n%s\n%s", code, stdout, stderr)
	}
}

// TestIntegrationAPIOff: a listing whose API is not enabled (GCP) is a
// gap unless -strict-api=false or -strict-ignore.
func TestIntegrationAPIOff(t *testing.T) {
	_, root := setup(t)
	write(t, filepath.Join(root, "main.tf"), "terraform {\n  required_providers {\n    fake = { source = \"example.com/fake/fake\" }\n  }\n}\n")
	t.Chdir(root)
	t.Setenv("FAKE_FAIL", "thing:Compute Engine API has not been used in project p before or it is disabled. Enable it by visiting ...")
	// One type listed, the other's API is off: a disabled API can still
	// hold data, so -strict fails.
	code, stdout, stderr := runTool("-types", "fake_thing,fake_zoned", "-arg", "zone=a", "-strict", "-format", "json")
	if code != exitError || !strings.Contains(stderr, "1 listings failed because their API is not enabled") {
		t.Errorf("exit %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, `"api_not_enabled": 1`) || !strings.Contains(stdout, `"reasons": [`) || !strings.Contains(stdout, `"complete": false`) {
		t.Errorf("json:\n%s", stdout)
	}
	// Exempted explicitly: by type, or every disabled API as before.
	if code, _, stderr := runTool("-types", "fake_thing,fake_zoned", "-arg", "zone=a", "-strict", "-strict-ignore", "fake_thing"); code != exitOK {
		t.Errorf("-strict-ignore: exit %d\n%s", code, stderr)
	}
	if code, _, stderr := runTool("-types", "fake_thing,fake_zoned", "-arg", "zone=a", "-strict", "-strict-api=false"); code != exitOK || !strings.Contains(stderr, "1 listings whose API is not enabled (-strict-api=false left them out)") {
		t.Errorf("-strict-api=false: exit %d\n%s", code, stderr)
	}
	// A provider with only an alias: the user's block with that alias meets
	// the disabled API, which covers the alias-only skip of fake_thing.
	write(t, "main.tf", "terraform {\n  required_providers {\n    fake = { source = \"example.com/fake/fake\" }\n  }\n}\nprovider \"fake\" {\n  alias = \"second\"\n}\n")
	write(t, "mine.tfquery.hcl", "list \"fake_thing\" \"mine\" {\n  provider = fake.second\n}\n")
	if code, _, stderr := runTool("-types", "fake_thing", "-strict", "-strict-api=false"); code != exitOK {
		t.Errorf("aliased block with the API off: exit %d\n%s", code, stderr)
	}
}

// TestIntegrationStrictIgnore: failures and missing arguments of types
// named in -strict-ignore are reported but are no gap.
func TestIntegrationStrictIgnore(t *testing.T) {
	_, root := setup(t)
	write(t, filepath.Join(root, "main.tf"), "terraform {\n  required_providers {\n    fake = { source = \"example.com/fake/fake\" }\n  }\n}\n")
	t.Chdir(root)
	t.Setenv("FAKE_FAIL", "thing:Error 403: permission denied on resource")
	code, _, stderr := runTool("-types", "fake_thing,fake_zoned", "-strict")
	if code != exitError || !strings.Contains(stderr, "1 failed or truncated listings, 1 types skipped that need an argument (-arg NAME=...") {
		t.Errorf("exit %d\n%s", code, stderr)
	}
	code, stdout, stderr := runTool("-types", "fake_thing,fake_zoned", "-strict", "-strict-ignore", "fake_thing,fake_z*", "-format", "json")
	if code != exitOK || !strings.Contains(stderr, "2 failed listings or skipped types of -strict-ignore") || !strings.Contains(stderr, "permission denied") {
		t.Errorf("-strict-ignore: exit %d\n%s", code, stderr)
	}
	for _, want := range []string{`"strict_ignored": 2`, `"-strict-ignore: permission denied"`, `"failed_queries"`} {
		if !strings.Contains(stdout, want) {
			t.Errorf("json lacks %s:\n%s", want, stdout)
		}
	}
	var out struct {
		Summary report.Summary `json:"summary"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatal(err)
	}
	if sk := out.Summary.Skipped; len(sk) != 1 || sk[0].Type != "fake_zoned" || !sk[0].Excluded || sk[0].Reason != "-strict-ignore: list config requires zone" {
		t.Errorf("skipped_types %+v", sk)
	}
	// Only the types named.
	if code, _, stderr := runTool("-types", "fake_thing,fake_zoned", "-strict", "-strict-ignore", "fake_thing"); code != exitError || !strings.Contains(stderr, "coverage: INCOMPLETE (1 types skipped that need an argument") {
		t.Errorf("-strict-ignore one type: exit %d\n%s", code, stderr)
	}
}

// TestIntegrationAutoZones: -arg zone=auto reads every region's zones with
// data sources through the root's provider and fans the query out over
// them. The fake provider stands in for google: its zones data source plays
// the regions, its records data source the zones of one region.
func TestIntegrationAutoZones(t *testing.T) {
	data, root := setup(t)
	saved := tf.AutoSources
	tf.AutoSources = map[string]tf.AutoSource{"fake": {Regions: "fake_zones", RegionsAttr: "result[*].id", Zones: "fake_records", ZoneArg: "zone_id", Attr: "result[*].id"}}
	t.Cleanup(func() { tf.AutoSources = saved })
	write(t, filepath.Join(root, "main.tf"), "terraform {\n  required_providers {\n    fake = { source = \"example.com/fake/fake\" }\n  }\n}\n")
	for _, f := range []string{"zone/r1", "zone/r2", "record/r1/z1a", "record/r1/z1b", "record/r2/z2a", "zoned/z1a/a", "zoned/z1b/b", "zoned/z2a/c", "zoned/other/d", "thing/t"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(data, f)), 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(data, f), "")
	}
	t.Chdir(root)
	code, stdout, stderr := runTool("-types", "fake_thing,fake_zoned", "-arg", "fake_zoned:zone=auto", "-strict", "-format", "json")
	if code != exitOrphans {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	for _, z := range []string{"z1a", "z1b", "z2a"} {
		if !strings.Contains(stdout, `list.fake_zoned.tfo_fake_fake_zoned[\"zone=`+z+`\"]`) {
			t.Errorf("zone %s not queried:\n%s", z, stdout)
		}
	}
	if strings.Contains(stdout, "zone=other") {
		t.Errorf("a zone of no region was queried:\n%s", stdout)
	}
	if code, stdout, _ := runTool("-types", "fake_zoned", "-arg", "zone=auto", "-print-query"); code != exitOK || !strings.Contains(stdout, `data "fake_records" "tfo_auto_fake"`) || !strings.Contains(stdout, "[for v in flatten([for r in data.fake_records.tfo_auto_fake : r.result[*].id]) : [v]]") {
		t.Errorf("-print-query: exit %d\n%s", code, stdout)
	}
	if files, _ := filepath.Glob("zz_tforphans_*"); len(files) > 0 {
		t.Errorf("temporary files left: %v", files)
	}
	// Zones that cannot be read: their queries fail, the others run. A
	// disabled API is a gap too (it may still hold data), unless
	// -strict-api=false takes it as empty.
	t.Setenv("FAKE_FAIL", "record:Compute Engine API has not been used in project p before or it is disabled")
	if code, stdout, stderr := runTool("-types", "fake_zoned", "-arg", "zone=auto", "-strict"); code != exitError || strings.Contains(stderr, "coverage: complete") {
		t.Errorf("only zones with the API off: exit %d\n%s\n%s", code, stdout, stderr)
	}
	// Nothing else runs: with -strict-api=false the disabled API still is
	// the check.
	if code, stdout, stderr := runTool("-types", "fake_zoned", "-arg", "zone=auto", "-strict", "-strict-api=false"); code != exitOK || !strings.Contains(stderr, "coverage: complete") {
		t.Errorf("only zones with the API off, -strict-api=false: exit %d\n%s\n%s", code, stdout, stderr)
	}
	if code, stdout, stderr := runTool("-types", "fake_thing,fake_zoned", "-arg", "fake_zoned:zone=auto", "-strict", "-strict-api=false"); code != exitOrphans || !strings.Contains(stdout, "fake_thing") || !strings.Contains(stderr, "coverage: complete") {
		t.Errorf("zones with the API off, -strict-api=false: exit %d\n%s\n%s", code, stdout, stderr)
	}
	// A data source of the user's whose label looks like ours is not ours:
	// its error stays fatal.
	write(t, "mine.tf", "data \"fake_zones\" \"tfo_auto_custom\" {}\n")
	write(t, "mine.tfquery.hcl", "list \"fake_zoned\" \"mine\" {\n  provider = fake\n  for_each = toset(data.fake_zones.tfo_auto_custom.result[*].id)\n  config {\n    zone = each.key\n  }\n}\n")
	t.Setenv("FAKE_FAIL", "zone:Error 403: permission denied")
	if code, _, stderr := runTool("-types", "fake_thing", "-strict"); code != exitError || !strings.Contains(stderr, "data.fake_zones.tfo_auto_custom") {
		t.Errorf("user's data source: exit %d\n%s", code, stderr)
	}
	os.Remove("mine.tf")
	os.Remove("mine.tfquery.hcl")
	if code, stdout, stderr := runTool("-types", "fake_thing,fake_zoned", "-arg", "fake_zoned:zone=auto", "-strict"); code != exitError || !strings.Contains(stdout, "fake_thing") || !strings.Contains(stderr, "1 failed or truncated listings") {
		t.Errorf("regions not readable: exit %d\n%s\n%s", code, stdout, stderr)
	}
}

// TestIntegrationTerragrunt reads the state of a Terragrunt unit under
// -state-roots with -terragrunt (needs terragrunt 0.73+ in PATH).
func TestIntegrationTerragrunt(t *testing.T) {
	if _, err := exec.LookPath("terragrunt"); err != nil {
		t.Skip("terragrunt not in PATH")
	}
	if _, err := exec.LookPath("terraform"); err != nil {
		t.Skip("terraform not in PATH")
	}
	live := filepath.Join(t.TempDir(), "live")
	files := map[string]string{
		"root.hcl": `remote_state {
  backend  = "local"
  generate = {
    path      = "backend.tf"
    if_exists = "overwrite"
  }
  config = {
    path = "${get_parent_terragrunt_dir()}/../state/${path_relative_to_include()}/terraform.tfstate"
  }
}
`,
		"app/terragrunt.hcl":     "include \"root\" {\n  path = find_in_parent_folders(\"root.hcl\")\n}\nterraform {\n  source = \"../../modules/app\"\n}\n",
		"../modules/app/main.tf": "resource \"terraform_data\" \"x\" {\n  input = \"hello\"\n}\n",
	}
	for name, body := range files {
		p := filepath.Join(live, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, p, body)
	}
	apply := exec.Command("terragrunt", "apply", "-auto-approve", "--non-interactive")
	apply.Dir = filepath.Join(live, "app")
	if out, err := apply.CombinedOutput(); err != nil {
		t.Fatalf("terragrunt apply: %v\n%s", err, tail(string(out)))
	}
	// -strict: nothing unread, coverage complete; adopt writes.
	code, _, stderr := runTool("-no-query", "-plugin", "true", "-state-roots", live, "-terragrunt", "-strict")
	if code != exitOK || !strings.Contains(stderr, "1 states read") || strings.Contains(stderr, "not read") {
		t.Errorf("exit %d\n%s", code, stderr)
	}
	if code, _, stderr := runTool("adopt", "-no-query", "-plugin", "true", "-state-roots", live, "-terragrunt"); code != exitOK || strings.Contains(stderr, "not writing") {
		t.Errorf("adopt: exit %d\n%s", code, stderr)
	}
	// Without -terragrunt the unit is no root: nothing to read.
	if _, _, stderr := runTool("-no-query", "-plugin", "true", "-state-roots", live); !strings.Contains(stderr, "0 states read") {
		t.Errorf("without -terragrunt:\n%s", stderr)
	}
}

// TestIntegrationFailedRoot: a root that fails before its state is read
// does not take the other roots' results with it; its state counts as
// unread, so adopt does not import its objects a second time.
func TestIntegrationFailedRoot(t *testing.T) {
	data, root := setup(t)
	for _, d := range []string{"good", "broken"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(root, "good", "main.tf"), `
terraform {
  required_providers {
    fake = { source = "example.com/fake/fake" }
  }
}
provider "fake" {}
`)
	// Initialized like the good one, but its own list block does not parse.
	good, _ := os.ReadFile(filepath.Join(root, "good", "main.tf"))
	write(t, filepath.Join(root, "broken", "main.tf"), string(good))
	write(t, filepath.Join(root, "broken", "mine.tfquery.hcl"), "list \"fake_thing\" \"x\" {\n")
	write(t, filepath.Join(data, "thing", "stray"), "")
	t.Chdir(root)
	code, stdout, stderr := runTool("-format", "json", "-types", "fake_thing", "good", "broken")
	if code != exitError {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	var out struct {
		Orphans []model.Resource `json:"orphans"`
		Summary struct {
			Complete      bool                  `json:"complete"`
			FailedSources []report.FailedSource `json:"failed_sources"`
			UnreadStates  []report.UnreadState  `json:"unread_states"`
		} `json:"summary"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("%v\n%s", err, stdout)
	}
	if len(out.Orphans) != 1 || out.Orphans[0].Identity["id"] != "stray" || out.Summary.Complete ||
		len(out.Summary.FailedSources) != 1 || out.Summary.FailedSources[0].Source != "broken" ||
		len(out.Summary.UnreadStates) != 1 || out.Summary.UnreadStates[0].Root != "broken" {
		t.Errorf("report:\n%s\n%s", stdout, stderr)
	}
	if code, _, stderr := runTool("adopt", "-types", "fake_thing", "good", "broken"); code != exitError || !strings.Contains(stderr, "not writing import blocks") {
		t.Errorf("adopt with a failed root: exit %d\n%s", code, stderr)
	}
	// terraform query itself fails after the state was read: the root is a
	// failed source, but its state is not unread.
	code, stdout, stderr = runTool("-format", "json", "-types", "fake_thing", "-max-query-size", "10", "good")
	out.Summary.FailedSources, out.Summary.UnreadStates = nil, nil
	if err := json.Unmarshal([]byte(stdout), &out); code != exitError || err != nil || len(out.Summary.FailedSources) != 1 ||
		!strings.Contains(out.Summary.FailedSources[0].Error, "larger than 10 bytes") || len(out.Summary.UnreadStates) != 0 {
		t.Errorf("-max-query-size: exit %d, %v\n%s\n%s", code, err, stdout, stderr)
	}
}
