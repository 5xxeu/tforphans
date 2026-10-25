package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/report"
	"github.com/5xxeu/tforphans/internal/state"
)

func TestSensitiveOwnershipOutputs(t *testing.T) {
	const secret = "credential-that-must-stay-private"
	for _, c := range []struct {
		name string
		data string
		show bool
	}{
		{"raw attribute path", `{"version":4,"lineage":"a","serial":1,"resources":[{"mode":"managed","type":"google_compute_disk","name":"a","provider":"provider[\"registry.terraform.io/hashicorp/google\"]","instances":[{"attributes":{"id":"` + secret + `"},"sensitive_attributes":[["id"]]}]}]}`, false},
		{"raw cty path", `{"version":4,"lineage":"a","serial":1,"resources":[{"mode":"managed","type":"google_compute_disk","name":"a","provider":"provider[\"registry.terraform.io/hashicorp/google\"]","instances":[{"attributes":{"id":"` + secret + `"},"sensitive_attributes":[[{"type":"get_attr","value":"id"}]]}]}]}`, false},
		{"refresh", `{"prior_state":{"values":{"root_module":{"resources":[{"mode":"managed","type":"google_compute_disk","address":"google_compute_disk.a","provider_name":"registry.terraform.io/hashicorp/google","values":{"id":"` + secret + `"},"sensitive_values":{"id":true,"settings":{"password":true}}}]}}}}`, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var sensitive state.Snapshot
			var err error
			if c.show {
				sensitive, err = state.ParseShowSnapshot([]byte(c.data), "sensitive")
			} else {
				sensitive, err = state.ParseSnapshot([]byte(c.data), "sensitive")
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(sensitive.Instances) != 1 || !sensitive.Instances[0].IDSensitive {
				t.Fatalf("missing sensitivity: %+v", sensitive)
			}
			public := sensitive
			public.Lineage, public.Sources = "b", []string{"public"}
			public.Instances = slices.Clone(sensitive.Instances)
			public.Instances[0].Address, public.Instances[0].IDSensitive = "google_compute_disk.b", false
			for _, equivalent := range []bool{false, true} {
				for _, reverse := range []bool{false, true} {
					snapshots := []state.Snapshot{public, sensitive}
					if equivalent {
						// A public equivalent read must retain later sensitivity.
						copy := sensitive
						copy.Instances = slices.Clone(sensitive.Instances)
						copy.Instances[0].IDSensitive = false
						copy.Sources = []string{"equivalent"}
						if c.show {
							// Without a lineage only a repeated read of one source merges.
							copy.Sources = sensitive.Sources
						}
						snapshots = append([]state.Snapshot{copy}, snapshots...)
					}
					if reverse {
						slices.Reverse(snapshots)
					}
					ix := state.NewIndex()
					ix.AddSnapshots(snapshots)
					if !ix.Match(model.Resource{Type: "google_compute_disk", ProviderSource: "google", Identity: map[string]any{"id": secret}, RequiredKeys: []string{"id"}}).Managed {
						t.Fatal("sensitivity changed matching")
					}
					own, within := ix.Duplicates()
					if len(own) != 1 || len(within) != 0 || own[0].ID != "(sensitive)" || !own[0].IDSensitive || len(own[0].Owners) != 2 {
						t.Fatalf("equivalent=%v, reverse=%v: %+v, %+v", equivalent, reverse, own, within)
					}
					var text, output bytes.Buffer
					printOwnership(&text, own, within)
					if strings.Contains(text.String(), secret) || !strings.Contains(text.String(), "id=(sensitive)") {
						t.Fatalf("text redaction: %s", &text)
					}
					if err := report.JSON(&output, nil, nil, report.Summary{Ownership: own, InState: within}); err != nil {
						t.Fatal(err)
					}
					if strings.Contains(output.String(), secret) || !strings.Contains(output.String(), `"id": "(sensitive)"`) || !strings.Contains(output.String(), `"id_sensitive": true`) {
						t.Fatalf("JSON redaction: %s", &output)
					}
				}
			}
			// Within one state, a single sensitive address redacts the group.
			sensitive.Instances = append(slices.Clone(sensitive.Instances), public.Instances...)
			ix := state.NewIndex()
			ix.AddSnapshots([]state.Snapshot{sensitive})
			own, within := ix.Duplicates()
			if len(own) != 0 || len(within) != 1 || within[0].ID != "(sensitive)" || !within[0].IDSensitive {
				t.Fatalf("within-state redaction: %+v, %+v", own, within)
			}
			var text bytes.Buffer
			printOwnership(&text, own, within)
			if strings.Contains(text.String(), secret) || !strings.Contains(text.String(), "id=(sensitive)") {
				t.Fatalf("within-state text redaction: %s", &text)
			}
		})
	}
}

func TestOwnershipPartialIdentitySummaryCount(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell plugin")
	}
	var paths []string
	dir := t.TempDir()
	for i, region := range []string{"", "", "r"} {
		data := fmt.Sprintf(`{"version":4,"lineage":"lineage-%d","serial":1,"resources":[{"mode":"managed","type":"google_compute_disk","name":"a","provider":"provider[\"registry.terraform.io/hashicorp/google\"]","instances":[{"identity":{"id":"x","region":%q},"attributes":{"id":"disk-1"}}]}]}`, i, region)
		path := filepath.Join(dir, fmt.Sprintf("%d.tfstate", i))
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	for _, reverse := range []bool{false, true} {
		if reverse {
			slices.Reverse(paths)
		}
		args := []string{"-no-config", "-no-query", "-plugin", `printf '%s\n' '{"done":true}'`, "-format", "json"}
		for _, path := range paths {
			args = append(args, "-state", path)
		}
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr); code != exitOK {
			t.Fatalf("exit %d: %s", code, &stderr)
		}
		var result struct {
			Ownership []state.Duplicate `json:"duplicate_ownership"`
			Summary   report.Summary    `json:"summary"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Summary.PossibleDuplicateOwnership != 1 || result.Summary.DuplicateOwnership != 0 || result.Summary.DuplicateInState != 0 || len(result.Ownership) != 1 || result.Ownership[0].Reason != "partial_identity" || len(result.Ownership[0].Owners) != 3 {
			t.Fatalf("reverse=%v: %s", reverse, &stdout)
		}
	}
}
