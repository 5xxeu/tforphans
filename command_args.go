package main

import (
	"flag"
	"strings"
)

type commandArg struct {
	raw, name, value          string
	separate, positional, end bool
}

func commandArgs(fs *flag.FlagSet, args []string) []commandArg {
	var out []commandArg
	ended := false
	for i := 0; i < len(args); i++ {
		raw := args[i]
		arg := commandArg{raw: raw}
		switch {
		case ended || raw == "-" || !strings.HasPrefix(raw, "-"):
			arg.positional = true
		case raw == "--":
			arg.end, ended = true, true
		default:
			name := strings.TrimPrefix(strings.TrimPrefix(raw, "-"), "-")
			var attached bool
			arg.name, arg.value, attached = strings.Cut(name, "=")
			if f := fs.Lookup(arg.name); f != nil && !attached {
				boolean, ok := f.Value.(interface{ IsBoolFlag() bool })
				if (!ok || !boolean.IsBoolFlag()) && i+1 < len(args) {
					i++
					arg.value, arg.separate = args[i], true
				}
			}
		}
		out = append(out, arg)
	}
	return out
}

func (a commandArg) parts() []string {
	if a.separate {
		return []string{a.raw, a.value}
	}
	return []string{a.raw}
}
