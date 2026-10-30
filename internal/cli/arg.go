package cli

import (
	"regexp"
	"slices"
	"strings"

	"github.com/5xxeu/tforphans/internal/state"
	"github.com/5xxeu/tforphans/internal/tf"
)

func redactArg(arg string) string {
	parsed, err := tf.ParseArg(arg)
	if err != nil {
		return "<redacted>"
	}
	key, _, _ := strings.Cut(arg, "=")
	if parsed.Secret() {
		return key + "=<redacted>"
	}
	if strings.Contains(arg, "://") {
		values := slices.Clone(parsed.Values)
		for i, value := range values {
			values[i] = redactArgURLs(value)
		}
		return redactArgURLs(key + "=" + strings.Join(values, ","))
	}
	return arg
}

func redactArgURLs(arg string) string {
	if name, value, ok := strings.Cut(arg, "="); ok && (name == "-state" || name == "--state") {
		return name + "=" + redactArgURLs(value)
	}
	arg = argURLUserinfo.ReplaceAllString(arg, "${1}")
	urls := argURLScheme.FindAllStringIndex(arg, -1)
	if len(urls) == 0 {
		return arg
	}
	if urls[0][0] == 0 {
		return state.Redact(arg)
	}
	var out strings.Builder
	out.WriteString(arg[:urls[0][0]])
	for i, loc := range urls {
		end := len(arg)
		if i+1 < len(urls) {
			end = urls[i+1][0]
		}
		value := arg[loc[0]:end]
		// A URL embedded inside a query belongs to that query too.
		if i+1 < len(urls) && strings.ContainsAny(value, "?#") && !strings.HasSuffix(value, ",") {
			out.WriteString(state.Redact(arg[loc[0]:]))
			break
		}
		separator := ""
		if i+1 < len(urls) && strings.HasSuffix(value, ",") {
			value, separator = strings.TrimSuffix(value, ","), ","
		}
		if strings.ContainsAny(value, "?#") {
			value = state.Redact(value)
		}
		out.WriteString(value)
		out.WriteString(separator)
	}
	return out.String()
}

var (
	argURLScheme   = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://`)
	argURLUserinfo = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s]*@`)
)
