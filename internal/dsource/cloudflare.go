package dsource

import (
	"encoding/json"
	"strings"
)

// Cloudflare covers cloudflare/cloudflare 5.x, whose plural data sources
// are generated from the API: results are in result[], objects carry an id,
// and the scope is account_id or zone_id.
var Cloudflare = Adapter{
	Name:     "cloudflare",
	Provider: "registry.terraform.io/cloudflare/cloudflare",
	Target:   "account",
	Scopes: []Scope{
		{Arg: "account_id", Parent: "cloudflare_accounts", Prefix: "accounts"},
		{Arg: "zone_id", Parent: "cloudflare_zones", Prefix: "zones"},
	},
	Skip: map[string]string{
		"cloudflare_account":                          "the account itself",
		"cloudflare_worker":                           "listed as cloudflare_workers_script",
		"cloudflare_zero_trust_dlp_entry":             "listed as cloudflare_zero_trust_dlp_custom_entry",
		"cloudflare_zero_trust_dlp_predefined_entry":  "built-in entries",
		"cloudflare_zero_trust_dlp_integration_entry": "entries owned by integrations",
	},
	AltTypes: map[string][]string{
		"cloudflare_dns_record":                          {"cloudflare_record"},
		"cloudflare_workers_script":                      {"cloudflare_worker_script", "cloudflare_worker"},
		"cloudflare_zero_trust_dlp_custom_entry":         {"cloudflare_zero_trust_dlp_entry"},
		"cloudflare_workers_route":                       {"cloudflare_worker_route"},
		"cloudflare_workers_custom_domain":               {"cloudflare_worker_domain"},
		"cloudflare_zero_trust_access_application":       {"cloudflare_access_application"},
		"cloudflare_zero_trust_access_policy":            {"cloudflare_access_policy"},
		"cloudflare_zero_trust_tunnel_cloudflared":       {"cloudflare_tunnel"},
		"cloudflare_zero_trust_tunnel_cloudflared_route": {"cloudflare_tunnel_route"},
		"cloudflare_zero_trust_gateway_policy":           {"cloudflare_teams_rule"},
		"cloudflare_zero_trust_device_custom_profile":    {"cloudflare_device_settings_policy"},
	},
	Keep: cloudflareKeep,
	// The provider cannot list a zone whose records have data of
	// different shapes (data.flags is a number for CAA and a string for
	// NAPTR: cloudflare/terraform-provider-cloudflare#5581); such a zone is
	// read again one record type at a time.
	Split: map[string]Split{"cloudflare_dns_records": {Attr: "type", Values: []string{
		"A", "AAAA", "CAA", "CERT", "CNAME", "DNSKEY", "DS", "HTTPS", "LOC", "MX", "NAPTR",
		"NS", "OPENPGPKEY", "PTR", "SMIMEA", "SRV", "SSHFP", "SVCB", "TLSA", "TXT", "URI",
	}}},
	// Data sources taking both account_id and zone_id usually serve one of
	// them; the other fails in the provider before any request.
	NotApplicable: func(message string) bool {
		return strings.Contains(message, "missing required account_id parameter") ||
			strings.Contains(message, "missing required zone_id parameter")
	},
	// A script's id is its name; cloudflare_worker keys it by the tag.
	AltIdentity: func(resourceType string, item map[string]any) map[string]map[string]any {
		if tag, ok := item["tag"].(string); ok && resourceType == "cloudflare_workers_script" && tag != "" {
			return map[string]map[string]any{"cloudflare_worker": {"id": tag}}
		}
		return nil
	},
}

// cloudflareManagedRulesets are the global IDs of rulesets Cloudflare owns.
// The API accepts kind "managed" on create, so kind alone could let a
// user-made ruleset hide; an unknown ID is simply reported. Checked against
// the API (DDoS L7, Normalization, Free) and the WAF docs' ID suffixes.
var cloudflareManagedRulesets = map[string]bool{
	"efb7b8c949ac4650a09736fc376e9aee": true, // Cloudflare Managed Ruleset
	"4814384a9e5d4991b9815dcfc25d2f1f": true, // Cloudflare OWASP Core Ruleset
	"c2e184081120413c86c3ab7e14069605": true, // Cloudflare Exposed Credentials Check
	"77454fe2d30c4220b5701f6fdfb893ba": true, // Cloudflare Managed Free Ruleset
	"e22d83c647c64a3eae91b71b499d988e": true, // Cloudflare Sensitive Data Detection
	"4d21379b4f9f4bb088e0729962c8b3cf": true, // DDoS L7 ruleset
	"70339d97bdb34195bbf054b1ebe81f76": true, // Cloudflare Normalization Ruleset
}

// cloudflareKeep drops objects Cloudflare creates and owns, judged only by
// fields users cannot set (a field a user can set, such as a name or the
// default-network flag, would let a hand-made object hide): Cloudflare's
// managed rulesets, Universal SSL packs (only "advanced" can be ordered),
// managed Gateway certificates, records owned by other objects, built-in
// DLP entries, the resource library catalog, and singletons that have
// resources of their own.
func cloudflareKeep(resourceType string, item map[string]any) bool {
	switch resourceType {
	case "cloudflare_ruleset":
		id, _ := item["id"].(string)
		return !(item["kind"] == "managed" && cloudflareManagedRulesets[id])
	case "cloudflare_certificate_pack":
		return item["type"] != "universal"
	case "cloudflare_zero_trust_gateway_certificate":
		return item["type"] != "gateway_managed"
	case "cloudflare_zero_trust_dlp_custom_entry":
		return item["type"] == nil || item["type"] == "custom"
	case "cloudflare_dns_record":
		// Records of Workers custom domains and Email Routing.
		meta := asMap(item["meta"])
		if meta["read_only"] == true || meta["email_routing"] == true {
			return false
		}
		if v, _ := meta["origin_worker_id"].(string); v != "" {
			return false
		}
	case "cloudflare_email_routing_rule":
		// The catch-all rule is cloudflare_email_routing_catch_all.
		matchers, _ := item["matchers"].([]any)
		for _, m := range matchers {
			if asMap(m)["type"] == "all" {
				return false
			}
		}
	case "cloudflare_zero_trust_resource_library_application":
		// Cloudflare's catalog of known applications, in every account.
		return item["application_source"] != "cloudflare"
	case "cloudflare_zero_trust_device_custom_profile":
		// The default profile is cloudflare_zero_trust_device_default_profile.
		if item["default"] == true {
			return false
		}
	}
	return true
}

// asMap accepts an object or a JSON-encoded object (dynamic attributes are
// stored as strings).
func asMap(v any) map[string]any {
	switch t := v.(type) {
	case map[string]any:
		return t
	case string:
		var m map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(t)), &m) == nil {
			return m
		}
	}
	return nil
}
