package report

import (
	"slices"
	"strings"

	"github.com/5xxeu/tforphans/internal/model"
)

// Listing records actual checks and scopes from explicit checks or their
// returned objects.
type Listing struct {
	Source   string   `json:"source"`
	Provider string   `json:"provider"`
	Type     string   `json:"type"`
	Scopes   []string `json:"scopes"`
	Complete bool     `json:"complete"`
}

// AddListing combines instances of the same source, provider family and type.
// Any incomplete instance keeps the combined listing incomplete.
func (s *Summary) AddListing(source, provider, typ, scope string, complete bool) {
	if provider != "" {
		provider = model.ProviderFamily(provider)
	}
	i := slices.IndexFunc(s.Listings, func(l Listing) bool {
		return l.Source == source && l.Provider == provider && l.Type == typ
	})
	if i < 0 {
		i = len(s.Listings)
		s.Listings = append(s.Listings, Listing{Source: source, Provider: provider, Type: typ, Scopes: []string{}, Complete: true})
	}
	l := &s.Listings[i]
	l.Complete = l.Complete && complete
	if scope != "" && !slices.Contains(l.Scopes, scope) {
		l.Scopes = append(l.Scopes, scope)
		slices.Sort(l.Scopes)
	}
}

func (s *Summary) SortListings() {
	slices.SortFunc(s.Listings, func(a, b Listing) int {
		if c := strings.Compare(a.Source, b.Source); c != 0 {
			return c
		}
		if c := strings.Compare(a.Provider, b.Provider); c != 0 {
			return c
		}
		return strings.Compare(a.Type, b.Type)
	})
}
