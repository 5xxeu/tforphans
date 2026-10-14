package dsource

import (
	"testing"

	"github.com/5xxeu/tforphans/internal/tf"
)

func TestScanListingInstances(t *testing.T) {
	a := Adapter{Name: "test", Provider: "hashicorp/foo"}
	b := block{dsType: "foo_things", label: "by_zone_id", scope: &Scope{Arg: "zone_id"}, parent: "data.foo_zones.all", listings: []listing{{resourceType: "foo_thing"}}}
	instances := map[string][]string{b.addr(): {"Z1", "Z2"}}
	failed := []tf.Failure{{Address: `data.foo_things.by_zone_id["Z2"]`, Type: "foo_thing", Message: "permission denied"}}
	got := scanListings(a, []block{b}, []block{b}, instances, failed, nil)
	if len(got) != 2 || got[0].Address != `data.foo_things.by_zone_id["Z1"]` || !got[0].Complete || got[1].Address != failed[0].Address || got[1].Complete || got[0].Fingerprint == got[1].Fingerprint {
		t.Fatalf("instance evidence: %+v", got)
	}
	failed[0].Address = b.addr()
	got = scanListings(a, []block{b}, []block{b}, instances, failed, nil)
	for _, l := range got {
		if l.Complete {
			t.Fatalf("block failure did not affect instance: %+v", l)
		}
	}
	failed[0].Message = "parent data.foo_zones.all failed: permission denied"
	got = scanListings(a, nil, []block{b}, nil, failed, nil)
	if len(got) != 0 {
		t.Fatalf("unstarted child listing recorded: %+v", got)
	}
}
