// Package model holds the types shared by discovery sources and the report.
package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Resource is an object that exists in the real infrastructure, as reported
// by `terraform query` or by a plugin.
type Resource struct {
	Type string `json:"type"`
	// AltTypes are other resource types that may manage the same object,
	// such as the type's name in an older provider version or in another
	// provider for the same platform. State under any of them counts.
	AltTypes []string `json:"alt_types,omitempty"`
	// AltIdentity overrides Identity for an alternative type whose state
	// keys the same object differently.
	AltIdentity map[string]map[string]any `json:"-"`
	Identity    map[string]any            `json:"identity"`
	DisplayName string                    `json:"display_name"`
	// BaseName, when set, is the object's own name where DisplayName adds
	// to it ("web" for "web (pve1)"): import addresses and ignore rules by
	// name use it.
	BaseName string `json:"-"`
	// TagName is the object's Name tag, read for adopt -names-from-tags:
	// it names the import address before BaseName and DisplayName.
	TagName string `json:"-"`
	// ScopeName says for people where the object lives: "zone example.com",
	// "project my-project".
	ScopeName string `json:"scope,omitempty"`
	// ImportID is an optional legacy import ID. When set, generated import
	// blocks use `id` instead of `identity`.
	ImportID string `json:"import_id"`
	// RequiredKeys lists identity attributes that must be present and equal
	// in state for the resource to count as managed. Nil means all keys.
	RequiredKeys []string `json:"-"`
	// AttributeKeys are the RequiredKeys that are also resource attributes,
	// checked against state that has no identity; nil means the primary
	// keys are derived from RequiredKeys (or "id", or all keys).
	AttributeKeys []string `json:"-"`
	// Provider is the local provider name used to list the resource, when it
	// is not implied by the type prefix (import blocks then need it).
	Provider string `json:"provider"`
	// ProviderSource is the address of the provider that listed the
	// resource (registry.terraform.io/hashicorp/aws), "" when unknown (a
	// plugin that does not say). State of another provider never manages
	// it, except under AltTypes.
	ProviderSource string `json:"provider_source"`
	// UndeclaredProvider is the provider address when the root module does
	// not declare the provider at all (only child modules do).
	UndeclaredProvider string `json:"undeclared_provider"`
	// ImportNote is a comment for the resource's import block.
	ImportNote string `json:"-"`
	// Attributes are the resource's attributes that were asked for
	// (-include-attrs, or attr. conditions of ignore rules).
	Attributes map[string]any `json:"attributes,omitempty"`
	// ListAddress is the list block that found it (terraform query only).
	ListAddress string `json:"list_address"`
	// Scope restricts which state instances may manage the object, for
	// providers whose state does not record where an object lives (the
	// Proxmox cluster). Nil means no restriction.
	Scope *Scope `json:"-"`
	// Source is the root module directory or plugin that reported it.
	Source   string `json:"source"`
	Baseline string `json:"baseline,omitempty"`
}

// Key identifies a resource for de-duplication across sources. Names and
// values are quoted: a value from an API or a plugin may hold any byte,
// and must not pass for another key.
func (r Resource) Key() string {
	var b strings.Builder
	b.WriteString(strconv.Quote(r.Type))
	for _, k := range SortedKeys(r.Identity) {
		if v, ok := Canonical(r.Identity[k]); ok {
			b.WriteString(" " + strconv.Quote(k) + "=" + strconv.Quote(v))
		}
	}
	return b.String()
}

// SameProvider reports whether two provider addresses can name the same
// provider. An unknown address ("") matches any. Hosts that mirror one
// namespace (the Terraform and OpenTofu registries) are ignored, a short
// "aws" means hashicorp/aws, and providers known to share resource types
// and objects (google-beta, the moved hashicorp/github) count as one.
func SameProvider(a, b string) bool {
	if a == "" || b == "" {
		return true
	}
	return ProviderFamily(a) == ProviderFamily(b)
}

var providerAliases = map[string]string{
	"hashicorp/google-beta": "hashicorp/google",
	"hashicorp/github":      "integrations/github",
}

// ProviderFamily is the name SameProvider compares: namespace/type, with
// providers that share resource types under one name (google-beta is
// hashicorp/google).
func ProviderFamily(addr string) string {
	parts := strings.Split(strings.ToLower(addr), "/")
	switch {
	case len(parts) == 3 && (parts[0] == "registry.terraform.io" || parts[0] == "registry.opentofu.org"):
		parts = parts[1:]
	case len(parts) == 1:
		parts = []string{"hashicorp", parts[0]}
	}
	f := strings.Join(parts, "/")
	if a, ok := providerAliases[f]; ok {
		return a
	}
	return f
}

// IdentityString renders the identity as "k=v, k=v" for humans.
func (r Resource) IdentityString() string {
	var parts []string
	for _, k := range SortedKeys(r.Identity) {
		if v, ok := Canonical(r.Identity[k]); ok {
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, ", ")
}

// Scalar returns the canonical string form of a JSON scalar decoded with
// DecodeJSON. Nulls, objects and arrays are reported as not scalar.
func Scalar(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	case bool:
		if t {
			return "true", true
		}
		return "false", true
	default:
		return "", false
	}
}

// Canonical is Scalar for scalars and compact JSON (object keys sorted) for
// lists and objects, so that any non-null value can be compared. Null is
// reported as absent.
func Canonical(v any) (string, bool) {
	if s, ok := Scalar(v); ok {
		return s, true
	}
	switch v.(type) {
	case []any, map[string]any:
		b, err := json.Marshal(v)
		if err != nil {
			return "", false
		}
		return string(b), true
	}
	return "", false
}

// SortedKeys returns map keys in a stable order.
func SortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// DecodeJSON decodes data keeping numbers as json.Number, so identities
// compare exactly regardless of size. data must hold one JSON value: a
// second object on the same line (a plugin's error line without its
// newline) would otherwise be dropped silently.
func DecodeJSON(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("unexpected data after the JSON value")
	}
	return nil
}

// DecodeJSONUnique is DecodeJSON for input that tforphans does not trust
// to be unambiguous (plugin lines): a key repeated in one object, also in
// another case, is an error. encoding/json would keep the last value and
// match struct fields ignoring case, so {"done":false,"done":true} or
// {"type":"a","Type":"b"} would mean whatever the last copy says.
func DecodeJSONUnique(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber() // a float64 would overflow on 1e7000000000
	if err := uniqueKeys(dec, 0); err != nil {
		return err
	}
	return DecodeJSON(data, v)
}

// maxDepth is the nesting encoding/json accepts; uniqueKeys stops there
// too, since it recurses and "[[[[..." in a 64 MiB line would overflow
// the stack, which no error handling survives.
const maxDepth = 10000

// uniqueKeys reads one JSON value from dec and checks its objects for
// repeated keys. Keys come unescaped from the decoder, so "\u0064one" is
// "done".
func uniqueKeys(dec *json.Decoder, depth int) error {
	if depth > maxDepth {
		return errors.New("JSON nested too deeply")
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch tok {
	case json.Delim('{'):
		seen := map[string]bool{}
		for dec.More() {
			tok, err := dec.Token()
			if err != nil {
				return err
			}
			key, _ := tok.(string)
			folded := foldKey(key)
			if seen[folded] {
				return fmt.Errorf("key %q repeated in one object", key)
			}
			seen[folded] = true
			if err := uniqueKeys(dec, depth+1); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	case json.Delim('['):
		for dec.More() {
			if err := uniqueKeys(dec, depth+1); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	}
	return nil
}

// foldKey maps keys that strings.EqualFold treats as equal to one string:
// each rune becomes the smallest rune of its case folding orbit.
func foldKey(key string) string {
	var b strings.Builder
	for _, r := range key {
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			least = min(least, f)
		}
		b.WriteRune(least)
	}
	return b.String()
}

// Scope tells which state instances can belong to the scanned place.
type Scope struct {
	// Attrs maps resource types to the state attribute holding the
	// location, such as a Proxmox node.
	Attrs map[string]string
	// Values are the locations of the scanned place (its nodes).
	Values map[string]bool
	// Unverifiable marks objects whose state carries no location at all.
	Unverifiable bool
	// Trusted is the user's statement that every state given belongs to
	// the scanned place. Locations in state only rule instances out (a node
	// the place does not have); they cannot prove that an instance belongs,
	// since node names repeat across clusters. Without Trusted, matches are
	// unverified.
	Trusted bool
	// Also is the scope of another copy of the same object (JoinScope):
	// both must hold.
	Also *Scope
}

// JoinScope returns the scope of an object listed twice: an instance must
// pass both scopes, and a match is verified only if both say so. The order
// of the copies does not matter, and neither scope is changed.
func JoinScope(a, b *Scope) *Scope {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	// A copy of b's chain, ending in a.
	joined := *b
	last := &joined
	for last.Also != nil {
		next := *last.Also
		last.Also = &next
		last = &next
	}
	last.Also = a
	return &joined
}
