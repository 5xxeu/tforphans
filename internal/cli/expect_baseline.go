package cli

import (
	"github.com/5xxeu/tforphans/internal/report"
)

// mergeScopes is a and then the scopes of b that a does not have, each scope
// once.
func mergeScopes(a, b []map[string]string) []map[string]string {
	seen := map[string]bool{}
	var out []map[string]string
	for _, list := range [][]map[string]string{a, b} {
		for _, s := range list {
			if k := report.ScopeText(s); !seen[k] && len(out) < report.MaxExpected {
				seen[k] = true
				out = append(out, s)
			}
		}
	}
	return out
}

func firstNScopes(s []map[string]string, n int) []map[string]string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
