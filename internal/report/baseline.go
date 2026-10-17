package report

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"

	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/state"
)

// Snapshot is a previous JSON report, used without running discovery.
type Snapshot struct {
	Orphans []model.Resource
	Summary Summary
}

func ReadSnapshot(ctx context.Context, path string, maxSize int64) (*Snapshot, error) {
	if path == "-" {
		return nil, errors.New("report must be a file path, not -")
	}
	data, err := state.ReadFile(ctx, path, maxSize)
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return nil, err
	}
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
	// Transition says what became of a missing finding, when this run
	// knows: became_managed (StateAddress), ignored (Rule), uncertain, or
	// no_longer_found (its listing ran again without it).
	Transition   string `json:"transition,omitempty"`
	StateAddress string `json:"state_address,omitempty"`
	Rule         string `json:"rule,omitempty"`
}

func finding(r model.Resource, reason string) Finding {
	return Finding{Type: r.Type, Identity: r.Identity, Scope: r.ScopeName, ProviderSource: r.ProviderSource, DisplayName: r.DisplayName,
		ImportID: r.ImportID, Source: r.Source, ListAddress: r.ListAddress, Reason: reason}
}

// Seen is a live object of the current run that is no orphan: managed,
// ignored or uncertain.
type Seen struct {
	Resource     model.Resource
	Status       string
	StateAddress string
	Rule         string
}

// Transitions for missing findings.
const (
	BecameManaged = "became_managed"
	BecameIgnored = "ignored"
	BecameUnsure  = "uncertain"
	NoLongerFound = "no_longer_found"
)

// seenAs finds r among the objects seen this run (same key, compatible
// provider; the same listing when a location is unknown). Several matches give the least certain status: uncertain,
// then managed, then ignored.
func seenAs(r model.Resource, seen map[findingKey][]Seen) (Seen, bool) {
	k, ok := keyFinding(r)
	if !ok {
		return Seen{}, false
	}
	rank := map[string]int{BecameUnsure: 0, "managed": 1, BecameIgnored: 2}
	var best Seen
	found := false
	// With a location the listing left empty (project ""), equal keys of
	// another source may be another object (as in dedupe): only the same
	// listing says what became of this one.
	unknown := state.UnknownLocation(r.Identity)
	for _, s := range seen[k] {
		if !model.SameProvider(r.ProviderSource, s.Resource.ProviderSource) {
			continue
		}
		if unknown && (s.Resource.Source != r.Source || s.Resource.ListAddress != r.ListAddress) {
			continue
		}
		if !found || rank[s.Status] < rank[best.Status] {
			best, found = s, true
		}
	}
	return best, found
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
	// EnvironmentChanged: the baseline has another -environment (or one
	// of the two has none).
	EnvironmentChanged bool `json:"environment_changed,omitempty"`
}

// environmentChanged is the reason of every missing finding when the
// baseline comes from another environment.
const environmentChanged = "environment changed"

// Compare marks current orphans and checks missing findings against listings.
// An object without an identity or import ID can never be matched or resolved.
func Compare(old, current []model.Resource, oldSummary, currentSummary Summary) ([]model.Resource, Baseline) {
	return compare(old, current, oldSummary, currentSummary, nil, false)
}

// CompareRun is Compare during a run, which knows every live object: seen
// are those that are no orphans, and a missing finding among them says
// what became of it; one not among them was not found at all.
func CompareRun(old, current []model.Resource, oldSummary, currentSummary Summary, seen []Seen) ([]model.Resource, Baseline) {
	return compare(old, current, oldSummary, currentSummary, seen, true)
}

func compare(old, current []model.Resource, oldSummary, currentSummary Summary, seen []Seen, live bool) ([]model.Resource, Baseline) {
	seenIdx := map[findingKey][]Seen{}
	for _, s := range seen {
		if k, ok := keyFinding(s.Resource); ok {
			seenIdx[k] = append(seenIdx[k], s)
		}
	}
	before, now := indexFindings(old), indexFindings(current)
	marked := make([]model.Resource, len(current))
	b := Baseline{ResolvedFindings: []Finding{}, UnverifiedFindings: []Finding{}}
	// Objects of another environment can carry the same identities (the
	// same account under another label, or a copied setup): nothing there
	// makes a finding here known or a missing one resolved.
	b.EnvironmentChanged = oldSummary.Environment != currentSummary.Environment
	for i, r := range current {
		r.Baseline = "new"
		if !b.EnvironmentChanged && before.contains(r) {
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
		if b.EnvironmentChanged {
			b.UnverifiedFindings = append(b.UnverifiedFindings, finding(r, environmentChanged))
			continue
		}
		if _, ok := keyFinding(r); !ok {
			b.UnverifiedFindings = append(b.UnverifiedFindings, finding(r, "identity and import_id missing"))
			continue
		}
		if now.contains(r) {
			continue
		}
		// Seen this run as something else: that is the answer, whatever
		// the listings say.
		if s, ok := seenAs(r, seenIdx); ok {
			f := finding(r, "")
			switch s.Status {
			case "managed":
				f.Transition, f.StateAddress = BecameManaged, s.StateAddress
				b.ResolvedFindings = append(b.ResolvedFindings, f)
			case BecameIgnored:
				f.Transition, f.Rule = BecameIgnored, s.Rule
				b.ResolvedFindings = append(b.ResolvedFindings, f)
			default:
				f.Transition, f.StateAddress, f.Reason = BecameUnsure, s.StateAddress, "now uncertain"
				b.UnverifiedFindings = append(b.UnverifiedFindings, f)
			}
			continue
		}
		reason := missingReason(r, oldListings, newListings)
		if reason == "" {
			f := finding(r, "")
			if live {
				f.Transition = NoLongerFound
			}
			b.ResolvedFindings = append(b.ResolvedFindings, f)
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
