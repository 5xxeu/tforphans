// Package glob implements shell-style patterns where "*" matches any run of
// characters (including "/", "." and newlines) and "?" matches one
// character.
package glob

import (
	"regexp"
	"strings"
)

type Pattern struct {
	raw string
	re  *regexp.Regexp
}

func Compile(p string) Pattern {
	var b strings.Builder
	b.WriteString("(?s)^")
	for _, r := range p {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return Pattern{raw: p, re: regexp.MustCompile(b.String())}
}

func (p Pattern) Match(s string) bool { return p.re.MatchString(s) }

func (p Pattern) String() string { return p.raw }

// List is a set of patterns; an empty list matches nothing.
type List []Pattern

func CompileList(patterns []string) List {
	var l List
	for _, p := range patterns {
		l = append(l, Compile(p))
	}
	return l
}

// Names reports whether one of the patterns is s itself, not a wildcard
// that happens to match it.
func (l List) Names(s string) bool {
	for _, p := range l {
		if p.raw == s {
			return true
		}
	}
	return false
}

func (l List) Match(s string) bool {
	for _, p := range l {
		if p.Match(s) {
			return true
		}
	}
	return false
}
