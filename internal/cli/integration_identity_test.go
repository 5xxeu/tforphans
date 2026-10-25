//go:build integration

package main

import (
	"os"
	"path/filepath"
	"testing"
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
