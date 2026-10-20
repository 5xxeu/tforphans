package dsource

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/glob"
	"github.com/5xxeu/tforphans/internal/model"
)

// A trimmed provider schema in the shape of cloudflare 5.x.
const schemaJSON = `{"provider_schemas": {"registry.terraform.io/cloudflare/cloudflare": {
  "resource_schemas": {
    "cloudflare_zone": {"block": {"attributes": {"id": {}, "account": {}}}},
    "cloudflare_account": {"block": {"attributes": {"id": {}}}},
    "cloudflare_dns_record": {"block": {"attributes": {"id": {}, "zone_id": {}}}},
    "cloudflare_ruleset": {"block": {"attributes": {"id": {}, "zone_id": {}, "account_id": {}}}},
    "cloudflare_bot_management": {"block": {"attributes": {"zone_id": {}}}},
    "cloudflare_image": {"block": {"attributes": {"account_id": {}}}},
    "cloudflare_list_item": {"block": {"attributes": {"account_id": {}, "list_id": {}}}}
  },
  "data_source_schemas": {
    "cloudflare_zones": {"block": {"attributes": {"max_items": {"optional": true}, "result": {"nested_type": {"attributes": {"id": {}}}}}}},
    "cloudflare_accounts": {"block": {"attributes": {"max_items": {"optional": true}, "result": {"nested_type": {"attributes": {"id": {}}}}}}},
    "cloudflare_dns_records": {"block": {"attributes": {"zone_id": {"optional": true}, "result": {"nested_type": {"attributes": {"id": {}}}}}}},
    "cloudflare_rulesets": {"block": {"attributes": {"zone_id": {"optional": true}, "account_id": {"optional": true}, "result": {"nested_type": {"attributes": {"id": {}}}}}}},
    "cloudflare_images": {"block": {"attributes": {"account_id": {"required": true}, "result": {"nested_type": {"attributes": {"filename": {}}}}}}},
    "cloudflare_list_items": {"block": {"attributes": {"account_id": {"required": true}, "list_id": {"required": true}, "result": {"nested_type": {"attributes": {"id": {}}}}}}}
  }
}}}`

func testSchema(t *testing.T) *schema {
	t.Helper()
	s, err := parseSchema([]byte(schemaJSON), Cloudflare.Provider)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestBuildPlan(t *testing.T) {
	s := testSchema(t)
	p := buildPlan(s, Cloudflare, Options{Exclude: glob.CompileList([]string{"cloudflare_bot_*"})})

	var listed []string
	for _, l := range p.listings {
		scope := "-"
		if l.scope != nil {
			scope = l.scope.Arg
		}
		listed = append(listed, l.resourceType+"@"+scope)
	}
	want := "cloudflare_dns_record@zone_id cloudflare_ruleset@account_id cloudflare_ruleset@zone_id cloudflare_zone@-"
	if got := strings.Join(listed, " "); got != want {
		t.Errorf("listings = %s\nwant       %s", got, want)
	}
	reasons := map[string]string{}
	for _, sk := range p.skipped {
		reasons[sk.Type] = sk.Reason
	}
	for typ, want := range map[string]string{
		"cloudflare_account":   "the account itself",
		"cloudflare_image":     "results carry no id",
		"cloudflare_list_item": "requires list_id",
	} {
		if !strings.Contains(reasons[typ], want) {
			t.Errorf("skip %s = %q, want %q", typ, reasons[typ], want)
		}
	}
	if _, ok := reasons["cloudflare_bot_management"]; ok {
		t.Error("excluded type reported as skipped")
	}

	hcl := string(renderBlocks(p.blocks()))
	for _, want := range []string{
		`data "cloudflare_zones" "all" {`,
		`data "cloudflare_accounts" "all" {`,
		`data "cloudflare_dns_records" "by_zone_id" {`,
		`  for_each  = { for o in (data.cloudflare_zones.all.result == null ? [] : data.cloudflare_zones.all.result) : o.id => o.id }`,
		`  zone_id = each.value`,
		`data "cloudflare_rulesets" "by_account_id" {`,
		`max_items = 1000000`,
	} {
		if !strings.Contains(hcl, want) {
			t.Errorf("HCL lacks %q:\n%s", want, hcl)
		}
	}
	if strings.Count(hcl, `data "cloudflare_zones"`) != 1 {
		t.Errorf("parent block repeated:\n%s", hcl)
	}
}

func TestApplyErrors(t *testing.T) {
	out := `{"type":"version"}
{"@level":"error","diagnostic":{"severity":"error","summary":"failed to make http request","detail":"403 Forbidden","address":"data.cloudflare_rulesets.by_zone_id[\"z1\"]"},"type":"diagnostic"}
{"@level":"error","diagnostic":{"severity":"error","summary":"boom","address":"data.cloudflare_zones.all"},"type":"diagnostic"}
{"@level":"error","diagnostic":{"severity":"error","summary":"Invalid provider configuration"},"type":"diagnostic"}
{"@level":"warn","diagnostic":{"severity":"warning","summary":"ignored","address":"data.x.y"},"type":"diagnostic"}`
	failed, fatal := applyErrors([]byte(out))
	want := []diagErr{
		{block: "data.cloudflare_rulesets.by_zone_id", key: "z1", message: "failed to make http request: 403 Forbidden"},
		{block: "data.cloudflare_zones.all", message: "boom"},
	}
	if len(failed) != 2 || failed[0] != want[0] || failed[1] != want[1] {
		t.Errorf("failed = %+v", failed)
	}
	if len(fatal) != 1 {
		t.Errorf("fatal = %v", fatal)
	}
}

func TestNextRound(t *testing.T) {
	zones := &Cloudflare.Scopes[1]
	blocks := []block{
		{dsType: "cloudflare_zones", label: "all"},
		{dsType: "cloudflare_dns_records", label: "by_zone_id", scope: zones, parent: "data.cloudflare_zones.all"},
		{dsType: "cloudflare_rulesets", label: "by_zone_id", scope: zones, parent: "data.cloudflare_zones.all"},
		{dsType: "cloudflare_accounts", label: "all"},
		{dsType: "cloudflare_images", label: "by_account_id", scope: &Cloudflare.Scopes[0], parent: "data.cloudflare_accounts.all"},
	}
	// One zone fails for DNS; the accounts listing fails as a whole.
	keep, failed, changed := nextRound(blocks, []diagErr{
		{block: "data.cloudflare_dns_records.by_zone_id", key: "z2", message: "403"},
		{block: "data.cloudflare_accounts.all", message: "500"},
	})
	if !changed {
		t.Fatal("no change")
	}
	var kept []string
	for _, b := range keep {
		kept = append(kept, b.addr()+fmt.Sprint(b.exclude))
	}
	if got := strings.Join(kept, " "); got != "data.cloudflare_zones.all[] data.cloudflare_dns_records.by_zone_id[z2] data.cloudflare_rulesets.by_zone_id[]" {
		t.Errorf("kept %s", got)
	}
	var addrs []string
	for _, f := range failed {
		addrs = append(addrs, f.address)
	}
	if got := strings.Join(addrs, " "); got != `data.cloudflare_dns_records.by_zone_id["z2"] data.cloudflare_accounts.all data.cloudflare_images.by_account_id` {
		t.Errorf("failed %s", got)
	}
	hcl := string(renderBlocks(keep))
	if !strings.Contains(hcl, `for_each  = { for o in (data.cloudflare_zones.all.result == null ? [] : data.cloudflare_zones.all.result) : o.id => o.id if !contains(["z2"], o.id) }`) {
		t.Errorf("exclusion not rendered:\n%s", hcl)
	}
	// The same error again changes nothing: retrying would loop.
	if _, _, changed := nextRound(keep, []diagErr{{block: "data.cloudflare_dns_records.by_zone_id", key: "z2", message: "403"}}); changed {
		t.Error("repeated error reported as progress")
	}
}

func TestExtract(t *testing.T) {
	s := testSchema(t)
	p := buildPlan(s, Cloudflare, Options{})
	blocks := p.blocks()
	state := map[string]any{"resources": []any{
		dataRes("cloudflare_zones", "all", nil, []any{map[string]any{"id": "z1", "name": "example.com", "type": "full"}}),
		dataRes("cloudflare_dns_records", "by_zone_id", "z1", []any{
			map[string]any{"id": "r1", "name": "www.example.com", "type": "A", "meta": `{"read_only":false}`},
			map[string]any{"id": "r2", "name": "api.example.com", "type": "AAAA", "meta": `{"origin_worker_id":"w1","read_only":true}`},
		}),
		dataRes("cloudflare_rulesets", "by_account_id", "acc", []any{
			map[string]any{"id": "0123456789abcdef0123", "name": "custom", "kind": "root"},
			map[string]any{"id": "77454fe2d30c4220b5701f6fdfb893ba", "name": "Cloudflare Managed Free Ruleset", "kind": "managed"},
		}),
		// The same account ruleset shows up in the zone listing.
		dataRes("cloudflare_rulesets", "by_zone_id", "z1", []any{
			map[string]any{"id": "0123456789abcdef0123", "name": "custom", "kind": "root"},
		}),
	}}
	data, _ := json.Marshal(state)
	got, err := extract(data, blocks, Cloudflare, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, r := range got {
		lines = append(lines, r.Type+" "+r.ImportID+" "+r.DisplayName+" "+r.IdentityString())
	}
	sort.Strings(lines)
	want := []string{
		"cloudflare_dns_record z1/r1 A www.example.com id=r1, zone_id=z1",
		"cloudflare_ruleset accounts/acc/0123456789abcdef0123 custom account_id=acc, id=0123456789abcdef0123",
		"cloudflare_zone z1 full example.com id=z1",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("got:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	for _, r := range got {
		if r.Type == "cloudflare_dns_record" && (len(r.AltTypes) != 1 || r.AltTypes[0] != "cloudflare_record") {
			t.Errorf("dns AltTypes = %v", r.AltTypes)
		}
	}
}

// A list that reached max_items may have stopped there, and so may every
// listing over its objects (zones beyond the cut were never read).
func TestTruncated(t *testing.T) {
	defer func(n int) { maxItems = n }(maxItems)
	maxItems = 2
	s := testSchema(t)
	blocks := buildPlan(s, Cloudflare, Options{}).blocks()
	state := map[string]any{"resources": []any{
		dataRes("cloudflare_zones", "all", nil, []any{map[string]any{"id": "z1"}, map[string]any{"id": "z2"}}),
		dataRes("cloudflare_accounts", "all", nil, []any{map[string]any{"id": "acc"}}),
		// No max_items argument: a long list is not a cut.
		dataRes("cloudflare_dns_records", "by_zone_id", "z1", []any{map[string]any{"id": "r1"}, map[string]any{"id": "r2"}, map[string]any{"id": "r3"}}),
	}}
	data, _ := json.Marshal(state)
	got, err := truncated(data, blocks, Cloudflare)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, f := range got {
		lines = append(lines, f.Address+" "+f.Type)
	}
	sort.Strings(lines)
	want := []string{
		"data.cloudflare_dns_records.by_zone_id cloudflare_dns_record",
		"data.cloudflare_rulesets.by_zone_id cloudflare_ruleset",
		"data.cloudflare_zones.all cloudflare_zone",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("got:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

func TestCollapse(t *testing.T) {
	mk := func(typ, id, arg, val string) scoped {
		r := scoped{res: model.Resource{Type: typ, Identity: map[string]any{"id": id}}, id: id, scopeArg: arg}
		if arg != "" {
			r.res.Identity[arg] = val
		}
		return r
	}
	got := collapse([]scoped{
		mk("cloudflare_workers_script", "api", "account_id", "acc1"),
		mk("cloudflare_workers_script", "api", "account_id", "acc2"), // another account: kept
		mk("cloudflare_ruleset", "0123456789abcdef0123", "account_id", "acc1"),
		mk("cloudflare_ruleset", "0123456789abcdef0123", "zone_id", "z1"),      // same object via the zone: dropped
		mk("cloudflare_ruleset", "0123456789abcdef0123", "account_id", "acc1"), // exact repeat: dropped
		// A user-chosen name equal to another type's ID must not merge.
		mk("cloudflare_dns_record", "0123456789abcdef0123", "zone_id", "z1"),
	})
	var keys []string
	for _, r := range got {
		keys = append(keys, r.Type+" "+r.IdentityString())
	}
	want := []string{
		"cloudflare_workers_script account_id=acc1, id=api",
		"cloudflare_workers_script account_id=acc2, id=api",
		"cloudflare_ruleset account_id=acc1, id=0123456789abcdef0123",
		"cloudflare_dns_record id=0123456789abcdef0123, zone_id=z1",
	}
	if strings.Join(keys, "\n") != strings.Join(want, "\n") {
		t.Errorf("got:\n%s\nwant:\n%s", strings.Join(keys, "\n"), strings.Join(want, "\n"))
	}
	for _, r := range got {
		if len(r.AltTypes) != 0 {
			t.Errorf("%s got AltTypes %v", r.Type, r.AltTypes)
		}
	}
}

func TestNotApplicable(t *testing.T) {
	if !Cloudflare.NotApplicable("failed to make http request: missing required account_id parameter") {
		t.Error("missing scope parameter not recognised")
	}
	if Cloudflare.NotApplicable("failed to make http request: 403 Forbidden") {
		t.Error("403 treated as not applicable")
	}
}

func TestCloudflareKeep(t *testing.T) {
	cases := []struct {
		typ  string
		item map[string]any
		want bool
	}{
		{"cloudflare_dns_record", map[string]any{"meta": map[string]any{"email_routing": true}}, false},
		{"cloudflare_dns_record", map[string]any{"meta": "{}"}, true},
		{"cloudflare_email_routing_rule", map[string]any{"matchers": []any{map[string]any{"type": "all"}}}, false},
		{"cloudflare_email_routing_rule", map[string]any{"matchers": []any{map[string]any{"type": "literal"}}}, true},
		{"cloudflare_zero_trust_device_custom_profile", map[string]any{"default": true}, false},
		{"cloudflare_ruleset", map[string]any{"kind": "managed", "id": "77454fe2d30c4220b5701f6fdfb893ba"}, false},
		{"cloudflare_ruleset", map[string]any{"kind": "managed", "id": "0000000000000000000000000000beef"}, true}, // user-made "managed"
		{"cloudflare_ruleset", map[string]any{"kind": "zone"}, true},
		{"cloudflare_certificate_pack", map[string]any{"type": "universal"}, false},
		{"cloudflare_certificate_pack", map[string]any{"type": "advanced"}, true},
		// User-settable fields never hide an object.
		{"cloudflare_zero_trust_tunnel_cloudflared_virtual_network", map[string]any{"is_default_network": true}, true},
		{"cloudflare_mtls_certificate", map[string]any{"name": "Gateway CA - Cloudflare Managed G1 x"}, true},
		{"cloudflare_zero_trust_dlp_custom_entry", map[string]any{"type": "predefined"}, false},
		{"cloudflare_zero_trust_dlp_custom_entry", map[string]any{"type": "custom"}, true},
		{"cloudflare_zero_trust_resource_library_application", map[string]any{"application_source": "cloudflare", "name": "Asana"}, false},
		{"cloudflare_zero_trust_resource_library_application", map[string]any{"application_source": "user", "name": "Asana"}, true},
	}
	for _, c := range cases {
		if got := cloudflareKeep(c.typ, c.item); got != c.want {
			t.Errorf("Keep(%s, %v) = %v", c.typ, c.item, got)
		}
	}
	alt := Cloudflare.AltIdentity("cloudflare_workers_script", map[string]any{"id": "api", "tag": "uuid"})
	if alt["cloudflare_worker"]["id"] != "uuid" {
		t.Errorf("AltIdentity = %v", alt)
	}
}

func dataRes(typ, name string, key any, result []any) map[string]any {
	return map[string]any{"mode": "data", "type": typ, "name": name, "instances": []any{
		map[string]any{"index_key": key, "attributes": map[string]any{"result": result}},
	}}
}

func TestListItemsIDs(t *testing.T) {
	attrs := map[string]any{
		"role_ids": []any{"Administrator", "Auditor"},
		"special":  []any{true, false},
		"id":       "roles",
	}
	got := listItems(attrs, Proxmox.spec("proxmox_virtual_environment_roles"))
	if len(got) != 2 || got[1]["id"] != "Auditor" || got[1]["special"] != false || got[0]["special"] != true {
		t.Errorf("got %v", got)
	}
	if Proxmox.Keep("proxmox_virtual_environment_role", got[0]) || !Proxmox.Keep("proxmox_virtual_environment_role", got[1]) {
		t.Error("built-in role kept or custom role dropped")
	}
	if Proxmox.Keep("proxmox_virtual_environment_user", map[string]any{"id": "root@pam"}) {
		t.Error("root@pam kept")
	}
}

func TestHasFieldCtyType(t *testing.T) {
	var a attr
	if err := json.Unmarshal([]byte(`{"type": ["list", ["object", {"name": "string", "vm_id": "number"}]]}`), &a); err != nil {
		t.Fatal(err)
	}
	if !a.hasField("vm_id") || a.hasField("id") {
		t.Error("cty object fields not read")
	}
}

func TestProxmoxRenames(t *testing.T) {
	deprecated := map[string]bool{
		"proxmox_virtual_environment_hagroup": true, "proxmox_hagroup": false,
		"proxmox_virtual_environment_acl": true, "proxmox_acl": false,
		"proxmox_virtual_environment_vm": false, "proxmox_vm": false,
	}
	hasPlural := func(t string) bool { return t == "proxmox_hagroup" || t == "proxmox_virtual_environment_acl" }
	skip, alt := proxmoxRenames(deprecated, hasPlural)
	if skip["proxmox_virtual_environment_hagroup"] == "" || alt["proxmox_hagroup"][0] != "proxmox_virtual_environment_hagroup" {
		t.Errorf("successor with plural: skip=%v alt=%v", skip, alt)
	}
	if _, ok := skip["proxmox_virtual_environment_acl"]; ok || alt["proxmox_virtual_environment_acl"][0] != "proxmox_acl" {
		t.Errorf("successor without plural: skip=%v alt=%v", skip, alt)
	}
	if _, ok := alt["proxmox_virtual_environment_vm"]; ok {
		t.Error("non-deprecated type paired")
	}
}

func TestProxmoxImportAndAltIdentity(t *testing.T) {
	item := map[string]any{"vm_id": json.Number("102"), "node_name": "pve1"}
	if got := Proxmox.ImportID("proxmox_virtual_environment_vm", item, "102"); got != "pve1/102" {
		t.Errorf("import id %s", got)
	}
	alt := Proxmox.AltIdentity("proxmox_virtual_environment_vm", item)
	if alt["proxmox_vm_qemu"]["vmid"] != json.Number("102") || alt["proxmox_vm"]["id"] != json.Number("102") {
		t.Errorf("alt identity %v", alt)
	}
}

func TestLockEntry(t *testing.T) {
	dir := t.TempDir()
	lock := `# provider "registry.terraform.io/cloudflare/cloudflare" in a comment
provider "registry.terraform.io/hashicorp/null" {
  version = "3.2.0"
  hashes  = ["h1:a"]
}

provider "registry.terraform.io/cloudflare/cloudflare" {
  version     = "5.26.0"
  constraints = "~> 5.0"
  hashes = [
    "h1:b",
  ]
}
`
	os.WriteFile(filepath.Join(dir, ".terraform.lock.hcl"), []byte(lock), 0o644)
	got, err := lockEntry(dir, Cloudflare.Provider)
	if err != nil || !strings.HasPrefix(string(got), `provider "registry.terraform.io/cloudflare/cloudflare" {`) || strings.Contains(string(got), "null") {
		t.Errorf("entry %q %v", got, err)
	}
	if VersionRoot([]string{dir}, Proxmox.Provider) != "" {
		t.Error("unpinned provider accepted")
	}
	other := t.TempDir()
	os.WriteFile(filepath.Join(other, ".terraform.lock.hcl"), []byte("# provider \"registry.terraform.io/bpg/proxmox\"\n"), 0o644)
	if VersionRoot([]string{other}, Proxmox.Provider) != "" {
		t.Error("a comment counted as a pin")
	}
}

func TestProxmoxHooks(t *testing.T) {
	for _, m := range []string{"failed to authenticate: received an HTTP 401 response - Reason: authentication failure", "received an HTTP 401 response - Reason: invalid token value!", "status code: 401"} {
		if !Proxmox.AuthFailed(m) {
			t.Errorf("AuthFailed(%q) false", m)
		}
	}
	for _, m := range []string{"received an HTTP 403 response - Reason: Permission check failed (request 401abc)", "https://pve:401/api2/json", "vm 401 not found"} {
		if Proxmox.AuthFailed(m) {
			t.Errorf("AuthFailed(%q) true", m)
		}
	}
	msg := "received an HTTP 500 response - Reason: ha groups have been migrated to rules"
	if Proxmox.Unsupported("proxmox_hagroups", msg) == "" || Proxmox.Unsupported("proxmox_hagroups", "HTTP 500") != "" || Proxmox.Unsupported("proxmox_virtual_environment_users", msg) != "" {
		t.Error("Unsupported")
	}
	if got := Proxmox.DisplayName("proxmox_virtual_environment_vm", map[string]any{"node_name": "pve2"}, "tmpl-debian"); got != "tmpl-debian (pve2)" {
		t.Errorf("display name %q", got)
	}
}

func TestUnsupported(t *testing.T) {
	a := Adapter{Unsupported: func(_, m string) string {
		if strings.Contains(m, "migrated") {
			return "gone"
		}
		return ""
	}}
	all := []block{{dsType: "x_groups", label: "all", listings: []listing{{resourceType: "x_group"}}}, {dsType: "x_users", label: "all", listings: []listing{{resourceType: "x_user"}}}}
	res := &Result{}
	rest := unsupported([]failure{
		{block: "data.x_groups.all", address: "data.x_groups.all", message: "500: migrated to rules"},
		{block: "data.x_users.all", address: "data.x_users.all", message: "500: other"},
	}, all, a, res)
	if len(rest) != 1 || rest[0].block != "data.x_users.all" {
		t.Errorf("rest %+v", rest)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Type != "x_group" || !res.Skipped[0].Excluded || res.Skipped[0].Reason != "gone" {
		t.Errorf("skipped %+v", res.Skipped)
	}
}

func TestErrorMessages(t *testing.T) {
	long := strings.Repeat("x", 300) + " 401"
	out := []byte(`{"diagnostic":{"severity":"error","summary":"Error configuring provider","detail":"` + long + `"}}
{"diagnostic":{"severity":"error","summary":"E","detail":"short","address":"data.x.probe"}}
{"diagnostic":{"severity":"warning","summary":"W"}}
`)
	msgs := errorMessages(out)
	if len(msgs) != 2 || !strings.HasSuffix(msgs[0], " 401") || msgs[1] != "E: short" {
		t.Errorf("messages %q", msgs)
	}
	if _, fatal := applyErrors(out); len(fatal) != 1 || strings.HasSuffix(fatal[0], " 401") {
		t.Errorf("applyErrors still cuts messages: %q", fatal)
	}
}

func TestParseSchemaEmptyResponse(t *testing.T) {
	for _, raw := range []string{`{}`, `{"provider_schemas":null}`, `{"provider_schemas":{}}`} {
		_, err := parseSchema([]byte(raw), Cloudflare.Provider)
		var response *model.ResponseError
		if !errors.As(err, &response) {
			t.Errorf("%s: want ResponseError, got %v", raw, err)
		}
	}
	_, err := parseSchema([]byte(`{"provider_schemas":{"example.com/other/other":{}}}`), Cloudflare.Provider)
	var response *model.ResponseError
	if errors.As(err, &response) || err == nil || err.Error() != "provider "+Cloudflare.Provider+" not installed" {
		t.Fatalf("missing provider: %v", err)
	}
}
