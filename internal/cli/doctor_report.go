package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode"

	"github.com/5xxeu/tforphans/internal/report"
	"github.com/5xxeu/tforphans/internal/tf"
)

type doctorCheck struct {
	ID          string    `json:"id"`
	Status      string    `json:"status"`
	Message     string    `json:"message"`
	Remediation string    `json:"remediation,omitempty"`
	Root        string    `json:"root,omitempty"`
	Provider    string    `json:"provider,omitempty"`
	Category    string    `json:"category,omitempty"`
	Types       []string  `json:"types,omitempty"`
	Read        *int      `json:"read,omitempty"`
	Total       *int      `json:"total,omitempty"`
	Blocks      *int      `json:"blocks,omitempty"`
	Count       *int      `json:"count,omitempty"`
	Skipped     []tf.Skip `json:"skipped_types,omitempty"`
}

type doctorReport struct {
	SchemaVersion int           `json:"schema_version"`
	Status        string        `json:"status"`
	Checks        []doctorCheck `json:"checks"`
	NextCommand   []string      `json:"next_command"`
	Notes         []string      `json:"notes,omitempty"`
	incomplete    bool
	fixes         []string
	queries       int
	scanBlocks    int
	providers     []string
	argFixes      []doctorArgFix
	verbose       bool
	// recheck are the doctor-only flags the next doctor run repeats.
	recheck []string
}

type doctorArgFix struct {
	check            int
	typ, name, value string
	userList         string
}

func (d *doctorReport) add(id, status, message, root, remediation string) {
	d.Checks = append(d.Checks, doctorCheck{ID: id, Status: status, Message: report.Clean(message), Root: report.Clean(root), Remediation: remediation})
}

func (d *doctorReport) result(offline bool) int {
	code := 0
	d.Status = "ready"
	for _, c := range d.Checks {
		if c.Status == "failed" {
			code, d.Status = 2, "needs_configuration"
		}
	}
	if offline {
		d.Status = "unknown"
	}
	if d.incomplete {
		code, d.Status = 1, "unknown"
	}
	return code
}

func (d doctorReport) write(w io.Writer, format string) error {
	d.NextCommand = slices.Clone(d.NextCommand)
	unsafe := false
	for i, arg := range d.NextCommand {
		arg = redactArgURLs(arg)
		d.NextCommand[i] = arg
		if strings.ContainsFunc(arg, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) {
			d.NextCommand[i] = "<argument with control characters>"
			unsafe = true
		}
	}
	if unsafe {
		d.Notes = append(slices.Clone(d.Notes), "Next run contains a placeholder for an argument with control characters; supply it manually.")
	}

	if format == "json" {
		return json.NewEncoder(w).Encode(d)
	}
	ew := &errWriter{w: w}
	counts := map[string]int{}
	for _, c := range d.Checks {
		counts[c.Status]++
	}
	failed := counts["failed"] > 0
	verdict := map[string]string{"ready": "READY", "needs_configuration": "BLOCKED", "unknown": "UNKNOWN"}[d.Status]
	tone := map[string]report.Tone{"ready": report.Good, "needs_configuration": report.Failure, "unknown": report.Attention}[d.Status]
	fmt.Fprintln(ew, report.SectionLine(w, "Readiness"))
	hint := "-v lists every check"
	if d.verbose {
		hint = "every check below"
	}
	fmt.Fprintf(ew, "Result: %s (%s, %s; %d passed, %d skipped; %s)\n", report.Accent(w, tone, verdict),
		plural(counts["failed"], "problem"), plural(counts["warning"], "warning"), counts["ok"], counts["skipped"], hint)
	if d.verbose {
		fmt.Fprint(ew, "\n"+report.SectionLine(w, "Readiness checks")+"\n")
		for _, g := range doctorGroups(d.Checks, false) {
			d.writeGroup(ew, g)
		}
	} else {
		// Problems in the order they are fixed: states and roots before
		// discovery, which depends on them; warnings after.
		for _, part := range []struct{ title, status string }{{"Problems", "failed"}, {"Warnings", "warning"}} {
			var shown []doctorCheck
			for _, c := range d.Checks {
				if c.Status == part.status {
					shown = append(shown, c)
				}
			}
			if len(shown) == 0 {
				continue
			}
			slices.SortStableFunc(shown, func(a, b doctorCheck) int { return doctorRank(a.ID) - doctorRank(b.ID) })
			fmt.Fprint(ew, "\n"+report.SectionLine(w, part.title)+"\n")
			for _, g := range doctorGroups(shown, true) {
				d.writeGroup(ew, g)
			}
		}
	}
	fmt.Fprint(ew, "\n"+report.SectionLine(w, "Next")+"\n")
	for _, note := range d.Notes {
		fmt.Fprintln(ew, report.Clean(note))
	}
	parts := make([]string, len(d.NextCommand))
	for i, a := range d.NextCommand {
		parts[i] = shellArg(a)
	}
	if failed {
		// An audit now would fail or miss what the problems hide: check
		// again with the suggested flags first.
		recheck := slices.Insert(slices.Clone(parts), 1, append([]string{"doctor"}, d.recheck...)...)
		fmt.Fprintln(ew, "Fix the problems above in order, then check again:\n  "+strings.Join(recheck, " "))
	} else {
		fmt.Fprintln(ew, "Next run:\n  "+strings.Join(parts, " "))
	}
	return ew.err
}

// errWriter keeps the first write error, so that a long report is not a
// chain of checks.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.w.Write(p)
	e.err = err
	return n, err
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// doctorRank orders checks for fixing: what later checks depend on first.
func doctorRank(id string) int {
	switch {
	case id == "config" || id == "ignore" || id == "run":
		return 0
	case id == "terraform.version":
		return 1
	case id == "roots" || strings.HasPrefix(id, "root."):
		return 2
	case strings.HasPrefix(id, "states."):
		return 3
	case id == "providers.schema" || strings.HasPrefix(id, "query.") || strings.HasPrefix(id, "scan."):
		return 4
	case id == "discovery":
		return 5
	default:
		return 6
	}
}

// doctorGroup is one line of text: a check, or the same check with the
// same outcome in several roots.
type doctorGroup struct {
	doctorCheck
	roots []string
}

// doctorGroups merges checks that differ only in their root when merge is
// set; a root in the remediation of a merged check becomes <root>.
func doctorGroups(checks []doctorCheck, merge bool) []doctorGroup {
	var out []doctorGroup
	at := map[string]int{}
	for _, c := range checks {
		g := doctorGroup{doctorCheck: c}
		if c.Root != "" {
			g.roots = []string{c.Root}
		}
		if !merge || c.Root == "" {
			out = append(out, g)
			continue
		}
		fix := strings.ReplaceAll(c.Remediation, shellQuote(c.Root), "<root>")
		key := strings.Join([]string{c.ID, c.Status, c.Message, c.Category, strings.Join(c.Types, ","), fix}, "\x00")
		if i, ok := at[key]; ok {
			out[i].roots = append(out[i].roots, c.Root)
			out[i].Remediation = fix
			continue
		}
		at[key] = len(out)
		out = append(out, g)
	}
	return out
}

func (d doctorReport) writeGroup(w io.Writer, g doctorGroup) {
	tones := map[string]report.Tone{"ok": report.Good, "warning": report.Attention, "failed": report.Failure, "skipped": report.Muted}
	labels := map[string]string{"ok": "OK", "warning": "WARN", "failed": "FAIL", "skipped": "SKIP"}
	where := ""
	switch n := len(g.roots); {
	case n == 1:
		where = "[" + g.roots[0] + "]"
	case n > 1:
		shown := g.roots
		if !d.verbose {
			shown = firstN(g.roots, 8)
		}
		where = fmt.Sprintf("[%d roots: %s]", n, strings.Join(shown, ", "))
	}
	message := g.Message
	if g.Category != "" {
		message = doctorSkipMessage(g.Category, g.Types, d.verbose)
	}
	if where != "" {
		message += " " + report.Clean(where)
	}
	fmt.Fprintf(w, "%s %-23s %s\n", report.Accent(w, tones[g.Status], fmt.Sprintf("%-5s", labels[g.Status])), g.ID, message)
	if g.Remediation != "" {
		fmt.Fprintln(w, "      Fix:", report.Clean(g.Remediation))
	}
}
