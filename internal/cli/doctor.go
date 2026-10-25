package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/5xxeu/tforphans/internal/dsource"
	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/report"
	"github.com/5xxeu/tforphans/internal/state"
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
	o.doctor.configFlags = map[string]bool{}
	o.doctor.ignoreFlags = all
	all.VisitAll(func(f *flag.Flag) {
		if slices.Contains(allowed, f.Name) {
			fs.Var(f.Value, f.Name, f.Usage)
		} else {
			o.doctor.configFlags[f.Name] = true
		}
	})
	fs.Lookup("format").Usage = "output format: text or json"
	fs.BoolVar(&o.doctor.offline, "offline", false, "skip workspace, state and authentication reads; report unknown")
	fs.BoolVar(&o.doctor.checkAuth, "check-auth", false, "read supported providers' account identities (does not test resource read permissions)")
	o.doctor.flagSet = fs
	return fs
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
		o.run.steps = stderr
	}
	if o.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.timeout)
		defer cancel()
	}
	d := doctorReport{SchemaVersion: 1, Checks: []doctorCheck{}, verbose: o.verbose}
	d.add("config", "ok", "command line and config parsed", "", "")
	if o.src.impliedNoRoot {
		d.Notes = append(d.Notes, impliedNoRootNote)
	}
	if o.doctor.offline {
		d.add("offline", "warning", "installed provider processes may contact the network during schema startup; offline does not enforce network isolation", "", "")
	}
	if _, err := loadIgnore(ctx, o.ignore); err != nil {
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
	all := slices.Clone(o.src.roots)
	for _, dir := range dirs {
		if !slices.ContainsFunc(all, func(r string) bool { return sameDir(r, dir) }) {
			all = append(all, dir)
		}
	}
	if len(all) > 0 || len(o.src.stateRoots) > 0 || o.src.terragrunt || len(o.src.tgUnits) > 0 || len(o.src.scans) > 0 {
		d.version(ctx, o)
	} else {
		d.add("terraform.version", "skipped", "Terraform is not needed for explicit state sources and plugins without roots or scans", "", "")
	}
	listable := map[state.TypeKey]bool{}
	var checkedRoots []string
	for _, root := range all {
		o.stepf("checking %s", root)
		if d.root(ctx, o, root, slices.Contains(o.src.roots, root) && !o.src.noQuery, listable) {
			checkedRoots = append(checkedRoots, root)
		} else {
			d.add("states.read", "skipped", "temporary file cleanup failed", root, "")
		}
	}
	var snapshots []state.Snapshot
	if o.doctor.offline {
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
	idx := state.NewIndex()
	if o.src.scopeState {
		idx.AddSnapshots(snapshots)
	}
	for _, name := range o.src.scans {
		a := sources[name]
		pin := dsource.VersionRoot(checkedRoots, a.Provider)
		if pin == "" {
			d.add("scan."+name+".pin", "failed", "no root pins "+a.Provider+" with version and hashes", "", "Run terraform init and terraform providers lock in a root requiring "+a.Provider+", then pass it with -state-roots")
		} else {
			d.add("scan."+name+".pin", "ok", "provider version and hashes pinned", pin, "")
		}
		d.scanPlan(ctx, o, name, pin, scanScope(&o.src, a, idx))
		why := "-check-auth was not requested"
		if o.doctor.offline {
			why = "offline: authentication disabled"
		} else if a.Probe == "" {
			why = "provider has no supported identity probe"
		} else if pin == "" {
			why = "provider is not pinned"
		} else if o.doctor.checkAuth {
			_, err := withSource(ctx, o, func(ctx context.Context) (struct{}, error) {
				return struct{}{}, dsource.CheckAuth(ctx, a, dsource.Options{TerraformBin: o.src.terraformBin, VersionRoot: pin})
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
	d.discovery(o)
	if len(o.src.plugins) > 0 {
		d.add("plugins", "skipped", "plugins are not executed: the plugin protocol has no diagnostic mode", "", "")
	}
	if o.src.scopeState && !o.doctor.offline && len(snapshots) == 0 {
		d.add("scan.scope", "failed", "-scan-scope state needs at least one readable state", "", "Supply -state or -state-roots")
	}
	if ctx.Err() != nil {
		d.incomplete = true
		d.add("run", "failed", "diagnostics interrupted or overall timeout exceeded", "", "Increase -timeout or retry")
	}
	d.NextCommand = doctorNext(doctorConfiguredArgs(o, d.fixes), d.fixes)
	if o.doctor.offline {
		d.recheck = append(d.recheck, "-offline")
	}
	if o.doctor.checkAuth {
		d.recheck = append(d.recheck, "-check-auth")
	}
	code := d.result(o.doctor.offline)
	if err := d.write(report.WithColor(stdout, colorEnabled(stdout)), o.format); err != nil {
		fmt.Fprintln(stderr, "error:", report.Clean(err.Error()))
		return 1
	}
	return code
}

func doctorIgnoreKey(key string) bool {
	return key == "ignore" || key == "ignore-preset" || key == "no-default-presets"
}
