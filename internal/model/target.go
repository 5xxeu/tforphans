package model

import (
	"net/url"
	"regexp"
	"strings"
)

var targetRe = regexp.MustCompile(`^[A-Za-z0-9._:\[\]-]{1,260}$`)

// ValidTarget reports whether s can be a Resource.Target: a host name or
// address with an optional port. A URL, user name or query could carry a
// secret and is refused.
func ValidTarget(s string) bool { return targetRe.MatchString(s) }

// TargetOf reduces a server address (https://user:pw@vault.example:8200/x)
// to its host and port, "" when it has no usable host. Nothing else of the
// address is kept.
func TargetOf(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	if !strings.Contains(addr, "://") {
		addr = "https://" + addr
	}
	u, err := url.Parse(addr)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Host)
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	if !ValidTarget(host) {
		return ""
	}
	return host
}
