package glob

import "testing"

func TestMatch(t *testing.T) {
	for _, c := range []struct {
		pattern, s string
		want       bool
	}{
		{"aws_*", "aws_vpc", true},
		{"aws_*", "google_vpc", false},
		{"*", "", true},
		{"a*b", "a/x.y/b", true},
		{"?", "ж", true},
		{"?", "ab", false},
		{"a.b", "axb", false},
		{"a+b", "a+b", true},
		{"[x]", "[x]", true},
		{"[x]", "x", false},
		{"*key*", "line one\nkey\nline three", true},
		{"a?b", "a\nb", true},
		{"abc", "abc\n", false},
		{"abc", "xabc", false},
	} {
		if got := Compile(c.pattern).Match(c.s); got != c.want {
			t.Errorf("%q on %q: got %v", c.pattern, c.s, got)
		}
	}
}

func TestList(t *testing.T) {
	var empty List
	if empty.Match("x") {
		t.Error("empty list matched")
	}
	l := CompileList([]string{"aws_*", "google_project"})
	if !l.Match("aws_vpc") || !l.Match("google_project") || l.Match("google_folder") {
		t.Error("Match")
	}
	if !l.Names("google_project") || l.Names("aws_vpc") || !l.Names("aws_*") {
		t.Error("Names")
	}
	if l[0].String() != "aws_*" {
		t.Error("String")
	}
}
