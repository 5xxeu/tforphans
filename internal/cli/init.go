package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	iofs "io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/5xxeu/tforphans/internal/cliflag"
	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/report"
	"github.com/5xxeu/tforphans/internal/tf"
	"go.yaml.in/yaml/v3"
)

const initUsage = `Usage: tforphans init [flags] [DIR...]

Discover Terraform root modules under DIR (default ".") and create
.tforphans.yaml in the current directory. Directories must stay inside it.
Reads local configuration and lock files only; Terraform, providers and
plugins are not started. Run tforphans doctor to check readiness afterwards.

Nested roots are recognized by backend/cloud blocks or local state. A DIR
with Terraform configuration is also selected explicitly, even before its
first terraform init. Hidden directories, vendor and testdata are skipped.

An existing file is left alone unless -update is given. Updating replaces
roots and state-roots, preserving other settings and comments. Without DIR,
-update reuses state-roots (or roots) from the existing file. -scan sets
that key only when explicitly supplied; it uses credentials from the
environment when the subsequent audit runs, not the root's provider block.

Exit status: 0 config prepared, 1 invalid input, incomplete discovery or I/O error.

Flags:
`

type initOptions struct {
	update, dryRun bool
	maxRoots       int
	maxRootsSet    bool
	scans          listFlag
	dirs           []string
}

func runInit(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tforphans init", flag.ContinueOnError)
	var o initOptions
	fs.BoolVar(&o.update, "update", false, "refresh roots under the previous search directories, preserving other settings")
	fs.BoolVar(&o.dryRun, "dry-run", false, "show discovery and the proposed config without writing it")
	fs.IntVar(&o.maxRoots, "max-roots", 200, "maximum discovered root modules (0: no limit); saved in the config when given")
	fs.Var(&o.scans, "scan", "explicitly enable an adapter in the saved config: cloudflare, proxmox; repeatable")
	args := commandArgs(fs, argv)
	var flags []string
	for _, arg := range args {
		if arg.positional {
			o.dirs = append(o.dirs, arg.raw)
		} else if !arg.end {
			flags = append(flags, arg.parts()...)
		}
	}
	help := func(w io.Writer) {
		fmt.Fprint(w, initUsage)
		fs.SetOutput(w)
		fs.PrintDefaults()
	}
	if err := cliflag.Parse(fs, flags, stdout, help); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		usageError(stderr, err, "tforphans init")
		return exitError
	}
	fs.Visit(func(f *flag.Flag) { o.maxRootsSet = o.maxRootsSet || f.Name == "max-roots" })
	if err := prepareInit(ctx, o, report.WithColor(stdout, colorEnabled(stdout))); err != nil {
		fmt.Fprintln(stderr, "error:", report.CleanLines(err.Error()))
		return exitError
	}
	return exitOK
}

func prepareInit(ctx context.Context, o initOptions, w io.Writer) error {
	if o.maxRoots < 0 {
		return errors.New("-max-roots must be non-negative")
	}
	var saveMaxRoots *int
	if o.maxRootsSet {
		saveMaxRoots = &o.maxRoots
	}
	scans := o.scans.csv()
	for _, name := range scans {
		if _, ok := sources[name]; !ok {
			return fmt.Errorf("unknown -scan %q", name)
		}
	}
	scans = slices.Sorted(slices.Values(scans))
	scans = slices.Compact(scans)
	old, info, err := initExisting(o.update)
	if err != nil {
		return err
	}
	if len(old) > 0 && !o.maxRootsSet {
		if n, ok, err := initPreviousMaxRoots(old); err != nil {
			return err
		} else if ok {
			o.maxRoots = n
		}
	}
	previous := false
	if len(o.dirs) == 0 && len(old) > 0 {
		previous = true
		o.dirs, err = initPreviousDirs(old)
		if err != nil {
			return err
		}
	}
	if len(o.dirs) == 0 {
		o.dirs = []string{"."}
	}
	var roots, dirs []string
	for _, dir := range o.dirs {
		if err := ctx.Err(); err != nil {
			return err
		}
		given := dir
		dir, err = inside(configPath("", dir), false)
		if err == nil {
			var st os.FileInfo
			if st, err = os.Stat(dir); err == nil && !st.IsDir() {
				return fmt.Errorf("%s is not a directory", report.Clean(dir))
			}
		}
		if previous && errors.Is(err, iofs.ErrNotExist) {
			return fmt.Errorf("%s from %s no longer exists; pass the directories to scan as arguments: tforphans init -update DIR", report.Clean(given), defaultConfigFile)
		}
		if err != nil {
			return err
		}
		if !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
		found, unread, _, err := tf.FindRoots(dir, false)
		if err != nil {
			return fmt.Errorf("discover %s: %w", report.Clean(dir), err)
		}
		if len(unread) > 0 {
			// Do not write a partial selection that would look complete next time.
			report.Section(w, "Discovery incomplete")
			for _, u := range unread {
				fmt.Fprintf(w, "Not checked: %s (%s)\n", report.Clean(u.Path), report.Clean(u.Reason))
			}
			return errors.New("root discovery is incomplete; fix the listed directories before running init again")
		}
		if tf.HasConfig(dir) {
			found = append(found, dir)
		}
		for _, root := range found {
			root, err = inside(root, false)
			if err != nil {
				return err
			}
			if !slices.Contains(roots, root) {
				roots = append(roots, root)
			}
			if o.maxRoots > 0 && len(roots) > o.maxRoots {
				return fmt.Errorf("found more than %d roots; narrow DIR or raise -max-roots", o.maxRoots)
			}
		}
	}
	slices.Sort(roots)
	slices.Sort(dirs)
	var queryRoots, stateOnly []string
	var details []initRoot
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !tf.HasConfig(root) {
			stateOnly = append(stateOnly, root)
			continue
		}
		detail, err := inspectInitRoot(root)
		if err != nil {
			return err
		}
		queryRoots = append(queryRoots, root)
		details = append(details, detail)
	}
	if len(queryRoots) == 0 {
		return noInitRoots(ctx, dirs)
	}
	config, err := initConfig(old, queryRoots, dirs, scans, o.scans != nil, saveMaxRoots)
	if err != nil {
		return err
	}
	report.Section(w, "Terraform roots")
	fmt.Fprintf(w, "Found %d Terraform roots under %s\n", len(queryRoots), report.Clean(strings.Join(dirs, ", ")))
	for _, root := range stateOnly {
		fmt.Fprintf(w, "State source: %s (no Terraform configuration; included through state-roots)\n", report.Clean(root))
	}
	for _, root := range initRemoved(old, "roots", queryRoots, stateOnly) {
		fmt.Fprintf(w, "%s %s (not found under the selected directories; pass it as DIR to keep it)\n", report.Accent(w, report.Attention, "Removed from roots:"), report.Clean(root))
	}
	for _, dir := range initRemoved(old, "state-roots", dirs) {
		fmt.Fprintf(w, "%s %s (not among the given directories; pass it as DIR to keep it)\n", report.Accent(w, report.Attention, "Removed from state-roots:"), report.Clean(dir))
	}
	var saved map[string]any
	if err := yaml.Unmarshal(config, &saved); err != nil {
		return err
	}
	enabled, _ := configValues(saved["scan"])
	suggested := map[string]bool{}
	for _, d := range details {
		fmt.Fprintf(w, "\n  %s: backend %s\n", report.Clean(d.Dir), report.Clean(d.Backend))
		if !d.Initialized {
			fmt.Fprintf(w, "    %s terraform -chdir=%s init\n", report.Accent(w, report.Attention, "Setup needed:"), initDirArg(d.Dir))
		}
		for _, p := range d.Providers {
			version := "not locked"
			if p.Version != "" {
				version = p.Version
			}
			configs := strings.Join(p.Configs, ", ")
			if configs == "" {
				configs = "from lock file (possibly a child module)"
			}
			fmt.Fprintf(w, "    %s %s, %s\n", report.Clean(p.Source), report.Clean(version), report.Clean(configs))
			for _, name := range slices.Sorted(maps.Keys(sources)) {
				if model.SameProvider(p.Source, sources[name].Provider) {
					suggested[name] = true
				}
			}
		}
	}
	// The adapters of these roots' providers that the config does not
	// enable. One update enables them all: -scan replaces the saved list,
	// so the command repeats the enabled ones.
	var missing []string
	for _, name := range slices.Sorted(maps.Keys(suggested)) {
		if !slices.Contains(enabled, name) {
			missing = append(missing, name)
		}
	}
	enable := ""
	if len(missing) > 0 {
		enable = "tforphans init -update"
		for _, name := range slices.Concat(enabled, missing) {
			enable += " -scan " + initDirArg(name)
		}
		enable += " --"
		for _, dir := range dirs {
			enable += " " + initDirArg(dir)
		}
		fmt.Fprintln(w)
		report.Section(w, "Scan adapters")
		for _, name := range missing {
			fmt.Fprintf(w, "%s -scan %s before the first audit.\n", report.Accent(w, report.Attention, "Recommended:"), name)
			fmt.Fprintf(w, "  %s has no list resources in the versions tforphans knows:\n", strings.TrimPrefix(sources[name].Provider, "registry.terraform.io/"))
			fmt.Fprintln(w, "  without the scan, the audit reads its states but lists none of its")
			fmt.Fprintln(w, "  objects. The scan uses credentials from the environment, not the")
			fmt.Fprintln(w, "  roots' provider blocks.")
		}
		fmt.Fprintln(w, "  Enable:", enable)
	}
	fmt.Fprintln(w)
	report.Section(w, "Not checked")
	fmt.Fprintln(w, "Provider listing capabilities, credentials and cloud scopes have not been checked.")
	fmt.Fprintln(w, "Only states under the selected directories are included; add other state sources if needed.")
	if o.dryRun {
		fmt.Fprintln(w)
		report.Section(w, "Proposed "+defaultConfigFile+" (not written)")
		// The preview may carry names and comments from the repository.
		_, err = io.WriteString(w, report.CleanLines(string(config)))
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := writeInitConfig(config, old, info); err != nil {
		return err
	}
	action := "Created"
	if info != nil {
		action = "Updated"
	}
	fmt.Fprintln(w)
	report.Section(w, "Result")
	fmt.Fprintf(w, "%s %s\n", report.Accent(w, report.Good, action), defaultConfigFile)
	if enable != "" {
		fmt.Fprintf(w, "Next: %s\nThen: tforphans doctor\n", enable)
	} else {
		fmt.Fprintln(w, "Next: tforphans doctor\nThen, when it reports ready: tforphans")
	}
	return nil
}

// initRemoved are paths under key of the updated config that the update
// does not keep, so that a narrower search does not drop them silently.
func initRemoved(old []byte, key string, kept ...[]string) []string {
	var config map[string]any
	if yaml.Unmarshal(old, &config) != nil {
		return nil
	}
	previous, _ := configValues(config[key])
	var removed []string
	for _, p := range previous {
		if rel, err := inside(configPath("", p), false); err == nil {
			p = rel
		} else {
			p = filepath.Clean(p)
		}
		if !slices.ContainsFunc(kept, func(paths []string) bool { return slices.Contains(paths, p) }) && !slices.Contains(removed, p) {
			removed = append(removed, p)
		}
	}
	return removed
}

func initDirArg(dir string) string {
	if strings.ContainsFunc(dir, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) {
		return shellArg("<directory with control characters>")
	}
	return shellArg(dir)
}

// noInitRoots explains an init that found no root module. Directories with
// configuration but no backend block or local state are not taken for
// roots on their own (child modules look the same): name a few of them.
func noInitRoots(ctx context.Context, dirs []string) error {
	var withConfig []string
	for _, dir := range dirs {
		filepath.WalkDir(dir, func(path string, d iofs.DirEntry, err error) error {
			if err != nil || ctx.Err() != nil || len(withConfig) > 5 {
				return filepath.SkipAll
			}
			if !d.IsDir() {
				return nil
			}
			if path != dir && (strings.HasPrefix(d.Name(), ".") || d.Name() == "vendor" || d.Name() == "testdata") {
				return filepath.SkipDir
			}
			if path != dir && tf.HasConfig(path) {
				withConfig = append(withConfig, path)
			}
			return nil
		})
	}
	if len(withConfig) == 0 {
		return errors.New("no Terraform configuration found; pass a root directory explicitly, or use -scan/-plugin for a provider without a root")
	}
	var example []string
	for _, d := range withConfig[:min(2, len(withConfig))] {
		example = append(example, shellArg(d))
	}
	more := ""
	if len(withConfig) > 5 {
		withConfig, more = withConfig[:5], " ..."
	}
	return fmt.Errorf("no root module found: directories count as roots by a backend or cloud block or a local state, and these only have configuration: %s%s; pass the roots among them as DIR, e.g. tforphans init %s", report.Clean(strings.Join(withConfig, ", ")), more, report.Clean(strings.Join(example, " ")))
}
