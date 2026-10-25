package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/tf"
)

func TestDoctorNextConfigArguments(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		for _, cli := range []string{"", "-arg", "-arg="} {
			name := cli
			if explicit {
				name += " explicit"
			}
			t.Run(name, func(t *testing.T) {
				bin := doctorFixture(t)
				config := "arg: [\"aws_*:region=eu-north-1\", \"aws_*:region=us-east-1\"]\n"
				path := defaultConfigFile
				if explicit {
					path = "custom.yaml"
				}
				if err := os.WriteFile(path, []byte(config), 0600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("DOCTOR_SCHEMA_JSON", doctorTestSchema(t, map[string]map[string][]string{
					"registry.terraform.io/hashicorp/aws":    {"aws_thing": {"region"}},
					"registry.terraform.io/hashicorp/google": {"google_thing": {"zone"}},
				}))
				args := []string{"doctor", "-terraform", bin, "-format", "json"}
				if explicit {
					args = append(args, "-config", path)
				}
				if cli == "-arg" {
					args = append(args, "-arg", "aws_*:region=cli")
				}
				if cli == "-arg=" {
					args = append(args, "-arg=aws_*:region=cli")
				}
				args = append(args, "--", ".")
				original, err := parseFlags(args, io.Discard)
				if err != nil {
					t.Fatal(err)
				}
				var out bytes.Buffer
				if code := run(context.Background(), args, &out, io.Discard); code != 2 {
					t.Fatalf("exit %d: %s", code, &out)
				}
				var d doctorReport
				if err := json.Unmarshal(out.Bytes(), &d); err != nil {
					t.Fatal(err)
				}
				next, err := parseFlags(d.NextCommand[1:], io.Discard)
				if err != nil {
					t.Fatal(err)
				}
				want := append(slices.Clone(original.src.args), tf.Arg{Name: "zone", Values: []string{"auto"}})
				if !reflect.DeepEqual(next.src.args, want) {
					t.Fatalf("next args %+v want %+v, command %q", next.src.args, want, d.NextCommand)
				}
				if cli != "" && strings.Contains(strings.Join(d.NextCommand, " "), "eu-north-1") {
					t.Fatalf("overridden config restored: %q", d.NextCommand)
				}
				end := slices.Index(d.NextCommand, "--")
				if end < 0 || slices.Index(d.NextCommand, "zone=auto") > end {
					t.Fatal(d.NextCommand)
				}
			})
		}
	}
}

func TestDoctorNextConfigLists(t *testing.T) {
	doctorFixture(t)
	if err := os.WriteFile(defaultConfigFile, []byte("arg: [\"region=eu\", \"api_key=SECRET\", \"endpoint=https://user:password@host/state?token=SECRET\"]\nscan: [cloudflare]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	original, err := parseFlags([]string{"doctor"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	// Scan is also added by discovery suggestions and must retain its config list.
	fixes := []string{"-arg", "zone=auto", "-scan", "proxmox"}
	command := doctorNext(doctorConfiguredArgs(original, fixes), fixes)
	joined := strings.Join(command, " ")
	if strings.Contains(joined, "SECRET") || strings.Contains(joined, "password") {
		t.Fatal(command)
	}
	next, err := parseFlags(command[1:], io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := []tf.Arg{
		{Name: "region", Values: []string{"eu"}},
		{Name: "api_key", Values: []string{"<redacted>"}},
		{Name: "endpoint", Values: []string{"https://host/state?REDACTED"}},
		{Name: "zone", Values: []string{"auto"}},
	}
	if !reflect.DeepEqual(next.src.args, want) || !reflect.DeepEqual(next.src.scans, []string{"cloudflare", "proxmox"}) {
		t.Fatalf("args=%+v scans=%q command=%q", next.src.args, next.src.scans, command)
	}
	noFix := doctorNext(doctorConfiguredArgs(original, nil), nil)
	if slices.Contains(noFix, "-arg") || slices.Contains(noFix, "-scan") {
		t.Fatalf("config unnecessarily copied: %q", noFix)
	}
	unchanged, err := parseFlags(noFix[1:], io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(unchanged.src.args, original.src.args) || !reflect.DeepEqual(unchanged.src.scans, original.src.scans) {
		t.Fatalf("config changed without fixes: %+v", unchanged)
	}
}

func TestDoctorRootWithoutConfiguration(t *testing.T) {
	for _, mode := range []string{"state", "query", "unread state"} {
		t.Run(mode, func(t *testing.T) {
			bin := doctorFixture(t)
			if err := os.Remove("main.tf"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile("terraform.tfstate", []byte(`{"version":4,"resources":[]}`), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("DOCTOR_STATE_JSON", `{"version":4,"resources":[]}`)
			args := []string{"doctor", "-no-config", "-terraform", bin, "-format", "json", "."}
			want, status := 0, "warning"
			if mode == "query" {
				args = append(args, "-arg", "region=eu")
				want, status = 2, "failed"
			} else {
				args = append(args, "-no-query", "-plugin", "must not run")
			}
			if mode == "unread state" {
				t.Setenv("DOCTOR_STATE_ERROR", "Error: AccessDenied")
				want = 2
			}
			var out bytes.Buffer
			if code := run(context.Background(), args, &out, io.Discard); code != want {
				t.Fatalf("exit %d want %d: %s", code, want, &out)
			}
			var d doctorReport
			if err := json.Unmarshal(out.Bytes(), &d); err != nil {
				t.Fatal(err)
			}
			c := doctorFindCheck(t, d, "root.config", ".")
			if c.Status != status {
				t.Fatalf("config: %+v", c)
			}
			if mode == "state" {
				if c := doctorFindCheck(t, d, "states.read", ""); c.Status != "ok" {
					t.Fatal(c)
				}
			}
		})
	}
}
