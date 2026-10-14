package report

import (
	"slices"
	"strings"

	"github.com/5xxeu/tforphans/internal/tf"
)

type ProviderCoverage struct {
	Provider      string `json:"provider"`
	ResourceTypes int    `json:"resource_types"`
	ListTypes     int    `json:"list_types"`
	QueriedTypes  int    `json:"queried_types"`
	queried       map[string]bool
}

// MergeProviders keeps the largest schema and the union of queried types
// across roots. Provider addresses are kept as returned by the schema.
func (s *Summary) MergeProviders(schemas *tf.Schemas, queried map[string][]string) {
	for addr, ps := range schemas.ProviderSchemas {
		i := slices.IndexFunc(s.Providers, func(p ProviderCoverage) bool { return p.Provider == addr })
		if i < 0 {
			i = len(s.Providers)
			s.Providers = append(s.Providers, ProviderCoverage{Provider: addr, queried: map[string]bool{}})
		}
		p := &s.Providers[i]
		if p.queried == nil {
			p.queried = map[string]bool{}
		}
		p.ResourceTypes = max(p.ResourceTypes, len(ps.ResourceSchemas))
		p.ListTypes = max(p.ListTypes, len(ps.ListResourceSchemas))
		for _, typ := range queried[addr] {
			p.queried[typ] = true
		}
		p.QueriedTypes = len(p.queried)
	}
	slices.SortFunc(s.Providers, func(a, b ProviderCoverage) int { return strings.Compare(a.Provider, b.Provider) })
}
