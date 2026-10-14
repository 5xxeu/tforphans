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

func TestListingMixedScopes(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		for _, complete := range []bool{false, true} {
			var s Summary
			scopes := []string{"", "project B"}
			if reverse {
				scopes[0], scopes[1] = scopes[1], scopes[0]
			}
			for _, scope := range scopes {
				s.AddListing("infra", "hashicorp/foo", "thing", scope, scope != "" || complete)
			}
			// The listing without an explicit scope returned an object in A.
			s.AddListing("infra", "hashicorp/foo", "thing", "project A", true)
			want := []Listing{{Source: "infra", Provider: "hashicorp/foo", Type: "thing", Scopes: []string{"project A", "project B"}, Complete: complete}}
			if !reflect.DeepEqual(s.Listings, want) {
				t.Fatalf("reverse %t, complete %t: %+v", reverse, complete, s.Listings)
			}
		}
	}
}
