package cli

import (
	"maps"
	"slices"
	"strings"

	"github.com/5xxeu/tforphans/internal/dsource"
	"github.com/5xxeu/tforphans/internal/state"
)

func scanScope(o *options, a dsource.Adapter, idx *state.Index) map[string][]string {
	scope := maps.Clone(o.scanScope)
	if o.scopeState {
		if scope == nil {
			scope = map[string][]string{}
		}
		for _, sc := range a.Scopes {
			if _, ok := scope[sc.Arg]; !ok {
				// Parent objects (zones) are in state under their own
				// id; their children carry it as zone_id.
				vals := idx.Values("", sc.Arg)
				for _, v := range idx.Values(strings.TrimSuffix(sc.Parent, "s"), "id") {
					if !slices.Contains(vals, v) {
						vals = append(vals, v)
					}
				}
				scope[sc.Arg] = vals
			}
		}
	}
	return scope
}
