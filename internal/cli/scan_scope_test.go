package cli

import (
	"reflect"
	"testing"

	"github.com/5xxeu/tforphans/internal/dsource"
	"github.com/5xxeu/tforphans/internal/state"
)

func TestScanScope(t *testing.T) {
	idx := state.NewIndex()
	idx.Add([]state.Instance{
		{Type: "cloudflare_zone", Attributes: map[string]any{"id": "parent"}},
		{Type: "cloudflare_dns_record", Identity: map[string]any{"zone_id": "identity"}},
		{Type: "cloudflare_dns_record", Attributes: map[string]any{"zone_id": "child"}},
		{Type: "cloudflare_dns_record", Attributes: map[string]any{"zone_id": "parent"}},
	})
	o := &options{src: sourceOptions{scopeState: true, scanScope: map[string][]string{"account_id": {}, "custom": {"explicit"}}}}
	got := scanScope(&o.src, dsource.Cloudflare, idx)
	want := map[string][]string{"zone_id": {"child", "identity", "parent"}, "account_id": {}, "custom": {"explicit"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v want %v", got, want)
	}
	if _, ok := o.src.scanScope["zone_id"]; ok {
		t.Fatal("explicit scope mutated")
	}
	o.src.scanScope["zone_id"] = []string{}
	if got := scanScope(&o.src, dsource.Cloudflare, idx); len(got["zone_id"]) != 0 {
		t.Fatalf("explicit empty scope replaced: %v", got)
	}
	o.src.scopeState = false
	if got := scanScope(&o.src, dsource.Cloudflare, idx); !reflect.DeepEqual(got, o.src.scanScope) {
		t.Fatalf("state scope applied without request: %v", got)
	}
}
