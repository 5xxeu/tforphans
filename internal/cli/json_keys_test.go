package cli

import "slices"

// jsonKeys collects object keys, except inside maps keyed by data (identity
// keys, type names, scopes).
func jsonKeys(v any, keys []string) []string {
	switch v := v.(type) {
	case map[string]any:
		for k, e := range v {
			if !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
			switch k {
			case "identity", "attributes", "by_type", "by_scope", "scope":
				continue
			}
			keys = jsonKeys(e, keys)
		}
	case []any:
		for _, e := range v {
			keys = jsonKeys(e, keys)
		}
	}
	return keys
}
