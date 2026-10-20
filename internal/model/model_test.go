package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSameProvider(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"", "registry.terraform.io/hashicorp/aws", true},
		{"registry.terraform.io/hashicorp/aws", "registry.terraform.io/hashicorp/aws", true},
		{"registry.terraform.io/hashicorp/aws", "registry.opentofu.org/hashicorp/aws", true},
		{"registry.terraform.io/hashicorp/aws", "hashicorp/aws", true},
		{"registry.terraform.io/hashicorp/aws", "aws", true},
		{"registry.terraform.io/Hashicorp/AWS", "hashicorp/aws", true},
		{"registry.terraform.io/hashicorp/google", "registry.terraform.io/hashicorp/google-beta", true},
		{"registry.terraform.io/hashicorp/github", "registry.terraform.io/integrations/github", true},
		{"registry.terraform.io/acme/foo", "registry.terraform.io/other/foo", false},
		{"example.com/fake/fake", "example.com/fake2/fake", false},
		{"example.com/acme/foo", "registry.terraform.io/acme/foo", false},
	} {
		if got := SameProvider(c.a, c.b); got != c.want {
			t.Errorf("SameProvider(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func TestCanonical(t *testing.T) {
	for _, c := range []struct {
		v    any
		want string
		ok   bool
	}{
		{"x", "x", true},
		{"", "", true},
		{json.Number("100000000000000000000001"), "100000000000000000000001", true},
		{true, "true", true},
		{false, "false", true},
		{nil, "", false},
		{[]any{"b", "a"}, `["b","a"]`, true},
		{map[string]any{"b": "1", "a": json.Number("2")}, `{"a":2,"b":"1"}`, true},
		{float64(1), "", false}, // not decoded with DecodeJSON
	} {
		got, ok := Canonical(c.v)
		if got != c.want || ok != c.ok {
			t.Errorf("Canonical(%#v) = %q, %v", c.v, got, ok)
		}
	}
	if _, ok := Scalar([]any{"x"}); ok {
		t.Error("a list is not a scalar")
	}
}

func TestKey(t *testing.T) {
	a := Resource{Type: "t", Identity: map[string]any{"id": "1", "zone": "z", "gone": nil}}
	b := Resource{Type: "t", Identity: map[string]any{"zone": "z", "id": "1"}}
	if a.Key() != b.Key() {
		t.Errorf("key order or null changed the key: %q, %q", a.Key(), b.Key())
	}
	for _, other := range []Resource{
		{Type: "u", Identity: b.Identity},
		{Type: "t", Identity: map[string]any{"id": "1", "zone": "y"}},
		// The separator cannot be forged by a value.
		{Type: "t", Identity: map[string]any{"id": "1\x00zone=z"}},
	} {
		if other.Key() == a.Key() {
			t.Errorf("%+v has the key of %+v", other, a)
		}
	}
	if got := a.IdentityString(); got != "id=1, zone=z" {
		t.Errorf("IdentityString: %q", got)
	}
}

func TestDecodeJSON(t *testing.T) {
	var v map[string]any
	if err := DecodeJSON([]byte(`{"n": 12345678901234567890}`), &v); err != nil || v["n"] != json.Number("12345678901234567890") {
		t.Errorf("number: %v %#v", err, v)
	}
	if err := DecodeJSON([]byte(" {\"a\": 1}\n"), &v); err != nil {
		t.Errorf("surrounding space: %v", err)
	}
	for _, bad := range []string{`{"a":1}{"b":2}`, `{"a":1} x`, `{"a":`, ``} {
		if err := DecodeJSON([]byte(bad), &v); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestDecodeJSONUnique(t *testing.T) {
	var v map[string]any
	for _, ok := range []string{`{"a":{"x":1},"b":{"x":2}}`, `[{"a":1},{"a":2}]`, `{"a":[{"x":1,"y":2}]}`, `"s"`, `{}`,
		// Nested objects are maps: keys differing in case are two keys.
		`{"type":"t","attributes":{"tags":{"Env":"prod","env":"test"}}}`, `{"identity":{"Name":"a","name":"b"}}`} {
		if err := DecodeJSONUnique([]byte(ok), new(any)); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{
		`{"done":false,"done":true}`,
		`{"done":false,"\u0064one":true}`,
		`{"type":"a","Type":"b"}`,
		"{\"s\":1,\"\u017f\":2}", // long s folds to s
		`{"identity":{"name":"a","name":"b"}}`,
		`{"identity":{"name":"a","n\u0061me":"b"}}`,
		`{"a":[{"x":1,"x":2}]}`,
		`{"a":1}{"b":2}`,
		`{"a":`,
	} {
		if err := DecodeJSONUnique([]byte(bad), &v); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
	// Deep nesting is an error, not a stack overflow; what encoding/json
	// accepts still passes.
	deep := strings.Repeat("[", 10_000_000)
	if err := DecodeJSONUnique([]byte(deep), new(any)); err == nil {
		t.Error("deep nesting: no error")
	}
	ok := strings.Repeat("[", 9000) + strings.Repeat("]", 9000)
	if err := DecodeJSONUnique([]byte(ok), new(any)); err != nil {
		t.Errorf("nesting 9000: %v", err)
	}
}

func TestProviderFamily(t *testing.T) {
	for in, want := range map[string]string{
		"aws": "hashicorp/aws",
		"registry.terraform.io/hashicorp/google-beta": "hashicorp/google",
		"registry.opentofu.org/integrations/github":   "integrations/github",
		"example.com/acme/thing":                      "example.com/acme/thing",
		"Registry.Terraform.io/Cloudflare/Cloudflare": "cloudflare/cloudflare",
	} {
		if got := ProviderFamily(in); got != want {
			t.Errorf("ProviderFamily(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestJoinScope(t *testing.T) {
	a, b := &Scope{Trusted: true}, &Scope{Unverifiable: true}
	if JoinScope(nil, nil) != nil || JoinScope(a, nil) != a || JoinScope(nil, b) != b {
		t.Error("nil is no restriction")
	}
	j := JoinScope(a, b)
	if j == a || j == b || !j.Unverifiable || j.Also != a || a.Also != nil || b.Also != nil {
		t.Errorf("joined %+v from %+v and %+v", j, a, b)
	}
}

// TestJoinScopeChain: joining an already joined scope keeps every link.
func TestJoinScopeChain(t *testing.T) {
	a, b, c := &Scope{Trusted: true}, &Scope{Unverifiable: true}, &Scope{Values: map[string]bool{"n": true}}
	for _, j := range []*Scope{JoinScope(a, JoinScope(b, c)), JoinScope(JoinScope(a, b), c), JoinScope(JoinScope(c, a), b)} {
		var got []*Scope
		for s := j; s != nil; s = s.Also {
			got = append(got, s)
		}
		if len(got) != 3 {
			t.Fatalf("chain of %d", len(got))
		}
		trusted, unverifiable, values := 0, 0, 0
		for _, s := range got {
			if s.Trusted {
				trusted++
			}
			if s.Unverifiable {
				unverifiable++
			}
			if s.Values["n"] {
				values++
			}
		}
		if trusted != 1 || unverifiable != 1 || values != 1 {
			t.Errorf("chain %+v", got)
		}
	}
	if a.Also != nil || b.Also != nil || c.Also != nil {
		t.Error("JoinScope changed a scope")
	}
}
