package model

import "testing"

func TestSameProvider(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"", "registry.terraform.io/hashicorp/aws", true},
		{"registry.terraform.io/hashicorp/aws", "registry.terraform.io/hashicorp/aws", true},
		{"registry.terraform.io/hashicorp/aws", "registry.opentofu.org/hashicorp/aws", true},
		{"registry.terraform.io/hashicorp/aws", "hashicorp/aws", true},
		{"registry.terraform.io/hashicorp/aws", "aws", true},
		{"registry.terraform.io/Hashicorp/AWS", "hashicorp/aws", true},
		{"registry.terraform.io/hashicorp/google", "registry.terraform.io/hashicorp/google-beta", true},
		{"registry.terraform.io/hashicorp/github", "registry.terraform.io/integrations/github", true},
		{"registry.terraform.io/acme/foo", "registry.terraform.io/other/foo", false},
		{"example.com/fake/fake", "example.com/fake2/fake", false},
		{"example.com/acme/foo", "registry.terraform.io/acme/foo", false},
	} {
		if got := SameProvider(c.a, c.b); got != c.want {
			t.Errorf("SameProvider(%q, %q) = %v", c.a, c.b, got)
		}
	}
}
