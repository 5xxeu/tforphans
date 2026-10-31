package cli

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/dsource"
	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/report"
	"github.com/5xxeu/tforphans/internal/tf"
)

type listingReport struct {
	Orphans  []model.Resource `json:"orphans"`
	Summary  report.Summary   `json:"summary"`
	Baseline report.Baseline  `json:"baseline"`
}

func decodeListingReport(t *testing.T, data string) listingReport {
	t.Helper()
	var doc listingReport
	if err := json.Unmarshal([]byte(data), &doc); err != nil {
		t.Fatal(err)
	}
	for _, l := range doc.Summary.Listings {
		hash, err := hex.DecodeString(l.Fingerprint)
		if err != nil || len(hash) != 32 || l.Address == "" {
			t.Fatalf("invalid listing evidence: %+v", l)
		}
	}
	return doc
}

func listingStream(t *testing.T, address string, ids []string, failed bool) string {
	t.Helper()
	var out strings.Builder
	enc := json.NewEncoder(&out)
	write := func(kind string, fields map[string]any) {
		fields["address"], fields["resource_type"] = address, "foo_thing"
		if err := enc.Encode(map[string]any{kind: fields}); err != nil {
			t.Fatal(err)
		}
	}
	write("list_start", map[string]any{})
	for _, id := range ids {
		write("list_resource_found", map[string]any{"identity": map[string]any{"id": id, "project": "P"}})
	}
	if failed {
		write("diagnostic", map[string]any{"severity": "error", "summary": "permission denied"})
	} else {
		write("list_complete", map[string]any{"total": len(ids)})
	}
	return out.String()
}

func queryListingSetup(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("TF_DATA_DIR", ".terraform")
	if err := os.Mkdir("plugin-infra", 0o700); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "terraform")
	script := `#!/bin/sh
case "$1" in
 version) printf '%s\n' '{"terraform_version":"1.14.0"}' ;;
 providers) printf '%s\n' '{"provider_schemas":{"registry.terraform.io/hashicorp/foo":{"list_resource_schemas":{"foo_thing":{"block":{"attributes":{"project":{"type":"string","optional":true},"zone":{"type":"string","optional":true}}}}}}}}' ;;
 state) printf '%s\n' '{"version":4,"resources":[]}' ;;
 query) cat query.json ;;
 *) exit 1 ;;
esac
`
	writeListingFile(t, bin, script, 0o700)
	return dir, bin
}

func writeListingFile(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
}

func TestUserListingEvidence(t *testing.T) {
	for _, c := range []struct {
		name, config, address, reason string
		failed, extraFailed, complete bool
	}{
		{name: "unchanged complete", reason: "listing scope not confirmed", complete: true},
		{name: "narrow zone still returns known object", config: "zone = \"Z1\"", reason: "listing configuration changed", complete: true},
		{name: "changed block whitespace", config: "zone  = \"auto\"", reason: "listing configuration changed", complete: true},
		{name: "provider alias changed", config: "zone = \"auto\"", reason: "listing configuration changed", complete: true},
		{name: "for_each key removed", address: `list.foo_thing.tfo_custom["Z1"]`, reason: "not listed this run", complete: true},
		{name: "no instance started", address: "empty", reason: "not listed this run"},
		{name: "strict ignored instance failure", failed: true, reason: "listing incomplete"},
		{name: "other instance failed", reason: "listing scope not confirmed", extraFailed: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, bin := queryListingSetup(t)
			address := `list.foo_thing.tfo_custom["Z2"]`
			config := "zone = \"auto\""
			writeConfig := func(config, provider string) {
				text := fmt.Sprintf("list \"foo_thing\" \"tfo_custom\" {\n  for_each = toset([\"Z1\", \"Z2\"])\n  provider = %s\n  config {\n    project = \"P\"\n    %s\n  }\n}\n", provider, config)
				writeListingFile(t, "plugin-infra/user.tfquery.hcl", text, 0o600)
			}
			writeConfig(config, "foo")
			writeListingFile(t, "plugin-infra/query.json", listingStream(t, address, []string{"known", "gone"}, false), 0o600)
			args := []string{"-no-config", "-terraform", bin, "-current-workspace", "-format", "json", "-strict-ignore", "foo_thing", "plugin-infra"}
			code, initial, stderr := runReportTest(t, args...)
			if code != exitOrphans {
				t.Fatalf("initial exit %d: %s%s", code, initial, stderr)
			}
			before := decodeListingReport(t, initial)
			code, identical, stderr := runReportTest(t, args...)
			if code != exitOrphans || !reflect.DeepEqual(before.Summary.Listings, decodeListingReport(t, identical).Summary.Listings) {
				t.Fatalf("unstable identical runs: %s%s", identical, stderr)
			}
			baseline := filepath.Join(dir, "baseline.json")
			writeListingFile(t, baseline, initial, 0o600)
			if c.config != "" {
				config = c.config
			}
			provider := "foo"
			if c.name == "provider alias changed" {
				provider = "foo.other"
			}
			writeConfig(config, provider)
			if c.address != "" {
				data, err := os.ReadFile("plugin-infra/user.tfquery.hcl")
				if err != nil {
					t.Fatal(err)
				}
				keys := `toset(["Z1"])`
				if c.address == "empty" {
					keys = "toset([])"
				}
				writeListingFile(t, "plugin-infra/user.tfquery.hcl", strings.Replace(string(data), `toset(["Z1", "Z2"])`, keys, 1), 0o600)
			}
			currentAddress := address
			if c.address != "" {
				currentAddress = c.address
			}
			query := listingStream(t, currentAddress, []string{"known"}, c.failed)
			if c.address == "empty" {
				query = ""
			}
			if c.extraFailed {
				query += listingStream(t, `list.foo_thing.tfo_custom["Z1"]`, nil, true)
			}
			writeListingFile(t, "plugin-infra/query.json", query, 0o600)
			code, output, stderr := runReportTest(t, append(args, "-strict", "-baseline", baseline)...)
			// Ignored failures and a missing baseline finding do not alter coverage's exit rules.
			wantCode := exitOK
			if currentAddress == "empty" {
				wantCode = exitError
			}
			if code != wantCode {
				t.Fatalf("current exit %d: %s%s", code, output, stderr)
			}
			now := decodeListingReport(t, output)
			b := now.Baseline
			if c.reason == "" {
				if b.Resolved != 1 || b.Unverified != 0 {
					t.Fatalf("same listing not resolved: %s", output)
				}
			} else {
				wantUnverified := 1
				if currentAddress == "empty" {
					wantUnverified = len(before.Orphans)
				}
				if b.Resolved != 0 || b.Unverified != wantUnverified {
					t.Fatalf("want %q: %s", c.reason, output)
				}
				for _, f := range b.UnverifiedFindings {
					if f.Reason != c.reason {
						t.Fatalf("want %q: %+v", c.reason, f)
					}
				}
			}
			if c.complete && !now.Summary.Complete {
				t.Fatalf("coverage changed: %s", output)
			}
			if currentAddress != "empty" {
				l := now.Summary.Listings[0]
				if l.Source != "plugin-infra" || l.Provider != "hashicorp/foo" || l.Type != "foo_thing" || (l.Address != currentAddress && !c.extraFailed) {
					t.Fatalf("wrong listing: %+v", l)
				}
				for _, l := range now.Summary.Listings {
					if l.Address != currentAddress && l.Complete {
						t.Fatalf("failed instance complete: %+v", l)
					}
					if l.Address == currentAddress && l.Complete == c.failed {
						t.Fatalf("wrong instance completion: %+v", l)
					}
				}
			} else if len(now.Summary.Listings) != 0 {
				t.Fatalf("unstarted listing recorded: %s", output)
			}
			current := filepath.Join(dir, "current.json")
			writeListingFile(t, current, output, 0o600)
			code, diffJSON, stderr := runReportTest(t, "diff", "-format", "json", baseline, current)
			var d report.Diff
			if err := json.Unmarshal([]byte(diffJSON), &d); err != nil {
				t.Fatal(err)
			}
			// The run also knows what became of a finding (it saw every
			// live object); diff only has the two reports.
			if code != exitOK || !reflect.DeepEqual(d.Resolved, withoutTransitions(b.ResolvedFindings)) || !reflect.DeepEqual(d.Unverified, withoutTransitions(b.UnverifiedFindings)) {
				t.Fatalf("diff disagrees: %s%s", diffJSON, stderr)
			}
		})
	}
}

func TestGeneratedListingNarrowedZones(t *testing.T) {
	dir, bin := queryListingSetup(t)
	z1, z2 := `list.foo_thing.tfo_foo_foo_thing["project=P,zone=Z1"]`, `list.foo_thing.tfo_foo_foo_thing["project=P,zone=Z2"]`
	args := []string{"-no-config", "-terraform", bin, "-current-workspace", "-format", "json", "-arg", "project=P", "plugin-infra"}
	writeListingFile(t, "plugin-infra/query.json", listingStream(t, z1, []string{"one"}, false)+listingStream(t, z2, []string{"two"}, false), 0o600)
	code, initial, stderr := runReportTest(t, append(args, "-arg", "zone=Z1,Z2")...)
	if code != exitOrphans {
		t.Fatalf("initial exit %d: %s%s", code, initial, stderr)
	}
	before := decodeListingReport(t, initial)
	if len(before.Summary.Listings) != 2 {
		t.Fatalf("instances merged: %s", initial)
	}
	baseline := filepath.Join(dir, "baseline.json")
	writeListingFile(t, baseline, initial, 0o600)
	writeListingFile(t, "plugin-infra/query.json", listingStream(t, z1, []string{"one"}, false), 0o600)
	code, output, stderr := runReportTest(t, append(args, "-arg", "zone=Z1", "-strict", "-baseline", baseline)...)
	now := decodeListingReport(t, output)
	if code != exitOK || now.Baseline.Known != 1 || now.Baseline.Resolved != 0 || now.Baseline.Unverified != 1 || now.Baseline.UnverifiedFindings[0].Reason != "not listed this run" || !now.Summary.Complete {
		t.Fatalf("narrow nonempty listing exit %d: %s%s", code, output, stderr)
	}
	if now.Summary.Listings[0].Fingerprint == before.Summary.Listings[0].Fingerprint {
		t.Fatal("changed -arg values did not change fingerprint")
	}
}

func TestDiffWithoutListingEvidence(t *testing.T) {
	dir := t.TempDir()
	old, current := filepath.Join(dir, "old.json"), filepath.Join(dir, "new.json")
	saveReportTest(t, old, []model.Resource{{Type: "thing", Identity: map[string]any{"id": "gone"}, Source: "infra"}}, report.Summary{})
	writeListingFile(t, current, `{"schema_version":1,"orphans":[],"summary":{"complete":true,"coverage":[{"type":"thing","status":"complete"}]}}`, 0o600)
	code, output, stderr := runReportTest(t, "diff", "-format", "json", old, current)
	var d report.Diff
	if err := json.Unmarshal([]byte(output), &d); err != nil {
		t.Fatal(err)
	}
	if code != exitOK || len(d.Resolved) != 0 || len(d.Unverified) != 1 || d.Unverified[0].Reason != "no listing evidence" {
		t.Fatalf("older report exit %d: %s%s", code, output, stderr)
	}
}

func TestBaselineScanListing(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("TF_PLUGIN_CACHE_DIR", dir)
	bin := filepath.Join(dir, "terraform")
	script := `#!/bin/sh
case "$1" in
 init) exit 0 ;;
 providers) printf '%s\n' '{"provider_schemas":{"hashicorp/foo":{"resource_schemas":{"foo_thing":{}},"data_source_schemas":{"foo_accounts":{"block":{"attributes":{"result":{"type":["list",["object",{"id":"string","name":"string"}]]}}}},"foo_things":{"block":{"attributes":{"account_id":{"type":"string","required":true},"result":{"type":["list",["object",{"id":"string"}]]}}}}}}}}' ;;
 apply) printf '{"version":4,"resources":[{"mode":"data","type":"foo_accounts","name":"all","instances":[{"attributes":{"result":[{"id":"a1","name":"A"}]}}]},{"mode":"data","type":"foo_things","name":"by_account_id","instances":[{"index_key":"a1","attributes":{"result":%s}}]}]}\n' "$TFO_TEST_SCAN_RESULT" > terraform.tfstate ;;
 *) exit 1 ;;
esac
`
	writeListingFile(t, bin, script, 0o700)
	sources["listingtest"] = dsource.Adapter{Name: "listingtest", Provider: "hashicorp/foo", Target: "account", Scopes: []dsource.Scope{{Arg: "account_id", Parent: "foo_accounts", Prefix: "accounts"}}}
	t.Cleanup(func() { delete(sources, "listingtest") })
	args := []string{"-no-config", "-no-query", "-terraform", bin, "-scan", "listingtest", "-allow-unpinned", "-format", "json"}
	t.Setenv("TFO_TEST_SCAN_RESULT", `[{"id":"gone"}]`)
	code, initial, stderr := runReportTest(t, append(args, "-scan-scope", "account_id=A")...)
	if code != exitOrphans {
		t.Fatalf("initial exit %d: %s%s", code, initial, stderr)
	}
	before := decodeListingReport(t, initial)
	if before.Orphans[0].ListAddress != `data.foo_things.by_account_id["a1"]` || before.Summary.Listings[0].Address != before.Orphans[0].ListAddress {
		t.Fatalf("scan address mismatch: %s", initial)
	}
	baseline := filepath.Join(dir, "baseline.json")
	writeListingFile(t, baseline, initial, 0o600)
	for _, scope := range []string{"A", "a1"} {
		t.Run(scope, func(t *testing.T) {
			t.Setenv("TFO_TEST_SCAN_RESULT", "[]")
			code, output, stderr := runReportTest(t, append(args, "-scan-scope", "account_id="+scope, "-baseline", baseline)...)
			if code != exitOK {
				t.Fatalf("current exit %d: %s%s", code, output, stderr)
			}
			now := decodeListingReport(t, output)
			if scope == "A" {
				if now.Baseline.Resolved != 1 || now.Baseline.Unverified != 0 || !reflect.DeepEqual(before.Summary.Listings, now.Summary.Listings) {
					t.Fatalf("unchanged scan not resolved: %s", output)
				}
			} else if now.Baseline.Resolved != 0 || now.Baseline.Unverified != 1 || now.Baseline.UnverifiedFindings[0].Reason != "listing configuration changed" {
				t.Fatalf("changed scan scope resolved: %s", output)
			}
		})
	}
}

func withoutTransitions(fs []report.Finding) []report.Finding {
	out := make([]report.Finding, len(fs))
	for i, f := range fs {
		f.Transition, f.StateAddress, f.Rule = "", "", ""
		out[i] = f
	}
	return out
}

func TestQueryListingScope(t *testing.T) {
	for _, c := range []struct {
		name                          string
		user, optIn, dynamic, limited bool
	}{
		{name: "generated"},
		{name: "user", user: true},
		{name: "generated opt-in", optIn: true},
		{name: "user opt-in", user: true, optIn: true},
		{name: "user variable project", user: true, dynamic: true},
		{name: "identity state limit", limited: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, bin := queryListingSetup(t)
			unverified := c.optIn || c.dynamic || c.limited
			t.Setenv("FAKE_ACCOUNT", "acct-1")
			savedSource, hadSource := tf.IdentitySources["hashicorp/foo"]
			tf.IdentitySources["hashicorp/foo"] = []tf.IdentityRead{{DataSource: "foo_whoami", Attrs: map[string][]string{"account": {"account"}}}}
			savedOptIn, hadOptIn := tf.OptIn["foo_thing"]
			if c.optIn {
				tf.OptIn["foo_thing"] = "lists everything the credentials can see"
			}
			t.Cleanup(func() {
				if hadSource {
					tf.IdentitySources["hashicorp/foo"] = savedSource
				} else {
					delete(tf.IdentitySources, "hashicorp/foo")
				}
				if hadOptIn {
					tf.OptIn["foo_thing"] = savedOptIn
				} else {
					delete(tf.OptIn, "foo_thing")
				}
			})
			script, err := os.ReadFile(bin)
			if err != nil {
				t.Fatal(err)
			}
			identity := ` plan) exit 0 ;;
 show) printf '{"prior_state":{"values":{"root_module":{"resources":[{"mode":"data","address":"data.foo_whoami.tfo_id_0_0","values":{"account":"%s"}}]}}}}\n' "$FAKE_ACCOUNT" ;;
`
			writeListingFile(t, bin, strings.Replace(string(script), " query)", identity+" query)", 1), 0700)
			address := "list.foo_thing.tfo_foo_foo_thing"
			if c.user {
				address = "list.foo_thing.mine"
				config := ""
				if c.dynamic {
					config = "  config { project = var.target_project }\n"
				}
				writeListingFile(t, "plugin-infra/user.tfquery.hcl", "list \"foo_thing\" \"mine\" {\n  provider = foo\n"+config+"}\n", 0600)
			}
			args := []string{"-no-config", "-terraform", bin, "-current-workspace", "-format", "json", "-types", "foo_thing", "plugin-infra"}
			if c.limited {
				args = append(args, "-max-state-size", "128")
			}
			t.Setenv("TF_VAR_target_project", "A")
			writeListingFile(t, "plugin-infra/query.json", listingStream(t, address, []string{"gone"}, false), 0600)
			code, baseline, stderr := runReportTest(t, args...)
			if code != exitOrphans {
				t.Fatalf("baseline exit %d: %s%s", code, baseline, stderr)
			}
			before := decodeListingReport(t, baseline)
			if len(before.Summary.Listings) != 1 {
				t.Fatal(baseline)
			}
			scope := before.Summary.Listings[0].Scope
			if unverified && scope != nil || !unverified && scope["account"] != "acct-1" {
				t.Fatalf("wrong scope: %s", baseline)
			}
			if c.limited && !strings.Contains(strings.Join(before.Summary.Notes, "\n"), "state is larger than 128 bytes") {
				t.Fatalf("identity limit was not reported: %s", baseline)
			}
			t.Setenv("TF_VAR_target_project", "B")
			baselinePath := filepath.Join(dir, "baseline.json")
			writeListingFile(t, baselinePath, baseline, 0600)
			writeListingFile(t, "plugin-infra/query.json", listingStream(t, address, nil, false), 0600)
			code, output, stderr := runReportTest(t, append(args, "-baseline", baselinePath)...)
			if code != exitOK {
				t.Fatalf("current exit %d: %s%s", code, output, stderr)
			}
			current := decodeListingReport(t, output)
			if !reflect.DeepEqual(before.Summary.Listings, current.Summary.Listings) {
				t.Fatalf("listing changed: %s", output)
			}
			b := current.Baseline
			if unverified {
				if b.Resolved != 0 || b.Unverified != 1 || b.UnverifiedFindings[0].Reason != "listing scope not confirmed" {
					t.Fatal(output)
				}
			} else if b.Resolved != 1 || b.Unverified != 0 {
				t.Fatal(output)
			}
		})
	}
}

// TestArgScope: a region or project given to the list block replaces the
// provider's; one whose value is unknown leaves the scope unknown.
func TestArgScope(t *testing.T) {
	scope := map[string]string{"account_id": "1", "region": "us-east-1"}
	for _, c := range []struct {
		name    string
		values  map[string]string
		unknown []string
		want    map[string]string
	}{
		{"none", nil, nil, scope},
		{"region", map[string]string{"region": "eu-west-1", "name": "x"}, nil, map[string]string{"account_id": "1", "region": "eu-west-1"}},
		{"unknown region", nil, []string{"region"}, nil},
		{"unknown other", nil, []string{"filter"}, scope},
	} {
		if got := argScope(scope, c.values, c.unknown); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
	if scope["region"] != "us-east-1" {
		t.Error("provider scope changed")
	}
	if argScope(nil, map[string]string{"region": "x"}, nil) != nil {
		t.Error("unknown provider scope became known")
	}
}
