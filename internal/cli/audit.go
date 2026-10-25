package cli

import (
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
func loadIgnore(ctx context.Context, o *options) (*report.Ignore, error) {
	ignorePath, optional := o.ignorePath, false
	if ignorePath == "" {
		ignorePath, optional = defaultIgnoreFile, true
	}
	ignore, err := report.LoadIgnore(ctx, ignorePath, optional)
	if err != nil {
		return nil, fmt.Errorf("ignore file: %w", err)
	}
	names := slices.Clone(o.presets)
	if !o.noDefaultPresets {
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
	var previous *report.Snapshot
	if o.baselinePath != "" {
		var err error
		previous, err = report.ReadSnapshot(ctx, o.baselinePath, state.DefaultMaxSize)
		if err != nil {
			return fmt.Errorf("baseline: %w", err)
		}
	}
	ignore, err := loadIgnore(ctx, o)
	if err != nil {
		return err
	}
	o.ignore = ignore

	var sum report.Summary
	idx := state.NewIndex()
	for _, src := range o.states {
		data, err := state.Fetch(ctx, src, o.allowHTTP, o.maxStateSize)
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

	stateDirs, err := readStateRoots(ctx, o, idx, &sum)
	if err != nil {
		return err
	}
	if !o.printQuery {
		fmt.Fprintln(stderr, "plan:", report.Clean(runPlan(o, stateDirs)))
	}
	sum.Notes = append(sum.Notes, scanHints(o, stateDirs)...)
	if o.impliedNoQuery {
		sum.Notes = append(sum.Notes, "no ROOT_DIR given and no Terraform configuration in the current directory: terraform query was not run, only -scan and -plugin (as with -no-query)")
	}
	if o.impliedNoRoot {
		sum.Notes = append(sum.Notes, impliedNoRootNote)
	}
	if !o.noQuery && len(o.roots) > 0 {
		// No root can be queried when this fails, so it stops the run; a
		// hanging terraform is still bounded by -source-timeout.
		if _, err := withSource(ctx, o, func(ctx context.Context) (struct{}, error) {
			return struct{}{}, (tf.Runner{Bin: o.terraformBin, Dir: o.roots[0]}).QueryVersion(ctx)
		}); err != nil {
			return err
		}
	}
	var live []model.Resource
	for i, root := range o.roots {
		if !o.noQuery && !o.printQuery {
			o.stepf("querying %s (%d/%d)", root, i+1, len(o.roots))
		}
		found, err := withSource(ctx, o, func(ctx context.Context) ([]model.Resource, error) {
			return scanRoot(ctx, o, root, idx, &sum, stdout)
		})
		if err != nil {
			if ctx.Err() != nil || o.printQuery {
				return fmt.Errorf("%s: %w", root, err)
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
	if o.printQuery {
		return nil
	}
	sum.Ownership, sum.InState = idx.Duplicates()
	for _, d := range sum.Ownership {
		if d.Confidence == "confirmed" {
			sum.DuplicateOwnership++
		} else {
			sum.PossibleDuplicateOwnership++
		}
	}
	sum.DuplicateInState = len(sum.InState)
	sum.Notes = append(sum.Notes, o.stateNotes...)
	if len(o.scans) > 0 && len(o.stateRoots) == 0 && o.scanScope == nil && !o.scopeState {
		sum.Notes = append(sum.Notes, fmt.Sprintf("-scan lists whole accounts, but only %d states were read: objects that other root modules manage show up as unmanaged. Read every root's state with -state-roots DIR, or narrow the scan with -scan-scope zone_id=ID_OR_NAME or -scan-scope state.", sum.States))
	}
	if o.scopeState && sum.States == 0 {
		return errors.New("-scan-scope state: no state was read (-state, -state-roots or root modules)")
	}
	// Crash recovery says it takes extra runs; -quiet drops that too.
	var progress func(string)
	if !o.quiet {
		progress = func(msg string) { fmt.Fprintln(stderr, "note:", msg) }
	}
	for _, name := range o.scans {
		a := sources[name]
		// The root -adopt-root names gets the blocks: its pin comes first.
		versionRoot := dsource.VersionRoot(slices.Concat(adoptRootOf(o, a.Name), o.roots, stateDirs), a.Provider)
		scope := scanScope(o, a, idx)
		o.stepf("-scan %s: reading its data sources", name)
		res, err := withSource(ctx, o, func(ctx context.Context) (*dsource.Result, error) {
			return dsource.Run(ctx, a, dsource.Options{
				TerraformBin:  o.terraformBin,
				VersionRoot:   versionRoot,
				Include:       o.include,
				Exclude:       o.exclude,
				AllowUnpinned: o.allowUnpinned,
				Progress:      progress,
				Scope:         scope,
			})
		})
		if err != nil {
			if ctx.Err() != nil {
				return err
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
		target, assigned := o.adoptRoots[a.Name]
		if assigned {
			nameRoot = target
		}
		undeclared := false
		if nameRoot != "" {
			names, err := tf.LocalNames(nameRoot)
			if err != nil {
				return err
			}
			local = names[a.Provider]
			undeclared = assigned && local == ""
		}
		for i := range res.Resources {
			r := &res.Resources[i]
			r.Source = "-scan " + a.Name
			if r.Scope != nil && a.Name == "proxmox" {
				r.Scope.Trusted = o.proxmoxStates
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
	sum.SortListings()
	for i, p := range o.plugins {
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
				return err
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
				found[i].Scope.Trusted = o.pluginStates
			}
		}
		for _, w := range warns {
			sum.Warnings = append(sum.Warnings, w.Error())
		}
		live = append(live, found...)
	}
	// A source that ignored the cancel must not turn into a report that
	// looks finished.
	if err := ctx.Err(); err != nil {
		return err
	}

	// -types and -exclude-types narrow every source, also the user's own
	// list blocks and plugins, which list whatever they were written for.
	live = slices.DeleteFunc(live, func(r model.Resource) bool {
		return len(o.include) > 0 && !o.include.Match(r.Type) || o.exclude.Match(r.Type)
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
	c := classify(o, idx, live, origins, &sum)
	orphans, uncertain := c.orphans, c.uncertain
	sortResources(orphans)
	sum.Notes = append(sum.Notes, presetHints(orphans, o.presets)...)
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
	counting, skipped, checks := coverageInputs(o, &sum)
	if sum.APIOff > 0 && o.strictAPI {
		sum.Notes = append(sum.Notes, fmt.Sprintf("%d listings failed because their API is not enabled: a disabled API can still hold data (Cloud Storage and BigQuery keep theirs), so they count as not checked; if the project does not use those types, name them in -strict-ignore", sum.APIOff))
	}
	sum.Coverage = report.BuildCoverage(c.byTypeScope, checks, skipped, counting)
	sum.UnlistedTypes = unlistedTypes(o, idx.TypeCounts(), sum)
	if n := len(sum.UnlistedTypes); n > 0 {
		sum.Notes = append(sum.Notes, unlistedNote(sum.UnlistedTypes))
	}
	sum.StatesComplete = len(sum.UnreadStates) == 0
	sum.DiscoveryComplete = !nothingChecked(o, sum) && len(sum.UnqueriedRoots) == 0 && len(counting) == 0 && len(sum.Warnings) == 0 &&
		!slices.ContainsFunc(sum.Coverage, func(c report.Coverage) bool { return c.Status == report.Incomplete })
	sum.Complete = sum.StatesComplete && sum.DiscoveryComplete
	sum.Environment = o.environment
	var baseline *report.Baseline
	shown := orphans
	if previous != nil {
		var b report.Baseline
		orphans, b = report.CompareRun(previous.Orphans, orphans, previous.Summary, sum, c.seen)
		baseline = &b
		if b.EnvironmentChanged {
			sum.Notes = append(sum.Notes, fmt.Sprintf("the baseline is from environment %q, this run is %q (-environment): every finding counts as new, none as resolved", previous.Summary.Environment, o.environment))
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

	// -timeout means no report: matching a large set may outlast it.
	if err := ctx.Err(); err != nil {
		return err
	}
	details := func(w io.Writer) error {
		if baseline != nil {
			why := "their type or scope was not fully listed this run"
			if baseline.EnvironmentChanged {
				why = "the baseline is from another environment"
			}
			resolved := ""
			if parts := transitionSummary(baseline.ResolvedFindings); parts != "" {
				resolved = " (" + parts + ")"
			}
			if n := countTransition(baseline.UnverifiedFindings, report.BecameUnsure); n == baseline.Unverified && n > 0 {
				why = "now uncertain"
			} else if n > 0 {
				why = fmt.Sprintf("%d now uncertain; the others: %s", n, why)
			}
			fmt.Fprintln(w)
			report.Section(w, "Baseline")
			fmt.Fprintf(w, "baseline: %d new, %d known (not shown), %d resolved%s, %d not verified (%s)\n", baseline.New, baseline.Known, baseline.Resolved, resolved, baseline.Unverified, why)
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
	summarized := false
	switch {
	case o.adopt && len(sum.UnreadStates) > 0 && !o.allowIncomplete:
		if err := printSummary(stderr, o, sum, nil); err != nil {
			return err
		}
		var roots []string
		for _, u := range sum.UnreadStates {
			roots = append(roots, u.Root)
		}
		return fmt.Errorf("not writing import blocks: the state of %s could not be read, and its objects would be imported a second time; fix access, or pass -allow-incomplete", strings.Join(roots, ", "))
	case o.adopt:
		reserved := maps.Clone(idx.RootAddresses())
		var imports []tf.Import
		for _, dir := range slices.Concat(o.roots, stateDirs, adoptRootDirs(o)) {
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
		for _, r := range o.roots {
			roots[r] = true
		}
		placeOrphans(o, orphans, copies, roots, &sum)
		n := len(orphans)
		orphans = slices.DeleteFunc(orphans, func(r model.Resource) bool { return hasImport(r, imports, adoptTarget(o, r, roots)) })
		if n > len(orphans) {
			fmt.Fprintf(stderr, "%d objects already have import blocks in the root modules; not written again.\n", n-len(orphans))
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		err = adopt(o, orphans, reserved, &sum, stdout, stderr)
	case o.format == "json":
		err = report.JSON(stdout, orphans, uncertain, sum, baseline)
	case o.format == "summary":
		err = report.SummaryTable(report.WithColor(stdout, colorEnabled(stdout)), sum)
	default:
		// People read the summary and coverage first; the list follows.
		// Machine formats keep the report after their output.
		summarized = true
		if len(sum.Warnings) > 0 {
			sum.Complete = false
			sum.DiscoveryComplete = false
		}
		if err := printSummary(stderr, o, sum, details); err != nil {
			return err
		}
		textOut := report.WithColor(stdout, colorEnabled(stdout))
		err = report.Text(textOut, shown, uncertain)
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
		if err := printSummary(stderr, o, sum, details); err != nil {
			return err
		}
	}
	if nothingChecked(o, sum) {
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
	case o.strictOwnership && (sum.DuplicateOwnership > 0 || sum.DuplicateInState > 0):
		return exitCode(exitError)
	case o.strict && (!sum.Complete || sum.Uncertain > 0):
		return exitCode(exitError)
	case len(shown) > 0 && !o.adopt:
		return exitCode(exitOrphans)
	}
	return nil
}
