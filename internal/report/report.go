// Package report filters and prints unmanaged resources.
package report

import (
	"bufio"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/5xxeu/tforphans/internal/glob"
	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/tf"
)

// Ignore holds rules of the form TYPE_GLOB[:NAME_GLOB] [KEY=GLOB...].
// NAME_GLOB is matched against the display name, the import ID and every
// identity value. Each KEY=GLOB must hold too: KEY is an identity key
// (project=prod-*), or attr. and a path into the resource's attributes
// (attr.labels.goog-gke-node=*), which tforphans then fetches for the
// rule's types.
type Ignore struct {
	rules []ignoreRule
}

type ignoreRule struct {
	typ, name glob.Pattern
	hasName   bool
	conds     []ignoreCond
}

type ignoreCond struct {
	attr bool     // a path into attributes, not an identity key
	path []string // identity key, or attribute path
	val  glob.Pattern
}

// condition matches a trailing KEY=GLOB word.
var condition = regexp.MustCompile(`^(attr\.)?([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z0-9_.-]+)?)=(\S*)$`)

// LoadIgnore reads an ignore file. A missing optional file is an empty rule
// set.
func LoadIgnore(path string, optional bool) (*Ignore, error) {
	f, err := os.Open(path)
	if err != nil {
		if optional && os.IsNotExist(err) {
			return &Ignore{}, nil
		}
		return nil, err
	}
	defer f.Close()
	return ParseIgnore(f)
}

func ParseIgnore(r io.Reader) (*Ignore, error) {
	ig := &Ignore{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		// Conditions are the trailing KEY=GLOB words; a display name glob
		// before them may hold spaces.
		words := strings.Fields(l)
		var conds []ignoreCond
		for len(words) > 1 {
			m := condition.FindStringSubmatch(words[len(words)-1])
			if m == nil {
				break
			}
			c := ignoreCond{attr: m[1] != "", path: []string{m[2]}, val: glob.Compile(m[3])}
			if c.attr {
				c.path = strings.Split(m[2], ".")
			} else if strings.Contains(m[2], ".") {
				return nil, fmt.Errorf("line %d: %s: identity keys have no path; attribute paths start with attr.", n, words[len(words)-1])
			}
			conds = append([]ignoreCond{c}, conds...)
			words = words[:len(words)-1]
		}
		typ, name, hasName := strings.Cut(strings.Join(words, " "), ":")
		ig.rules = append(ig.rules, ignoreRule{typ: glob.Compile(strings.TrimSpace(typ)), name: glob.Compile(strings.TrimSpace(name)), hasName: hasName, conds: conds})
	}
	return ig, sc.Err()
}

//go:embed presets/*.ignore
var presets embed.FS

// Presets lists the built-in ignore presets.
func Presets() []string {
	entries, _ := presets.ReadDir("presets")
	var out []string
	for _, e := range entries {
		out = append(out, strings.TrimSuffix(e.Name(), ".ignore"))
	}
	return out
}

// Preset returns a built-in ignore preset by name.
func Preset(name string) (*Ignore, error) {
	data, err := presets.ReadFile("presets/" + name + ".ignore")
	if err != nil || strings.ContainsAny(name, "/.") {
		return nil, fmt.Errorf("unknown ignore preset %q (have %s)", name, strings.Join(Presets(), ", "))
	}
	return ParseIgnore(strings.NewReader(string(data)))
}

// Merge adds the rules of other.
func (ig *Ignore) Merge(other *Ignore) { ig.rules = append(ig.rules, other.rules...) }

// Attributes returns the top-level attributes that rules for typ look at,
// which have to be fetched (include_resource) for the type.
func (ig *Ignore) Attributes(typ string) []string {
	var out []string
	for _, rule := range ig.rules {
		if !rule.typ.Match(typ) {
			continue
		}
		for _, c := range rule.conds {
			if c.attr && !slices.Contains(out, c.path[0]) {
				out = append(out, c.path[0])
			}
		}
	}
	return out
}

func (ig *Ignore) Match(r model.Resource) bool {
	for _, rule := range ig.rules {
		if rule.typ.Match(r.Type) && rule.matchName(r) && rule.matchConds(r) {
			return true
		}
	}
	return false
}

func (rule ignoreRule) matchName(r model.Resource) bool {
	if !rule.hasName {
		return true
	}
	cands := []string{r.DisplayName, r.ImportID}
	for _, v := range r.Identity {
		if s, ok := model.Scalar(v); ok {
			cands = append(cands, s)
		}
	}
	for _, c := range cands {
		if c != "" && rule.name.Match(c) {
			return true
		}
	}
	return false
}

func (rule ignoreRule) matchConds(r model.Resource) bool {
	for _, c := range rule.conds {
		var v any = r.Identity
		if c.attr {
			v = r.Attributes
		}
		for _, p := range c.path {
			m, ok := v.(map[string]any)
			if !ok {
				return false
			}
			if v, ok = m[p]; !ok {
				return false
			}
		}
		s, ok := model.Scalar(v)
		if !ok || !c.val.Match(s) {
			return false
		}
	}
	return true
}

// Summary describes how complete the scan was.
type Summary struct {
	States int `json:"states"`
	// Unverified counts objects matched by ID only: state without
	// identity could not confirm their account or subscription.
	Unverified int          `json:"unverified_matches"`
	Live       int          `json:"live"`
	Managed    int          `json:"managed"`
	Ignored    int          `json:"ignored"`
	Orphans    int          `json:"orphans"`
	Queried    int          `json:"queried_types"`
	Skipped    []tf.Skip    `json:"skipped_types,omitempty"`
	Failed     []tf.Failure `json:"failed_queries,omitempty"`
	Warnings   []string     `json:"warnings,omitempty"`
	// Notes are hints that do not make coverage incomplete.
	Notes []string `json:"notes,omitempty"`
	// NoList are providers whose installed version has no list resources.
	NoList []string `json:"providers_without_list,omitempty"`
	// ByType counts the live objects of each type by outcome.
	ByType map[string]*TypeCount `json:"by_type"`
}

type TypeCount struct {
	Live    int `json:"live"`
	Managed int `json:"managed"`
	Ignored int `json:"ignored"`
	Orphans int `json:"orphans"`
}

// SummaryTable prints the counts per type, types with orphans first, and
// the types that were not fully listed.
func SummaryTable(w io.Writer, s Summary) error {
	types := slices.Collect(maps.Keys(s.ByType))
	sort.Slice(types, func(i, j int) bool {
		a, b := s.ByType[types[i]], s.ByType[types[j]]
		if a.Orphans != b.Orphans {
			return a.Orphans > b.Orphans
		}
		return types[i] < types[j]
	})
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "TYPE\tLIVE\tMANAGED\tIGNORED\tUNMANAGED\t")
	for _, t := range types {
		c := s.ByType[t]
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t\n", Clean(t), c.Live, c.Managed, c.Ignored, c.Orphans)
	}
	fmt.Fprintf(tw, "total\t%d\t%d\t%d\t%d\t\n", s.Live, s.Managed, s.Ignored, s.Orphans)
	if err := tw.Flush(); err != nil {
		return err
	}
	var failed []string
	for _, f := range s.Failed {
		if !slices.Contains(failed, f.Type) {
			failed = append(failed, f.Type)
		}
	}
	if len(failed) > 0 {
		slices.Sort(failed)
		_, err := fmt.Fprintf(w, "\nnot fully listed (%d): %s\n", len(failed), Clean(strings.Join(failed, ", ")))
		return err
	}
	return nil
}

func Text(w io.Writer, orphans []model.Resource) error {
	if len(orphans) == 0 {
		_, err := fmt.Fprintln(w, "No unmanaged resources found.")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TYPE\tNAME\tIDENTITY\tSOURCE")
	for _, r := range orphans {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", Clean(r.Type), Clean(r.DisplayName), Clean(r.IdentityString()), Clean(r.Source))
	}
	return tw.Flush()
}

func JSON(w io.Writer, orphans []model.Resource, s Summary) error {
	if orphans == nil {
		orphans = []model.Resource{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(struct {
		Orphans []model.Resource `json:"orphans"`
		Summary Summary          `json:"summary"`
	}{orphans, s})
}

// Import writes Terraform import blocks for the orphans. Feed them to
// `terraform plan -generate-config-out=generated.tf` to get resource blocks.
// When the orphans come from several roots or plugins, the blocks are
// grouped by source, since each group belongs in its own root module.
//
// reserved holds addresses (<type>.<name>) already used by state or
// configuration; generated names avoid them, since importing into a taken
// address either fails or binds the object to someone else's configuration.
//
// It returns the number of blocks written: an object whose type or provider
// is not a plain name is only noted in a comment.
func Import(w io.Writer, orphans []model.Resource, reserved map[string]bool) (int, error) {
	sources := map[string]bool{}
	for _, r := range orphans {
		sources[r.Source] = true
	}
	grouped := len(sources) > 1
	if grouped {
		orphans = append([]model.Resource(nil), orphans...)
		sort.SliceStable(orphans, func(i, j int) bool { return orphans[i].Source < orphans[j].Source })
	}
	used := maps.Clone(reserved)
	if used == nil {
		used = map[string]bool{}
	}
	var b strings.Builder
	written := 0
	for i, r := range orphans {
		// Types and identity keys come from providers and plugins; anything
		// that is not a plain type name is not written as configuration.
		if !typeName.MatchString(r.Type) || (r.Provider != "" && !localName.MatchString(r.Provider)) {
			fmt.Fprintf(&b, "# skipped: %s is not a valid resource type or provider name\n\n", tf.HCLString(r.Type))
			continue
		}
		if grouped && (i == 0 || orphans[i-1].Source != r.Source) {
			fmt.Fprintf(&b, "# Found by %s\n\n", Clean(r.Source))
		}
		base := resourceName(r)
		name := base
		for i := 2; used[r.Type+"."+name]; i++ {
			name = fmt.Sprintf("%s_%d", base, i)
		}
		used[r.Type+"."+name] = true
		fmt.Fprintf(&b, "import {\n  to = %s.%s\n", r.Type, name)
		if r.Provider != "" {
			fmt.Fprintf(&b, "  provider = %s\n", r.Provider)
		}
		if r.UndeclaredProvider != "" {
			fmt.Fprintf(&b, "  # the root module does not declare %s: add it to required_providers\n  # (and set provider = <its local name>), or move this block into the module\n", Clean(r.UndeclaredProvider))
		}
		if r.ImportNote != "" {
			for _, line := range strings.Split(r.ImportNote, "\n") {
				fmt.Fprintf(&b, "  # %s\n", Clean(line))
			}
		}
		if r.ImportID != "" {
			fmt.Fprintf(&b, "  id = %s\n", tf.HCLString(r.ImportID))
		} else {
			b.WriteString("  identity = {\n")
			for _, k := range model.SortedKeys(r.Identity) {
				if r.Identity[k] == nil {
					continue
				}
				fmt.Fprintf(&b, "    %s = %s\n", tf.HCLString(k), hclValue(r.Identity[k]))
			}
			b.WriteString("  }\n")
		}
		b.WriteString("}\n\n")
		written++
	}
	_, err := io.WriteString(w, b.String())
	return written, err
}

var (
	nonIdent  = regexp.MustCompile(`[^a-z0-9]+`)
	typeName  = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)
	localName = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9-]*(\.[a-zA-Z_][a-zA-Z0-9_-]*)?$`)
)

func resourceName(r model.Resource) string {
	n := identName(r.DisplayName)
	if n == "" {
		// A name in another script (Cyrillic VM names) leaves nothing.
		n = identName(r.IdentityString())
	}
	if len(n) > 60 {
		n = strings.TrimRight(n[:60], "_")
	}
	if n == "" || (n[0] >= '0' && n[0] <= '9') {
		n = "r_" + n
	}
	return n
}

// Clean flattens s to one line and drops control characters, since names
// and error messages come from APIs and could carry terminal escape
// sequences.
func Clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// CleanLines drops control characters except newlines and tabs, for
// multi-line messages such as errors with provider diagnostics.
func CleanLines(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, s)
}

// hclValue renders a JSON value as an HCL expression: strings quoted
// without template sequences, lists as tuples, objects with quoted keys.
func hclValue(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return tf.HCLString(t)
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = hclValue(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		parts := make([]string, 0, len(t))
		for _, k := range model.SortedKeys(t) {
			parts = append(parts, tf.HCLString(k)+" = "+hclValue(t[k]))
		}
		return "{ " + strings.Join(parts, ", ") + " }"
	default:
		if s, ok := model.Scalar(t); ok {
			return s
		}
		return "null"
	}
}

func identName(s string) string {
	return strings.Trim(nonIdent.ReplaceAllString(strings.ToLower(s), "_"), "_")
}
