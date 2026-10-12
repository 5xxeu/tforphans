package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// defaultConfigFile is read from the current directory when present.
const defaultConfigFile = ".tforphans.yaml"

// configOnlyExplicit are keys a config file found on its own may not set:
// they run commands or the root's data sources (refresh plans the root),
// install providers, send credentials, put attribute values into the
// output, create files (adopt -o), or make results look more certain than
// they are (a state file of its own, trusting cluster states, adopt with
// unread states), and the file may come with someone else's repository. A
// file given with -config may.
var configOnlyExplicit = []string{"plugin", "terraform", "init", "refresh", "allow-unpinned", "allow-http", "include-attrs", "o",
	"state", "proxmox-cluster-states", "plugin-cluster-states", "allow-incomplete"}

// configPathKeys hold paths, which are relative to the config file.
var configPathKeys = []string{"state-roots", "state", "ignore", "o", "terraform"}

// modeOnly are flags of one mode (adopt or not): a config file shared by
// both may set them, and the other mode skips them.
var modeOnly = []string{"format", "o", "allow-incomplete"}

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
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return err
	}
	// Paths of a file in the current directory stay as written.
	if cwd, err := os.Getwd(); err == nil && cwd == dir {
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
		vals, err := configValues(doc[key])
		if err != nil {
			return fmt.Errorf("%s: %s: %w", path, key, err)
		}
		if key == "roots" {
			if len(o.roots) == 0 {
				for _, v := range vals {
					p := configPath(dir, v)
					if err := inside(p, explicit); err != nil {
						return fmt.Errorf("%s: roots: %w", path, err)
					}
					o.roots = append(o.roots, p)
				}
			}
			continue
		}
		f := fs.Lookup(key)
		switch {
		case f == nil && slices.Contains(modeOnly, key):
			continue
		case f == nil || key == "config" || key == "no-config":
			return fmt.Errorf("%s: unknown key %q (keys are flag names without the dash, and roots)", path, key)
		case !explicit && slices.Contains(configOnlyExplicit, key):
			return fmt.Errorf("%s: %s is accepted only from a file given with -config, since this file is read on its own and could come with someone else's repository", path, key)
		case set[key]:
			continue
		}
		if _, isBool := f.Value.(interface{ IsBoolFlag() bool }); isBool && len(vals) != 1 {
			return fmt.Errorf("%s: %s takes true or false", path, key)
		}
		for _, v := range vals {
			if slices.Contains(configPathKeys, key) && !strings.Contains(v, "://") && v != "-" && (key != "terraform" || strings.ContainsRune(v, '/')) {
				v = configPath(dir, v)
				if err := inside(v, explicit); err != nil {
					return fmt.Errorf("%s: %s: %w", path, key, err)
				}
			}
			if err := fs.Set(key, v); err != nil {
				return fmt.Errorf("%s: %s: %w", path, key, err)
			}
		}
	}
	return nil
}

// inside keeps the paths of a config file read on its own within the
// current directory, symbolic links resolved: the file comes with the
// repository and may choose among its roots, not reach other directories
// (another checkout's states, a home directory). A file given with
// -config may point anywhere.
func inside(p string, explicit bool) error {
	if explicit {
		return nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return err
	}
	if real, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = real
	}
	// A path that does not exist yet: resolve its longest existing part,
	// which may be a link out.
	for rest := ""; ; {
		real, err := filepath.EvalSymlinks(abs)
		if err == nil {
			abs = filepath.Join(real, rest)
			break
		}
		parent := filepath.Dir(abs)
		if !errors.Is(err, fs.ErrNotExist) || parent == abs {
			return fmt.Errorf("%s: %w", p, err)
		}
		rest = filepath.Join(filepath.Base(abs), rest)
		abs = parent
	}
	rel, err := filepath.Rel(cwd, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("%s is outside the current directory; only a file given with -config may point there", p)
	}
	return nil
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
			return filepath.Join(home, rest)
		}
	}
	if filepath.IsAbs(p) || dir == "" {
		return p
	}
	return filepath.Join(dir, p)
}
