package tf

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/zclconf/go-cty/cty"
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
	return parseWorkspaces(string(out)), nil
}

// parseWorkspaces reads `terraform workspace list`. Lines that are not a
// workspace name (notices of wrappers or remote backends) are skipped. A
// name is what terraform accepts: unchanged by URL path escaping (so
// "_prod" and "a+b" are names, text with spaces is not).
func parseWorkspaces(out string) []string {
	var ws []string
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		if name := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(sc.Text()), "*")); name != "" && url.PathEscape(name) == name {
			ws = append(ws, name)
		}
	}
	return ws
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
	To string // <type>.<name>, without an instance key
	// ID and Identity are the target object, when given as literals.
	ID       string
	Identity map[string]string
}

var (
	importSchema = &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "import"}}}
	importAttrs  = &hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "to"}, {Name: "id"}, {Name: "identity"}}}
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
			target := importTarget(to.Expr, f.Bytes)
			if target == "" {
				continue
			}
			im := Import{To: target}
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

// resourceAddr matches <type>.<name>.
var resourceAddr = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*\.[A-Za-z_][A-Za-z0-9_-]*$`)

// importTarget returns the <type>.<name> an import block's to points at,
// without its instance key, which may be an expression (each.key).
func importTarget(expr hcl.Expression, src []byte) string {
	if tr, diags := hcl.AbsTraversalForExpr(expr); !diags.HasErrors() && len(tr) >= 2 {
		if name, ok := tr[1].(hcl.TraverseAttr); ok {
			return tr.RootName() + "." + name.Name
		}
	}
	r := expr.Range()
	if r.End.Byte > len(src) {
		return ""
	}
	text := strings.Trim(strings.TrimSpace(string(src[r.Start.Byte:r.End.Byte])), `"`)
	addr, _, _ := strings.Cut(text, "[")
	if !resourceAddr.MatchString(addr) {
		return ""
	}
	return addr
}

// literalMap returns an object of literal scalars as strings, nil when the
// expression is anything else.
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
		switch el.Type() {
		case cty.String:
			out[k] = el.AsString()
		case cty.Number:
			out[k] = el.AsBigFloat().Text('f', -1)
		case cty.Bool:
			out[k] = fmt.Sprint(el.True())
		default:
			return nil
		}
	}
	return out
}
