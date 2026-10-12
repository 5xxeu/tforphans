package dsource

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/tf"
)

// parentIDs returns the IDs a parent block listed (its for_each keys),
// only those with an ID or name in only when it is set.
func parentIDs(stateData []byte, addr string, only []string) ([]string, error) {
	var st struct {
		Resources []struct {
			Mode      string `json:"mode"`
			Type      string `json:"type"`
			Name      string `json:"name"`
			Instances []struct {
				Attributes map[string]any `json:"attributes"`
			} `json:"instances"`
		} `json:"resources"`
	}
	if err := model.DecodeJSON(stateData, &st); err != nil {
		return nil, fmt.Errorf("decode data source state: %w", err)
	}
	var out []string
	for _, r := range st.Resources {
		if r.Mode != "data" || "data."+r.Type+"."+r.Name != addr {
			continue
		}
		for _, in := range r.Instances {
			for _, it := range listItems(in.Attributes, ListSpec{Items: "result"}) {
				id, _ := it["id"].(string)
				name, _ := it["name"].(string)
				if id == "" || slices.Contains(out, id) || len(only) > 0 && !slices.Contains(only, id) && (name == "" || !slices.Contains(only, name)) {
					continue
				}
				out = append(out, id)
			}
		}
	}
	return out, nil
}

// unknownScopes reports -scan-scope values that name no parent the scan
// read: a typo would otherwise look like a zone without objects.
func unknownScopes(stateData []byte, blocks []block, a Adapter, limit map[string][]string) ([]tf.Failure, error) {
	var out []tf.Failure
	for _, sc := range a.Scopes {
		vals := limit[sc.Arg]
		addr := "data." + sc.Parent + ".all"
		if len(vals) == 0 || !slices.ContainsFunc(blocks, func(b block) bool { return b.addr() == addr }) {
			continue
		}
		ids, err := parentIDs(stateData, addr, nil)
		if err != nil {
			return nil, err
		}
		for _, v := range vals {
			if found, err := parentIDs(stateData, addr, []string{v}); err != nil {
				return nil, err
			} else if len(found) == 0 {
				out = append(out, tf.Failure{Address: "-scan-scope " + sc.Arg + "=" + v, Type: strings.TrimSuffix(sc.Parent, "s"),
					Message: fmt.Sprintf("not found: no %s with this ID or name among the %d %s listed", strings.TrimSuffix(sc.Arg, "_id"), len(ids), addr)})
			}
		}
	}
	return out, nil
}

// parentNames returns the names of the parents (zones, accounts) by ID, by
// parent block address.
func parentNames(stateData []byte) (map[string]map[string]string, error) {
	names := map[string]map[string]string{}
	if stateData == nil {
		return names, nil
	}
	var st struct {
		Resources []struct {
			Mode      string `json:"mode"`
			Type      string `json:"type"`
			Name      string `json:"name"`
			Instances []struct {
				Attributes map[string]any `json:"attributes"`
			} `json:"instances"`
		} `json:"resources"`
	}
	if err := model.DecodeJSON(stateData, &st); err != nil {
		return nil, fmt.Errorf("decode data source state: %w", err)
	}
	for _, r := range st.Resources {
		addr := "data." + r.Type + "." + r.Name
		if r.Mode != "data" || r.Name != "all" {
			continue
		}
		for _, in := range r.Instances {
			for _, it := range listItems(in.Attributes, ListSpec{Items: "result"}) {
				id, _ := it["id"].(string)
				name, _ := it["name"].(string)
				if id != "" {
					if names[addr] == nil {
						names[addr] = map[string]string{}
					}
					names[addr][id] = name
				}
			}
		}
	}
	return names, nil
}

// failureScopes names the scope of failures of one instance of a scoped
// block (one zone); a failure of a whole block concerns every scope.
func failureScopes(failed []tf.Failure, all []block, names map[string]map[string]string) {
	byAddr := map[string]block{}
	for _, b := range all {
		byAddr[b.addr()] = b
	}
	for i := range failed {
		f := &failed[i]
		addr, key, ok := strings.Cut(f.Address, "[")
		b, known := byAddr[addr]
		if !ok || !known || b.scope == nil {
			continue
		}
		if id, err := strconv.Unquote(strings.TrimSuffix(key, "]")); err == nil {
			f.Scope = scopeLabel(b.scope, id, names[b.parent][id])
		}
	}
}

// scopeLabel names a parent for people: "zone example.com", or the ID
// when the parent has no name.
func scopeLabel(sc *Scope, id, name string) string {
	if name == "" {
		name = id
	}
	return strings.TrimSuffix(sc.Arg, "_id") + " " + name
}

// limitScope applies -scan-scope: blocks iterating over a limited scope
// read only the parents given, and a scope given with no values is not read
// at all (its types are reported as skipped).
func limitScope(blocks []block, limit map[string][]string) ([]block, []tf.Skip) {
	if len(limit) == 0 {
		return blocks, nil
	}
	scopes := map[string]int{} // data source -> scoped blocks
	for _, b := range blocks {
		if b.scope != nil {
			scopes[b.dsType]++
		}
	}
	var out []block
	var skipped []tf.Skip
	for _, b := range blocks {
		if b.scope != nil {
			if vals, ok := limit[b.scope.Arg]; ok {
				if len(vals) == 0 && scopes[b.dsType] > 1 {
					// The other scope's listing may return these objects
					// too; reading them lets collapse drop those.
					b.hidden = true
					out = append(out, b)
					continue
				}
				if len(vals) == 0 {
					for _, l := range b.listings {
						skipped = append(skipped, tf.Skip{Type: l.resourceType, Reason: "-scan-scope names no " + b.scope.Arg, Excluded: true})
					}
					continue
				}
				b.only = vals
			}
		}
		out = append(out, b)
	}
	return out, skipped
}
