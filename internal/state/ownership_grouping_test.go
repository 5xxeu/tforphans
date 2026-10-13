package state

import (
	"fmt"
	"slices"
	"testing"

	"github.com/5xxeu/tforphans/internal/model"
)

func TestOwnershipFullIdentityPreservesLegacyIDs(t *testing.T) {
	for _, c := range []struct {
		name       string
		firstID    string
		equivalent bool
		unknown    bool
	}{
		{"missing first ID", "", false, false},
		{"different first ID", "disk-2", false, false},
		{"equivalent snapshots", "", true, false},
		{"unknown provider", "", false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			identity := map[string]any{"name": "disk", "project": "p", "zone": "z"}
			a := ownershipInstance("disk.a", identity)
			if c.firstID == "" {
				delete(a.Attributes, "id")
			} else {
				a.Attributes["id"] = c.firstID
			}
			b := ownershipInstance("disk.b", identity)
			if c.unknown {
				b.Provider = ""
			}
			legacy := ownershipInstance("disk.legacy", nil)
			snapshots := []Snapshot{
				ownershipSnapshot("a", "a", 1, a),
				ownershipSnapshot("b", "b", 1, b),
				ownershipSnapshot("c", "c", 1, legacy),
			}
			if c.firstID != "" {
				legacyFirst := ownershipInstance("disk.legacy_first", nil)
				legacyFirst.Attributes["id"] = c.firstID
				snapshots[2].Instances = append(snapshots[2].Instances, legacyFirst)
			}
			if c.equivalent {
				snapshots[1].Lineage = "a"
				snapshots[1].Instances[0].Address = "disk.a"
			}
			var previous string
			for _, order := range [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
				t.Run(fmt.Sprint(order), func(t *testing.T) {
					ix := NewIndex()
					ix.AddSnapshots([]Snapshot{snapshots[order[0]], snapshots[order[1]], snapshots[order[2]]})
					own, within := ix.Duplicates()
					wantRows, wantOwners := 2, 3
					if c.firstID != "" {
						wantOwners++
					}
					if c.equivalent {
						wantRows, wantOwners = 1, 2
					}
					if len(own) != wantRows || len(within) != 0 {
						t.Fatalf("ownership %+v, within %+v", own, within)
					}
					confirmed, possible := 0, 0
					for _, d := range own {
						if canonical(d.Identity) != canonical(identity) {
							t.Fatalf("lost full identity: %+v", d)
						}
						if d.Reason == "no_identity" {
							possible++
							if d.Confidence != "possible" || len(d.Owners) != wantOwners || !slices.ContainsFunc(d.Owners, func(o Owner) bool { return o.Address == "disk.legacy" }) {
								t.Fatalf("legacy fallback lost owners: %+v", d)
							}
						} else {
							confirmed++
							if d.Reason != "" || d.Confidence != "confirmed" || len(d.Owners) != 2 {
								t.Fatalf("full identity ownership: %+v", d)
							}
						}
					}
					if confirmed != wantRows-1 || possible != 1 {
						t.Fatalf("expected confirmed ownership and legacy fallback: %+v", own)
					}
					result := canonical([]any{own, within})
					if previous != "" && previous != result {
						t.Fatalf("snapshot order changed ownership: %s, %s", previous, result)
					}
					previous = result
				})
			}
		})
	}
}

func TestOwnershipIncompleteIdentityPreservesLegacyIDs(t *testing.T) {
	a := ownershipInstance("disk.a", map[string]any{"id": ""})
	b := ownershipInstance("disk.b", map[string]any{"id": ""})
	a.Attributes["id"], b.Attributes["id"] = "disk-A", "disk-B"
	legacyA, legacyB := a, b
	legacyA.Address, legacyA.Identity = "disk.legacy_a", nil
	legacyB.Address, legacyB.Identity = "disk.legacy_b", nil
	snapshots := []Snapshot{
		ownershipSnapshot("a", "a", 1, a, b),
		ownershipSnapshot("b", "b", 1, legacyA, legacyB),
	}
	var previous string
	for _, reverse := range []bool{false, true} {
		if reverse {
			slices.Reverse(snapshots)
		}
		ix := NewIndex()
		ix.AddSnapshots(snapshots)
		own, within := ix.Duplicates()
		if len(own) != 2 || len(within) != 0 {
			t.Fatalf("reverse=%v: ownership %+v, within %+v", reverse, own, within)
		}
		for _, d := range own {
			if d.Confidence != "possible" || d.Reason != "no_identity" || len(d.Owners) != 2 {
				t.Fatalf("legacy fallback: %+v", d)
			}
			addresses := []string{d.Owners[0].Address, d.Owners[1].Address}
			if !slices.Equal(addresses, []string{"disk.a", "disk.legacy_a"}) && !slices.Equal(addresses, []string{"disk.b", "disk.legacy_b"}) {
				t.Fatalf("unrelated legacy IDs merged: %+v", d)
			}
		}
		if reverse && previous != canonical(own) {
			t.Fatalf("snapshot order changed ownership: %s, %s", previous, canonical(own))
		}
		previous = canonical(own)
	}
}

func TestOwnershipPartialIdentityMergedBeforeCounting(t *testing.T) {
	partial := ownershipInstance("disk.partial", map[string]any{"id": "x", "region": ""})
	full := ownershipInstance("disk.full", map[string]any{"id": "x", "region": "r"})
	snapshots := []Snapshot{
		ownershipSnapshot("a", "a", 1, partial),
		ownershipSnapshot("b", "b", 1, partial),
		ownershipSnapshot("c", "c", 1, full),
	}
	var previous string
	for _, reverse := range []bool{false, true} {
		if reverse {
			slices.Reverse(snapshots)
		}
		ix := NewIndex()
		ix.AddSnapshots(snapshots)
		own, within := ix.Duplicates()
		if len(own) != 1 || len(within) != 0 || own[0].Confidence != "possible" || own[0].Reason != "partial_identity" || len(own[0].Owners) != 3 {
			t.Fatalf("reverse=%v: ownership %+v, within %+v", reverse, own, within)
		}
		if canonical(own[0].Identity) != canonical(full.Identity) {
			t.Fatalf("lost complete identity: %+v", own[0])
		}
		lineages := []string{own[0].Owners[0].Lineage, own[0].Owners[1].Lineage, own[0].Owners[2].Lineage}
		slices.Sort(lineages)
		if !slices.Equal(lineages, []string{"a", "b", "c"}) {
			t.Fatalf("lost owners: %+v", own[0])
		}
		if reverse && previous != canonical(own) {
			t.Fatalf("snapshot order changed ownership: %s, %s", previous, canonical(own))
		}
		previous = canonical(own)
	}
}

func TestOwnershipIncompleteIdentityUnknownLegacyID(t *testing.T) {
	// Without a known legacy ID, equal partial identities are separate
	// candidates, so neither can disambiguate the other from the full object.
	a := ownershipInstance("disk.a", map[string]any{"id": "x", "region": ""})
	b := ownershipInstance("disk.b", map[string]any{"id": "x", "region": ""})
	a.Attributes["id"], b.Attributes["id"] = "", ""
	full := ownershipInstance("disk.full", map[string]any{"id": "x", "region": "r"})
	for _, reverse := range []bool{false, true} {
		snapshots := []Snapshot{
			ownershipSnapshot("a", "a", 1, a),
			ownershipSnapshot("b", "b", 1, b),
			ownershipSnapshot("c", "c", 1, full),
		}
		if reverse {
			slices.Reverse(snapshots)
		}
		ix := NewIndex()
		ix.AddSnapshots(snapshots)
		if own, within := ix.Duplicates(); len(own)+len(within) != 0 {
			t.Fatalf("ambiguous objects: %+v, %+v", own, within)
		}
	}
}

func TestParseSensitiveLegacyID(t *testing.T) {
	for _, c := range []struct {
		name      string
		paths     string
		sensitive bool
	}{
		{"absent", "null", false},
		{"empty", "[]", false},
		{"attribute path", `[["id"]]`, true},
		{"cty path", `[[{"type":"get_attr","value":"id"}]]`, true},
		{"other attribute", `[["password"]]`, false},
		{"nested id", `[["settings","id"]]`, false},
		{"cty nested id", `[[{"type":"get_attr","value":"settings"},{"type":"get_attr","value":"id"}]]`, false},
		{"cty index", `[[{"type":"get_attr","value":"settings"},{"type":"index","value":0}]]`, false},
		{"numeric index", `[["settings",0,"id"]]`, false},
		{"multiple paths", `[["password"],["id"]]`, true},
		{"whole instance", `[[]]`, true},
		{"unknown path", `[[{"type":"future","value":"id"}]]`, true},
		{"malformed step", `[[{"type":"get_attr","value":1}]]`, true},
		{"malformed index", `[[{"type":"get_attr","value":"settings"},{"type":"index","value":{}}]]`, true},
		{"malformed entry", `["id"]`, true},
		{"malformed list", `{}`, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			data := fmt.Sprintf(`{"version":4,"resources":[{"mode":"managed","type":"google_compute_disk","name":"a","instances":[{"attributes":{"id":"secret"},"sensitive_attributes":%s}]}]}`, c.paths)
			instances, err := Parse([]byte(data))
			if err != nil {
				t.Fatal(err)
			}
			if len(instances) != 1 || instances[0].IDSensitive != c.sensitive || instances[0].Attributes["id"] != "secret" {
				t.Fatalf("sensitivity or matching value lost: %+v", instances)
			}
			ix := NewIndex()
			ix.Add(instances)
			if !ix.Match(model.Resource{Type: "google_compute_disk", Identity: map[string]any{"id": "secret"}, RequiredKeys: []string{"id"}}).Managed {
				t.Fatal("sensitivity changed live matching")
			}
		})
	}
}

func TestOwnershipLegacyUnknownProviderKeepsFamiliesSeparate(t *testing.T) {
	a := ownershipInstance("disk.a", nil)
	b := ownershipInstance("disk.b", nil)
	b.Provider = "registry.terraform.io/acme/google"
	unknown := ownershipInstance("disk.unknown", nil)
	unknown.Provider, unknown.IDSensitive = "", true
	snapshots := []Snapshot{
		ownershipSnapshot("a", "a", 1, a),
		ownershipSnapshot("b", "b", 1, b),
		ownershipSnapshot("c", "c", 1, unknown),
	}
	var previous string
	for _, reverse := range []bool{false, true} {
		if reverse {
			slices.Reverse(snapshots)
		}
		ix := NewIndex()
		ix.AddSnapshots(snapshots)
		own, within := ix.Duplicates()
		if len(own) != 2 || len(within) != 0 {
			t.Fatalf("provider families merged: %+v, %+v", own, within)
		}
		for _, d := range own {
			if d.Reason != "no_identity" || len(d.Owners) != 2 || d.ID != "(sensitive)" || !d.IDSensitive {
				t.Fatalf("legacy ownership: %+v", d)
			}
		}
		if reverse && previous != canonical(own) {
			t.Fatalf("snapshot order changed ownership: %s, %s", previous, canonical(own))
		}
		previous = canonical(own)
	}
}
