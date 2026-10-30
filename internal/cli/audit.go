package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/5xxeu/tforphans/internal/dsource"
	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/plugin"
	"github.com/5xxeu/tforphans/internal/report"
	"github.com/5xxeu/tforphans/internal/state"
	"github.com/5xxeu/tforphans/internal/tf"
)

// loadIgnore reads the ignore file and adds the presets asked for and,
// unless -no-default-presets, the default ones.
func loadIgnore(ctx context.Context, ig ignoreOptions) (*report.Ignore, error) {
	ignorePath, optional := ig.path, false
	if ignorePath == "" {
		ignorePath, optional = defaultIgnoreFile, true
	}
	ignore, err := report.LoadIgnore(ctx, ignorePath, optional)
	if err != nil {
		return nil, fmt.Errorf("ignore file: %w", err)
	}
	names := slices.Clone(ig.presets)
	if !ig.noDefaults {
		names = append(names, report.DefaultPresets...)
	}
	slices.Sort(names)
	for _, p := range slices.Compact(names) {
		pr, err := report.Preset(p)
		if err != nil {
			return nil, err
		}
		ignore.Merge(pr)
	}
	return ignore, nil
}

func execute(ctx context.Context, o *options, stdout, stderr io.Writer) error {
	if err := checkRoots(&o.src); err != nil {
		return err
	}
	var previous *report.Snapshot
	if o.audit.baseline != "" {
		var err error
		previous, err = report.ReadSnapshot(ctx, o.audit.baseline, state.DefaultMaxSize)
		if err != nil {
			return fmt.Errorf("baseline: %w", err)
		}
	}
	ignore, err := loadIgnore(ctx, o.ignore)
	if err != nil {
		return err
	}
	o.run.ignore = ignore

	var sum report.Summary
	idx := state.NewIndex()
	if err := readStates(ctx, o, idx, &sum); err != nil {
		return err
	}
	stateDirs, err := readStateRoots(ctx, o, idx, &sum)
	if err != nil {
		return err
	}
	if !o.src.printQuery {
		fmt.Fprintln(stderr, "plan:", report.Clean(runPlan(o, stateDirs)))
	}
	sum.Notes = append(sum.Notes, scanHints(&o.src, stateDirs)...)
	if o.src.impliedNoQuery {
		sum.Notes = append(sum.Notes, "no ROOT_DIR given and no Terraform configuration in the current directory: terraform query was not run, only -scan and -plugin (as with -no-query)")
	}
	if o.src.impliedNoRoot {
		sum.Notes = append(sum.Notes, impliedNoRootNote)
	}
	live, err := queryRoots(ctx, o, idx, &sum, stdout)
	if err != nil {
		return err
	}
	if o.src.printQuery {
		return nil
	}
	countOwnership(idx, &sum)
	sum.Notes = append(sum.Notes, o.run.stateNotes...)
	if len(o.src.scans) > 0 && len(o.src.stateRoots) == 0 && o.src.scanScope == nil && !o.src.scopeState {
		sum.Notes = append(sum.Notes, fmt.Sprintf("-scan lists whole accounts, but only %d states were read: objects that other root modules manage show up as unmanaged. Read every root's state with -state-roots DIR, or narrow the scan with -scan-scope zone_id=ID_OR_NAME or -scan-scope state.", sum.States))
	}
	if o.src.scopeState && sum.States == 0 {
		return errors.New("-scan-scope state: no state was read (-state, -state-roots or root modules)")
	}
	scanned, err := runScans(ctx, o, idx, stateDirs, &sum, stderr)
	if err != nil {
		return err
	}
	live = append(live, scanned...)
	sum.SortListings()
	listed, err := runPlugins(ctx, o, &sum)
	if err != nil {
		return err
	}
	live = append(live, listed...)
	// A source that ignored the cancel must not turn into a report that
	// looks finished.
	if err := ctx.Err(); err != nil {
		return err
	}

	out := conclude(o, idx, live, previous, &sum)
	// -timeout means no report: matching a large set may outlast it.
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := writeReport(ctx, o, idx, stateDirs, out, &sum, stdout, stderr); err != nil {
		return err
	}
	return exitStatus(o, sum, out.shown, stderr)
}

// readStates adds the states given with -state to idx.
func readStates(ctx context.Context, o *options, idx *state.Index, sum *report.Summary) error {
	for _, src := range o.src.states {
		data, err := state.Fetch(ctx, src, o.src.allowHTTP, o.src.maxStateSize)
		if err != nil {
			return fmt.Errorf("state %s: %w", state.Redact(src), err)
		}
		snapshot, err := state.ParseSnapshot(data, state.Redact(src))
		if err != nil {
			return fmt.Errorf("state %s: %w", state.Redact(src), err)
		}
		idx.AddSnapshots([]state.Snapshot{snapshot})
		sum.States++
	}
	return nil
}

// queryRoots adds the state of each root module to idx and returns what
// terraform query lists there. A root that fails does not stop the others.
func queryRoots(ctx context.Context, o *options, idx *state.Index, sum *report.Summary, stdout io.Writer) ([]model.Resource, error) {
	if !o.src.noQuery && len(o.src.roots) > 0 {
		// No root can be queried when this fails, so it stops the run; a
		// hanging terraform is still bounded by -source-timeout.
		if _, err := withSource(ctx, o, func(ctx context.Context) (struct{}, error) {
			return struct{}{}, (tf.Runner{Bin: o.src.terraformBin, Dir: o.src.roots[0]}).QueryVersion(ctx)
		}); err != nil {
			return nil, err
		}
	}
	var live []model.Resource
	for i, root := range o.src.roots {
		if !o.src.noQuery && !o.src.printQuery {
			o.stepf("querying %s (%d/%d)", root, i+1, len(o.src.roots))
		}
		found, err := withSource(ctx, o, func(ctx context.Context) ([]model.Resource, error) {
			return scanRoot(ctx, o, root, idx, sum, stdout)
		})
		if err != nil {
			if ctx.Err() != nil || o.src.printQuery {
				return nil, fmt.Errorf("%s: %w", root, err)
			}
			// The others still run. When the failure came before the state
			// was read, its objects would show as unmanaged (and adopt
			// would import them again): the state counts as unread.
			sum.FailSource(root, err)
			var qe queryError
			if !errors.As(err, &qe) {
				sum.UnreadStates = append(sum.UnreadStates, report.NewUnreadState(root, err))
			}
			sum.UnqueriedRoots = append(sum.UnqueriedRoots, root)
			continue
		}
		live = append(live, found...)
	}
	return live, nil
}

// countOwnership records objects that more than one state manages.
func countOwnership(idx *state.Index, sum *report.Summary) {
	sum.Ownership, sum.InState = idx.Duplicates()
	for _, d := range sum.Ownership {
		if d.Confidence == "confirmed" {
			sum.DuplicateOwnership++
		} else {
			sum.PossibleDuplicateOwnership++
		}
	}
	sum.DuplicateInState = len(sum.InState)
}

// runScans lists the objects of each -scan provider through its data
// sources. A scan that fails does not stop the others.
func runScans(ctx context.Context, o *options, idx *state.Index, stateDirs []string, sum *report.Summary, stderr io.Writer) ([]model.Resource, error) {
	// Crash recovery says it takes extra runs; -quiet drops that too.
	var progress func(string)
	if !o.quiet {
		progress = func(msg string) { fmt.Fprintln(stderr, "note:", msg) }
	}
	var live []model.Resource
	for _, name := range o.src.scans {
		a := sources[name]
		// The root -adopt-root names gets the blocks: its pin comes first.
		versionRoot := dsource.VersionRoot(slices.Concat(adoptRootOf(o, a.Name), o.src.roots, stateDirs), a.Provider)
		scope := scanScope(&o.src, a, idx)
		o.stepf("-scan %s: reading its data sources", name)
		res, err := withSource(ctx, o, func(ctx context.Context) (*dsource.Result, error) {
			return dsource.Run(ctx, a, dsource.Options{
				TerraformBin:  o.src.terraformBin,
				VersionRoot:   versionRoot,
				Include:       o.src.include,
				Exclude:       o.src.exclude,
				AllowUnpinned: o.src.allowUnpinned,
				Progress:      progress,
				Scope:         scope,
			})
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
			sum.FailSource("-scan "+name, err)
			continue
		}
		for _, l := range res.Listings {
			sum.AddListing(l)
		}
		// Asked even when nothing came back. Alternative names match state
		// of another provider too (telmate for Proxmox).
		sum.AddListable(a.Provider, res.Covered...)
		sum.AddListable("", res.AltNames...)
		sum.Failed = append(sum.Failed, res.Failed...)
		sum.Queried += len(res.Listed)
		sum.Ran += len(res.Listed)
		sum.Checks = append(sum.Checks, res.Checks...)
		sum.Skipped = append(sum.Skipped, res.Skipped...)
		// Import blocks need the provider's local name in that root when it
		// is not the type prefix (cf = { source = "cloudflare/cloudflare" }).
		// With -adopt-root the blocks go to that root: its name counts.
		local, nameRoot := "", versionRoot
		target, assigned := o.adopt.root(a.Name)
		if assigned {
			nameRoot = target
		}
		undeclared := false
		if nameRoot != "" {
			names, err := tf.LocalNames(nameRoot)
			if err != nil {
				return nil, err
			}
			local = names[a.Provider]
			undeclared = assigned && local == ""
		}
		for i := range res.Resources {
			r := &res.Resources[i]
			r.Source = "-scan " + a.Name
			if r.Scope != nil && a.Name == "proxmox" {
				r.Scope.Trusted = o.src.proxmoxStates
			}
			if local != "" && !strings.HasPrefix(r.Type, local+"_") {
				r.Provider = local
			}
			if undeclared {
				r.UndeclaredProvider = a.Provider
			}
			// The scan cannot tell which account or cluster the root's own
			// provider block points to.
			r.ImportNote = "found with the " + a.Name + " provider configured from the environment;\ncheck that this root's provider points to the same " + a.Target
		}
		live = append(live, res.Resources...)
	}
	return live, nil
}

// runPlugins returns what each -plugin lists. A plugin that fails does not
// stop the others.
func runPlugins(ctx context.Context, o *options, sum *report.Summary) ([]model.Resource, error) {
	var live []model.Resource
	for i, p := range o.src.plugins {
		var warns []error
		found, err := withSource(ctx, o, func(ctx context.Context) ([]model.Resource, error) {
			found, w, done, err := plugin.Run(ctx, p, fmt.Sprintf("plugin%d", i+1))
			if err == nil && len(found) == 0 && len(w) == 0 && !done {
				err = errors.New(`printed nothing: an empty answer cannot be told from a check that did not run; print {"done": true} when the check finished`)
			}
			warns = w
			return found, err
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
			sum.FailSource(fmt.Sprintf("plugin%d", i+1), err)
			continue
		}
		// Before -types and dedupe: the plugin was asked about its
		// provider even if none of its results is kept.
		for _, r := range found {
			sum.AddListable(r.ProviderSource, r.Type)
			// Alternative names match state of another provider too.
			sum.AddListable("", r.AltTypes...)
		}
		for i := range found {
			found[i].Baseline = ""
			if found[i].Scope != nil {
				found[i].Scope.Trusted = o.src.pluginStates
			}
		}
		for _, w := range warns {
			sum.Warnings = append(sum.Warnings, w.Error())
		}
		live = append(live, found...)
	}
	return live, nil
}

// outcome is what the audit decided, before anything is printed.
type outcome struct {
	orphans   []model.Resource
	uncertain []report.Uncertain
	// shown are the orphans the report lists: with -baseline only new ones.
	shown    []model.Resource
	baseline *report.Baseline
	// scopes are the results of every source before dedupe, for -v.
	scopes []model.Resource
	// copies are the results dedupe merged into each kept one.
	copies map[string][]model.Resource
}

// conclude narrows the results to -types, matches them against the
// states and fills in coverage, completeness and the baseline comparison.
func conclude(o *options, idx *state.Index, live []model.Resource, previous *report.Snapshot, sum *report.Summary) outcome {
	// -types and -exclude-types narrow every source, also the user's own
	// list blocks and plugins, which list whatever they were written for.
	live = slices.DeleteFunc(live, func(r model.Resource) bool {
		return len(o.src.include) > 0 && !o.src.include.Match(r.Type) || o.src.exclude.Match(r.Type)
	})
	scopes := slices.Clone(live) // every source's results, for -v
	// Every listing that returned an object, also those whose copy dedupe
	// drops: -baseline looks a finding up under its own listing.
	origins := map[string][]model.Resource{}
	if previous != nil {
		for _, r := range scopes {
			origins[originKey(r)] = append(origins[originKey(r)], r)
		}
	}
	// adopt places an object by every source that listed it: dedupe keeps
	// one copy, and -adopt-root may name the source of another.
	live, copies := dedupeMembers(live)
	c := classify(o, idx, live, origins, sum)
	orphans, uncertain := c.orphans, c.uncertain
	sortResources(orphans)
	sum.Notes = append(sum.Notes, presetHints(orphans, o.ignore.presets)...)
	sum.Notes = append(sum.Notes, ignoreHints(orphans)...)
	slices.SortStableFunc(uncertain, func(a, b report.Uncertain) int { return compareResources(a.Resource, b.Resource) })
	sum.Live, sum.Orphans = len(live), len(orphans)
	// Attributes fetched for ignore rules are not printed: an ignore file
	// from someone else's repository could otherwise pull secrets
	// (attr.password=*) into the JSON output.
	for i := range orphans {
		o.trimAttrs(&orphans[i])
	}
	for i := range uncertain {
		o.trimAttrs(&uncertain[i].Resource)
	}
	for i := range sum.Failed {
		sum.Failed[i].Classify()
	}
	counting, skipped, checks := coverageInputs(o.audit, sum)
	if sum.APIOff > 0 && o.audit.strictAPI {
		sum.Notes = append(sum.Notes, fmt.Sprintf("%d listings failed because their API is not enabled: a disabled API can still hold data (Cloud Storage and BigQuery keep theirs), so they count as not checked; if the project does not use those types, name them in -strict-ignore", sum.APIOff))
	}
	sum.Coverage = report.BuildCoverage(c.byTypeScope, checks, skipped, counting)
	sum.UnlistedTypes = unlistedTypes(&o.src, idx.TypeCounts(), *sum)
	if n := len(sum.UnlistedTypes); n > 0 {
		sum.Notes = append(sum.Notes, unlistedNote(sum.UnlistedTypes))
	}
	// An account or project left out of the configuration lists nothing
	// and fails nothing: only an expectation shows it is missing.
	expected, unknownKeys := report.CheckExpected(o.audit.expect, sum.Listings)
	sum.Expected = expected
	if len(unknownKeys) > 0 {
		sum.Notes = append(sum.Notes, fmt.Sprintf("-expect: no listing of this run reports scope %s (a typo, or sources that do not say where they look: plugins, -scan blocks without a parent, user list blocks with references)", strings.Join(unknownKeys, ", ")))
	}
	sum.StatesComplete = len(sum.UnreadStates) == 0
	sum.DiscoveryComplete = !nothingChecked(&o.src, *sum) && len(sum.UnqueriedRoots) == 0 && len(counting) == 0 && len(sum.Warnings) == 0 &&
		!slices.ContainsFunc(sum.Coverage, func(c report.Coverage) bool { return c.Status == report.Incomplete }) &&
		!slices.ContainsFunc(sum.Expected, func(e report.Expected) bool { return e.Status != report.ExpectConfirmed })
	sum.Complete = sum.StatesComplete && sum.DiscoveryComplete
	sum.Environment = o.audit.environment
	var baseline *report.Baseline
	shown := orphans
	if previous != nil {
		var b report.Baseline
		orphans, b = report.CompareRun(previous.Orphans, orphans, previous.Summary, *sum, c.seen)
		baseline = &b
		if b.EnvironmentChanged {
			sum.Notes = append(sum.Notes, fmt.Sprintf("the baseline is from environment %q, this run is %q (-environment): every finding counts as new, none as resolved", previous.Summary.Environment, o.audit.environment))
		}
		shown = nil
		for _, r := range orphans {
			if r.Baseline == "new" {
				shown = append(shown, r)
			}
		}
	}
	if n := idx.NoIdentity(); n > 0 && len(live) > 0 {
		sum.Notes = append(sum.Notes, fmt.Sprintf("%d of %d state instances have no resource identity (state before Terraform 1.12): matched by attributes instead; -refresh reads identities (slow, see README)", n, idx.Len()))
	}
	return outcome{orphans: orphans, uncertain: uncertain, shown: shown, baseline: baseline, scopes: scopes, copies: copies}
}

// writeReport prints the report in the format asked for, or with adopt
// writes import blocks, and the summary to stderr.
func writeReport(ctx context.Context, o *options, idx *state.Index, stateDirs []string, out outcome, sum *report.Summary, stdout, stderr io.Writer) error {
	details := func(w io.Writer) error { return printDetails(w, o, out.baseline, out.scopes) }
	var err error
	summarized := false
	switch {
	case o.adopt != nil:
		err = writeImports(ctx, o, idx, stateDirs, out, sum, stdout, stderr)
	case o.format == "json":
		err = report.JSON(stdout, out.orphans, out.uncertain, *sum, out.baseline)
	case o.format == "summary":
		err = report.SummaryTable(report.WithColor(stdout, colorEnabled(stdout)), *sum)
	default:
		// People read the summary and coverage first; the list follows.
		// Machine formats keep the report after their output.
		summarized = true
		if len(sum.Warnings) > 0 {
			sum.Complete = false
			sum.DiscoveryComplete = false
		}
		if err := printSummary(stderr, o, *sum, details); err != nil {
			return err
		}
		textOut := report.WithColor(stdout, colorEnabled(stdout))
		switch {
		case nothingChecked(&o.src, *sum) && len(out.shown) == 0 && len(out.uncertain) == 0:
			// The run fails below: "No unmanaged resources found" would
			// read as a clean result.
		default:
			err = report.TextWith(textOut, out.shown, out.uncertain, report.TextOptions{New: out.baseline != nil, Incomplete: !sum.Complete})
		}
		if err == nil {
			err = report.ExplainText(textOut, sum.Explain)
		}
	}
	if err != nil {
		return err
	}
	// adopt may warn about objects it could not write: the verdict and
	// -strict must see that too.
	if len(sum.Warnings) > 0 {
		sum.Complete = false
		sum.DiscoveryComplete = false
	}
	if !summarized {
		if err := printSummary(stderr, o, *sum, details); err != nil {
			return err
		}
	}
	return nil
}

// printDetails prints what -baseline and -v add to the summary.
func printDetails(w io.Writer, o *options, baseline *report.Baseline, scopes []model.Resource) error {
	if baseline != nil {
		resolved := ""
		if parts := transitionSummary(baseline.ResolvedFindings); parts != "" {
			resolved = " (" + parts + ")"
		}
		fmt.Fprintln(w)
		report.Section(w, "Baseline")
		fmt.Fprintf(w, "baseline: %d new, %d known (not shown), %d resolved%s, %d not verified%s\n", baseline.New, baseline.Known, baseline.Resolved, resolved, baseline.Unverified, unverifiedReasons(baseline.UnverifiedFindings))
		if o.verbose {
			if err := report.PrintFindings(w, baseline.UnverifiedFindings, 8); err != nil {
				return err
			}
		}
	}
	if o.verbose {
		printScopes(w, scopes)
	}
	return nil
}

// unverifiedReasons counts the reasons of findings the run could not
// verify, " (listing incomplete 2, plugin results cannot be verified 1)",
// most frequent first; "" for none.
func unverifiedReasons(fs []report.Finding) string {
	counts := map[string]int{}
	for _, f := range fs {
		reason := f.Reason
		if reason == "" {
			reason = "no listing evidence"
		}
		counts[reason]++
	}
	reasons := slices.Collect(maps.Keys(counts))
	slices.SortFunc(reasons, func(a, b string) int {
		return cmp.Or(cmp.Compare(counts[b], counts[a]), strings.Compare(a, b))
	})
	var parts []string
	for _, r := range reasons {
		parts = append(parts, fmt.Sprintf("%s %d", report.Clean(r), counts[r]))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// writeImports writes import blocks for the orphans (adopt). While a state
// could not be read it writes none: its objects would be imported twice.
func writeImports(ctx context.Context, o *options, idx *state.Index, stateDirs []string, out outcome, sum *report.Summary, stdout, stderr io.Writer) error {
	if len(sum.UnreadStates) > 0 && !o.adopt.allowIncomplete {
		if err := printSummary(stderr, o, *sum, nil); err != nil {
			return err
		}
		var roots []string
		for _, u := range sum.UnreadStates {
			roots = append(roots, u.Root)
		}
		return fmt.Errorf("not writing import blocks: the state of %s could not be read, and its objects would be imported a second time; fix access, or pass -allow-incomplete", strings.Join(roots, ", "))
	}
	orphans := out.orphans
	reserved := maps.Clone(idx.RootAddresses())
	var imports []tf.Import
	for _, dir := range slices.Concat(o.src.roots, stateDirs, adoptRootDirs(o)) {
		cfg, err := tf.ConfigAddresses(dir)
		if err != nil {
			return err
		}
		maps.Copy(reserved, cfg)
		ims, err := tf.ConfigImports(dir)
		if err != nil {
			return err
		}
		for i := range ims {
			ims[i].Dir = canonDir(ims[i].Dir)
		}
		imports = append(imports, ims...)
	}
	// An earlier adopt whose blocks are not applied yet: keep its
	// addresses free and do not write a second block for its objects.
	for _, im := range imports {
		if im.To != "" {
			reserved[im.To] = true
		}
	}
	roots := map[string]bool{}
	for _, r := range o.src.roots {
		roots[r] = true
	}
	placeOrphans(o, orphans, out.copies, roots, sum)
	n := len(orphans)
	orphans = slices.DeleteFunc(orphans, func(r model.Resource) bool { return hasImport(r, imports, adoptTarget(o, r, roots)) })
	if n > len(orphans) {
		fmt.Fprintf(stderr, "%d objects already have import blocks in the root modules; not written again.\n", n-len(orphans))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return adopt(o, orphans, reserved, sum, stdout, stderr)
}

// exitStatus is the error execute returns for a finished audit: an
// exitCode, or nil for exit status 0.
func exitStatus(o *options, sum report.Summary, shown []model.Resource, stderr io.Writer) error {
	if nothingChecked(&o.src, sum) {
		msg := "no resource types were queried, so nothing could be found"
		if n := len(sum.Failed); n > 0 {
			// Every listing failed: the cause is in them, usually the
			// credentials, not the provider.
			byCat := map[string]int{}
			for _, f := range sum.Failed {
				byCat[f.Category]++
			}
			var cats []string
			for _, c := range slices.Sorted(maps.Keys(byCat)) {
				cats = append(cats, fmt.Sprintf("%s %d", c, byCat[c]))
			}
			msg = fmt.Sprintf("every listing failed (%d: %s), so nothing could be found; check the credentials and the warnings above", n, strings.Join(cats, ", "))
		}
		if len(sum.NoList) > 0 {
			slices.Sort(sum.NoList)
			msg += fmt.Sprintf(". These providers have no list resources in the installed version: %s. List resources need Terraform 1.14 and a provider release that adds them (see its changelog); upgrade the provider, or use -scan or -plugin", strings.Join(slices.Compact(sum.NoList), ", "))
		}
		fmt.Fprintln(stderr, "error:", report.Clean(msg))
		return exitCode(exitError)
	}

	switch {
	case len(sum.FailedSources) > 0:
		return exitCode(exitError)
	case o.audit.strictOwnership && (sum.DuplicateOwnership > 0 || sum.DuplicateInState > 0):
		return exitCode(exitError)
	case o.audit.strict && (!sum.Complete || sum.Uncertain > 0):
		return exitCode(exitError)
	case len(shown) > 0 && o.adopt == nil:
		return exitCode(exitOrphans)
	}
	return nil
}

// nothingChecked reports a run that could not have found anything: no list
// query or data source scan listed a type, and no plugin ran. To a CI job exit 0 would mean that
// nothing is unmanaged.
func nothingChecked(src *sourceOptions, s report.Summary) bool {
	// Empty plugin answers without done are recorded as failed sources.
	return !hasDiscovery(s.Ran, len(src.plugins)) && !src.printQuery
}

// hasDiscovery accepts either planned listings or listings that actually ran.
func hasDiscovery(listings, plugins int) bool {
	return listings != 0 || plugins != 0
}
