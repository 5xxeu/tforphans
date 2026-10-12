package main

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/5xxeu/tforphans/internal/dsource"
	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/report"
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
	return strings.Compare(a.DisplayName, b.DisplayName)
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
	// A plugin is a check of its own.
	return s.Ran == 0 && len(o.plugins) == 0 && !o.printQuery
}

func printSummary(w io.Writer, o *options, s report.Summary) {
	fmt.Fprintf(w, "\n%d live, %d managed, %d uncertain, %d ignored, %d unmanaged; %d states read, %d resource types queried, %d skipped",
		s.Live, s.Managed, s.Uncertain, s.Ignored, s.Orphans, s.States, s.Queried, len(s.Skipped))
	if len(s.Skipped) > 0 && !o.verbose {
		fmt.Fprint(w, " (-v to list)")
	}
	fmt.Fprintln(w)
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
			fmt.Fprintf(w, "  skipped %s%s: %s\n", report.Clean(sk.Type), where, report.Clean(sk.Reason))
		}
		for _, p := range slices.Compact(slices.Sorted(slices.Values(s.NoList))) {
			fmt.Fprintf(w, "  no list resources: %s\n", report.Clean(p))
		}
	}
	printFailed(w, o, s.Failed)
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
		fmt.Fprintf(w, "warning: %d objects are uncertain (%s): they match state only by ID, so their account, subscription or cluster could not be checked; with states of several accounts or clusters they may be unmanaged. State without resource identity: pass -refresh to read identities without writing anything, or record them with terraform apply -refresh-only and a current provider. Proxmox and plugin results marked unverified: state does not record the cluster; scan one cluster with its own states and pass -proxmox-cluster-states or -plugin-cluster-states.\n", s.Uncertain, where)
	}
	// The last line says whether "nothing unmanaged" can be trusted.
	// Complete covers what can be listed; say what could not be.
	var out []string
	gapTypes := map[string]bool{}
	for _, c := range s.Coverage {
		switch {
		case c.Status == report.Skipped:
			out = append(out, c.Type)
		case slices.ContainsFunc(c.Reasons, func(r string) bool { return strings.HasPrefix(r, report.GapPrefix) }):
			gapTypes[c.Type] = true
		}
	}
	if s.Complete {
		var beyond []string
		if len(out) > 0 {
			beyond = append(beyond, fmt.Sprintf("%d types left out on purpose or without any listing", len(out)))
		}
		if len(s.NoList) > 0 {
			beyond = append(beyond, fmt.Sprintf("%d providers without list resources", len(slices.Compact(slices.Sorted(slices.Values(s.NoList))))))
		}
		if len(beyond) == 0 {
			fmt.Fprintln(w, "coverage: complete")
			return
		}
		fmt.Fprintf(w, "coverage: complete for what can be listed; not checked: %s (-v lists them)\n", strings.Join(beyond, ", "))
		return
	}
	var gaps []string
	if n := len(s.Failed); n > 0 {
		gaps = append(gaps, fmt.Sprintf("%d failed or truncated listings", n))
	}
	if n := len(gapTypes); n > 0 {
		gaps = append(gaps, fmt.Sprintf("%d types skipped that needed a check", n))
	}
	if n := len(s.UnreadStates); n > 0 {
		gaps = append(gaps, fmt.Sprintf("%d states not read", n))
	}
	if n := len(s.Warnings) - len(s.UnreadStates); n > 0 {
		gaps = append(gaps, fmt.Sprintf("%d other warnings", n))
	}
	effect := "unmanaged objects there may be missing from the result"
	if len(s.UnreadStates) > 0 {
		effect += ", and objects of unread states may show as unmanaged"
	}
	fmt.Fprintf(w, "coverage: INCOMPLETE (%s): %s; -strict makes this exit 1\n", strings.Join(gaps, ", "), effect)
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
		fmt.Fprintf(w, "  %s (%d): %s\n", cat, len(types), report.Clean(strings.Join(types, ", ")))
		if cat == tf.FailMissingArg {
			fmt.Fprintln(w, "    pass the argument with -arg, for one set of types if needed: -arg 'google_compute_*:zone=europe-west1-b,europe-west1-c'")
		}
	}
}
