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
	"github.com/5xxeu/tforphans/internal/state"
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
				return nil, fmt.Errorf("line %d: %s: identity keys have no path; attribute paths start with \"attr.\"", n, words[len(words)-1])
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

// DefaultPresets are applied unless -no-default-presets: they hide only
// objects known by marks a user cannot give their own (reserved names,
// labels and domains), or whose hiding does no harm.
var DefaultPresets = []string{"aws-system", "cloudflare-system", "gcp-system"}

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
	cands := []string{r.DisplayName, r.BaseName, r.ImportID}
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

// UnreadState is a root module whose state could not be read, with the
// line of the error that says why.
type UnreadState struct {
	Root  string `json:"root"`
	Error string `json:"error"`
}

// NewUnreadState keeps the telling line of err: terraform's "Error: ..."
// line when there is one, else the first.
//
// Errors of several workspaces ("workspace NAME: ...") are shortened one by
// one and keep their names.
func NewUnreadState(root string, err error) UnreadState {
	var parts []string
	ws, first, found := "", "", false
	flush := func() {
		if !found && first != "" && !slices.Contains(parts, first) {
			parts = append(parts, first)
		}
	}
	for _, l := range strings.Split(err.Error(), "\n") {
		l = strings.TrimSpace(l)
		if name, ok := strings.CutPrefix(l, "workspace "); ok {
			if n, _, ok := strings.Cut(name, ": "); ok {
				flush()
				ws, first, found = "workspace "+n+": ", l, false
				continue
			}
		}
		if first == "" {
			first = ws + l
		}
		if strings.HasPrefix(l, "Error: ") && !slices.Contains(parts, ws+l) {
			parts = append(parts, ws+l)
			found = true
		}
	}
	flush()
	return UnreadState{Root: root, Error: strings.Join(parts, "; ")}
}

// Summary describes how complete the scan was.
type Summary struct {
	States                     int               `json:"states"`
	DuplicateOwnership         int               `json:"duplicate_ownership"`
	PossibleDuplicateOwnership int               `json:"possible_duplicate_ownership"`
	DuplicateInState           int               `json:"duplicate_in_state"`
	Ownership                  []state.Duplicate `json:"-"`
	InState                    []state.Duplicate `json:"-"`
	// UnreadStates are root modules whose state could not be read in full:
	// their objects may show as unmanaged.
	UnreadStates []UnreadState `json:"unread_states,omitempty"`
	// ManagedByIdentity counts managed objects matched by the resource
	// identity in state; the others were matched by attributes.
	ManagedByIdentity int `json:"managed_by_identity"`
	Live              int `json:"live"`
	// Managed counts objects matched to state, with their account or
	// cluster confirmed.
	Managed int `json:"managed"`
	// Uncertain counts objects matched to state whose account or cluster
	// state could not confirm (no identity, Proxmox, plugin results marked
	// unverified): managed, or an object of the same ID elsewhere.
	Uncertain int `json:"uncertain"`
	Ignored   int `json:"ignored"`
	Orphans   int `json:"orphans"`
	Queried   int `json:"queried_types"`
	// APIOff counts failed listings whose API is not enabled (they are in
	// failed_queries too); they count as gaps only with -strict-api.
	APIOff int `json:"api_not_enabled,omitempty"`
	// StrictIgnored counts failed listings and skipped types that
	// -strict-ignore kept from making coverage incomplete.
	StrictIgnored int `json:"strict_ignored,omitempty"`
	// Complete is false when something was not checked: failed or
	// truncated listings, unread states, plugin errors, types that needed
	// a check and were skipped.
	Complete          bool               `json:"complete"`
	StatesComplete    bool               `json:"states_complete"`
	DiscoveryComplete bool               `json:"discovery_complete"`
	Providers         []ProviderCoverage `json:"providers,omitempty"`
	// Ran counts list block instances that started and types that scans
	// listed: what was actually checked, unlike Queried, which counts
	// blocks (a block with for_each = {} runs nothing).
	Ran int `json:"-"`
	// Checks are the listings that ran to the end, with or without
	// results.
	Checks   []tf.Check   `json:"-"`
	Skipped  []tf.Skip    `json:"skipped_types,omitempty"`
	Failed   []tf.Failure `json:"failed_queries,omitempty"`
	Warnings []string     `json:"warnings,omitempty"`
	// Notes are hints that do not make coverage incomplete.
	Notes []string `json:"notes,omitempty"`
	// NoList are providers whose installed version has no list resources.
	NoList []string `json:"providers_without_list,omitempty"`
	// ByType counts the live objects of each type by outcome.
	ByType map[string]*TypeCount `json:"by_type"`
	// ByScope counts them by where they live ("zone example.com", "project
	// p"), when that is known.
	ByScope map[string]*TypeCount `json:"by_scope,omitempty"`
	// Coverage says for every type and scope whether it was listed in full.
	Coverage []Coverage `json:"coverage"`
}

type TypeCount struct {
	Live      int `json:"live"`
	Managed   int `json:"managed"`
	Uncertain int `json:"uncertain"`
	Ignored   int `json:"ignored"`
	Orphans   int `json:"orphans"`
}

// Uncertain is a live object matched to state without proof that the state
// instance is in the same account or cluster.
type Uncertain struct {
	model.Resource
	StateAddress string `json:"state_address"`
}

// Coverage statuses.
const (
	Complete   = "complete"   // listed, nothing failed
	Incomplete = "incomplete" // a listing failed or was cut off
	Skipped    = "skipped"    // left out on purpose or cannot be listed (see skipped_types)
)

// Coverage is how far one type was listed in one scope. Scope "" stands for
// the type as a whole: a failure without a scope concerns all of them.
type Coverage struct {
	Type   string `json:"type"`
	Scope  string `json:"scope,omitempty"`
	Status string `json:"status"`
	// Reasons are the failure categories (see failed_queries), the
	// reason a type was skipped, or GapPrefix and the reason a type that
	// needed a check was skipped.
	Reasons []string `json:"reasons,omitempty"`
	TypeCount
}

// GapPrefix starts the reason of a type that needed a check and was
// skipped (see BuildCoverage).
const GapPrefix = "skipped: "

// StrictIgnored starts the reason of a failure or a skip that -strict-ignore
// kept from being a gap.
const StrictIgnored = "-strict-ignore: "

// BuildCoverage combines the counts per type and scope with the listings
// that ran (checks), the types skipped and the listings that failed.
// Every scope a check read gets a row, also one without objects. A type
// skipped where a check was needed (a required argument, a provider with
// only aliases) is incomplete unless the same root listed it through the
// same provider (the user's own list block with an alias of it) and, for a
// scan, in the same kind of scope: another root, provider or kind of scope
// (accounts for zones) may look elsewhere. A type left out on
// purpose (excluded) is skipped, and only when nothing listed it. failed
// must be classified.
func BuildCoverage(counts map[[2]string]*TypeCount, checks []tf.Check, skipped []tf.Skip, failed []tf.Failure) []Coverage {
	byKey := map[[2]string]*Coverage{}
	types := map[string]bool{}
	entry := func(typ, scope string) *Coverage {
		k := [2]string{typ, scope}
		if byKey[k] == nil {
			byKey[k] = &Coverage{Type: typ, Scope: scope, Status: Complete}
			types[typ] = true
		}
		return byKey[k]
	}
	for k, c := range counts {
		entry(k[0], k[1]).TypeCount = *c
	}
	checked := map[[4]string]bool{} // type, root, provider, scope kind
	for _, c := range checks {
		checked[[4]string{c.Type, c.Root, c.Provider, c.Kind}] = true
		if c.Scope != "" {
			entry(c.Type, c.Scope)
		}
	}
	for _, c := range checks {
		if c.Scope == "" && !types[c.Type] {
			entry(c.Type, "")
		}
	}
	addReason := func(c *Coverage, why string) {
		c.Status = Incomplete
		if !slices.Contains(c.Reasons, why) {
			c.Reasons = append(c.Reasons, why)
		}
	}
	for _, f := range failed {
		why := f.Category
		if why == "" {
			why = tf.FailOther
		}
		if f.Scope != "" || !types[f.Type] {
			addReason(entry(f.Type, f.Scope), why)
			continue
		}
		for _, c := range byKey {
			if c.Type == f.Type {
				addReason(c, why)
			}
		}
	}
	isSkipped := map[string]bool{}
	for _, sk := range skipped {
		if !sk.Excluded {
			if checked[[4]string{sk.Type, sk.Root, sk.Provider, sk.Kind}] {
				continue
			}
			scope := ""
			if sk.Root != "" {
				scope = "root " + sk.Root
			}
			addReason(entry(sk.Type, scope), GapPrefix+sk.Reason)
			continue
		}
		// A disabled API in one root says nothing about another root
		// that listed the type: it keeps a row of its own. So does a gap
		// that -strict-ignore lets pass, to stay visible.
		if sk.Reason == tf.FailAPIOff && sk.Root != "" || strings.HasPrefix(sk.Reason, StrictIgnored) {
			if checked[[4]string{sk.Type, sk.Root, sk.Provider, sk.Kind}] {
				continue
			}
			scope := ""
			if sk.Root != "" {
				scope = "root " + sk.Root
			}
			c := entry(sk.Type, scope)
			if c.Status == Complete {
				c.Status = Skipped
			}
			if !slices.Contains(c.Reasons, sk.Reason) {
				c.Reasons = append(c.Reasons, sk.Reason)
			}
			continue
		}
		if types[sk.Type] && !isSkipped[sk.Type] {
			continue
		}
		isSkipped[sk.Type] = true
		c := entry(sk.Type, "")
		if c.Status == Complete {
			c.Status = Skipped
		}
		if !slices.Contains(c.Reasons, sk.Reason) {
			c.Reasons = append(c.Reasons, sk.Reason)
		}
	}
	out := make([]Coverage, 0, len(byKey))
	for _, c := range byKey {
		slices.Sort(c.Reasons)
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].Scope < out[j].Scope
	})
	return out
}

// typeStatus folds the coverage of each type over its scopes: incomplete
// when one scope is.
func typeStatus(cov []Coverage) map[string]string {
	out := map[string]string{}
	for _, c := range cov {
		if out[c.Type] == "" || c.Status == Incomplete {
			out[c.Type] = c.Status
		}
	}
	return out
}

// SummaryTable prints the counts per type, types with orphans first, and
// the types that were not fully listed.
func SummaryTable(w io.Writer, s Summary) error {
	// Incomplete types without objects get a row too: a skipped or failed
	// type is where unmanaged objects may hide.
	byType := maps.Clone(s.ByType)
	if byType == nil {
		byType = map[string]*TypeCount{}
	}
	status := typeStatus(s.Coverage)
	for t, st := range status {
		if st == Incomplete && byType[t] == nil {
			byType[t] = &TypeCount{}
		}
	}
	if err := countTable(w, "TYPE", byType, s, status); err != nil {
		return err
	}
	if len(s.ByScope) > 0 {
		fmt.Fprintln(w)
		if err := countTable(w, "SCOPE", s.ByScope, s, nil); err != nil {
			return err
		}
	}
	byCat := map[string][]string{}
	var cats []string
	for _, f := range s.Failed {
		if !slices.Contains(cats, f.Category) {
			cats = append(cats, f.Category)
		}
		if !slices.Contains(byCat[f.Category], f.Type) {
			byCat[f.Category] = append(byCat[f.Category], f.Type)
		}
	}
	const gap = "skipped, needed a check"
	for _, c := range s.Coverage {
		if !slices.ContainsFunc(c.Reasons, func(r string) bool { return strings.HasPrefix(r, GapPrefix) }) {
			continue
		}
		if !slices.Contains(cats, gap) {
			cats = append(cats, gap)
		}
		if !slices.Contains(byCat[gap], c.Type) {
			byCat[gap] = append(byCat[gap], c.Type)
		}
	}
	if len(cats) > 0 {
		fmt.Fprintln(w, "\nnot fully listed:")
	}
	slices.Sort(cats)
	for _, c := range cats {
		types := byCat[c]
		slices.Sort(types)
		if c == "" {
			c = "failed"
		}
		if _, err := fmt.Fprintf(w, "  %s (%d): %s\n", c, len(types), Clean(strings.Join(types, ", "))); err != nil {
			return err
		}
	}
	return nil
}

// countTable prints counts by type or scope, most unmanaged first, with the
// coverage of each row when status is given.
func countTable(w io.Writer, title string, counts map[string]*TypeCount, s Summary, status map[string]string) error {
	keys := slices.Collect(maps.Keys(counts))
	sort.Slice(keys, func(i, j int) bool {
		a, b := counts[keys[i]], counts[keys[j]]
		if a.Orphans != b.Orphans {
			return a.Orphans > b.Orphans
		}
		if a.Uncertain != b.Uncertain {
			return a.Uncertain > b.Uncertain
		}
		return keys[i] < keys[j]
	})
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', tabwriter.AlignRight)
	fmt.Fprint(tw, title+"\tLIVE\tMANAGED\tUNCERTAIN\tIGNORED\tUNMANAGED\t")
	if status != nil {
		fmt.Fprint(tw, "COVERAGE\t")
	}
	fmt.Fprintln(tw)
	for _, k := range keys {
		c := counts[k]
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t", Clean(k), c.Live, c.Managed, c.Uncertain, c.Ignored, c.Orphans)
		if status != nil {
			fmt.Fprintf(tw, "%s\t", status[k])
		}
		fmt.Fprintln(tw)
	}
	fmt.Fprintf(tw, "total\t%d\t%d\t%d\t%d\t%d\t", s.Live, s.Managed, s.Uncertain, s.Ignored, s.Orphans)
	if status != nil {
		total := Complete
		if !s.Complete {
			total = Incomplete
		}
		fmt.Fprintf(tw, "%s\t", total)
	}
	fmt.Fprintln(tw)
	return tw.Flush()
}

// Text prints the unmanaged objects, then those that may be managed.
func Text(w io.Writer, orphans []model.Resource, uncertain []Uncertain) error {
	if len(orphans) == 0 {
		fmt.Fprintln(w, "No unmanaged resources found.")
	} else {
		scoped := slices.ContainsFunc(orphans, func(r model.Resource) bool { return r.ScopeName != "" })
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		if scoped {
			fmt.Fprint(tw, "SCOPE\t")
		}
		fmt.Fprintln(tw, "TYPE\tNAME\tIDENTITY\tSOURCE")
		for _, r := range orphans {
			if scoped {
				fmt.Fprintf(tw, "%s\t", Clean(r.ScopeName))
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", Clean(r.Type), Clean(r.DisplayName), Clean(r.IdentityString()), Clean(r.Source))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if len(uncertain) == 0 {
		return nil
	}
	fmt.Fprintln(w, "\nUncertain (matched to state, but the state cannot confirm the account or cluster):")
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TYPE\tNAME\tIDENTITY\tSOURCE\tSTATE ADDRESS")
	for _, u := range uncertain {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", Clean(u.Type), Clean(u.DisplayName), Clean(u.IdentityString()), Clean(u.Source), Clean(u.StateAddress))
	}
	return tw.Flush()
}

func JSON(w io.Writer, orphans []model.Resource, uncertain []Uncertain, s Summary, baseline ...*Baseline) error {
	if orphans == nil {
		orphans = []model.Resource{}
	}
	if uncertain == nil {
		uncertain = []Uncertain{}
	}
	if s.Coverage == nil {
		s.Coverage = []Coverage{}
	}
	var b *Baseline
	if len(baseline) > 0 {
		b = baseline[0]
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(struct {
		SchemaVersion      int               `json:"schema_version"`
		Orphans            []model.Resource  `json:"orphans"`
		Uncertain          []Uncertain       `json:"uncertain"`
		Summary            Summary           `json:"summary"`
		DuplicateOwnership []state.Duplicate `json:"duplicate_ownership,omitempty"`
		DuplicateInState   []state.Duplicate `json:"duplicate_in_state,omitempty"`
		Baseline           *Baseline         `json:"baseline,omitempty"`
	}{SchemaVersion, orphans, uncertain, s, s.Ownership, s.InState, b})
}

// SchemaVersion of the JSON output. It goes up when a field is removed or
// changes its meaning or type; new fields do not change it.
const SchemaVersion = 1

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
		if r.ScopeName != "" {
			fmt.Fprintf(&b, "  # in %s\n", Clean(r.ScopeName))
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
	base := r.DisplayName
	if r.BaseName != "" {
		base = r.BaseName
	}
	n := identName(base)
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
