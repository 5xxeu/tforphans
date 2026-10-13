package state

import (
	"fmt"
	"slices"
	"testing"

	"github.com/5xxeu/tforphans/internal/model"
)

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
