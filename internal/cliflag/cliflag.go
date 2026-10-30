// Package cliflag parses the flags of tforphans commands: help goes to
// stdout, a parse error comes back for the caller to print in one line, and
// the flags are listed in groups.
package cliflag

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
)

// Group is a heading of -h and the names of its flags, in the order shown.
type Group struct {
	Title string
	Names []string
}

// Parse parses args with fs. On -h it prints help to out and returns
// flag.ErrHelp. A parse error is returned without printing: the caller
// prints it with HelpHint, not with the whole usage, which would push the
// error off the screen.
func Parse(fs *flag.FlagSet, args []string, out io.Writer, help func(io.Writer)) error {
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	err := fs.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		help(out)
	}
	return err
}

// HelpHint is the line printed after a parse error.
func HelpHint(command string) string {
	return fmt.Sprintf("Run %q for usage.", command+" -h")
}

// PrintGroups prints the flags of fs under the headings of groups, each
// flag as flag.PrintDefaults prints it. Names fs does not define are
// skipped; flags no group names follow under "Other flags".
func PrintGroups(w io.Writer, fs *flag.FlagSet, groups []Group) {
	listed := map[string]bool{}
	for _, g := range groups {
		var names []string
		for _, n := range g.Names {
			if fs.Lookup(n) != nil {
				names = append(names, n)
				listed[n] = true
			}
		}
		printFlags(w, fs, g.Title, names)
	}
	var rest []string
	fs.VisitAll(func(f *flag.Flag) {
		if !listed[f.Name] {
			rest = append(rest, f.Name)
		}
	})
	printFlags(w, fs, "Other flags", rest)
}

// Ungrouped returns the flags of fs that no group names.
func Ungrouped(fs *flag.FlagSet, groups []Group) []string {
	var rest []string
	fs.VisitAll(func(f *flag.Flag) {
		if !slices.ContainsFunc(groups, func(g Group) bool { return slices.Contains(g.Names, f.Name) }) {
			rest = append(rest, f.Name)
		}
	})
	return rest
}

func printFlags(w io.Writer, fs *flag.FlagSet, title string, names []string) {
	if len(names) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s:\n", title)
	// A set of one flag prints it exactly as PrintDefaults does, defaults
	// included.
	one := flag.NewFlagSet("", flag.ContinueOnError)
	var buf bytes.Buffer
	one.SetOutput(&buf)
	for _, n := range names {
		f := fs.Lookup(n)
		one.Var(f.Value, f.Name, f.Usage)
		// Var takes the current value as the default: restore the real one.
		one.Lookup(n).DefValue = f.DefValue
	}
	one.PrintDefaults()
	// PrintDefaults sorts by name: keep the order of the group.
	byName := map[string][]byte{}
	var cur string
	for _, line := range bytes.SplitAfter(buf.Bytes(), []byte("\n")) {
		if bytes.HasPrefix(line, []byte("  -")) {
			name := string(bytes.TrimPrefix(line, []byte("  -")))
			if i := bytes.IndexAny([]byte(name), " \t\n"); i >= 0 {
				name = name[:i]
			}
			cur = name
		}
		byName[cur] = append(byName[cur], line...)
	}
	for _, n := range names {
		_, _ = w.Write(byName[n])
	}
}
