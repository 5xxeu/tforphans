package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/tf"
)

func TestDoctorScanLimitations(t *testing.T) {
	bin := doctorScanFixture(t)
	var schema map[string]any
	if err := json.Unmarshal([]byte(doctorScanSchema), &schema); err != nil {
		t.Fatal(err)
	}
	provider := schema["provider_schemas"].(map[string]any)["registry.terraform.io/cloudflare/cloudflare"].(map[string]any)
	resources := provider["resource_schemas"].(map[string]any)
	data := provider["data_source_schemas"].(map[string]any)
	for _, category := range []string{"identity", "required", "pagination", "result", "plural"} {
		for i := 0; i < 12; i++ {
			typ := fmt.Sprintf("cloudflare_%s_%02d", category, i)
			resources[typ] = map[string]any{"block": map[string]any{}}
			if category == "plural" {
				continue
			}
			attrs := map[string]any{"result": map[string]any{"nested_type": map[string]any{"attributes": map[string]any{"id": map[string]any{}}}}}
			switch category {
			case "identity":
				attrs["result"] = map[string]any{"nested_type": map[string]any{"attributes": map[string]any{"name": map[string]any{}}}}
			case "required":
				attrs["site_id"] = map[string]any{"required": true}
			case "pagination":
				attrs["page"] = map[string]any{"required": true}
				attrs["per_page"] = map[string]any{"required": true}
			case "result":
				delete(attrs, "result")
			}
			data[typ+"s"] = map[string]any{"block": map[string]any{"attributes": attrs}}
		}
	}
	for _, typ := range []string{"cloudflare_account", "cloudflare_worker", "cloudflare_zero_trust_dlp_predefined_entry"} {
		resources[typ] = map[string]any{"block": map[string]any{}}
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCTOR_SCHEMA_JSON", string(raw))
	args := []string{"doctor", "-no-config", "-terraform", bin, "-no-query", "-scan", "cloudflare", "-state-roots", "."}
	for _, stateFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("stateFailure=%t", stateFailure), func(t *testing.T) {
			want := 0
			if stateFailure {
				want = 2
				t.Setenv("DOCTOR_STATE_ERROR", "Error: AccessDenied")
			}
			var out bytes.Buffer
			if code := run(context.Background(), append(slices.Clone(args), "-format", "json"), &out, io.Discard); code != want {
				t.Fatalf("exit %d want %d: %s", code, want, &out)
			}
			var d doctorReport
			if err := json.Unmarshal(out.Bytes(), &d); err != nil {
				t.Fatal(err)
			}
			categories, warnings := 0, 0
			for _, c := range d.Checks {
				if c.ID != "scan.cloudflare.plan" {
					if c.Status == "failed" && c.ID != "states.read" {
						t.Fatalf("unexpected failure: %+v", c)
					}
					continue
				}
				if c.Category == "" {
					if c.Status != "ok" || c.Count == nil || *c.Count != 1 {
						t.Fatalf("plan: %+v", c)
					}
					continue
				}
				categories++
				if c.Status == "warning" {
					warnings++
				} else if c.Status != "skipped" {
					t.Fatalf("unfixable limitation failed: %+v", c)
				}
				if c.Remediation != "" || len(c.Types) == 0 || c.Count == nil || *c.Count != len(c.Types) || len(c.Skipped) != len(c.Types) {
					t.Fatalf("category: %+v", c)
				}
			}
			if categories != 6 || warnings != 3 {
				t.Fatalf("%d categories, %d warnings: %s", categories, warnings, &out)
			}
			if c := doctorFindCheck(t, d, "discovery", ""); c.Status != "ok" {
				t.Fatal(c)
			}
			for _, verbose := range []bool{false, true} {
				out.Reset()
				argv := append(slices.Clone(args), "-format", "text")
				if verbose {
					argv = append(argv, "-v")
				}
				if code := run(context.Background(), argv, &out, io.Discard); code != want {
					t.Fatalf("text exit %d: %s", code, &out)
				}
				// Without -v only warnings and problems are listed.
				lines := 3
				if verbose {
					lines = 7
				}
				if n := strings.Count(out.String(), "scan.cloudflare.plan"); n != lines {
					t.Fatalf("%d plan lines: %s", n, &out)
				}
				if strings.Contains(out.String(), "cloudflare_identity_11") != verbose {
					t.Fatalf("verbose=%t: %s", verbose, &out)
				}
				if !strings.Contains(out.String(), "cloudflare_identity_00") {
					t.Fatal(&out)
				}
			}
		})
	}
}

func TestDoctorQuerySkipGroups(t *testing.T) {
	bin := doctorFixture(t)
	types := map[string][]string{"aws_ready": nil}
	for i := 0; i < 12; i++ {
		types[fmt.Sprintf("aws_missing_%02d", i)] = []string{"region"}
	}
	t.Setenv("DOCTOR_SCHEMA_JSON", doctorTestSchema(t, map[string]map[string][]string{"registry.terraform.io/hashicorp/aws": types}))
	if err := os.WriteFile("main.tf", []byte("terraform {\n required_providers {\n aws = { source = \"hashicorp/aws\" }\n }\n}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, verbose := range []bool{false, true} {
		var out bytes.Buffer
		args := []string{"doctor", "-no-config", "-terraform", bin}
		if verbose {
			args = append(args, "-v")
		}
		if code := run(context.Background(), args, &out, io.Discard); code != 2 {
			t.Fatalf("exit %d: %s", code, &out)
		}
		if strings.Count(out.String(), "FAIL  query.arguments") != 1 || strings.Contains(out.String(), "aws_missing_11") != verbose {
			t.Fatalf("verbose=%t: %s", verbose, &out)
		}
	}
	var out bytes.Buffer
	if code := run(context.Background(), []string{"doctor", "-no-config", "-terraform", bin, "-format", "json"}, &out, io.Discard); code != 2 {
		t.Fatalf("exit %d: %s", code, &out)
	}
	var d doctorReport
	if err := json.Unmarshal(out.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(d.Checks, func(c doctorCheck) bool { return c.ID == "query.arguments" && c.Category != "" })
	if i < 0 {
		t.Fatalf("missing argument group: %+v", d)
	}
	c := d.Checks[i]
	if c.Status != "failed" || len(c.Types) != 12 || len(c.Skipped) != 12 || c.Count == nil || *c.Count != 12 || !strings.Contains(c.Remediation, "-arg region=...") {
		t.Fatalf("group: %+v", c)
	}
	out.Reset()
	if code := run(context.Background(), []string{"doctor", "-no-config", "-terraform", bin, "-arg", "region=auto", "-format", "json"}, &out, io.Discard); code != 2 {
		t.Fatalf("exit %d: %s", code, &out)
	}
	if err := json.Unmarshal(out.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	i = slices.IndexFunc(d.Checks, func(c doctorCheck) bool {
		return c.ID == "query.plan" && c.Category == "unsupported automatic argument"
	})
	if i < 0 || len(d.Checks[i].Types) != 12 || d.Checks[i].Status != "failed" || d.Checks[i].Remediation == "" {
		t.Fatalf("unsupported argument group: %+v", d)
	}
}

func TestDoctorPlanSkipCategories(t *testing.T) {
	for _, tc := range []struct {
		sk   tf.Skip
		want string
	}{
		{tf.Skip{Reason: "source results carry no id"}, "results have no identity"},
		{tf.Skip{Reason: "source requires page, per_page"}, "unsupported required arguments"},
		{tf.Skip{Reason: "source requires list_id"}, "unsupported required arguments"},
		{tf.Skip{Reason: "source has no result list"}, "unsupported result schema"},
		{tf.Skip{Reason: "listed as other", Excluded: true}, "listed as another type"},
	} {
		if got := doctorScanCategory(tc.sk); got != tc.want {
			t.Fatalf("%q: %q want %q", tc.sk.Reason, got, tc.want)
		}
	}
}
