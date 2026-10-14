package report

import (
	"encoding/hex"
	"reflect"
	"testing"
)

func TestListingInstances(t *testing.T) {
	var s Summary
	a := Listing{Source: "root-b", Address: `list.thing.all["Z1"]`, Provider: "registry.terraform.io/hashicorp/google-beta", Type: "thing", Complete: true, Fingerprint: "a"}
	b := a
	b.Address, b.Complete, b.Fingerprint = `list.thing.all["Z2"]`, false, "b"
	s.AddListing(b)
	s.AddListing(a)
	a.Provider = "hashicorp/google"
	b.Provider = a.Provider
	s.SortListings()
	if !reflect.DeepEqual(s.Listings, []Listing{a, b}) {
		t.Fatalf("instances merged: %+v", s.Listings)
	}
}

func TestListingFingerprint(t *testing.T) {
	base := ListingFingerprint("block text", `"Z1"`, "aws.other", "")
	decoded, err := hex.DecodeString(base)
	if err != nil || len(decoded) != 32 || base != ListingFingerprint("block text", `"Z1"`, "aws.other", "") {
		t.Fatalf("unstable sha256: %s", base)
	}
	for _, args := range [][4]string{
		{"block text ", `"Z1"`, "aws.other", ""},
		{"block text", `"Z2"`, "aws.other", ""},
		{"block text", `"Z1"`, "aws", ""},
		{"block text", `"Z1"`, "aws.other", "scan"},
	} {
		if base == ListingFingerprint(args[0], args[1], args[2], args[3]) {
			t.Fatalf("configuration ignored: %v", args)
		}
	}
}
