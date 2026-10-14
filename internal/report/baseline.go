package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

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
	Reason         string         `json:"reason,omitempty"`
}

func finding(r model.Resource, reason string) Finding {
	return Finding{r.Type, r.Identity, r.ScopeName, r.ProviderSource, r.DisplayName, r.ImportID, r.Source, reason}
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

type findingIndex map[findingKey][]string

func indexFindings(rs []model.Resource) findingIndex {
	idx := findingIndex{}
	for _, r := range rs {
		if k, ok := keyFinding(r); ok {
			idx[k] = append(idx[k], r.ProviderSource)
		}
	}
	return idx
}

func (idx findingIndex) contains(r model.Resource) bool {
	k, ok := keyFinding(r)
	if !ok {
		return false
	}
	for _, p := range idx[k] {
		if model.SameProvider(p, r.ProviderSource) {
			return true
		}
	}
	return false
}

type Baseline struct {
	New                int       `json:"new"`
	Known              int       `json:"known"`
	Resolved           int       `json:"resolved"`
	Unverified         int       `json:"unverified"`
	ResolvedFindings   []Finding `json:"resolved_findings"`
	UnverifiedFindings []Finding `json:"unverified_findings"`
}

// Compare marks current orphans and checks missing findings against coverage.
// An object without an identity or import ID can never be matched or resolved.
func Compare(old, current []model.Resource, s Summary) ([]model.Resource, Baseline) {
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
	byType := map[string][]Coverage{}
	for _, c := range s.Coverage {
		byType[c.Type] = append(byType[c.Type], c)
	}
	for _, r := range old {
		if _, ok := keyFinding(r); !ok {
			b.UnverifiedFindings = append(b.UnverifiedFindings, finding(r, "identity and import_id missing"))
			continue
		}
		if now.contains(r) {
			continue
		}
		reason := missingReason(r, byType[r.Type], s)
		if reason == "" {
			b.ResolvedFindings = append(b.ResolvedFindings, finding(r, ""))
		} else {
			b.UnverifiedFindings = append(b.UnverifiedFindings, finding(r, reason))
		}
	}
	b.Resolved, b.Unverified = len(b.ResolvedFindings), len(b.UnverifiedFindings)
	return marked, b
}

func missingReason(r model.Resource, rows []Coverage, s Summary) string {
	if strings.HasPrefix(r.Source, "plugin") && (s.Plugins == 0 || hasPluginFailure(s.Warnings)) {
		return "plugin failed or not run"
	}
	if r.ScopeName != "" {
		for _, c := range rows {
			if c.Scope == r.ScopeName {
				return rowReason(c)
			}
		}
		return "scope not listed"
	}
	if len(rows) == 0 {
		return "type not listed"
	}
	for _, c := range rows {
		if why := rowReason(c); why != "" {
			return why
		}
	}
	return ""
}

func rowReason(c Coverage) string {
	if c.Status == Complete {
		return ""
	}
	for _, why := range c.Reasons {
		if strings.HasPrefix(why, GapPrefix) {
			return why
		}
	}
	if c.Status == Skipped && len(c.Reasons) > 0 {
		return GapPrefix + c.Reasons[0]
	}
	return "listing incomplete"
}

func hasPluginFailure(warnings []string) bool {
	for _, w := range warnings {
		if strings.HasPrefix(w, "plugin") {
			return true
		}
	}
	return false
}
