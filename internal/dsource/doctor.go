package dsource

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/5xxeu/tforphans/internal/tf"
)

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
