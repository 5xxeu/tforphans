package state

import (
	"testing"

	"github.com/5xxeu/tforphans/internal/model"
)

// FuzzParse: no state, pulled or shown, makes the parsers or the index
// panic, and every instance with an identity manages its own object.
func FuzzParse(f *testing.F) {
	for _, s := range []string{
		`{"version":4,"lineage":"l","serial":1,"resources":[{"mode":"managed","type":"vault_policy","name":"p","provider":"provider[\"registry.terraform.io/hashicorp/vault\"]","instances":[{"identity":{"id":"kept","n":12345678901234567890},"attributes":{"id":"kept"}}]}]}`,
		`{"version":4,"resources":[{"mode":"managed","type":"x","name":"a","provider":"provider.x","instances":[{"index_key":"k","attributes":{"id":"1","list":[1,{"a":null}]},"sensitive_attributes":[[{"type":"get_attr","value":"id"}]]}]}]}`,
		`{"version":4,"resources":[{"mode":"data","type":"x","name":"a","instances":[{}]},{"module":"module.m[0]","mode":"managed","type":"y","name":"b","instances":[{"identity":{"project":"","name":"n"}}]}]}`,
		`{"prior_state":{"values":{"root_module":{"resources":[{"address":"x.a","mode":"managed","type":"x","provider_name":"registry.terraform.io/hashicorp/x","identity":{"id":"i"},"values":{"id":"i"},"sensitive_values":{"id":true}}],"child_modules":[{"address":"module.c","resources":[]}]}}}}`,
		`{"version":4,"resources":[{"mode":"managed","type":"x","name":"a","instances":[{"attributes":{"id":"x"}}]},{"mode":"managed","type":"x","name":"b","instances":[{"identity":{"id":"x"},"attributes":{"id":"x"}}]}]}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var snapshots []Snapshot
		if s, err := ParseSnapshot(data, "pull"); err == nil {
			snapshots = append(snapshots, s)
		}
		if s, err := ParseShowSnapshot(data, "show"); err == nil {
			snapshots = append(snapshots, s)
		}
		_, _ = Parse(data)
		_, _ = ParseShow(data)
		ix := NewIndex()
		ix.AddSnapshots(snapshots)
		ix.Duplicates()
		for _, s := range snapshots {
			for _, in := range s.Instances {
				if !selfMatchable(in.Identity) {
					continue
				}
				// On its own: in the shared index another instance (one
				// without identity, matched by attributes) may come first.
				own := NewIndex()
				own.Add([]Instance{in})
				r := model.Resource{Type: in.Type, ProviderSource: in.Provider, Identity: in.Identity}
				ix.Match(r)
				if m := own.Match(r); !m.Managed || !m.ByIdentity {
					t.Fatalf("%s does not manage its own identity %v: %+v", in.Address, in.Identity, m)
				}
			}
		}
	})
}

// selfMatchable: an identity whose values are all known (a null or empty
// value is unknown and never confirms anything).
func selfMatchable(id map[string]any) bool {
	if len(id) == 0 {
		return false
	}
	for _, v := range id {
		if v == nil || v == "" {
			return false
		}
	}
	return true
}
