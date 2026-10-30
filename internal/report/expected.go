package report

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/5xxeu/tforphans/internal/model"
)

// Expectation statuses.
const (
	ExpectConfirmed  = "confirmed"  // a listing of that scope ran to the end
	ExpectIncomplete = "incomplete" // listings of that scope ran, none to the end
	ExpectMissing    = "missing"    // no listing reported that scope
)

// MaxExpected bounds the scopes -expect may name: one entry stands for the
// product of its values.
const MaxExpected = 10000

// Expected is one scope the run was told to check (-expect) and whether a
// listing confirmed it.
type Expected struct {
	Scope  map[string]string `json:"scope"`
	Status string            `json:"status"`
	// Listings counts the listings whose scope holds every pair, complete
	// or not.
	Listings int `json:"listings"`
	model.ReasonDetails
}

// maxExpectKeys bounds the keys of one entry: a scope has a few
// (account_id and region), and each key multiplies the work.
const maxExpectKeys = 16

// Lengths bound the memory of the scopes built: account, project, region
// and zone IDs are short, and a config file may come with someone else's
// repository.
const (
	maxExpectValue = 256
	maxExpectBytes = 4 << 20 // all scopes written out as KEY=VALUE pairs
)

var expectKey = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// ParseExpect reads -expect entries: "KEY=V[,V...] [KEY=V[,V...]...]".
// An entry stands for every combination of its values; the same scope from
// two entries counts once.
func ParseExpect(entries []string) ([]map[string]string, error) {
	var out []map[string]string
	seen := map[string]bool{}
	total := 0
	for _, entry := range entries {
		var keys []string
		values := map[string][]string{}
		for _, field := range strings.Fields(entry) {
			key, vals, ok := strings.Cut(field, "=")
			if !ok || !expectKey.MatchString(key) {
				return nil, fmt.Errorf("-expect %q: want KEY=VALUE[,VALUE...] pairs separated by spaces, e.g. account_id=111,222 region=eu-west-1", entry)
			}
			if len(keys) == maxExpectKeys {
				return nil, fmt.Errorf("-expect %q: more than %d keys", entry, maxExpectKeys)
			}
			if _, dup := values[key]; dup {
				return nil, fmt.Errorf("-expect %q: %s given twice", entry, key)
			}
			keys = append(keys, key)
			// A config file may come with someone else's repository and
			// is read before -timeout applies: no quadratic work on it.
			have := map[string]bool{}
			for _, v := range strings.Split(vals, ",") {
				v = strings.TrimSpace(v)
				if len(v) > maxExpectValue {
					return nil, fmt.Errorf("-expect: %s has a value longer than %d bytes", key, maxExpectValue)
				}
				if v != "" && !have[v] {
					have[v] = true
					values[key] = append(values[key], v)
				}
			}
			if len(values[key]) > MaxExpected {
				return nil, fmt.Errorf("-expect: more than %d scopes", MaxExpected)
			}
			if len(values[key]) == 0 {
				return nil, fmt.Errorf("-expect %q: %s has no value", entry, key)
			}
		}
		if len(keys) == 0 {
			return nil, fmt.Errorf("-expect %q: no KEY=VALUE pair", entry)
		}
		// The product is sized before any map is built: a config file
		// may come with someone else's repository.
		size := 1
		for _, key := range keys {
			if size *= len(values[key]); size > MaxExpected {
				return nil, fmt.Errorf("-expect %q: more than %d scopes", entry, MaxExpected)
			}
		}
		// Each value appears in size/len(values) scopes, each key in all.
		for _, key := range keys {
			n := 0
			for _, v := range values[key] {
				n += len(v)
			}
			if total += size/len(values[key])*n + size*(len(key)+2); total > maxExpectBytes {
				return nil, fmt.Errorf("-expect: the scopes take more than %d bytes", maxExpectBytes)
			}
		}
		at := make([]int, len(keys))
		for range size {
			c := make(map[string]string, len(keys))
			for i, key := range keys {
				c[key] = values[key][at[i]]
			}
			if k := ScopeText(c); !seen[k] {
				if len(out) == MaxExpected {
					return nil, fmt.Errorf("-expect: more than %d scopes", MaxExpected)
				}
				seen[k] = true
				out = append(out, c)
			}
			// The last key varies fastest, as in the order given.
			for i := len(keys) - 1; i >= 0; i-- {
				if at[i]++; at[i] < len(values[keys[i]]) {
					break
				}
				at[i] = 0
			}
		}
	}
	return out, nil
}

// CheckExpected says for each expected scope whether a listing confirmed
// it: one listing whose scope holds every pair and that ran to the end is
// enough. The keys no listing reports at all come back too: a typo, or a
// source that does not tell where it looks.
func CheckExpected(want []map[string]string, listings []Listing) (out []Expected, unknownKeys []string) {
	reported := map[string]bool{}
	for _, l := range listings {
		for k := range l.Scope {
			reported[k] = true
		}
	}
	for _, scope := range want {
		e := Expected{Scope: scope, Status: ExpectMissing}
		complete := false
		for _, l := range listings {
			if holds(l.Scope, scope) {
				e.Listings++
				complete = complete || l.Complete
			}
		}
		switch {
		case complete:
			e.Status = ExpectConfirmed
		case e.Listings > 0:
			e.Status = ExpectIncomplete
			e.ReasonDetails = model.Reason("expected_scope_incomplete")
		default:
			e.ReasonDetails = model.Reason("expected_scope_missing")
		}
		out = append(out, e)
		for k := range scope {
			if !reported[k] && !slices.Contains(unknownKeys, k) {
				unknownKeys = append(unknownKeys, k)
			}
		}
	}
	slices.Sort(unknownKeys)
	return out, unknownKeys
}

func holds(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// ScopeText writes a scope as sorted KEY=VALUE pairs.
func ScopeText(scope map[string]string) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(scope)) {
		parts = append(parts, k+"="+scope[k])
	}
	return strings.Join(parts, " ")
}
