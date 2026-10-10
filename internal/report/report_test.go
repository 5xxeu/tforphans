package report

import (
	"bytes"
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

func TestImport(t *testing.T) {
	var b bytes.Buffer
	err := Import(&b, []model.Resource{
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
	err := Import(&b, []model.Resource{
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
	if err := Import(&b, []model.Resource{{Type: "t", Identity: map[string]any{"id": "x"}, Source: "."}}, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "# Found by") {
		t.Error("single source must not be annotated")
	}
}

func TestImportRejectsInjection(t *testing.T) {
	var b bytes.Buffer
	err := Import(&b, []model.Resource{
		{Type: "aws_instance\nresource \"x\" \"y\" {}", Identity: map[string]any{"id": "i-1"}, Source: "a"},
		{Type: "aws_instance", Identity: map[string]any{"id\" = 1 }\n#": "i-2"}, Source: "plugin\nresource \"evil\" \"x\" {}"},
		{Type: "aws_s3_bucket", Identity: map[string]any{"bucket": "b"}, Source: "b"},
	}, nil)
	if err != nil {
		t.Fatal(err)
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
	if err := Import(&b, []model.Resource{{Type: "aws_instance", DisplayName: "web", Identity: map[string]any{"id": "i-new"}}}, reserved); err != nil {
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
	err := Import(&b, []model.Resource{{Type: "x_thing", DisplayName: "a",
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
	if err := Import(&b, []model.Resource{{Type: "proxmox_virtual_environment_vm", DisplayName: "Сервер", Identity: map[string]any{"vm_id": "105"}, ImportID: "pve1/105"}}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "to = proxmox_virtual_environment_vm.vm_id_105\n") {
		t.Errorf("output:\n%s", b.String())
	}
}
