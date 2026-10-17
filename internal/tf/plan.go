package tf

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/5xxeu/tforphans/internal/glob"
	"github.com/5xxeu/tforphans/internal/model"
)

// LabelPrefix starts the labels of generated list blocks. It is not how
// they are told apart from the user's blocks: that is by the plan's exact
// addresses, since a user's label may start with it too.
const LabelPrefix = "tfo_"

// MissingArgs starts the reason of a type skipped because its list config
// has required arguments that no -arg gives.
const MissingArgs = "list config requires "

// Arg fans every list query whose config has a string argument Name out
// over Values, for example region=eu-north-1,us-east-1. Types, when set,
// limits it to the resource types it matches
// (google_compute_*:zone=europe-west1-b).
type Arg struct {
	Types  string
	Name   string
	Values []string
}

// Key identifies the argument among repeated -arg flags.
func (a Arg) Key() string { return a.Types + ":" + a.Name }

// Auto is the value that stands for every zone or region (see AutoArgs).
const Auto = "auto"

// AutoSource says how to read every region and zone of a provider family
// (see family) with data sources: Regions lists the regions (attribute
// expression RegionsAttr), Zones the zones of one region given as ZoneArg.
type AutoSource struct {
	Regions, RegionsAttr string
	Zones, ZoneArg, Attr string
}

// AutoSources are the families whose region and zone arguments take Auto.
// Google Cloud: read with the root's own provider configuration, so in its
// project.
var AutoSources = map[string]AutoSource{
	"google": {Regions: "google_compute_regions", RegionsAttr: "names", Zones: "google_compute_zones", ZoneArg: "region", Attr: "names"},
}

// IsAuto reports whether the argument asks for every value (zone=auto).
func (a Arg) IsAuto() bool { return len(a.Values) == 1 && a.Values[0] == Auto }

func ParseArg(s string) (Arg, error) {
	name, vals, ok := strings.Cut(s, "=")
	types, name, scoped := strings.Cut(name, ":")
	if !scoped {
		types, name = "", types
	}
	types, name = strings.TrimSpace(types), strings.TrimSpace(name)
	if !ok || name == "" || scoped && types == "" {
		return Arg{}, fmt.Errorf("invalid -arg %q, want [TYPE_GLOB:]name=value[,value...]", s)
	}
	a := Arg{Types: types, Name: name}
	for _, v := range strings.Split(vals, ",") {
		if v = strings.TrimSpace(v); v != "" {
			a.Values = append(a.Values, v)
		}
	}
	if len(a.Values) == 0 {
		return Arg{}, fmt.Errorf("invalid -arg %q: no values", s)
	}
	if len(a.Values) > 1 && slices.Contains(a.Values, Auto) {
		return Arg{}, fmt.Errorf("invalid -arg %q: %s stands for every value and takes no others", s, Auto)
	}
	return a, nil
}

// Query is one generated list block.
type Query struct {
	Type     string
	Provider string // local name
	// Alias is the provider configuration's alias ("" for the default):
	// each configuration may look at another account.
	Alias string
	// Source is the provider address. Temporary is set when the root module
	// does not declare the provider (only child modules do) and the local
	// name exists only for the duration of the run.
	Source    string
	Temporary bool
	Args      []Arg // args the type's config accepts
	// IncludeResource asks for the resource's attributes too.
	IncludeResource bool
}

// Skip is a list-capable type that was not queried.
type Skip struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
	// Root is the root module the type was skipped in ("" for -scan).
	Root string `json:"root,omitempty"`
	// Excluded marks a type left out on purpose (opt-in, listed through
	// another provider, a scope not asked for) or one that has no listing
	// at all; any other skip is a check that was needed and not done.
	Excluded bool `json:"excluded,omitempty"`
	// Provider is the local name of the provider the type was skipped
	// for: a listing through another provider (google instead of an
	// aliased google-beta) may look at another project.
	Provider string `json:"provider,omitempty"`
	// Kind is the kind of scan scope the type was skipped in (zone);
	// a listing in another kind (account) does not cover it.
	Kind string `json:"-"`
}

// Check is a listing that ran to the end: a type in a root (or in a scan
// scope such as one zone), with or without results, and the local name of
// the provider that listed it.
type Check struct {
	Type     string
	Scope    string
	Root     string
	Provider string
	Kind     string // scan scope kind (zone, account); "" when unscoped
}

type Plan struct {
	Queries []Query
	Skipped []Skip
	// Declare maps temporary local names to provider addresses.
	Declare map[string]string
	// Notes explain whole providers left out (configured only with aliases).
	Notes []string
	// NoList are the providers whose installed version has no list
	// resources at all.
	NoList []string
}

type PlanOptions struct {
	Args    []Arg
	Include glob.List // empty: all types
	Exclude glob.List
	// Configs holds the root's provider configurations by provider address
	// (see ProviderSourceConfigs): each alias is listed as well as the
	// default, under the local name the generated blocks use.
	Configs map[string]SourceConfigs
	// NoAliases lists only default configurations; the types of each
	// aliased one are skipped, a gap in coverage (-no-aliases).
	NoAliases bool
	// IncludeResource tells which types need their attributes; nil: none.
	IncludeResource func(resourceType string) bool
	// ProviderConfigs are the root's default provider configurations by
	// local name (see ProviderConfigs).
	ProviderConfigs map[string]string
}

// OptIn are list-capable types whose lists reach far beyond what a root
// module manages (every project the credentials can see); they are queried
// only when -types names them exactly.
var OptIn = map[string]string{
	"google_project": "lists every project the credentials can see, not only this root's",
	"google_folder":  "lists folders across the organization, not only this root's",
}

// BuildPlan picks the list-capable resource types to query. Types whose list
// config needs an argument that no Arg supplies are skipped: a single invalid
// list block makes Terraform reject the whole query.
func BuildPlan(s *Schemas, declared map[string]string, opt PlanOptions) Plan {
	var p Plan
	addrs := make([]string, 0, len(s.ProviderSchemas))
	for a := range s.ProviderSchemas {
		addrs = append(addrs, a)
	}
	sort.Strings(addrs)
	// Names already taken: declared ones and the implicit hashicorp ones.
	used := map[string]bool{}
	for _, n := range declared {
		used[n] = true
	}
	for _, addr := range addrs {
		if parts := strings.Split(addr, "/"); len(parts) == 3 && parts[0] == "registry.terraform.io" && parts[1] == "hashicorp" {
			if _, own := declared[addr]; !own {
				used[parts[2]] = true
			}
		}
	}
	for _, addr := range addrs {
		ps := s.ProviderSchemas[addr]
		if len(ps.ListResourceSchemas) == 0 {
			p.NoList = append(p.NoList, addr)
			continue
		}
		types := make([]string, 0, len(ps.ListResourceSchemas))
		for t := range ps.ListResourceSchemas {
			if (len(opt.Include) == 0 || opt.Include.Match(t)) && !opt.Exclude.Match(t) {
				types = append(types, t)
			}
		}
		sort.Strings(types)
		if len(types) == 0 {
			continue
		}
		local, temporary := localName(addr, declared, used)
		// Generated blocks use the default configuration and every aliased
		// one: each may look at another account. When the root has only
		// aliased ones, the default is an empty configuration that may
		// reach another account through the environment: it is left out.
		sc := opt.Configs[addr]
		// Two local names of one source: terraform resolves the provider
		// blocks unreliably (a run may give an alias an empty
		// configuration), so a listing that ran proves nothing.
		if !temporary && len(sc.LocalNames) > 1 {
			names := strings.Join(sc.LocalNames, ", ")
			// One gap per configuration, the default under every name: a
			// user block through one of them covers that one only.
			var refs []string
			if sc.Plain || len(sc.Aliases) == 0 {
				refs = append(refs, sc.LocalNames...)
			}
			for _, a := range sc.Aliases {
				refs = append(refs, a.Ref())
			}
			for _, t := range types {
				if reason, ok := OptIn[t]; ok && !opt.Include.Names(t) {
					p.Skipped = append(p.Skipped, Skip{Type: t, Reason: reason + "; name it in -types to list it", Excluded: true})
					continue
				}
				for _, ref := range refs {
					p.Skipped = append(p.Skipped, Skip{Type: t, Reason: "provider " + addr + " is required under several local names (" + names + ")", Provider: ref})
				}
			}
			p.Notes = append(p.Notes, fmt.Sprintf("provider %s is required under several local names (%s), and terraform resolves its configurations unreliably then: its %d list-capable types were not queried; give it one local name", addr, names, len(types)))
			continue
		}
		configs := []ConfigRef{{Local: local}}
		var aliases []ConfigRef
		if !temporary {
			aliases = sc.Aliases
		}
		if !temporary && len(aliases) > 0 && !sc.Plain {
			configs = nil
			var names []string
			for _, a := range aliases {
				names = append(names, a.Ref())
			}
			refs := strings.Join(names, ", ")
			if opt.NoAliases {
				p.Notes = append(p.Notes, fmt.Sprintf("provider %s is configured only with aliases (%s) and -no-aliases leaves them out, so its list-capable types were not queried (not with the empty default configuration either)", local, refs))
			} else {
				p.Notes = append(p.Notes, fmt.Sprintf("provider %s is configured only with aliases, so its list-capable types were queried through %s, not with the empty default configuration", local, refs))
			}
		}
		configs = append(configs, aliases...)
		for _, t := range types {
			if reason, ok := OptIn[t]; ok && !opt.Include.Names(t) {
				p.Skipped = append(p.Skipped, Skip{Type: t, Reason: reason + "; name it in -types to list it", Excluded: true})
				continue
			}
			for _, c := range configs {
				if c.Alias != "" && opt.NoAliases {
					p.Skipped = append(p.Skipped, Skip{Type: t, Reason: "provider configuration " + c.Ref() + " not listed (-no-aliases)", Provider: c.Ref()})
					continue
				}
				q, reason := planType(t, c.Local, addr, ps.ListResourceSchemas[t], opt.Args)
				q.Alias = c.Alias
				if reason != "" {
					// The kind of reason stays first (missing argument).
					if c.Alias != "" {
						reason += " (provider " + q.Ref() + ")"
					}
					p.Skipped = append(p.Skipped, Skip{Type: t, Reason: reason, Provider: q.Ref()})
					continue
				}
				q.Source, q.Temporary = addr, temporary
				q.IncludeResource = opt.IncludeResource != nil && opt.IncludeResource(t)
				p.Queries = append(p.Queries, q)
				if temporary {
					if p.Declare == nil {
						p.Declare = map[string]string{}
					}
					p.Declare[local] = addr
				}
			}
		}
	}
	p.dropTwins(opt.ProviderConfigs)
	return p
}

// dropTwins keeps one query per type among providers that list the same
// objects (google and google-beta) when the root configures them alike:
// both would report every object, under identities that need not be equal.
// Configured differently (another project), or given other -arg values,
// both are queried.
func (p *Plan) dropTwins(configs map[string]string) {
	keep := map[string]int{} // type, family, configuration, args -> index in kept
	var kept []Query
	for _, q := range p.Queries {
		cfg := ""
		if !q.Temporary {
			var known bool
			cfg, known = configs[q.Ref()]
			// An alias says by its name that it is a configuration of its
			// own; only a known equal text makes two aliases twins.
			if !known && q.Alias != "" {
				cfg = "alias " + q.Ref()
			}
		}
		// Arguments too: a provider whose schema lacks one (-arg
		// project=...) lists another place than the one that takes it.
		var args []string
		for _, a := range q.Args {
			args = append(args, a.Name+"="+strings.Join(a.Values, ","))
		}
		sort.Strings(args)
		fam := model.ProviderFamily(q.Source)
		k := q.Type + "\x00" + fam + "\x00" + cfg + "\x00" + strings.Join(args, "\x00")
		i, seen := keep[k]
		if !seen {
			keep[k] = len(kept)
			kept = append(kept, q)
			continue
		}
		// Equal configurations scan the same place; a name the root has
		// needs no temporary declaration, then google beats google-beta.
		drop, cur := q, kept[i]
		if !q.Temporary && cur.Temporary || q.Temporary == cur.Temporary && isFamilyName(q.Source, fam) && !isFamilyName(cur.Source, fam) {
			drop, kept[i] = cur, q
		}
		p.Skipped = append(p.Skipped, Skip{Type: drop.Type, Reason: fmt.Sprintf("listed through %s, which lists the same objects as %s with the same configuration", kept[i].Ref(), drop.Ref()), Excluded: true})
	}
	p.Queries = kept
	for local := range p.Declare {
		if !slices.ContainsFunc(kept, func(q Query) bool { return q.Provider == local }) {
			delete(p.Declare, local)
		}
	}
}

// isFamilyName reports whether addr is the provider its family is named
// after (hashicorp/google, not hashicorp/google-beta).
func isFamilyName(addr, family string) bool {
	return strings.HasSuffix(strings.ToLower(addr), "/"+family) || strings.ToLower(addr) == family
}

func planType(typ, local, addr string, ls ListSchema, args []Arg) (Query, string) {
	q := Query{Type: typ, Provider: local}
	// Arguments for matching types win over ones for all types; the values
	// of several matching ones are combined (two attributes of one name
	// would make terraform reject the query).
	supplied := map[string]bool{}
	for _, scoped := range []bool{true, false} {
		index := map[string]int{} // name -> position in q.Args, this pass
		for _, a := range args {
			if (a.Types != "") != scoped || supplied[a.Name] && index[a.Name] == 0 || scoped && !glob.Compile(a.Types).Match(typ) {
				continue
			}
			at, ok := ls.Block.Attributes[a.Name]
			if !ok || string(at.Type) != `"string"` {
				continue
			}
			if i := index[a.Name]; i > 0 {
				merged := &q.Args[i-1]
				for _, v := range a.Values {
					if !slices.Contains(merged.Values, v) {
						merged.Values = append(merged.Values, v)
					}
				}
				continue
			}
			q.Args = append(q.Args, Arg{Types: a.Types, Name: a.Name, Values: slices.Clone(a.Values)})
			index[a.Name] = len(q.Args)
			supplied[a.Name] = true
		}
	}
	for i, a := range q.Args {
		if !slices.Contains(a.Values, Auto) {
			continue
		}
		// auto covers every value; another -arg of the same name adds none.
		q.Args[i].Values = []string{Auto}
		if _, ok := AutoSources[family(addr)]; !ok || a.Name != "region" && a.Name != "zone" {
			return q, fmt.Sprintf("-arg %s=%s works only for %s", a.Name, Auto, autoHelp())
		}
	}
	var missing []string
	for name, at := range ls.Block.Attributes {
		if at.Required && !supplied[name] {
			missing = append(missing, name)
		}
	}
	for name, bt := range ls.Block.BlockTypes {
		if bt.MinItems > 0 {
			missing = append(missing, name+" {}")
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return q, MissingArgs + strings.Join(missing, ", ")
	}
	return q, ""
}

// family is the provider family of a provider address: google for
// hashicorp/google and hashicorp/google-beta, else the address's type name.
func family(addr string) string {
	name := strings.ToLower(addr[strings.LastIndex(addr, "/")+1:])
	if name == "google-beta" {
		return "google"
	}
	return name
}

func autoHelp() string {
	return "region and zone of " + strings.Join(slices.Sorted(maps.Keys(AutoSources)), ", ")
}

// auto reports whether the query fans out over values read at run time.
func (q Query) auto() bool { return slices.ContainsFunc(q.Args, Arg.IsAuto) }

// autoZones reports whether the query needs every zone (not just regions).
func (q Query) autoZones() bool {
	return slices.ContainsFunc(q.Args, func(a Arg) bool { return a.IsAuto() && a.Name == "zone" })
}

// autoLabel is the label of the data sources that read the regions and
// zones for queries through a provider configuration (local name and
// alias): each configuration may look at another project.
func autoLabel(local, alias string) string {
	if alias != "" {
		return LabelPrefix + "auto_" + local + "_" + alias
	}
	return LabelPrefix + "auto_" + local
}

func (q Query) autoLabel() string { return autoLabel(q.Provider, q.Alias) }

// AutoHCL renders the data sources that the queries with auto arguments
// read their values from, or nil when there are none. They go into a .tf
// file: terraform query reads data sources of the configuration, not of
// .tfquery.hcl files.
func (p Plan) AutoHCL() []byte {
	type need struct {
		src   AutoSource
		zones bool
	}
	needs := map[[2]string]need{} // by local name and alias
	for _, q := range p.Queries {
		if q.auto() {
			k := [2]string{q.Provider, q.Alias}
			n := needs[k]
			n.src, n.zones = AutoSources[family(q.Source)], n.zones || q.autoZones()
			needs[k] = n
		}
	}
	if len(needs) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("# Generated by tforphans; removed after the run.\n")
	keys := slices.SortedFunc(maps.Keys(needs), func(a, b [2]string) int { return strings.Compare(a[0]+"\x00"+a[1], b[0]+"\x00"+b[1]) })
	for _, k := range keys {
		n, label := needs[k], autoLabel(k[0], k[1])
		ref := (Query{Provider: k[0], Alias: k[1]}).Ref()
		fmt.Fprintf(&b, "\ndata %q %q {\n  provider = %s\n}\n", n.src.Regions, label, ref)
		if n.zones {
			fmt.Fprintf(&b, "\ndata %q %q {\n  provider = %s\n  for_each = toset(data.%s.%s.%s)\n  %s = each.key\n}\n",
				n.src.Zones, label, ref, n.src.Regions, label, n.src.RegionsAttr, n.src.ZoneArg)
		}
	}
	return []byte(b.String())
}

// autoValues is the expression for every value of an auto argument of q.
func (q Query) autoValues(name string) string {
	src, label := AutoSources[family(q.Source)], q.autoLabel()
	if name == "region" {
		return "data." + src.Regions + "." + label + "." + src.RegionsAttr
	}
	return "flatten([for r in data." + src.Zones + "." + label + " : r." + src.Attr + "])"
}

// AutoDependents returns the generated queries that need the values of the
// data source at addr (data.<type>.<label>, or an instance of it), when
// AutoHCL declared it for this plan: every auto query of its provider for
// the regions, those with zone=auto for the zones. ok is false for any
// other data source.
func (p Plan) AutoDependents(addr string) (deps []Query, ok bool) {
	block, _, _ := strings.Cut(addr, "[")
	for _, q := range p.Queries {
		if !q.auto() {
			continue
		}
		src, label := AutoSources[family(q.Source)], q.autoLabel()
		switch block {
		case "data." + src.Regions + "." + label:
			deps = append(deps, q)
		case "data." + src.Zones + "." + label:
			if q.autoZones() {
				deps = append(deps, q)
			}
		}
	}
	return deps, len(deps) > 0
}

// HCL renders the plan as a .tfquery.hcl file.
func (p Plan) HCL(limit int) []byte {
	var b strings.Builder
	b.WriteString("# Generated by tforphans; removed after the run.\n")
	for _, q := range p.Queries {
		b.WriteByte('\n')
		b.Write(q.HCL(limit))
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// HCL is the generated text of this list block, without the file header.
func (q Query) HCL(limit int) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "list %q %q {\n", q.Type, q.Label())
	fmt.Fprintf(&b, "  provider = %s\n", q.Ref())
	fmt.Fprintf(&b, "  limit    = %d\n", limit)
	if q.IncludeResource {
		b.WriteString("  include_resource = true\n")
	}
	if q.auto() {
		// Values known only at run time: the combinations come from
		// setproduct, keyed as the static ones are.
		var lists, key, fields []string
		for i, a := range q.Args {
			if a.IsAuto() {
				lists = append(lists, q.autoValues(a.Name))
			} else {
				var vals []string
				for _, v := range a.Values {
					vals = append(vals, HCLString(v))
				}
				lists = append(lists, "["+strings.Join(vals, ", ")+"]")
			}
			key = append(key, fmt.Sprintf("%s=${c[%d]}", a.Name, i))
			fields = append(fields, fmt.Sprintf("%s = c[%d]", a.Name, i))
		}
		combos := "setproduct(" + strings.Join(lists, ", ") + ")"
		if len(lists) == 1 { // setproduct wants two lists at least
			combos = "[for v in " + lists[0] + " : [v]]"
		}
		fmt.Fprintf(&b, "  for_each = { for c in %s : \"%s\" => { %s } }\n", combos, strings.Join(key, ","), strings.Join(fields, ", "))
		b.WriteString("  config {\n")
		for _, a := range q.Args {
			fmt.Fprintf(&b, "    %s = each.value.%s\n", a.Name, a.Name)
		}
		b.WriteString("  }\n")
	} else if len(q.Args) > 0 {
		b.WriteString("  for_each = {\n")
		for _, combo := range product(q.Args) {
			var key, fields []string
			for i, a := range q.Args {
				key = append(key, a.Name+"="+combo[i])
				fields = append(fields, fmt.Sprintf("%s = %s", a.Name, HCLString(combo[i])))
			}
			fmt.Fprintf(&b, "    %s = { %s }\n", HCLString(strings.Join(key, ",")), strings.Join(fields, ", "))
		}
		b.WriteString("  }\n  config {\n")
		for _, a := range q.Args {
			fmt.Fprintf(&b, "    %s = each.value.%s\n", a.Name, a.Name)
		}
		b.WriteString("  }\n")
	}
	b.WriteString("}")
	return []byte(b.String())
}

// ProvidersHCL declares the temporary local names, or returns nil when none
// are needed. Adding a local name for an installed provider needs no
// terraform init; the provider gets the root's default (empty)
// configuration, the same one child modules inherit.
func (p Plan) ProvidersHCL() []byte {
	if len(p.Declare) == 0 {
		return nil
	}
	names := make([]string, 0, len(p.Declare))
	for n := range p.Declare {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("# Generated by tforphans; removed after the run.\n")
	b.WriteString("terraform {\n  required_providers {\n")
	for _, n := range names {
		fmt.Fprintf(&b, "    %s = { source = %s }\n", n, HCLString(p.Declare[n]))
	}
	b.WriteString("  }\n}\n")
	return []byte(b.String())
}

// product returns every combination of argument values.
func product(args []Arg) [][]string {
	out := [][]string{{}}
	for _, a := range args {
		var next [][]string
		for _, prefix := range out {
			for _, v := range a.Values {
				next = append(next, append(append([]string{}, prefix...), v))
			}
		}
		out = next
	}
	return out
}

// HCLString quotes s as an HCL string literal without template sequences.
func HCLString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`, "${", "$${", "%{", "%%{")
	return `"` + r.Replace(s) + `"`
}

// Label is the block label: the provider is part of it, since two providers
// (google and google-beta) can list the same resource type.
func (q Query) Label() string {
	if q.Alias != "" {
		// Local names have no underscore, so the alias stays apart from
		// the provider; a type always starts with its provider's name.
		return LabelPrefix + q.Provider + "_" + q.Alias + "_" + q.Type
	}
	return LabelPrefix + q.Provider + "_" + q.Type
}

// Ref is the provider configuration the block uses: aws, or aws.prod.
func (q Query) Ref() string {
	if q.Alias != "" {
		return q.Provider + "." + q.Alias
	}
	return q.Provider
}

// Address is the list block's address, list.<type>.<label>.
func (q Query) Address() string { return "list." + q.Type + "." + q.Label() }
