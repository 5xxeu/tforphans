package tf

import (
	"bytes"
	"reflect"
	"testing"
	"testing/iotest"

	"github.com/5xxeu/tforphans/internal/model"
)

// FuzzParseQuery: terraform query output read whole or one byte at a time
// gives the same result, and no output makes the parser panic.
func FuzzParseQuery(f *testing.F) {
	for _, s := range []string{
		`{"list_start":{"address":"list.fake_thing.t","resource_type":"fake_thing"},"type":"list_start"}
{"list_resource_found":{"address":"list.fake_thing.t","display_name":"a","identity":{"id":"a","n":12345678901234567890},"identity_version":0,"resource_type":"fake_thing","resource_object":{"tags":{"Name":"web"},"type":"t"}},"type":"list_resource_found"}
{"list_complete":{"address":"list.fake_thing.t","resource_type":"fake_thing","total":1},"type":"list_complete"}
`,
		`{"list_start":{"address":"list.fake_broken.b[\"k\"]","resource_type":"fake_broken"},"type":"list_start"}
{"@level":"error","@message":"Error: denied","diagnostic":{"severity":"error","summary":"denied","address":"list.fake_broken.b[\"k\"]"},"type":"diagnostic"}
plain text line
`,
		`{"@level":"error","diagnostic":{"severity":"error","summary":"bad","detail":"x","range":{"filename":"main.tf"}},"type":"diagnostic"}`,
		`{"list_complete":{"address":"list.x.y","resource_type":"x","total":100},"type":"list_complete"}` + "\n{",
	} {
		f.Add([]byte(s))
	}
	keys := func(block, typ string) ([]string, []string) { return []string{"id"}, nil }
	limit := func(string) int { return 100 }
	f.Fuzz(func(t *testing.T, data []byte) {
		whole, wErrs, wErr := ParseQueryStream(data, "root", keys, limit)
		var seen []model.Resource
		bytewise, bErrs, bErr := ParseQueryReader(iotest.OneByteReader(bytes.NewReader(data)), "root", keys, limit, func(r *model.Resource) { seen = append(seen, *r) })
		if (wErr == nil) != (bErr == nil) || wErr != nil && wErr.Error() != bErr.Error() {
			t.Fatalf("errors differ: %v / %v", wErr, bErr)
		}
		if !reflect.DeepEqual(whole, bytewise) || !reflect.DeepEqual(wErrs, bErrs) {
			t.Fatalf("results differ:\n%+v\n%+v", whole, bytewise)
		}
		if bytewise != nil && len(seen) != len(bytewise.Found) {
			t.Fatalf("callback saw %d of %d", len(seen), len(bytewise.Found))
		}
	})
}
