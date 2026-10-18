package model

import (
	"encoding/json"
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
