package model

import "testing"

func TestTargetOf(t *testing.T) {
	for in, want := range map[string]string{
		"https://vault.example.com:8200":               "vault.example.com:8200",
		"https://root:pw@PVE.example:8006/api2/json?x": "pve.example:8006",
		"pve.example":             "pve.example",
		"https://[fd00::1]:8006/": "[fd00::1]:8006",
		"":                        "",
		"https://":                "",
		"https://exa mple.com":    "",
	} {
		if got := TargetOf(in); got != want {
			t.Errorf("TargetOf(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"https://a", "a/b", "a?x", "u@a", "a b", ""} {
		if ValidTarget(bad) {
			t.Errorf("ValidTarget(%q)", bad)
		}
	}
}
