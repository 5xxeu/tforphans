package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/5xxeu/tforphans/internal/report"
	"github.com/5xxeu/tforphans/internal/state"
	"github.com/5xxeu/tforphans/internal/tf"
)

// pullRoot reads the state of every workspace of a root module and returns
// the instances and the number of states read. A workspace that fails does
// not stop the others: the instances read so far come back with the error.
func pullRoot(ctx context.Context, o *options, dir string) ([]state.Instance, int, error) {
	r := tf.Runner{Bin: o.terraformBin, Dir: dir, AnyVersion: o.anyVersion, MaxStateSize: o.maxStateSize}
	// "" is the selected workspace (no TF_WORKSPACE).
	workspaces := []string{""}
	if !o.currentWS {
		var err error
		if workspaces, err = r.Workspaces(ctx); err != nil {
			return nil, 0, err
		}
	}
	var all []state.Instance
	var errs []error
	n := 0
	for _, ws := range workspaces {
		if ctx.Err() != nil {
			return all, n, errors.Join(append(errs, ctx.Err())...)
		}
		r.Workspace = ws
		if o.refresh {
			// A refresh plans the whole root: minutes on a large one, so
			// say what is going on.
			start := time.Now()
			o.progressf("refreshing %s", inWorkspaceName(dir, ws))
			raw, err := r.RefreshedState(ctx)
			o.progressf("refreshed %s in %s", inWorkspaceName(dir, ws), time.Since(start).Round(time.Second))
			if err == nil {
				var insts []state.Instance
				if insts, err = state.ParseShow(raw); err == nil {
					all = append(all, insts...)
					n++
					continue
				}
			}
			if ctx.Err() != nil {
				return all, n, errors.Join(append(errs, ctx.Err())...)
			}
			o.mu.Lock()
			o.stateNotes = append(o.stateNotes, fmt.Sprintf("%s: -refresh failed, the stored state was read instead: %v", dir, inWorkspace(ws, err)))
			o.mu.Unlock()
		}
		raw, err := r.StatePull(ctx)
		if err != nil {
			errs = append(errs, inWorkspace(ws, err))
			continue
		}
		insts, err := state.Parse(raw)
		if err != nil {
			errs = append(errs, inWorkspace(ws, err))
			continue
		}
		all = append(all, insts...)
		n++
	}
	return all, n, errors.Join(errs...)
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
	// A root argument is queried with terraform, which reads the backend of
	// its .tf files, not the one terragrunt renders: a unit there would
	// stand in with a stale state (or none).
	for _, r := range o.roots {
		if !o.terragrunt {
			break
		}
		kind, err := tf.TerragruntKind(r)
		if err != nil {
			return nil, fmt.Errorf("root %s: %w", r, err)
		}
		if kind != tf.TerragruntNone {
			return nil, fmt.Errorf("root %s is a Terragrunt unit: terraform query does not run in units, give its directory to -state-roots only", r)
		}
	}
	for _, r := range o.roots {
		if abs, err := filepath.Abs(r); err == nil {
			skip[abs] = true
		}
	}
	var dirs, skipped []string
	for _, tree := range o.stateRoots {
		found, unread, parents, err := tf.FindRoots(tree, o.terragrunt)
		if err != nil {
			return nil, fmt.Errorf("-state-roots %s: %w", tree, err)
		}
		for _, p := range parents {
			if !slices.ContainsFunc(o.tgUnits, func(u string) bool { return sameDir(u, p.Dir) }) {
				skipped = append(skipped, fmt.Sprintf("%s (by %s)", p.Dir, p.By))
			}
		}
		// A root there may manage what is found: like an unread state
		// (adopt refuses, coverage is incomplete).
		for _, u := range unread {
			sum.UnreadStates = append(sum.UnreadStates, report.UnreadState{Root: u.Path, Error: u.Reason})
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

	// Units named with -terragrunt-unit are read even when units include
	// them (FindRoots took them for shared configuration).
	for _, u := range o.tgUnits {
		kind, err := tf.TerragruntKind(u)
		if err != nil {
			return nil, fmt.Errorf("-terragrunt-unit %s: %w", u, err)
		}
		if kind == tf.TerragruntNone {
			return nil, fmt.Errorf("-terragrunt-unit %s: no terragrunt.hcl there", u)
		}
		if abs, err := filepath.Abs(u); err == nil && !skip[abs] {
			skip[abs] = true
			dirs = append(dirs, u)
		}
	}
	if len(skipped) > 0 {
		sum.Notes = append(sum.Notes, fmt.Sprintf("%d terragrunt.hcl taken for shared configuration, since units include them, and not read as units: %s; -terragrunt-unit DIR reads one as a unit", len(skipped), strings.Join(firstN(skipped, 5), ", ")))
	}

	// A path that is wrong (.. from the wrong directory) can reach a whole
	// home directory: stop before terraform runs in all of it.
	if o.maxRoots > 0 && len(dirs) > o.maxRoots {
		return nil, fmt.Errorf("-state-roots found %d root modules under %s, more than -max-roots %d: check the paths (relative paths of a config file are relative to the file), or raise -max-roots", len(dirs), strings.Join(o.stateRoots, ", "), o.maxRoots)
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	done := 0
	for _, d := range dirs {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			insts, n, err := pullStateRoot(ctx, o, d)
			mu.Lock()
			defer mu.Unlock()
			// The workspaces that were read still count.
			idx.Add(insts)
			sum.States += n
			if err != nil {
				sum.UnreadStates = append(sum.UnreadStates, report.NewUnreadState(d, err))
			}
			done++
			o.stepf("read state of %s (%d/%d roots)", d, done, len(dirs))
		}()
	}
	wg.Wait()
	return dirs, ctx.Err()
}

func pullStateRoot(ctx context.Context, o *options, dir string) ([]state.Instance, int, error) {
	if err := tf.RemoveStale(dir); err != nil {
		return nil, 0, err
	}
	// A Terragrunt unit: terragrunt renders the backend and initializes
	// on its own. One state (units do not use workspaces), never refreshed.
	unit := false
	if o.terragrunt {
		kind, err := tf.TerragruntKind(dir)
		if err != nil {
			return nil, 0, err
		}
		// FindRoots kept only the parents with no units below.
		unit = kind != tf.TerragruntNone
	}
	if unit {
		raw, err := tf.TerragruntStatePull(ctx, "terragrunt", o.terraformBin, dir, o.maxStateSize)
		if err != nil {
			return nil, 0, err
		}
		insts, err := state.Parse(raw)
		if err != nil {
			return nil, 0, err
		}
		return insts, 1, nil
	}
	r := tf.Runner{Bin: o.terraformBin, Dir: dir, AnyVersion: o.anyVersion}
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

// sameDir reports whether two paths name one directory.
func sameDir(a, b string) bool {
	x, err1 := filepath.Abs(a)
	y, err2 := filepath.Abs(b)
	return err1 == nil && err2 == nil && x == y
}

// firstN returns at most n elements and a count of the rest.
func firstN(xs []string, n int) []string {
	if len(xs) <= n {
		return xs
	}
	return append(slices.Clone(xs[:n]), fmt.Sprintf("and %d more", len(xs)-n))
}

// inWorkspaceName names a root and, when not the selected one, its
// workspace.
func inWorkspaceName(dir, ws string) string {
	if ws == "" {
		return dir
	}
	return dir + " (workspace " + ws + ")"
}

// stepf prints a progress line when steps are shown (see options.steps).
func (o *options) stepf(format string, args ...any) {
	if o.steps == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	fmt.Fprintln(o.steps, report.Clean(fmt.Sprintf(format, args...)))
}

// isTerminal reports whether w is a terminal.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// progressf prints a progress line on stderr for long steps.
func (o *options) progressf(format string, args ...any) {
	if o.progress == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	fmt.Fprintln(o.progress, report.Clean(fmt.Sprintf(format, args...)))
}
