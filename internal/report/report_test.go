package report

import (
	"bytes"
	"fmt"
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
	}
	checks := []tf.Check{
		{Type: "rec", Scope: "zone a"}, {Type: "rec", Scope: "zone b"}, {Type: "rec", Scope: "zone d"},
		{Type: "rule", Scope: "zone a"}, {Type: "none"},
		{Type: "empty", Root: "prod"}, {Type: "aliased", Root: "prod"}, {Type: "twin", Root: "prod"},
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
		"none/ complete [] 0/0/0/0",
		"rec/zone a complete [] 2/2/0/0",
		"rec/zone b complete [] 1/0/0/1",
		"rec/zone c incomplete [permission denied] 0/0/0/0",
		"rec/zone d complete [] 0/0/0/0",
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
