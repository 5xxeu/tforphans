package report

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"

	"github.com/5xxeu/tforphans/internal/model"
)

// Listing records one listing instance that actually started.
type Listing struct {
	Source      string            `json:"source"`
	Address     string            `json:"address"`
	Provider    string            `json:"provider"`
	Type        string            `json:"type"`
	Complete    bool              `json:"complete"`
	Fingerprint string            `json:"fingerprint"`
	Scope       map[string]string `json:"scope,omitempty"`
}

// ListingFingerprint hashes the configuration as written, without evaluating
// variables, locals, data sources or the provider configuration body.
func ListingFingerprint(config, key, provider, scan, workspace string) string {
	fields := map[string]string{
		"configuration": config, "instance_key": key,
		"provider": provider, "scan": scan,
	}
	if workspace != "" {
		fields["workspace"] = workspace
	}
	data, _ := json.Marshal(fields)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s *Summary) AddListing(l Listing) {
	if l.Provider != "" {
		l.Provider = model.ProviderFamily(l.Provider)
	}
	s.Listings = append(s.Listings, l)
}

func (s *Summary) SortListings() {
	slices.SortFunc(s.Listings, func(a, b Listing) int {
		if c := strings.Compare(a.Source, b.Source); c != 0 {
			return c
		}
		if c := strings.Compare(a.Address, b.Address); c != 0 {
			return c
		}
		return strings.Compare(a.Type, b.Type)
	})
}
