// Package dsource lists live objects for providers without list resources
// through their plural data sources (cloudflare_dns_records for
// cloudflare_dns_record, and so on), so the provider's own API code and
// authentication do the work.
//
// A temporary root module with one data block per plural data source is
// read with `terraform apply -refresh-only` (data sources only read) and its
// local state is parsed. The provider version comes from the user's root
// module when one is given: its lock file is copied and its installed
// provider is used as the plugin directory.
package dsource

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/5xxeu/tforphans/internal/glob"
	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/tf"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// Scope is an argument that places a data source in a parent object, such
// as zone_id, filled from the parent's own plural data source.
type Scope struct {
	Arg    string // argument name, e.g. zone_id
	Parent string // plural data source listing the parents, e.g. cloudflare_zones
	// Prefix is the scope's word in import IDs of resources that can live
	// in more than one scope ("zones", "accounts").
	Prefix string
}

// Adapter describes the conventions of one provider.
type Adapter struct {
	Name     string // -scan name and report source
	Provider string // provider address
	Target   string // what credentials point to, "account" or "cluster"
	Scopes   []Scope
	// Skip maps resource types that are never reported to the reason.
	Skip map[string]string
	// AltTypes maps resource types to older or alternative type names.
	AltTypes map[string][]string
	// Keep reports whether a listed object is reported; nil keeps all.
	Keep func(resourceType string, item map[string]any) bool
	// AltIdentity returns identities for AltTypes that key the object
	// differently; nil means AltTypes share the identity.
	AltIdentity func(resourceType string, item map[string]any) map[string]map[string]any
	// Lists describes data sources, by type, whose objects are not
	// result[] with an id.
	Lists map[string]ListSpec
	// NotApplicable recognises errors meaning that a data source does not
	// take the scope it was tried with (it has both account_id and zone_id,
	// but the API serves only one of them).
	NotApplicable func(message string) bool
	// ImportID overrides the "<scope>/<id>" import ID convention.
	ImportID func(resourceType string, item map[string]any, id string) string
	// Context lists data sources read along with the listings, not
	// listed themselves; their attributes go to Finish.
	Context []string
	// Finish adjusts the listed objects using the context data sources
	// (missing from context when they could not be read).
	Finish func(resources []model.Resource, context map[string]map[string]any)
	// Prepare derives more Skip and AltTypes entries from the schema, such
	// as pairs of renamed resource types. deprecated holds every resource
	// type with its deprecation flag; hasPlural tells which have a plural
	// data source.
	Prepare func(deprecated map[string]bool, hasPlural func(string) bool) (skip map[string]string, alt map[string][]string)
}

// ListSpec says where a plural data source keeps its objects.
type ListSpec struct {
	// Items is the attribute holding a list of objects (default "result").
	Items string
	// IDs is, instead, an attribute holding a list of IDs; sibling lists of
	// the same length (names, flags) are merged into each object.
	IDs string
	// Field is the object field holding the ID (default "id").
	Field string
	// Key is the attribute the ID is matched against in state (default "id").
	Key string
}

func (a Adapter) spec(dsType string) ListSpec {
	l := a.Lists[dsType]
	if l.Items == "" && l.IDs == "" {
		l.Items = "result"
	}
	if l.Field == "" {
		l.Field = "id"
	}
	if l.Key == "" {
		l.Key = "id"
	}
	return l
}

// prepared returns a copy of a with the entries of Prepare merged in.
func (a Adapter) prepared(s *schema) Adapter {
	if a.Prepare == nil {
		return a
	}
	deprecated := map[string]bool{}
	for t, rs := range s.Resources {
		deprecated[t] = rs.Block.Deprecated
	}
	skip, alt := a.Prepare(deprecated, func(t string) bool { return plural(s, t) != "" })
	merged := a
	merged.Skip = maps.Clone(a.Skip)
	if merged.Skip == nil {
		merged.Skip = map[string]string{}
	}
	maps.Copy(merged.Skip, skip)
	merged.AltTypes = maps.Clone(a.AltTypes)
	if merged.AltTypes == nil {
		merged.AltTypes = map[string][]string{}
	}
	for t, types := range alt {
		merged.AltTypes[t] = append(slices.Clone(merged.AltTypes[t]), types...)
	}
	return merged
}

// Options are the per-run settings.
type Options struct {
	TerraformBin string
	// VersionRoot is a root module whose lock file and installed provider
	// fix the provider version; empty uses the newest version.
	VersionRoot string
	Include     glob.List
	Exclude     glob.List
	// AllowUnpinned lets the scan install the newest provider release when
	// no root pins a version. Off by default: a release that nobody chose
	// would run with the user's cloud credentials.
	AllowUnpinned bool
}

// Result of a scan.
type Result struct {
	Resources []model.Resource
	Listed    int          // resource types listed
	Skipped   []tf.Skip    // resource types without a usable data source
	Failed    []tf.Failure // resource types whose data source failed
}

// maxItems is passed to data sources that take max_items; a variable so
// that tests can reach it.
var maxItems = 1000000

// Run lists every resource type that has a usable plural data source.
func Run(ctx context.Context, a Adapter, opt Options) (*Result, error) {
	if opt.VersionRoot == "" && !opt.AllowUnpinned {
		return nil, fmt.Errorf("%s: no root module pins %s in .terraform.lock.hcl; pass one (as an argument or under -state-roots), or -allow-unpinned to use the newest release", a.Name, a.Provider)
	}
	dir, err := os.MkdirTemp("", "tforphans-"+a.Name+"-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	r := tf.Runner{Bin: opt.TerraformBin, Dir: dir, Isolated: true}
	if err := initRoot(ctx, &r, a, opt.VersionRoot); err != nil {
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
	a = a.prepared(sch)
	p := buildPlan(sch, a, opt)
	res := &Result{Skipped: p.skipped}
	if len(p.listings) == 0 {
		return res, nil
	}

	all := p.blocks()
	blocks := all
	var failures []failure
	for len(blocks) > 0 {
		if err := os.WriteFile(filepath.Join(dir, "main.tf"), renderBlocks(blocks), 0o644); err != nil {
			return nil, err
		}
		out, stderr, runErr := r.Exec(ctx, "apply", "-refresh-only", "-auto-approve", "-input=false", "-no-color", "-json", "-lock=false")
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if runErr == nil {
			break
		}
		errs, fatal := applyErrors(out)
		keep, failed, changed := nextRound(blocks, errs)
		if len(fatal) > 0 || !changed {
			msg := strings.Join(fatal, "\n")
			if msg == "" {
				msg = strings.TrimSpace(string(stderr))
			}
			return nil, fmt.Errorf("%s: reading data sources: %v\n%s", a.Name, runErr, msg)
		}
		failures = append(failures, failed...)
		blocks = keep
	}

	var stateData []byte
	if len(blocks) > 0 {
		var err error
		if stateData, err = os.ReadFile(filepath.Join(dir, "terraform.tfstate")); err != nil {
			return nil, fmt.Errorf("%s: read data sources: %w", a.Name, err)
		}
	}
	// A type counts as listed when one of its blocks read something: an
	// unscoped block, or a scoped one with no failed instance or at least
	// one instance that worked (one whose every zone failed listed nothing).
	instances, err := instanceCounts(stateData)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", a.Name, err)
	}
	listed := map[string]bool{}
	for _, b := range blocks {
		if b.scope != nil && len(b.exclude) > 0 && instances[b.addr()] == 0 {
			continue
		}
		for _, l := range b.listings {
			listed[l.resourceType] = true
		}
	}
	res.Listed = len(listed)
	// Every failed scope is a gap, even when another scope listed the type:
	// a 403 on the account must not hide behind a working zone listing.
	// Only an error the adapter knows to mean "this data source does not
	// take this scope" is dropped, and only when another scope worked.
	byAddr := map[string]block{}
	for _, b := range all {
		byAddr[b.addr()] = b
	}
	for _, f := range failures {
		if len(byAddr[f.block].listings) == 0 {
			// A context data source: the adapter treats it as missing.
			res.Failed = append(res.Failed, tf.Failure{Address: f.address, Type: byAddr[f.block].dsType, Message: f.message})
			continue
		}
		for _, l := range byAddr[f.block].listings {
			if listed[l.resourceType] && a.NotApplicable != nil && a.NotApplicable(f.message) {
				continue
			}
			res.Failed = append(res.Failed, tf.Failure{Address: f.address, Type: l.resourceType, Message: f.message})
		}
	}
	if len(blocks) == 0 {
		return res, nil
	}
	cut, err := truncated(stateData, blocks, a)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", a.Name, err)
	}
	res.Failed = append(res.Failed, cut...)

	res.Resources, err = extract(stateData, blocks, a, sch)
	if err != nil {
		return nil, err
	}
	if a.Finish != nil {
		ctxAttrs, err := contextAttrs(stateData, p.context)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", a.Name, err)
		}
		a.Finish(res.Resources, ctxAttrs)
	}
	return res, err
}

func initRoot(ctx context.Context, r *tf.Runner, a Adapter, versionRoot string) error {
	local := a.Name
	versions := fmt.Sprintf("terraform {\n  required_providers {\n    %s = { source = %s }\n  }\n}\n", local, tf.HCLString(a.Provider))
	if err := os.WriteFile(filepath.Join(r.Dir, "versions.tf"), []byte(versions), 0o644); err != nil {
		return err
	}
	args := []string{"init", "-input=false", "-no-color", "-backend=false"}
	if versionRoot != "" {
		// Only the provider's own lock entry is copied, and init may not
		// change it: terraform then installs exactly that version, checked
		// against its hashes, or fails.
		entry, err := lockEntry(versionRoot, a.Provider)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(r.Dir, ".terraform.lock.hcl"), entry, 0o644); err != nil {
			return err
		}
		args = append(args, "-lockfile=readonly")
		plugins, err := filepath.Abs(filepath.Join(tf.DataDir(versionRoot), "providers"))
		if err != nil {
			return err
		}
		if fi, err := os.Stat(filepath.Join(plugins, a.Provider)); err == nil && fi.IsDir() {
			args = append(args, "-plugin-dir="+plugins)
		}
	}
	if os.Getenv("TF_PLUGIN_CACHE_DIR") == "" {
		if cache, err := os.UserCacheDir(); err == nil {
			cache = filepath.Join(cache, "tforphans", "plugins")
			if err := os.MkdirAll(cache, 0o755); err == nil {
				r.Env = append(r.Env, "TF_PLUGIN_CACHE_DIR="+cache)
			}
		}
	}
	_, _, err := r.Exec(ctx, args...)
	return err
}

// VersionRoot picks the first root whose lock file pins the provider.
func VersionRoot(roots []string, provider string) string {
	for _, root := range roots {
		if _, err := lockEntry(root, provider); err == nil {
			return root
		}
	}
	return ""
}

// lockEntry returns the provider block of a root's .terraform.lock.hcl. It
// has to be a real block with a version and hashes; text elsewhere (a
// comment) does not count.
func lockEntry(root, provider string) ([]byte, error) {
	path := filepath.Join(root, ".terraform.lock.hcl")
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, diags := hclsyntax.ParseConfig(src, path, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, fmt.Errorf("%s: %s", path, diags.Error())
	}
	body, ok := f.Body.(*hclsyntax.Body)
	if !ok {
		return nil, fmt.Errorf("%s: unexpected syntax", path)
	}
	for _, b := range body.Blocks {
		if b.Type != "provider" || len(b.Labels) != 1 || b.Labels[0] != provider {
			continue
		}
		if b.Body.Attributes["version"] == nil || b.Body.Attributes["hashes"] == nil {
			return nil, fmt.Errorf("%s: %s has no version or hashes", path, provider)
		}
		r := b.Range()
		return append(src[r.Start.Byte:r.End.Byte:r.End.Byte], '\n'), nil
	}
	return nil, fmt.Errorf("%s does not pin %s", path, provider)
}

// attr is a schema attribute. Object lists come either as NestedType
// (framework providers) or as a cty Type such as ["list",["object",{...}]].
type attr struct {
	Required   bool            `json:"required"`
	Optional   bool            `json:"optional"`
	Type       json.RawMessage `json:"type"`
	NestedType *struct {
		Attributes map[string]attr `json:"attributes"`
	} `json:"nested_type"`
}

// hasField reports whether the attribute is a list of objects with field.
func (at attr) hasField(field string) bool {
	if at.NestedType != nil {
		_, ok := at.NestedType.Attributes[field]
		return ok
	}
	var t []json.RawMessage
	if json.Unmarshal(at.Type, &t) != nil || len(t) != 2 {
		return false
	}
	var obj []json.RawMessage
	if json.Unmarshal(t[1], &obj) != nil || len(obj) != 2 {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(obj[1], &fields) != nil {
		return false
	}
	_, ok := fields[field]
	return ok
}

type blockSchema struct {
	Block struct {
		Attributes map[string]attr `json:"attributes"`
		Deprecated bool            `json:"deprecated"`
	} `json:"block"`
}

type schema struct {
	Resources   map[string]blockSchema `json:"resource_schemas"`
	DataSources map[string]blockSchema `json:"data_source_schemas"`
}

func parseSchema(raw []byte, provider string) (*schema, error) {
	var all struct {
		ProviderSchemas map[string]schema `json:"provider_schemas"`
	}
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, fmt.Errorf("decode provider schema: %w", err)
	}
	s, ok := all.ProviderSchemas[provider]
	if !ok {
		return nil, fmt.Errorf("provider %s not installed", provider)
	}
	return &s, nil
}

// plural returns the plural data source of a resource type, if any.
func plural(s *schema, resourceType string) string {
	cands := []string{resourceType + "s", resourceType + "es"}
	if strings.HasSuffix(resourceType, "y") {
		cands = append(cands, strings.TrimSuffix(resourceType, "y")+"ies")
	}
	for _, c := range cands {
		if _, ok := s.DataSources[c]; ok {
			return c
		}
	}
	return ""
}

func (s *schema) hasMaxItems(dsType string) bool {
	_, ok := s.DataSources[dsType].Block.Attributes["max_items"]
	return ok
}

// listing is one resource type read from a data block.
type listing struct {
	resourceType string
	scope        *Scope
}

// block is one data block of the generated configuration.
type block struct {
	dsType   string
	label    string
	scope    *Scope // set for blocks iterating over a parent
	parent   string // address of the parent block
	listings []listing
	maxItems bool     // the data source takes max_items
	exclude  []string // parent IDs (zones, accounts) whose read failed
}

// failure is a data block, or one instance of it, that could not be read.
type failure struct {
	block   string // block address
	address string // address with the instance key, if any
	message string
}

// diagErr is an error diagnostic of terraform apply.
type diagErr struct {
	block   string // block address
	key     string // for_each key, "" when the error is not per instance
	message string
}

// nextRound decides what to read again after a failed apply. A block whose
// error concerns one for_each instance (one zone) keeps going without that
// instance, so one zone without access does not cost the others; a block
// failing as a whole, or iterating over a parent that failed, is dropped.
// changed is false when nothing could be taken out, so a retry is useless.
func nextRound(blocks []block, errs []diagErr) (keep []block, failed []failure, changed bool) {
	byBlock := map[string][]diagErr{}
	for _, e := range errs {
		byBlock[e.block] = append(byBlock[e.block], e)
	}
	for _, b := range blocks {
		es := byBlock[b.addr()]
		whole := ""
		for _, e := range es {
			if e.key == "" || b.scope == nil {
				whole = e.message
			}
		}
		if whole == "" && b.parent != "" {
			if pes := byBlock[b.parent]; len(pes) > 0 {
				whole = "parent " + b.parent + " failed: " + pes[0].message
			}
		}
		if whole != "" {
			failed = append(failed, failure{block: b.addr(), address: b.addr(), message: whole})
			changed = true
			continue
		}
		for _, e := range es {
			if slices.Contains(b.exclude, e.key) {
				continue
			}
			b.exclude = append(slices.Clone(b.exclude), e.key)
			failed = append(failed, failure{block: b.addr(), address: fmt.Sprintf("%s[%q]", b.addr(), e.key), message: e.message})
			changed = true
		}
		keep = append(keep, b)
	}
	return keep, failed, changed
}

func (b block) addr() string { return "data." + b.dsType + "." + b.label }

type plan struct {
	listings []listing
	context  []string // context data sources present in the schema
	skipped  []tf.Skip
	ds       map[string]string // resource type -> plural data source
	schema   *schema
}

func buildPlan(s *schema, a Adapter, opt Options) plan {
	p := plan{ds: map[string]string{}, schema: s}
	types := make([]string, 0, len(s.Resources))
	for t := range s.Resources {
		if (len(opt.Include) == 0 || opt.Include.Match(t)) && !opt.Exclude.Match(t) {
			types = append(types, t)
		}
	}
	sort.Strings(types)
	for _, c := range a.Context {
		if _, ok := s.DataSources[c]; ok {
			p.context = append(p.context, c)
		}
	}
	scopeArgs := map[string]bool{}
	for _, sc := range a.Scopes {
		scopeArgs[sc.Arg] = true
	}
	for _, t := range types {
		if reason, ok := a.Skip[t]; ok {
			p.skipped = append(p.skipped, tf.Skip{Type: t, Reason: reason})
			continue
		}
		ds := plural(s, t)
		if ds == "" {
			p.skipped = append(p.skipped, tf.Skip{Type: t, Reason: "no list resource or plural data source"})
			continue
		}
		attrs := s.DataSources[ds].Block.Attributes
		spec := a.spec(ds)
		if spec.IDs != "" {
			if _, ok := attrs[spec.IDs]; !ok {
				p.skipped = append(p.skipped, tf.Skip{Type: t, Reason: ds + " has no " + spec.IDs})
				continue
			}
		} else {
			items, ok := attrs[spec.Items]
			if !ok {
				p.skipped = append(p.skipped, tf.Skip{Type: t, Reason: ds + " has no " + spec.Items + " list"})
				continue
			}
			if !items.hasField(spec.Field) {
				p.skipped = append(p.skipped, tf.Skip{Type: t, Reason: ds + " results carry no " + spec.Field})
				continue
			}
		}
		var missing []string
		for name, at := range attrs {
			if at.Required && !scopeArgs[name] {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			p.skipped = append(p.skipped, tf.Skip{Type: t, Reason: ds + " requires " + strings.Join(missing, ", ")})
			continue
		}
		p.ds[t] = ds
		scoped := false
		for i := range a.Scopes {
			sc := &a.Scopes[i]
			if at, ok := attrs[sc.Arg]; ok && (at.Required || at.Optional) {
				p.listings = append(p.listings, listing{resourceType: t, scope: sc})
				scoped = true
			}
		}
		if !scoped {
			p.listings = append(p.listings, listing{resourceType: t})
		}
	}
	return p
}

// blocks groups listings into data blocks, adding the parent blocks that
// scoped listings iterate over. A parent block also serves as the listing
// of its own resource type.
func (p plan) blocks() []block {
	byKey := map[string]*block{}
	var order []string
	get := func(dsType, label string) *block {
		k := dsType + "." + label
		if b, ok := byKey[k]; ok {
			return b
		}
		byKey[k] = &block{dsType: dsType, label: label, maxItems: p.schema != nil && p.schema.hasMaxItems(dsType)}
		order = append(order, k)
		return byKey[k]
	}
	for _, c := range p.context {
		get(c, "context")
	}
	for _, l := range p.listings {
		ds := p.ds[l.resourceType]
		if l.scope == nil {
			b := get(ds, "all")
			b.listings = append(b.listings, l)
			continue
		}
		parent := get(l.scope.Parent, "all")
		b := get(ds, "by_"+l.scope.Arg)
		b.scope, b.parent = l.scope, parent.addr()
		b.listings = append(b.listings, l)
	}
	out := make([]block, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	return out
}

// renderBlocks writes the data blocks. Scope parents are result[] lists of
// objects with an id (the convention of every adapter so far).
func renderBlocks(blocks []block) []byte {
	var b strings.Builder
	for _, bl := range blocks {
		fmt.Fprintf(&b, "data %q %q {\n", bl.dsType, bl.label)
		if bl.scope != nil {
			cond := ""
			if len(bl.exclude) > 0 {
				quoted := make([]string, len(bl.exclude))
				for i, k := range bl.exclude {
					quoted[i] = tf.HCLString(k)
				}
				cond = fmt.Sprintf(" if !contains([%s], o.id)", strings.Join(quoted, ", "))
			}
			fmt.Fprintf(&b, "  for_each  = { for o in (%s.result == null ? [] : %s.result) : o.id => o.id%s }\n", bl.parent, bl.parent, cond)
			fmt.Fprintf(&b, "  %s = each.value\n", bl.scope.Arg)
		}
		if bl.maxItems {
			fmt.Fprintf(&b, "  max_items = %d\n", maxItems)
		}
		b.WriteString("}\n\n")
	}
	return []byte(b.String())
}

// applyErrors returns the error diagnostics of `terraform apply -json` that
// belong to a data block (with the for_each key when they name one), and
// the errors not tied to a block.
func applyErrors(out []byte) ([]diagErr, []string) {
	var failed []diagErr
	var fatal []string
	for _, line := range bytes.Split(out, []byte("\n")) {
		var m struct {
			Diagnostic *struct {
				Severity string `json:"severity"`
				Summary  string `json:"summary"`
				Detail   string `json:"detail"`
				Address  string `json:"address"`
			} `json:"diagnostic"`
		}
		if json.Unmarshal(line, &m) != nil || m.Diagnostic == nil || m.Diagnostic.Severity != "error" {
			continue
		}
		d := m.Diagnostic
		msg := d.Summary
		if d.Detail != "" {
			msg += ": " + strings.Join(strings.Fields(d.Detail), " ")
		}
		if len(msg) > 240 {
			msg = msg[:240] + "..."
		}
		addr, index, hasIndex := strings.Cut(d.Address, "[")
		if !strings.HasPrefix(addr, "data.") {
			fatal = append(fatal, msg)
			continue
		}
		e := diagErr{block: addr, message: msg}
		if hasIndex {
			if key, err := strconv.Unquote(strings.TrimSuffix(index, "]")); err == nil {
				e.key = key
			}
		}
		failed = append(failed, e)
	}
	return failed, fatal
}

// contextAttrs returns the attributes of the context data sources read.
func contextAttrs(stateData []byte, names []string) (map[string]map[string]any, error) {
	out := map[string]map[string]any{}
	if len(stateData) == 0 {
		return out, nil
	}
	var st struct {
		Resources []struct {
			Mode      string `json:"mode"`
			Type      string `json:"type"`
			Name      string `json:"name"`
			Instances []struct {
				Attributes map[string]any `json:"attributes"`
			} `json:"instances"`
		} `json:"resources"`
	}
	if err := model.DecodeJSON(stateData, &st); err != nil {
		return nil, fmt.Errorf("decode data source state: %w", err)
	}
	for _, r := range st.Resources {
		if r.Mode == "data" && r.Name == "context" && slices.Contains(names, r.Type) && len(r.Instances) == 1 {
			out[r.Type] = r.Instances[0].Attributes
		}
	}
	return out, nil
}

// instanceCounts returns how many instances each data block has in the
// scan's state.
func instanceCounts(stateData []byte) (map[string]int, error) {
	out := map[string]int{}
	if len(stateData) == 0 {
		return out, nil
	}
	var st struct {
		Resources []struct {
			Mode      string            `json:"mode"`
			Type      string            `json:"type"`
			Name      string            `json:"name"`
			Instances []json.RawMessage `json:"instances"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(stateData, &st); err != nil {
		return nil, fmt.Errorf("decode data source state: %w", err)
	}
	for _, r := range st.Resources {
		if r.Mode == "data" {
			out["data."+r.Type+"."+r.Name] = len(r.Instances)
		}
	}
	return out, nil
}

// truncated reports listings that may have stopped early: a data source
// instance that returned max_items objects, and every block iterating over
// a parent list that did (zones beyond the cut were never read).
func truncated(stateData []byte, blocks []block, a Adapter) ([]tf.Failure, error) {
	var st struct {
		Resources []struct {
			Mode      string `json:"mode"`
			Type      string `json:"type"`
			Name      string `json:"name"`
			Instances []struct {
				IndexKey   any            `json:"index_key"`
				Attributes map[string]any `json:"attributes"`
			} `json:"instances"`
		} `json:"resources"`
	}
	if err := model.DecodeJSON(stateData, &st); err != nil {
		return nil, fmt.Errorf("decode data source state: %w", err)
	}
	byAddr := map[string]block{}
	for _, b := range blocks {
		byAddr[b.addr()] = b
	}
	var out []tf.Failure
	full := map[string]bool{}
	for _, r := range st.Resources {
		b, ok := byAddr["data."+r.Type+"."+r.Name]
		if r.Mode != "data" || !ok || !b.maxItems {
			continue
		}
		for _, in := range r.Instances {
			n := len(listItems(in.Attributes, a.spec(b.dsType)))
			if n < maxItems {
				continue
			}
			full[b.addr()] = true
			addr := b.addr()
			if key, ok := in.IndexKey.(string); ok {
				addr = fmt.Sprintf("%s[%q]", addr, key)
			}
			for _, l := range b.listings {
				out = append(out, tf.Failure{Address: addr, Type: l.resourceType, Message: fmt.Sprintf("returned %d objects, its max_items; there may be more", n)})
			}
		}
	}
	for _, b := range blocks {
		if b.parent == "" || !full[b.parent] {
			continue
		}
		for _, l := range b.listings {
			out = append(out, tf.Failure{Address: b.addr(), Type: l.resourceType, Message: b.parent + " returned its max_items; parents beyond it were not read"})
		}
	}
	return out, nil
}

// listItems returns the objects of one data source instance.
func listItems(attrs map[string]any, spec ListSpec) []map[string]any {
	var out []map[string]any
	if spec.IDs == "" {
		list, _ := attrs[spec.Items].([]any)
		for _, it := range list {
			if m, ok := it.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	ids, _ := attrs[spec.IDs].([]any)
	for i, id := range ids {
		m := map[string]any{spec.Field: id}
		for k, v := range attrs {
			if sib, ok := v.([]any); ok && k != spec.IDs && len(sib) == len(ids) {
				m[k] = sib[i]
			}
		}
		out = append(out, m)
	}
	return out
}

func extract(stateData []byte, blocks []block, a Adapter, s *schema) ([]model.Resource, error) {
	var st struct {
		Resources []struct {
			Mode      string `json:"mode"`
			Type      string `json:"type"`
			Name      string `json:"name"`
			Instances []struct {
				IndexKey   any            `json:"index_key"`
				Attributes map[string]any `json:"attributes"`
			} `json:"instances"`
		} `json:"resources"`
	}
	if err := model.DecodeJSON(stateData, &st); err != nil {
		return nil, fmt.Errorf("%s: decode data source state: %w", a.Name, err)
	}
	byAddr := map[string]block{}
	for _, b := range blocks {
		byAddr[b.addr()] = b
	}
	var out []scoped
	for _, r := range st.Resources {
		b, ok := byAddr["data."+r.Type+"."+r.Name]
		if r.Mode != "data" || !ok {
			continue
		}
		spec := a.spec(b.dsType)
		for _, in := range r.Instances {
			scopeVal, _ := in.IndexKey.(string)
			for _, item := range listItems(in.Attributes, spec) {
				id, ok := model.Scalar(item[spec.Field])
				if !ok || id == "" {
					continue
				}
				for _, l := range b.listings {
					if a.Keep != nil && !a.Keep(l.resourceType, item) {
						continue
					}
					res := model.Resource{
						Type:        l.resourceType,
						AltTypes:    a.AltTypes[l.resourceType],
						Identity:    map[string]any{spec.Key: id},
						DisplayName: displayName(item, id),
						ImportID:    id,
						// The object's own ID must match; the scope alone
						// (zone_id, account_id) never makes it managed.
						RequiredKeys: []string{spec.Key},
					}
					if a.AltIdentity != nil {
						res.AltIdentity = a.AltIdentity(l.resourceType, item)
					}
					if l.scope != nil {
						res.Identity[l.scope.Arg] = scopeVal
						res.ImportID = importID(s, a, l, scopeVal, id)
					}
					if a.ImportID != nil {
						res.ImportID = a.ImportID(l.resourceType, item, id)
					}
					sc := scoped{res: res, id: id}
					if l.scope != nil {
						sc.scopeArg = l.scope.Arg
					}
					out = append(out, sc)
				}
			}
		}
	}
	return collapse(out), nil
}

// scoped is a listed object with the scope it was listed in.
type scoped struct {
	res      model.Resource
	id       string
	scopeArg string
}

// collapse drops repeats of the same object. Account objects also show up
// in zone listings: an object of the same type and id seen in a different
// kind of scope (account vs zone) is the same object and is kept once, in
// the scope listed first. Objects in different scopes of the same kind (two
// accounts with a Worker named "api") are different objects and all kept.
// Objects of different types are never merged here; equivalent types are
// declared by adapters (Skip and AltTypes), since IDs such as Worker names
// are chosen by users and must not let one object hide another.
func collapse(rs []scoped) []model.Resource {
	var out []model.Resource
	seen := map[string]bool{}
	firstScope := map[string]string{} // type + id -> scope argument
	for _, r := range rs {
		if seen[r.res.Key()] {
			continue
		}
		seen[r.res.Key()] = true
		k := r.res.Type + "\x00" + r.id
		if arg, ok := firstScope[k]; ok && arg != r.scopeArg {
			continue
		}
		firstScope[k] = r.scopeArg
		out = append(out, r.res)
	}
	return out
}

// importID follows the common provider convention: "<scope>/<id>", or
// "<prefix>/<scope>/<id>" when the resource can live in several scopes.
func importID(s *schema, a Adapter, l listing, scopeVal, id string) string {
	attrs := s.Resources[l.resourceType].Block.Attributes
	scopes := 0
	for _, sc := range a.Scopes {
		if _, ok := attrs[sc.Arg]; ok {
			scopes++
		}
	}
	if scopes > 1 && l.scope.Prefix != "" {
		return l.scope.Prefix + "/" + scopeVal + "/" + id
	}
	return scopeVal + "/" + id
}

var nameFields = []string{"name", "hostname", "title", "pattern", "email", "network", "domain", "slug", "script_name", "description"}

func displayName(item map[string]any, id string) string {
	name := id
	for _, f := range nameFields {
		if v, ok := item[f].(string); ok && v != "" {
			name = v
			break
		}
	}
	if t, ok := item["type"].(string); ok && t != "" && len(t) <= 16 {
		name = t + " " + name
	}
	return name
}
