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
	"slices"
	"sort"
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
	states        listFlag
	plugins       listFlag
	scans         []string
	args          []tf.Arg
	include       glob.List
	exclude       glob.List
	limit         int
	format        string
	adopt         bool   // "adopt" subcommand: write import blocks
	out           string // adopt: output file, empty for stdout
	ignorePath    string
	strict        bool
	verbose       bool
	printQuery    bool
	noQuery       bool
	allowUnpinned bool
	allowHTTP     bool
	proxmoxStates bool
	pluginStates  bool
	terraformBin  string
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
	var argFlags, include, exclude, scans listFlag
	fs.Var(&o.states, "state", "extra state that manages resources in the same accounts: file, http(s) URL (uses TF_HTTP_USERNAME/TF_HTTP_PASSWORD) or - for stdin; repeatable")
	fs.Var(&o.stateRoots, "state-roots", "directory tree whose root modules' states (all workspaces) count as managed, read with terraform state pull; repeatable")
	fs.BoolVar(&o.proxmoxStates, "proxmox-cluster-states", false, "state that every state given belongs to the Proxmox cluster scanned with -scan proxmox; without it Proxmox matches are unverified, since state does not record the cluster")
	fs.BoolVar(&o.pluginStates, "plugin-cluster-states", false, "state that every state given belongs to the cluster or account the plugins scan; without it results a plugin marks \"unverified\" (state does not record where they live) match only as unverified")
	fs.BoolVar(&o.allowHTTP, "allow-http", false, "allow -state URLs over plain http (never with credentials)")
	fs.BoolVar(&o.allowUnpinned, "allow-unpinned", false, "let -scan install the newest provider release when no root module pins its version")
	fs.BoolVar(&o.init, "init", false, "run terraform init in root modules that are not initialized (creates .terraform there)")
	fs.Var(&scans, "scan", "list a provider without list resources through its plural data sources: cloudflare, proxmox; credentials from the provider's environment variables (CLOUDFLARE_API_TOKEN, PROXMOX_VE_*); repeatable")
	fs.Var(&o.plugins, "plugin", "discovery command for providers without list support, run with sh -c; repeatable")
	fs.Var(&argFlags, "arg", "fan list queries out over values of a list config argument, e.g. region=eu-north-1,us-east-1; repeatable")
	fs.Var(&include, "types", "only query resource types matching these globs, e.g. aws_s3_*,aws_iam_role")
	fs.Var(&exclude, "exclude-types", "do not query resource types matching these globs")
	fs.IntVar(&o.limit, "limit", 100000, "maximum results per list query")
	if o.adopt {
		fs.StringVar(&o.out, "o", "", "write the import blocks to this new file instead of stdout")
	} else {
		fs.StringVar(&o.format, "format", "text", "output format: text or json")
	}
	fs.StringVar(&o.ignorePath, "ignore", "", "ignore file with TYPE_GLOB[:NAME_GLOB] lines (default "+defaultIgnoreFile+" if present)")
	fs.BoolVar(&o.strict, "strict", false, "exit 1 when coverage is incomplete: failed or truncated queries, unread states, plugin errors, or matches that could not be verified (state without identity, Proxmox, plugin results marked unverified)")
	fs.BoolVar(&o.verbose, "v", false, "list skipped resource types")
	fs.BoolVar(&o.printQuery, "print-query", false, "print the generated .tfquery.hcl for each root and exit")
	fs.BoolVar(&o.noQuery, "no-query", false, "do not run terraform query: read the roots' states and run plugins only")
	fs.StringVar(&o.terraformBin, "terraform", "terraform", "terraform binary")

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

	switch o.format {
	case "", "text", "json":
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
		i, ok := argIndex[parsed.Name]
		if !ok {
			i = len(o.args)
			argIndex[parsed.Name] = i
			o.args = append(o.args, tf.Arg{Name: parsed.Name})
		}
		for _, v := range parsed.Values {
			if !slices.Contains(o.args[i].Values, v) {
				o.args[i].Values = append(o.args[i].Values, v)
			}
		}
	}
	o.scans = scans.csv()
	for _, s := range o.scans {
		if _, ok := sources[s]; !ok {
			return nil, fmt.Errorf("unknown -scan %q", s)
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
	for _, name := range o.scans {
		a := sources[name]
		versionRoot := dsource.VersionRoot(slices.Concat(o.roots, stateDirs), a.Provider)
		res, err := dsource.Run(ctx, a, dsource.Options{
			TerraformBin:  o.terraformBin,
			VersionRoot:   versionRoot,
			Include:       o.include,
			Exclude:       o.exclude,
			AllowUnpinned: o.allowUnpinned,
		})
		if err != nil {
			return err
		}
		sum.Failed = append(sum.Failed, res.Failed...)
		sum.Queried += res.Listed
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
	live = dedupe(live)
	var orphans []model.Resource
	for _, r := range live {
		switch m := idx.Match(r); {
		case m.Managed:
			sum.Managed++
			if !m.Verified {
				sum.Unverified++
			}
		case ignore.Match(r):
			sum.Ignored++
		default:
			orphans = append(orphans, r)
		}
	}
	sort.SliceStable(orphans, func(i, j int) bool {
		if orphans[i].Type != orphans[j].Type {
			return orphans[i].Type < orphans[j].Type
		}
		return orphans[i].DisplayName < orphans[j].DisplayName
	})
	sum.Live, sum.Orphans = len(live), len(orphans)

	switch {
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
			reserved[im.To] = true
		}
		n := len(orphans)
		orphans = slices.DeleteFunc(orphans, func(r model.Resource) bool { return hasImport(r, imports) })
		if n > len(orphans) {
			fmt.Fprintf(stderr, "%d objects already have import blocks in the root modules; not written again.\n", n-len(orphans))
		}
		err = adopt(o, orphans, reserved, stdout, stderr)
	case o.format == "json":
		err = report.JSON(stdout, orphans, sum)
	default:
		err = report.Text(stdout, orphans)
	}
	if err != nil {
		return err
	}
	printSummary(stderr, o, sum)

	incomplete := len(sum.Failed) > 0 || len(sum.Warnings) > 0 || sum.Unverified > 0
	switch {
	case o.strict && incomplete:
		return exitCode(exitError)
	case len(orphans) > 0 && !o.adopt:
		return exitCode(exitOrphans)
	}
	return nil
}

// hasImport reports whether an import block of the configuration already
// targets r: same type and the same literal id or identity.
func hasImport(r model.Resource, imports []tf.Import) bool {
	for _, im := range imports {
		if !strings.HasPrefix(im.To, r.Type+".") {
			continue
		}
		if im.ID != "" && im.ID == r.ImportID {
			return true
		}
		if im.Identity != nil && r.ImportID == "" && identityEqual(r.Identity, im.Identity) {
			return true
		}
	}
	return false
}

func identityEqual(live map[string]any, literal map[string]string) bool {
	n := 0
	for k, v := range live {
		c, ok := model.Canonical(v)
		if !ok {
			continue
		}
		if literal[k] != c {
			return false
		}
		n++
	}
	return n > 0 && n == len(literal)
}

// adopt writes import blocks for the orphans to stdout or to a new file.
func adopt(o *options, orphans []model.Resource, reserved map[string]bool, stdout, stderr io.Writer) error {
	if len(orphans) == 0 {
		fmt.Fprintln(stderr, "Nothing to adopt.")
		return nil
	}
	if o.out == "" {
		return report.Import(stdout, orphans, reserved)
	}
	f, err := os.OpenFile(o.out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if err := report.Import(f, orphans, reserved); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "Wrote %d import blocks to %s. Next, in the root module:\n  terraform plan -generate-config-out=generated.tf\n", len(orphans), o.out)
	return nil
}

// scanRoot adds the root's state to idx and returns the live resources its
// providers list.
func scanRoot(ctx context.Context, o *options, root string, idx *state.Index, sum *report.Summary, stdout io.Writer) ([]model.Resource, error) {
	r := tf.Runner{Bin: o.terraformBin, Dir: root}
	if !r.Initialized() && o.init {
		if err := r.Init(ctx); err != nil {
			return nil, err
		}
	}
	if o.noQuery {
		// Only the state is needed: no provider schemas or query files.
		insts, n, err := pullRoot(ctx, o, root)
		if err != nil {
			return nil, err
		}
		idx.Add(insts)
		sum.States += n
		return nil, nil
	}
	schemas, err := r.ProvidersSchema(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w\n(is the module initialized? run terraform init)", err)
	}
	declared, err := tf.LocalNames(root)
	if err != nil {
		return nil, err
	}
	aliasOnly, err := tf.AliasOnly(root)
	if err != nil {
		return nil, err
	}
	plan := tf.BuildPlan(schemas, declared, tf.PlanOptions{Args: o.args, Include: o.include, Exclude: o.exclude, AliasOnly: aliasOnly})
	sum.Skipped = append(sum.Skipped, plan.Skipped...)
	for _, n := range plan.Notes {
		sum.Notes = append(sum.Notes, root+": "+n)
	}
	if o.printQuery {
		fmt.Fprintf(stdout, "# %s\n%s%s", root, plan.ProvidersHCL(), plan.HCL(o.limit))
		return nil, nil
	}

	insts, n, err := pullRoot(ctx, o, root)
	if err != nil {
		return nil, err
	}
	idx.Add(insts)
	sum.States += n

	// The user's own list blocks run too, even when nothing is generated.
	userLists, err := tf.UserLists(root)
	if err != nil {
		return nil, err
	}
	if o.noQuery || len(plan.Queries) == 0 && len(userLists) == 0 {
		return nil, nil
	}
	// Generated blocks are the plan's own addresses: a block of the user's
	// whose label happens to start with tfo_ is still the user's.
	byAddress := map[string]tf.Query{}
	for _, q := range plan.Queries {
		byAddress[q.Address()] = q
	}
	// Generated blocks use -limit; the user's blocks their own limit.
	limitFor := func(block string) int {
		if _, ok := byAddress[block]; block == "" || ok {
			return o.limit
		}
		return userLists[block].Limit
	}
	// Identity keys come from the schema of the provider that listed the
	// object (google and google-beta share types).
	idKeys := schemas.IdentityKeys()
	keysFor := func(block, typ string) ([]string, []string) {
		if q, ok := byAddress[block]; ok {
			return idKeys.For(q.Source, typ)
		}
		return idKeys.For(userListProvider(userLists[block].Provider, typ, declared, schemas), typ)
	}
	res, err := r.Query(ctx, plan, limitFor, keysFor)
	if err != nil {
		return nil, err
	}
	sum.Queried += len(plan.Queries) + len(userLists)
	sum.Failed = append(sum.Failed, res.Failed...)
	// Import blocks have to use the provider configuration the object was
	// listed with: ours for generated blocks, the block's own (possibly an
	// alias such as aws.other_account) for the user's list blocks.
	for i := range res.Found {
		f := &res.Found[i]
		block, _, _ := strings.Cut(f.ListAddress, "[")
		q, generated := byAddress[block]
		if !generated {
			// A block without a provider argument uses the type's default
			// provider, and so does an import block without one.
			f.Provider = userLists[block].Provider
			continue
		}
		switch {
		case q.Temporary:
			f.UndeclaredProvider = q.Source
		case !strings.HasPrefix(q.Type, q.Provider+"_") && q.Type != q.Provider:
			f.Provider = q.Provider
		}
	}
	return res.Found, nil
}

// userListProvider resolves the provider address of a user's list block
// from its provider argument ("aws", "aws.other"; empty means the type's
// prefix, as terraform infers it): the root's required_providers, else the
// implicit hashicorp provider. "" when it cannot be told.
func userListProvider(arg, typ string, declared map[string]string, schemas *tf.Schemas) string {
	local, _, _ := strings.Cut(arg, ".")
	if local == "" {
		local, _, _ = strings.Cut(typ, "_")
	}
	for addr, name := range declared {
		if name == local {
			return addr
		}
	}
	if addr := "registry.terraform.io/hashicorp/" + local; schemas.ProviderSchemas[addr].ResourceIdentitySchemas != nil {
		return addr
	}
	return ""
}

// pullRoot reads the state of every workspace of a root module and returns
// the instances and the number of states read.
func pullRoot(ctx context.Context, o *options, dir string) ([]state.Instance, int, error) {
	r := tf.Runner{Bin: o.terraformBin, Dir: dir}
	workspaces, err := r.Workspaces(ctx)
	if err != nil {
		return nil, 0, err
	}
	var all []state.Instance
	for _, ws := range workspaces {
		r.Workspace = ws
		raw, err := r.StatePull(ctx)
		if err != nil {
			return nil, 0, inWorkspace(ws, err)
		}
		insts, err := state.Parse(raw)
		if err != nil {
			return nil, 0, inWorkspace(ws, err)
		}
		all = append(all, insts...)
	}
	return all, len(workspaces), nil
}

func inWorkspace(ws string, err error) error {
	if ws == "" {
		return err
	}
	return fmt.Errorf("workspace %s: %w", ws, err)
}

// readStateRoots adds the states of all root modules found under
// -state-roots, except the roots that are scanned anyway, and returns the
// directories. A state that cannot be read is a warning: coverage is
// incomplete, not wrong.
func readStateRoots(ctx context.Context, o *options, idx *state.Index, sum *report.Summary) ([]string, error) {
	skip := map[string]bool{}
	for _, r := range o.roots {
		if abs, err := filepath.Abs(r); err == nil {
			skip[abs] = true
		}
	}
	var dirs []string
	for _, tree := range o.stateRoots {
		found, err := tf.FindRoots(tree)
		if err != nil {
			return nil, fmt.Errorf("-state-roots %s: %w", tree, err)
		}
		for _, d := range found {
			abs, err := filepath.Abs(d)
			if err != nil {
				return nil, err
			}
			if !skip[abs] {
				skip[abs] = true
				dirs = append(dirs, d)
			}
		}
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, d := range dirs {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			insts, n, err := pullStateRoot(ctx, o, d)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				sum.Warnings = append(sum.Warnings, fmt.Sprintf("state of %s not read, its resources may show as unmanaged: %v", d, err))
				return
			}
			idx.Add(insts)
			sum.States += n
		}()
	}
	wg.Wait()
	return dirs, ctx.Err()
}

func pullStateRoot(ctx context.Context, o *options, dir string) ([]state.Instance, int, error) {
	r := tf.Runner{Bin: o.terraformBin, Dir: dir}
	if !r.Initialized() {
		if !o.init {
			return nil, 0, errors.New("not initialized (run terraform init there, or pass -init)")
		}
		if err := r.Init(ctx); err != nil {
			return nil, 0, err
		}
	}
	return pullRoot(ctx, o, dir)
}

// dedupe drops resources listed more than once, for example by two roots
// that share a provider account.
func dedupe(rs []model.Resource) []model.Resource {
	seen := map[string]bool{}
	out := rs[:0]
	for _, r := range rs {
		k := r.Key()
		if !seen[k] {
			seen[k] = true
			out = append(out, r)
		}
	}
	return out
}

func printSummary(w io.Writer, o *options, s report.Summary) {
	fmt.Fprintf(w, "\n%d live, %d managed, %d ignored, %d unmanaged; %d states read, %d resource types queried, %d skipped",
		s.Live, s.Managed, s.Ignored, s.Orphans, s.States, s.Queried, len(s.Skipped))
	if len(s.Skipped) > 0 && !o.verbose {
		fmt.Fprint(w, " (-v to list)")
	}
	fmt.Fprintln(w)
	if s.Queried == 0 && len(o.plugins) == 0 && len(o.scans) == 0 && !o.printQuery {
		fmt.Fprintln(w, "warning: no resource types were queried, so nothing could be found")
	}
	if o.verbose {
		for _, sk := range s.Skipped {
			fmt.Fprintf(w, "  skipped %s: %s\n", report.Clean(sk.Type), report.Clean(sk.Reason))
		}
	}
	if len(s.Failed) > 5 && !o.verbose {
		var types []string
		for _, f := range s.Failed {
			if !slices.Contains(types, f.Type) {
				types = append(types, f.Type)
			}
		}
		fmt.Fprintf(w, "warning: %d resource types not fully listed (-v for reasons): %s\n", len(types), report.Clean(strings.Join(types, ", ")))
	} else {
		for _, f := range s.Failed {
			fmt.Fprintf(w, "warning: %s not fully listed: %s\n", report.Clean(f.Address), report.Clean(f.Message))
		}
	}
	for _, msg := range s.Warnings {
		fmt.Fprintln(w, "warning:", report.Clean(msg))
	}
	for _, msg := range s.Notes {
		fmt.Fprintln(w, "note:", report.Clean(msg))
	}
	if s.Unverified > 0 {
		fmt.Fprintf(w, "warning: %d objects matched state only by ID: their account, subscription or cluster could not be checked; with states of several accounts or clusters they may be unmanaged. State without resource identity: refresh it with a current provider (terraform apply -refresh-only) to record identities. Proxmox and plugin results marked unverified: state does not record the cluster; scan one cluster with its own states and pass -proxmox-cluster-states or -plugin-cluster-states.\n", s.Unverified)
	}
}
