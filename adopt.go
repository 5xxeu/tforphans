package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/report"
	"github.com/5xxeu/tforphans/internal/tf"
)

// hasImport reports whether an import block of the configuration already
// targets r: same type, the same provider configuration, and the same
// literal id or identity. With a target root (the root module that listed
// r, or the one -adopt-root names) only blocks of that root count; an
// object of -scan or a plugin without one has no root, and a block of any
// root with its provider configuration counts.
func hasImport(r model.Resource, imports []tf.Import, target string) bool {
	for _, im := range imports {
		if im.Type != r.Type || target != "" && im.Dir != target {
			continue
		}
		if providerConfig(im.Provider, im.Type) != providerConfig(r.Provider, r.Type) {
			continue
		}
		if im.ID != "" && im.ID == r.ImportID {
			return true
		}
		if im.Identity != nil && r.ImportID == "" && identityEqual(r.Identity, im.Identity) {
			return true
		}
	}
	return false
}

// providerConfig names the provider configuration of a reference such as
// vault.other; none means the default configuration of the type's prefix.
func providerConfig(ref, typ string) string {
	if ref == "" {
		ref, _, _ = strings.Cut(typ, "_")
	}
	return ref
}

func identityEqual(live map[string]any, literal map[string]string) bool {
	n := 0
	for k, v := range live {
		c, ok := model.Canonical(v)
		if !ok {
			continue
		}
		if literal[k] != c {
			return false
		}
		n++
	}
	return n > 0 && n == len(literal)
}

// providerNote is printed when an import block names its provider;
// Terraform 1.16 lifted this restriction.
const providerNote = "With Terraform before 1.16, move the provider line of each import block into its generated resource before applying: older versions generate resources without it, and accept provider in an import block only while its configuration is still to be generated."

// adopt writes import blocks for the orphans to stdout or to a new file.
// Objects that cannot be written as an import block are warnings.
func adopt(o *options, orphans []model.Resource, reserved map[string]bool, sum *report.Summary, stdout, stderr io.Writer) error {
	if len(orphans) == 0 {
		fmt.Fprintln(stderr, "Nothing to adopt.")
		return nil
	}
	skipped := func(n int) {
		if n < len(orphans) {
			sum.Warnings = append(sum.Warnings, fmt.Sprintf("%d objects have no valid resource type or provider name; no import blocks written for them", len(orphans)-n))
		}
	}
	// The hint goes to stderr, so it also reaches people who redirect
	// stdout to a file.
	providerHint := func() {
		if slices.ContainsFunc(orphans, func(r model.Resource) bool { return r.Provider != "" }) {
			fmt.Fprintln(stderr, providerNote)
		}
	}
	head := adoptHeader(sum)
	if o.outDir != "" {
		n, err := adoptDir(o, orphans, reserved, head, stderr)
		if err == nil {
			skipped(n)
			providerHint()
		}
		return err
	}
	if o.out == "" {
		if _, err := io.WriteString(stdout, head); err != nil {
			return err
		}
		n, err := report.Import(stdout, orphans, reserved)
		skipped(n)
		if err == nil {
			providerHint()
		}
		return err
	}
	f, err := os.OpenFile(o.out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = io.WriteString(f, head)
	n := 0
	if err == nil {
		n, err = report.Import(f, orphans, reserved)
	}
	if err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	skipped(n)
	fmt.Fprintf(stderr, "Wrote %d import blocks to %s. Next, in the root module:\n  terraform plan -generate-config-out=generated.tf\n", n, o.out)
	providerHint()
	return nil
}

// adoptHeader is the comment above the import blocks: what they were
// checked against, and what is not among them.
func adoptHeader(sum *report.Summary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Objects that none of the %d states read manages.\n", sum.States)
	if sum.Uncertain > 0 {
		fmt.Fprintf(&b, "# Not included: %d uncertain objects (matched to state, but the state cannot confirm the account or cluster).\n", sum.Uncertain)
	}
	var gaps []string
	for _, c := range sum.Coverage {
		if c.Status == report.Incomplete && !slices.Contains(gaps, c.Type) {
			gaps = append(gaps, c.Type)
		}
	}
	switch {
	case len(gaps) > 0:
		fmt.Fprintf(&b, "# Coverage incomplete: not fully listed: %s.\n", report.Clean(strings.Join(gaps, ", ")))
	case !sum.Complete:
		b.WriteString("# Coverage incomplete (see the warnings of the run).\n")
	}
	b.WriteString("\n")
	return b.String()
}

var pluginSource = regexp.MustCompile(`^plugin[1-9][0-9]*$`)

// adoptRootFlags checks -out-dir and the NAME=DIR pairs of -adopt-root.
// NAME is a -scan provider or a plugin (plugin1 is the first -plugin).
func adoptRootFlags(o *options, pairs []string) error {
	if o.out != "" && o.outDir != "" {
		return errors.New("-o and -out-dir cannot be used together")
	}
	if len(pairs) > 0 && o.outDir == "" {
		return errors.New("-adopt-root needs -out-dir")
	}
	if o.outDir != "" {
		if _, err := os.Lstat(o.outDir); err == nil {
			return fmt.Errorf("-out-dir %s already exists: adopt writes a new directory and never overwrites", o.outDir)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for _, p := range pairs {
		name, dir, ok := strings.Cut(p, "=")
		if !ok || dir == "" {
			return fmt.Errorf("-adopt-root %q: want NAME=DIR, e.g. cloudflare=zones/example.com", p)
		}
		known := slices.Contains(o.scans, name)
		if pluginSource.MatchString(name) {
			n, err := strconv.Atoi(strings.TrimPrefix(name, "plugin"))
			known = err == nil && n <= len(o.plugins)
		}
		if !known {
			return fmt.Errorf("-adopt-root %q: %s is neither a -scan provider of this run nor one of its %d plugins (plugin1 is the first -plugin)", p, name, len(o.plugins))
		}
		if prev, ok := o.adoptRoots[name]; ok && prev != dir {
			return fmt.Errorf("-adopt-root %s is given twice: %s and %s", name, prev, dir)
		}
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			dir = real
		}
		if fi, err := os.Stat(dir); err != nil {
			return fmt.Errorf("-adopt-root %s: %w", name, err)
		} else if !fi.IsDir() {
			return fmt.Errorf("-adopt-root %s: %s is not a directory", name, dir)
		}
		if o.adoptRoots == nil {
			o.adoptRoots = map[string]string{}
		}
		o.adoptRoots[name] = dir
	}
	return nil
}

// adoptRootOf is the directory -adopt-root names for a -scan provider, if any.
func adoptRootOf(o *options, name string) []string {
	if d, ok := o.adoptRoots[name]; ok {
		return []string{d}
	}
	return nil
}

// adoptRootDirs are the directories -adopt-root names, in a fixed order.
func adoptRootDirs(o *options) []string {
	var dirs []string
	for _, d := range o.adoptRoots {
		if !slices.Contains(dirs, d) {
			dirs = append(dirs, d)
		}
	}
	slices.Sort(dirs)
	return dirs
}

// outDirRoots checks before the run that every root module that may get a
// file under -out-dir has a path there.
func outDirRoots(o *options) error {
	if o.outDir == "" {
		return nil
	}
	cwd, err := realCwd()
	if err != nil {
		return err
	}
	dirs := adoptRootDirs(o)
	if !o.noQuery {
		dirs = append(dirs, o.roots...)
	}
	for _, d := range dirs {
		if _, err := relRoot(cwd, d); err != nil {
			return err
		}
	}
	return nil
}

func realCwd() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = real
	}
	return cwd, nil
}

// fromRoot reports whether a root module's terraform query listed r: only
// those have a list block address, so a root directory named like a plugin
// (plugin1) does not take that plugin's objects.
func fromRoot(r model.Resource, roots map[string]bool) bool {
	return r.ListAddress != "" && roots[r.Source]
}

// adoptTarget is the root module directory r is adopted into, as canonDir
// spells it: the root that listed it, or the one -adopt-root names for its
// -scan provider or plugin; "" when it has none.
func adoptTarget(o *options, r model.Resource, roots map[string]bool) string {
	if fromRoot(r, roots) {
		return canonDir(r.Source)
	}
	if name, ok := sourceName(r.Source); ok {
		if d, ok := o.adoptRoots[name]; ok {
			return canonDir(d)
		}
	}
	return ""
}

// canonDir is the absolute path of a directory with links resolved before
// .. is applied, as the system resolves it.
func canonDir(dir string) string {
	if !filepath.IsAbs(dir) {
		// Not Abs, which cleans: a link before .. must count as the
		// system resolves it.
		if cwd, err := realCwd(); err == nil {
			dir = cwd + string(filepath.Separator) + dir
		}
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		return real
	}
	return filepath.Clean(dir)
}

// sourceName is the name -adopt-root uses for a source that is not a root
// module: the provider of -scan, or pluginN.
func sourceName(source string) (string, bool) {
	if name, ok := strings.CutPrefix(source, "-scan "); ok {
		return name, true
	}
	return source, pluginSource.MatchString(source)
}

// relRoot is the path of a root module directory from the current one, the
// path its file gets under -out-dir. Links are resolved, so that two
// spellings of one root share a file.
func relRoot(cwd, dir string) (string, error) {
	rel, err := filepath.Rel(cwd, canonDir(dir))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("root module %s is outside the current directory: run adopt -out-dir from a directory that contains every root module", dir)
	}
	return rel, nil
}

// adoptDir writes a file per root module under the new directory -out-dir:
// <root path>/imports.tf for the objects of that root and of the -scan
// providers and plugins -adopt-root assigns to it, _unassigned/NAME.tf for
// the other ones. The directory appears whole or not at all.
// It returns how many import blocks it wrote.
func adoptDir(o *options, orphans []model.Resource, reserved map[string]bool, head string, stderr io.Writer) (int, error) {
	cwd, err := realCwd()
	if err != nil {
		return 0, err
	}
	roots := map[string]bool{}
	for _, r := range o.roots {
		roots[r] = true
	}
	files := map[string][]model.Resource{}
	unassigned := map[string]string{} // file: source name
	for _, r := range orphans {
		dir := adoptTarget(o, r, roots)
		if dir == "" {
			name, ok := sourceName(r.Source)
			if !ok {
				name = "other"
			}
			file := filepath.Join("_unassigned", name+".tf")
			files[file] = append(files[file], r)
			unassigned[file] = name
			continue
		}
		rel, err := relRoot(cwd, dir)
		if err != nil {
			return 0, err
		}
		file := filepath.Join(rel, "imports.tf")
		files[file] = append(files[file], r)
	}
	parent, base := splitOutDir(o.outDir)
	if base == "" || base == "." || base == ".." {
		return 0, fmt.Errorf("-out-dir %s: name a new directory", o.outDir)
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return 0, err
	}
	parent = canonDir(parent)
	out := filepath.Join(parent, base)
	tmp, err := os.MkdirTemp(parent, "."+base+".tmp-")
	if err != nil {
		return 0, err
	}
	done := false
	defer func() {
		if !done {
			os.RemoveAll(tmp)
		}
	}()
	if err := os.Chmod(tmp, 0o755); err != nil {
		return 0, err
	}
	names := make([]string, 0, len(files))
	for f := range files {
		names = append(names, f)
	}
	sort.Strings(names)
	written := 0
	for _, f := range names {
		path := filepath.Join(tmp, f)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return 0, err
		}
		var b strings.Builder
		b.WriteString(head)
		if name, ok := unassigned[f]; ok {
			fmt.Fprintf(&b, "# Not assigned to a root module: %s found these objects with the credentials of its own environment.\n", report.Clean(sourceLabel(name, o)))
			fmt.Fprintf(&b, "# Move the blocks into the root module whose provider configuration points at the same account or cluster, or run again with -adopt-root %s=DIR.\n\n", report.Clean(name))
		}
		n, err := report.Import(&b, files[f], reserved)
		if err != nil {
			return 0, err
		}
		written += n
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			return 0, err
		}
	}
	// Checked again: the scan may have taken a while.
	if _, err := os.Lstat(out); err == nil {
		return 0, fmt.Errorf("-out-dir %s already exists: adopt writes a new directory and never overwrites", o.outDir)
	}
	if err := os.Rename(tmp, out); err != nil {
		return 0, err
	}
	done = true
	fmt.Fprintf(stderr, "Wrote %d import blocks to %d files under %s. Next, copy each <root>/imports.tf into its root module and run there:\n  terraform plan -generate-config-out=generated.tf\n", written, len(names), report.Clean(o.outDir))
	for _, f := range names {
		if name, ok := unassigned[f]; ok {
			fmt.Fprintf(stderr, "note: %d objects of %s are in %s, not assigned to a root module: move them into the root module that points at the same account, or pass -adopt-root %s=DIR.\n", len(files[f]), report.Clean(sourceLabel(name, o)), report.Clean(filepath.Join(o.outDir, f)), report.Clean(name))
		}
	}
	return written, nil
}

// splitOutDir splits -out-dir into its parent and the new directory's
// name without Clean: in jump/../imports the link counts before ..
// Both separators count on Windows, and a volume (C:) stays with the parent.
func splitOutDir(dir string) (parent, base string) {
	vol := filepath.VolumeName(dir)
	rest := dir[len(vol):]
	for len(rest) > 1 && os.IsPathSeparator(rest[len(rest)-1]) {
		rest = rest[:len(rest)-1]
	}
	for i := len(rest) - 1; i >= 0; i-- {
		if os.IsPathSeparator(rest[i]) {
			return vol + rest[:i+1], rest[i+1:]
		}
	}
	return vol + ".", rest
}

// sourceLabel names a source in messages as the command line spells it.
func sourceLabel(name string, o *options) string {
	if slices.Contains(o.scans, name) {
		return "-scan " + name
	}
	return name
}
