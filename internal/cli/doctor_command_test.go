package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestDoctorWithoutTerraform(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("local.tfstate", []byte(`{"version":4,"resources":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, withRoot := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit state only", true: "root needs Terraform"}[withRoot], func(t *testing.T) {
			args := []string{"doctor", "-no-config", "-terraform", filepath.Join(t.TempDir(), "missing-terraform"), "-format", "json", "-no-query", "-plugin", "x", "-state", "local.tfstate"}
			want, status := 0, "skipped"
			if withRoot {
				args = append(args, ".")
				want, status = 1, "failed"
			}
			var out bytes.Buffer
			if code := run(context.Background(), args, &out, io.Discard); code != want {
				t.Fatalf("exit %d want %d: %s", code, want, &out)
			}
			var d doctorReport
			if err := json.Unmarshal(out.Bytes(), &d); err != nil {
				t.Fatal(err)
			}
			c := doctorFindCheck(t, d, "terraform.version", "")
			if c.Status != status || c.Message == "" {
				t.Fatal(c)
			}
			if !withRoot && d.Status != "ready" {
				t.Fatal(d.Status)
			}
			if withRoot && d.Status != "unknown" {
				t.Fatal(d.Status)
			}
		})
	}
}

func TestDoctorNextPositionalRoots(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, root := range []string{"diff", "adopt", "doctor", "-x"} {
		t.Run(root, func(t *testing.T) {
			bin := doctorFixture(t)
			t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("DOCTOR_SCHEMA_JSON", doctorTestSchema(t, map[string]map[string][]string{"registry.terraform.io/hashicorp/aws": {"aws_thing": nil}}))
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "main.tf"), []byte("terraform {}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"doctor", "-format", "json"}
			if strings.HasPrefix(root, "-") {
				args = append(args, "--")
			}
			args = append(args, root)
			o, err := parseFlags(args, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if code := run(context.Background(), args, &out, io.Discard); code != 0 {
				t.Fatalf("exit %d: %s", code, &out)
			}
			var d doctorReport
			if err := json.Unmarshal(out.Bytes(), &d); err != nil {
				t.Fatal(err)
			}
			next := d.NextCommand
			want := []string{"tforphans", "--", root}
			if !reflect.DeepEqual(next, want) {
				t.Fatalf("%q want %q", next, want)
			}
			audit, err := parseFlags(next[1:], io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if audit.adopt != nil || audit.doctor != nil || !slices.Equal(audit.src.roots, o.src.roots) {
				t.Fatalf("root interpreted as command or flag: %+v", audit)
			}
		})
	}
}
