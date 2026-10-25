package cli

import (
	"fmt"
	"slices"
	"strings"

	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/report"
	"github.com/5xxeu/tforphans/internal/state"
	"github.com/5xxeu/tforphans/internal/tf"
)

// dedupe drops resources listed more than once, for example by two roots
// that share a provider account. Equal types and identities from different
// providers are different objects; an unknown provider (a plugin) is taken
// for the same one. An object whose location is unknown (project "") is
// only merged with copies from the same list block: two roots can list
// different projects' objects under the same name. Attributes of the
// dropped copies are kept: one copy may come from a list block that did
// not ask for them.
func dedupe(rs []model.Resource) []model.Resource {
	out, _ := dedupeMembers(rs)
	return out
}

// dedupeMembers is dedupe that also returns, by memberKey of each kept
// result, every result merged into it (the kept one too).
func dedupeMembers(rs []model.Resource) ([]model.Resource, map[string][]model.Resource) {
	// One slot per key and provider family; "" collects the results
	// without a provider. The order of the results must not matter, so
	// those join a known family only when it is the only one: an object of
	// unknown provider could be either of two, and swallowing both would
	// hide one.
	type slot struct {
		r       model.Resource
		first   int // position of the first result in the slot
		members []model.Resource
	}
	slots := map[string]map[string]*slot{}
	var keys []string
	for i, r := range rs {
		k := r.Key()
		if state.UnknownLocation(r.Identity) {
			k += "\x00" + r.Source + "\x00" + r.ListAddress
		}
		family := ""
		if r.ProviderSource != "" {
			family = model.ProviderFamily(r.ProviderSource)
		}
		if slots[k] == nil {
			slots[k] = map[string]*slot{}
			keys = append(keys, k)
		}
		if s := slots[k][family]; s != nil {
			mergeResult(&s.r, r)
			s.members = append(s.members, r)
			continue
		}
		slots[k][family] = &slot{r, i, []model.Resource{r}}
	}
	var out []*slot
	for _, k := range keys {
		group := slots[k]
		if unknown := group[""]; unknown != nil && len(group) == 2 {
			delete(group, "")
			for _, known := range group {
				mergeResult(&known.r, unknown.r)
				known.first = min(known.first, unknown.first)
				known.members = append(known.members, unknown.members...)
			}
		}
		for _, s := range group {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b *slot) int { return a.first - b.first })
	res := make([]model.Resource, len(out))
	members := map[string][]model.Resource{}
	for i, s := range out {
		res[i] = s.r
		members[memberKey(s.r)] = s.members
	}
	return res, members
}

// memberKey tells the results dedupe kept apart: one slot per originKey and
// provider family.
func memberKey(r model.Resource) string {
	family := ""
	if r.ProviderSource != "" {
		family = model.ProviderFamily(r.ProviderSource)
	}
	return originKey(r) + "\x00" + family
}

// originKey keys the listings of an object as dedupe's slots: with an
// unknown location, only the same listing is the same object.
func originKey(r model.Resource) string {
	if state.UnknownLocation(r.Identity) {
		return r.Key() + "\x00" + r.Source + "\x00" + r.ListAddress
	}
	return r.Key()
}

// classified is what classify sorts the live objects into.
type classified struct {
	orphans   []model.Resource
	uncertain []report.Uncertain
	// seen are the live objects that are no orphans, once per listing
	// that returned them (origins), for -baseline.
	seen []report.Seen
	// byTypeScope counts per type and scope name, for coverage.
	byTypeScope map[[2]string]*report.TypeCount
}

// classify matches each live object against the states and the ignore
// rules and counts it in sum (managed, ignored, uncertain, orphans; per
// type and scope). It fills in the scope names of live.
func classify(o *options, idx *state.Index, live []model.Resource, origins map[string][]model.Resource, sum *report.Summary) classified {
	c := classified{byTypeScope: map[[2]string]*report.TypeCount{}}
	addSeen := func(r model.Resource, status, address, rule string, details model.ReasonDetails) {
		for _, origin := range origins[originKey(r)] {
			if !model.SameProvider(origin.ProviderSource, r.ProviderSource) {
				continue
			}
			cp := r
			cp.Source, cp.ListAddress = origin.Source, origin.ListAddress
			c.seen = append(c.seen, report.Seen{ReasonDetails: details, Resource: cp, Status: status, StateAddress: address, Rule: rule})
		}
	}
	sum.ByType = map[string]*report.TypeCount{}
	byScope := map[string]*report.TypeCount{}
	for i := range live {
		r := &live[i]
		if r.ScopeName == "" {
			r.ScopeName = scopeName(r.Identity)
		}
		where := r.ScopeName
		if where == "" {
			where = "unknown"
		}
		counts := []*report.TypeCount{sum.ByType[r.Type], byScope[where], c.byTypeScope[[2]string{r.Type, r.ScopeName}]}
		for i, tc := range counts {
			if tc == nil {
				counts[i] = &report.TypeCount{}
			}
		}
		sum.ByType[r.Type], byScope[where], c.byTypeScope[[2]string{r.Type, r.ScopeName}] = counts[0], counts[1], counts[2]
		for _, tc := range counts {
			tc.Live++
		}
		// An ignore rule wins over an uncertain match, not over a
		// confirmed one.
		var m state.Match
		var rejected []state.Rejection
		var ofType int
		if o.audit.explain {
			m, rejected, ofType = idx.Explain(*r, 3)
		} else {
			m = idx.Match(*r)
		}
		rule, ignored := "", false
		if !m.Managed || !m.Verified {
			rule, ignored = o.run.ignore.Rule(*r)
		}
		if o.audit.explain {
			sum.Explain = append(sum.Explain, explain(*r, m, rejected, ofType, rule, ignored))
		}
		switch {
		case m.Managed && m.Verified:
			addSeen(*r, "managed", m.Address, "", model.ReasonDetails{})
			sum.Managed++
			if m.ByIdentity {
				sum.ManagedByIdentity++
			}
			for _, tc := range counts {
				tc.Managed++
			}
		case ignored:
			addSeen(*r, report.BecameIgnored, "", rule, model.ReasonDetails{})
			sum.Ignored++
			for _, tc := range counts {
				tc.Ignored++
			}
		case m.Managed:
			addSeen(*r, report.BecameUnsure, m.Address, "", uncertainReason(m))
			sum.Uncertain++
			c.uncertain = append(c.uncertain, report.Uncertain{Resource: *r, StateAddress: m.Address})
			for _, tc := range counts {
				tc.Uncertain++
			}
		default:
			c.orphans = append(c.orphans, *r)
			for _, tc := range counts {
				tc.Orphans++
			}
		}
	}
	// Scopes are worth a breakdown when there are several.
	if len(byScope) > 1 || len(byScope) == 1 && byScope["unknown"] == nil {
		sum.ByScope = byScope
	}
	return c
}

// coverageInputs returns the failed listings that count as gaps, and the
// skips and checks coverage is built from, after -strict-api and
// -strict-ignore. It counts disabled APIs and strict-ignored listings in
// sum and marks the skips -strict-ignore lets pass.
func coverageInputs(a auditOptions, sum *report.Summary) (counting []tf.Failure, skipped []tf.Skip, checks []tf.Check) {
	// A disabled API (GCP) is a gap: disabling Cloud Storage or BigQuery
	// keeps their data, so the failure proves nothing about objects.
	// -strict-api=false takes it as empty, as before; types named in
	// -strict-ignore are no gap either, whatever went wrong.
	counting, skipped, checks = sum.Failed, sum.Skipped, sum.Checks
	for _, f := range sum.Failed {
		if f.Category == tf.FailAPIOff {
			sum.APIOff++
		}
	}
	// A skip that -strict-ignore lets pass says so in skipped_types too.
	for i, sk := range sum.Skipped {
		if !sk.Excluded && a.strictIgnore.Match(sk.Type) {
			sum.Skipped[i].Excluded = true
			sum.Skipped[i].Reason = report.StrictIgnored + sk.Reason
			sum.StrictIgnored++
		}
	}
	if !a.strictAPI || len(a.strictIgnore) > 0 {
		counting = nil
		skipped = slices.Clone(sum.Skipped)
		checks = slices.Clone(sum.Checks)
		for _, f := range sum.Failed {
			switch {
			case f.Category == tf.FailAPIOff && !a.strictAPI:
				skipped = append(skipped, tf.Skip{ReasonDetails: f.ReasonDetails(), Type: f.Type, Reason: tf.FailAPIOff, Excluded: true, Root: f.Root})
				// The listing ran far enough to learn there is nothing:
				// it stands for the check (an aliased block of the user's
				// covers the alias-only skip of its provider).
				if f.Root != "" {
					checks = append(checks, tf.Check{Type: f.Type, Scope: "root " + f.Root, Root: f.Root, Provider: f.Provider})
				}
			case a.strictIgnore.Match(f.Type):
				skipped = append(skipped, tf.Skip{ReasonDetails: f.ReasonDetails(), Type: f.Type, Reason: report.StrictIgnored + f.Category, Excluded: true, Root: f.Root})
				sum.StrictIgnored++
			default:
				counting = append(counting, f)
			}
		}
	}
	return counting, skipped, checks
}

// mergeResult adds what a duplicate result knows to the kept one.
func mergeResult(kept *model.Resource, r model.Resource) {
	// A user block without include_resource may answer first: the copy
	// with the object keeps its Name tag for the address.
	if kept.TagName == "" {
		kept.TagName = r.TagName
	}
	// A copy whose place is unknown or restricted must not be forgotten
	// because the other copy came first.
	kept.Scope = model.JoinScope(kept.Scope, r.Scope)
	for a, v := range r.Attributes {
		if kept.Attributes == nil {
			kept.Attributes = map[string]any{}
		}
		if _, ok := kept.Attributes[a]; !ok {
			kept.Attributes[a] = v
		}
	}
}

// unlistedTypes are the types in the states whose provider was asked (its
// schema read, a scan of it run, or results from a plugin, which declares
// nothing up front: what it returned is what it lists) but which that
// provider cannot list: tforphans never checks objects of them. Types left
// out with -types or -exclude-types are the user's choice and not reported.
func unlistedTypes(src *sourceOptions, counts map[state.TypeKey]int, sum report.Summary) []report.UnlistedType {
	var out []report.UnlistedType
	for k, n := range counts {
		if !sum.QueriedFamilies[k.Provider] || sum.CanList(k.Type, k.Provider) || len(src.include) > 0 && !src.include.Match(k.Type) || src.exclude.Match(k.Type) {
			continue
		}
		out = append(out, report.UnlistedType{Type: k.Type, Provider: k.Provider, Instances: n})
	}
	slices.SortFunc(out, func(a, b report.UnlistedType) int {
		if a.Instances != b.Instances {
			return b.Instances - a.Instances
		}
		return strings.Compare(a.Type+"\x00"+a.Provider, b.Type+"\x00"+b.Provider)
	})
	return out
}

func unlistedNote(types []report.UnlistedType) string {
	var names []string
	for i, t := range types {
		if i == 8 {
			names = append(names, fmt.Sprintf("and %d more (-format json lists all)", len(types)-i))
			break
		}
		names = append(names, fmt.Sprintf("%s (%d)", t.Type, t.Instances))
	}
	return fmt.Sprintf("%d resource types in the states cannot be listed by their provider, so objects of them are never checked: %s", len(types), strings.Join(names, ", "))
}

// explain says why a live object counts as it does (-explain); the order
// of the cases is the one of the decision.
func explain(r model.Resource, m state.Match, rejected []state.Rejection, ofType int, rule string, ignored bool) report.Explanation {
	e := report.Explanation{Type: r.Type, Identity: r.Identity, DisplayName: r.DisplayName, Scope: r.ScopeName, Source: r.Source}
	where := func(m state.Match) string {
		s := m.Address
		if m.Source != "" {
			s += " in " + m.Source
		}
		switch m.Via {
		case "identity":
			s += ", by resource identity"
		case "attributes":
			s += ", by attributes (the state instance has no resource identity)"
		case "owner":
			s = "created and owned by " + s
		}
		if m.Type != "" {
			s += ", as " + m.Type
		}
		return s
	}
	switch {
	case m.Managed && m.Verified:
		e.Status, e.Reason = "managed", where(m)
		e.ReasonDetails = model.Reason("state_match")
	case ignored:
		e.ReasonDetails = model.Reason("ignore_rule")
		e.Status, e.Rule, e.Reason = "ignored", rule, "ignore rule "+rule
		if m.Managed {
			e.Reason += "; it also matches " + m.Address + " without confirmation"
		}
	case m.Managed:
		e.ReasonDetails = uncertainReason(m)
		e.Status, e.Reason = "uncertain", "matches "+where(m)+", but "
		switch {
		case m.ScopeUnknown:
			e.Reason += "the state does not record the cluster or account (trust it with -proxmox-cluster-states or -plugin-cluster-states)"
		case len(m.Unknown) > 0:
			e.Reason += "the listing left " + strings.Join(m.Unknown, ", ") + " empty, so where the object lives is not known"
		case len(m.Unconfirmed) > 0:
			e.Reason += "the state does not hold " + strings.Join(m.Unconfirmed, ", ") + " to confirm where the object lives"
		default:
			e.Reason += "the state cannot confirm where the object lives"
		}
	default:
		e.Status = "unmanaged"
		switch {
		case ofType == 0:
			e.ReasonDetails = model.Reason("no_state_instance")
			e.Reason = "no state instance of " + r.Type + " was read"
		case len(rejected) == 0:
			e.ReasonDetails = model.Reason("identity_not_matched")
			e.Reason = fmt.Sprintf("none of the %d state instances of %s shares its identity", ofType, r.Type)
		default:
			e.ReasonDetails = model.Reason("identity_conflict")
			var near []string
			for _, x := range rejected {
				s := x.Address
				if x.Source != "" {
					s += " in " + x.Source
				}
				near = append(near, s+": "+x.Why)
			}
			e.Reason = "closest: " + strings.Join(near, "; ")
		}
	}
	if m.Managed {
		e.StateAddress, e.StateSource = m.Address, m.Source
	}
	return e
}

func uncertainReason(m state.Match) model.ReasonDetails {
	code := "scope_unverified"
	if !m.ScopeUnknown {
		switch {
		case len(m.Unknown) > 0:
			code = "discovery_identity_missing"
		case len(m.Unconfirmed) > 0:
			code = "state_identity_missing"
		}
	}
	return model.Reason(code, append(slices.Clone(m.Unknown), m.Unconfirmed...)...)
}

// scopeName names the account of an object from its identity, for people:
// "project my-project"; "" when the identity does not say.
// sortResources orders objects by scope, type and name.
func sortResources(rs []model.Resource) { slices.SortStableFunc(rs, compareResources) }

func compareResources(a, b model.Resource) int {
	if c := strings.Compare(a.ScopeName, b.ScopeName); c != 0 {
		return c
	}
	if c := strings.Compare(a.Type, b.Type); c != 0 {
		return c
	}
	// The bare name ignores display details such as a guest's node.
	// Break ties by source, provider and identity so import suffixes
	// _2, _3 do not depend on the API's listing order.
	if c := strings.Compare(baseName(a), baseName(b)); c != 0 {
		return c
	}
	if c := strings.Compare(a.Source, b.Source); c != 0 {
		return c
	}
	if c := strings.Compare(a.Provider, b.Provider); c != 0 {
		return c
	}
	if c := strings.Compare(a.ProviderSource, b.ProviderSource); c != 0 {
		return c
	}
	if c := strings.Compare(a.ImportID, b.ImportID); c != 0 {
		return c
	}
	if c := strings.Compare(a.IdentityString(), b.IdentityString()); c != 0 {
		return c
	}
	// Quoted keys and values distinguish identities with separators in them.
	return strings.Compare(a.Key(), b.Key())
}

func baseName(r model.Resource) string {
	if r.BaseName != "" {
		return r.BaseName
	}
	return r.DisplayName
}

func scopeName(identity map[string]any) string {
	var parts []string
	for _, k := range scopeKeys {
		if v, ok := model.Scalar(identity[k]); ok && v != "" {
			parts = append(parts, k+" "+v)
		}
	}
	return strings.Join(parts, ", ")
}
