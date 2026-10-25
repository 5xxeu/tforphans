package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/proc"
	"github.com/5xxeu/tforphans/internal/state"
	"github.com/5xxeu/tforphans/internal/tf"
)

func TestDoctorStaleRoots(t *testing.T) {
	for _, query := range []bool{true, false} {
		for _, offline := range []bool{true, false} {
			for _, alive := range []bool{true, false} {
				t.Run(fmt.Sprintf("query=%t/offline=%t/alive=%t", query, offline, alive), func(t *testing.T) {
					bin := doctorFixture(t)
					pid := 99999999
					if alive {
						pid = os.Getpid()
					} else if proc.Alive(pid) {
						t.Skip("cannot identify dead process")
					}
					name := fmt.Sprintf("zz_tforphans_%d_version_override.tf", pid)
					if err := os.WriteFile(name, []byte("# tforphans temporary file, run test\nterraform { required_version = \">= 0\" }\n"), 0600); err != nil {
						t.Fatal(err)
					}
					// Invalid provider configuration must not be read before cleanup succeeds.
					if alive {
						if err := os.WriteFile("main.tf", []byte("invalid {"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					o, err := parseFlags([]string{"doctor", "-no-config", "-terraform", bin}, &bytes.Buffer{})
					if err != nil {
						t.Fatal(err)
					}
					o.doctor.offline = offline
					var d doctorReport
					ok := d.root(context.Background(), o, ".", query, map[state.TypeKey]bool{})
					c := doctorFindCheck(t, d, "root.stale", ".")
					if alive {
						if ok || c.Status != "failed" || !strings.Contains(c.Remediation, "zz_tforphans_*") || len(d.Checks) != 1 {
							t.Fatalf("root continued after cleanup failure: %+v", d)
						}
						if _, err := os.Stat(name); err != nil {
							t.Fatal(err)
						}
					} else {
						if !ok || c.Status != "warning" || c.Count == nil || *c.Count != 1 {
							t.Fatalf("cleanup: %+v", d)
						}
						if _, err := os.Stat(name); !os.IsNotExist(err) {
							t.Fatalf("stale file remains: %v", err)
						}
						doctorFindCheck(t, d, "root.config", ".")
					}
				})
			}
		}
	}
}

func TestDoctorStaleVersionOverride(t *testing.T) {
	if proc.Alive(99999999) {
		t.Skip("cannot identify dead process")
	}
	bin := doctorFixture(t)
	if err := os.Rename(bin, bin+"-fixture"); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
if [ "$1 $2" = "providers schema" ] && [ ! -f zz_tforphans_99999999_version_override.tf ]; then
 printf 'Error: Unsupported Terraform Core version' >&2
 exit 1
fi
exec "$(dirname "$0")/terraform-fixture" "$@"
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("main.tf", []byte(`terraform { required_version = ">= 999.0.0" }`), 0600); err != nil {
		t.Fatal(err)
	}
	stale := "zz_tforphans_99999999_version_override.tf"
	if err := os.WriteFile(stale, []byte("# tforphans temporary file, run test\nterraform { required_version = \">= 0\" }"), 0600); err != nil {
		t.Fatal(err)
	}
	r := tf.Runner{Bin: bin, Dir: "."}
	if _, err := r.ProvidersSchema(context.Background()); err != nil {
		t.Fatalf("override prerequisite: %v", err)
	}
	o, err := parseFlags([]string{"doctor", "-no-config", "-offline", "-terraform", bin}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	var d doctorReport
	d.root(context.Background(), o, ".", true, map[state.TypeKey]bool{})
	if c := doctorFindCheck(t, d, "providers.schema", "."); c.Status != "failed" {
		t.Fatalf("schema used stale override: %+v", c)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale override remains: %v", err)
	}
	if _, err := r.ProvidersSchema(context.Background()); err == nil || !strings.Contains(err.Error(), "Unsupported Terraform Core version") {
		t.Fatalf("version constraint: %v", err)
	}
}

func TestDoctorStaleFailureSkipsState(t *testing.T) {
	bin := doctorFixture(t)
	name := fmt.Sprintf("zz_tforphans_%d_version_override.tf", os.Getpid())
	if err := os.WriteFile(name, []byte("# tforphans temporary file, run test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Another root remains independently readable.
	other := filepath.Join(t.TempDir(), "other")
	if err := os.MkdirAll(filepath.Join(other, ".terraform"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "main.tf"), []byte("terraform {}"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code := run(context.Background(), []string{"doctor", "-no-config", "-terraform", bin, "-arg", "region=eu", "-format", "json", ".", other}, &out, &stderr)
	if code != 2 {
		t.Fatalf("exit %d: %s\n%s", code, &out, &stderr)
	}
	var d doctorReport
	if err := json.Unmarshal(out.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	var skipped bool
	for _, c := range d.Checks {
		if c.ID == "states.read" && c.Status == "skipped" && strings.Contains(c.Message, "cleanup") {
			skipped = true
		}
		if c.ID == "states.read" && c.Total != nil && (*c.Total != 2 || *c.Read != 2) {
			t.Fatalf("blocked root was read: %+v", c)
		}
	}
	if !skipped {
		t.Fatalf("missing state skip: %+v", d)
	}
}
