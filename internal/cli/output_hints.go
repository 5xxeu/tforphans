package cli

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"

	"github.com/5xxeu/tforphans/internal/dsource"
	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/report"
)

// scanHints names providers of the roots that -scan could list but that
// were not scanned. The scan is not started on its own: it lists a whole
// account with credentials from the environment, which may not be the
// account of these roots.
func scanHints(src *sourceOptions, stateDirs []string) []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(sources)) {
		if slices.Contains(src.scans, name) {
			continue
		}
		a := sources[name]
		if root := dsource.VersionRoot(slices.Concat(src.roots, stateDirs), a.Provider); root != "" {
			out = append(out, fmt.Sprintf("%s uses %s, which has no list resources: -scan %s lists its objects through data sources (with credentials from the environment)", root, a.Provider, name))
		}
	}
	return out
}

// hintMin is how many unmanaged objects of one type in one scope make an
// ignore hint worth printing.
const hintMin = 20

// hintSafe reports whether a value can go into a suggested ignore line:
// it must stay one word (no space or control character of any kind) and
// match only itself (no glob or brace characters).
func hintSafe(v string) bool {
	return v != "" && !strings.ContainsAny(v, "*?[]\\{},") && !strings.ContainsFunc(v, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}

// ignoreHints suggests ignore lines for many unmanaged objects of one type
// in one scope (certificate packs a zone got automatically): the line
// hides every object of the type there, now and later, so it is only a
// suggestion for objects that are expected. Objects with a kind of their
// own (the type attribute: advanced or uploaded certificate packs, DNS
// record types) are grouped by it too, and the line names it, so that it
// does not hide a later object of another kind.
func ignoreHints(orphans []model.Resource) []string {
	type group struct {
		typ, scope, kind string
		rs               []model.Resource
	}
	var groups []*group
	byKey := map[string]*group{}
	for _, r := range orphans {
		kind, _ := model.Scalar(r.Attributes["type"])
		k := r.Type + "\x00" + r.ScopeName + "\x00" + kind
		if byKey[k] == nil {
			byKey[k] = &group{typ: r.Type, scope: r.ScopeName, kind: kind}
			groups = append(groups, byKey[k])
		}
		byKey[k].rs = append(byKey[k].rs, r)
	}
	var out []string
	for _, g := range groups {
		if len(g.rs) < hintMin || g.scope == "" {
			continue
		}
		// The identity key all of them share with one value (zone_id).
		cond := ""
		for _, k := range slices.Sorted(maps.Keys(g.rs[0].Identity)) {
			v, ok := model.Scalar(g.rs[0].Identity[k])
			if !ok || !hintSafe(v) || slices.Contains(g.rs[0].RequiredKeys, k) {
				continue
			}
			if !slices.ContainsFunc(g.rs, func(r model.Resource) bool { s, _ := model.Scalar(r.Identity[k]); return s != v }) {
				cond = k + "=" + v
				break
			}
		}
		if cond == "" {
			continue
		}
		what := g.typ
		if g.kind != "" {
			if !hintSafe(g.kind) {
				continue
			}
			cond += " attr.type=" + g.kind
			what += " (type " + g.kind + ")"
		}
		out = append(out, fmt.Sprintf("%d unmanaged %s in %s; if they are expected (created by the service, not by people), this line in the ignore file hides them, and any later ones there: %s %s", len(g.rs), what, g.scope, g.typ, cond))
		if len(out) == 3 {
			break
		}
	}
	return out
}

// presetHintMin is how many unmanaged objects an opt-in preset has to
// match for a note to name it.
const presetHintMin = 5

// presetHints names the opt-in presets (by names anyone could choose, so
// not applied by default) that would hide some of the unmanaged objects:
// a reader then learns that no ignore file of their own is needed. Only
// the attributes at hand are looked at; fetching more for a hint (attr.
// rules of a preset not applied) would slow every run, so the count is a
// lower bound.
func presetHints(orphans []model.Resource, applied []string) []string {
	var out []string
	for _, name := range report.Presets() {
		if slices.Contains(report.DefaultPresets, name) || slices.Contains(applied, name) {
			continue
		}
		pr, err := report.Preset(name)
		if err != nil {
			continue
		}
		n := 0
		for _, r := range orphans {
			if pr.Match(r) {
				n++
			}
		}
		if n >= presetHintMin {
			out = append(out, fmt.Sprintf("at least %d unmanaged objects match -ignore-preset %s; it hides them by names or flags anyone can set, so it is not applied unless asked for (see the README)", n, name))
		}
	}
	return out
}
