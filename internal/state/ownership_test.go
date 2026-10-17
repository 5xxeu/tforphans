package state

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/5xxeu/tforphans/internal/model"
)

func ownershipInstance(address string, identity map[string]any) Instance {
	return Instance{Type: "google_compute_disk", Provider: "registry.terraform.io/hashicorp/google", Address: address,
		Identity: identity, Attributes: map[string]any{"id": "disk-1"}}
}

func ownershipSnapshot(lineage, source string, serial int64, instances ...Instance) Snapshot {
	return Snapshot{Lineage: lineage, Serial: &serial, Sources: []string{source}, Instances: instances}
}

func TestDuplicateOwnership(t *testing.T) {
	full := map[string]any{"name": "disk", "project": "p", "zone": "z"}
	for _, c := range []struct {
		name       string
		identity   map[string]any
		provider   string
		lineage    string
		serial     int64
		confidence string
		reason     string
	}{
		{"full", full, "google", "b", 1, "confirmed", ""},
		{"diverged serial", full, "google", "a", 2, "possible", "same_lineage_diverged"},
		{"diverged content", full, "google", "a", 1, "possible", "same_lineage_diverged"},
		{"missing scope", map[string]any{"name": "disk", "zone": "z"}, "google", "b", 1, "possible", "partial_identity"},
		{"empty scope", map[string]any{"name": "disk", "project": "", "zone": "z"}, "google", "b", 1, "possible", "partial_identity"},
		{"null scope", map[string]any{"name": "disk", "project": nil, "zone": "z"}, "google", "b", 1, "possible", "partial_identity"},
		{"empty list", map[string]any{"name": "disk", "project": []any{}, "zone": "z"}, "google", "b", 1, "possible", "partial_identity"},
		{"empty map", map[string]any{"name": "disk", "project": map[string]any{}, "zone": "z"}, "google", "b", 1, "possible", "partial_identity"},
		{"legacy", nil, "google", "b", 1, "possible", "no_identity"},
		{"unknown lineage", full, "google", "", 1, "possible", "lineage_unknown"},
		{"different provider", full, "registry.terraform.io/acme/google", "b", 1, "", ""},
		{"provider alias", full, "registry.opentofu.org/hashicorp/google-beta", "b", 1, "confirmed", ""},
		{"conflicting scope", map[string]any{"name": "disk", "project": "other", "zone": "z"}, "google", "b", 1, "", ""},
		{"different scalar type", map[string]any{"name": json.Number("1"), "project": "p", "zone": "z"}, "google", "b", 1, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := ownershipInstance("google_compute_disk.a", full)
			b := ownershipInstance("google_compute_disk.b", c.identity)
			b.Provider = c.provider
			ix := NewIndex()
			ix.AddSnapshots([]Snapshot{ownershipSnapshot("a", "root-a", 1, a), ownershipSnapshot(c.lineage, "root-b", c.serial, b)})
			got, within := ix.Duplicates()
			if len(within) != 0 {
				t.Fatalf("within: %+v", within)
			}
			if c.confidence == "" {
				if len(got) != 0 {
					t.Fatalf("unexpected duplicates: %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].Confidence != c.confidence || got[0].Reason != c.reason || len(got[0].Owners) != 2 {
				t.Fatalf("duplicates: %+v", got)
			}
			if got[0].Owners[0].Serial == nil || got[0].Owners[1].Serial == nil {
				t.Fatal("missing serial")
			}
			if c.name == "diverged serial" {
				serials := []int64{*got[0].Owners[0].Serial, *got[0].Owners[1].Serial}
				slices.Sort(serials)
				if !slices.Equal(serials, []int64{1, 2}) {
					t.Fatalf("serials: %v", serials)
				}
			}
		})
	}
}

func TestEquivalentSnapshots(t *testing.T) {
	data := `{"version":4,"lineage":"remote","serial":0,"resources":[
	{"mode":"managed","type":"github_repository","name":"repo","provider":"provider[\"registry.terraform.io/hashicorp/github\"]",
	"instances":[{"identity":{"name":"repo","owner":"team"},"attributes":{"id":"ignored","description":"old"}}]},
	{"mode":"data","type":"github_repository","name":"data","instances":[{"attributes":{"id":"repo"}}]}]}`
	a, err := ParseSnapshot([]byte(data), Redact("https://user:secret@example.com/state?token=secret"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Serial == nil || *a.Serial != 0 || a.Lineage != "remote" || len(a.Instances) != 1 {
		t.Fatalf("snapshot: %+v", a)
	}
	b, err := ParseSnapshot([]byte(`{"serial":0,"lineage":"remote","version":4,"terraform_version":"different","resources":[
	{"name":"repo","mode":"managed","type":"github_repository","provider":"provider[\"registry.opentofu.org/integrations/github\"]",
	"instances":[{"attributes":{"id":"also ignored","description":"new"},"identity":{"owner":"team","name":"repo"}}]}]}`), "checkout-b")
	if err != nil {
		t.Fatal(err)
	}
	ix := NewIndex()
	ix.AddSnapshots([]Snapshot{a, b, b})
	if len(ix.snapshots) != 1 || ix.Len() != 3 || !slices.Equal(ix.snapshots[0].Sources, []string{"checkout-b", "https://example.com/state?REDACTED"}) {
		t.Fatalf("merged: %+v, count %d", ix.snapshots, ix.Len())
	}
	if own, within := ix.Duplicates(); len(own)+len(within) != 0 {
		t.Fatalf("equivalent reads: %+v %+v", own, within)
	}
	c := b
	c.Lineage, c.Sources = "other", []string{"other-state"}
	ix.AddSnapshots([]Snapshot{c})
	own, _ := ix.Duplicates()
	if len(own) != 1 || !slices.ContainsFunc(own[0].Owners, func(o Owner) bool { return o.Lineage == "remote" && slices.Equal(o.Sources, ix.snapshots[0].Sources) }) {
		t.Fatalf("merged sources in owners: %+v", own)
	}
}

func TestUnknownSnapshotsAndSerials(t *testing.T) {
	in := ownershipInstance("google_compute_disk.a", map[string]any{"id": "disk"})
	ix := NewIndex()
	a := ownershipSnapshot("", "a", 1, in)
	ix.AddSnapshots([]Snapshot{a, a})
	if len(ix.snapshots) != 1 {
		t.Fatal("a repeated read of one source was not merged")
	}
	own, _ := ix.Duplicates()
	if len(own) != 0 {
		t.Fatalf("one source read twice: %+v", own)
	}
	// Copied roots with separate states: same address and content, no lineage.
	ix.AddSnapshots([]Snapshot{ownershipSnapshot("", "b", 2, in)})
	if len(ix.snapshots) != 2 {
		t.Fatal("unknown lineage snapshots of different sources were merged")
	}
	own, _ = ix.Duplicates()
	if len(own) != 1 || own[0].Reason != "lineage_unknown" || len(own[0].Owners) != 2 {
		t.Fatalf("separate unknown lineage states: %+v", own)
	}
	ix = NewIndex()
	c := ownershipSnapshot("known", "known", 0, in)
	d := c
	d.Serial, d.Sources = nil, []string{"unknown-serial"}
	ix.AddSnapshots([]Snapshot{c, d})
	if len(ix.snapshots) != 2 {
		t.Fatal("serial zero and unknown were merged")
	}
	own, _ = ix.Duplicates()
	if len(own) != 1 || own[0].Reason != "same_lineage_diverged" {
		t.Fatalf("diverged: %+v", own)
	}
}

func TestDuplicateInStateAndOwnership(t *testing.T) {
	for _, identity := range []map[string]any{{"name": "disk", "project": "p"}, nil} {
		a := ownershipInstance("google_compute_disk.a", identity)
		b := ownershipInstance("google_compute_disk.b", identity)
		ix := NewIndex()
		ix.AddSnapshots([]Snapshot{ownershipSnapshot("a", "a", 1, a, b), ownershipSnapshot("b", "b", 1, a)})
		own, within := ix.Duplicates()
		if len(own) != 1 || len(within) != 1 || len(own[0].Owners) != 3 || len(within[0].Owners) != 2 {
			t.Fatalf("ownership %+v, within %+v", own, within)
		}
		if identity == nil && (within[0].Confidence != "possible" || within[0].Reason != "no_identity" || within[0].ID != "disk-1") {
			t.Fatalf("legacy anomaly: %+v", within)
		}
	}
}

func TestOwnershipCanonicalIdentity(t *testing.T) {
	a := ownershipInstance("disk.a", map[string]any{"ids": []any{"x", json.Number("2")}, "scope": map[string]any{"b": "2", "a": "1"}})
	b := ownershipInstance("disk.b", map[string]any{"scope": map[string]any{"a": "1", "b": "2"}, "ids": []any{"x", json.Number("2")}})
	ix := NewIndex()
	ix.AddSnapshots([]Snapshot{ownershipSnapshot("a", "a", 1, a), ownershipSnapshot("b", "b", 2, b)})
	if own, _ := ix.Duplicates(); len(own) != 1 || own[0].Confidence != "confirmed" {
		t.Fatalf("canonical: %+v", own)
	}
}

func TestOwnershipExcludedProviders(t *testing.T) {
	for _, provider := range []string{"null", "random", "time", "local", "tls", "archive", "external", "http", "cloudinit", "terraform.io/builtin/terraform"} {
		t.Run(provider, func(t *testing.T) {
			a := ownershipInstance("thing.a", nil)
			a.Provider = provider
			if provider == "terraform.io/builtin/terraform" {
				a.Type = "terraform_data"
			}
			b := a
			b.Address = "thing.b"
			ix := NewIndex()
			ix.AddSnapshots([]Snapshot{ownershipSnapshot("a", "a", 1, a, b), ownershipSnapshot("b", "b", 1, a)})
			if own, within := ix.Duplicates(); len(own)+len(within) != 0 {
				t.Fatalf("excluded: %+v %+v", own, within)
			}
		})
	}
}

func TestOwnershipRefresh(t *testing.T) {
	refresh, err := ParseShowSnapshot([]byte(`{"prior_state":{"values":{"root_module":{"resources":[
	{"mode":"managed","type":"google_compute_disk","address":"google_compute_disk.a","provider_name":"registry.terraform.io/hashicorp/google","identity":{"id":"disk"}}]}}}}`), "root (workspace prod)")
	if err != nil {
		t.Fatal(err)
	}
	if refresh.Lineage != "" || refresh.Serial != nil || refresh.Sources[0] != "root (workspace prod)" {
		t.Fatalf("refresh: %+v", refresh)
	}
	ix := NewIndex()
	ix.AddSnapshots([]Snapshot{refresh, ownershipSnapshot("known", "stored", 1, ownershipInstance("google_compute_disk.b", map[string]any{"id": "disk"}))})
	own, _ := ix.Duplicates()
	if len(own) != 1 || own[0].Reason != "lineage_unknown" {
		t.Fatalf("refresh ownership: %+v", own)
	}
}

// Shared account and region must not turn unique IDs into pairwise work.
func TestOwnershipScales(t *testing.T) {
	const n = 50000
	instances := make([]Instance, n)
	for i := range instances {
		id := fmt.Sprintf("i-%07d", i)
		instances[i] = Instance{Type: "aws_instance", Provider: "aws", Address: "aws_instance.x[" + id + "]", Identity: map[string]any{"id": id, "account_id": "111", "region": "eu-north-1"}}
	}
	ix := NewIndex()
	ix.AddSnapshots([]Snapshot{ownershipSnapshot("a", "a", 1, instances[:n/2]...), ownershipSnapshot("b", "b", 1, instances[n/2:]...)})
	start := time.Now()
	own, within := ix.Duplicates()
	if d := time.Since(start); d > 30*time.Second { // a guard against quadratic work, see the benchmarks
		t.Errorf("%d instances took %s", n, d)
	}
	if len(own)+len(within) != 0 {
		t.Fatalf("unique objects: %d ownership, %d within", len(own), len(within))
	}
}

func TestOwnershipLegacyIDs(t *testing.T) {
	for _, c := range []struct {
		name string
		a, b any
		want bool
	}{
		{"equal", "disk", "disk", true},
		{"different", "disk", "other", false},
		{"empty", "", "", false},
		{"null", nil, nil, false},
		{"number versus string", json.Number("42"), "42", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, b := ownershipInstance("disk.a", nil), ownershipInstance("disk.b", nil)
			a.Attributes["id"], b.Attributes["id"] = c.a, c.b
			ix := NewIndex()
			ix.AddSnapshots([]Snapshot{ownershipSnapshot("a", "-", 1, a), ownershipSnapshot("b", "b", 1, b)})
			own, _ := ix.Duplicates()
			if (len(own) == 1) != c.want || len(own) > 1 {
				t.Fatalf("duplicates: %+v", own)
			}
			if c.want && (own[0].Reason != "no_identity" || own[0].Confidence != "possible" || own[0].ID != "disk") {
				t.Fatalf("legacy: %+v", own)
			}
		})
	}
}

func TestOwnershipGroupsSeveralStates(t *testing.T) {
	in := ownershipInstance("disk.a", map[string]any{"id": "disk"})
	ix := NewIndex()
	ix.AddSnapshots([]Snapshot{ownershipSnapshot("a", "a", 1, in), ownershipSnapshot("b", "b", 1, in), ownershipSnapshot("c", "c", 1, in)})
	own, within := ix.Duplicates()
	if len(own) != 1 || len(own[0].Owners) != 3 || len(within) != 0 {
		t.Fatalf("grouping: %+v %+v", own, within)
	}
	// A repeated address in one snapshot is not a duplicate address.
	ix = NewIndex()
	ix.AddSnapshots([]Snapshot{ownershipSnapshot("a", "a", 1, in, in)})
	if own, within := ix.Duplicates(); len(own)+len(within) != 0 {
		t.Fatalf("same address: %+v %+v", own, within)
	}
}

func TestOwnershipUnknownProviderGrouping(t *testing.T) {
	a := ownershipInstance("disk.a", map[string]any{"id": "disk"})
	b := a
	b.Address, b.Provider = "disk.b", ""
	c := b
	c.Address = "disk.c"
	ix := NewIndex()
	ix.AddSnapshots([]Snapshot{ownershipSnapshot("a", "a", 1, a, b, c), ownershipSnapshot("b", "b", 1, a)})
	own, within := ix.Duplicates()
	if len(own) != 1 || len(within) != 1 || len(own[0].Owners) != 4 || len(within[0].Owners) != 3 {
		t.Fatalf("provider grouping: %+v %+v", own, within)
	}
}

func TestOwnershipRepeatedAddressesScales(t *testing.T) {
	const n = 10000
	a, b := make([]Instance, n), make([]Instance, n)
	for i := range a {
		a[i] = ownershipInstance(fmt.Sprintf("disk.x[%d]", i), map[string]any{"id": "disk", "project": "p"})
		b[i] = ownershipInstance(fmt.Sprintf("disk.y[%d]", i), map[string]any{"id": "disk"})
	}
	ix := NewIndex()
	ix.AddSnapshots([]Snapshot{ownershipSnapshot("a", "a", 1, a...), ownershipSnapshot("b", "b", 1, b...)})
	start := time.Now()
	own, within := ix.Duplicates()
	if d := time.Since(start); d > 30*time.Second { // a guard against quadratic work, see the benchmarks
		t.Errorf("repeated addresses took %s", d)
	}
	if len(own) != 1 || own[0].Reason != "partial_identity" || len(own[0].Owners) != 2*n || len(within) != 2 {
		t.Fatalf("repeated addresses: %d ownership, %d within", len(own), len(within))
	}
}

func TestOwnershipAllUnknownIdentity(t *testing.T) {
	for _, other := range []map[string]any{{"id": "disk"}, {"id": ""}, {"id": nil}, {"id": []any{}}, {"id": map[string]any{}}} {
		a := ownershipInstance("disk.a", map[string]any{"id": ""})
		b := ownershipInstance("disk.b", other)
		ix := NewIndex()
		ix.AddSnapshots([]Snapshot{ownershipSnapshot("a", "a", 1, a), ownershipSnapshot("b", "b", 1, b)})
		own, within := ix.Duplicates()
		if len(own)+len(within) != 0 {
			t.Fatalf("unknown identity: %+v %+v", own, within)
		}
	}
}

func TestOwnershipPartialIdentity(t *testing.T) {
	for _, c := range []struct {
		name       string
		identity   map[string]any
		candidates []Instance
		owners     int
		confirmed  int
	}{
		{
			name:       "no known value",
			identity:   map[string]any{"project": "", "name": ""},
			candidates: []Instance{ownershipInstance("disk.full", map[string]any{"project": "p", "name": "x"})},
		},
		{
			name:     "scope only ambiguous",
			identity: map[string]any{"project": "p", "name": ""},
			candidates: []Instance{
				ownershipInstance("disk.x", map[string]any{"project": "p", "name": "x"}),
				ownershipInstance("disk.y", map[string]any{"project": "p", "name": "y"}),
			},
		},
		{
			name:       "scope only single candidate",
			identity:   map[string]any{"project": "p", "name": ""},
			candidates: []Instance{ownershipInstance("disk.full", map[string]any{"project": "p", "name": "x"})},
			owners:     2,
		},
		{
			name:       "missing zone single candidate",
			identity:   map[string]any{"project": "p", "name": "x", "zone": ""},
			candidates: []Instance{ownershipInstance("disk.full", map[string]any{"project": "p", "name": "x", "zone": "z"})},
			owners:     2,
		},
		{
			name:     "missing zone ambiguous",
			identity: map[string]any{"project": "p", "name": "x", "zone": ""},
			candidates: []Instance{
				ownershipInstance("disk.z1", map[string]any{"project": "p", "name": "x", "zone": "z1"}),
				ownershipInstance("disk.z2", map[string]any{"project": "p", "name": "x", "zone": "z2"}),
			},
		},
		{
			name:     "conflicts do not count",
			identity: map[string]any{"project": "p", "name": "x", "zone": ""},
			candidates: []Instance{
				ownershipInstance("disk.full", map[string]any{"project": "p", "name": "x", "zone": "z"}),
				ownershipInstance("disk.other", map[string]any{"project": "p", "name": "other", "zone": "z"}),
			},
			owners: 2,
		},
		{
			name:     "grouped objects count once",
			identity: map[string]any{"project": "p", "name": "x", "zone": ""},
			candidates: []Instance{
				ownershipInstance("disk.first", map[string]any{"project": "p", "name": "x", "zone": "z"}),
				ownershipInstance("disk.second", map[string]any{"project": "p", "name": "x", "zone": "z"}),
			},
			owners:    3,
			confirmed: 1,
		},
		{
			name:       "missing key single candidate",
			identity:   map[string]any{"project": "p", "name": "x"},
			candidates: []Instance{ownershipInstance("disk.full", map[string]any{"project": "p", "name": "x", "zone": "z"})},
			owners:     2,
		},
		{
			name:     "missing key ambiguous",
			identity: map[string]any{"project": "p", "name": "x"},
			candidates: []Instance{
				ownershipInstance("disk.z1", map[string]any{"project": "p", "name": "x", "zone": "z1"}),
				ownershipInstance("disk.z2", map[string]any{"project": "p", "name": "x", "zone": "z2"}),
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Reverse state order so uniqueness includes earlier and later objects.
			for _, reverse := range []bool{false, true} {
				snapshots := []Snapshot{ownershipSnapshot("a", "a", 1, ownershipInstance("disk.partial", c.identity))}
				for i, candidate := range c.candidates {
					snapshots = append(snapshots, ownershipSnapshot(fmt.Sprintf("b%d", i), fmt.Sprintf("b%d", i), 1, candidate))
				}
				if reverse {
					slices.Reverse(snapshots)
				}
				ix := NewIndex()
				ix.AddSnapshots(snapshots)
				own, within := ix.Duplicates()
				if len(within) != 0 {
					t.Fatalf("within: %+v", within)
				}
				if c.owners == 0 {
					if len(own) != 0 {
						t.Fatalf("unexpected ownership: %+v", own)
					}
					continue
				}
				partial := slices.IndexFunc(own, func(d Duplicate) bool { return d.Reason == "partial_identity" })
				if len(own) != 1+c.confirmed || partial < 0 || own[partial].Confidence != "possible" || len(own[partial].Owners) != c.owners {
					t.Fatalf("ownership: %+v", own)
				}
				for i, d := range own {
					if i != partial && (d.Confidence != "confirmed" || d.Reason != "" || len(d.Owners) != 2) {
						t.Fatalf("confirmed ownership: %+v", d)
					}
				}
			}
		})
	}
}

func TestOwnershipPartialIdentityScales(t *testing.T) {
	const n, incomplete = 20000, 200
	instances := make([]Instance, n)
	for i := range instances {
		name := fmt.Sprintf("disk-%07d", i)
		instances[i] = ownershipInstance("disk.x["+name+"]", map[string]any{"project": "p", "name": name})
	}
	for _, project := range []string{"p", ""} {
		t.Run("project="+project, func(t *testing.T) {
			partial := make([]Instance, incomplete)
			for i := range partial {
				partial[i] = ownershipInstance(fmt.Sprintf("disk.partial[%d]", i), map[string]any{"project": project, "name": ""})
			}
			ix := NewIndex()
			ix.AddSnapshots([]Snapshot{ownershipSnapshot("a", "a", 1, instances...), ownershipSnapshot("b", "b", 1, partial...)})
			start := time.Now()
			own, within := ix.Duplicates()
			if d := time.Since(start); d > 30*time.Second { // a guard against quadratic work, see the benchmarks
				t.Errorf("%d full and %d incomplete instances took %s", n, incomplete, d)
			}
			if len(own)+len(within) != 0 {
				t.Fatalf("ambiguous objects: %d ownership, %d within", len(own), len(within))
			}
		})
	}
}

func TestOwnershipPartialIdentityCandidateBoundaries(t *testing.T) {
	for _, c := range []struct {
		name     string
		provider string
		typ      string
		want     bool
	}{
		{"same family", "google", "google_compute_disk", false},
		{"provider alias", "registry.opentofu.org/hashicorp/google-beta", "google_compute_disk", false},
		{"different family", "registry.terraform.io/acme/google", "google_compute_disk", true},
		{"different type", "google", "google_compute_instance", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			partial := ownershipInstance("disk.partial", map[string]any{"project": "p", "name": "x", "zone": ""})
			full := ownershipInstance("disk.full", map[string]any{"project": "p", "name": "x", "zone": "z1"})
			extra := ownershipInstance("disk.extra", map[string]any{"project": "p", "name": "x", "zone": "z2"})
			extra.Provider, extra.Type = c.provider, c.typ
			ix := NewIndex()
			// A different address in the incomplete object's state also counts.
			ix.AddSnapshots([]Snapshot{ownershipSnapshot("a", "a", 1, partial, extra), ownershipSnapshot("b", "b", 1, full)})
			own, within := ix.Duplicates()
			if len(within) != 0 || (len(own) == 1) != c.want || len(own) > 1 {
				t.Fatalf("ownership: %+v, within: %+v", own, within)
			}
			if c.want && (own[0].Confidence != "possible" || own[0].Reason != "partial_identity" || len(own[0].Owners) != 2) {
				t.Fatalf("partial ownership: %+v", own)
			}
		})
	}
}

func TestOwnershipAmbiguousIdentityPreservesConfirmed(t *testing.T) {
	partial := ownershipInstance("disk.partial", map[string]any{"project": "p", "name": "x", "zone": ""})
	z1 := ownershipInstance("disk.z1", map[string]any{"project": "p", "name": "x", "zone": "z1"})
	z2 := ownershipInstance("disk.z2", map[string]any{"project": "p", "name": "x", "zone": "z2"})
	ix := NewIndex()
	ix.AddSnapshots([]Snapshot{
		ownershipSnapshot("a", "a", 1, partial),
		ownershipSnapshot("b", "b", 1, z1, z2),
		ownershipSnapshot("c", "c", 1, z1),
	})
	own, within := ix.Duplicates()
	if len(own) != 1 || own[0].Confidence != "confirmed" || own[0].Reason != "" || len(own[0].Owners) != 2 || len(within) != 0 {
		t.Fatalf("confirmed ownership: %+v, within: %+v", own, within)
	}
}

func TestOwnershipUnknownIdentityLegacyID(t *testing.T) {
	for _, id := range []string{"disk-1", "other", ""} {
		t.Run("id="+id, func(t *testing.T) {
			unknown := ownershipInstance("disk.unknown", map[string]any{"project": "", "name": ""})
			legacy := ownershipInstance("disk.legacy", nil)
			legacy.Attributes["id"] = id
			ix := NewIndex()
			ix.AddSnapshots([]Snapshot{ownershipSnapshot("a", "a", 1, unknown), ownershipSnapshot("b", "b", 1, legacy)})
			own, within := ix.Duplicates()
			if len(within) != 0 || (len(own) == 1) != (id == "disk-1") || len(own) > 1 {
				t.Fatalf("legacy ownership: %+v, within: %+v", own, within)
			}
			if id == "disk-1" && (own[0].Confidence != "possible" || own[0].Reason != "no_identity") {
				t.Fatalf("legacy fallback: %+v", own)
			}
		})
	}
}

func TestOwnershipDifferentTypes(t *testing.T) {
	a := ownershipInstance("disk.a", map[string]any{"id": "disk"})
	b := a
	b.Type = "google_compute_instance"
	ix := NewIndex()
	ix.AddSnapshots([]Snapshot{ownershipSnapshot("a", "a", 1, a), ownershipSnapshot("b", "b", 1, b)})
	if own, within := ix.Duplicates(); len(own)+len(within) != 0 {
		t.Fatalf("different types: %+v %+v", own, within)
	}
}

func TestOwnershipOrderIndependent(t *testing.T) {
	a := ownershipSnapshot("a", "a", 1, ownershipInstance("disk.a", map[string]any{"id": "disk", "project": "p"}))
	b := ownershipSnapshot("b", "b", 1, ownershipInstance("disk.b", map[string]any{"id": "disk", "region": "r"}))
	one, two := NewIndex(), NewIndex()
	one.AddSnapshots([]Snapshot{a, b})
	two.AddSnapshots([]Snapshot{b, a})
	ownOne, inOne := one.Duplicates()
	ownTwo, inTwo := two.Duplicates()
	if canonical(ownOne) != canonical(ownTwo) || canonical(inOne) != canonical(inTwo) {
		t.Fatalf("order affected output: %+v %+v", ownOne, ownTwo)
	}
}

func TestEquivalentSnapshotsPreserveLiveAttributes(t *testing.T) {
	a := ownershipInstance("disk.a", nil)
	a.Attributes["project"] = "p1"
	b := ownershipInstance("disk.a", nil)
	b.Attributes["project"] = "p2"
	ix := NewIndex()
	ix.AddSnapshots([]Snapshot{ownershipSnapshot("same", "a", 1, a), ownershipSnapshot("same", "b", 1, b)})
	if len(ix.snapshots) != 1 {
		t.Fatal("equivalent legacy snapshots did not merge")
	}
	for _, project := range []string{"p1", "p2"} {
		r := model.Resource{Type: a.Type, ProviderSource: a.Provider, Identity: map[string]any{"id": "disk-1", "project": project}, RequiredKeys: []string{"id"}}
		if !ix.Match(r).Managed {
			t.Fatalf("lost legacy attributes for %s", project)
		}
	}
	if own, within := ix.Duplicates(); len(own)+len(within) != 0 {
		t.Fatalf("equivalent copies: %+v %+v", own, within)
	}
}

func BenchmarkDuplicates(b *testing.B) {
	for _, n := range []int{1000, 10000, 50000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			instances := make([]Instance, n)
			for i := range instances {
				id := fmt.Sprintf("i-%07d", i)
				instances[i] = Instance{Type: "aws_instance", Provider: "aws", Address: "aws_instance.x[" + id + "]", Identity: map[string]any{"id": id, "account_id": "111", "region": "eu-north-1"}}
			}
			ix := NewIndex()
			ix.AddSnapshots([]Snapshot{ownershipSnapshot("a", "a", 1, instances[:n/2]...), ownershipSnapshot("b", "b", 1, instances[n/2:]...)})
			b.ReportAllocs()
			for b.Loop() {
				ix.Duplicates()
			}
		})
	}
}
