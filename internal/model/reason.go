package model

import "slices"

// ReasonDetails is the stable, additive machine-readable explanation.
// MissingKeys contains names only, never identity or argument values.
type ReasonDetails struct {
	ReasonCode  string   `json:"reason_code,omitempty"`
	Action      string   `json:"action,omitempty"`
	MissingKeys []string `json:"missing_keys,omitempty"`
}

// Reason attaches an action only when the cause identifies a specific remedy.
func Reason(code string, keys ...string) ReasonDetails {
	keys = slices.Clone(keys)
	slices.Sort(keys)
	r := ReasonDetails{ReasonCode: code, MissingKeys: slices.Compact(keys)}
	switch code {
	case "state_identity_missing":
		r.Action = "refresh_state"
	case "limit_reached":
		r.Action = "raise_limit"
	case "api_disabled":
		r.Action = "enable_api"
	case "permission_denied":
		r.Action = "grant_permission"
	}
	return r
}

// ReasonLink points at the public reference. Callers pass internal codes.
func ReasonLink(code string) string {
	if code == "" {
		return ""
	}
	return "https://github.com/5xxeu/tforphans/blob/main/docs/reasons.md#" + code
}
