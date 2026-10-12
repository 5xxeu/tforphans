package tf

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
func FindRoots(dir string) ([]string, error) {
	var roots []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if path != dir && (strings.HasPrefix(d.Name(), ".") || d.Name() == "vendor" || d.Name() == "testdata" || d.Name() == "terraform.tfstate.d") {
			return filepath.SkipDir
		}
		ok, err := isRoot(path)
		if err != nil {
			return err
		}
		if ok {
			roots = append(roots, path)
		}
		return nil
	})
	sort.Strings(roots)
	return roots, err
}

var (
	terraformSchema = &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "terraform"}}}
	backendSchema   = &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{
		{Type: "backend", LabelNames: []string{"type"}},
		{Type: "cloud"},
	}}
)

func isRoot(dir string) (bool, error) {
	for _, local := range []string{"terraform.tfstate", "terraform.tfstate.d"} {
		if _, err := os.Stat(filepath.Join(dir, local)); err == nil {
			return true, nil
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	p := hclparse.NewParser()
	for _, e := range entries {
		name := e.Name()
		var f *hcl.File
		switch {
		case e.IsDir():
			continue
		case strings.HasSuffix(name, ".tf"):
			f, _ = p.ParseHCLFile(filepath.Join(dir, name))
		case strings.HasSuffix(name, ".tf.json"):
			f, _ = p.ParseJSONFile(filepath.Join(dir, name))
		default:
			continue
		}
		// Files that do not parse are left for terraform to report.
		if f == nil {
			continue
		}
		content, _, _ := f.Body.PartialContent(terraformSchema)
		for _, tb := range content.Blocks {
			inner, _, _ := tb.Body.PartialContent(backendSchema)
			if len(inner.Blocks) > 0 {
				return true, nil
			}
		}
	}
	return false, nil
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

// Init runs terraform init without prompting.
func (r Runner) Init(ctx context.Context) error {
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
				return nil, errors.New("unexpected output of terraform workspace list: more than one list")
			}
			for _, l := range block {
				ws = append(ws, l[2:])
			}
			selected = n
		}
		block = nil
	}
	if selected != 1 {
		return nil, errors.New("unexpected output of terraform workspace list: no single selected workspace (* name)")
	}
	return ws, nil
}

// UserList is one of the user's own list blocks.
type UserList struct {
	// Provider is the source text of the provider argument, such as "aws"
	// or "aws.other_account".
	Provider string
	// Limit is the block's limit: 100 (Terraform's default) when unset, 0
	// when it is an expression that cannot be read without evaluation.
	Limit int
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
		if strings.HasPrefix(filepath.Base(path), "zz_tforphans_") {
			continue
		}
		f, diags := p.ParseHCLFile(path)
		if diags.HasErrors() {
			return nil, fmt.Errorf("%s: %s", path, diags.Error())
		}
		content, _, _ := f.Body.PartialContent(listSchema)
		for _, b := range content.Blocks {
			inner, _, _ := b.Body.PartialContent(listAttrs)
			l := UserList{Limit: DefaultListLimit}
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
		case e.IsDir():
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

// ProviderConfigs returns, per local name, the source text of the root
// module's default provider configuration (the provider block without an
// alias), with whitespace collapsed; a provider without one is absent,
// which is the same configuration as an empty block. Two names with equal
// text configure the same account and project. A block in a .tf.json file
// gets a text of its own that equals nothing, since it cannot be compared.
func ProviderConfigs(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	p := hclparse.NewParser()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, "zz_tforphans_") || !(strings.HasSuffix(name, ".tf") || strings.HasSuffix(name, ".tf.json")) {
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
		content, _, _ := f.Body.PartialContent(providerSchema)
		for i, b := range content.Blocks {
			attrs, _, _ := b.Body.PartialContent(aliasSchema)
			if _, aliased := attrs.Attributes["alias"]; aliased {
				continue
			}
			text := fmt.Sprintf("json %s %d", path, i)
			if body, ok := b.Body.(*hclsyntax.Body); ok {
				r := body.SrcRange
				text = strings.Join(strings.Fields(string(f.Bytes[r.Start.Byte:r.End.Byte])), " ")
				text = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, "{"), "}"))
			}
			out[b.Labels[0]] = text
		}
	}
	return out, nil
}
