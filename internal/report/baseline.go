package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"

	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/state"
)

// Snapshot is a previous JSON report, used without running discovery.
type Snapshot struct {
	Orphans []model.Resource
	Summary Summary
}

func ReadSnapshot(path string, maxSize int64) (*Snapshot, error) {
	if path == "-" {
		return nil, errors.New("report must be a file path, not -")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := state.ReadLimited(f, maxSize)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var doc *struct {
		SchemaVersion int             `json:"schema_version"`
		Orphans       json.RawMessage `json:"orphans"`
		Summary       Summary         `json:"summary"`
	}
	if err := model.DecodeJSON(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: expected one JSON object: %w", path, err)
	}
	if doc == nil {
		return nil, fmt.Errorf("%s: expected one JSON object", path)
	}
	if doc.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("%s: unsupported schema_version %d (want %d)", path, doc.SchemaVersion, SchemaVersion)
	}
	if b := bytes.TrimSpace(doc.Orphans); len(b) == 0 || b[0] != '[' {
		return nil, fmt.Errorf("%s: orphans must be an array", path)
	}
	out := &Snapshot{Summary: doc.Summary}
	if err := model.DecodeJSON(doc.Orphans, &out.Orphans); err != nil {
		return nil, fmt.Errorf("%s: orphans: %w", path, err)
	}
	return out, nil
}

// Finding retains the baseline fields needed to identify and show an object.
type Finding struct {
	Type           string         `json:"type"`
	Identity       map[string]any `json:"identity"`
	Scope          string         `json:"scope,omitempty"`
	ProviderSource string         `json:"provider_source"`
	DisplayName    string         `json:"display_name"`
	ImportID       string         `json:"import_id"`
	Source         string         `json:"source"`
	ListAddress    string         `json:"list_address"`
	Reason         string         `json:"reason,omitempty"`
}

func finding(r model.Resource, reason string) Finding {
	return Finding{r.Type, r.Identity, r.ScopeName, r.ProviderSource, r.DisplayName, r.ImportID, r.Source, r.ListAddress, reason}
}

type findingKey struct {
	typ, identity, scope string
}

func keyFinding(r model.Resource) (findingKey, bool) {
	var identity string
	switch {
	case len(r.Identity) > 0:
		b, err := json.Marshal(r.Identity)
		if err != nil {
			return findingKey{}, false
		}
		identity = string(b)
	case r.ImportID != "":
		b, _ := json.Marshal(r.ImportID)
		identity = string(b)
	default:
		return findingKey{}, false
	}
	return findingKey{r.Type, identity, r.ScopeName}, true
}

// findingProviders holds the provider families seen for one key; repeated
// keys from a crafted report must not make each lookup scan a long list.
type findingProviders struct {
	unknown  bool
	families map[string]bool
}

type findingIndex map[findingKey]*findingProviders

func indexFindings(rs []model.Resource) findingIndex {
	idx := findingIndex{}
	for _, r := range rs {
		k, ok := keyFinding(r)
		if !ok {
			continue
		}
		p := idx[k]
		if p == nil {
			p = &findingProviders{families: map[string]bool{}}
			idx[k] = p
		}
		if r.ProviderSource == "" {
			p.unknown = true
		} else {
			p.families[model.ProviderFamily(r.ProviderSource)] = true
		}
	}
	return idx
}

// contains follows model.SameProvider: an unknown provider matches any.
func (idx findingIndex) contains(r model.Resource) bool {
	k, ok := keyFinding(r)
	if !ok {
		return false
	}
	p := idx[k]
	if p == nil {
		return false
	}
	return r.ProviderSource == "" || p.unknown || p.families[model.ProviderFamily(r.ProviderSource)]
}

type Baseline struct {
	New                int       `json:"new"`
	Known              int       `json:"known"`
	Resolved           int       `json:"resolved"`
	Unverified         int       `json:"unverified"`
	ResolvedFindings   []Finding `json:"resolved_findings"`
	UnverifiedFindings []Finding `json:"unverified_findings"`
}

// Compare marks current orphans and checks missing findings against listings.
// An object without an identity or import ID can never be matched or resolved.
func Compare(old, current []model.Resource, oldSummary, currentSummary Summary) ([]model.Resource, Baseline) {
	before, now := indexFindings(old), indexFindings(current)
	marked := make([]model.Resource, len(current))
	b := Baseline{ResolvedFindings: []Finding{}, UnverifiedFindings: []Finding{}}
	for i, r := range current {
		r.Baseline = "new"
		if before.contains(r) {
			r.Baseline = "known"
			b.Known++
		} else {
			b.New++
		}
		marked[i] = r
	}
	oldListings := indexListings(oldSummary.Listings)
	newListings := indexListings(currentSummary.Listings)
	for _, r := range old {
		if _, ok := keyFinding(r); !ok {
			b.UnverifiedFindings = append(b.UnverifiedFindings, finding(r, "identity and import_id missing"))
			continue
		}
		if now.contains(r) {
			continue
		}
		reason := missingReason(r, oldListings, newListings)
		if reason == "" {
			b.ResolvedFindings = append(b.ResolvedFindings, finding(r, ""))
		} else {
			b.UnverifiedFindings = append(b.UnverifiedFindings, finding(r, reason))
		}
	}
	b.Resolved, b.Unverified = len(b.ResolvedFindings), len(b.UnverifiedFindings)
	return marked, b
}

var pluginSource = regexp.MustCompile(`^plugin[0-9]+$`)

// Listing addresses include instance keys, so another instance cannot supply
// evidence for the listing that found an object.
func indexListings(listings []Listing) map[[2]string]Listing {
	out := map[[2]string]Listing{}
	for _, l := range listings {
		out[[2]string{l.Source, l.Address}] = l
	}
	return out
}

func missingReason(r model.Resource, old, current map[[2]string]Listing) string {
	if pluginSource.MatchString(r.Source) {
		return "plugin results cannot be verified"
	}
	key := [2]string{r.Source, r.ListAddress}
	before, ok := old[key]
	if r.ListAddress == "" || !ok || before.Fingerprint == "" {
		return "no listing evidence"
	}
	now, ok := current[key]
	if !ok {
		return "not listed this run"
	}
	if !model.SameProvider(before.Provider, now.Provider) {
		return "listed by another provider"
	}
	if !now.Complete {
		return "listing incomplete"
	}
	if now.Fingerprint != before.Fingerprint {
		return "listing configuration changed"
	}
	return ""
}
