package state

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/model"
)

// referenceMatch is the matching rule written out plainly, without the
// index: every instance of every state is compared with the live object.
// It returns whether some instance manages r verified, whether some does at
// all, and the addresses of the instances that do (verified ones when there
// are any).
func referenceMatch(r model.Resource, instances []Instance) (managed, verified bool, addresses []string) {
	required, primary := keySets(r)
	var all, confirmed []string
	for _, in := range instances {
		if in.Type != r.Type || !model.SameProvider(r.ProviderSource, in.Provider) {
			continue
		}
		ok, sure := referenceCompare(r, in, required, primary)
		if !ok {
			continue
		}
		all = append(all, in.Address)
		if sure {
			confirmed = append(confirmed, in.Address)
		}
	}
	if len(confirmed) > 0 {
		return true, true, confirmed
	}
	return len(all) > 0, false, all
}

// referenceCompare: with an identity in state, every value the live object
// knows must be equal where state has the key, a required key must be
// there, and at least one must be equal; without one, every primary key
// must be equal in the attributes (an unknown, empty location aside) and
// no value may conflict. Anything state cannot confirm makes the match
// unverified.
func referenceCompare(r model.Resource, in Instance, required, primary []string) (ok, verified bool) {
	src, strict := in.Identity, true
	if len(src) == 0 {
		src, strict = in.Attributes, false
	}
	unsure, equal := false, 0
	primaryEqual, primaryLeft := 0, len(primary)
	for k, v := range r.Identity {
		want, known := model.Canonical(v)
		if !known {
			if slices.Contains(r.RequiredKeys, k) {
				return false, false
			}
			if !emptyIsUnknown[k] {
				continue
			}
		}
		isPrimary := slices.Contains(primary, k)
		if (!known || want == "") && emptyIsUnknown[k] {
			unsure = true
			if isPrimary {
				primaryLeft--
			}
			continue
		}
		if want == "" && (strict && slices.Contains(required, k) || !strict && isPrimary) {
			return false, false
		}
		got, has := model.Canonical(src[k])
		if !has {
			if strict && slices.Contains(required, k) {
				return false, false
			}
			unsure = true
			continue
		}
		if got != want {
			return false, false
		}
		equal++
		if isPrimary {
			primaryEqual++
		}
	}
	if strict {
		return equal > 0, !unsure
	}
	ok = primaryEqual > 0 && primaryEqual == primaryLeft
	return ok, ok && !unsure
}

var (
	propTypes     = []string{"t_one", "t_two"}
	propProviders = []string{"", "registry.terraform.io/hashicorp/x", "registry.opentofu.org/hashicorp/x", "acme/x"}
	propKeys      = []string{"id", "name", "project", "region"}
	propValues    = []any{"", "a", "b", nil}
)

func pick[T any](rng *rand.Rand, xs []T) T { return xs[rng.IntN(len(xs))] }

// propValueMap draws a few keys with values from a small set, so that
// equal and conflicting values both turn up often.
func propValueMap(rng *rand.Rand) map[string]any {
	m := map[string]any{}
	for _, k := range propKeys {
		if rng.IntN(2) == 0 {
			m[k] = pick(rng, propValues)
		}
	}
	return m
}

func propInstances(rng *rand.Rand) []Instance {
	instances := make([]Instance, rng.IntN(8))
	for i := range instances {
		in := Instance{Type: pick(rng, propTypes), Address: fmt.Sprintf("x.i%d", i), Provider: pick(rng, propProviders)}
		if rng.IntN(2) == 0 {
			in.Identity = propValueMap(rng)
		}
		in.Attributes = propValueMap(rng)
		instances[i] = in
	}
	return instances
}

func propResource(rng *rand.Rand) model.Resource {
	r := model.Resource{Type: pick(rng, propTypes), ProviderSource: pick(rng, propProviders), Identity: propValueMap(rng)}
	switch rng.IntN(3) {
	case 0:
		r.RequiredKeys = []string{"id"}
	case 1:
		r.RequiredKeys = []string{"name", "project"}
	}
	// AttributeKeys are the required keys that are attributes too.
	if len(r.RequiredKeys) > 0 && rng.IntN(3) == 0 {
		r.AttributeKeys = r.RequiredKeys[:1]
	}
	// Sources always give the keys they require (an empty location is
	// tested on its own); a plugin may give one as null.
	for _, k := range r.RequiredKeys {
		r.Identity[k] = pick(rng, []any{"a", "b", "a", "b", nil})
	}
	return r
}

// TestMatchReference: the index finds what comparing with every instance
// finds, whatever the order of the instances, and a state read twice
// changes nothing.
func TestMatchReference(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for round := range 20000 {
		instances := propInstances(rng)
		r := propResource(rng)
		managed, verified, addresses := referenceMatch(r, instances)

		check := func(name string, ix *Index) {
			t.Helper()
			m := ix.Match(r)
			if m.Managed != managed || m.Verified != verified || managed && !slices.Contains(addresses, m.Address) {
				t.Fatalf("round %d, %s: Match %+v, reference managed %v verified %v at %v\nresource %+v\ninstances %+v", round, name, m, managed, verified, addresses, r, instances)
			}
		}
		ix := NewIndex()
		ix.Add(slices.Clone(instances))
		check("as read", ix)

		shuffled := slices.Clone(instances)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		ix = NewIndex()
		ix.Add(shuffled)
		check("shuffled", ix)

		ix = NewIndex()
		ix.AddSnapshots([]Snapshot{{Sources: []string{"a"}, Instances: slices.Clone(instances)}, {Sources: []string{"b"}, Instances: slices.Clone(instances)}})
		check("read twice", ix)
	}
}

// TestMatchProperties: rules that hold whatever the data.
func TestMatchProperties(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	for round := range 20000 {
		instances := propInstances(rng)
		r := propResource(rng)
		ix := NewIndex()
		ix.Add(slices.Clone(instances))
		m := ix.Match(r)
		if !m.Managed {
			continue
		}
		var in *Instance
		for i := range instances {
			if instances[i].Address == m.Address {
				in = &instances[i]
			}
		}
		// The instance is of the type and of a compatible provider.
		if in == nil || in.Type != r.Type || !model.SameProvider(r.ProviderSource, in.Provider) {
			t.Fatalf("round %d: %+v manages %+v", round, in, r)
		}
		// Another provider family never matches.
		other := r
		other.ProviderSource = "other/x"
		if in.Provider != "" && ix.Match(other).Address == m.Address {
			t.Fatalf("round %d: %s of %s manages a resource of other/x", round, m.Address, in.Provider)
		}
		// A key the live object knows in addition, that no state holds,
		// can only take confirmation away.
		more := r
		more.Identity = maps.Clone(r.Identity)
		more.Identity["zone_extra"] = "z"
		if mm := ix.Match(more); mm.Verified && !m.Verified {
			t.Fatalf("round %d: an extra key confirmed %+v", round, mm)
		}
	}
}

// TestMatchEmptyOwnID: an empty own ID is no match, also where another
// key would bring the instance up as a candidate: compare says so itself.
func TestMatchEmptyOwnID(t *testing.T) {
	r := model.Resource{Type: "t", Identity: map[string]any{"id": "", "name": "b"}}
	for _, in := range []Instance{
		{Type: "t", Address: "t.attrs", Attributes: map[string]any{"id": "", "name": "b"}},
		{Type: "t", Address: "t.identity", Identity: map[string]any{"id": "", "name": "b"}},
	} {
		if ok, _, _, _ := compare(r, &in); ok {
			t.Errorf("%s matches an object with an empty id", in.Address)
		}
		ix := NewIndex()
		ix.Add([]Instance{in})
		if m := ix.Match(r); m.Managed {
			t.Errorf("%s manages an object with an empty id: %+v", in.Address, m)
		}
	}
}

// TestMatchNullRequiredKey: a key the source requires but gives as null
// (a plugin's "id": null) confirms nothing; the account alone is no match.
func TestMatchNullRequiredKey(t *testing.T) {
	r := model.Resource{Type: "t", Identity: map[string]any{"account_id": "1", "id": nil}, RequiredKeys: []string{"account_id", "id"}}
	in := Instance{Type: "t", Address: "t.x", Identity: map[string]any{"account_id": "1", "id": "a"}}
	if ok, _, why, _ := compare(r, &in); ok || !strings.Contains(why, "id is null") {
		t.Errorf("compare: %v %q", ok, why)
	}
	ix := NewIndex()
	ix.Add([]Instance{in})
	if m := ix.Match(r); m.Managed {
		t.Errorf("managed: %+v", m)
	}
}
