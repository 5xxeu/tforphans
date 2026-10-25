package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/5xxeu/tforphans/internal/report"
	"github.com/5xxeu/tforphans/internal/tf"
)

func (d *doctorReport) planSkip(id, status, category, root, fix string, sk tf.Skip) int {
	i := slices.IndexFunc(d.Checks, func(c doctorCheck) bool {
		return c.ID == id && c.Status == status && c.Category == category && c.Root == report.Clean(root) && c.Remediation == fix
	})
	if i < 0 {
		d.add(id, status, "", root, fix)
		i = len(d.Checks) - 1
		d.Checks[i].Category = category
	}
	c := &d.Checks[i]
	c.Skipped = append(c.Skipped, sk)
	if !slices.Contains(c.Types, sk.Type) {
		c.Types = append(c.Types, sk.Type)
		slices.Sort(c.Types)
	}
	n := len(c.Types)
	c.Count = &n
	c.Message = doctorSkipMessage(category, c.Types, false)
	return i
}

func doctorSkipMessage(category string, types []string, verbose bool) string {
	shown := types
	if !verbose {
		shown = firstN(types, 8)
	}
	return report.Clean(fmt.Sprintf("%s (%s): %s", category, plural(len(types), "type"), strings.Join(shown, ", ")))
}

func doctorScanCategory(sk tf.Skip) string {
	switch {
	case strings.HasPrefix(sk.Reason, "listed as "):
		return "listed as another type"
	case strings.Contains(sk.Reason, "no list resource or plural data source"):
		return "no plural data source"
	case sk.Excluded:
		return "excluded by adapter or scope"
	case strings.Contains(sk.Reason, "results carry no "):
		return "results have no identity"
	case strings.Contains(sk.Reason, " has no "):
		return "unsupported result schema"
	case strings.Contains(sk.Reason, " requires "):
		return "unsupported required arguments"
	default:
		return "provider or adapter limitation"
	}
}

func doctorQueryCategory(sk tf.Skip) string {
	switch {
	case strings.HasPrefix(sk.Reason, tf.MissingArgs):
		return strings.Split(sk.Reason, " (provider ")[0]
	case strings.HasPrefix(sk.Reason, "listed through "):
		return "listed through an equivalent provider"
	case sk.Excluded && strings.Contains(sk.Reason, "name it in -types"):
		return "opt-in types"
	case sk.Excluded:
		return "excluded by planner"
	case strings.HasSuffix(sk.Reason, " not listed (-no-aliases)"):
		return "provider configurations excluded by -no-aliases"
	case strings.Contains(sk.Reason, " is required under several local names "):
		return "provider required under multiple local names"
	case strings.HasPrefix(sk.Reason, "-arg "):
		return "unsupported automatic argument"
	default:
		return sk.Reason
	}
}

func doctorUserListCoverage(sk tf.Skip, root string, lists map[string]tf.UserList) string {
	if sk.Excluded {
		return ""
	}
	sk.Root = root
	for _, address := range slices.Sorted(maps.Keys(lists)) {
		typ := strings.SplitN(address, ".", 3)[1]
		check := tf.Check{Type: typ, Root: root, Provider: lists[address].ProviderRef(typ)}
		coverage := report.BuildCoverage(nil, []tf.Check{check}, []tf.Skip{sk}, nil)
		if !slices.ContainsFunc(coverage, func(c report.Coverage) bool { return c.Status == report.Incomplete }) {
			return address
		}
	}
	return ""
}
