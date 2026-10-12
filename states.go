package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

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
			raw, err := r.RefreshedState(ctx)
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
			// The workspaces that were read still count.
			idx.Add(insts)
			sum.States += n
			if err != nil {
				sum.UnreadStates = append(sum.UnreadStates, d)
				sum.Warnings = append(sum.Warnings, fmt.Sprintf("state of %s not read in full, its resources may show as unmanaged: %v", d, err))
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
