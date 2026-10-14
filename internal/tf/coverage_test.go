package tf

import (
	"reflect"
	"testing"
)

func TestQueriedListings(t *testing.T) {
	data := `{"list_start":{"address":"list.foo_thing.a[\"one\"]","resource_type":"foo_thing"}}
{"list_start":{"address":"list.foo_thing.a[\"two\"]"}}
{"list_complete":{"address":"list.foo_thing.a[\"one\"]","resource_type":"foo_thing","total":0}}
{"diagnostic":{"severity":"error","summary":"denied","address":"list.foo_thing.a[\"two\"]"}}
{"diagnostic":{"severity":"error","summary":"lookup failed","address":"data.foo_regions.all"}}`
	res, fatal, err := ParseQueryStream([]byte(data), "root", noKeys, func(string) int { return 100 })
	want := []Listing{{`list.foo_thing.a["one"]`, "foo_thing"}, {`list.foo_thing.a["two"]`, "foo_thing"}}
	if err != nil || len(fatal) != 0 || !reflect.DeepEqual(res.Queried, want) || len(res.DataErrors) != 1 {
		t.Fatalf("queried listings: %+v, fatal %v, err %v", res, fatal, err)
	}
	// A query file whose blocks expand to no instances queried no types.
	res, fatal, err = ParseQueryStream([]byte(`{"type":"version"}`), "root", noKeys, func(string) int { return 100 })
	if err != nil || len(fatal) != 0 || len(res.Queried) != 0 {
		t.Fatalf("empty listings: %+v, fatal %v, err %v", res, fatal, err)
	}
}
