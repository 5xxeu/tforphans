package dsource

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/tf"
)

func TestCrashed(t *testing.T) {
	if !crashed(nil, nil, nil) {
		t.Error("a failed run without diagnostics is a crash")
	}
	if crashed(nil, []diagErr{{block: "data.x.all", message: "403"}}, nil) {
		t.Error("an error of one data source is not a crash")
	}
	errs := []diagErr{{block: "data.x.all", message: "Plugin did not respond: The plugin encountered an error, and failed to respond to the plugin.(*GRPCProvider).ReadDataSource call."}}
	if !crashed(nil, errs, nil) {
		t.Error("a plugin crash names the blocks in flight, it is still a crash")
	}
	stderr := []byte("Stack trace from the terraform binary:\npanic: inconsistent list element types (cty.Object(...) then cty.Object(...))\n")
	if got := crashText(stderr, nil, nil); !strings.HasPrefix(got, "Stack trace") && !strings.HasPrefix(got, "panic:") {
		t.Errorf("crash text %q", got)
	}
}

// TestIsolate covers a crash in one zone of one block and a block that
// crashes as a whole; the other blocks and zones keep going.
func TestIsolate(t *testing.T) {
	zones, accounts := &Cloudflare.Scopes[1], &Cloudflare.Scopes[0]
	blocks := []block{
		{dsType: "cloudflare_zones", label: "all"},
		{dsType: "cloudflare_accounts", label: "all"},
		{dsType: "cloudflare_dns_records", label: "by_zone_id", scope: zones, parent: "data.cloudflare_zones.all"},
		{dsType: "cloudflare_rulesets", label: "by_zone_id", scope: zones, parent: "data.cloudflare_zones.all"},
		{dsType: "cloudflare_images", label: "all"},
		{dsType: "cloudflare_lists", label: "by_account_id", scope: accounts, parent: "data.cloudflare_accounts.all"},
	}
	zoneIDs := []string{"z1", "z2", "bad", "z4", "z5"}
	var state strings.Builder
	state.WriteString(`{"resources": [{"mode": "data", "type": "cloudflare_zones", "name": "all", "instances": [{"attributes": {"result": [`)
	for i, z := range zoneIDs {
		if i > 0 {
			state.WriteString(",")
		}
		fmt.Fprintf(&state, `{"id": %q}`, z)
	}
	state.WriteString(`]}}]}]}`)

	runs := 0
	probe := func(set []block) (bool, []byte, error) {
		runs++
		for _, b := range set {
			if b.parent != "" && !slices.ContainsFunc(set, func(p block) bool { return p.addr() == b.parent }) {
				t.Errorf("%s probed without its parent", b.addr())
			}
			switch b.addr() {
			case "data.cloudflare_images.all":
				return true, nil, nil
			case "data.cloudflare_dns_records.by_zone_id":
				if !slices.Contains(b.exclude, "bad") && (b.only == nil || slices.Contains(b.only, "bad")) {
					return true, nil, nil
				}
			}
		}
		return false, []byte(state.String()), nil
	}
	keep, failed, err := isolate(blocks, "crashed: panic", probe)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, b := range keep {
		kept = append(kept, b.addr()+fmt.Sprint(b.exclude)+fmt.Sprint(b.only))
	}
	want := "data.cloudflare_zones.all[][] data.cloudflare_accounts.all[][] data.cloudflare_dns_records.by_zone_id[bad][] data.cloudflare_rulesets.by_zone_id[][] data.cloudflare_lists.by_account_id[][]"
	if got := strings.Join(kept, " "); got != want {
		t.Errorf("kept\n%s\nwant\n%s", got, want)
	}
	var addrs []string
	for _, f := range failed {
		addrs = append(addrs, f.address+": "+f.message)
	}
	slices.Sort(addrs)
	if got := strings.Join(addrs, "\n"); got != "data.cloudflare_dns_records.by_zone_id[\"bad\"]: crashed: panic\ndata.cloudflare_images.all: crashed: panic" {
		t.Errorf("failed:\n%s", got)
	}
	if runs > 20 {
		t.Errorf("%d probe runs", runs)
	}
}

func TestIsolateParent(t *testing.T) {
	blocks := []block{
		{dsType: "cloudflare_zones", label: "all"},
		{dsType: "cloudflare_dns_records", label: "by_zone_id", scope: &Cloudflare.Scopes[1], parent: "data.cloudflare_zones.all"},
		{dsType: "cloudflare_images", label: "all"},
	}
	probe := func(set []block) (bool, []byte, error) {
		return slices.ContainsFunc(set, func(b block) bool { return b.dsType == "cloudflare_zones" }), nil, nil
	}
	keep, failed, err := isolate(blocks, "", probe)
	if err != nil {
		t.Fatal(err)
	}
	if len(keep) != 1 || keep[0].dsType != "cloudflare_images" || len(failed) != 2 || !strings.HasPrefix(failed[1].message, "parent data.cloudflare_zones.all crashed") {
		t.Errorf("keep %v failed %v", keep, failed)
	}
}

// TestIsolateNotReproduced: a crash that does not happen again keeps every
// block for another full read; one that always comes back on the full set
// ends, through the budget, with the suspects reported.
func TestIsolateNotReproduced(t *testing.T) {
	blocks := []block{{dsType: "cloudflare_zones", label: "all"}, {dsType: "cloudflare_images", label: "all"}}
	keep, failed, err := isolate(blocks, "", func([]block) (bool, []byte, error) { return false, nil, nil })
	if err != nil || len(keep) != 2 || len(failed) != 0 {
		t.Errorf("keep %v failed %v err %v", keep, failed, err)
	}
	n := 0
	probe := budget(func(set []block) (bool, []byte, error) { return len(set) == 2, nil, nil }, &n, 6, nil)
	for range 10 {
		keep, failed, err = isolate(blocks, "", probe)
		if err != nil {
			t.Fatal(err)
		}
		if len(failed) > 0 {
			break
		}
	}
	if len(keep) != 0 || len(failed) != 2 {
		t.Errorf("after the budget: keep %v failed %v", keep, failed)
	}
}

func TestRenderOnly(t *testing.T) {
	hcl := string(renderBlocks([]block{{dsType: "cloudflare_dns_records", label: "by_zone_id", scope: &Cloudflare.Scopes[1], parent: "data.cloudflare_zones.all", only: []string{"example.com"}, exclude: []string{"z2"}}}))
	if !strings.Contains(hcl, `o.id => o.id if (contains(["example.com"], o.id) || contains(["example.com"], try(o.name, ""))) && !contains(["z2"], o.id) }`) {
		t.Errorf("render:\n%s", hcl)
	}
}

// TestLimitScopeHidden: a scope given with no value is still read for data
// sources that also take the other scope, only to drop the objects that
// both scopes list; it reports nothing of its own.
func TestLimitScopeHidden(t *testing.T) {
	zones, accounts := &Cloudflare.Scopes[1], &Cloudflare.Scopes[0]
	blocks := []block{
		{dsType: "cloudflare_accounts", label: "all"},
		{dsType: "cloudflare_zones", label: "all"},
		{dsType: "cloudflare_rulesets", label: "by_account_id", scope: accounts, parent: "data.cloudflare_accounts.all", listings: []listing{{resourceType: "cloudflare_ruleset", scope: accounts}}},
		{dsType: "cloudflare_rulesets", label: "by_zone_id", scope: zones, parent: "data.cloudflare_zones.all", listings: []listing{{resourceType: "cloudflare_ruleset", scope: zones}}},
		{dsType: "cloudflare_images", label: "by_account_id", scope: accounts, parent: "data.cloudflare_accounts.all", listings: []listing{{resourceType: "cloudflare_image", scope: accounts}}},
	}
	got, skipped := limitScope(blocks, map[string][]string{"account_id": {}})
	var addrs []string
	for _, b := range got {
		addrs = append(addrs, fmt.Sprintf("%s hidden=%v", b.addr(), b.hidden))
	}
	want := "data.cloudflare_accounts.all hidden=false data.cloudflare_zones.all hidden=false data.cloudflare_rulesets.by_account_id hidden=true data.cloudflare_rulesets.by_zone_id hidden=false"
	if strings.Join(addrs, " ") != want {
		t.Errorf("blocks:\n%s", strings.Join(addrs, "\n"))
	}
	if len(skipped) != 1 || skipped[0].Type != "cloudflare_image" {
		t.Errorf("skipped %v", skipped)
	}

	res := func(id, arg string, hidden bool) scoped {
		r := scoped{id: id, scopeArg: arg, hidden: hidden}
		r.res.Type = "cloudflare_ruleset"
		r.res.Identity = map[string]any{"id": id, arg: "x"}
		return r
	}
	out := collapse([]scoped{res("acct", "account_id", true), res("acct", "zone_id", false), res("zoned", "zone_id", false)})
	if len(out) != 1 || out[0].Identity["id"] != "zoned" {
		t.Errorf("collapse %v", out)
	}
}

// TestIsolateBudget: a block crashing in every one of 40 zones needs more
// runs than allowed; it is reported as crashed and the scan goes on.
func TestIsolateBudget(t *testing.T) {
	zones := &Cloudflare.Scopes[1]
	blocks := []block{
		{dsType: "cloudflare_zones", label: "all"},
		{dsType: "cloudflare_dns_records", label: "by_zone_id", scope: zones, parent: "data.cloudflare_zones.all"},
		{dsType: "cloudflare_rulesets", label: "by_zone_id", scope: zones, parent: "data.cloudflare_zones.all"},
	}
	var state strings.Builder
	state.WriteString(`{"resources": [{"mode": "data", "type": "cloudflare_zones", "name": "all", "instances": [{"attributes": {"result": [`)
	for i := range 40 {
		if i > 0 {
			state.WriteString(",")
		}
		fmt.Fprintf(&state, `{"id": "z%d"}`, i)
	}
	state.WriteString(`]}}]}]}`)
	exhausted, spent := 0, 0
	probe := budget(func(set []block) (bool, []byte, error) {
		for _, b := range set {
			if b.dsType == "cloudflare_dns_records" {
				return true, nil, nil
			}
		}
		return false, []byte(state.String()), nil
	}, &spent, 10, func() { exhausted++ })
	keep, failed, err := isolate(blocks, "", probe)
	if err != nil {
		t.Fatal(err)
	}
	if exhausted != 1 {
		t.Errorf("exhausted %d times", exhausted)
	}
	var kept []string
	for _, b := range keep {
		kept = append(kept, b.addr())
	}
	if strings.Join(kept, " ") != "data.cloudflare_zones.all data.cloudflare_rulesets.by_zone_id" {
		t.Errorf("kept %v", kept)
	}
	if len(failed) != 1 || failed[0].address != "data.cloudflare_dns_records.by_zone_id" {
		t.Errorf("failed %v", failed)
	}
	f := tf.Failure{Message: failed[0].message}
	f.Classify()
	if f.Category != tf.FailCrash {
		t.Errorf("a crash without a message: category %q (%s)", f.Category, f.Message)
	}
}

// TestReportHiddenFailure: a hidden listing that failed is a gap of the
// type it lists, since account objects in zone listings can no longer be
// told apart; an error meaning "wrong scope" is still dropped.
func TestReportHiddenFailure(t *testing.T) {
	zones, accounts := &Cloudflare.Scopes[1], &Cloudflare.Scopes[0]
	all := []block{
		{dsType: "cloudflare_rulesets", label: "by_account_id", scope: accounts, parent: "data.cloudflare_accounts.all", hidden: true, listings: []listing{{resourceType: "cloudflare_ruleset", scope: accounts}}},
		{dsType: "cloudflare_rulesets", label: "by_zone_id", scope: zones, parent: "data.cloudflare_zones.all", listings: []listing{{resourceType: "cloudflare_ruleset", scope: zones}}},
	}
	listed := map[string]bool{"cloudflare_ruleset": true}
	got := reportFailures([]failure{{block: "data.cloudflare_rulesets.by_account_id", address: `data.cloudflare_rulesets.by_account_id["a1"]`, message: "403 Forbidden"}}, all, listed, Cloudflare)
	if len(got) != 1 || got[0].Type != "cloudflare_ruleset" || !strings.HasPrefix(got[0].Message, hiddenNote) {
		t.Errorf("got %+v", got)
	}
	if got := reportFailures([]failure{{block: "data.cloudflare_rulesets.by_account_id", message: "missing required account_id parameter"}}, all, listed, Cloudflare); len(got) != 0 {
		t.Errorf("not applicable reported: %+v", got)
	}
}

// A failure of one zone names the zone; one of a whole block or of an
// unscoped block concerns every scope.
func TestFailureScopes(t *testing.T) {
	zones := &Scope{Arg: "zone_id", Parent: "cloudflare_zones"}
	all := []block{
		{dsType: "cloudflare_zones", label: "all"},
		{dsType: "cloudflare_dns_records", label: "z", scope: zones, parent: "data.cloudflare_zones.all"},
		// Read only to tell account objects from zone ones (account_id=).
		{dsType: "cloudflare_rulesets", label: "a", scope: &Scope{Arg: "account_id", Parent: "cloudflare_accounts"}, parent: "data.cloudflare_accounts.all", hidden: true},
	}
	names := map[string]map[string]string{"data.cloudflare_zones.all": {"z1": "example.com"}}
	failed := []tf.Failure{
		{Address: `data.cloudflare_dns_records.z["z1"]`},
		{Address: `data.cloudflare_dns_records.z["z2"]`},
		{Address: `data.cloudflare_dns_records.z`},
		{Address: `data.cloudflare_zones.all`},
		{Address: "-scan-scope zone_id=nope"},
		{Address: `data.cloudflare_rulesets.a["acc"]`},
	}
	failureScopes(failed, all, names)
	var got []string
	for _, f := range failed {
		got = append(got, f.Scope)
	}
	if want := []string{"zone example.com", "zone z2", "", "", "", ""}; !slices.Equal(got, want) {
		t.Errorf("scopes %q, want %q", got, want)
	}
}
