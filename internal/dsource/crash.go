package dsource

import (
	"fmt"
	"slices"
	"strings"
)

// crashMarkers are texts of a crash rather than of an error a data source
// reported: the provider plugin died ("Plugin did not respond") or
// terraform itself panicked (a provider returned values its own schema
// does not allow, such as list elements of different types).
var crashMarkers = []string{"plugin did not respond", "failed to respond to the plugin", "panic:", "stack trace from the terraform"}

// crashed reports whether a failed apply was a crash. A crash names no
// data block at all, or names the blocks that were in flight when the
// plugin died, which says nothing about which of them caused it.
func crashed(stderr []byte, errs []diagErr, fatal []string) bool {
	if len(errs) == 0 && len(fatal) == 0 {
		return true
	}
	return crashText(stderr, errs, fatal) != ""
}

// crashText returns the first line describing the crash, if any.
func crashText(stderr []byte, errs []diagErr, fatal []string) string {
	lines := strings.Split(string(stderr), "\n")
	for _, e := range errs {
		lines = append(lines, e.message)
	}
	lines = append(lines, fatal...)
	for _, l := range lines {
		low := strings.ToLower(l)
		for _, m := range crashMarkers {
			if strings.Contains(low, m) {
				return strings.TrimSpace(l)
			}
		}
	}
	return ""
}

// prober runs apply with some blocks and reports whether it crashed; the
// state it wrote is returned when it did not.
type prober func(blocks []block) (crash bool, state []byte, err error)

// isolate finds the blocks, or the instances (zones) of scoped blocks,
// whose reading crashes, by reading halves of them until each crash is
// pinned down: a crash in one zone's DNS records then costs that zone, not
// the whole scan. The blocks returned keep going (a crashing instance is
// excluded); failed lists what was given up. err is set when the crash
// cannot be pinned down (it does not happen with any half).
func isolate(blocks []block, why string, probe prober) (keep []block, failed []failure, err error) {
	if why == "" {
		why = "crashed: terraform or the provider stopped without a message"
	}
	isParent := map[string]bool{}
	for _, b := range blocks {
		if b.parent != "" {
			isParent[b.parent] = true
		}
	}
	var parents, rest []block
	for _, b := range blocks {
		if isParent[b.addr()] {
			parents = append(parents, b)
		} else {
			rest = append(rest, b)
		}
	}
	// with adds the parents the given blocks iterate over.
	with := func(live []block, set []block) []block {
		var out []block
		for _, p := range live {
			for _, b := range set {
				if b.parent == p.addr() {
					out = append(out, p)
					break
				}
			}
		}
		return append(out, set...)
	}
	crashes := func(live, set []block) (bool, error) {
		c, _, err := probe(with(live, set))
		return c, err
	}

	var bad []block
	if len(parents) > 0 {
		c, err := crashes(nil, parents)
		if err != nil {
			return nil, nil, err
		}
		if c {
			if bad, err = bisect(parents, func(set []block) (bool, error) { return crashes(nil, set) }); err != nil {
				return nil, nil, err
			}
		}
	}
	gone := map[string]bool{}
	for _, b := range bad {
		gone[b.addr()] = true
		failed = append(failed, failure{block: b.addr(), address: b.addr(), message: why})
	}
	var liveParents []block
	for _, p := range parents {
		if !gone[p.addr()] {
			liveParents = append(liveParents, p)
		}
	}
	var candidates []block
	for _, b := range rest {
		if gone[b.parent] {
			gone[b.addr()] = true
			failed = append(failed, failure{block: b.addr(), address: b.addr(), message: "parent " + b.parent + " crashed: " + why})
			continue
		}
		candidates = append(candidates, b)
	}
	if len(bad) == 0 || len(candidates) > 0 {
		c, err := crashes(liveParents, candidates)
		if err != nil {
			return nil, nil, err
		}
		var culprits []block
		if c {
			if culprits, err = bisect(candidates, func(set []block) (bool, error) { return crashes(liveParents, set) }); err != nil {
				return nil, nil, err
			}
		}
		if len(bad) == 0 && len(culprits) == 0 {
			// Not repeated: read everything again. A crash that keeps
			// coming back runs into the probe budget, after which every
			// suspect is reported.
			return blocks, nil, nil
		}
		for i := range candidates {
			if !slices.ContainsFunc(culprits, func(c block) bool { return c.addr() == candidates[i].addr() }) {
				continue
			}
			b, f, err := isolateInstances(candidates[i], liveParents, why, probe)
			if err != nil {
				return nil, nil, err
			}
			failed = append(failed, f...)
			if b == nil {
				gone[candidates[i].addr()] = true
				continue
			}
			candidates[i] = *b
		}
	}
	for _, b := range blocks {
		if gone[b.addr()] {
			continue
		}
		if i := slices.IndexFunc(candidates, func(c block) bool { return c.addr() == b.addr() }); i >= 0 {
			b = candidates[i]
		}
		keep = append(keep, b)
	}
	return keep, failed, nil
}

// isolateInstances pins a crash of a scoped block down to its instances.
// It returns the block with the crashing instances excluded, or nil when
// the block crashes as a whole (or is not scoped).
func isolateInstances(b block, parents []block, why string, probe prober) (*block, []failure, error) {
	whole := []failure{{block: b.addr(), address: b.addr(), message: why}}
	if b.scope == nil {
		return nil, whole, nil
	}
	var parent []block
	for _, p := range parents {
		if p.addr() == b.parent {
			parent = append(parent, p)
		}
	}
	_, st, err := probe(parent)
	if err != nil || st == nil {
		return nil, whole, err
	}
	keys, err := parentIDs(st, b.parent, b.only)
	if err != nil {
		return nil, nil, err
	}
	keys = slices.DeleteFunc(keys, func(k string) bool { return slices.Contains(b.exclude, k) })
	if len(keys) < 2 {
		return nil, whole, nil
	}
	only := func(ks []string) block {
		c := b
		c.only = ks
		return c
	}
	bad, err := bisect(keys, func(ks []string) (bool, error) {
		c, _, err := probe(append(slices.Clone(parent), only(ks)))
		return c, err
	})
	if err != nil {
		return nil, nil, err
	}
	if len(bad) == len(keys) {
		return nil, whole, nil
	}
	var failed []failure
	b.exclude = slices.Clone(b.exclude)
	for _, k := range bad {
		b.exclude = append(b.exclude, k)
		failed = append(failed, failure{block: b.addr(), address: fmt.Sprintf("%s[%q]", b.addr(), k), message: why})
	}
	return &b, failed, nil
}

// budget bounds the probe runs. Past the bound every probe counts as a
// crash without running: isolate then gives up on what is still suspect
// (it is reported as crashed) instead of failing the whole scan. exhausted
// is called once when that happens.
func budget(probe prober, max int, exhausted func()) prober {
	n := 0
	return func(set []block) (bool, []byte, error) {
		if len(set) == 0 {
			return false, nil, nil
		}
		if n++; n > max {
			if n == max+1 && exhausted != nil {
				exhausted()
			}
			return true, nil, nil
		}
		return probe(set)
	}
}

// bisect returns the members of set (known to crash as a whole) that crash
// on their own, halving the set each round. When neither half crashes the
// crash needs members of both, and the whole set is returned.
func bisect[T any](set []T, crashes func([]T) (bool, error)) ([]T, error) {
	if len(set) <= 1 {
		return set, nil
	}
	a, b := set[:len(set)/2], set[len(set)/2:]
	ca, err := crashes(a)
	if err != nil {
		return nil, err
	}
	cb, err := crashes(b)
	if err != nil {
		return nil, err
	}
	if !ca && !cb {
		return set, nil
	}
	var out []T
	for _, half := range []struct {
		set   []T
		crash bool
	}{{a, ca}, {b, cb}} {
		if !half.crash {
			continue
		}
		found, err := bisect(half.set, crashes)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	return out, nil
}
