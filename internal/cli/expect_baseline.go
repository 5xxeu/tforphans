package cli

import (
	"github.com/5xxeu/tforphans/internal/report"
)

// mergeScopes is a and then the scopes of b that a does not have, each scope
// once, and how many it had to leave out at report.MaxExpected.
func mergeScopes(a, b []map[string]string) ([]map[string]string, int) {
	seen := map[string]bool{}
	var out []map[string]string
	dropped := 0
	for _, list := range [][]map[string]string{a, b} {
		for _, s := range list {
			k := report.ScopeText(s)
			if seen[k] {
				continue
			}
			if len(out) >= report.MaxExpected {
				dropped++
				continue
			}
			seen[k] = true
			out = append(out, s)
		}
	}
	return out, dropped
}

func firstNScopes(s []map[string]string, n int) []map[string]string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
