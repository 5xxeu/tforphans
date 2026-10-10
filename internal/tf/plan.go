package tf

import (
	"fmt"
	"sort"
	"strings"

	"github.com/5xxeu/tforphans/internal/glob"
)

// LabelPrefix starts the labels of generated list blocks. It is not how
// they are told apart from the user's blocks: that is by the plan's exact
// addresses, since a user's label may start with it too.
const LabelPrefix = "tfo_"

// Arg fans every list query whose config has a string argument Name out
// over Values, for example region=eu-north-1,us-east-1.
type Arg struct {
	Name   string
	Values []string
}

func ParseArg(s string) (Arg, error) {
	name, vals, ok := strings.Cut(s, "=")
	name = strings.TrimSpace(name)
	if !ok || name == "" {
		return Arg{}, fmt.Errorf("invalid -arg %q, want name=value[,value...]", s)
	}
	a := Arg{Name: name}
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
}

// Skip is a list-capable type that was not queried.
type Skip struct {
	Type   string
	Reason string
}

type Plan struct {
	Queries []Query
	Skipped []Skip
	// Declare maps temporary local names to provider addresses.
	Declare map[string]string
	// Notes explain whole providers left out (configured only with aliases).
	Notes []string
}

type PlanOptions struct {
	Args    []Arg
	Include glob.List // empty: all types
	Exclude glob.List
	// AliasOnly holds the local names the root configures only with
	// aliases (see AliasOnly), with the aliases.
	AliasOnly map[string][]string
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
			q, reason := planType(t, local, ps.ListResourceSchemas[t], opt.Args)
			if reason != "" {
				p.Skipped = append(p.Skipped, Skip{t, reason})
				continue
			}
			q.Source, q.Temporary = addr, temporary
			p.Queries = append(p.Queries, q)
			if temporary {
				if p.Declare == nil {
					p.Declare = map[string]string{}
				}
				p.Declare[local] = addr
			}
		}
	}
	return p
}

func planType(typ, local string, ls ListSchema, args []Arg) (Query, string) {
	q := Query{Type: typ, Provider: local}
	supplied := map[string]bool{}
	for _, a := range args {
		if at, ok := ls.Block.Attributes[a.Name]; ok && string(at.Type) == `"string"` {
			q.Args = append(q.Args, a)
			supplied[a.Name] = true
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
