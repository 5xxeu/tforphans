// Package report filters and prints unmanaged resources.
package report

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/5xxeu/tforphans/internal/glob"
	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/tf"
)

// Ignore holds rules of the form TYPE_GLOB[:NAME_GLOB]. NAME_GLOB is matched
// against the display name, the import ID and every identity value.
type Ignore struct {
	rules []ignoreRule
}

type ignoreRule struct {
	typ, name glob.Pattern
	hasName   bool
}

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
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		typ, name, hasName := strings.Cut(l, ":")
		ig.rules = append(ig.rules, ignoreRule{typ: glob.Compile(strings.TrimSpace(typ)), name: glob.Compile(strings.TrimSpace(name)), hasName: hasName})
	}
	return ig, sc.Err()
}

func (ig *Ignore) Match(r model.Resource) bool {
	for _, rule := range ig.rules {
		if !rule.typ.Match(r.Type) {
			continue
		}
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
	}
	return false
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
func Import(w io.Writer, orphans []model.Resource, reserved map[string]bool) error {
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
	}
	_, err := io.WriteString(w, b.String())
	return err
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
