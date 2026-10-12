// Command tforphans finds live infrastructure that no Terraform state
// manages.
//
// For every root module it lists live resources of each list-capable
// resource type with `terraform query` (Terraform 1.14+), using the module's
// own provider configuration, and compares their resource identities with
// the module's state plus any extra states. Providers without list support
// are covered by plugins: commands that print the same data as JSON lines.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/5xxeu/tforphans/internal/dsource"
	"github.com/5xxeu/tforphans/internal/glob"
	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/plugin"
	"github.com/5xxeu/tforphans/internal/report"
	"github.com/5xxeu/tforphans/internal/state"
	"github.com/5xxeu/tforphans/internal/tf"
)

const (
	exitOK      = 0
	exitError   = 1
	exitOrphans = 2

	defaultIgnoreFile = ".tforphans-ignore"
)

const usage = `Usage: tforphans [flags] [ROOT_DIR...]

Finds live resources that are not managed by any of the given Terraform
states. Each ROOT_DIR (default ".") must be an initialized root module; its
state is read with "terraform state pull" and its providers are asked to list
live resources with "terraform query".

Exit status: 0 nothing found, 2 unmanaged resources found, 1 error (also on
incomplete coverage with -strict).

Run "tforphans adopt -h" to generate import blocks for what it finds.

Flags:
`

const adoptUsage = `Usage: tforphans adopt [flags] [ROOT_DIR...]

Finds unmanaged resources like tforphans does and writes an import block for
each one. Put the blocks into the root module and generate the configuration:

  tforphans adopt -o imports.tf
  terraform plan -generate-config-out=generated.tf

Objects that state may manage but cannot confirm (uncertain) get no block.
When a state could not be read, adopt stops: objects of that state would show
as unmanaged and be imported a second time (-allow-incomplete writes anyway).

Exit status: 0 on success, whether or not anything was found; 1 on error
(also on incomplete coverage with -strict).

Flags:
`

type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, " ") }
func (l *listFlag) Set(v string) error {
	*l = append(*l, v)
	return nil
}

// csv splits comma-separated values from repeated flags.
func (l listFlag) csv() []string {
	var out []string
	for _, v := range l {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

type options struct {
	roots         []string
	stateRoots    listFlag
	init          bool
	currentWS     bool // read only the selected workspace of each root
	anyVersion    bool // lift the roots' required_version
	states        listFlag
	plugins       listFlag
	scans         []string
	scanScope     map[string][]string // -scan-scope ARG=values
	scopeState    bool                // -scan-scope state
	refresh       bool
	mu            sync.Mutex
	stateNotes    []string // notes of state reading, from several goroutines
	args          []tf.Arg
	include       glob.List
	exclude       glob.List
	limit         int
	format        string
	adopt         bool   // "adopt" subcommand: write import blocks
	out           string // adopt: output file, empty for stdout
	ignorePath    string
	presets       []string
	includeAttrs  []string
	ignore        *report.Ignore // loaded in execute
	strict        bool
	verbose       bool
	printQuery    bool
	noQuery       bool
	allowUnpinned bool
	// adopt: write import blocks although a state could not be read.
	allowIncomplete bool
	allowHTTP       bool
	proxmoxStates   bool
	pluginStates    bool
	terraformBin    string
	maxRoots        int
	progress        io.Writer // progress lines of long steps (stderr)
	strictAPI       bool
	terragrunt      bool
	configFile      string // flags read from this file
}

// version is set by release builds; go install builds take the module
// version.
var version = ""

func buildVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "unknown"
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func parseFlags(argv []string, stderr io.Writer) (*options, error) {
	o := &options{}
	name, help := "tforphans", usage
	// A root module directory named "adopt" can be passed as ./adopt.
	if len(argv) > 0 && argv[0] == "adopt" {
		o.adopt, argv = true, argv[1:]
		name, help = "tforphans adopt", adoptUsage
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, help)
		fs.PrintDefaults()
	}
	var argFlags, include, exclude, scans, presets, attrs, scanScopes listFlag
	fs.Var(&o.states, "state", "extra state that manages resources in the same accounts: file, http(s) URL (uses TF_HTTP_USERNAME/TF_HTTP_PASSWORD) or - for stdin; repeatable")
	fs.Var(&o.stateRoots, "state-roots", "directory tree whose root modules' states (all workspaces) count as managed, read with terraform state pull; repeatable")
	fs.BoolVar(&o.proxmoxStates, "proxmox-cluster-states", false, "state that every state given belongs to the Proxmox cluster scanned with -scan proxmox; without it Proxmox matches are uncertain, since state does not record the cluster")
	fs.BoolVar(&o.pluginStates, "plugin-cluster-states", false, "state that every state given belongs to the cluster or account the plugins scan; without it results a plugin marks \"unverified\" (state does not record where they live) match only as uncertain")
	fs.BoolVar(&o.allowHTTP, "allow-http", false, "allow -state URLs over plain http (never with credentials)")
	fs.BoolVar(&o.allowUnpinned, "allow-unpinned", false, "let -scan install the newest provider release when no root module pins its version")
	fs.BoolVar(&o.currentWS, "current-workspace", false, "read only the selected workspace of each root module instead of listing all workspaces")
	fs.BoolVar(&o.anyVersion, "ignore-required-version", false, "lift the root modules' required_version with a temporary override file, so that a root pinned to an older Terraform can be queried (child modules' constraints still apply)")
	fs.BoolVar(&o.refresh, "refresh", false, "read each root's state through terraform plan -refresh-only (no lock, nothing written) instead of state pull: state written before Terraform 1.12 gets resource identities, so matches are exact; slower, needs the root's credentials and variables")
	fs.BoolVar(&o.strictAPI, "strict-api", false, "count listings that failed because the API is not enabled (GCP) as incomplete coverage; by default a disabled API is taken to hold no objects")
	fs.IntVar(&o.maxRoots, "max-roots", 200, "stop when -state-roots finds more root modules than this, before terraform runs in them (0: no limit)")
	fs.BoolVar(&o.terragrunt, "terragrunt", false, "with -state-roots, also read Terragrunt units (directories with a terragrunt.hcl, except parents of other units) through terragrunt in PATH; terragrunt initializes them (.terragrunt-cache)")
	fs.BoolVar(&o.init, "init", false, "run terraform init in root modules that are not initialized (creates .terraform there)")
	fs.Var(&scans, "scan", "list a provider without list resources through its plural data sources: cloudflare, proxmox; credentials from the provider's environment variables (CLOUDFLARE_API_TOKEN, PROXMOX_VE_*), not from the root's provider block; repeatable")
	fs.Var(&scanScopes, "scan-scope", "limit -scan to some zones or accounts: zone_id=ID_OR_NAME,... or account_id=...; with no value (account_id=) that scope is not read; \"state\" limits every other scope to the IDs found in the states read; repeatable")
	fs.Var(&o.plugins, "plugin", "discovery command for providers without list support, run with sh -c; repeatable")
	fs.Var(&argFlags, "arg", "fan list queries out over values of a list config argument, e.g. region=eu-north-1,us-east-1; TYPE_GLOB:name=... limits it to matching types (google_compute_*:zone=europe-west1-b) and wins over the same name without one; repeatable")
	fs.Var(&include, "types", "only query resource types matching these globs, e.g. aws_s3_*,aws_iam_role")
	fs.Var(&exclude, "exclude-types", "do not query resource types matching these globs")
	fs.IntVar(&o.limit, "limit", 100000, "maximum results per list query")
	if o.adopt {
		fs.StringVar(&o.out, "o", "", "write the import blocks to this new file instead of stdout")
		fs.BoolVar(&o.allowIncomplete, "allow-incomplete", false, "write import blocks even when a state could not be read (its objects would be imported a second time)")
	} else {
		fs.StringVar(&o.format, "format", "text", "output format: text, json, or summary (counts per type)")
	}
	fs.StringVar(&o.ignorePath, "ignore", "", "ignore file with TYPE_GLOB[:NAME_GLOB] [KEY=GLOB...] lines (default "+defaultIgnoreFile+" if present)")
	fs.Var(&presets, "ignore-preset", "add built-in ignore rules: "+strings.Join(report.Presets(), ", ")+"; repeatable")
	fs.Var(&attrs, "include-attrs", "fetch these resource attributes with the list results and print them in -format json, e.g. labels,creation_timestamp (slower: terraform query returns whole objects)")
	fs.BoolVar(&o.strict, "strict", false, "exit 1 when coverage is incomplete: failed or truncated queries, unread states, plugin errors, types skipped for want of an argument or a default provider configuration, or uncertain matches (state without identity, Proxmox, plugin results marked unverified)")
	fs.BoolVar(&o.verbose, "v", false, "list skipped resource types and every failed query, and the projects and accounts results came from")
	fs.BoolVar(&o.printQuery, "print-query", false, "print the generated .tfquery.hcl for each root and exit")
	fs.BoolVar(&o.noQuery, "no-query", false, "do not run terraform query: read the roots' states and run plugins only")
	fs.StringVar(&o.terraformBin, "terraform", "terraform", "terraform binary")
	fs.StringVar(&o.configFile, "config", "", "read flags from this YAML file (keys are flag names, lists repeat a flag, roots lists root modules; the command line wins); default "+defaultConfigFile+" in the current directory, if present")
	noConfig := fs.Bool("no-config", false, "do not read "+defaultConfigFile)
	showVersion := fs.Bool("version", false, "print the version and exit")

	// Allow flags after positional arguments.
	for rest := argv; ; {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		o.roots = append(o.roots, rest[0])
		rest = rest[1:]
	}
	if *showVersion {
		fmt.Fprintln(stderr, "tforphans", buildVersion())
		return nil, flag.ErrHelp
	}
	if !*noConfig {
		explicit := o.configFile != ""
		if !explicit {
			if _, err := os.Stat(defaultConfigFile); err == nil {
				o.configFile = defaultConfigFile
			}
		}
		if o.configFile != "" {
			if err := applyConfig(fs, o, o.configFile, explicit); err != nil {
				return nil, err
			}
		}
	} else {
		o.configFile = ""
	}
	// terraform runs in each root and in temporary directories: a relative
	// path (./tools/terraform) would be looked up there. A plain name
	// stays a PATH lookup.
	// Root paths are joined with file names later (filepath.Join cleans):
	// resolve links first, so that jump/../prod means what terraform,
	// running in it, takes it for.
	for _, list := range [][]string{o.roots, o.stateRoots} {
		for i, p := range list {
			if real, err := filepath.EvalSymlinks(p); err == nil {
				list[i] = real
			}
		}
	}
	if strings.ContainsAny(o.terraformBin, "/"+string(filepath.Separator)) && !filepath.IsAbs(o.terraformBin) {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		// Not Abs, which cleans: a link before .. must count as the
		// system resolves it.
		o.terraformBin = cwd + string(filepath.Separator) + o.terraformBin
	}

	switch o.format {
	case "", "text", "json", "summary":
	default:
		return nil, fmt.Errorf("unknown -format %q", o.format)
	}
	// Repeated names are merged (-arg region=a -arg region=b): two
	// attributes of one name would make terraform reject the whole query.
	argIndex := map[string]int{}
	for _, a := range argFlags {
		parsed, err := tf.ParseArg(a)
		if err != nil {
			return nil, err
		}
		i, ok := argIndex[parsed.Key()]
		if !ok {
			i = len(o.args)
			argIndex[parsed.Key()] = i
			o.args = append(o.args, tf.Arg{Types: parsed.Types, Name: parsed.Name})
		}
		for _, v := range parsed.Values {
			if !slices.Contains(o.args[i].Values, v) {
				o.args[i].Values = append(o.args[i].Values, v)
			}
		}
	}
	o.presets = presets.csv()
	for _, p := range o.presets {
		if _, err := report.Preset(p); err != nil {
			return nil, err
		}
	}
	o.includeAttrs = attrs.csv()
	o.scans = scans.csv()
	for _, s := range o.scans {
		if _, ok := sources[s]; !ok {
			return nil, fmt.Errorf("unknown -scan %q", s)
		}
	}
	for _, sc := range scanScopes {
		if sc == "state" {
			o.scopeState = true
			continue
		}
		arg, vals, ok := strings.Cut(sc, "=")
		known := false
		for _, name := range o.scans {
			for _, s := range sources[name].Scopes {
				known = known || s.Arg == arg
			}
		}
		if !ok || !known {
			return nil, fmt.Errorf("-scan-scope %q: want state or ARG=VALUE,... where ARG is a scope of the scanned providers (zone_id, account_id for cloudflare)", sc)
		}
		if o.scanScope == nil {
			o.scanScope = map[string][]string{}
		}
		// No values (account_id=) reads nothing of that scope.
		if _, ok := o.scanScope[arg]; !ok {
			o.scanScope[arg] = []string{}
		}
		for _, v := range strings.Split(vals, ",") {
			if v = strings.TrimSpace(v); v != "" && !slices.Contains(o.scanScope[arg], v) {
				o.scanScope[arg] = append(o.scanScope[arg], v)
			}
		}
	}
	o.include = glob.CompileList(include.csv())
	o.exclude = glob.CompileList(exclude.csv())
	if len(o.roots) == 0 && !o.noQuery {
		o.roots = []string{"."}
	}
	if n := len(slices.DeleteFunc(slices.Clone(o.states), func(s string) bool { return s != "-" })); n > 1 {
		return nil, errors.New("-state - (stdin) can be given only once")
	}
	if o.noQuery && len(o.plugins) == 0 && len(o.scans) == 0 {
		return nil, errors.New("-no-query needs at least one -scan or -plugin")
	}
	return o, nil
}

func run(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	o, err := parseFlags(argv, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", report.CleanLines(err.Error()))
		return exitError
	}
	o.progress = stderr
	if err := execute(ctx, o, stdout, stderr); err != nil {
		var code exitCode
		if errors.As(err, &code) {
			return int(code)
		}
		fmt.Fprintln(stderr, "error:", report.CleanLines(err.Error()))
		return exitError
	}
	return exitOK
}

// sources are providers without list resources that are listed through
// their plural data sources. Credentials come from the provider's
// environment variables, since the generated root has no provider block.
var sources = map[string]dsource.Adapter{
	"cloudflare": dsource.Cloudflare,
	"proxmox":    dsource.Proxmox,
}

type exitCode int

func (c exitCode) Error() string { return fmt.Sprintf("exit %d", int(c)) }

func execute(ctx context.Context, o *options, stdout, stderr io.Writer) error {
	ignorePath, optional := o.ignorePath, false
	if ignorePath == "" {
		ignorePath, optional = defaultIgnoreFile, true
	}
	ignore, err := report.LoadIgnore(ignorePath, optional)
	if err != nil {
		return fmt.Errorf("ignore file: %w", err)
	}
	for _, p := range o.presets {
		pr, err := report.Preset(p)
		if err != nil {
			return err
		}
		ignore.Merge(pr)
	}
	o.ignore = ignore

	var sum report.Summary
	idx := state.NewIndex()
	for _, src := range o.states {
		data, err := state.Fetch(ctx, src, o.allowHTTP)
		if err != nil {
			return fmt.Errorf("state %s: %w", state.Redact(src), err)
		}
		insts, err := state.Parse(data)
		if err != nil {
			return fmt.Errorf("state %s: %w", state.Redact(src), err)
		}
		idx.Add(insts)
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
	if !o.noQuery && len(o.roots) > 0 {
		if err := (tf.Runner{Bin: o.terraformBin, Dir: o.roots[0]}).QueryVersion(ctx); err != nil {
			return err
		}
	}
	var live []model.Resource
	for _, root := range o.roots {
		found, err := scanRoot(ctx, o, root, idx, &sum, stdout)
		if err != nil {
			return fmt.Errorf("%s: %w", root, err)
		}
		live = append(live, found...)
	}
	if o.printQuery {
		return nil
	}
	sum.Notes = append(sum.Notes, o.stateNotes...)
	if len(o.scans) > 0 && len(o.stateRoots) == 0 && o.scanScope == nil && !o.scopeState {
		sum.Notes = append(sum.Notes, fmt.Sprintf("-scan lists whole accounts, but only %d states were read: objects that other root modules manage show up as unmanaged. Read every root's state with -state-roots DIR, or narrow the scan with -scan-scope zone_id=ID_OR_NAME or -scan-scope state.", sum.States))
	}
	if o.scopeState && sum.States == 0 {
		return errors.New("-scan-scope state: no state was read (-state, -state-roots or root modules)")
	}
	for _, name := range o.scans {
		a := sources[name]
		versionRoot := dsource.VersionRoot(slices.Concat(o.roots, stateDirs), a.Provider)
		scope := maps.Clone(o.scanScope)
		if o.scopeState {
			if scope == nil {
				scope = map[string][]string{}
			}
			for _, sc := range a.Scopes {
				if _, ok := scope[sc.Arg]; !ok {
					// Parent objects (zones) are in state under their own
					// id; their children carry it as zone_id.
					vals := idx.Values("", sc.Arg)
					for _, v := range idx.Values(strings.TrimSuffix(sc.Parent, "s"), "id") {
						if !slices.Contains(vals, v) {
							vals = append(vals, v)
						}
					}
					scope[sc.Arg] = vals
				}
			}
		}
		res, err := dsource.Run(ctx, a, dsource.Options{
			TerraformBin:  o.terraformBin,
			VersionRoot:   versionRoot,
			Include:       o.include,
			Exclude:       o.exclude,
			AllowUnpinned: o.allowUnpinned,
			Progress:      func(msg string) { fmt.Fprintln(stderr, "note:", msg) },
			Scope:         scope,
		})
		if err != nil {
			return err
		}
		sum.Failed = append(sum.Failed, res.Failed...)
		sum.Queried += len(res.Listed)
		sum.Ran += len(res.Listed)
		sum.Checks = append(sum.Checks, res.Checks...)
		sum.Skipped = append(sum.Skipped, res.Skipped...)
		// Import blocks need the provider's local name in that root when it
		// is not the type prefix (cf = { source = "cloudflare/cloudflare" }).
		local := ""
		if versionRoot != "" {
			names, err := tf.LocalNames(versionRoot)
			if err != nil {
				return err
			}
			local = names[a.Provider]
		}
		for i := range res.Resources {
			r := &res.Resources[i]
			r.Source = a.Name
			if r.Scope != nil && a.Name == "proxmox" {
				r.Scope.Trusted = o.proxmoxStates
			}
			if local != "" && !strings.HasPrefix(r.Type, local+"_") {
				r.Provider = local
			}
			// The scan cannot tell which account or cluster the root's own
			// provider block points to.
			r.ImportNote = "found with the " + a.Name + " provider configured from the environment;\ncheck that this root's provider points to the same " + a.Target
		}
		live = append(live, res.Resources...)
	}
	for i, p := range o.plugins {
		found, warns, err := plugin.Run(ctx, p, fmt.Sprintf("plugin%d", i+1))
		if err != nil {
			return err
		}
		for i := range found {
			if found[i].Scope != nil {
				found[i].Scope.Trusted = o.pluginStates
			}
		}
		for _, w := range warns {
			sum.Warnings = append(sum.Warnings, w.Error())
		}
		live = append(live, found...)
	}

	// -types and -exclude-types narrow every source, also the user's own
	// list blocks and plugins, which list whatever they were written for.
	live = slices.DeleteFunc(live, func(r model.Resource) bool {
		return len(o.include) > 0 && !o.include.Match(r.Type) || o.exclude.Match(r.Type)
	})
	scopes := slices.Clone(live) // every source's results, for -v
	live = dedupe(live)
	var orphans []model.Resource
	var uncertain []report.Uncertain
	sum.ByType = map[string]*report.TypeCount{}
	byScope := map[string]*report.TypeCount{}
	byTypeScope := map[[2]string]*report.TypeCount{}
	for i := range live {
		r := &live[i]
		if r.ScopeName == "" {
			r.ScopeName = scopeName(r.Identity)
		}
		where := r.ScopeName
		if where == "" {
			where = "unknown"
		}
		counts := []*report.TypeCount{sum.ByType[r.Type], byScope[where], byTypeScope[[2]string{r.Type, r.ScopeName}]}
		for i, tc := range counts {
			if tc == nil {
				counts[i] = &report.TypeCount{}
			}
		}
		sum.ByType[r.Type], byScope[where], byTypeScope[[2]string{r.Type, r.ScopeName}] = counts[0], counts[1], counts[2]
		for _, tc := range counts {
			tc.Live++
		}
		// An ignore rule wins over an uncertain match, not over a
		// confirmed one.
		m := idx.Match(*r)
		switch {
		case m.Managed && m.Verified:
			sum.Managed++
			if m.ByIdentity {
				sum.ManagedByIdentity++
			}
			for _, tc := range counts {
				tc.Managed++
			}
		case ignore.Match(*r):
			sum.Ignored++
			for _, tc := range counts {
				tc.Ignored++
			}
		case m.Managed:
			sum.Uncertain++
			uncertain = append(uncertain, report.Uncertain{Resource: *r, StateAddress: m.Address})
			for _, tc := range counts {
				tc.Uncertain++
			}
		default:
			orphans = append(orphans, *r)
			for _, tc := range counts {
				tc.Orphans++
			}
		}
	}
	// Scopes are worth a breakdown when there are several.
	if len(byScope) > 1 || len(byScope) == 1 && byScope["unknown"] == nil {
		sum.ByScope = byScope
	}
	sortResources(orphans)
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
	// A disabled API (GCP) can hold no objects: such listings stay in
	// failed_queries but are no gap, unless -strict-api.
	counting, skipped, checks := sum.Failed, sum.Skipped, sum.Checks
	for _, f := range sum.Failed {
		if f.Category == tf.FailAPIOff {
			sum.APIOff++
		}
	}
	if !o.strictAPI {
		counting = nil
		skipped = slices.Clone(sum.Skipped)
		checks = slices.Clone(sum.Checks)
		for _, f := range sum.Failed {
			if f.Category == tf.FailAPIOff {
				skipped = append(skipped, tf.Skip{Type: f.Type, Reason: tf.FailAPIOff, Excluded: true, Root: f.Root})
				// The listing ran far enough to learn there is nothing:
				// it stands for the check (an aliased block of the user's
				// covers the alias-only skip of its provider).
				if f.Root != "" {
					checks = append(checks, tf.Check{Type: f.Type, Scope: "root " + f.Root, Root: f.Root, Provider: f.Provider})
				}
				continue
			}
			counting = append(counting, f)
		}
	}
	sum.Coverage = report.BuildCoverage(byTypeScope, checks, skipped, counting)
	sum.Complete = !nothingChecked(o, sum) && len(counting) == 0 && len(sum.Warnings) == 0 && len(sum.UnreadStates) == 0 &&
		!slices.ContainsFunc(sum.Coverage, func(c report.Coverage) bool { return c.Status == report.Incomplete })
	if n := idx.NoIdentity(); n > 0 && len(live) > 0 {
		sum.Notes = append(sum.Notes, fmt.Sprintf("%d of %d state instances have no resource identity (state before Terraform 1.12): matched by attributes instead; -refresh reads identities (slow, see README)", n, idx.Len()))
	}

	switch {
	case o.adopt && len(sum.UnreadStates) > 0 && !o.allowIncomplete:
		printSummary(stderr, o, sum)
		var roots []string
		for _, u := range sum.UnreadStates {
			roots = append(roots, u.Root)
		}
		return fmt.Errorf("not writing import blocks: the state of %s could not be read, and its objects would be imported a second time; fix access, or pass -allow-incomplete", strings.Join(roots, ", "))
	case o.adopt:
		reserved := maps.Clone(idx.RootAddresses())
		var imports []tf.Import
		for _, dir := range slices.Concat(o.roots, stateDirs) {
			cfg, err := tf.ConfigAddresses(dir)
			if err != nil {
				return err
			}
			maps.Copy(reserved, cfg)
			ims, err := tf.ConfigImports(dir)
			if err != nil {
				return err
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
		n := len(orphans)
		orphans = slices.DeleteFunc(orphans, func(r model.Resource) bool { return hasImport(r, imports, roots) })
		if n > len(orphans) {
			fmt.Fprintf(stderr, "%d objects already have import blocks in the root modules; not written again.\n", n-len(orphans))
		}
		err = adopt(o, orphans, reserved, &sum, stdout, stderr)
	case o.format == "json":
		err = report.JSON(stdout, orphans, uncertain, sum)
	case o.format == "summary":
		err = report.SummaryTable(stdout, sum)
	default:
		err = report.Text(stdout, orphans, uncertain)
	}
	if err != nil {
		return err
	}
	// adopt may warn about objects it could not write: the verdict and
	// -strict must see that too.
	if len(sum.Warnings) > 0 {
		sum.Complete = false
	}
	printSummary(stderr, o, sum)
	if o.verbose {
		printScopes(stderr, scopes)
	}
	if nothingChecked(o, sum) {
		msg := "no resource types were queried, so nothing could be found"
		if len(sum.NoList) > 0 {
			slices.Sort(sum.NoList)
			msg += fmt.Sprintf(". These providers have no list resources in the installed version: %s. List resources need Terraform 1.14 and a provider release that adds them (see its changelog); upgrade the provider, or use -scan or -plugin", strings.Join(slices.Compact(sum.NoList), ", "))
		}
		fmt.Fprintln(stderr, "error:", report.Clean(msg))
		return exitCode(exitError)
	}

	switch {
	case o.strict && (!sum.Complete || sum.Uncertain > 0):
		return exitCode(exitError)
	case len(orphans) > 0 && !o.adopt:
		return exitCode(exitOrphans)
	}
	return nil
}

// dedupe drops resources listed more than once, for example by two roots
// that share a provider account. Equal types and identities from different
// providers are different objects; an unknown provider (a plugin) is taken
// for the same one. An object whose location is unknown (project "") is
// only merged with copies from the same list block: two roots can list
// different projects' objects under the same name. Attributes of the
// dropped copies are kept: one copy may come from a list block that did
// not ask for them.
func dedupe(rs []model.Resource) []model.Resource {
	seen := map[string][]int{} // key -> indexes in out
	out := rs[:0]
	for _, r := range rs {
		k := r.Key()
		if state.UnknownLocation(r.Identity) {
			k += "\x00" + r.Source + "\x00" + r.ListAddress
		}
		i := slices.IndexFunc(seen[k], func(j int) bool { return model.SameProvider(out[j].ProviderSource, r.ProviderSource) })
		if i < 0 {
			seen[k] = append(seen[k], len(out))
			out = append(out, r)
			continue
		}
		kept := &out[seen[k][i]]
		for a, v := range r.Attributes {
			if kept.Attributes == nil {
				kept.Attributes = map[string]any{}
			}
			if _, ok := kept.Attributes[a]; !ok {
				kept.Attributes[a] = v
			}
		}
	}
	return out
}

// scopeKeys are identity keys that tell which account a result came from.
var scopeKeys = []string{"project", "account_id", "subscription_id"}
