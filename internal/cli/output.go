package main

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"unicode"

	"github.com/5xxeu/tforphans/internal/dsource"
	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/report"
	"github.com/5xxeu/tforphans/internal/state"
	"github.com/5xxeu/tforphans/internal/tf"
)

// runPlan says what the run checks, before the slow part starts: which
// roots are queried, which states are read and what else lists objects.
func runPlan(o *options, stateDirs []string) string {
	var parts []string
	if o.configFile != "" {
		parts = append(parts, "flags from "+o.configFile)
	}
	if !o.noQuery {
		parts = append(parts, fmt.Sprintf("terraform query in %s", strings.Join(o.roots, ", ")))
	} else if len(o.roots) > 0 {
		parts = append(parts, fmt.Sprintf("states of %s", strings.Join(o.roots, ", ")))
	}
	if len(stateDirs) > 0 {
		parts = append(parts, fmt.Sprintf("states of %d more root modules under -state-roots", len(stateDirs)))
	}
	if n := len(o.states); n > 0 {
		parts = append(parts, fmt.Sprintf("%d -state", n))
	}
	for _, name := range o.scans {
		what := "whole account"
		if o.scanScope != nil || o.scopeState {
			what = "limited by -scan-scope"
		}
		parts = append(parts, fmt.Sprintf("-scan %s (provider from the environment, %s)", name, what))
	}
	if n := len(o.plugins); n > 0 {
		parts = append(parts, fmt.Sprintf("%d plugins", n))
	}
	if !o.noDefaultPresets {
		parts = append(parts, "default ignore presets (objects the cloud creates; -no-default-presets shows them)")
	}
	return strings.Join(parts, "; ")
}

// scanHints names providers of the roots that -scan could list but that
// were not scanned. The scan is not started on its own: it lists a whole
// account with credentials from the environment, which may not be the
// account of these roots.
func scanHints(o *options, stateDirs []string) []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(sources)) {
		if slices.Contains(o.scans, name) {
			continue
		}
		a := sources[name]
		if root := dsource.VersionRoot(slices.Concat(o.roots, stateDirs), a.Provider); root != "" {
			out = append(out, fmt.Sprintf("%s uses %s, which has no list resources: -scan %s lists its objects through data sources (with credentials from the environment)", root, a.Provider, name))
		}
	}
	return out
}

// scopeName names the account of an object from its identity, for people:
// "project my-project"; "" when the identity does not say.
// sortResources orders objects by scope, type and name.
func sortResources(rs []model.Resource) { slices.SortStableFunc(rs, compareResources) }

func compareResources(a, b model.Resource) int {
	if c := strings.Compare(a.ScopeName, b.ScopeName); c != 0 {
		return c
	}
	if c := strings.Compare(a.Type, b.Type); c != 0 {
		return c
	}
	// The bare name ignores display details such as a guest's node.
	// Break ties by source, provider and identity so import suffixes
	// _2, _3 do not depend on the API's listing order.
	if c := strings.Compare(baseName(a), baseName(b)); c != 0 {
		return c
	}
	if c := strings.Compare(a.Source, b.Source); c != 0 {
		return c
	}
	if c := strings.Compare(a.Provider, b.Provider); c != 0 {
		return c
	}
	if c := strings.Compare(a.ProviderSource, b.ProviderSource); c != 0 {
		return c
	}
	if c := strings.Compare(a.ImportID, b.ImportID); c != 0 {
		return c
	}
	if c := strings.Compare(a.IdentityString(), b.IdentityString()); c != 0 {
		return c
	}
	// Quoted keys and values distinguish identities with separators in them.
	return strings.Compare(a.Key(), b.Key())
}

func baseName(r model.Resource) string {
	if r.BaseName != "" {
		return r.BaseName
	}
	return r.DisplayName
}

func scopeName(identity map[string]any) string {
	var parts []string
	for _, k := range scopeKeys {
		if v, ok := model.Scalar(identity[k]); ok && v != "" {
			parts = append(parts, k+" "+v)
		}
	}
	return strings.Join(parts, ", ")
}

// printScopes prints, per source, the accounts its results came from: with
// credentials from the environment, a root can list another project than
// the one its configuration manages.
func printScopes(w io.Writer, live []model.Resource) {
	w = report.LazySection(w, "Result scopes")
	seen := map[string]map[string]bool{}
	var sources []string
	for _, r := range live {
		for _, k := range scopeKeys {
			v, ok := model.Scalar(r.Identity[k])
			if !ok || v == "" {
				continue
			}
			if seen[r.Source] == nil {
				seen[r.Source] = map[string]bool{}
				sources = append(sources, r.Source)
			}
			seen[r.Source][k+" "+v] = true
		}
	}
	slices.Sort(sources)
	for _, src := range sources {
		vals := slices.Sorted(maps.Keys(seen[src]))
		fmt.Fprintf(w, "  %s: results from %s\n", report.Clean(src), report.Clean(strings.Join(vals, ", ")))
	}
}

// nothingChecked reports a run that could not have found anything: no list
// query or data source scan listed a type, and no plugin ran. To a CI job exit 0 would mean that
// nothing is unmanaged.
func nothingChecked(o *options, s report.Summary) bool {
	// Empty plugin answers without done are recorded as failed sources.
	return !hasDiscovery(s.Ran, len(o.plugins)) && !o.printQuery
}

// hasDiscovery accepts either planned listings or listings that actually ran.
func hasDiscovery(listings, plugins int) bool {
	return listings != 0 || plugins != 0
}

// printSummary writes the report to stderr: summary, diagnostics, the
// blocks details adds, and coverage last. Only the error of details is
// returned; the report itself does not fail a run on a closed stderr.
func printSummary(w io.Writer, o *options, s report.Summary, details func(io.Writer) error) error {
	fmt.Fprintln(w)
	report.Section(w, "Summary")
	fmt.Fprintf(w, "%d live, %s managed, %s uncertain, %d ignored, %s unmanaged\n",
		s.Live, report.Accent(w, report.Good, fmt.Sprint(s.Managed)),
		report.Accent(w, report.Attention, fmt.Sprint(s.Uncertain)), s.Ignored,
		report.Accent(w, report.Attention, fmt.Sprint(s.Orphans)))
	fmt.Fprintf(w, "%d states read, %d resource types queried, %d skipped", s.States, s.Queried, len(s.Skipped))
	if len(s.Skipped) > 0 && !o.verbose {
		fmt.Fprint(w, " (-v to list)")
	}
	fmt.Fprintln(w)
	for _, p := range s.Providers {
		fmt.Fprintf(w, "%s: %d of %d resource types can be listed, %d queried\n", report.Clean(strings.TrimPrefix(p.Provider, "registry.terraform.io/")), p.ListTypes, p.ResourceTypes, p.QueriedTypes)
	}
	if s.Managed > 0 {
		// How certain "managed" is: an identity names the object exactly,
		// attributes may miss a key that is not an attribute.
		fmt.Fprintf(w, "managed objects matched by identity: %d, by attributes: %d\n", s.ManagedByIdentity, s.Managed-s.ManagedByIdentity)
	}
	if o.verbose {
		for _, sk := range s.Skipped {
			where := ""
			if sk.Root != "" {
				where = " in " + report.Clean(sk.Root)
			}
			fmt.Fprintf(w, "  skipped %s%s: %s\n", report.Clean(sk.Type), where, report.Clean(sk.Reason)+reasonLink(sk.ReasonCode))
		}
		for _, p := range slices.Compact(slices.Sorted(slices.Values(s.NoList))) {
			fmt.Fprintf(w, "  no list resources: %s\n", report.Clean(p))
		}
	}
	printDiagnostics(report.LazySection(w, "Diagnostics"), o, s)
	if details != nil {
		// Before coverage, which stays the last line.
		if err := details(w); err != nil {
			return err
		}
	}
	fmt.Fprintln(w)
	report.Section(w, "Coverage")
	// The last line says whether "nothing unmanaged" can be trusted.
	// Complete covers what can be listed; say what could not be.
	var out []string
	gapTypes, argTypes := map[string]bool{}, map[string]bool{}
	for _, c := range s.Coverage {
		switch {
		case c.Status == report.Skipped:
			out = append(out, c.Type)
		case slices.ContainsFunc(c.Reasons, func(r string) bool { return strings.HasPrefix(r, report.GapPrefix) }):
			gapTypes[c.Type] = true
			if slices.ContainsFunc(c.Reasons, func(r string) bool { return strings.HasPrefix(r, report.GapPrefix+tf.MissingArgs) }) {
				argTypes[c.Type] = true
			}
		}
	}
	// What no run with these sources can check, as opposed to the gaps of
	// this run below.
	var beyond []string
	if len(out) > 0 {
		beyond = append(beyond, fmt.Sprintf("%d types left out on purpose or without any listing", len(out)))
	}
	if len(s.NoList) > 0 {
		beyond = append(beyond, fmt.Sprintf("%d providers without list resources", len(slices.Compact(slices.Sorted(slices.Values(s.NoList))))))
	}
	if n := len(s.UnlistedTypes); n > 0 {
		beyond = append(beyond, fmt.Sprintf("%d types in the states that their provider cannot list", n))
	}
	if s.APIOff > 0 && !o.strictAPI {
		beyond = append(beyond, fmt.Sprintf("%d listings whose API is not enabled (-strict-api=false left them out)", s.APIOff))
	}
	if s.StrictIgnored > 0 {
		beyond = append(beyond, fmt.Sprintf("%d failed listings or skipped types of -strict-ignore", s.StrictIgnored))
	}
	if s.Complete {
		if len(beyond) == 0 {
			fmt.Fprintln(w, "coverage: complete")
			return nil
		}
		fmt.Fprintf(w, "coverage: complete for what can be listed; not checked: %s (-v lists them)\n", strings.Join(beyond, ", "))
		return nil
	}
	var gaps []string
	failed := 0
	for _, f := range s.Failed {
		if (f.Category != tf.FailAPIOff || o.strictAPI) && !o.strictIgnore.Match(f.Type) {
			failed++
		}
	}
	if n := failed; n > 0 {
		gaps = append(gaps, fmt.Sprintf("%d failed or truncated listings", n))
	}
	switch n, a := len(gapTypes), len(argTypes); {
	case n > 0 && a == n:
		gaps = append(gaps, fmt.Sprintf("%d types skipped that need an argument (-arg NAME=...; -v lists them)", n))
	case a > 0:
		gaps = append(gaps, fmt.Sprintf("%d types skipped that needed a check, %d of them for want of an argument (-arg NAME=...; -v lists them)", n, a))
	case n > 0:
		gaps = append(gaps, fmt.Sprintf("%d types skipped that needed a check", n))
	}
	if n := len(s.UnreadStates); n > 0 {
		// Name them: a count alone sends people through every root.
		var which []string
		for _, u := range s.UnreadStates {
			which = append(which, u.Root+": "+u.Error)
		}
		gaps = append(gaps, fmt.Sprintf("%d states not read: %s", n, report.Clean(strings.Join(firstN(which, 5), "; "))))
	}
	if n := len(s.Warnings); n > 0 {
		gaps = append(gaps, fmt.Sprintf("%d other warnings", n))
	}
	if len(gaps) == 0 {
		gaps = append(gaps, "nothing was listed")
	}
	effect := "unmanaged objects there may be missing from the result"
	if len(s.UnreadStates) > 0 {
		effect += ", and objects of unread states may show as unmanaged"
	}
	if len(beyond) > 0 {
		fmt.Fprintf(w, "not checkable with these sources, not counted as gaps: %s (-v lists them)\n", strings.Join(beyond, ", "))
	}
	fmt.Fprintf(w, "coverage: INCOMPLETE (%s): %s; -strict makes this exit 1\n", strings.Join(gaps, ", "), effect)
	return nil
}

// printDiagnostics writes to a section that gets its heading on the first
// line, so a new kind of diagnostic cannot end up under the summary.
func printDiagnostics(w io.Writer, o *options, s report.Summary) {
	printFailed(w, o, s.Failed)
	// The codes are in JSON; their pages only with -v, where the lines
	// that name them are printed too.
	links := map[string]bool{}
	for _, c := range s.Coverage {
		for _, d := range c.ReasonDetails {
			if o.verbose && !links[d.ReasonCode] {
				links[d.ReasonCode] = true
				fmt.Fprintf(w, "  reason %s: %s\n", d.ReasonCode, model.ReasonLink(d.ReasonCode))
			}
		}
	}
	printOwnership(w, s.Ownership, s.InState)
	for _, msg := range s.Warnings {
		fmt.Fprintln(w, "warning:", report.Clean(msg))
	}
	for _, msg := range s.Notes {
		fmt.Fprintln(w, "note:", report.Clean(msg))
	}
	if s.Uncertain > 0 {
		where := "they are listed in the output"
		if o.adopt || o.format == "summary" {
			where = "-format json or text lists them"
		}
		fmt.Fprintf(w, "warning: %d objects are uncertain (%s).\n", s.Uncertain, where)
		fmt.Fprintln(w, "  They match state only by ID; their account, subscription or cluster could not be checked.")
		fmt.Fprintln(w, "  With states from several accounts or clusters, these objects may be unmanaged.")
		fmt.Fprintln(w, "  State without resource identity:")
		fmt.Fprintln(w, "    Pass -refresh to read identities without writing, or record them with")
		fmt.Fprintln(w, "    terraform apply -refresh-only and a current provider.")
		fmt.Fprintln(w, "  Proxmox and plugins marked unverified (state does not record the cluster):")
		fmt.Fprintln(w, "    Scan one cluster with its own states, then pass -proxmox-cluster-states")
		fmt.Fprintln(w, "    or -plugin-cluster-states.")
	}
}

// printFailed prints the queries that did not list everything: each one
// with -v or when there are few, otherwise one line per cause with the
// types, so that a missing argument is not lost among disabled APIs.
func printFailed(w io.Writer, o *options, failed []tf.Failure) {
	if len(failed) <= 5 || o.verbose {
		for _, f := range failed {
			fmt.Fprintf(w, "warning: %s not fully listed (%s): %s\n", report.Clean(f.Address), f.Category, report.Clean(f.Message))
		}
	}
	if len(failed) <= 5 {
		return
	}
	byCat := map[string][]string{}
	for _, f := range failed {
		if !slices.Contains(byCat[f.Category], f.Type) {
			byCat[f.Category] = append(byCat[f.Category], f.Type)
		}
	}
	n := 0
	for _, types := range byCat {
		n += len(types)
	}
	hint := ""
	if !o.verbose {
		hint = " (-v for messages)"
	}
	fmt.Fprintf(w, "warning: %d resource types not fully listed%s:\n", n, hint)
	for _, cat := range []string{tf.FailCrash, tf.FailMissingArg, tf.FailDenied, tf.FailTruncated, tf.FailNotFound, tf.FailRejected, tf.FailRetired, tf.FailProvider, tf.FailOther, tf.FailAPIOff} {
		types := byCat[cat]
		if len(types) == 0 {
			continue
		}
		slices.Sort(types)
		shown := types
		if !o.verbose {
			// A long list (a token without most permissions) buries the
			// few causes that need a look.
			shown = firstN(types, 8)
		}
		fmt.Fprintf(w, "  %s (%d): %s\n", cat, len(types), report.Clean(strings.Join(shown, ", ")))
		if cat == tf.FailDenied && len(types) > 8 {
			fmt.Fprintln(w, "    the credentials lack permission for these, or the account does not have the product (a bare 403 does not say which):")
			fmt.Fprintln(w, "    grant the permission, or leave out products you do not use with -exclude-types (or let them pass -strict with -strict-ignore)")
		}
		if cat == tf.FailMissingArg {
			fmt.Fprintln(w, "    pass the argument with -arg, for one set of types if needed: -arg 'google_compute_*:zone=europe-west1-b,europe-west1-c' (google: zone=auto reads every zone)")
		}
	}
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

// printOwnership limits the warning to eight objects; JSON keeps every owner.
func printOwnership(w io.Writer, ownership, inState []state.Duplicate) {
	if len(ownership)+len(inState) == 0 {
		return
	}
	fmt.Fprintln(w, "warning: duplicate state ownership:")
	entries := slices.Concat(ownership, inState)
	for i, d := range entries {
		if i == 8 {
			fmt.Fprintf(w, "  and %d more\n", len(entries)-i)
			break
		}
		what := "duplicate ownership"
		if i >= len(ownership) {
			what = "duplicate in state"
		}
		identity := model.Resource{Identity: d.Identity}.IdentityString()
		if len(d.Identity) == 0 {
			identity = fmt.Sprintf("id=%v", d.ID)
		}
		why := ""
		if d.Reason != "" {
			why = " (" + strings.ReplaceAll(d.Reason, "_", " ") + ")"
		}
		var owners []string
		for _, owner := range d.Owners {
			where := strings.Join(owner.Sources, ", ")
			if owner.Serial != nil {
				where += fmt.Sprintf("; serial %d", *owner.Serial)
			}
			owners = append(owners, owner.Address+" in "+where)
		}
		fmt.Fprintf(w, "  %s %s%s: %s %s: %s\n", d.Confidence, what, why, report.Clean(d.Type), report.Clean(identity), report.Clean(strings.Join(owners, "; ")))
	}
	fmt.Fprintln(w, "  use -format json for all objects and their state sources")
}

func reasonLink(code string) string {
	if code == "" {
		return ""
	}
	return " (" + model.ReasonLink(code) + ")"
}
