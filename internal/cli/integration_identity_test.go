//go:build integration

package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/5xxeu/tforphans/internal/report"
	"github.com/5xxeu/tforphans/internal/tf"
)

func TestIntegrationBaselineIdentity(t *testing.T) {
	for _, c := range []struct{ name, account, workspace, reason string }{
		{name: "same account", account: "acct-1"},
		{name: "changed account", account: "acct-2", reason: "listing scope changed"},
		{name: "changed workspace", account: "acct-1", workspace: "other", reason: "listing configuration changed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			data, root := setup(t)
			write(t, filepath.Join(root, "main.tf"), `terraform {
 required_providers {
  fake = { source = "example.com/fake/fake" }
 }
}
`)
			t.Chdir(root)
			t.Setenv("FAKE_ACCOUNT", "acct-1")
			t.Setenv("TF_WORKSPACE", "default")
			write(t, filepath.Join(data, "thing", "gone"), "")
			args := []string{"-no-config", "-types", "fake_thing", "-format", "json"}
			code, baseline, stderr := runTool(args...)
			if code != exitOrphans {
				t.Fatalf("baseline exit %d: %s\n%s", code, baseline, stderr)
			}
			before := decodeListingReport(t, baseline)
			if len(before.Summary.Listings) != 1 || before.Summary.Listings[0].Scope["account"] != "acct-1" {
				t.Fatalf("scope: %s", baseline)
			}
			write(t, "baseline.json", baseline)
			if err := os.Remove(filepath.Join(data, "thing", "gone")); err != nil {
				t.Fatal(err)
			}
			t.Setenv("FAKE_ACCOUNT", c.account)
			if c.workspace != "" {
				t.Setenv("TF_WORKSPACE", "")
				terraform(t, root, "workspace", "new", c.workspace)
				t.Setenv("TF_WORKSPACE", c.workspace)
			}
			code, output, stderr := runTool(append(args, "-baseline", "baseline.json")...)
			if code != exitOK {
				t.Fatalf("current exit %d: %s\n%s", code, output, stderr)
			}
			b := decodeListingReport(t, output).Baseline
			if c.reason == "" {
				if b.Resolved != 1 || b.Unverified != 0 || b.ResolvedFindings[0].Transition != "no_longer_found" {
					t.Fatalf("%s", output)
				}
			} else if b.Resolved != 0 || b.Unverified != 1 || b.UnverifiedFindings[0].Reason != c.reason {
				t.Fatalf("%s", output)
			}
			if left, _ := filepath.Glob("zz_tforphans_*"); len(left) != 0 {
				t.Fatal(left)
			}
		})
	}
}

// TestIntegrationArgScope: a list argument named like a scope key (here a
// zone the provider also reports) gives each listing the zone it lists,
// not the provider's.
func TestIntegrationArgScope(t *testing.T) {
	_, root := setup(t)
	tf.IdentitySources["example.com/fake/fake"] = []tf.IdentityRead{{DataSource: "fake_whoami", Attrs: map[string][]string{"account": {"account"}, "zone": {"account"}}}}
	write(t, filepath.Join(root, "main.tf"), `terraform {
 required_providers {
  fake = { source = "example.com/fake/fake" }
 }
}
`)
	t.Chdir(root)
	t.Setenv("FAKE_ACCOUNT", "acct-1")
	code, output, stderr := runTool("-no-config", "-types", "fake_zoned", "-arg", "zone=a,b", "-format", "json", "-expect", "zone=acct-1", "-expect", "zone=a")
	if code != exitOK {
		t.Fatalf("exit %d: %s\n%s", code, output, stderr)
	}
	doc := decodeListingReport(t, output)
	zones := map[string]string{}
	for _, l := range doc.Summary.Listings {
		zones[l.Address] = l.Scope["zone"]
		if l.Scope["account"] != "acct-1" {
			t.Errorf("account: %+v", l)
		}
	}
	if !reflect.DeepEqual(zones, map[string]string{`list.fake_zoned.tfo_fake_fake_zoned["zone=a"]`: "a", `list.fake_zoned.tfo_fake_fake_zoned["zone=b"]`: "b"}) {
		t.Fatalf("zones %v: %s", zones, output)
	}
	if e := doc.Summary.Expected; len(e) != 2 || e[0].Status != report.ExpectMissing || e[1].Status != report.ExpectConfirmed {
		t.Fatalf("expected scopes: %s", output)
	}
}
