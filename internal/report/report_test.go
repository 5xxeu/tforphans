package report

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/model"
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
	if err := Text(&b, []model.Resource{{Type: "t", DisplayName: "vm\x1b]0;owned\x07\x1b[31m", Identity: map[string]any{"id": "x"}}}); err != nil {
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
	if _, err := Preset("../report"); err == nil {
		t.Error("path accepted as a preset")
	}
}
