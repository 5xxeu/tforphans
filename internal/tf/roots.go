package tf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/5xxeu/tforphans/internal/model"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// FindRoots returns the root module directories under dir: directories whose
// configuration has a backend or cloud block, or that keep a local
// terraform.tfstate. Child modules have neither and are skipped, as are
// .terraform, .git and other hidden directories, and vendor and testdata
// trees, which hold copies and fixtures rather than live roots. Local state
// of non-default workspaces (terraform.tfstate.d) marks its parent as a
// root and is not walked into.
//
// With units, a Terragrunt unit (see TerragruntKind) counts as a root too;
// its .terragrunt-cache is hidden and not walked into.
//
// Directories below dir that cannot be read (permission denied), those
// without a backend found where a configuration file cannot be read or
// parsed, and with units a terragrunt.hcl that cannot be read or parsed,
// are returned as unread instead of failing the walk: a tree such as a
// home directory has some, and a root there may manage what is found.
// parents are the terragrunt.hcl files taken for shared configuration (see
// TerragruntKind).
func FindRoots(dir string, units bool) (roots []string, unread []Unread, parents []Parent, err error) {
	var maybe, tg []string            // candidate parents, every directory with a terragrunt.hcl
	includedBy := map[string]string{} // absolute directory of an included terragrunt.hcl: a unit including it
	var nearest []string              // units that include the nearest terragrunt.hcl above
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path != dir && errors.Is(err, fs.ErrPermission) {
				unread = append(unread, Unread{path, "directory could not be read (permission denied)"})
				return filepath.SkipDir
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if path != dir && (strings.HasPrefix(d.Name(), ".") || d.Name() == "vendor" || d.Name() == "testdata" || d.Name() == "terraform.tfstate.d") {
			return filepath.SkipDir
		}
		ok, unsure, err := isRoot(path)
		if err != nil {
			if path != dir && errors.Is(err, fs.ErrPermission) {
				unread = append(unread, Unread{path, "directory could not be read (permission denied)"})
				return filepath.SkipDir
			}
			return err
		}
		if units {
			// Checked even in a terraform root: a broken terragrunt.hcl
			// there must not leave the old terraform state standing in for
			// the unit's.
			kind, err := TerragruntKind(path)
			if err != nil {
				unread = append(unread, Unread{path, err.Error()})
				return nil
			}
			if kind != TerragruntNone {
				tg = append(tg, path)
			}
			if kind == TerragruntUnit {
				dirs, near := terragruntIncludes(path)
				for _, d := range dirs {
					if a := absPath(d); includedBy[a] == "" {
						includedBy[a] = path
					}
				}
				if near {
					nearest = append(nearest, path)
				}
			}
			if kind == TerragruntMaybe && !ok {
				maybe = append(maybe, path)
				return nil
			}
			ok = ok || kind != TerragruntNone
		}
		if ok {
			roots = append(roots, path)
		} else if unsure != "" {
			unread = append(unread, Unread{path, unsure})
		}
		return nil
	})
	for _, u := range nearest {
		best := ""
		for _, d := range tg {
			if a := absPath(d); below(d, u) && len(a) > len(best) {
				best = a
			}
		}
		if best != "" && includedBy[best] == "" {
			includedBy[best] = u
		}
	}
	// A terragrunt.hcl with no terraform block, include or .tf files is a
	// parent only when a unit includes it; otherwise it is a unit (its
	// configuration may be all generated). Taking a parent for a unit costs
	// an unread state (terragrunt fails there); the other way round would
	// lose a unit without a trace.
	for _, m := range maybe {
		if by := includedBy[absPath(m)]; by != "" {
			parents = append(parents, Parent{m, by})
		} else {
			roots = append(roots, m)
		}
	}
	sort.Strings(roots)
	return roots, unread, parents, err
}

// Parent is a terragrunt.hcl FindRoots took for shared configuration, not
// a unit, and a unit that includes it.
type Parent struct {
	Dir, By string
}

// terragruntIncludes returns the directories of the terragrunt.hcl files a
// unit includes by a literal path, and whether it includes the nearest one
// above it (find_in_parent_folders() or find_in_parent_folders("terragrunt.hcl")).
// Other expressions are not followed.
func terragruntIncludes(dir string) (dirs []string, nearest bool) {
	name := filepath.Join(dir, "terragrunt.hcl")
	src, err := os.ReadFile(name)
	if err != nil {
		return nil, false
	}
	f, diags := hclparse.NewParser().ParseHCL(src, name)
	if diags.HasErrors() {
		return nil, false
	}
	body, ok := f.Body.(*hclsyntax.Body)
	if !ok {
		return nil, false
	}
	for _, b := range body.Blocks {
		if b.Type != "include" || b.Body.Attributes["path"] == nil {
			continue
		}
		expr := b.Body.Attributes["path"].Expr
		if call, ok := expr.(*hclsyntax.FunctionCallExpr); ok {
			if call.Name != "find_in_parent_folders" {
				continue
			}
			if len(call.Args) == 0 {
				nearest = true
			} else if v, ok := literal(call.Args[0]); ok && len(call.Args) == 1 && v == "terragrunt.hcl" {
				nearest = true
			}
			continue
		}
		if v, ok := literal(expr); ok && filepath.Base(v) == "terragrunt.hcl" {
			if !filepath.IsAbs(v) {
				v = filepath.Join(dir, v)
			}
			dirs = append(dirs, filepath.Dir(v))
		}
	}
	return dirs, nearest
}

// literal is the value of a string expression without variables or calls.
func literal(expr hclsyntax.Expression) (string, bool) {
	if len(expr.Variables()) > 0 {
		return "", false
	}
	v, diags := expr.Value(nil)
	if diags.HasErrors() || !v.IsKnown() || v.IsNull() || v.Type() != cty.String {
		return "", false
	}
	return v.AsString(), true
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// below reports whether path lies inside dir (not dir itself); both come
// from one walk, so they are relative or absolute alike (dir may be ".").
func below(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Unread is a directory FindRoots could not tell a root or not, and why.
type Unread struct {
	Path, Reason string
}

var (
	terraformSchema = &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "terraform"}}}
	backendSchema   = &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{
		{Type: "backend", LabelNames: []string{"type"}},
		{Type: "cloud"},
	}}
)

// HasConfig reports whether dir holds terraform configuration files.
func HasConfig(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() && !IgnoredFile(n) && (strings.HasSuffix(n, ".tf") || strings.HasSuffix(n, ".tf.json")) {
			return true
		}
	}
	return false
}

// isRoot reports whether dir is a root module. Without a backend found,
// unsure says why that is not known: a configuration file that cannot be
// read or parsed (a broken symlink, a stray "}") may hold the backend
// block, and a root left out silently would look checked.
func isRoot(dir string) (ok bool, unsure string, err error) {
	for _, local := range []string{"terraform.tfstate", "terraform.tfstate.d"} {
		if _, err := os.Stat(filepath.Join(dir, local)); err == nil {
			return true, "", nil
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, "", err
	}
	p := hclparse.NewParser()
	for _, e := range entries {
		name := e.Name()
		var f *hcl.File
		var diags hcl.Diagnostics
		switch {
		case e.IsDir() || IgnoredFile(name):
			continue
		case strings.HasSuffix(name, ".tf"):
			f, diags = p.ParseHCLFile(filepath.Join(dir, name))
		case strings.HasSuffix(name, ".tf.json"):
			f, diags = p.ParseJSONFile(filepath.Join(dir, name))
		default:
			continue
		}
		if diags.HasErrors() && unsure == "" {
			unsure = "configuration could not be read: " + firstError(diags)
		}
		if f == nil {
			continue
		}
		content, _, _ := f.Body.PartialContent(terraformSchema)
		for _, tb := range content.Blocks {
			inner, _, _ := tb.Body.PartialContent(backendSchema)
			if len(inner.Blocks) > 0 {
				return true, "", nil
			}
		}
	}
	return false, unsure, nil
}

// firstError is the first error of diags with its file and line.
func firstError(diags hcl.Diagnostics) string {
	for _, d := range diags {
		if d.Severity != hcl.DiagError {
			continue
		}
		msg := d.Summary
		if d.Detail != "" {
			msg += "; " + d.Detail
		}
		if d.Subject != nil {
			msg = fmt.Sprintf("%s:%d: %s", d.Subject.Filename, d.Subject.Start.Line, msg)
		}
		return msg
	}
	return diags.Error()
}

// Initialized reports whether terraform init has run in the module.
func (r Runner) Initialized() bool {
	fi, err := os.Stat(DataDir(r.Dir))
	return err == nil && fi.IsDir()
}

// DataDir is the module's data directory as terraform run in it sees it:
// TF_DATA_DIR (relative to the module) or .terraform.
func DataDir(dir string) string {
	d := os.Getenv("TF_DATA_DIR")
	if d == "" {
		d = ".terraform"
	}
	if filepath.IsAbs(d) {
		return d
	}
	return filepath.Join(dir, d)
}

// SharedDataDir reports two of the root modules whose data directories
// (TF_DATA_DIR, see DataDir) are one directory: an absolute TF_DATA_DIR, or
// a relative one such as ../shared for sibling roots. Terraform keeps a
// root's backend settings there: with a partial backend configuration
// (backend "s3" {} and -backend-config at init) another root with the same
// block reads that root's state, without an error (checked with the local
// backend). It returns the shared directory and the two roots, or "".
func SharedDataDir(roots []string) (dir, a, b string) {
	type use struct{ root, real string }
	seen := map[string]use{}
	for _, root := range roots {
		// The root first: the OS takes .. in a relative TF_DATA_DIR from
		// the directory a symlink leads to.
		real := resolvePath(root)
		d := resolvePath(DataDir(real))
		if first, ok := seen[d]; ok && first.real != real {
			return d, first.root, root
		}
		seen[d] = use{root, real}
	}
	return "", "", ""
}

// resolvePath makes p absolute and resolves symlinks in the longest part
// of it that exists.
func resolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	rest := ""
	for d := abs; ; d = filepath.Dir(d) {
		if r, err := filepath.EvalSymlinks(d); err == nil {
			return filepath.Join(r, rest)
		}
		if filepath.Dir(d) == d {
			return abs
		}
		rest = filepath.Join(filepath.Base(d), rest)
	}
}

// Init runs terraform init without prompting, one root at a time.
func (r Runner) Init(ctx context.Context) error {
	unlock, err := lockInit(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	args := []string{"init", "-input=false", "-no-color"}
	if _, stderr, err := r.run(ctx, args...); err != nil {
		return cmdError(args, stderr, err)
	}
	return nil
}

// Workspaces lists the module's workspaces. Backends without workspace
// support (http, for example GitLab managed state) have a single state,
// reported as one workspace with an empty name.
func (r Runner) Workspaces(ctx context.Context) ([]string, error) {
	args := []string{"workspace", "list", "-no-color"}
	out, stderr, err := r.run(ctx, args...)
	if err != nil {
		if strings.Contains(string(stderr)+string(out), "workspaces not supported") {
			return []string{""}, nil
		}
		return nil, cmdError(args, stderr, err)
	}
	return parseWorkspaces(string(out))
}

// workspaceLine is a line of the list: "* name" for the selected
// workspace, "  name" for the others.
var workspaceLine = regexp.MustCompile(`^(\*| ) (\S+)$`)

// parseWorkspaces reads `terraform workspace list`. Backends and wrappers
// print other text to the same stream, such as warnings that -no-color
// renders without their frame, so a word wrapped onto its own line
// ("instead.") must never pass for a workspace: pulling the state of a
// workspace that does not exist makes some backends (S3) write an empty
// one. Only the block of list lines that holds the selected workspace
// counts, and a name is what terraform accepts: unchanged by URL path
// escaping (so "_prod" and "a+b" are names, text with spaces is not).
func parseWorkspaces(out string) ([]string, error) {
	var block, ws []string
	selected := 0
	lines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	for _, line := range append(lines, "") {
		m := workspaceLine.FindStringSubmatch(line)
		if m != nil && url.PathEscape(m[2]) == m[2] {
			block = append(block, m[0])
			continue
		}
		// The end of a block of list lines.
		n := 0
		for _, l := range block {
			if l[0] == '*' {
				n++
			}
		}
		if n > 0 {
			if ws != nil {
				return nil, &model.ResponseError{Err: errors.New("unexpected output of terraform workspace list: more than one list")}
			}
			for _, l := range block {
				ws = append(ws, l[2:])
			}
			selected = n
		}
		block = nil
	}
	if selected != 1 {
		return nil, &model.ResponseError{Err: errors.New("unexpected output of terraform workspace list: no single selected workspace (* name)")}
	}
	return ws, nil
}

// UserList is one of the user's own list blocks.
type UserList struct {
	// Configuration is the exact source bytes of the whole block.
	Configuration string
	// Static means expressions outside provider depend only on literals,
	// each.key, count.index, or each.value with a reference-free for_each.
	// Function calls are excluded, including in for_each.
	// Other dependencies can change the listing scope without changing its text.
	Static bool
	// Provider is the source text of the provider argument, such as "aws"
	// or "aws.other_account".
	Provider string
	// Limit is the block's limit: 100 (Terraform's default) when unset, 0
	// when it is an expression that cannot be read without evaluation.
	Limit int
}

// ProviderRef returns the provider configuration used by a user list block.
func (l UserList) ProviderRef(typ string) string {
	if l.Provider != "" {
		return l.Provider
	}
	local, _, _ := strings.Cut(typ, "_")
	return local
}

// DefaultListLimit is Terraform's limit for list blocks without one.
const DefaultListLimit = 100

// UserLists returns the user's list blocks (list.<type>.<label>) in the
// module's *.tfquery.hcl files.
func UserLists(dir string) (map[string]UserList, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.tfquery.hcl"))
	if err != nil {
		return nil, err
	}
	out := map[string]UserList{}
	p := hclparse.NewParser()
	for _, path := range files {
		if name := filepath.Base(path); strings.HasPrefix(name, "zz_tforphans_") || IgnoredFile(name) {
			continue
		}
		f, diags := p.ParseHCLFile(path)
		if diags.HasErrors() {
			return nil, fmt.Errorf("%s: %s", path, diags.Error())
		}
		content, _, _ := f.Body.PartialContent(listSchema)
		for _, b := range content.Blocks {
			inner, _, _ := b.Body.PartialContent(listAttrs)
			body := b.Body.(*hclsyntax.Body)
			l := UserList{Limit: DefaultListLimit, Static: staticUserList(body), Configuration: string(f.Bytes[b.DefRange.Start.Byte:body.EndRange.End.Byte])}
			if attr, ok := inner.Attributes["provider"]; ok {
				r := attr.Expr.Range()
				l.Provider = strings.TrimSpace(string(f.Bytes[r.Start.Byte:r.End.Byte]))
			}
			if attr, ok := inner.Attributes["limit"]; ok {
				l.Limit = 0
				if v, diags := attr.Expr.Value(nil); !diags.HasErrors() && v.Type() == cty.Number && v.IsKnown() && !v.IsNull() {
					if n, acc := v.AsBigFloat().Int64(); acc == big.Exact && n > 0 {
						l.Limit = int(n)
					}
				}
			}
			out["list."+b.Labels[0]+"."+b.Labels[1]] = l
		}
	}
	return out, nil
}

// staticUserList rejects function calls and references outside the instance key
// or a value from a reference-free, function-free for_each. The provider argument
// is checked separately when reading identity.
func staticUserList(body *hclsyntax.Body) bool {
	hasCalls := func(expr hclsyntax.Expression) bool {
		found := false
		// No diagnostics: the callback always returns nil.
		_ = hclsyntax.VisitAll(expr, func(node hclsyntax.Node) hcl.Diagnostics {
			if _, ok := node.(*hclsyntax.FunctionCallExpr); ok {
				found = true
			}
			return nil
		})
		return found
	}
	forEachStatic := false
	if attr := body.Attributes["for_each"]; attr != nil {
		forEachStatic = len(attr.Expr.Variables()) == 0 && !hasCalls(attr.Expr)
	}
	var staticBody func(*hclsyntax.Body, bool) bool
	staticBody = func(body *hclsyntax.Body, top bool) bool {
		for name, attr := range body.Attributes {
			if top && name == "provider" {
				continue
			}
			if hasCalls(attr.Expr) {
				return false
			}
			for _, ref := range attr.Expr.Variables() {
				if len(ref) < 2 {
					return false
				}
				field, ok := ref[1].(hcl.TraverseAttr)
				if !ok {
					return false
				}
				switch {
				case ref.RootName() == "each" && field.Name == "key" && len(ref) == 2:
				case ref.RootName() == "count" && field.Name == "index" && len(ref) == 2:
				case ref.RootName() == "each" && field.Name == "value" && forEachStatic:
				default:
					return false
				}
			}
		}
		for _, block := range body.Blocks {
			if !staticBody(block.Body, false) {
				return false
			}
		}
		return true
	}
	return staticBody(body, true)
}

var (
	listSchema = &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "list", LabelNames: []string{"type", "name"}}}}
	listAttrs  = &hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "provider"}, {Name: "limit"}}}
)

// Import is an import block of the module's own configuration.
type Import struct {
	Dir string // the root module
	// To is the <type>.<name> of a target in the root module itself, ""
	// when the block imports into a child module.
	To   string
	Type string // the target's resource type
	// Provider is the block's provider argument (vault.other), "" for the
	// type's default provider configuration.
	Provider string
	// ID and Identity are the target object, when given as literals;
	// Identity values are in model.Canonical form.
	ID       string
	Identity map[string]string
}

var (
	importSchema = &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "import"}}}
	importAttrs  = &hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "to"}, {Name: "id"}, {Name: "identity"}, {Name: "provider"}}}
)

// ConfigImports returns the import blocks in the module's .tf and .tf.json
// files, so that adopt neither imports into an address an earlier run
// already used nor writes a second block for the same object.
func ConfigImports(dir string) ([]Import, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Import
	p := hclparse.NewParser()
	for _, e := range entries {
		var f *hcl.File
		path := filepath.Join(dir, e.Name())
		switch {
		case e.IsDir() || IgnoredFile(e.Name()):
			continue
		case strings.HasSuffix(e.Name(), ".tf"):
			f, _ = p.ParseHCLFile(path)
		case strings.HasSuffix(e.Name(), ".tf.json"):
			f, _ = p.ParseJSONFile(path)
		}
		// Files that do not parse are left for terraform to report.
		if f == nil {
			continue
		}
		content, _, _ := f.Body.PartialContent(importSchema)
		for _, b := range content.Blocks {
			attrs, _, _ := b.Body.PartialContent(importAttrs)
			to, ok := attrs.Attributes["to"]
			if !ok {
				continue
			}
			module, typ, name := importTarget(to.Expr, f.Bytes)
			if typ == "" {
				continue
			}
			im := Import{Dir: dir, Type: typ}
			if !module {
				im.To = typ + "." + name
			}
			if a, ok := attrs.Attributes["provider"]; ok {
				im.Provider = providerRef(a.Expr)
			}
			if a, ok := attrs.Attributes["id"]; ok {
				if v, diags := a.Expr.Value(nil); !diags.HasErrors() && v.Type() == cty.String && v.IsKnown() && !v.IsNull() {
					im.ID = v.AsString()
				}
			}
			if a, ok := attrs.Attributes["identity"]; ok {
				im.Identity = literalMap(a.Expr)
			}
			out = append(out, im)
		}
	}
	return out, nil
}

// ident matches a Terraform identifier.
var ident = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

// importTarget returns the resource type and name an import block's to
// points at, and whether that is inside a child module. Instance keys of
// the resource and of module calls are dropped: they may be expressions
// (each.key), which are read from the source text.
func importTarget(expr hcl.Expression, src []byte) (module bool, typ, name string) {
	var parts []string
	if tr, diags := hcl.AbsTraversalForExpr(expr); !diags.HasErrors() {
		for _, step := range tr {
			switch s := step.(type) {
			case hcl.TraverseRoot:
				parts = append(parts, s.Name)
			case hcl.TraverseAttr:
				parts = append(parts, s.Name)
			case hcl.TraverseIndex:
				// An instance key.
			default:
				return false, "", ""
			}
		}
	} else {
		r := expr.Range()
		if r.End.Byte > len(src) {
			return false, "", ""
		}
		text := strings.Trim(strings.TrimSpace(string(src[r.Start.Byte:r.End.Byte])), `"`)
		parts = strings.Split(dropIndexes(text), ".")
	}
	for len(parts) > 2 && parts[0] == "module" {
		if !ident.MatchString(parts[1]) {
			return false, "", ""
		}
		parts, module = parts[2:], true
	}
	if len(parts) != 2 || !ident.MatchString(parts[0]) || !ident.MatchString(parts[1]) {
		return false, "", ""
	}
	return module, parts[0], parts[1]
}

// dropIndexes removes the [...] keys from an address, nested brackets and
// quoted strings included.
func dropIndexes(s string) string {
	var b strings.Builder
	depth, quoted := 0, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quoted:
			if c == '\\' {
				i++
			} else if c == '"' {
				quoted = false
			}
		case c == '"' && depth > 0:
			quoted = true
		case c == '[':
			depth++
		case c == ']' && depth > 0:
			depth--
		case depth == 0:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// providerRef returns an import block's provider reference (vault or
// vault.other), "" when it is not one.
func providerRef(expr hcl.Expression) string {
	tr, diags := hcl.AbsTraversalForExpr(expr)
	if diags.HasErrors() {
		return ""
	}
	ref := tr.RootName()
	if len(tr) == 2 {
		if a, ok := tr[1].(hcl.TraverseAttr); ok {
			ref += "." + a.Name
		}
	}
	return ref
}

// literalMap returns an object of literal values, each in model.Canonical
// form (lists and objects as compact JSON), nil when the expression is not
// a wholly known object.
func literalMap(expr hcl.Expression) map[string]string {
	v, diags := expr.Value(nil)
	if diags.HasErrors() || !v.IsWhollyKnown() || v.IsNull() || !(v.Type().IsObjectType() || v.Type().IsMapType()) {
		return nil
	}
	out := map[string]string{}
	for k, el := range v.AsValueMap() {
		if el.IsNull() {
			continue
		}
		raw, err := ctyjson.Marshal(el, el.Type())
		if err != nil {
			return nil
		}
		var val any
		if err := model.DecodeJSON(raw, &val); err != nil {
			return nil
		}
		c, ok := model.Canonical(val)
		if !ok {
			return nil
		}
		out[k] = c
	}
	return out
}

var (
	providerSchema = &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "provider", LabelNames: []string{"name"}}}}
	aliasSchema    = &hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "alias"}}}
)

// IgnoredFile reports whether terraform skips a configuration file: hidden
// files and editor backups (#name#, name~). Reading them would let a file
// terraform never loads change what tforphans concludes.
func IgnoredFile(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasSuffix(name, "~") || strings.HasPrefix(name, "#") && strings.HasSuffix(name, "#")
}

// ProviderConfigs returns, per local name, the root module's default
// provider configuration (the provider block without an alias) as its HCL
// tokens, so that only layout and comments are ignored; a provider without
// one is absent, which is the same configuration as an empty block. Two
// names with equal text configure the same account and project. A block
// that cannot be compared (in a .tf.json file, or changed by an override
// file) gets a text of its own that equals nothing.
func ProviderConfigs(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	overridden := map[string]string{}
	p := hclparse.NewParser()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || IgnoredFile(name) || strings.HasPrefix(name, "zz_tforphans_") || !(strings.HasSuffix(name, ".tf") || strings.HasSuffix(name, ".tf.json")) {
			continue
		}
		path := filepath.Join(dir, name)
		var f *hcl.File
		if strings.HasSuffix(name, ".tf") {
			f, _ = p.ParseHCLFile(path)
		} else {
			f, _ = p.ParseJSONFile(path)
		}
		if f == nil {
			continue
		}
		base := strings.TrimSuffix(strings.TrimSuffix(name, ".json"), ".tf")
		override := base == "override" || strings.HasSuffix(base, "_override")
		content, _, _ := f.Body.PartialContent(providerSchema)
		for i, b := range content.Blocks {
			attrs, _, _ := b.Body.PartialContent(aliasSchema)
			// Aliased configurations under local.alias: generated blocks
			// list through each of them too.
			key := b.Labels[0]
			if a, aliased := attrs.Attributes["alias"]; aliased {
				v, diags := a.Expr.Value(nil)
				if diags.HasErrors() || v.IsNull() || !v.IsKnown() || v.Type() != cty.String {
					continue
				}
				key += "." + v.AsString()
			}
			unique := fmt.Sprintf("unique %s %d", path, i)
			if override {
				overridden[key] = unique
				continue
			}
			text := unique
			if body, ok := b.Body.(*hclsyntax.Body); ok {
				// The body's range includes its braces.
				r := body.SrcRange
				text = configTokens(f.Bytes[r.Start.Byte+1 : r.End.Byte-1])
			}
			out[key] = text
		}
	}
	maps.Copy(out, overridden)
	return out, nil
}

// configTokens renders HCL source as its tokens without newlines and
// comments; string contents stay as they are.
func configTokens(src []byte) string {
	tokens, _ := hclsyntax.LexConfig(src, "", hcl.InitialPos)
	var parts []string
	for _, t := range tokens {
		switch t.Type {
		case hclsyntax.TokenNewline, hclsyntax.TokenComment, hclsyntax.TokenEOF:
			continue
		}
		parts = append(parts, string(t.Bytes))
	}
	return strings.Join(parts, "\x00")
}

// QueryVersion checks that terraform has the query command (1.14). Older
// versions show no list resources in their provider schemas, so without
// this check a run would blame the providers. A version that cannot be
// read is let through: the query itself then says what is wrong.
func (r Runner) QueryVersion(ctx context.Context) error {
	out, _, err := r.run(ctx, "version", "-json")
	if err != nil {
		// A version that cannot be told is no reason to stop; running out
		// of time is (-source-timeout must see it).
		return ctx.Err()
	}
	var v struct {
		Version string `json:"terraform_version"`
	}
	if json.Unmarshal(out, &v) != nil {
		return nil
	}
	return CheckQueryVersion(r.Bin, v.Version)
}

// CheckQueryVersion checks whether the version supports list resources.
func CheckQueryVersion(bin, version string) error {
	major, minor, ok := majorMinor(version)
	if !ok || major > 1 || major == 1 && minor >= 14 {
		return nil
	}
	return fmt.Errorf("%s %s has no query command: list resources need Terraform 1.14 or newer (OpenTofu has none yet). Upgrade, or pass -no-query and list objects with -scan or -plugin instead", filepath.Base(bin), version)
}

// majorMinor reads "1.13.5" or "1.14.0-beta1".
func majorMinor(v string) (major, minor int, ok bool) {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(strings.SplitN(parts[1], "-", 2)[0])
	return major, minor, err1 == nil && err2 == nil
}

// Kinds of a directory's terragrunt.hcl.
const (
	TerragruntNone  = iota // no terragrunt.hcl
	TerragruntUnit         // deploys a module
	TerragruntMaybe        // a unit, or a parent of units
)

// TerragruntKind tells what a directory's terragrunt.hcl is. A terraform
// block (source), an include of a parent, or .tf files next to it make a
// unit. Without them it may still be one (remote_state with generate, in a
// fresh checkout) or a parent such as a legacy top-level terragrunt.hcl:
// FindRoots takes it for a parent only when a unit includes it. A
// terragrunt.hcl that exists but cannot be read or parsed (a dangling link
// too) is an error, not a missing unit.
func TerragruntKind(dir string) (int, error) {
	name := filepath.Join(dir, "terragrunt.hcl")
	if _, err := os.Lstat(name); errors.Is(err, fs.ErrNotExist) {
		return TerragruntNone, nil
	}
	src, err := os.ReadFile(name)
	if err != nil {
		return TerragruntNone, fmt.Errorf("terragrunt.hcl could not be read: %w", err)
	}
	f, diags := hclparse.NewParser().ParseHCL(src, name)
	if diags.HasErrors() {
		return TerragruntNone, fmt.Errorf("terragrunt.hcl could not be parsed: %s", diags.Errs()[0].Error())
	}
	if body, ok := f.Body.(*hclsyntax.Body); ok {
		for _, b := range body.Blocks {
			if b.Type == "terraform" || b.Type == "include" {
				return TerragruntUnit, nil
			}
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return TerragruntNone, fmt.Errorf("terragrunt.hcl: directory could not be read: %w", err)
	}
	for _, e := range entries {
		if n := e.Name(); !e.IsDir() && (strings.HasSuffix(n, ".tf") || strings.HasSuffix(n, ".tf.json")) {
			return TerragruntUnit, nil
		}
	}
	return TerragruntMaybe, nil
}

// TerragruntStatePull reads the state of a Terragrunt unit with terragrunt,
// which renders the backend and runs terraform (init included: it fetches
// the module and providers into .terragrunt-cache). tfBin is the terraform
// it runs, through tforphans itself, so that terragrunt's inits take turns
// with all others (see initTurn and wrapTerragrunt).
func TerragruntStatePull(ctx context.Context, bin, tfBin, dir string, maxSize int64) ([]byte, error) {
	wrap := []string{"TG_TF_PATH=" + tfBin, "TERRAGRUNT_TFPATH=" + tfBin}
	if wrapTerragrunt {
		var done func()
		var err error
		if wrap, done, err = wrapperEnv(ctx, tfBin); err != nil {
			return nil, err
		}
		defer done()
	} else {
		unlock, err := lockInit(ctx)
		if err != nil {
			return nil, err
		}
		defer func() {
			// terragrunt is killed, a terraform init below it may not be.
			if ctx.Err() != nil {
				breakTurn(fmt.Errorf("terraform init not run: terragrunt in %s was stopped (%v), and a terraform it started may still be installing providers", dir, ctx.Err()))
			}
			unlock()
		}()
	}
	r := Runner{Bin: bin, Dir: dir, Env: append([]string{
		// Current and older names of the same settings.
		"TG_NON_INTERACTIVE=true", "TERRAGRUNT_NON_INTERACTIVE=true",
		"TG_LOG_LEVEL=error", "TERRAGRUNT_LOG_LEVEL=error",
		"TG_NO_COLOR=true", "TERRAGRUNT_NO_COLOR=true",
	}, wrap...), MaxStateSize: maxSize}
	// "run --" needs Terragrunt 0.73 or newer.
	args := []string{"run", "--", "state", "pull"}
	out, stderr, err := r.runLimited(ctx, r.stateSize(), args...)
	if err != nil {
		return nil, cmdError(append([]string{"(terragrunt)"}, args...), stderr, err)
	}
	return out, nil
}
