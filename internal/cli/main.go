// Package cli is the tforphans command line: flags, the audit, adopt,
// doctor and init commands and their output. diff lives in diffcmd.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"slices"
	"syscall"

	"github.com/5xxeu/tforphans/internal/cliflag"
	"github.com/5xxeu/tforphans/internal/diffcmd"
	"github.com/5xxeu/tforphans/internal/report"
	"github.com/5xxeu/tforphans/internal/tf"
)

const (
	exitOK      = 0
	exitError   = 1
	exitOrphans = 2

	defaultIgnoreFile = ".tforphans-ignore"
)

// version is the release version given to Main; go install builds take
// the module version.
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

// Main runs tforphans with the command line of the process and exits.
// v is the release version, empty for other builds.
func Main(v string) {
	version = v
	// Started by terragrunt as its terraform (see tf.TerragruntStatePull).
	tf.RunWrapper()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	tf.CloseTurns()
	os.Exit(code)
}

func run(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	if len(argv) > 0 && argv[0] == "init" {
		return runInit(ctx, argv[1:], stdout, stderr)
	}
	if len(argv) > 0 && argv[0] == "doctor" {
		return runDoctor(ctx, argv, stdout, stderr)
	}
	if len(argv) > 0 && argv[0] == "diff" {
		return diffcmd.Run(argv[1:], stdout, stderr)
	}
	terminalErr := isTerminal(stderr)
	stderr = report.WithColor(stderr, colorEnabled(stderr))
	o, err := parseFlags(argv, stdout)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		usageError(stderr, err, commandName(argv))
		return exitError
	}
	if !o.quiet {
		o.run.progress = stderr
		if terminalErr {
			o.run.steps = stderr
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

// usageError prints an error of the command line and where to find usage.
func usageError(w io.Writer, err error, command string) {
	fmt.Fprintln(w, "error:", report.CleanLines(err.Error()))
	fmt.Fprintln(w, cliflag.HelpHint(command))
}

// commandName is "tforphans" or "tforphans <command>" for -h hints.
func commandName(argv []string) string {
	if len(argv) > 0 && slices.Contains(commands, argv[0]) {
		return "tforphans " + argv[0]
	}
	return "tforphans"
}

type exitCode int

func (c exitCode) Error() string { return fmt.Sprintf("exit %d", int(c)) }
