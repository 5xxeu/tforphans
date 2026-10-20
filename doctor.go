package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode"

	"github.com/5xxeu/tforphans/internal/dsource"
	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/report"
	"github.com/5xxeu/tforphans/internal/state"
	"github.com/5xxeu/tforphans/internal/tf"
	"github.com/hashicorp/hcl/v2/hclparse"
)

const doctorUsage = `Usage: tforphans doctor [flags] [ROOT_DIR...]

Check readiness in the selected audit scope without listing resources.
No terraform query, refresh or plugin runs. State pull confirms backend
access, not permission to read cloud resources. -check-auth reads identity
only and does not prove that a future scan will succeed.

Exit status: 0 ready (warnings allowed), 2 configuration or access obstacles,
1 diagnostics could not complete. Readiness does not mean no orphans.
-offline skips state and authentication reads and reports unknown, with exit
2 if local checks failed, otherwise 0. Schemas may still start installed
provider processes; use a network sandbox when network isolation is required.

Flags:
`

func doctorFlags(all *flag.FlagSet, o *options) *flag.FlagSet {
	fs := flag.NewFlagSet("tforphans doctor", flag.ContinueOnError)
	allowed := strings.Fields("config no-config terraform state state-roots current-workspace terragrunt terragrunt-unit types exclude-types arg no-aliases scan scan-scope no-query format v quiet timeout source-timeout max-state-size state-parallelism max-roots plugin")
	o.doctorConfigFlags = map[string]bool{}
	o.doctorIgnoreFlags = all
	all.VisitAll(func(f *flag.Flag) {
		if slices.Contains(allowed, f.Name) {
			fs.Var(f.Value, f.Name, f.Usage)
		} else {
			o.doctorConfigFlags[f.Name] = true
		}
	})
	fs.Lookup("format").Usage = "output format: text or json"
	fs.BoolVar(&o.offline, "offline", false, "skip workspace, state and authentication reads; report unknown")
	fs.BoolVar(&o.checkAuth, "check-auth", false, "read supported providers' account identities (does not test resource read permissions)")
	return fs
}

type doctorCheck struct {
	ID          string    `json:"id"`
	Status      string    `json:"status"`
	Message     string    `json:"message"`
	Remediation string    `json:"remediation,omitempty"`
	Root        string    `json:"root,omitempty"`
	Provider    string    `json:"provider,omitempty"`
	Types       []string  `json:"types,omitempty"`
	Read        *int      `json:"read,omitempty"`
	Total       *int      `json:"total,omitempty"`
	Count       *int      `json:"count,omitempty"`
	Skipped     []tf.Skip `json:"skipped_types,omitempty"`
}

type doctorReport struct {
	SchemaVersion int           `json:"schema_version"`
	Status        string        `json:"status"`
	Checks        []doctorCheck `json:"checks"`
	NextCommand   []string      `json:"next_command"`
	Notes         []string      `json:"notes,omitempty"`
	incomplete    bool
	fixes         []string
	queries       int
	providers     []string
	argFixes      []doctorArgFix
}

type doctorArgFix struct {
	check            int
	typ, name, value string
}

func (d *doctorReport) add(id, status, message, root, remediation string) {
	d.Checks = append(d.Checks, doctorCheck{ID: id, Status: status, Message: report.Clean(message), Root: report.Clean(root), Remediation: remediation})
}

func (d *doctorReport) result(offline bool) int {
	code := 0
	d.Status = "ready"
	for _, c := range d.Checks {
		if c.Status == "failed" {
			code, d.Status = 2, "needs_configuration"
		}
	}
	if offline {
		d.Status = "unknown"
	}
	if d.incomplete {
		code, d.Status = 1, "unknown"
	}
	return code
}

func runDoctor(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	o, err := parseFlags(argv, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", report.CleanLines(err.Error()))
		return 1
	}
	if o.format != "text" && o.format != "json" {
		fmt.Fprintln(stderr, "error: doctor -format must be text or json")
		return 1
	}
	if !o.quiet && isTerminal(stderr) {
		o.steps = stderr
	}
	if o.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.timeout)
		defer cancel()
	}
	d := doctorReport{SchemaVersion: 1, Checks: []doctorCheck{}}
	d.add("config", "ok", "command line and config parsed", "", "")
	if o.offline {
		d.add("offline", "warning", "installed provider processes may contact the network during schema startup; offline does not enforce network isolation", "", "")
	}
	if _, err := loadIgnore(ctx, o); err != nil {
		d.add("ignore", "failed", "ignore file or presets could not be parsed", "", "Fix the ignore file or presets in the config file")
	} else {
		d.add("ignore", "ok", "ignore file and presets parsed", "", "")
	}
	var sum report.Summary
	dirs, err := discoverStateRoots(o, &sum)
	if err != nil {
		d.add("roots", "failed", "root discovery failed: "+err.Error(), "", "Check -state-roots and -max-roots")
		d.incomplete = true
	}
	for _, u := range sum.UnreadStates {
		d.add("roots", "failed", u.Error, u.Root, "Check root configuration and directory permissions")
	}
	for _, n := range sum.Notes {
		d.add("roots", "warning", n, "", "")
	}
	all := slices.Clone(o.roots)
	for _, dir := range dirs {
		if !slices.ContainsFunc(all, func(r string) bool { return sameDir(r, dir) }) {
			all = append(all, dir)
		}
	}
	d.version(ctx, o)
	listable := map[state.TypeKey]bool{}
	var checkedRoots []string
	for _, root := range all {
		o.stepf("checking %s", root)
		if d.root(ctx, o, root, slices.Contains(o.roots, root) && !o.noQuery, listable) {
			checkedRoots = append(checkedRoots, root)
		} else {
			d.add("states.read", "skipped", "temporary file cleanup failed", root, "")
		}
	}
	d.discovery(o)
	var snapshots []state.Snapshot
	if o.offline {
		d.add("states.read", "skipped", "offline: workspace list and state pull disabled", "", "")
		d.add("states.identity", "skipped", "offline: no states read", "", "")
	} else {
		if err != nil {
			d.add("states.read", "skipped", "root discovery failed; root state reads cannot run safely", "", "")
			snapshots = d.states(ctx, o, nil)
		} else {
			snapshots = d.states(ctx, o, checkedRoots)
		}
		n := 0
		for _, s := range snapshots {
			for _, in := range s.Instances {
				if len(in.Identity) == 0 && (listable[state.TypeKey{Type: in.Type, Provider: model.ProviderFamily(in.Provider)}] || in.Provider == "" && listable[state.TypeKey{Type: in.Type}]) {
					n++
				}
			}
		}
		status := "ok"
		if n > 0 {
			status = "warning"
		}
		if len(listable) == 0 {
			status = "skipped"
		}
		message := fmt.Sprintf("%d instances of list-capable types without identity; attribute matching remains available", n)
		if status == "skipped" {
			message = "no list-capable schemas available for identity counting"
		}
		d.add("states.identity", status, message, "", "")
		d.Checks[len(d.Checks)-1].Count = &n
	}
	if len(snapshots) > 0 {
		d.Notes = append(d.Notes, "State pull confirms backend access only, not permission to read cloud resources.")
	}
	for _, name := range o.scans {
		a := sources[name]
		pin := dsource.VersionRoot(checkedRoots, a.Provider)
		if pin == "" {
			d.add("scan."+name+".pin", "failed", "no root pins "+a.Provider+" with version and hashes", "", "Run terraform init and terraform providers lock in a root requiring "+a.Provider+", then pass it with -state-roots")
		} else {
			d.add("scan."+name+".pin", "ok", "provider version and hashes pinned", pin, "")
		}
		why := "-check-auth was not requested"
		if o.offline {
			why = "offline: authentication disabled"
		} else if a.Probe == "" {
			why = "provider has no supported identity probe"
		} else if pin == "" {
			why = "provider is not pinned"
		} else if o.checkAuth {
			_, err := withSource(ctx, o, func(ctx context.Context) (struct{}, error) {
				return struct{}{}, dsource.CheckAuth(ctx, a, dsource.Options{TerraformBin: o.terraformBin, VersionRoot: pin})
			})
			if err != nil {
				d.add("auth."+name, "failed", "identity probe failed", pin, "Check provider credentials and connectivity")
			} else {
				d.add("auth."+name, "ok", "identity probe succeeded", pin, "")
			}
			continue
		}
		d.add("auth."+name, "skipped", why, "", "")
	}
	if len(o.plugins) > 0 {
		d.add("plugins", "skipped", "plugins are not executed: the plugin protocol has no diagnostic mode", "", "")
	}
	if o.scopeState && !o.offline && len(snapshots) == 0 {
		d.add("scan.scope", "failed", "-scan-scope state needs at least one readable state", "", "Supply -state or -state-roots")
	}
	if ctx.Err() != nil {
		d.incomplete = true
		d.add("run", "failed", "diagnostics interrupted or overall timeout exceeded", "", "Increase -timeout or retry")
	}
	d.NextCommand = doctorNext(argv[1:], d.fixes)
	code := d.result(o.offline)
	if err := d.write(stdout, o.format); err != nil {
		fmt.Fprintln(stderr, "error:", report.Clean(err.Error()))
		return 1
	}
	return code
}

func (d *doctorReport) version(ctx context.Context, o *options) {
	type version struct {
		Version string `json:"terraform_version"`
	}
	v, err := withSource(ctx, o, func(ctx context.Context) (version, error) {
		out, _, err := (tf.Runner{Bin: o.terraformBin, Dir: "."}).Exec(ctx, "version", "-json")
		var v version
		if err == nil {
			err = json.Unmarshal(out, &v)
			if err == nil && v.Version == "" {
				err = errors.New("missing terraform_version")
			}
		}
		return v, err
	})
	if err != nil {
		d.add("terraform.version", "failed", "could not read Terraform version JSON", "", "Check -terraform and retry")
		d.incomplete = true
		return
	}
	if !o.noQuery {
		if err := tf.CheckQueryVersion(o.terraformBin, v.Version); err != nil {
			d.add("terraform.version", "failed", err.Error(), "", "Install Terraform 1.14 or newer")
			return
		}
	}
	d.add("terraform.version", "ok", v.Version, "", "")
}

func (d *doctorReport) root(ctx context.Context, o *options, root string, query bool, listable map[state.TypeKey]bool) bool {
	removed, err := tf.RemoveStaleCount(root)
	if removed > 0 {
		d.add("root.stale", "warning", fmt.Sprintf("removed %d temporary files of dead tforphans processes", removed), root, "")
		d.Checks[len(d.Checks)-1].Count = &removed
	}
	if err != nil {
		d.add("root.stale", "failed", report.NewUnreadState(root, err).Error, root, "Remove zz_tforphans_* temporary files manually in "+shellQuote(root)+" after verifying no tforphans process is using them")
		return false
	}

	if o.terragrunt && !query {
		kind, err := tf.TerragruntKind(root)
		if err == nil && kind != tf.TerragruntNone {
			d.add("root.config", "ok", "Terragrunt unit configuration parsed", root, "")
			d.add("root.initialized", "skipped", "Terragrunt manages unit initialization", root, "")
			d.add("root.lock", "skipped", "Terragrunt selects the rendered root's lock file", root, "")
			d.add("root.query", "skipped", "used only for state", root, "")
			return true
		}
	}

	r := tf.Runner{Bin: o.terraformBin, Dir: root, MaxStateSize: o.maxStateSize}
	init := "terraform -chdir=" + shellQuote(root) + " init"
	declared, err := tf.LocalNames(root)
	if query {
		d.providers = append(d.providers, slices.Collect(maps.Keys(declared))...)
	}
	if err != nil || !tf.HasConfig(root) {
		d.add("root.config", "failed", "root configuration is missing or invalid", root, "Check the root's Terraform configuration")
	} else {
		d.add("root.config", "ok", "root configuration parsed", root, "")
	}
	if !r.Initialized() {
		d.add("root.initialized", "failed", "Terraform data directory is missing: "+tf.DataDir(root), root, init)
	} else {
		d.add("root.initialized", "ok", "Terraform data directory exists", root, "")
	}
	lockPath := filepath.Join(root, ".terraform.lock.hcl")
	if raw, err := os.ReadFile(lockPath); err == nil {
		_, diags := hclparse.NewParser().ParseHCL(raw, lockPath)
		if diags.HasErrors() {
			d.add("root.lock", "failed", "provider lock file is invalid", root, "terraform -chdir="+shellQuote(root)+" providers lock")
		} else {
			d.add("root.lock", "ok", "provider lock file parsed", root, "")
		}
	} else if os.IsNotExist(err) {
		d.add("root.lock", "warning", "no provider lock file", root, "terraform -chdir="+shellQuote(root)+" providers lock")
	} else {
		d.add("root.lock", "failed", "provider lock file is unreadable", root, "Check lock file permissions")
	}
	if !query {
		d.add("root.query", "skipped", "used only for state", root, "")
		return true
	}
	if o.offline && !r.Initialized() {
		d.add("providers.schema", "skipped", "offline: root is not initialized", root, init)
		d.skipPlan(root, "provider schema unavailable")
		d.add("auth.providers", "skipped", "offline: authentication disabled", root, "")
		return true
	}
	schemas, err := withSource(ctx, o, r.ProvidersSchema)
	if err != nil {
		d.add("providers.schema", "failed", "provider schema could not be read", root, init)
		if doctorResponseError(err) {
			d.incomplete = true
		}
		d.skipPlan(root, "provider schema unavailable")
		d.add("auth.providers", "skipped", "provider schema unavailable", root, "")
		return true
	}
	if schemas.ProviderSchemas == nil {
		d.add("providers.schema", "failed", "schema JSON has no provider_schemas object", root, "")
		d.incomplete = true
		d.skipPlan(root, "provider schema unavailable")
		d.add("auth.providers", "skipped", "provider schema unavailable", root, "")
		return true
	}
	if len(schemas.ProviderSchemas) == 0 {
		d.add("providers.schema", "ok", "no providers in this root", root, "")
	}
	for _, addr := range slices.Sorted(maps.Keys(schemas.ProviderSchemas)) {
		d.providers = append(d.providers, addr)
		ps := schemas.ProviderSchemas[addr]
		types := slices.Sorted(maps.Keys(ps.ListResourceSchemas))
		d.add("providers.schema", "ok", fmt.Sprintf("%s: %d list resource types", addr, len(types)), root, "")
		c := &d.Checks[len(d.Checks)-1]
		c.Provider, c.Types = addr, types
		for _, t := range types {
			listable[state.TypeKey{Type: t, Provider: model.ProviderFamily(addr)}] = true
			listable[state.TypeKey{Type: t}] = true
		}
	}
	sc, err := tf.ProviderSourceConfigs(root)
	if err != nil {
		d.add("query.plan", "failed", "provider configuration could not be parsed", root, "")
		return true
	}
	pc, err := tf.ProviderConfigs(root)
	if err != nil {
		d.add("query.plan", "failed", "provider configuration could not be parsed", root, "")
		return true
	}
	p := tf.BuildPlan(schemas, declared, tf.PlanOptions{Args: o.args, Include: o.include, Exclude: o.exclude, Configs: sc, NoAliases: o.noAliases, ProviderConfigs: pc})
	userLists, err := tf.UserLists(root)
	if err != nil {
		d.add("query.plan", "failed", "user list blocks could not be parsed", root, "Check .tfquery.hcl files")
	}
	d.queries += len(p.Queries) + len(userLists)
	d.add("query.plan", "ok", fmt.Sprintf("%d generated listings and %d user list blocks selected; no listings executed", len(p.Queries), len(userLists)), root, "")
	d.Checks[len(d.Checks)-1].Skipped = p.Skipped
	for _, n := range p.Notes {
		d.add("query.plan", "warning", n, root, "")
	}
	for _, addr := range p.NoList {
		d.add("query.plan", "warning", addr+" has no list resources", root, "Use a supported -scan or -plugin")
	}
	for _, q := range p.Queries {
		if q.Temporary {
			d.add("query.plan", "warning", q.Source+" is declared only in child modules; listing uses an empty root configuration", root, "")
			break
		}
	}
	argsStatus, aliasStatus := "ok", "ok"
	for _, sk := range p.Skipped {
		id, status, fix := "query.plan", "failed", ""
		if sk.Excluded {
			status = "skipped"
		} else if strings.HasPrefix(sk.Reason, tf.MissingArgs) {
			id, status, argsStatus = "query.arguments", "failed", "failed"
			missing := strings.Split(strings.TrimPrefix(sk.Reason, tf.MissingArgs), " (provider ")[0]
			for _, name := range strings.Split(missing, ", ") {
				if !strings.Contains(name, " ") {
					value := "..."
					source := userListProvider(sk.Provider, sk.Type, declared, schemas)
					if source == "" {
						for addr, ps := range schemas.ProviderSchemas {
							if _, ok := ps.ListResourceSchemas[sk.Type]; !ok {
								continue
							}
							if source != "" && model.ProviderFamily(addr) != model.ProviderFamily(source) {
								source = ""
								break
							}
							source = addr
						}
					}
					if doctorAutoArg(source, name) {
						value = "auto"
					}
					d.argFixes = append(d.argFixes, doctorArgFix{check: len(d.Checks), typ: sk.Type, name: name, value: value})
				}
			}
			fix = "Write a user list block for " + sk.Type + " supplying " + missing
		} else if strings.Contains(sk.Reason, "provider") {
			id, aliasStatus = "query.aliases", "warning"
			if o.noAliases && strings.HasSuffix(sk.Reason, " not listed (-no-aliases)") {
				status = "warning"
			} else {
				status = "failed"
				aliasStatus = "failed"
			}
			fix = "Review provider configurations and -no-aliases"
		}
		d.add(id, status, sk.Type+": "+sk.Reason, root, fix)
	}
	d.updateArgumentFixes()
	if argsStatus == "ok" {
		d.add("query.arguments", "ok", "selected generated listings have their required arguments", root, "")
	}
	if aliasStatus == "ok" {
		d.add("query.aliases", "ok", "selected provider configurations can be planned", root, "")
	}
	configs := map[string]tf.IdentityConfig{}
	for addr, c := range sc {
		local := declared[addr]
		if local == "" {
			continue
		}
		if c.Plain || len(c.Aliases) == 0 {
			configs[local] = tf.IdentityConfig{Ref: local, Source: addr}
		}
		if !o.noAliases {
			for _, a := range c.Aliases {
				configs[a.Ref()] = tf.IdentityConfig{Ref: a.Ref(), Source: addr}
			}
		}
	}
	for _, q := range p.Queries {
		configs[q.Ref()] = tf.IdentityConfig{Ref: q.Ref(), Source: q.Source}
	}
	for _, ref := range slices.Sorted(maps.Keys(configs)) {
		c := configs[ref]
		id := "auth." + ref
		why := "-check-auth was not requested"
		if o.offline {
			why = "offline: authentication disabled"
		} else if len(tf.IdentitySources[model.ProviderFamily(c.Source)]) == 0 {
			why = "provider has no supported identity reader"
		} else if o.checkAuth {
			type identities struct {
				scopes map[string]map[string]string
				failed map[string]error
			}
			result, err := withSource(ctx, o, func(ctx context.Context) (identities, error) {
				s, f, e := r.Identities(ctx, p, []tf.IdentityConfig{c})
				return identities{s, f}, e
			})
			if err != nil || len(result.failed) > 0 {
				malformed := doctorResponseError(err)
				for _, failure := range result.failed {
					malformed = malformed || doctorResponseError(failure)
				}
				if malformed {
					d.incomplete = true
					d.add(id, "failed", "could not decode identity plan", root, "Check Terraform and provider output, then retry")
				} else {
					d.add(id, "failed", "identity read failed", root, "Check provider credentials and connectivity")
				}
			} else {
				values := []string{}
				for _, k := range slices.Sorted(maps.Keys(result.scopes[ref])) {
					values = append(values, k+"="+result.scopes[ref][k])
				}
				d.add(id, "ok", strings.Join(values, ", "), root, "")
			}
			continue
		}
		d.add(id, "skipped", why, root, "")
	}
	if len(configs) == 0 {
		d.add("auth.providers", "skipped", "no provider configurations selected", root, "")
	}
	return true
}

func (d *doctorReport) states(ctx context.Context, o *options, roots []string) []state.Snapshot {
	type read struct {
		snapshots []state.Snapshot
		n, total  int
		err       error
	}
	results := make([]read, len(roots)+len(o.states))
	var wg sync.WaitGroup
	sem := make(chan struct{}, o.stateParallelism)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			v, err := withSource(ctx, o, func(ctx context.Context) (read, error) {
				v := read{total: 1}
				if i < len(roots) {
					root := roots[i]
					if slices.Contains(o.roots, root) {
						v.snapshots, v.n, v.err = pullRootCount(ctx, o, root, &v.total)
					} else {
						v.snapshots, v.n, v.err = pullStateRootCount(ctx, o, root, &v.total)
					}
					return v, v.err
				}
				raw, err := state.Fetch(ctx, o.states[i-len(roots)], false, o.maxStateSize)
				if err != nil {
					return v, err
				}
				snapshot, err := state.ParseSnapshot(raw, state.Redact(o.states[i-len(roots)]))
				if err == nil {
					v.n = 1
					v.snapshots = []state.Snapshot{snapshot}
				}
				return v, err
			})
			v.err = err
			results[i] = v
		}()
	}
	wg.Wait()
	var all []state.Snapshot
	n, total := 0, 0
	for i, v := range results {
		root := ""
		if i < len(roots) {
			root = roots[i]
		} else {
			root = state.Redact(o.states[i-len(roots)])
		}
		n += v.n
		total += v.total
		all = append(all, v.snapshots...)
		if v.err != nil {
			unread := report.NewUnreadState(root, v.err)
			d.add("states.read", "failed", unread.Error, unread.Root, doctorStateRemediation(root, v.err, o))
			if doctorResponseError(v.err) {
				d.incomplete = true
			}
		}
	}
	status := "ok"
	for _, v := range results {
		if v.err != nil {
			status = "failed"
		}
	}
	d.add("states.read", status, fmt.Sprintf("%d of %d states read", n, total), "", "")
	d.Checks[len(d.Checks)-1].Read, d.Checks[len(d.Checks)-1].Total = &n, &total
	return all
}

func doctorNext(args, fixes []string) []string {
	out := []string{"tforphans"}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name := strings.TrimLeft(strings.SplitN(arg, "=", 2)[0], "-")
		if strings.HasPrefix(arg, "-") && (name == "offline" || name == "check-auth" || name == "format") {
			if name == "format" && !strings.Contains(arg, "=") {
				i++
			}
			continue
		}
		if strings.HasPrefix(arg, "-") && !strings.Contains(arg, "=") && i+1 < len(args) {
			if name == "arg" {
				out = append(out, arg, doctorArg(args[i+1]))
				i++
				continue
			}
			if name == "plugin" {
				out = append(out, arg, "<plugin command>")
				i++
				continue
			}
		}
		if name == "arg" && strings.Contains(arg, "=") {
			out = append(out, strings.SplitN(arg, "=", 2)[0]+"="+doctorArg(strings.SplitN(arg, "=", 2)[1]))
			continue
		}
		if name == "plugin" && strings.Contains(arg, "=") {
			arg = strings.SplitN(arg, "=", 2)[0] + "=<plugin command>"
		}
		if strings.HasPrefix(arg, "-") && strings.Contains(arg, "=") {
			k, v, _ := strings.Cut(arg, "=")
			arg = k + "=" + state.Redact(v)
		} else {
			arg = state.Redact(arg)
		}
		out = append(out, arg)
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(fixes); i += 2 {
		if !seen[fixes[i+1]] {
			out = append(out, fixes[i], fixes[i+1])
			seen[fixes[i+1]] = true
		}
	}
	for i, arg := range out {
		out[i] = doctorURLUserinfo.ReplaceAllString(arg, "${1}")
	}
	return out
}

func doctorArg(arg string) string {
	parsed, err := tf.ParseArg(arg)
	if err != nil {
		return "<redacted>"
	}
	key, _, _ := strings.Cut(arg, "=")
	name := parsed.Name
	camelKey := strings.HasSuffix(name, "Key")
	name = strings.ToLower(name)
	words := strings.FieldsFunc(name, func(r rune) bool { return r == '_' || r == '-' || r == '.' })
	secret := strings.Contains(name, "token") || strings.Contains(name, "secret") || strings.Contains(name, "password") || strings.Contains(name, "credential")
	if secret || camelKey || name == "key" || len(words) > 0 && words[len(words)-1] == "key" {
		return key + "=<redacted>"
	}
	if strings.Contains(arg, "://") {
		values := slices.Clone(parsed.Values)
		for i, value := range values {
			value = doctorURLUserinfo.ReplaceAllString(value, "${1}")
			values[i] = doctorURLs.ReplaceAllStringFunc(value, state.Redact)
		}
		return doctorURLUserinfo.ReplaceAllString(key+"="+strings.Join(values, ","), "${1}")
	}
	return arg
}

func shellArg(s string) string {
	if s != "" && !strings.ContainsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_@%+=:,./-", r))
	}) {
		return s
	}
	return shellQuote(s)
}

func shellQuote(s string) string {
	if strings.Contains(s, "  ") {
		parts := strings.Split(s, " ")
		for i, part := range parts {
			parts[i] = "'" + strings.ReplaceAll(part, "'", "'\"'\"'") + "'"
		}
		return strings.Join(parts, "' '")
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func (d doctorReport) write(w io.Writer, format string) error {
	d.NextCommand = slices.Clone(d.NextCommand)
	unsafe := false
	for i, arg := range d.NextCommand {
		arg = doctorURLUserinfo.ReplaceAllString(arg, "${1}")
		d.NextCommand[i] = arg
		if strings.ContainsFunc(arg, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) {
			d.NextCommand[i] = "<argument with control characters>"
			unsafe = true
		}
	}
	if unsafe {
		d.Notes = append(slices.Clone(d.Notes), "Next run contains a placeholder for an argument with control characters; supply it manually.")
	}

	if format == "json" {
		return json.NewEncoder(w).Encode(d)
	}
	labels := map[string]string{"ok": "OK", "warning": "WARN", "failed": "FAIL", "skipped": "SKIP"}
	for _, c := range d.Checks {
		root := ""
		if c.Root != "" {
			root = " [" + c.Root + "]"
		}
		if _, err := fmt.Fprintf(w, "%-5s %-23s %s%s\n", labels[c.Status], c.ID, c.Message, root); err != nil {
			return err
		}
		if c.Remediation != "" {
			if _, err := fmt.Fprintln(w, "      Fix:", report.Clean(c.Remediation)); err != nil {
				return err
			}
		}
	}
	if _, err := fmt.Fprintln(w, "Result:", strings.ReplaceAll(d.Status, "_", " ")); err != nil {
		return err
	}
	for _, note := range d.Notes {
		if _, err := fmt.Fprintln(w, report.Clean(note)); err != nil {
			return err
		}
	}
	parts := make([]string, len(d.NextCommand))
	for i, a := range d.NextCommand {
		parts[i] = shellArg(a)
	}
	_, err := fmt.Fprintln(w, "Next run:\n  "+report.Clean(strings.Join(parts, " ")))
	return err
}

func (d *doctorReport) skipPlan(root, reason string) {
	for _, id := range []string{"query.plan", "query.arguments", "query.aliases"} {
		d.add(id, "skipped", reason, root, "")
	}
}

func doctorIgnoreKey(key string) bool {
	return key == "ignore" || key == "ignore-preset" || key == "no-default-presets"
}

func doctorStateRemediation(root string, err error, o *options) string {
	var size *state.TooLargeError
	if errors.As(err, &size) {
		return "Increase -max-state-size above " + fmt.Sprint(size.Max) + " bytes"
	}
	var timeout sourceTimeout
	if errors.As(err, &timeout) || errors.Is(err, context.DeadlineExceeded) {
		return "Increase -source-timeout or check backend connectivity"
	}
	message := strings.ToLower(err.Error())
	for _, text := range []string{"module source has changed", "backend initialization required", "required plugins are not installed", "please run terraform init", "run terraform init", "not initialized", "initialization required", "initialization is required", "module not installed", `run "terraform init"`, "run 'terraform init'"} {
		if strings.Contains(message, text) {
			if o.terragrunt {
				if kind, e := tf.TerragruntKind(root); e == nil && kind != tf.TerragruntNone {
					return "cd " + shellQuote(root) + " && terragrunt run -- init"
				}
			}
			return "terraform -chdir=" + shellQuote(root) + " init"
		}
	}
	for _, text := range []string{"403", "401", "accessdenied", "access denied", "unauthorized", "forbidden"} {
		if strings.Contains(message, text) {
			return "Check backend credentials and read permissions for " + root
		}
	}
	return "Check backend credentials, state source and Terraform initialization"
}

func doctorResponseError(err error) bool {
	var response *model.ResponseError
	return errors.As(err, &response)
}

func doctorAutoArg(source, name string) bool {
	family := model.ProviderFamily(source)
	_, ok := tf.AutoSources[family[strings.LastIndex(family, "/")+1:]]
	return ok && (name == "region" || name == "zone")
}

func (d *doctorReport) discovery(o *options) {
	queries := d.queries
	if o.noQuery {
		queries = 0
	}
	count := queries + len(o.scans) + len(o.plugins)
	status, fix := "ok", ""
	if !hasDiscovery(queries+len(o.scans), len(o.plugins)) {
		status = "failed"
		fix = "Use -plugin COMMAND or a root with list resources; review -types and -exclude-types"
		for _, name := range slices.Sorted(maps.Keys(sources)) {
			if slices.ContainsFunc(d.providers, func(addr string) bool { return tf.NormalizeSource(addr) == tf.NormalizeSource(sources[name].Provider) }) {
				fix = "tforphans -scan " + name
				d.fixes = append(d.fixes, "-scan", name)
				break
			}
		}
	}
	d.add("discovery", status, fmt.Sprintf("%d list queries, %d scans, %d plugins selected", queries, len(o.scans), len(o.plugins)), "", fix)
	d.Checks[len(d.Checks)-1].Count = &count
}

var (
	doctorURLs        = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^,\s"'<>]+`)
	doctorURLUserinfo = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s]*@`)
)

func (d *doctorReport) updateArgumentFixes() {
	values := map[string]map[string]bool{}
	for _, fix := range d.argFixes {
		if values[fix.name] == nil {
			values[fix.name] = map[string]bool{}
		}
		values[fix.name][fix.value] = true
	}
	kept := []string{}
	for i := 0; i+1 < len(d.fixes); i += 2 {
		if d.fixes[i] != "-arg" {
			kept = append(kept, d.fixes[i:i+2]...)
		}
	}
	d.fixes = kept
	byCheck := map[int][]string{}
	seen := map[string]bool{}
	for _, fix := range d.argFixes {
		name := fix.name
		if len(values[name]) > 1 {
			name = fix.typ + ":" + name
		}
		arg := name + "=" + fix.value
		if !slices.Contains(byCheck[fix.check], arg) {
			byCheck[fix.check] = append(byCheck[fix.check], arg)
		}
		if !seen[arg] {
			d.fixes = append(d.fixes, "-arg", arg)
			seen[arg] = true
		}
	}
	for check, args := range byCheck {
		d.Checks[check].Remediation = "tforphans -arg " + strings.Join(args, " -arg ")
	}
}
