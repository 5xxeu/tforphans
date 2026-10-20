package dsource

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/5xxeu/tforphans/internal/tf"
)

// ErrProviderNotInstalled means schema inspection cannot use a local provider.
var ErrProviderNotInstalled = errors.New("pinned provider is not installed locally")

// PlanReport describes a scan without reading any data sources.
type PlanReport struct {
	Blocks  int
	Skipped []tf.Skip
}

// InspectPlan uses the audit planner and only the pinned root's installed provider.
func InspectPlan(ctx context.Context, a Adapter, opt Options) (*PlanReport, error) {
	if opt.VersionRoot == "" {
		return nil, errors.New("provider is not pinned")
	}
	plugins := filepath.Join(tf.DataDir(opt.VersionRoot), "providers", a.Provider)
	if fi, err := os.Stat(plugins); err != nil || !fi.IsDir() {
		return nil, ErrProviderNotInstalled
	}
	dir, err := os.MkdirTemp("", "tforphans-doctor-plan-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	cache := filepath.Join(dir, "plugins")
	if err := os.Mkdir(cache, 0700); err != nil {
		return nil, err
	}
	r := tf.Runner{Bin: opt.TerraformBin, Dir: dir, Isolated: true, Env: []string{"TF_PLUGIN_CACHE_DIR=" + cache}}
	if err := initRootCache(ctx, &r, a, opt.VersionRoot, false); err != nil {
		return nil, err
	}
	raw, _, err := r.Exec(ctx, "providers", "schema", "-json", "-no-color")
	if err != nil {
		return nil, err
	}
	sch, err := parseSchema(raw, a.Provider)
	if err != nil {
		return nil, err
	}
	p := buildPlan(sch, a.prepared(sch), opt)
	result := &PlanReport{Skipped: p.skipped}
	if len(p.listings) > 0 {
		blocks, skipped := limitScope(p.blocks(), opt.Scope)
		result.Blocks = len(blocks)
		result.Skipped = append(result.Skipped, skipped...)
	}
	return result, nil
}

// CheckAuth reads only the adapter's probe, with a targeted plan and no lock.
// The isolated root uses environment credentials, like a scan does.
func CheckAuth(ctx context.Context, a Adapter, opt Options) error {
	if a.Probe == "" {
		return errors.New("no supported authentication probe")
	}
	if opt.VersionRoot == "" {
		return errors.New("provider is not pinned")
	}
	dir, err := os.MkdirTemp("", "tforphans-doctor-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	cache := filepath.Join(dir, "plugins")
	if err := os.Mkdir(cache, 0700); err != nil {
		return err
	}
	r := tf.Runner{Bin: opt.TerraformBin, Dir: dir, Isolated: true, Env: []string{"TF_PLUGIN_CACHE_DIR=" + cache}}
	if err := initRootCache(ctx, &r, a, opt.VersionRoot, false); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), renderBlocks([]block{{dsType: a.Probe, label: "probe"}}), 0600); err != nil {
		return err
	}
	_, _, err = r.Exec(ctx, "plan", "-refresh=false", "-lock=false", "-input=false", "-no-color", "-target=data."+a.Probe+".probe")
	return err
}
