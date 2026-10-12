package tf

import (
	"fmt"
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
	return a, nil
}

// Query is one generated list block.
type Query struct {
	Type     string
	Provider string // local name
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
	// AliasOnly holds the local names the root configures only with
	// aliases (see AliasOnly), with the aliases.
	AliasOnly map[string][]string
	// IncludeResource tells which types need their attributes; nil: none.
	IncludeResource func(resourceType string) bool
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
		// Generated blocks use the default configuration. When the root
		// has only aliased ones, the default is an empty configuration
		// that may reach another account through the environment.
		if aliases := opt.AliasOnly[local]; !temporary && len(aliases) > 0 {
			for _, t := range types {
				p.Skipped = append(p.Skipped, Skip{t, "provider " + local + " is configured only with aliases"})
			}
			p.Notes = append(p.Notes, fmt.Sprintf("provider %s is configured only with aliases (%s.%s), so its %d list-capable types were not queried with the empty default configuration; add your own list blocks with provider = %s.<alias> to a .tfquery.hcl file in the root",
				local, local, strings.Join(aliases, ", "+local+"."), len(types), local))
			continue
		}
		for _, t := range types {
			if reason, ok := OptIn[t]; ok && !opt.Include.Names(t) {
				p.Skipped = append(p.Skipped, Skip{t, reason + "; name it in -types to list it"})
				continue
			}
			q, reason := planType(t, local, ps.ListResourceSchemas[t], opt.Args)
			if reason != "" {
				p.Skipped = append(p.Skipped, Skip{t, reason})
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
	p.dropTwins()
	return p
}

// dropTwins keeps one query per type among providers that list the same
// objects (google and google-beta): both would report every object, under
// identities that need not be equal. A provider the root declares wins over
// one only child modules use, then the one the family is named after
// (google over google-beta).
func (p *Plan) dropTwins() {
	keep := map[string]int{} // type \x00 family -> index in kept
	var kept []Query
	for _, q := range p.Queries {
		fam := model.ProviderFamily(q.Source)
		k := q.Type + "\x00" + fam
		i, seen := keep[k]
		if !seen {
			keep[k] = len(kept)
			kept = append(kept, q)
			continue
		}
		drop, cur := q, kept[i]
		if !q.Temporary && cur.Temporary || q.Temporary == cur.Temporary && isFamilyName(q.Source, fam) && !isFamilyName(cur.Source, fam) {
			drop, kept[i] = cur, q
		}
		p.Skipped = append(p.Skipped, Skip{drop.Type, fmt.Sprintf("listed through %s, which lists the same objects as %s", kept[i].Provider, drop.Provider)})
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

func planType(typ, local string, ls ListSchema, args []Arg) (Query, string) {
	q := Query{Type: typ, Provider: local}
	// An argument for matching types wins over one for all types; two
	// values for one attribute would make terraform reject the query.
	supplied := map[string]bool{}
	for _, scoped := range []bool{true, false} {
		for _, a := range args {
			if (a.Types != "") != scoped || supplied[a.Name] || scoped && !glob.Compile(a.Types).Match(typ) {
				continue
			}
			if at, ok := ls.Block.Attributes[a.Name]; ok && string(at.Type) == `"string"` {
				q.Args = append(q.Args, a)
				supplied[a.Name] = true
			}
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
		return q, "list config requires " + strings.Join(missing, ", ")
	}
	return q, ""
}

// HCL renders the plan as a .tfquery.hcl file.
func (p Plan) HCL(limit int) []byte {
	var b strings.Builder
	b.WriteString("# Generated by tforphans; removed after the run.\n")
	for _, q := range p.Queries {
		fmt.Fprintf(&b, "\nlist %q %q {\n", q.Type, q.Label())
		fmt.Fprintf(&b, "  provider = %s\n", q.Provider)
		fmt.Fprintf(&b, "  limit    = %d\n", limit)
		if q.IncludeResource {
			b.WriteString("  include_resource = true\n")
		}
		if len(q.Args) > 0 {
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
		b.WriteString("}\n")
	}
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
func (q Query) Label() string { return LabelPrefix + q.Provider + "_" + q.Type }

// Address is the list block's address, list.<type>.<label>.
func (q Query) Address() string { return "list." + q.Type + "." + q.Label() }
