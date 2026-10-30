// Package diffcmd implements tforphans diff: it compares two saved JSON
// reports and runs no Terraform.
package diffcmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/5xxeu/tforphans/internal/cliflag"
	"github.com/5xxeu/tforphans/internal/report"
	"github.com/5xxeu/tforphans/internal/state"
)

const (
	exitOK      = 0
	exitError   = 1
	exitOrphans = 2
)

const diffUsage = `Usage: tforphans diff [flags] OLD.json NEW.json

Compare two -format json reports without running Terraform. Missing findings
are resolved only when NEW's coverage fully lists their type and scope.

Exit status: 2 when added findings exist, 0 otherwise, 1 on error.

Flags:
`

// Run executes tforphans diff with the arguments after "diff" and returns
// the exit status: 2 when NEW has added findings, 1 on error, 0 otherwise.
func Run(argv []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tforphans diff", flag.ContinueOnError)
	format := fs.String("format", "text", "output format: text or json")
	verbose := fs.Bool("v", false, "show all findings and coverage changes")
	help := func(w io.Writer) {
		fmt.Fprint(w, diffUsage)
		fs.SetOutput(w)
		fs.PrintDefaults()
	}
	fail := func(err error) int {
		fmt.Fprintln(stderr, "error:", report.CleanLines(err.Error()))
		fmt.Fprintln(stderr, cliflag.HelpHint("tforphans diff"))
		return exitError
	}
	// Flags may follow the paths; after -- everything is a path.
	var paths []string
	for rest := argv; ; {
		if err := cliflag.Parse(fs, rest, stdout, help); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return exitOK
			}
			return fail(err)
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		// Parse stops at the first path, or after --, which it drops.
		if n := len(argv) - len(rest); n > 0 && argv[n-1] == "--" {
			paths = append(paths, rest...)
			break
		}
		paths = append(paths, rest[0])
		argv, rest = rest[1:], rest[1:]
	}
	if len(paths) != 2 {
		return fail(errors.New("diff needs OLD.json and NEW.json"))
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "error: unknown -format %q\n", report.Clean(*format))
		return exitError
	}
	old, err := report.ReadSnapshot(context.Background(), paths[0], state.DefaultMaxSize)
	if err != nil {
		fmt.Fprintln(stderr, "error:", report.CleanLines(err.Error()))
		return exitError
	}
	current, err := report.ReadSnapshot(context.Background(), paths[1], state.DefaultMaxSize)
	if err != nil {
		fmt.Fprintln(stderr, "error:", report.CleanLines(err.Error()))
		return exitError
	}
	d := report.CompareSnapshots(*old, *current)
	if *format == "json" {
		err = d.JSON(stdout)
	} else {
		err = d.Text(stdout, *verbose)
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", report.CleanLines(err.Error()))
		return exitError
	}
	if len(d.Added) > 0 {
		return exitOrphans
	}
	return exitOK
}
