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
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

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

Exit status: 0 nothing new found, 2 unmanaged resources found (only new with
-baseline), 1 error (also on incomplete coverage with -strict, duplicate
ownership with -strict-ownership).

Run "tforphans adopt -h" to generate import blocks for what it finds.
Run "tforphans diff -h" to compare two saved JSON reports.

Flags:
`

const adoptUsage = `Usage: tforphans adopt [flags] [ROOT_DIR...]

Finds unmanaged resources like tforphans does and writes an import block for
each one. Put the blocks into the root module and generate the configuration:

  tforphans adopt -o imports.tf
  terraform plan -generate-config-out=generated.tf

With several root modules, -out-dir DIR writes a file per root module
(DIR/<root path>/imports.tf) to copy into it.

Objects that state may manage but cannot confirm (uncertain) get no block.
When a state could not be read, adopt stops: objects of that state would show
as unmanaged and be imported a second time (-allow-incomplete writes anyway).

Exit status: 0 on success, whether or not anything was found; 1 on error
(also on incomplete coverage with -strict, duplicate ownership with -strict-ownership).

Flags:
`

type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, " ") }
func (l *listFlag) Set(v string) error {
	*l = append(*l, v)
	return nil
}

type sizeFlag int64

func (s *sizeFlag) String() string { return strconv.FormatInt(int64(*s), 10) }
func (s *sizeFlag) Set(v string) error {
	multiplier := int64(1)
	if len(v) > 0 {
		switch v[len(v)-1] {
		case 'k', 'K':
			multiplier = 1 << 10
		case 'm', 'M':
			multiplier = 1 << 20
		case 'g', 'G':
			multiplier = 1 << 30
		}
		if multiplier != 1 {
			v = v[:len(v)-1]
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 || n > (math.MaxInt64-1)/multiplier {
		return errors.New("want a positive byte count, optionally followed by K, M or G (powers of 1024), below 9223372036854775807 bytes")
	}
	*s = sizeFlag(n * multiplier)
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
	roots        []string
	stateRoots   listFlag
	init         bool
	currentWS    bool // read only the selected workspace of each root
	anyVersion   bool // lift the roots' required_version
	states       listFlag
	maxStateSize int64
	maxQuerySize int64
	// timeout bounds the whole run, sourceTimeout each root, scan and plugin.
	timeout, sourceTimeout time.Duration
	plugins                listFlag
	scans                  []string
	scanScope              map[string][]string // -scan-scope ARG=values
	scopeState             bool                // -scan-scope state
	refresh                bool
	mu                     sync.Mutex
	stateNotes             []string // notes of state reading, from several goroutines
	args                   []tf.Arg
	include                glob.List
	exclude                glob.List
	limit                  int
	format                 string
	adopt                  bool              // "adopt" subcommand: write import blocks
	out                    string            // adopt: output file, empty for stdout
	outDir                 string            // adopt: directory with a file per root module
	adoptRoots             map[string]string // adopt -adopt-root: -scan provider or pluginN to root module directory
	baselinePath           string
	environment            string
	ignorePath             string
	presets                []string
	includeAttrs           []string
	ignore                 *report.Ignore // loaded in execute
	strict                 bool
	verbose                bool
	explain                bool
	noAliases              bool
	printQuery             bool
	noQuery                bool
	allowUnpinned          bool
	// adopt: write import blocks although a state could not be read.
	allowIncomplete bool
	allowHTTP       bool
	proxmoxStates   bool
	pluginStates    bool
	terraformBin    string
	maxRoots        int
	progress        io.Writer // progress lines of long steps (stderr)
	// steps, when set, gets a line per root and data source scan, so that
	// a long run does not look hung (stderr on a terminal, without -quiet).
	steps io.Writer
	quiet bool
	// impliedNoQuery: no root given and none in the current directory, so
	// only -scan and -plugin run.
	impliedNoQuery bool
	// noDefaultPresets: report.DefaultPresets are not applied.
	noDefaultPresets bool
	// namesFromTags: adopt names resources after their Name tag.
	namesFromTags bool
	// stateParallelism is how many -state-roots are read at once.
	stateParallelism int
	strictOwnership  bool
	strictAPI        bool
	strictIgnore     glob.List // -strict-ignore
	terragrunt       bool
	tgUnits          listFlag // -terragrunt-unit
	configFile       string   // flags read from this file
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
	// Started by terragrunt as its terraform (see tf.TerragruntStatePull).
	tf.RunWrapper()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	tf.CloseTurns()
	os.Exit(code)
}

func parseFlags(argv []string, stderr io.Writer) (*options, error) {
	o := &options{maxStateSize: state.DefaultMaxSize, maxQuerySize: tf.DefaultMaxQuerySize}
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
	var argFlags, include, exclude, scans, presets, attrs, scanScopes, strictIgnore, adoptRoots listFlag
	fs.Var(&o.states, "state", "extra state that manages resources in the same accounts: file, http(s) URL (uses TF_HTTP_USERNAME/TF_HTTP_PASSWORD) or - for stdin; repeatable")
	fs.IntVar(&o.stateParallelism, "state-parallelism", 8, "how many -state-roots states are read at the same time (1 to 64)")
	fs.DurationVar(&o.timeout, "timeout", 0, "stop the whole run after this long, e.g. 30m (0: no limit)")
	fs.DurationVar(&o.sourceTimeout, "source-timeout", 0, "give up on one root, -scan or plugin after this long and go on with the others, e.g. 10m (0: no limit)")
	fs.Var((*sizeFlag)(&o.maxQuerySize), "max-query-size", "maximum bytes of one terraform query's output, read as it comes; accepts K, M or G suffixes")
	fs.Var((*sizeFlag)(&o.maxStateSize), "max-state-size", "maximum bytes per state, including state pull and refresh output; accepts K, M or G suffixes (powers of 1024)")
	fs.Var(&o.stateRoots, "state-roots", "directory tree whose root modules' states (all workspaces) count as managed, read with terraform state pull; repeatable")
	fs.BoolVar(&o.proxmoxStates, "proxmox-cluster-states", false, "state that every state given belongs to the Proxmox cluster scanned with -scan proxmox; without it Proxmox matches are uncertain, since state does not record the cluster")
	fs.BoolVar(&o.pluginStates, "plugin-cluster-states", false, "state that every state given belongs to the cluster or account the plugins scan; without it results a plugin marks \"unverified\" (state does not record where they live) match only as uncertain")
	fs.BoolVar(&o.allowHTTP, "allow-http", false, "allow -state URLs over plain http (never with credentials)")
	fs.BoolVar(&o.allowUnpinned, "allow-unpinned", false, "let -scan install the newest provider release when no root module pins its version")
	fs.BoolVar(&o.currentWS, "current-workspace", false, "read only the selected workspace of each root module instead of listing all workspaces")
	fs.BoolVar(&o.anyVersion, "ignore-required-version", false, "lift the root modules' required_version with a temporary override file, so that a root pinned to an older Terraform can be queried (child modules' constraints still apply)")
	fs.BoolVar(&o.refresh, "refresh", false, "read each root's state through terraform plan -refresh-only (no lock, nothing written) instead of state pull: state written before Terraform 1.12 gets resource identities, so matches are exact; slower, needs the root's credentials and variables")
	fs.BoolVar(&o.strictAPI, "strict-api", true, "count listings that failed because the API is not enabled (GCP) as incomplete coverage (the default: a disabled API can still hold data, Cloud Storage and BigQuery keep theirs); -strict-api=false takes it as holding no objects, -strict-ignore TYPE_GLOB exempts named types")
	fs.Var(&strictIgnore, "strict-ignore", "types matching these globs do not make coverage incomplete when their listing fails or they are skipped for want of an argument (still listed when possible, and still reported), e.g. google_firebase_*")
	fs.IntVar(&o.maxRoots, "max-roots", 200, "stop when -state-roots finds more root modules than this, before terraform runs in them (0: no limit)")
	fs.BoolVar(&o.terragrunt, "terragrunt", false, "with -state-roots, also read Terragrunt units (directories with a terragrunt.hcl, except parents that units include) through terragrunt in PATH; terragrunt initializes them (.terragrunt-cache)")
	fs.Var(&o.tgUnits, "terragrunt-unit", "with -terragrunt, read DIR as a Terragrunt unit even though units include its terragrunt.hcl (taken for shared configuration otherwise); repeatable")
	fs.BoolVar(&o.init, "init", false, "run terraform init in root modules that are not initialized (creates .terraform there)")
	fs.Var(&scans, "scan", "list a provider without list resources through its plural data sources: cloudflare, proxmox; credentials from the provider's environment variables (CLOUDFLARE_API_TOKEN, PROXMOX_VE_*), not from the root's provider block; repeatable")
	fs.Var(&scanScopes, "scan-scope", "limit -scan to some zones or accounts: zone_id=ID_OR_NAME,... or account_id=...; with no value (account_id=) that scope is not read; \"state\" limits every other scope to the IDs found in the states read; repeatable")
	fs.Var(&o.plugins, "plugin", "discovery command for providers without list support, run with sh -c; repeatable")
	fs.Var(&argFlags, "arg", "fan list queries out over values of a list config argument, e.g. region=eu-north-1,us-east-1; TYPE_GLOB:name=... limits it to matching types (google_compute_*:zone=europe-west1-b) and wins over the same name without one; zone=auto or region=auto (google) reads every zone or region through the root's provider; repeatable")
	fs.Var(&include, "types", "only query resource types matching these globs, e.g. aws_s3_*,aws_iam_role")
	fs.Var(&exclude, "exclude-types", "do not query resource types matching these globs")
	fs.IntVar(&o.limit, "limit", 100000, "maximum results per list query")
	if o.adopt {
		fs.StringVar(&o.out, "o", "", "write the import blocks to this new file instead of stdout")
		fs.StringVar(&o.outDir, "out-dir", "", "write the import blocks to this new directory, one imports.tf per root module at its path from the current directory")
		fs.Var(&adoptRoots, "adopt-root", "with -out-dir: put what a -scan provider or plugin found into this root module's file, e.g. cloudflare=zones/example.com or plugin1=vault; without it they go to _unassigned/NAME.tf; repeatable")
		fs.BoolVar(&o.allowIncomplete, "allow-incomplete", false, "write import blocks even when a state could not be read (its objects would be imported a second time)")
		fs.BoolVar(&o.namesFromTags, "names-from-tags", false, "name the resources after their Name tag where the type has tags (slower: terraform query returns whole objects for those types)")
	} else {
		fs.StringVar(&o.format, "format", "text", "output format: text, json, or summary (counts per type)")
	}
	fs.StringVar(&o.baselinePath, "baseline", "", "previous -format json report; show and fail only on new findings (not supported with adopt)")
	fs.StringVar(&o.environment, "environment", "", "name of the environment this run checks, e.g. prod; stored in the JSON report, and a -baseline of another environment verifies nothing")
	fs.StringVar(&o.ignorePath, "ignore", "", "ignore file with TYPE_GLOB[:NAME_GLOB] [KEY=GLOB...] lines (default "+defaultIgnoreFile+" if present)")
	fs.Var(&presets, "ignore-preset", "add built-in ignore rules: "+strings.Join(report.Presets(), ", ")+"; repeatable ("+strings.Join(report.DefaultPresets, ", ")+" are applied by default)")
	fs.BoolVar(&o.noDefaultPresets, "no-default-presets", false, "do not apply the default ignore presets ("+strings.Join(report.DefaultPresets, ", ")+"), which hide objects the cloud creates on its own")
	fs.Var(&attrs, "include-attrs", "fetch these resource attributes with the list results and print them in -format json, e.g. labels,creation_timestamp (slower: terraform query returns whole objects)")
	fs.BoolVar(&o.strictOwnership, "strict-ownership", false, "exit 1 for confirmed duplicate state ownership or duplicate addresses within a state; possible ownership does not fail")
	fs.BoolVar(&o.strict, "strict", false, "exit 1 when coverage is incomplete: failed or truncated queries, unread states, plugin errors, types skipped for want of an argument or a default provider configuration, or uncertain matches (state without identity, Proxmox, plugin results marked unverified)")
	fs.BoolVar(&o.noAliases, "no-aliases", false, "list resources only through each provider's default configuration, not through its aliases (provider \"aws\" { alias = \"prod\" }); the aliases' types then count as not checked")
	fs.BoolVar(&o.explain, "explain", false, "say for every live object why it counts as managed, uncertain, ignored or unmanaged: the state address and where it was read, the ignore rule, or the closest state instances")
	fs.BoolVar(&o.verbose, "v", false, "list skipped resource types and every failed query, and the projects and accounts results came from")
	fs.BoolVar(&o.quiet, "quiet", false, "print no progress lines (by default printed on a terminal: roots read, roots queried, data source scans, -refresh)")
	fs.BoolVar(&o.printQuery, "print-query", false, "print the temporary files a run would write into each root (provider override, auto data sources, list blocks), each under a \"# file\" header, and exit")
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
	if len(o.tgUnits) > 0 && !o.terragrunt {
		return nil, errors.New("-terragrunt-unit needs -terragrunt")
	}
	for _, list := range [][]string{o.roots, o.stateRoots, o.tgUnits} {
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

	// An upper bound too: a config file from the repository must not start
	// thousands of terraform processes at once.
	if o.stateParallelism < 1 || o.stateParallelism > 64 {
		return nil, errors.New("-state-parallelism must be between 1 and 64")
	}
	if o.adopt && o.baselinePath != "" {
		return nil, errors.New("-baseline is not supported with adopt")
	}
	if o.explain && (o.adopt || o.format == "summary") {
		return nil, errors.New("-explain needs the text or json report (not adopt or -format summary)")
	}
	if len(o.environment) > 100 || report.Clean(o.environment) != o.environment {
		return nil, errors.New("-environment must be one line of at most 100 bytes without control characters or extra spaces")
	}
	if o.baselinePath == "-" {
		return nil, errors.New("-baseline must be a file path, not -")
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
	if err := adoptRootFlags(o, adoptRoots); err != nil {
		return nil, err
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
	o.strictIgnore = glob.CompileList(strictIgnore.csv())
	if len(o.roots) == 0 && !o.noQuery {
		if (len(o.scans) > 0 || len(o.plugins) > 0) && !tf.HasConfig(".") {
			// Nothing to query here: -scan and -plugin need no root.
			o.noQuery, o.impliedNoQuery = true, true
		} else {
			o.roots = []string{"."}
		}
	}
	if n := len(slices.DeleteFunc(slices.Clone(o.states), func(s string) bool { return s != "-" })); n > 1 {
		return nil, errors.New("-state - (stdin) can be given only once")
	}
	if o.noQuery && len(o.plugins) == 0 && len(o.scans) == 0 {
		return nil, errors.New("-no-query needs at least one -scan or -plugin")
	}
	if err := outDirRoots(o); err != nil {
		return nil, err
	}
	return o, nil
}

func run(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	if len(argv) > 0 && argv[0] == "diff" {
		return runDiff(argv[1:], stdout, stderr)
	}
	o, err := parseFlags(argv, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", report.CleanLines(err.Error()))
		return exitError
	}
	if !o.quiet {
		o.progress = stderr
		if isTerminal(stderr) {
			o.steps = stderr
		}
	}
	if o.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.timeout)
		defer cancel()
	}
	if err := execute(ctx, o, stdout, stderr); err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
			err = fmt.Errorf("stopped after %s (-timeout)", o.timeout)
		}
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
		err = report.SummaryTable(stdout, sum)
	default:
		err = report.Text(stdout, shown, uncertain)
		if err == nil {
			err = report.ExplainText(stdout, sum.Explain)
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
	printSummary(stderr, o, sum)
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
		fmt.Fprintf(stderr, "baseline: %d new, %d known (not shown), %d resolved%s, %d not verified (%s)\n", baseline.New, baseline.Known, baseline.Resolved, resolved, baseline.Unverified, why)
		if o.verbose {
			if err := report.PrintFindings(stderr, baseline.UnverifiedFindings, 8); err != nil {
				return err
			}
		}
	}
	if o.verbose {
		printScopes(stderr, scopes)
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

// dedupe drops resources listed more than once, for example by two roots
// that share a provider account. Equal types and identities from different
// providers are different objects; an unknown provider (a plugin) is taken
// for the same one. An object whose location is unknown (project "") is
// only merged with copies from the same list block: two roots can list
// different projects' objects under the same name. Attributes of the
// dropped copies are kept: one copy may come from a list block that did
// not ask for them.
func dedupe(rs []model.Resource) []model.Resource {
	out, _ := dedupeMembers(rs)
	return out
}

// dedupeMembers is dedupe that also returns, by memberKey of each kept
// result, every result merged into it (the kept one too).
func dedupeMembers(rs []model.Resource) ([]model.Resource, map[string][]model.Resource) {
	// One slot per key and provider family; "" collects the results
	// without a provider. The order of the results must not matter, so
	// those join a known family only when it is the only one: an object of
	// unknown provider could be either of two, and swallowing both would
	// hide one.
	type slot struct {
		r       model.Resource
		first   int // position of the first result in the slot
		members []model.Resource
	}
	slots := map[string]map[string]*slot{}
	var keys []string
	for i, r := range rs {
		k := r.Key()
		if state.UnknownLocation(r.Identity) {
			k += "\x00" + r.Source + "\x00" + r.ListAddress
		}
		family := ""
		if r.ProviderSource != "" {
			family = model.ProviderFamily(r.ProviderSource)
		}
		if slots[k] == nil {
			slots[k] = map[string]*slot{}
			keys = append(keys, k)
		}
		if s := slots[k][family]; s != nil {
			mergeResult(&s.r, r)
			s.members = append(s.members, r)
			continue
		}
		slots[k][family] = &slot{r, i, []model.Resource{r}}
	}
	var out []*slot
	for _, k := range keys {
		group := slots[k]
		if unknown := group[""]; unknown != nil && len(group) == 2 {
			delete(group, "")
			for _, known := range group {
				mergeResult(&known.r, unknown.r)
				known.first = min(known.first, unknown.first)
				known.members = append(known.members, unknown.members...)
			}
		}
		for _, s := range group {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b *slot) int { return a.first - b.first })
	res := make([]model.Resource, len(out))
	members := map[string][]model.Resource{}
	for i, s := range out {
		res[i] = s.r
		members[memberKey(s.r)] = s.members
	}
	return res, members
}

// memberKey tells the results dedupe kept apart: one slot per originKey and
// provider family.
func memberKey(r model.Resource) string {
	family := ""
	if r.ProviderSource != "" {
		family = model.ProviderFamily(r.ProviderSource)
	}
	return originKey(r) + "\x00" + family
}

// originKey keys the listings of an object as dedupe's slots: with an
// unknown location, only the same listing is the same object.
func originKey(r model.Resource) string {
	if state.UnknownLocation(r.Identity) {
		return r.Key() + "\x00" + r.Source + "\x00" + r.ListAddress
	}
	return r.Key()
}

// classified is what classify sorts the live objects into.
type classified struct {
	orphans   []model.Resource
	uncertain []report.Uncertain
	// seen are the live objects that are no orphans, once per listing
	// that returned them (origins), for -baseline.
	seen []report.Seen
	// byTypeScope counts per type and scope name, for coverage.
	byTypeScope map[[2]string]*report.TypeCount
}

// classify matches each live object against the states and the ignore
// rules and counts it in sum (managed, ignored, uncertain, orphans; per
// type and scope). It fills in the scope names of live.
func classify(o *options, idx *state.Index, live []model.Resource, origins map[string][]model.Resource, sum *report.Summary) classified {
	c := classified{byTypeScope: map[[2]string]*report.TypeCount{}}
	addSeen := func(r model.Resource, status, address, rule string) {
		for _, origin := range origins[originKey(r)] {
			if !model.SameProvider(origin.ProviderSource, r.ProviderSource) {
				continue
			}
			cp := r
			cp.Source, cp.ListAddress = origin.Source, origin.ListAddress
			c.seen = append(c.seen, report.Seen{Resource: cp, Status: status, StateAddress: address, Rule: rule})
		}
	}
	sum.ByType = map[string]*report.TypeCount{}
	byScope := map[string]*report.TypeCount{}
	for i := range live {
		r := &live[i]
		if r.ScopeName == "" {
			r.ScopeName = scopeName(r.Identity)
		}
		where := r.ScopeName
		if where == "" {
			where = "unknown"
		}
		counts := []*report.TypeCount{sum.ByType[r.Type], byScope[where], c.byTypeScope[[2]string{r.Type, r.ScopeName}]}
		for i, tc := range counts {
			if tc == nil {
				counts[i] = &report.TypeCount{}
			}
		}
		sum.ByType[r.Type], byScope[where], c.byTypeScope[[2]string{r.Type, r.ScopeName}] = counts[0], counts[1], counts[2]
		for _, tc := range counts {
			tc.Live++
		}
		// An ignore rule wins over an uncertain match, not over a
		// confirmed one.
		var m state.Match
		var rejected []state.Rejection
		var ofType int
		if o.explain {
			m, rejected, ofType = idx.Explain(*r, 3)
		} else {
			m = idx.Match(*r)
		}
		rule, ignored := "", false
		if !m.Managed || !m.Verified {
			rule, ignored = o.ignore.Rule(*r)
		}
		if o.explain {
			sum.Explain = append(sum.Explain, explain(*r, m, rejected, ofType, rule, ignored))
		}
		switch {
		case m.Managed && m.Verified:
			addSeen(*r, "managed", m.Address, "")
			sum.Managed++
			if m.ByIdentity {
				sum.ManagedByIdentity++
			}
			for _, tc := range counts {
				tc.Managed++
			}
		case ignored:
			addSeen(*r, report.BecameIgnored, "", rule)
			sum.Ignored++
			for _, tc := range counts {
				tc.Ignored++
			}
		case m.Managed:
			addSeen(*r, report.BecameUnsure, m.Address, "")
			sum.Uncertain++
			c.uncertain = append(c.uncertain, report.Uncertain{Resource: *r, StateAddress: m.Address})
			for _, tc := range counts {
				tc.Uncertain++
			}
		default:
			c.orphans = append(c.orphans, *r)
			for _, tc := range counts {
				tc.Orphans++
			}
		}
	}
	// Scopes are worth a breakdown when there are several.
	if len(byScope) > 1 || len(byScope) == 1 && byScope["unknown"] == nil {
		sum.ByScope = byScope
	}
	return c
}

// coverageInputs returns the failed listings that count as gaps, and the
// skips and checks coverage is built from, after -strict-api and
// -strict-ignore. It counts disabled APIs and strict-ignored listings in
// sum and marks the skips -strict-ignore lets pass.
func coverageInputs(o *options, sum *report.Summary) (counting []tf.Failure, skipped []tf.Skip, checks []tf.Check) {
	// A disabled API (GCP) is a gap: disabling Cloud Storage or BigQuery
	// keeps their data, so the failure proves nothing about objects.
	// -strict-api=false takes it as empty, as before; types named in
	// -strict-ignore are no gap either, whatever went wrong.
	counting, skipped, checks = sum.Failed, sum.Skipped, sum.Checks
	for _, f := range sum.Failed {
		if f.Category == tf.FailAPIOff {
			sum.APIOff++
		}
	}
	// A skip that -strict-ignore lets pass says so in skipped_types too.
	for i, sk := range sum.Skipped {
		if !sk.Excluded && o.strictIgnore.Match(sk.Type) {
			sum.Skipped[i].Excluded = true
			sum.Skipped[i].Reason = report.StrictIgnored + sk.Reason
			sum.StrictIgnored++
		}
	}
	if !o.strictAPI || len(o.strictIgnore) > 0 {
		counting = nil
		skipped = slices.Clone(sum.Skipped)
		checks = slices.Clone(sum.Checks)
		for _, f := range sum.Failed {
			switch {
			case f.Category == tf.FailAPIOff && !o.strictAPI:
				skipped = append(skipped, tf.Skip{Type: f.Type, Reason: tf.FailAPIOff, Excluded: true, Root: f.Root})
				// The listing ran far enough to learn there is nothing:
				// it stands for the check (an aliased block of the user's
				// covers the alias-only skip of its provider).
				if f.Root != "" {
					checks = append(checks, tf.Check{Type: f.Type, Scope: "root " + f.Root, Root: f.Root, Provider: f.Provider})
				}
			case o.strictIgnore.Match(f.Type):
				skipped = append(skipped, tf.Skip{Type: f.Type, Reason: report.StrictIgnored + f.Category, Excluded: true, Root: f.Root})
				sum.StrictIgnored++
			default:
				counting = append(counting, f)
			}
		}
	}
	return counting, skipped, checks
}

// mergeResult adds what a duplicate result knows to the kept one.
func mergeResult(kept *model.Resource, r model.Resource) {
	// A user block without include_resource may answer first: the copy
	// with the object keeps its Name tag for the address.
	if kept.TagName == "" {
		kept.TagName = r.TagName
	}
	// A copy whose place is unknown or restricted must not be forgotten
	// because the other copy came first.
	kept.Scope = model.JoinScope(kept.Scope, r.Scope)
	for a, v := range r.Attributes {
		if kept.Attributes == nil {
			kept.Attributes = map[string]any{}
		}
		if _, ok := kept.Attributes[a]; !ok {
			kept.Attributes[a] = v
		}
	}
}

// scopeKeys are identity keys that tell which account a result came from.
var scopeKeys = []string{"project", "account_id", "subscription_id"}

// unlistedTypes are the types in the states whose provider was asked (its
// schema read, a scan of it run, or results from a plugin, which declares
// nothing up front: what it returned is what it lists) but which that
// provider cannot list: tforphans never checks objects of them. Types left
// out with -types or -exclude-types are the user's choice and not reported.
func unlistedTypes(o *options, counts map[state.TypeKey]int, sum report.Summary) []report.UnlistedType {
	var out []report.UnlistedType
	for k, n := range counts {
		if !sum.QueriedFamilies[k.Provider] || sum.CanList(k.Type, k.Provider) || len(o.include) > 0 && !o.include.Match(k.Type) || o.exclude.Match(k.Type) {
			continue
		}
		out = append(out, report.UnlistedType{Type: k.Type, Provider: k.Provider, Instances: n})
	}
	slices.SortFunc(out, func(a, b report.UnlistedType) int {
		if a.Instances != b.Instances {
			return b.Instances - a.Instances
		}
		return strings.Compare(a.Type+"\x00"+a.Provider, b.Type+"\x00"+b.Provider)
	})
	return out
}

func unlistedNote(types []report.UnlistedType) string {
	var names []string
	for i, t := range types {
		if i == 8 {
			names = append(names, fmt.Sprintf("and %d more (-format json lists all)", len(types)-i))
			break
		}
		names = append(names, fmt.Sprintf("%s (%d)", t.Type, t.Instances))
	}
	return fmt.Sprintf("%d resource types in the states cannot be listed by their provider, so objects of them are never checked: %s", len(types), strings.Join(names, ", "))
}

// withSource runs one root, scan or plugin under -source-timeout and says
// so when that ran out.
func withSource[T any](ctx context.Context, o *options, f func(context.Context) (T, error)) (T, error) {
	if o.sourceTimeout <= 0 {
		return f(ctx)
	}
	// Waiting for the turn to run terraform init does not count.
	srcCtx, cancel := tf.WithBudget(ctx, o.sourceTimeout)
	defer cancel()
	v, err := f(srcCtx)
	// Also when f returned no error: work after its last command may not
	// look at the context, and a result past the deadline is not complete.
	if ctx.Err() == nil && srcCtx.Err() != nil {
		err = sourceTimeout{o.sourceTimeout, cmp.Or(err, srcCtx.Err())}
	}
	return v, err
}

// sourceTimeout keeps the error it replaces in the chain: a root whose
// query timed out after its state was read is still a queryError.
type sourceTimeout struct {
	after time.Duration
	err   error
}

func (e sourceTimeout) Error() string {
	return fmt.Sprintf("gave up after %s (-source-timeout)", e.after)
}
func (e sourceTimeout) Unwrap() error { return e.err }

// explain says why a live object counts as it does (-explain); the order
// of the cases is the one of the decision.
func explain(r model.Resource, m state.Match, rejected []state.Rejection, ofType int, rule string, ignored bool) report.Explanation {
	e := report.Explanation{Type: r.Type, Identity: r.Identity, DisplayName: r.DisplayName, Scope: r.ScopeName, Source: r.Source}
	where := func(m state.Match) string {
		s := m.Address
		if m.Source != "" {
			s += " in " + m.Source
		}
		switch m.Via {
		case "identity":
			s += ", by resource identity"
		case "attributes":
			s += ", by attributes (the state instance has no resource identity)"
		case "owner":
			s = "created and owned by " + s
		}
		if m.Type != "" {
			s += ", as " + m.Type
		}
		return s
	}
	switch {
	case m.Managed && m.Verified:
		e.Status, e.Reason = "managed", where(m)
	case ignored:
		e.Status, e.Rule, e.Reason = "ignored", rule, "ignore rule "+rule
		if m.Managed {
			e.Reason += "; it also matches " + m.Address + " without confirmation"
		}
	case m.Managed:
		e.Status, e.Reason = "uncertain", "matches "+where(m)+", but "
		switch {
		case m.ScopeUnknown:
			e.Reason += "the state does not record the cluster or account (trust it with -proxmox-cluster-states or -plugin-cluster-states)"
		case len(m.Unknown) > 0:
			e.Reason += "the listing left " + strings.Join(m.Unknown, ", ") + " empty, so where the object lives is not known"
		case len(m.Unconfirmed) > 0:
			e.Reason += "the state does not hold " + strings.Join(m.Unconfirmed, ", ") + " to confirm where the object lives"
		default:
			e.Reason += "the state cannot confirm where the object lives"
		}
	default:
		e.Status = "unmanaged"
		switch {
		case ofType == 0:
			e.Reason = "no state instance of " + r.Type + " was read"
		case len(rejected) == 0:
			e.Reason = fmt.Sprintf("none of the %d state instances of %s shares its identity", ofType, r.Type)
		default:
			var near []string
			for _, x := range rejected {
				s := x.Address
				if x.Source != "" {
					s += " in " + x.Source
				}
				near = append(near, s+": "+x.Why)
			}
			e.Reason = "closest: " + strings.Join(near, "; ")
		}
	}
	if m.Managed {
		e.StateAddress, e.StateSource = m.Address, m.Source
	}
	return e
}

func countTransition(fs []report.Finding, transition string) int {
	n := 0
	for _, f := range fs {
		if f.Transition == transition {
			n++
		}
	}
	return n
}

// transitionSummary counts what became of resolved findings: "1 became
// managed, 2 no longer found".
func transitionSummary(fs []report.Finding) string {
	var parts []string
	for _, t := range [][2]string{{report.BecameManaged, "became managed"}, {report.BecameIgnored, "ignored"}, {report.NoLongerFound, "no longer found"}} {
		if n := countTransition(fs, t[0]); n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, t[1]))
		}
	}
	return strings.Join(parts, ", ")
}
