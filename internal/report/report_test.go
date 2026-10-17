package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/tf"
)

func TestIgnore(t *testing.T) {
	ig, err := ParseIgnore(strings.NewReader(`
# comment
aws_iam_role:AWSServiceRole*
aws_default_*
vault_mount:sys
`))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		r    model.Resource
		want bool
	}{
		{model.Resource{Type: "aws_iam_role", DisplayName: "AWSServiceRoleForSupport"}, true},
		{model.Resource{Type: "aws_iam_role", Identity: map[string]any{"name": "AWSServiceRoleForECS"}}, true},
		{model.Resource{Type: "aws_iam_role", DisplayName: "app"}, false},
		{model.Resource{Type: "aws_default_vpc", DisplayName: "anything"}, true},
		{model.Resource{Type: "vault_mount", ImportID: "sys"}, true},
	}
	for _, c := range cases {
		if got := ig.Match(c.r); got != c.want {
			t.Errorf("Match(%+v) = %v", c.r, got)
		}
	}
}

func TestIgnoreConditions(t *testing.T) {
	ig, err := ParseIgnore(strings.NewReader(`
google_compute_disk name=pvc-*
google_compute_instance attr.labels.goog-gke-node=*
aws_iam_role:My role* account_id=111
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		r    model.Resource
		want bool
	}{
		{model.Resource{Type: "google_compute_disk", Identity: map[string]any{"name": "pvc-1", "project": "p"}}, true},
		// The project happens to start with pvc-: the condition is on name only.
		{model.Resource{Type: "google_compute_disk", Identity: map[string]any{"name": "data", "project": "pvc-x"}}, false},
		{model.Resource{Type: "google_compute_instance", Attributes: map[string]any{"labels": map[string]any{"goog-gke-node": ""}}}, true},
		{model.Resource{Type: "google_compute_instance", Attributes: map[string]any{"labels": map[string]any{"env": "prod"}}}, false},
		{model.Resource{Type: "google_compute_instance"}, false},
		{model.Resource{Type: "aws_iam_role", DisplayName: "My role 1", Identity: map[string]any{"account_id": "111"}}, true},
		{model.Resource{Type: "aws_iam_role", DisplayName: "My role 1", Identity: map[string]any{"account_id": "222"}}, false},
	} {
		if got := ig.Match(c.r); got != c.want {
			t.Errorf("Match(%+v) = %v", c.r, got)
		}
	}
	if got := ig.Attributes("google_compute_instance"); !slices.Equal(got, []string{"labels"}) {
		t.Errorf("attributes %v", got)
	}
	if got := ig.Attributes("google_compute_disk"); got != nil {
		t.Errorf("attributes %v", got)
	}
	if _, err := ParseIgnore(strings.NewReader("x labels.a=b\n")); err == nil {
		t.Error("identity key with a path accepted")
	}
}

func TestImport(t *testing.T) {
	var b bytes.Buffer
	_, err := Import(&b, []model.Resource{
		{Type: "aws_instance", DisplayName: "web 1", Identity: map[string]any{"id": "i-1", "region": "eu-north-1", "absent": nil}},
		{Type: "aws_instance", DisplayName: "Web-1", Identity: map[string]any{"id": "i-2"}, Provider: "aws-eu"},
		{Type: "vault_policy", DisplayName: "1st", ImportID: "1st", Identity: map[string]any{"id": "1st"}},
		{Type: "cloudflare_zone", DisplayName: "z", ImportID: "z1", Identity: map[string]any{"id": "z1"}, ImportNote: "first\nsecond"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"to = aws_instance.web_1\n",
		`    "id" = "i-1"`,
		`    "region" = "eu-north-1"`,
		"to = aws_instance.web_1_2\n  provider = aws-eu\n",
		"to = vault_policy.r_1st\n  id = \"1st\"\n",
		"to = cloudflare_zone.z\n  # first\n  # second\n  id = \"z1\"\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "absent") {
		t.Error("null identity attribute rendered")
	}
}

func TestImportGroupsBySource(t *testing.T) {
	var b bytes.Buffer
	_, err := Import(&b, []model.Resource{
		{Type: "aws_instance", DisplayName: "a", Identity: map[string]any{"id": "i-1"}, Source: "infra/b"},
		{Type: "aws_instance", DisplayName: "b", Identity: map[string]any{"id": "i-2"}, Source: "infra/a"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := b.String()
	ia, ib := strings.Index(out, "# Found by infra/a"), strings.Index(out, "# Found by infra/b")
	if ia < 0 || ib < 0 || ia > ib || !strings.Contains(out[ia:ib], "aws_instance.b") {
		t.Errorf("not grouped by source:\n%s", out)
	}

	b.Reset()
	if _, err := Import(&b, []model.Resource{{Type: "t", Identity: map[string]any{"id": "x"}, Source: "."}}, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "# Found by") {
		t.Error("single source must not be annotated")
	}
}

func TestImportRejectsInjection(t *testing.T) {
	var b bytes.Buffer
	n, err := Import(&b, []model.Resource{
		{Type: "aws_instance\nresource \"x\" \"y\" {}", Identity: map[string]any{"id": "i-1"}, Source: "a"},
		{Type: "aws_instance", Identity: map[string]any{"id\" = 1 }\n#": "i-2"}, Source: "plugin\nresource \"evil\" \"x\" {}"},
		{Type: "aws_s3_bucket", Identity: map[string]any{"bucket": "b"}, Source: "b"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("written = %d, want 2", n)
	}
	out := b.String()
	if strings.Contains(out, "\nresource ") {
		t.Errorf("configuration injected:\n%s", out)
	}
	if !strings.Contains(out, "# skipped:") || !strings.Contains(out, "to = aws_s3_bucket.b") {
		t.Errorf("output:\n%s", out)
	}
}

func TestTextStripsControlCharacters(t *testing.T) {
	var b bytes.Buffer
	if err := Text(&b, []model.Resource{{Type: "t", DisplayName: "vm\x1b]0;owned\x07\x1b[31m", Identity: map[string]any{"id": "x"}}}, nil); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(b.String(), "\x1b\x07") {
		t.Errorf("control characters printed: %q", b.String())
	}
}

func TestCleanLines(t *testing.T) {
	if got := CleanLines("a\x1b[31mb\n\tc\x07"); got != "a[31mb\n\tc" {
		t.Errorf("got %q", got)
	}
}

func TestImportAvoidsTakenAddresses(t *testing.T) {
	var b bytes.Buffer
	reserved := map[string]bool{"aws_instance.web": true, "aws_instance.web_2": true}
	if _, err := Import(&b, []model.Resource{{Type: "aws_instance", DisplayName: "web", Identity: map[string]any{"id": "i-new"}}}, reserved); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "to = aws_instance.web_3\n") {
		t.Errorf("output:\n%s", b.String())
	}
	if len(reserved) != 2 {
		t.Error("reserved set modified")
	}
}

func TestImportListIdentity(t *testing.T) {
	var b bytes.Buffer
	_, err := Import(&b, []model.Resource{{Type: "x_thing", DisplayName: "a",
		Identity: map[string]any{"ids": []any{"x", "${y}"}, "tags": map[string]any{"k": "v"}, "n": nil}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{`"ids" = ["x", "$${y}"]`, `"tags" = { "k" = "v" }`} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, `"n"`) {
		t.Errorf("null rendered:\n%s", out)
	}
	if a, b := (model.Resource{Type: "t", Identity: map[string]any{"ids": []any{"x"}}}).Key(), (model.Resource{Type: "t", Identity: map[string]any{"ids": []any{"y"}}}).Key(); a == b {
		t.Error("dedupe key ignores lists")
	}
}

func TestImportNameFromIdentity(t *testing.T) {
	var b bytes.Buffer
	if _, err := Import(&b, []model.Resource{{Type: "proxmox_virtual_environment_vm", DisplayName: "Сервер", Identity: map[string]any{"vm_id": "105"}, ImportID: "pve1/105"}}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "to = proxmox_virtual_environment_vm.vm_id_105\n") {
		t.Errorf("output:\n%s", b.String())
	}
}

func TestPresets(t *testing.T) {
	for _, name := range Presets() {
		if _, err := Preset(name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	ig, err := Preset("gcp-system")
	if err != nil {
		t.Fatal(err)
	}
	if !ig.Match(model.Resource{Type: "google_service_account", Identity: map[string]any{"email": "123-compute@developer.gserviceaccount.com", "project": "p"}}) {
		t.Error("default compute service account not ignored")
	}
	if ig.Match(model.Resource{Type: "google_service_account", Identity: map[string]any{"email": "ci@p.iam.gserviceaccount.com", "project": "p"}}) {
		t.Error("user service account ignored")
	}
	gke, err := Preset("gcp-gke-names")
	if err != nil {
		t.Fatal(err)
	}
	fw := func(name, description string) model.Resource {
		return model.Resource{Type: "google_compute_firewall", Identity: map[string]any{"name": name, "project": "p"}, Attributes: map[string]any{"description": description}}
	}
	for _, r := range []model.Resource{fw("k8s-0a1b-node-hc", ""), fw("k8s-fw-0a1b", ""), fw("k8s2-x-y-l4-shared-hc-fw", ""), fw("0a1b2c-hc", `{"kubernetes.io/service-name":"ns/svc"}`), fw("lb-0a1b", "GCE L7 firewall rule")} {
		if !gke.Match(r) {
			t.Errorf("GKE rule %v not ignored", r.Identity["name"])
		}
	}
	if gke.Match(fw("allow-ssh", "office")) {
		t.Error("user firewall rule ignored")
	}
	if ig.Match(fw("k8s-fw-0a1b", "")) {
		t.Error("gcp-system ignores by name")
	}
	cf, err := Preset("cloudflare-system")
	if err != nil {
		t.Fatal(err)
	}
	sanitize := model.Resource{Type: "cloudflare_ruleset", Identity: map[string]any{"id": "r1", "zone_id": "z"}, Attributes: map[string]any{"phase": "http_request_sanitize", "kind": "zone"}}
	if !cf.Match(sanitize) {
		t.Error("URL normalization entrypoint not ignored")
	}
	sanitize.Attributes["phase"] = "http_request_firewall_custom"
	if cf.Match(sanitize) {
		t.Error("custom firewall ruleset ignored")
	}
	tmpl, err := Preset("proxmox-templates")
	if err != nil {
		t.Fatal(err)
	}
	vm := model.Resource{Type: "proxmox_virtual_environment_vm", Identity: map[string]any{"vm_id": json.Number("9000")}, Attributes: map[string]any{"template": true}}
	if !tmpl.Match(vm) {
		t.Error("template not ignored")
	}
	vm.Attributes["template"] = false
	if tmpl.Match(vm) {
		t.Error("VM ignored as a template")
	}
	if _, err := Preset("../report"); err == nil {
		t.Error("path accepted as a preset")
	}
}

// Coverage per type and scope: a failure of one zone makes only that zone
// incomplete, one without a scope every row of its type; every scope read
// gets a row, also one without objects; a type skipped where a check was
// needed is incomplete unless the same root listed it, and one left out on
// purpose is skipped only when nothing listed it.
func TestBuildCoverage(t *testing.T) {
	counts := map[[2]string]*TypeCount{
		{"rec", "zone a"}:  {Live: 2, Managed: 2},
		{"rec", "zone b"}:  {Live: 1, Orphans: 1},
		{"rule", "zone a"}: {Live: 1, Uncertain: 1},
	}
	failed := []tf.Failure{
		{Type: "rec", Scope: "zone c", Category: tf.FailDenied},
		{Type: "rule", Category: tf.FailTruncated},
		{Type: "acct", Category: tf.FailDenied},
	}
	skipped := []tf.Skip{
		{Type: "empty", Reason: "needs region", Root: "prod"},
		{Type: "aliased", Reason: "provider p is configured only with aliases", Root: "staging"},
		{Type: "gone", Reason: "no data source", Excluded: true},
		{Type: "twin", Reason: "listed through google", Excluded: true},
		{Type: "net", Reason: "provider google-beta is configured only with aliases", Root: "prod", Provider: "google-beta"},
		{Type: "net2", Reason: "provider google-beta is configured only with aliases", Root: "prod", Provider: "google-beta"},
		{Type: "rs", Reason: "no zone was listed to read it in", Kind: "zone"},
		// Root b's API is off; zone a of the same type was read elsewhere.
		{Type: "rec", Reason: tf.FailAPIOff, Excluded: true, Root: "b"},
	}
	checks := []tf.Check{
		{Type: "rec", Scope: "zone a"}, {Type: "rec", Scope: "zone b"}, {Type: "rec", Scope: "zone d"},
		{Type: "rule", Scope: "zone a"}, {Type: "none"},
		{Type: "empty", Root: "prod"}, {Type: "aliased", Root: "prod"}, {Type: "twin", Root: "prod"},
		// google lists another project than the aliased google-beta; the
		// user's block with google-beta.other covers it.
		{Type: "net", Root: "prod", Provider: "google"}, {Type: "net2", Root: "prod", Provider: "google-beta"},
		// Accounts were read, zones were not: the account listing does not
		// stand for the zones.
		{Type: "rs", Scope: "account A", Kind: "account"},
	}
	got := BuildCoverage(counts, checks, skipped, failed)
	var lines []string
	for _, c := range got {
		lines = append(lines, fmt.Sprintf("%s/%s %s %v %d/%d/%d/%d", c.Type, c.Scope, c.Status, c.Reasons, c.Live, c.Managed, c.Uncertain, c.Orphans))
	}
	want := []string{
		"acct/ incomplete [permission denied] 0/0/0/0",
		"aliased/ complete [] 0/0/0/0",
		"aliased/root staging incomplete [skipped: provider p is configured only with aliases] 0/0/0/0",
		"empty/ complete [] 0/0/0/0",
		"gone/ skipped [no data source] 0/0/0/0",
		"net/ complete [] 0/0/0/0",
		"net/root prod incomplete [skipped: provider google-beta is configured only with aliases] 0/0/0/0",
		"net2/ complete [] 0/0/0/0",
		"none/ complete [] 0/0/0/0",
		"rec/root b skipped [API not enabled] 0/0/0/0",
		"rec/zone a complete [] 2/2/0/0",
		"rec/zone b complete [] 1/0/0/1",
		"rec/zone c incomplete [permission denied] 0/0/0/0",
		"rec/zone d complete [] 0/0/0/0",
		"rs/ incomplete [skipped: no zone was listed to read it in] 0/0/0/0",
		"rs/account A complete [] 0/0/0/0",
		"rule/zone a incomplete [truncated] 1/0/1/0",
		"twin/ complete [] 0/0/0/0",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("coverage:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	st := typeStatus(got)
	if st["rec"] != Incomplete || st["gone"] != Skipped || st["none"] != Complete || st["aliased"] != Incomplete {
		t.Errorf("type status %v", st)
	}
}

// Text output lists uncertain objects apart from the unmanaged ones.
func TestTextUncertain(t *testing.T) {
	var b bytes.Buffer
	u := []Uncertain{{Resource: model.Resource{Type: "t", DisplayName: "vm", Identity: map[string]any{"id": "1"}, Source: "proxmox"}, StateAddress: "t.vm"}}
	if err := Text(&b, nil, u); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "No unmanaged resources found.") || !strings.Contains(out, "Uncertain") || !strings.Contains(out, "t.vm") {
		t.Errorf("text:\n%s", out)
	}
}

// Import blocks of scoped objects say where the object lives.
func TestImportScope(t *testing.T) {
	var b bytes.Buffer
	r := model.Resource{Type: "cloudflare_dns_record", DisplayName: "www", ImportID: "z/1", ScopeName: "zone example.com"}
	if _, err := Import(&b, []model.Resource{r}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "  # in zone example.com\n") {
		t.Errorf("import:\n%s", b.String())
	}
}

// A type skipped where a check was needed shows in the summary table even
// without objects, and is named among what was not fully listed.
func TestSummaryTableGap(t *testing.T) {
	s := Summary{
		ByType:   map[string]*TypeCount{"rec": {Live: 1, Managed: 1}},
		Coverage: BuildCoverage(map[[2]string]*TypeCount{{"rec", ""}: {Live: 1, Managed: 1}}, []tf.Check{{Type: "rec"}}, []tf.Skip{{Type: "zoned", Reason: "requires zone", Root: "prod"}}, nil),
	}
	var b bytes.Buffer
	if err := SummaryTable(&b, s); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !regexp.MustCompile(`zoned\s+0\s+0\s+0\s+0\s+0\s+incomplete`).MatchString(out) || !strings.Contains(out, "skipped, needed a check (1): zoned") {
		t.Errorf("summary:\n%s", out)
	}
}

func TestNewUnreadState(t *testing.T) {
	err := errors.Join(
		errors.New("workspace a: terraform state pull: exit status 1\nError: Backend initialization required"),
		errors.New("workspace b: terraform state pull: exit status 1\nError: Unsupported Terraform Core version\n\n  on main.tf line 2"))
	if got := NewUnreadState("r", err).Error; got != "workspace a: Error: Backend initialization required; workspace b: Error: Unsupported Terraform Core version" {
		t.Errorf("%q", got)
	}
	if got := NewUnreadState("r", errors.New("not initialized")).Error; got != "not initialized" {
		t.Errorf("%q", got)
	}
	err = errors.Join(
		errors.New("workspace a: terraform state pull: exit status 1\nError: Backend initialization required"),
		errors.New("workspace b: decode state: unexpected end of JSON input"),
		errors.New("workspace c: read: permission denied\nmore"))
	if got := NewUnreadState("r", err).Error; got != "workspace a: Error: Backend initialization required; workspace b: decode state: unexpected end of JSON input; workspace c: read: permission denied" {
		t.Errorf("%q", got)
	}
	// The advice tforphans adds after terraform's output stays.
	err = errors.New("terraform state pull: exit status 1\nError: Unsupported Terraform Core version\n\n  on main.tf line 2\n(a required_version rejects this terraform: use a version it allows)")
	if got := NewUnreadState("r", err).Error; got != "Error: Unsupported Terraform Core version; (a required_version rejects this terraform: use a version it allows)" {
		t.Errorf("%q", got)
	}
	// Terragrunt log lines: time, level, unit, colours.
	for _, line := range []string{
		"14:02:01.123 ERROR  [units/app] Unable to determine underlying exit code",
		"\x1b[31mERROR\x1b[0m  Unable to determine underlying exit code",
		"ERROR  Unable to determine underlying exit code",
	} {
		err = errors.New("(terragrunt) run -- state pull: exit status 1\n" + line)
		if got := NewUnreadState("r", err).Error; got != "Error: Unable to determine underlying exit code" {
			t.Errorf("%q: %q", line, got)
		}
	}
	err = errors.New("(terragrunt) run -- state pull: exit status 1\n14:02:01.123 STDERR [units/app] terraform: Error: Backend initialization required")
	if got := NewUnreadState("r", err).Error; got != "Error: Backend initialization required" {
		t.Errorf("%q", got)
	}
}

// TestIgnoreBaseName: a name rule matches the object's own name when the
// display name adds to it ("web (pve1)").
func TestIgnoreBaseName(t *testing.T) {
	ig, err := ParseIgnore(strings.NewReader("proxmox_virtual_environment_vm:web\n"))
	if err != nil {
		t.Fatal(err)
	}
	r := model.Resource{Type: "proxmox_virtual_environment_vm", DisplayName: "web (pve1)", BaseName: "web", Identity: map[string]any{"vm_id": json.Number("100")}}
	if !ig.Match(r) {
		t.Error("name rule does not match the base name")
	}
	r.BaseName = "web2"
	if ig.Match(r) {
		t.Error("name rule matches another name")
	}
}

func TestJSONSchemaVersion(t *testing.T) {
	var b strings.Builder
	if err := JSON(&b, nil, nil, Summary{}); err != nil {
		t.Fatal(err)
	}
	var out struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal([]byte(b.String()), &out); err != nil || out.SchemaVersion != SchemaVersion || SchemaVersion < 1 {
		t.Errorf("schema_version %d, %v", out.SchemaVersion, err)
	}
}

func TestTypeStatusMixedRows(t *testing.T) {
	for _, rows := range [][]Coverage{
		{{Type: "t", Scope: "root a", Status: Skipped}, {Type: "t", Scope: "zone b", Status: Complete}},
		{{Type: "t", Scope: "zone b", Status: Complete}, {Type: "t", Scope: "root a", Status: Skipped}},
	} {
		if got := typeStatus(rows)["t"]; got != Complete {
			t.Errorf("%v: %s", rows, got)
		}
		rows = append(rows, Coverage{Type: "t", Scope: "zone c", Status: Incomplete})
		if got := typeStatus(rows)["t"]; got != Incomplete {
			t.Errorf("%v: %s", rows, got)
		}
	}
	if got := typeStatus([]Coverage{{Type: "t", Status: Skipped}})["t"]; got != Skipped {
		t.Errorf("only skipped: %s", got)
	}
}

func TestResourceNameTag(t *testing.T) {
	r := model.Resource{Type: "aws_instance", DisplayName: "i-0abc", BaseName: "base", TagName: "Web Server 1", Identity: map[string]any{"id": "i-0abc"}}
	if got := resourceName(r); got != "web_server_1" {
		t.Errorf("%q", got)
	}
	r.TagName = ""
	if got := resourceName(r); got != "base" {
		t.Errorf("without tag: %q", got)
	}
}

// TestIgnoreRuleOrigin: -explain names the file and line, or the preset, of
// the rule that ignored an object.
func TestIgnoreRuleOrigin(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".tforphans-ignore")
	if err := os.WriteFile(path, []byte("# comment\n\nvault_policy:tmp-*\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ig, err := LoadIgnore(path, false)
	if err != nil {
		t.Fatal(err)
	}
	pr, err := Preset("aws-system")
	if err != nil {
		t.Fatal(err)
	}
	ig.Merge(pr)
	if rule, ok := ig.Rule(model.Resource{Type: "vault_policy", DisplayName: "tmp-1"}); !ok || rule != path+":3: vault_policy:tmp-*" {
		t.Errorf("%q %v", rule, ok)
	}
	if rule, ok := ig.Rule(model.Resource{Type: "aws_default_security_group"}); !ok || !strings.HasPrefix(rule, "preset aws-system:") || !strings.HasSuffix(rule, ": aws_default_security_group") {
		t.Errorf("%q %v", rule, ok)
	}
	if rule, ok := ig.Rule(model.Resource{Type: "vault_policy", DisplayName: "keep"}); ok {
		t.Errorf("matched %q", rule)
	}
}
