package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/5xxeu/tforphans/internal/report"
	"github.com/5xxeu/tforphans/internal/state"
)

const diffUsage = `Usage: tforphans diff [flags] OLD.json NEW.json

Compare two -format json reports without running Terraform. Missing findings
are resolved only when NEW's coverage fully lists their type and scope.

Exit status: 2 when added findings exist, 0 otherwise, 1 on error.

Flags:
`

func runDiff(argv []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tforphans diff", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, diffUsage)
		fs.PrintDefaults()
	}
	format := fs.String("format", "text", "output format: text or json")
	verbose := fs.Bool("v", false, "show all findings and coverage changes")
	var paths []string
	for rest := argv; ; {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return exitOK
			}
			return exitError
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		paths = append(paths, rest[0])
		rest = rest[1:]
	}
	if len(paths) != 2 {
		fmt.Fprintln(stderr, "error: diff needs OLD.json and NEW.json")
		fs.Usage()
		return exitError
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "error: unknown -format %q\n", report.Clean(*format))
		return exitError
	}
	old, err := report.ReadSnapshot(paths[0], state.DefaultMaxSize)
	if err != nil {
		fmt.Fprintln(stderr, "error:", report.CleanLines(err.Error()))
		return exitError
	}
	current, err := report.ReadSnapshot(paths[1], state.DefaultMaxSize)
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
