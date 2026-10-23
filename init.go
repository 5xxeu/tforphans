package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"unicode"

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
	scans          listFlag
	dirs           []string
}

func runInit(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tforphans init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, initUsage); fs.PrintDefaults() }
	var o initOptions
	fs.BoolVar(&o.update, "update", false, "refresh roots under the previous search directories, preserving other settings")
	fs.BoolVar(&o.dryRun, "dry-run", false, "show discovery and the proposed config without writing it")
	fs.IntVar(&o.maxRoots, "max-roots", 200, "maximum discovered root modules (0: no limit)")
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
	if err := fs.Parse(flags); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitError
	}
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
	if len(o.dirs) == 0 && len(old) > 0 {
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
		dir, err = inside(configPath("", dir), false)
		if err != nil {
			return err
		}
		st, err := os.Stat(dir)
		if err != nil {
			return err
		}
		if !st.IsDir() {
			return fmt.Errorf("%s is not a directory", report.Clean(dir))
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
		return errors.New("no Terraform configuration found; pass a root directory explicitly, or use -scan/-plugin for a provider without a root")
	}
	config, err := initConfig(old, queryRoots, dirs, scans, o.scans != nil)
	if err != nil {
		return err
	}
	report.Section(w, "Terraform roots")
	fmt.Fprintf(w, "Found %d Terraform roots under %s\n", len(queryRoots), report.Clean(strings.Join(dirs, ", ")))
	for _, root := range stateOnly {
		fmt.Fprintf(w, "State source: %s (no Terraform configuration; included through state-roots)\n", report.Clean(root))
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
	adapters := false
	for _, name := range slices.Sorted(maps.Keys(suggested)) {
		if !slices.Contains(enabled, name) {
			if !adapters {
				fmt.Fprintln(w)
				report.Section(w, "Scan adapters")
				adapters = true
			}
			fmt.Fprintf(w, "Adapter available: -scan %s (credentials from the environment).\n", name)
			fmt.Fprintf(w, "  Enable it explicitly: tforphans init -update -scan %s --", name)
			for _, dir := range dirs {
				fmt.Fprintf(w, " %s", initDirArg(dir))
			}
			fmt.Fprintln(w)
		}
	}
	fmt.Fprintln(w)
	report.Section(w, "Not checked")
	fmt.Fprintln(w, "Provider listing capabilities, credentials and cloud scopes have not been checked.")
	fmt.Fprintln(w, "Only states under the selected directories are included; add other state sources if needed.")
	if o.dryRun {
		fmt.Fprintln(w)
		report.Section(w, "Proposed "+defaultConfigFile+" (not written)")
		_, err = w.Write(config)
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
	fmt.Fprintf(w, "%s %s\nNext: tforphans doctor\nThen: tforphans\n", report.Accent(w, report.Good, action), defaultConfigFile)
	return nil
}

func initDirArg(dir string) string {
	if strings.ContainsFunc(dir, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) {
		return shellArg("<directory with control characters>")
	}
	return shellArg(dir)
}
