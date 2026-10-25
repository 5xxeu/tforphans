package cli

import (
	"slices"
	"strings"
)

func doctorConfiguredArgs(o *options, fixes []string) []commandArg {
	args := slices.Clone(o.doctor.args)
	set := map[string]bool{}
	for _, arg := range args {
		if !arg.positional && !arg.end {
			set[arg.name] = true
		}
	}
	var preserved []commandArg
	for i := 0; i+1 < len(fixes); i += 2 {
		name := strings.TrimPrefix(fixes[i], "-")
		if set[name] {
			continue
		}
		set[name] = true
		f := o.doctor.flagSet.Lookup(name)
		if f == nil {
			continue
		}
		// A CLI occurrence replaces the config's whole list, so keep the
		// effective values before adding the first occurrence as a fix.
		if values, ok := f.Value.(*listFlag); ok {
			for _, value := range *values {
				preserved = append(preserved, commandArg{raw: "-" + name, name: name, value: value, separate: true})
			}
		}
	}
	at := slices.IndexFunc(args, func(arg commandArg) bool { return arg.end })
	if at < 0 {
		at = len(args)
	}
	return slices.Insert(args, at, preserved...)
}

func doctorNext(args []commandArg, fixes []string) []string {
	out := []string{"tforphans"}
	var roots []string
	ended := false
	var extra []string
	seen := map[string]bool{}
	for i := 0; i+1 < len(fixes); i += 2 {
		if !seen[fixes[i+1]] {
			value := redactArgURLs(fixes[i+1])
			if fixes[i] == "-arg" {
				value = redactArg(fixes[i+1])
			}
			extra = append(extra, fixes[i], value)
			seen[fixes[i+1]] = true
		}
	}
	for _, arg := range args {
		if arg.end {
			ended = true
			continue
		}
		if arg.positional {
			roots = append(roots, redactArgURLs(arg.raw))
			continue
		}
		if arg.name == "offline" || arg.name == "check-auth" || arg.name == "format" {
			continue
		}
		value := arg.value
		switch arg.name {
		case "arg":
			value = redactArg(value)
		case "plugin":
			value = "<plugin command>"
		default:
			value = redactArgURLs(value)
		}
		if arg.separate {
			out = append(out, arg.raw, value)
		} else if name, _, attached := strings.Cut(arg.raw, "="); attached {
			out = append(out, name+"="+value)
		} else {
			out = append(out, arg.raw)
		}
	}
	out = append(out, extra...)
	if len(roots) > 0 || ended {
		out = append(out, "--")
	}
	out = append(out, roots...)
	for i, arg := range out {
		out[i] = redactArgURLs(arg)
	}
	return out
}

func shellArg(s string) string {
	if s != "" && !strings.ContainsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_@%+=:,./-", r))
	}) {
		return s
	}
	return shellQuote(s)
}

func shellQuote(s string) string {
	if strings.Contains(s, "  ") {
		parts := strings.Split(s, " ")
		for i, part := range parts {
			parts[i] = "'" + strings.ReplaceAll(part, "'", "'\"'\"'") + "'"
		}
		return strings.Join(parts, "' '")
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func (d *doctorReport) updateArgumentFixes() {
	values := map[string]map[string]bool{}
	for _, fix := range d.argFixes {
		if values[fix.name] == nil {
			values[fix.name] = map[string]bool{}
		}
		values[fix.name][fix.value] = true
	}
	kept := []string{}
	for i := 0; i+1 < len(d.fixes); i += 2 {
		if d.fixes[i] != "-arg" {
			kept = append(kept, d.fixes[i:i+2]...)
		}
	}
	d.fixes = kept
	byCheck := map[int][]string{}
	userLists := map[int]string{}
	seen := map[string]bool{}
	for _, fix := range d.argFixes {
		if fix.userList != "" {
			userLists[fix.check] = fix.userList
		}
		name := fix.name
		if len(values[name]) > 1 {
			name = fix.typ + ":" + name
		}
		arg := name + "=" + fix.value
		if !slices.Contains(byCheck[fix.check], arg) {
			byCheck[fix.check] = append(byCheck[fix.check], arg)
		}
		if !seen[arg] {
			d.fixes = append(d.fixes, "-arg", arg)
			seen[arg] = true
		}
	}
	for check, args := range byCheck {
		d.Checks[check].Remediation = "tforphans -arg " + strings.Join(args, " -arg ")
		if userLists[check] != "" {
			d.Checks[check].Remediation = userLists[check] + "; " + d.Checks[check].Remediation
		}
	}
}
