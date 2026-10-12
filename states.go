package main

import (
	"context"
	"errors"
	"fmt"
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
	r := tf.Runner{Bin: o.terraformBin, Dir: dir, AnyVersion: o.anyVersion}
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
	for _, r := range o.roots {
		if abs, err := filepath.Abs(r); err == nil {
			skip[abs] = true
		}
	}
	var dirs []string
	for _, tree := range o.stateRoots {
		found, unread, err := tf.FindRoots(tree, o.terragrunt)
		if err != nil {
			return nil, fmt.Errorf("-state-roots %s: %w", tree, err)
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

	// A path that is wrong (.. from the wrong directory) can reach a whole
	// home directory: stop before terraform runs in all of it.
	if o.maxRoots > 0 && len(dirs) > o.maxRoots {
		return nil, fmt.Errorf("-state-roots found %d root modules under %s, more than -max-roots %d: check the paths (relative paths of a config file are relative to the file), or raise -max-roots", len(dirs), strings.Join(o.stateRoots, ", "), o.maxRoots)
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
			// The workspaces that were read still count.
			idx.Add(insts)
			sum.States += n
			if err != nil {
				sum.UnreadStates = append(sum.UnreadStates, report.NewUnreadState(d, err))
			}
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
	if unit, _ := tf.IsTerragruntUnit(dir); o.terragrunt && unit {
		raw, err := tf.TerragruntStatePull(ctx, "terragrunt", o.terraformBin, dir)
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

// progressf prints a progress line on stderr for long steps.
func (o *options) progressf(format string, args ...any) {
	if o.progress == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	fmt.Fprintln(o.progress, report.Clean(fmt.Sprintf(format, args...)))
}
