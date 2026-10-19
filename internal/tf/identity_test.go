package tf

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestIdentityHCL(t *testing.T) {
	configs := []IdentityConfig{{Ref: "aws.prod", Source: "hashicorp/aws"}, {Ref: "google", Source: "hashicorp/google-beta"}}
	hcl, targets := identityHCL(configs)
	want := "data \"aws_caller_identity\" \"tfo_id_0_0\" {\n  provider = aws.prod\n}\ndata \"aws_region\" \"tfo_id_0_1\" {\n  provider = aws.prod\n}\ndata \"google_client_config\" \"tfo_id_1_0\" {\n  provider = google\n}\n"
	if string(hcl) != want || !reflect.DeepEqual(targets, []string{"data.aws_caller_identity.tfo_id_0_0", "data.aws_region.tfo_id_0_1", "data.google_client_config.tfo_id_1_0"}) {
		t.Fatalf("%s\n%v", hcl, targets)
	}
}

func TestParseIdentities(t *testing.T) {
	configs := []IdentityConfig{{Ref: "aws", Source: "hashicorp/aws"}}
	for _, c := range []struct {
		name            string
		region          any
		missing, failed bool
	}{
		{name: "old name", region: map[string]any{"name": "eu"}},
		{name: "new region wins", region: map[string]any{"region": "eu", "name": "old"}},
		{name: "empty", region: map[string]any{"region": ""}},
		{name: "non string", region: map[string]any{"region": 42}, failed: true},
		{name: "missing attribute", region: map[string]any{}, failed: true},
		{name: "missing data source", missing: true, failed: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			resources := []any{map[string]any{"mode": "data", "address": "data.aws_caller_identity.tfo_id_0_0", "values": map[string]string{"account_id": "a"}}}
			if !c.missing {
				resources = append(resources, map[string]any{"mode": "data", "address": "data.aws_region.tfo_id_0_1", "values": c.region})
			}
			data, _ := json.Marshal(map[string]any{"prior_state": map[string]any{"values": map[string]any{"root_module": map[string]any{"resources": resources}}}})
			scopes, failed := parseIdentities(data, configs)
			if (failed["aws"] != nil) != c.failed {
				t.Fatalf("%v %v", scopes, failed)
			}
			if !c.failed {
				want := "eu"
				if c.name == "empty" {
					want = ""
				}
				if scopes["aws"]["account_id"] != "a" || scopes["aws"]["region"] != want {
					t.Fatal(scopes)
				}
			}
		})
	}
}

func TestSelectedWorkspace(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TF_WORKSPACE", "")
	t.Setenv("TF_DATA_DIR", "data")
	if got := SelectedWorkspace(dir); got != "default" {
		t.Fatal(got)
	}
	if err := os.Mkdir(filepath.Join(dir, "data"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data", "environment"), []byte("  stored\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := SelectedWorkspace(dir); got != "stored" {
		t.Fatal(got)
	}
	t.Setenv("TF_WORKSPACE", "env")
	if got := SelectedWorkspace(dir); got != "env" {
		t.Fatal(got)
	}
}

func TestIdentitiesFallback(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "terraform")
	script := `#!/bin/sh
case "$1" in
plan)
 if grep -q 'provider = aws.bad' zz_tforphans_*_identity.tf; then echo 'credentials unavailable' >&2; exit 1; fi
 ;;
show)
 printf '%s' '{"prior_state":{"values":{"root_module":{"resources":[{"mode":"data","address":"data.aws_caller_identity.tfo_id_0_0","values":{"account_id":"a"}},{"mode":"data","address":"data.aws_region.tfo_id_0_1","values":{"name":"eu"}}]}}}}'
 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	r := Runner{Dir: dir, Bin: bin}
	configs := []IdentityConfig{{Ref: "aws.good", Source: "hashicorp/aws"}, {Ref: "aws.bad", Source: "hashicorp/aws"}, {Ref: "aws.good", Source: "hashicorp/aws"}, {Ref: "other", Source: "hashicorp/other"}}
	scopes, failed, err := r.Identities(context.Background(), Plan{}, configs)
	if err != nil || len(scopes) != 1 || scopes["aws.good"]["account_id"] != "a" || len(failed) != 1 || !strings.Contains(failed["aws.bad"].Error(), "credentials unavailable") {
		t.Fatalf("%v %v %v", scopes, failed, err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "zz_tforphans_*")); len(left) != 0 {
		t.Fatal(left)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := r.Identities(ctx, Plan{}, configs); err != context.Canceled {
		t.Fatal(err)
	}
}

func TestIdentitiesPlanFiles(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "terraform")
	script := `#!/bin/sh
case "$1" in
plan)
 test -f zz_tforphans_*_override.tf || exit 1
 test -f zz_tforphans_*_identity.tf || exit 1
 for arg in "$@"; do
  case "$arg" in
   -out=*) printf '%s' "${arg#-out=}" > plan-path; printf 'private' > "${arg#-out=}" ;;
  esac
 done
 ;;
show) printf '%s' '{"prior_state":{"values":{"root_module":{"resources":[{"mode":"data","address":"data.google_client_config.tfo_id_0_0","values":{"project":"","region":"","zone":"","access_token":"secret"}}]}}}}' ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	r := Runner{Dir: dir, Bin: bin}
	p := Plan{Declare: map[string]string{"tfo-google": "hashicorp/google"}}
	configs := []IdentityConfig{{Ref: "tfo-google", Source: "hashicorp/google-beta"}}
	scopes, failed, err := r.Identities(context.Background(), p, configs)
	if err != nil || len(failed) != 0 || !reflect.DeepEqual(scopes["tfo-google"], map[string]string{"project": "", "region": "", "zone": ""}) {
		t.Fatalf("%v %v %v", scopes, failed, err)
	}
	planPath, err := os.ReadFile(filepath.Join(dir, "plan-path"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(string(planPath))); !os.IsNotExist(err) {
		t.Fatalf("private plan directory remains: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "zz_tforphans_*")); len(left) != 0 {
		t.Fatal(left)
	}
	r.MaxStateSize = 10
	scopes, failed, err = r.Identities(context.Background(), p, configs)
	if err != nil || len(scopes) != 0 || failed["tfo-google"] == nil || strings.Contains(failed["tfo-google"].Error(), "secret") {
		t.Fatalf("%v %v %v", scopes, failed, err)
	}
}

func TestIdentitiesCanceledPlan(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "terraform")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	scopes, failed, err := (Runner{Dir: dir, Bin: bin}).Identities(ctx, Plan{}, []IdentityConfig{{Ref: "aws", Source: "hashicorp/aws"}, {Ref: "aws.prod", Source: "hashicorp/aws"}})
	if err != context.DeadlineExceeded || scopes != nil || failed != nil {
		t.Fatalf("%v %v %v", scopes, failed, err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "zz_tforphans_*")); len(left) != 0 {
		t.Fatal(left)
	}
}
