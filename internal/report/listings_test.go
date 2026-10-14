package report

import (
	"reflect"
	"testing"
)

func TestListingInstances(t *testing.T) {
	var s Summary
	s.AddListing("root-b", "registry.terraform.io/hashicorp/google-beta", "thing", "project Z", true)
	s.AddListing("root-a", "hashicorp/google", "thing", "", true)
	s.AddListing("root-b", "hashicorp/google", "thing", "project A", false)
	s.AddListing("root-b", "hashicorp/google", "thing", "project Z", true)
	s.AddListing("root-b", "acme/google", "thing", "project A", true)
	s.AddListing("root-b", "hashicorp/google", "other", "", true)
	s.SortListings()
	want := []Listing{
		{Source: "root-a", Provider: "hashicorp/google", Type: "thing", Scopes: []string{}, Complete: true},
		{Source: "root-b", Provider: "acme/google", Type: "thing", Scopes: []string{"project A"}, Complete: true},
		{Source: "root-b", Provider: "hashicorp/google", Type: "other", Scopes: []string{}, Complete: true},
		{Source: "root-b", Provider: "hashicorp/google", Type: "thing", Scopes: []string{"project A", "project Z"}, Complete: false},
	}
	if !reflect.DeepEqual(s.Listings, want) {
		t.Fatalf("listing instances: %+v", s.Listings)
	}
}
