package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/5xxeu/tforphans/internal/cliflag"
	"github.com/5xxeu/tforphans/internal/glob"
	"github.com/5xxeu/tforphans/internal/report"
	"github.com/5xxeu/tforphans/internal/state"
	"github.com/5xxeu/tforphans/internal/tf"
)

const usage = `Usage: tforphans [flags] [ROOT_DIR...]

Finds live resources that are not managed by any of the given Terraform
states. Each ROOT_DIR (default "." when it has Terraform configuration or no
other state source is given) must be an initialized root module; its state
is read with "terraform state pull" and its providers are asked to list
live resources with "terraform query".

Exit status: 0 nothing new found, 2 unmanaged resources found (only new with
-baseline), 1 error (also on incomplete coverage with -strict, duplicate
ownership with -strict-ownership).

Run "tforphans adopt -h" to generate import blocks for what it finds.
Run "tforphans init" to discover roots and create .tforphans.yaml.
Run "tforphans doctor -h" to check readiness before an audit.
Run "tforphans diff -h" to compare two saved JSON reports.

Examples:
  tforphans                         audit the root module in the current directory
  tforphans -state-roots infra      also count every state under infra/ as managed
  tforphans -scan cloudflare -state-roots dns
                                    list Cloudflare through data sources
  tforphans -format json > old.json && tforphans -baseline old.json
                                    later: report only what is new
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
`

// flagGroups orders -h of the audit, adopt and doctor; each lists only the
// flags it defines.
var flagGroups = []cliflag.Group{
	{Title: "States that count as managed", Names: []string{"state-roots", "state", "current-workspace", "refresh", "init", "terragrunt", "terragrunt-unit", "allow-http", "max-roots", "state-parallelism", "max-state-size"}},
	{Title: "What to list", Names: []string{"types", "exclude-types", "arg", "no-aliases", "scan", "scan-scope", "allow-unpinned", "plugin", "no-query", "include-attrs", "limit", "max-query-size", "ignore-required-version", "terraform"}},
	{Title: "Ignoring and comparing", Names: []string{"ignore", "ignore-preset", "no-default-presets", "baseline", "environment"}},
	{Title: "Strictness and trust", Names: []string{"strict", "strict-api", "strict-ignore", "strict-ownership", "expect", "proxmox-cluster-states", "plugin-cluster-states"}},
	{Title: "Import blocks", Names: []string{"o", "out-dir", "adopt-root", "allow-incomplete", "names-from-tags"}},
	{Title: "Checks", Names: []string{"offline", "check-auth"}},
	{Title: "Output", Names: []string{"format", "explain", "v", "quiet", "print-query"}},
	{Title: "Run", Names: []string{"config", "no-config", "timeout", "source-timeout", "version"}},
}

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

// options is the parsed command line of the audit, adopt and doctor. init
// and diff parse their own flags. Each group says which commands read it.
type options struct {
	src    sourceOptions
	ignore ignoreOptions
	audit  auditOptions
	adopt  *adoptOptions  // nil unless the adopt subcommand
	doctor *doctorOptions // nil unless the doctor subcommand
	run    runState

	format     string
	verbose    bool
	quiet      bool
	timeout    time.Duration // bounds the whole run
	configFile string        // flags read from this file
}

// sourceOptions say where states and live objects are read: root modules,
// other states, terraform query, -scan and plugins. Audit, adopt and doctor.
type sourceOptions struct {
	roots        []string
	stateRoots   listFlag
	init         bool
	currentWS    bool // read only the selected workspace of each root
	anyVersion   bool // lift the roots' required_version
	states       listFlag
	maxStateSize int64
	maxQuerySize int64
	// sourceTimeout bounds each root, scan and plugin.
	sourceTimeout time.Duration
	plugins       listFlag
	scans         []string
	scanScope     map[string][]string // -scan-scope ARG=values
	scopeState    bool                // -scan-scope state
	refresh       bool
	args          []tf.Arg
	include       glob.List
	exclude       glob.List
	limit         int
	includeAttrs  []string
	noAliases     bool
	printQuery    bool
	noQuery       bool
	allowUnpinned bool
	allowHTTP     bool
	proxmoxStates bool
	pluginStates  bool
	terraformBin  string
	maxRoots      int
	// impliedNoQuery: no root given and none in the current directory, so
	// only -scan and -plugin run.
	impliedNoQuery bool
	// impliedNoRoot: no root given and none in the current directory, but
	// other state sources are: the directory is not read as a root.
	impliedNoRoot bool
	// emptyDefaultRoot: "." was taken for the root without any Terraform
	// configuration in it and no -scan or -plugin (checkRoots).
	emptyDefaultRoot bool
	// stateParallelism is how many -state-roots are read at once.
	stateParallelism int
	terragrunt       bool
	tgUnits          listFlag // -terragrunt-unit
}

// ignoreOptions say which ignore rules apply. Audit, adopt and doctor.
type ignoreOptions struct {
	path    string
	presets []string
	// noDefaults: report.DefaultPresets are not applied.
	noDefaults bool
}

// auditOptions say how results are judged. Audit and adopt.
type auditOptions struct {
	baseline        string
	environment     string
	strict          bool
	explain         bool
	strictOwnership bool
	strictAPI       bool
	strictIgnore    glob.List // -strict-ignore
	// expect are the scopes some listing has to confirm (-expect).
	expect []map[string]string
}

// adoptOptions are the flags of the adopt subcommand: write import blocks.
type adoptOptions struct {
	out    string            // output file, empty for stdout
	outDir string            // directory with a file per root module
	roots  map[string]string // -adopt-root: -scan provider or pluginN to root module directory
	// allowIncomplete: write import blocks although a state could not be read.
	allowIncomplete bool
	// namesFromTags: name resources after their Name tag.
	namesFromTags bool
}

// root is the directory -adopt-root names for a -scan provider or plugin;
// a run that is not adopt has none.
func (a *adoptOptions) root(name string) (string, bool) {
	if a == nil {
		return "", false
	}
	d, ok := a.roots[name]
	return d, ok
}

// tagNames reports whether -names-from-tags is given.
func (a *adoptOptions) tagNames() bool { return a != nil && a.namesFromTags }

// doctorOptions are the flags of the doctor subcommand.
type doctorOptions struct {
	offline, checkAuth bool
	configFlags        map[string]bool
	ignoreFlags        *flag.FlagSet
	flagSet            *flag.FlagSet
	args               []commandArg
}

// runState is not configuration: what a run collects as it goes.
type runState struct {
	mu         sync.Mutex
	stateNotes []string  // notes of state reading, from several goroutines
	progress   io.Writer // progress lines of long steps (stderr)
	// steps, when set, gets a line per root and data source scan, so that
	// a long run does not look hung (stderr on a terminal, without -quiet).
	steps  io.Writer
	ignore *report.Ignore // loaded in execute
}

const impliedNoRootNote = "no ROOT_DIR given and no Terraform configuration in the current directory: only the given states are read; list objects with -scan, -plugin or a root with list resources"

const noRootError = "no Terraform configuration in the current directory: pass a ROOT_DIR, -state-roots DIR, -scan or -plugin (tforphans init finds the root modules under a directory)"

var commands = []string{"adopt", "doctor", "init", "diff"}

// checkRoots fails before a run that could not check anything: no
// directory to look at, or a root or -state-roots directory that does not
// exist, of which terraform would only report that it cannot start there.
// A missing directory named like a command is most likely a misplaced or
// misspelt one.
func checkRoots(src *sourceOptions) error {
	if src.emptyDefaultRoot {
		return errors.New(noRootError)
	}
	roots, stateRoots := src.roots, src.stateRoots
	for _, list := range []struct {
		what  string
		paths []string
	}{{"ROOT_DIR", roots}, {"-state-roots", stateRoots}} {
		for _, p := range list.paths {
			fi, err := os.Stat(p)
			switch {
			case err == nil && !fi.IsDir():
				return fmt.Errorf("%s %q is not a directory", list.what, report.Clean(p))
			case err == nil:
				continue
			case !errors.Is(err, os.ErrNotExist):
				return fmt.Errorf("%s %q: %w", list.what, report.Clean(p), err)
			}
			msg := fmt.Sprintf("%s %q does not exist", list.what, report.Clean(p))
			if list.what == "ROOT_DIR" {
				if c := nearCommand(p); c == p {
					msg += fmt.Sprintf("; the command goes first: tforphans %s [flags]", c)
				} else if c != "" {
					msg += fmt.Sprintf("; did you mean tforphans %s?", c)
				}
			}
			return errors.New(msg)
		}
	}
	return nil
}

// nearCommand returns the command name within two edits of s, or "".
func nearCommand(s string) string {
	for _, c := range commands {
		if editDistance(s, c) <= 2 {
			return c
		}
	}
	return ""
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// parseFlags parses the command line of the audit, adopt or doctor. -h and
// -version print to out.
func parseFlags(argv []string, out io.Writer) (*options, error) {
	o := &options{src: sourceOptions{maxStateSize: state.DefaultMaxSize, maxQuerySize: tf.DefaultMaxQuerySize}}
	name, help := "tforphans", usage
	// A root module directory named "adopt" can be passed as ./adopt.
	if len(argv) > 0 && argv[0] == "adopt" {
		o.adopt, argv = &adoptOptions{}, argv[1:]
		name, help = "tforphans adopt", adoptUsage
	} else if len(argv) > 0 && argv[0] == "doctor" {
		o.doctor, argv = &doctorOptions{}, argv[1:]
		name, help = "tforphans doctor", doctorUsage
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	var argFlags, include, exclude, scans, presets, attrs, scanScopes, strictIgnore, adoptRoots, expect listFlag
	fs.Var(&o.src.states, "state", "extra state that manages resources in the same accounts: file, http(s) URL (uses TF_HTTP_USERNAME/TF_HTTP_PASSWORD), s3://BUCKET/KEY, s3://BUCKET/PREFIX/ for every .tfstate under it (AWS default credentials) or - for stdin; repeatable")
	fs.IntVar(&o.src.stateParallelism, "state-parallelism", 8, "how many -state-roots states are read at the same time (1 to 64)")
	fs.DurationVar(&o.timeout, "timeout", 0, "stop the whole run after this long, e.g. 30m (0: no limit)")
	fs.DurationVar(&o.src.sourceTimeout, "source-timeout", 0, "give up on one root, -scan or plugin after this long and go on with the others, e.g. 10m (0: no limit)")
	fs.Var((*sizeFlag)(&o.src.maxQuerySize), "max-query-size", "maximum bytes of one terraform query's output, read as it comes; accepts K, M or G suffixes")
	fs.Var((*sizeFlag)(&o.src.maxStateSize), "max-state-size", "maximum bytes per state, including state pull and refresh output; accepts K, M or G suffixes (powers of 1024)")
	fs.Var(&o.src.stateRoots, "state-roots", "directory tree whose root modules' states (all workspaces) count as managed, read with terraform state pull; repeatable")
	fs.BoolVar(&o.src.proxmoxStates, "proxmox-cluster-states", false, "state that every state given belongs to the Proxmox cluster scanned with -scan proxmox; without it Proxmox matches are uncertain, since state does not record the cluster")
	fs.BoolVar(&o.src.pluginStates, "plugin-cluster-states", false, "state that every state given belongs to the cluster or account the plugins scan; without it results a plugin marks \"unverified\" (state does not record where they live) match only as uncertain")
	fs.BoolVar(&o.src.allowHTTP, "allow-http", false, "allow -state URLs over plain http (never with credentials)")
	fs.BoolVar(&o.src.allowUnpinned, "allow-unpinned", false, "let -scan install the newest provider release when no root module pins its version")
	fs.BoolVar(&o.src.currentWS, "current-workspace", false, "read only the selected workspace of each root module instead of listing all workspaces")
	fs.BoolVar(&o.src.anyVersion, "ignore-required-version", false, "lift the root modules' required_version with a temporary override file, so that a root pinned to an older Terraform can be queried (child modules' constraints still apply)")
	fs.BoolVar(&o.src.refresh, "refresh", false, "read each root's state through terraform plan -refresh-only (no lock, nothing written) instead of state pull: state written before Terraform 1.12 gets resource identities, so matches are exact; slower, needs the root's credentials and variables")
	fs.BoolVar(&o.audit.strictAPI, "strict-api", true, "count listings that failed because the API is not enabled (GCP) as incomplete coverage (the default: a disabled API can still hold data, Cloud Storage and BigQuery keep theirs); -strict-api=false takes it as holding no objects, -strict-ignore TYPE_GLOB exempts named types")
	fs.Var(&strictIgnore, "strict-ignore", "types matching these globs do not make coverage incomplete when their listing fails or they are skipped for want of an argument (still listed when possible, and still reported), e.g. google_firebase_*")
	fs.Var(&expect, "expect", "scopes this run must check, e.g. \"account_id=111,222 region=eu-west-1\" (every combination) or project=a,b; one that no complete listing confirms makes coverage incomplete; keys are those of the JSON listings' scope (account_id, region, project, subscription_id, zone_id); repeatable")
	fs.IntVar(&o.src.maxRoots, "max-roots", 200, "stop when -state-roots finds more root modules, or an s3:// -state prefix more states, than this, before any is read (0: no limit)")
	fs.BoolVar(&o.src.terragrunt, "terragrunt", false, "with -state-roots, also read Terragrunt units (directories with a terragrunt.hcl, except parents that units include) through terragrunt in PATH; terragrunt initializes them (.terragrunt-cache)")
	fs.Var(&o.src.tgUnits, "terragrunt-unit", "with -terragrunt, read DIR as a Terragrunt unit even though units include its terragrunt.hcl (taken for shared configuration otherwise); repeatable")
	fs.BoolVar(&o.src.init, "init", false, "run terraform init in root modules that are not initialized (creates .terraform there)")
	fs.Var(&scans, "scan", "list a provider without list resources through its plural data sources: cloudflare, proxmox; credentials from the provider's environment variables (CLOUDFLARE_API_TOKEN, PROXMOX_VE_*), not from the root's provider block; repeatable")
	fs.Var(&scanScopes, "scan-scope", "limit -scan to some zones or accounts: zone_id=ID_OR_NAME,... or account_id=...; with no value (account_id=) that scope is not read; \"state\" limits every other scope to the IDs found in the states read; repeatable")
	fs.Var(&o.src.plugins, "plugin", "discovery command for providers without list support, run with sh -c; repeatable")
	fs.Var(&argFlags, "arg", "fan list queries out over values of a list config argument, e.g. region=eu-north-1,us-east-1; TYPE_GLOB:name=... limits it to matching types (google_compute_*:zone=europe-west1-b) and wins over the same name without one; zone=auto or region=auto (google) reads every zone or region through the root's provider; repeatable")
	fs.Var(&include, "types", "only query resource types matching these globs, e.g. aws_s3_*,aws_iam_role")
	fs.Var(&exclude, "exclude-types", "do not query resource types matching these globs")
	fs.IntVar(&o.src.limit, "limit", 100000, "maximum results per list query")
	if o.adopt != nil {
		fs.StringVar(&o.adopt.out, "o", "", "write the import blocks to this new file instead of stdout")
		fs.StringVar(&o.adopt.outDir, "out-dir", "", "write the import blocks to this new directory, one imports.tf per root module at its path from the current directory")
		fs.Var(&adoptRoots, "adopt-root", "with -out-dir: put what a -scan provider or plugin found into this root module's file, e.g. cloudflare=zones/example.com or plugin1=vault; without it they go to _unassigned/NAME.tf; repeatable")
		fs.BoolVar(&o.adopt.allowIncomplete, "allow-incomplete", false, "write import blocks even when a state could not be read (its objects would be imported a second time)")
		fs.BoolVar(&o.adopt.namesFromTags, "names-from-tags", false, "name the resources after their Name tag where the type has tags (slower: terraform query returns whole objects for those types)")
	} else {
		fs.StringVar(&o.format, "format", "text", "output format: text, json, or summary (counts per type)")
	}
	fs.StringVar(&o.audit.baseline, "baseline", "", "previous -format json report; show and fail only on new findings (not supported with adopt)")
	fs.StringVar(&o.audit.environment, "environment", "", "name of the environment this run checks, e.g. prod; stored in the JSON report, and a -baseline of another environment verifies nothing")
	fs.StringVar(&o.ignore.path, "ignore", "", "ignore file with TYPE_GLOB[:NAME_GLOB] [KEY=GLOB...] lines (default "+defaultIgnoreFile+" if present)")
	fs.Var(&presets, "ignore-preset", "add built-in ignore rules: "+strings.Join(report.Presets(), ", ")+"; repeatable ("+strings.Join(report.DefaultPresets, ", ")+" are applied by default)")
	fs.BoolVar(&o.ignore.noDefaults, "no-default-presets", false, "do not apply the default ignore presets ("+strings.Join(report.DefaultPresets, ", ")+"), which hide objects the cloud creates on its own")
	fs.Var(&attrs, "include-attrs", "fetch these resource attributes with the list results and print them in -format json, e.g. labels,creation_timestamp (slower: terraform query returns whole objects)")
	fs.BoolVar(&o.audit.strictOwnership, "strict-ownership", false, "exit 1 for confirmed duplicate state ownership or duplicate addresses within a state; possible ownership does not fail")
	fs.BoolVar(&o.audit.strict, "strict", false, "exit 1 when coverage is incomplete: failed or truncated queries, unread states, plugin errors, types skipped for want of an argument or a default provider configuration, or uncertain matches (state without identity, Proxmox, plugin results marked unverified)")
	fs.BoolVar(&o.src.noAliases, "no-aliases", false, "list resources only through each provider's default configuration, not through its aliases (provider \"aws\" { alias = \"prod\" }); the aliases' types then count as not checked")
	fs.BoolVar(&o.audit.explain, "explain", false, "say for every live object why it counts as managed, uncertain, ignored or unmanaged: the state address and where it was read, the ignore rule, or the closest state instances")
	fs.BoolVar(&o.verbose, "v", false, "list skipped resource types and every failed query, and the projects and accounts results came from")
	fs.BoolVar(&o.quiet, "quiet", false, "print no progress lines (by default printed on a terminal: roots read, roots queried, data source scans, -refresh)")
	fs.BoolVar(&o.src.printQuery, "print-query", false, "print the temporary files a run would write into each root (provider override, auto data sources, list blocks), each under a \"# file\" header, and exit")
	fs.BoolVar(&o.src.noQuery, "no-query", false, "do not run terraform query: read the roots' states, run -scan and plugins only")
	fs.StringVar(&o.src.terraformBin, "terraform", "terraform", "terraform binary")
	fs.StringVar(&o.configFile, "config", "", "read flags from this YAML file (keys are flag names, lists repeat a flag, roots lists root modules; the command line wins); default "+defaultConfigFile+" in the current directory, if present")
	noConfig := fs.Bool("no-config", false, "do not read "+defaultConfigFile)
	showVersion := fs.Bool("version", false, "print the version and exit")

	if o.doctor != nil {
		fs = doctorFlags(fs, o)
	}
	// Allow flags after positional arguments, until --.
	args := commandArgs(fs, argv)
	var flags []string
	for _, arg := range args {
		if !arg.positional && !arg.end {
			flags = append(flags, arg.parts()...)
		}
	}
	printHelp := func(w io.Writer) {
		fmt.Fprint(w, help)
		cliflag.PrintGroups(w, fs, flagGroups)
	}
	if err := cliflag.Parse(fs, flags, out, printHelp); err != nil {
		return nil, err
	}
	for _, arg := range args {
		if arg.positional {
			o.src.roots = append(o.src.roots, arg.raw)
		}
	}
	if o.doctor != nil {
		o.doctor.args = args
	}
	if *showVersion {
		fmt.Fprintln(out, "tforphans", buildVersion())
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
	if len(o.src.tgUnits) > 0 && !o.src.terragrunt {
		return nil, errors.New("-terragrunt-unit needs -terragrunt")
	}
	for _, list := range [][]string{o.src.roots, o.src.stateRoots, o.src.tgUnits} {
		for i, p := range list {
			if real, err := filepath.EvalSymlinks(p); err == nil {
				list[i] = real
			}
		}
	}
	if strings.ContainsAny(o.src.terraformBin, "/"+string(filepath.Separator)) && !filepath.IsAbs(o.src.terraformBin) {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		// Not Abs, which cleans: a link before .. must count as the
		// system resolves it.
		o.src.terraformBin = cwd + string(filepath.Separator) + o.src.terraformBin
	}

	// An upper bound too: a config file from the repository must not start
	// thousands of terraform processes at once.
	if o.src.stateParallelism < 1 || o.src.stateParallelism > 64 {
		return nil, errors.New("-state-parallelism must be between 1 and 64")
	}
	// 0 is the documented "no limit"; a negative value would disable the
	// limit too, without saying so.
	for _, n := range []struct {
		name string
		neg  bool
	}{
		{"max-roots", o.src.maxRoots < 0},
		{"timeout", o.timeout < 0},
		{"source-timeout", o.src.sourceTimeout < 0},
	} {
		if n.neg {
			return nil, fmt.Errorf("-%s must not be negative (0: no limit)", n.name)
		}
	}
	if o.adopt != nil && o.audit.baseline != "" {
		return nil, errors.New("-baseline is not supported with adopt")
	}
	if o.audit.explain && (o.adopt != nil || o.format == "summary") {
		return nil, errors.New("-explain needs the text or json report (not adopt or -format summary)")
	}
	if len(o.audit.environment) > 100 || report.Clean(o.audit.environment) != o.audit.environment {
		return nil, errors.New("-environment must be one line of at most 100 bytes without control characters or extra spaces")
	}
	if o.audit.baseline == "-" {
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
	argSeen := map[[2]string]bool{}
	for _, a := range argFlags {
		parsed, err := tf.ParseArg(a)
		if err != nil {
			return nil, err
		}
		i, ok := argIndex[parsed.Key()]
		if !ok {
			i = len(o.src.args)
			argIndex[parsed.Key()] = i
			o.src.args = append(o.src.args, tf.Arg{Types: parsed.Types, Name: parsed.Name})
		}
		for _, v := range parsed.Values {
			if !argSeen[[2]string{parsed.Key(), v}] {
				argSeen[[2]string{parsed.Key(), v}] = true
				o.src.args[i].Values = append(o.src.args[i].Values, v)
			}
		}
	}
	// -arg may come from the automatic config: bounded before the plan
	// multiplies the values.
	if err := tf.CheckArgCombinations(o.src.args); err != nil {
		return nil, err
	}
	o.ignore.presets = presets.csv()
	for _, p := range o.ignore.presets {
		if _, err := report.Preset(p); err != nil {
			return nil, err
		}
	}
	o.src.includeAttrs = attrs.csv()
	o.src.scans = scans.csv()
	for _, s := range o.src.scans {
		if _, ok := sources[s]; !ok {
			return nil, fmt.Errorf("unknown -scan %q", s)
		}
	}
	if o.adopt != nil {
		if err := adoptRootFlags(o, adoptRoots); err != nil {
			return nil, err
		}
	}
	for _, sc := range scanScopes {
		if sc == "state" {
			o.src.scopeState = true
			continue
		}
		arg, vals, ok := strings.Cut(sc, "=")
		known := false
		for _, name := range o.src.scans {
			for _, s := range sources[name].Scopes {
				known = known || s.Arg == arg
			}
		}
		if !ok || !known {
			return nil, fmt.Errorf("-scan-scope %q: want state or ARG=VALUE,... where ARG is a scope of the scanned providers (zone_id, account_id for cloudflare)", sc)
		}
		if o.src.scanScope == nil {
			o.src.scanScope = map[string][]string{}
		}
		// No values (account_id=) reads nothing of that scope.
		if _, ok := o.src.scanScope[arg]; !ok {
			o.src.scanScope[arg] = []string{}
		}
		for _, v := range strings.Split(vals, ",") {
			if v = strings.TrimSpace(v); v != "" && !slices.Contains(o.src.scanScope[arg], v) {
				o.src.scanScope[arg] = append(o.src.scanScope[arg], v)
			}
		}
	}
	o.src.include = glob.CompileList(include.csv())
	o.src.exclude = glob.CompileList(exclude.csv())
	o.audit.strictIgnore = glob.CompileList(strictIgnore.csv())
	expected, err := report.ParseExpect(expect)
	if err != nil {
		return nil, err
	}
	o.audit.expect = expected
	if len(o.src.roots) == 0 && !o.src.noQuery {
		if (len(o.src.scans) > 0 || len(o.src.plugins) > 0) && !tf.HasConfig(".") {
			// Nothing to query here: -scan and -plugin need no root.
			o.src.noQuery, o.src.impliedNoQuery = true, true
		} else if (len(o.src.stateRoots) > 0 || len(o.src.states) > 0 || len(o.src.tgUnits) > 0) && !tf.HasConfig(".") {
			// A directory without configuration is no root: its state
			// pull reads an empty local state, and a query fails.
			o.src.impliedNoRoot = true
		} else {
			o.src.roots = []string{"."}
			// checkRoots stops before the run: "." would read an empty
			// local state and list nothing.
			o.src.emptyDefaultRoot = len(o.src.scans) == 0 && len(o.src.plugins) == 0 && !tf.HasConfig(".")
		}
	}
	if n := len(slices.DeleteFunc(slices.Clone(o.src.states), func(s string) bool { return s != "-" })); n > 1 {
		return nil, errors.New("-state - (stdin) can be given only once")
	}
	if o.src.noQuery && len(o.src.plugins) == 0 && len(o.src.scans) == 0 {
		return nil, errors.New("-no-query needs at least one -scan or -plugin")
	}
	if o.adopt != nil {
		if err := outDirRoots(o); err != nil {
			return nil, err
		}
	}
	return o, nil
}
