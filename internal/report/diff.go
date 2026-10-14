package report

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/5xxeu/tforphans/internal/model"
)

type CounterChange struct {
	Old int `json:"old"`
	New int `json:"new"`
}

type CoverageChange struct {
	Type  string `json:"type"`
	Scope string `json:"scope"`
	Old   string `json:"old"`
	New   string `json:"new"`
}

type CompleteChange struct {
	Old bool `json:"old"`
	New bool `json:"new"`
}

type Diff struct {
	Added           []model.Resource         `json:"added"`
	Resolved        []Finding                `json:"resolved"`
	Unverified      []Finding                `json:"unverified"`
	Counters        map[string]CounterChange `json:"counters"`
	CoverageChanges []CoverageChange         `json:"coverage_changes"`
	Complete        CompleteChange           `json:"complete"`
}

var diffCounters = []string{"live", "managed", "uncertain", "ignored", "orphans", "states", "queried_types"}

func CompareSnapshots(old, current Snapshot) Diff {
	marked, b := Compare(old.Orphans, current.Orphans, old.Summary, current.Summary)
	d := Diff{
		Added: []model.Resource{}, Resolved: b.ResolvedFindings, Unverified: b.UnverifiedFindings,
		Counters: map[string]CounterChange{}, CoverageChanges: []CoverageChange{},
		Complete: CompleteChange{old.Summary.Complete, current.Summary.Complete},
	}
	for i, r := range marked {
		if r.Baseline == "new" {
			d.Added = append(d.Added, current.Orphans[i])
		}
	}
	counts := func(s Summary) []int {
		return []int{s.Live, s.Managed, s.Uncertain, s.Ignored, s.Orphans, s.States, s.Queried}
	}
	a, z := counts(old.Summary), counts(current.Summary)
	for i, name := range diffCounters {
		d.Counters[name] = CounterChange{a[i], z[i]}
	}
	rows := map[[2]string]CoverageChange{}
	for _, c := range old.Summary.Coverage {
		rows[[2]string{c.Type, c.Scope}] = CoverageChange{c.Type, c.Scope, c.Status, "absent"}
	}
	for _, c := range current.Summary.Coverage {
		key := [2]string{c.Type, c.Scope}
		row, ok := rows[key]
		if !ok {
			row = CoverageChange{Type: c.Type, Scope: c.Scope, Old: "absent"}
		}
		row.New = c.Status
		rows[key] = row
	}
	for _, row := range rows {
		if row.Old != row.New {
			d.CoverageChanges = append(d.CoverageChanges, row)
		}
	}
	slices.SortFunc(d.CoverageChanges, func(a, b CoverageChange) int {
		if c := strings.Compare(a.Type, b.Type); c != 0 {
			return c
		}
		return strings.Compare(a.Scope, b.Scope)
	})
	return d
}

func (d Diff) JSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(d)
}

func (d Diff) Text(w io.Writer, verbose bool) error {
	limit := 8
	if verbose {
		limit = -1
	}
	added := make([]Finding, len(d.Added))
	for i, r := range d.Added {
		added[i] = finding(r, "")
	}
	for _, group := range []struct {
		name string
		rs   []Finding
	}{{"added", added}, {"resolved", d.Resolved}, {"not verified", d.Unverified}} {
		if _, err := fmt.Fprintf(w, "%s (%d):\n", group.name, len(group.rs)); err != nil {
			return err
		}
		if err := PrintFindings(w, group.rs, limit); err != nil {
			return err
		}
	}
	fmt.Fprintln(w, "counters:")
	for _, name := range diffCounters {
		c := d.Counters[name]
		if c.Old != c.New {
			fmt.Fprintf(w, "  %s: %d to %d\n", name, c.Old, c.New)
		}
	}
	fmt.Fprintln(w, "coverage:")
	for i, c := range d.CoverageChanges {
		if limit >= 0 && i == limit {
			fmt.Fprintf(w, "  and %d more\n", len(d.CoverageChanges)-i)
			break
		}
		fmt.Fprintf(w, "  %s: %s to %s\n", findingLabel(c.Type, "", c.Scope), Clean(c.Old), Clean(c.New))
	}
	_, err := fmt.Fprintf(w, "complete: %t to %t\n", d.Complete.Old, d.Complete.New)
	return err
}

// PrintFindings uses the report's identity formatting and an optional cap.
func PrintFindings(w io.Writer, rs []Finding, limit int) error {
	for i, r := range rs {
		if limit >= 0 && i == limit {
			_, err := fmt.Fprintf(w, "  and %d more\n", len(rs)-i)
			return err
		}
		identity := (model.Resource{Identity: r.Identity}).IdentityString()
		if identity == "" {
			identity = r.ImportID
		}
		reason := ""
		if r.Reason != "" {
			reason = ": " + Clean(r.Reason)
		}
		if _, err := fmt.Fprintf(w, "  %s%s\n", findingLabel(r.Type, identity, r.Scope), reason); err != nil {
			return err
		}
	}
	return nil
}

func findingLabel(typ, identity, scope string) string {
	label := strings.TrimSpace(Clean(typ) + " " + Clean(identity))
	if scope != "" {
		label += " (" + Clean(scope) + ")"
	}
	return label
}
