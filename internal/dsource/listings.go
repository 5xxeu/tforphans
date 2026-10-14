package dsource

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/5xxeu/tforphans/internal/report"
	"github.com/5xxeu/tforphans/internal/tf"
)

// scanListings records successful state instances and failed data reads.
// Recovery restrictions remain in the text of the block that finally ran.
func scanListings(a Adapter, blocks, all []block, instances map[string][]string, failures []tf.Failure, scope map[string][]string) []report.Listing {
	var out []report.Listing
	seen := map[[2]string]bool{}
	add := func(b block, address, key, typ string, complete bool) {
		if b.hidden {
			return
		}
		k := [2]string{address, typ}
		if seen[k] {
			return
		}
		seen[k] = true
		for _, f := range failures {
			if f.Type == typ && (f.Address == address || f.Address == b.addr()) {
				complete = false
			}
		}
		text := strings.TrimSpace(string(renderBlocks([]block{b})))
		scopeKey, _ := json.Marshal(struct {
			Instance string              `json:"instance"`
			Scope    map[string][]string `json:"scope"`
		}{key, scope})
		provider, _, _ := strings.Cut(b.dsType, "_")
		out = append(out, report.Listing{Source: "-scan " + a.Name, Address: address,
			Provider: a.Provider, Type: typ, Complete: complete,
			Fingerprint: report.ListingFingerprint(text, string(scopeKey), provider, a.Name)})
	}
	for _, b := range blocks {
		for _, key := range instances[b.addr()] {
			for _, l := range b.listings {
				address := b.addr()
				if b.scope != nil {
					address += "[" + strconv.Quote(key) + "]"
				}
				add(b, address, key, l.resourceType, true)
			}
		}
	}
	for _, f := range failures {
		if strings.HasPrefix(f.Message, "parent ") {
			continue
		}
		address, rawKey, _ := strings.Cut(f.Address, "[")
		key, _ := strconv.Unquote(strings.TrimSuffix(rawKey, "]"))
		for _, b := range all {
			if b.addr() == address {
				for _, l := range b.listings {
					if l.resourceType == f.Type {
						add(b, f.Address, key, l.resourceType, false)
					}
				}
			}
		}
	}
	return out
}
