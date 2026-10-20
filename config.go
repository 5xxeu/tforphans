package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/5xxeu/tforphans/internal/state"
	"github.com/5xxeu/tforphans/internal/tf"
	"go.yaml.in/yaml/v3"
)

// defaultConfigFile is read from the current directory when present.
const defaultConfigFile = ".tforphans.yaml"

// commandLineOnly are keys no config file may set, not even one given with
// -config: they run commands (plugin, terraform, terragrunt, check-auth),
// including units' run_cmd and hooks or provider configuration dependencies,
// send credentials to an address of the file's choosing (state URLs, allow-http)
// or overwrite any file (adopt -o). Config files live in repositories and
// change in pull requests that a CI job then runs; such a change has to show in the
// command itself.
var commandLineOnly = []string{"plugin", "terraform", "terragrunt", "allow-http", "o", "out-dir", "adopt-root", "check-auth"}

// configOnlyExplicit are keys a config file found on its own may not set,
// one given with -config may: they run the root's data sources (refresh
// plans the root) or terraform init, install an unpinned provider, put
// attribute values into the output, or make results look more certain
// than they are (a state file of its own, trusting cluster states, adopt
// with unread states).
var configOnlyExplicit = []string{"init", "refresh", "allow-unpinned", "include-attrs",
	"state", "proxmox-cluster-states", "plugin-cluster-states", "allow-incomplete"}

// configPathKeys hold paths, which are relative to the config file.
var configPathKeys = []string{"state-roots", "terragrunt-unit", "state", "ignore", "baseline"}

// modeOnly are flags of one mode (adopt or not): a config file shared by
// both may set them, and the other mode skips them.
var modeOnly = []string{"format", "allow-incomplete", "names-from-tags"}

// applyConfig sets the flags a config file gives and the command line does
// not. Keys are flag names; a list stands for a repeated flag, and roots for
// the root module arguments. Whatever the command line sets, list or not,
// replaces the file's value.
func applyConfig(fs *flag.FlagSet, o *options, path string, explicit bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	// The directory the file really is in: a link on the way to it counts
	// as the system resolves it.
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if cwd, err = filepath.EvalSymlinks(cwd); err != nil {
		return err
	}
	if !filepath.IsAbs(real) {
		real = filepath.Join(cwd, real)
	}
	dir := filepath.Dir(real)
	// Paths of a file in the current directory stay as written.
	if dir == cwd {
		dir = ""
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	keys := make([]string, 0, len(doc))
	for k := range doc {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if o.doctor && o.doctorConfigFlags[key] && !slices.Contains(commandLineOnly, key) && !doctorIgnoreKey(key) {
			continue
		}
		vals, err := configValues(doc[key])
		if err != nil {
			return fmt.Errorf("%s: %s: %w", path, key, err)
		}
		if key == "roots" {
			if len(o.roots) == 0 {
				for _, v := range vals {
					p, err := inside(configPath(dir, v), explicit)
					if err != nil {
						return fmt.Errorf("%s: roots: %w", path, err)
					}
					o.roots = append(o.roots, p)
				}
			}
			continue
		}
		f := fs.Lookup(key)
		if o.doctor && doctorIgnoreKey(key) {
			f = o.doctorIgnoreFlags.Lookup(key)
		}
		switch {
		case slices.Contains(commandLineOnly, key):
			return fmt.Errorf("%s: %s is accepted only on the command line: it runs a command, sends credentials or writes a file, and a change to a config file should not be enough for that", path, key)
		case f == nil && slices.Contains(modeOnly, key):
			continue
		case f == nil || key == "config" || key == "no-config":
			return fmt.Errorf("%s: unknown key %q (keys are flag names without the dash, and roots)", path, key)
		case !explicit && slices.Contains(configOnlyExplicit, key):
			return fmt.Errorf("%s: %s is accepted only from a file given with -config, since this file is read on its own and could come with someone else's repository", path, key)
		}
		if limit, ok := map[string]int64{"max-state-size": state.DefaultMaxSize, "max-query-size": tf.DefaultMaxQuerySize}[key]; ok && !explicit {
			for _, v := range vals {
				var size sizeFlag
				if err := size.Set(v); err != nil {
					return fmt.Errorf("%s: %s: %w", path, key, err)
				}
				if int64(size) > limit {
					return fmt.Errorf("%s: %s above %d bytes is accepted only on the command line or via -config, since this file is read on its own and could come with someone else's repository", path, key, limit)
				}
			}
		}
		if set[key] {
			continue
		}
		if _, isBool := f.Value.(interface{ IsBoolFlag() bool }); isBool && len(vals) != 1 {
			return fmt.Errorf("%s: %s takes true or false", path, key)
		}
		for _, v := range vals {
			// A state URL would receive the TF_HTTP_* credentials; for
			// the other keys "x://y" is a path like any other.
			if key == "state" && strings.Contains(v, "://") {
				return fmt.Errorf("%s: state: a URL is accepted only on the command line, since it receives the TF_HTTP_* credentials", path)
			}
			if slices.Contains(configPathKeys, key) && (key != "state" || v != "-") {
				p, err := inside(configPath(dir, v), explicit)
				if err != nil {
					return fmt.Errorf("%s: %s: %w", path, key, err)
				}
				v = p
			}
			target := fs
			if o.doctor && doctorIgnoreKey(key) {
				target = o.doctorIgnoreFlags
			}
			if err := target.Set(key, v); err != nil {
				return fmt.Errorf("%s: %s: %w", path, key, err)
			}
		}
	}
	return nil
}

// inside keeps the paths of a config file read on its own within the
// current directory: the file comes with the repository and may choose
// among its roots, not reach other directories (another checkout's states,
// a home directory). The path must exist; it is resolved as the operating
// system would (a link before .. counts), and the resolved path is used.
// A file given with -config may point anywhere.
func inside(p string, explicit bool) (string, error) {
	if explicit {
		return p, nil
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if cwd, err = filepath.EvalSymlinks(cwd); err != nil {
		return "", err
	}
	// A relative result is relative to the current directory.
	if !filepath.IsAbs(real) {
		real = filepath.Join(cwd, real)
	}
	rel, err := filepath.Rel(cwd, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%s is outside the current directory; only a file given with -config may point there", p)
	}
	return rel, nil
}

// configValues turns a scalar or a list of scalars into flag values.
func configValues(v any) ([]string, error) {
	switch t := v.(type) {
	case nil:
		return nil, errors.New("no value")
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			vals, err := configValues(e)
			if err != nil {
				return nil, err
			}
			if len(vals) != 1 || slices.ContainsFunc(t, func(e any) bool { _, ok := e.([]any); return ok }) {
				return nil, errors.New("lists of lists are not flags")
			}
			out = append(out, vals...)
		}
		return out, nil
	case map[string]any:
		return nil, errors.New("want a value or a list of values")
	default:
		return []string{fmt.Sprint(t)}, nil
	}
}

// configPath resolves a path of the config file: ~/ is the home directory,
// and a relative path is relative to the file.
func configPath(dir, p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			// Not Join: cleaning would fold a link before .. away.
			return home + string(filepath.Separator) + rest
		}
	}
	if filepath.IsAbs(p) || dir == "" {
		return p
	}
	// Not Join, for the same reason: jump/../prod with a link jump has to
	// end where the system would take it.
	return dir + string(filepath.Separator) + p
}
