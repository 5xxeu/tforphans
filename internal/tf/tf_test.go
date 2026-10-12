package tf

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/5xxeu/tforphans/internal/glob"
)

const schemaJSON = `{"provider_schemas": {
  "registry.terraform.io/hashicorp/aws": {
    "list_resource_schemas": {
      "aws_instance": {"block": {"attributes": {"region": {"type": "string", "optional": true}}}},
      "aws_api_gateway_stage": {"block": {"attributes": {"rest_api_id": {"type": "string", "required": true}, "region": {"type": "string", "optional": true}}}},
      "aws_s3_bucket": {"block": {"attributes": {"region": {"type": "string", "optional": true}}}}
    },
    "resource_identity_schemas": {
      "aws_instance": {"attributes": {"id": {"required_for_import": true}, "region": {"optional_for_import": true}}},
      "azurerm_thing": {"attributes": {"name": {"required_for_import": true}, "subscription_id": {"required_for_import": true}}}
    },
    "resource_schemas": {
      "aws_instance": {"block": {"attributes": {"id": {}, "region": {}}}},
      "azurerm_thing": {"block": {"attributes": {"id": {}, "name": {}}}}
    }
  },
  "registry.terraform.io/acme/thing": {
    "list_resource_schemas": {"thing_x": {"block": {"block_types": {"filter": {"min_items": 1}}}}}
  },
  "registry.terraform.io/other/widget": {
    "list_resource_schemas": {"widget_y": {"block": {}}}
  }
}}`

func schemas(t *testing.T) *Schemas {
	t.Helper()
	var s Schemas
	if err := json.Unmarshal([]byte(schemaJSON), &s); err != nil {
		t.Fatal(err)
	}
	return &s
}

func TestBuildPlan(t *testing.T) {
	declared := map[string]string{"registry.terraform.io/acme/thing": "acme"}
	region, _ := ParseArg("region=eu-north-1, us-east-1")
	p := BuildPlan(schemas(t), declared, PlanOptions{
		Args:    []Arg{region},
		Exclude: glob.CompileList([]string{"aws_s3_*"}),
	})
	var queried []string
	for _, q := range p.Queries {
		queried = append(queried, q.Provider+":"+q.Type)
	}
	if got := strings.Join(queried, " "); got != "aws:aws_instance tfo-other-widget:widget_y" {
		t.Errorf("queries = %s", got)
	}
	skipped := map[string]string{}
	for _, s := range p.Skipped {
		skipped[s.Type] = s.Reason
	}
	for typ, want := range map[string]string{
		"aws_api_gateway_stage": "requires rest_api_id",
		"thing_x":               "requires filter {}",
	} {
		if !strings.Contains(skipped[typ], want) {
			t.Errorf("skip reason for %s = %q, want it to mention %q", typ, skipped[typ], want)
		}
	}
	if _, ok := skipped["aws_s3_bucket"]; ok {
		t.Error("excluded type reported as skipped")
	}

	if got := string(p.ProvidersHCL()); !strings.Contains(got, `tfo-other-widget = { source = "registry.terraform.io/other/widget" }`) {
		t.Errorf("providers HCL:\n%s", got)
	}

	hcl := string(p.HCL(500))
	for _, want := range []string{
		`list "aws_instance" "tfo_aws_aws_instance" {`,
		`provider = aws`,
		`limit    = 500`,
		`"region=us-east-1" = { region = "us-east-1" }`,
		`region = each.value.region`,
	} {
		if !strings.Contains(hcl, want) {
			t.Errorf("HCL lacks %q:\n%s", want, hcl)
		}
	}
}

func TestBuildPlanArgSatisfiesRequired(t *testing.T) {
	a, _ := ParseArg("rest_api_id=abc")
	p := BuildPlan(schemas(t), nil, PlanOptions{Args: []Arg{a}, Include: glob.CompileList([]string{"aws_api_gateway_stage"})})
	if len(p.Queries) != 1 || len(p.Skipped) != 0 {
		t.Fatalf("queries=%v skipped=%v", p.Queries, p.Skipped)
	}
}

func TestIdentityKeys(t *testing.T) {
	k := schemas(t).IdentityKeys()
	if got := k.Required["aws_instance"]; len(got) != 1 || got[0] != "id" {
		t.Errorf("required %v", got)
	}
	// subscription_id is never an attribute, so state without identity
	// cannot be checked against it.
	if got := strings.Join(k.Required["azurerm_thing"], ","); got != "name,subscription_id" {
		t.Errorf("required %s", got)
	}
	if got := strings.Join(k.Attribute["azurerm_thing"], ","); got != "name" {
		t.Errorf("attribute %s", got)
	}
}

func noKeys(string, string) ([]string, []string) { return nil, nil }

// google and google-beta can define one type with different identities.
func TestIdentityKeysPerProvider(t *testing.T) {
	var s Schemas
	json.Unmarshal([]byte(`{"provider_schemas": {
	  "registry.terraform.io/acme/a": {"resource_identity_schemas": {"x_thing": {"attributes": {"id": {"required_for_import": true}}}},
	    "resource_schemas": {"x_thing": {"block": {"attributes": {"id": {}}}}}},
	  "registry.terraform.io/acme/b": {"resource_identity_schemas": {"x_thing": {"attributes": {"name": {"required_for_import": true}}}},
	    "resource_schemas": {"x_thing": {"block": {"attributes": {"id": {}}}}}}
	}}`), &s)
	k := s.IdentityKeys()
	if r, a := k.For("registry.terraform.io/acme/a", "x_thing"); strings.Join(r, ",") != "id" || strings.Join(a, ",") != "id" {
		t.Errorf("a: %v %v", r, a)
	}
	if r, a := k.For("registry.terraform.io/acme/b", "x_thing"); strings.Join(r, ",") != "name" || a == nil || len(a) != 0 {
		t.Errorf("b: %v %#v", r, a)
	}
	// Unknown provider: the union, so that no object is matched on fewer keys.
	if r, _ := k.For("", "x_thing"); strings.Join(r, ",") != "id,name" {
		t.Errorf("union: %v", r)
	}
}

func TestHCLString(t *testing.T) {
	if got := HCLString("a\"b${c}%{d}\\"); got != `"a\"b$${c}%%{d}\\"` {
		t.Errorf("got %s", got)
	}
}

// Captured from terraform 1.16 with a provider whose fake_broken list fails.
const stream = `{"@level":"info","@message":"Terraform 1.16.4","type":"version","terraform":"1.16.4","ui":"1.3"}
{"@level":"warn","diagnostic":{"severity":"warning","summary":"Provider development overrides are in effect"},"type":"diagnostic"}
{"list_start":{"address":"list.fake_broken.b","resource_type":"fake_broken"},"type":"list_start"}
{"list_start":{"address":"list.fake_thing.t","resource_type":"fake_thing"},"type":"list_start"}
{"list_resource_found":{"address":"list.fake_thing.t","display_name":"managed-a","identity":{"id":"managed-a","n":12345678901234567890},"identity_version":0,"resource_type":"fake_thing"},"type":"list_resource_found"}
{"list_complete":{"address":"list.fake_thing.t","resource_type":"fake_thing","total":1},"type":"list_complete"}
{"@level":"error","diagnostic":{"severity":"error","summary":"access denied","detail":"fake_broken always fails to list","address":"list.fake_broken.b"},"type":"diagnostic"}
`

func TestParseQueryStream(t *testing.T) {
	res, fatal, err := ParseQueryStream([]byte(stream), "root", func(string, string) ([]string, []string) { return []string{"id"}, nil }, func(string) int { return 100 })
	if err != nil || len(fatal) != 0 {
		t.Fatalf("err=%v fatal=%v", err, fatal)
	}
	if len(res.Found) != 1 || res.Found[0].DisplayName != "managed-a" || res.Found[0].RequiredKeys[0] != "id" {
		t.Fatalf("found = %+v", res.Found)
	}
	if got := res.Found[0].IdentityString(); got != "id=managed-a, n=12345678901234567890" {
		t.Errorf("identity = %s (large numbers must stay exact)", got)
	}
	if len(res.Failed) != 1 || res.Failed[0].Type != "fake_broken" {
		t.Errorf("failed = %+v", res.Failed)
	}
	if res.Started != 2 || res.Complete != 1 || len(res.Unfinished) != 0 {
		t.Errorf("started=%d complete=%d unfinished=%v", res.Started, res.Complete, res.Unfinished)
	}
}

// A block that started and neither finished nor failed on its own (the
// provider crashed) is unfinished; an error on the block covers its instances.
func TestParseQueryStreamUnfinished(t *testing.T) {
	in := `{"list_start":{"address":"list.fake_thing.t","resource_type":"fake_thing"},"type":"list_start"}
{"list_start":{"address":"list.fake_broken.b[\"k\"]","resource_type":"fake_broken"},"type":"list_start"}
{"list_start":{"address":"list.fake_zoned.z","resource_type":"fake_zoned"},"type":"list_start"}
{"list_complete":{"address":"list.fake_zoned.z","resource_type":"fake_zoned","total":100},"type":"list_complete"}
{"diagnostic":{"severity":"error","summary":"access denied","address":"list.fake_broken.b"},"type":"diagnostic"}`
	res, fatal, err := ParseQueryStream([]byte(in), "root", noKeys, func(string) int { return 100 })
	if err != nil || len(fatal) != 0 {
		t.Fatalf("err=%v fatal=%v", err, fatal)
	}
	if strings.Join(res.Unfinished, ",") != "list.fake_thing.t" || res.explained() {
		t.Errorf("unfinished = %v", res.Unfinished)
	}
	// A limit warning alone does not explain a failed run.
	limitOnly := `{"list_start":{"address":"list.fake_zoned.z","resource_type":"fake_zoned"},"type":"list_start"}
{"list_complete":{"address":"list.fake_zoned.z","resource_type":"fake_zoned","total":100},"type":"list_complete"}`
	if res, _, _ := ParseQueryStream([]byte(limitOnly), "root", noKeys, func(string) int { return 100 }); len(res.Failed) != 1 || res.explained() {
		t.Errorf("limit only: failed=%v explained=%v", res.Failed, res.explained())
	}
	if res, _, _ := ParseQueryStream([]byte(stream), "root", noKeys, func(string) int { return 100 }); !res.explained() {
		t.Error("a block with its own error explains a failed run")
	}
}

func TestParseQueryStreamConfigError(t *testing.T) {
	in := `{"diagnostic":{"severity":"error","summary":"Missing config block","address":"list.fake_zoned.z"},"type":"diagnostic"}`
	_, fatal, err := ParseQueryStream([]byte(in), "root", noKeys, func(string) int { return 100 })
	if err != nil || len(fatal) != 1 {
		t.Fatalf("an error before any list started must be fatal: err=%v fatal=%v", err, fatal)
	}
}

func TestEnviron(t *testing.T) {
	env := []string{"PATH=/bin", "TF_CLI_ARGS=-no-color", "TF_CLI_ARGS_apply=-state-out=/prod.tfstate", "TF_DATA_DIR=/x", "TF_WORKSPACE=prod", "TF_CLI_CONFIG_FILE=/rc", "TF_CLI_ARGSX=keep"}
	if got := strings.Join(Environ(env, false), " "); got != "PATH=/bin TF_DATA_DIR=/x TF_WORKSPACE=prod TF_CLI_CONFIG_FILE=/rc TF_CLI_ARGSX=keep" {
		t.Errorf("user root: %s", got)
	}
	if got := strings.Join(Environ(env, true), " "); got != "PATH=/bin TF_CLI_CONFIG_FILE=/rc TF_CLI_ARGSX=keep" {
		t.Errorf("isolated root: %s", got)
	}
}

func TestParseQueryStreamLimit(t *testing.T) {
	in := `{"list_start":{"address":"list.fake_thing.t","resource_type":"fake_thing"},"type":"list_start"}
{"list_complete":{"address":"list.fake_thing.t","resource_type":"fake_thing","total":2},"type":"list_complete"}`
	limit := func(n int) func(string) int { return func(string) int { return n } }
	res, _, err := ParseQueryStream([]byte(in), "root", noKeys, limit(2))
	if err != nil || len(res.Failed) != 1 || !strings.Contains(res.Failed[0].Message, "its limit") || res.Failed[0].Type != "fake_thing" {
		t.Errorf("truncated list not reported: %+v %v", res, err)
	}
	if res, _, _ := ParseQueryStream([]byte(in), "root", noKeys, limit(3)); len(res.Failed) != 0 {
		t.Errorf("complete list reported: %+v", res.Failed)
	}
	if res, _, _ := ParseQueryStream([]byte(in), "root", noKeys, limit(0)); len(res.Failed) != 1 || !strings.Contains(res.Failed[0].Message, "not a constant") {
		t.Errorf("unknown limit not reported: %+v", res.Failed)
	}
}

// Generated blocks would use the empty default configuration of a provider
// the root configures only with aliases, so its types are skipped.
func TestPlanAliasOnly(t *testing.T) {
	p := BuildPlan(schemas(t), nil, PlanOptions{AliasOnly: map[string][]string{"aws": {"prod"}}})
	for _, q := range p.Queries {
		if q.Provider == "aws" {
			t.Errorf("aws queried: %+v", q)
		}
	}
	if len(p.Notes) != 1 || !strings.Contains(p.Notes[0], "provider = aws.<alias>") || !strings.Contains(p.Notes[0], "(aws.prod)") {
		t.Errorf("notes %v", p.Notes)
	}
	if len(p.Skipped) == 0 {
		t.Error("aws types not reported as skipped")
	}
}

func TestPlanSameTypeTwoProviders(t *testing.T) {
	var s Schemas
	json.Unmarshal([]byte(`{"provider_schemas": {
	  "registry.terraform.io/hashicorp/google": {"list_resource_schemas": {"google_compute_network": {"block": {}}}},
	  "registry.terraform.io/hashicorp/google-beta": {"list_resource_schemas": {"google_compute_network": {"block": {}}}}
	}}`), &s)
	// Both list the same objects: one query, through google.
	p := BuildPlan(&s, nil, PlanOptions{})
	if len(p.Queries) != 1 || p.Queries[0].Provider != "google" {
		t.Fatalf("queries %+v", p.Queries)
	}
	if len(p.Skipped) != 1 || !strings.Contains(p.Skipped[0].Reason, "listed through google") {
		t.Errorf("skipped %+v", p.Skipped)
	}
	if hcl := string(p.HCL(10)); !strings.Contains(hcl, `"tfo_google_google_compute_network"`) {
		t.Errorf("HCL:\n%s", hcl)
	}
	// Configured for different projects, both are queried.
	p = BuildPlan(&s, nil, PlanOptions{ProviderConfigs: map[string]string{"google": `project = "a"`, "google-beta": `project = "b"`}})
	if len(p.Queries) != 2 {
		t.Errorf("different configurations: %+v", p.Queries)
	}
	p = BuildPlan(&s, nil, PlanOptions{ProviderConfigs: map[string]string{"google": `project = var.p`, "google-beta": `project = var.p`}})
	if len(p.Queries) != 1 {
		t.Errorf("equal configurations: %+v", p.Queries)
	}
	// Only google-beta takes the project argument: both are queried.
	var s3 Schemas
	json.Unmarshal([]byte(`{"provider_schemas": {
	  "registry.terraform.io/hashicorp/google": {"list_resource_schemas": {"google_compute_network": {"block": {}}}},
	  "registry.terraform.io/hashicorp/google-beta": {"list_resource_schemas": {"google_compute_network": {"block": {"attributes": {"project": {"type": "string", "optional": true}}}}}}
	}}`), &s3)
	project, _ := ParseArg("project=p2")
	if p := BuildPlan(&s3, nil, PlanOptions{Args: []Arg{project}}); len(p.Queries) != 2 {
		t.Errorf("different arguments: %+v", p.Queries)
	}
	// Two providers of different families both list the type.
	var s2 Schemas
	json.Unmarshal([]byte(`{"provider_schemas": {
	  "registry.terraform.io/acme/foo": {"list_resource_schemas": {"foo_thing": {"block": {}}}},
	  "registry.terraform.io/other/foo": {"list_resource_schemas": {"foo_thing": {"block": {}}}}
	}}`), &s2)
	if p := BuildPlan(&s2, nil, PlanOptions{}); len(p.Queries) != 2 {
		t.Errorf("different providers: %+v", p.Queries)
	}
}

func TestTemporaryNamesUnique(t *testing.T) {
	var s Schemas
	json.Unmarshal([]byte(`{"provider_schemas": {
	  "registry.terraform.io/ns1/foo": {"list_resource_schemas": {"foo_a": {"block": {}}}},
	  "registry.terraform.io/ns2/foo": {"list_resource_schemas": {"foo_a": {"block": {}}}},
	  "example.com/x/y": {"list_resource_schemas": {"y_b": {"block": {}}}}
	}}`), &s)
	declared := map[string]string{"registry.terraform.io/other/z": "tfo-ns1-foo"}
	p := BuildPlan(&s, declared, PlanOptions{})
	got := map[string]string{}
	for n, addr := range p.Declare {
		got[addr] = n
	}
	if got["registry.terraform.io/ns1/foo"] != "tfo-ns1-foo-2" || got["registry.terraform.io/ns2/foo"] != "tfo-ns2-foo" || got["example.com/x/y"] != "tfo-x-y" {
		t.Errorf("names %v", got)
	}
	if len(p.Queries) != 3 || p.Queries[0].Address() == p.Queries[1].Address() {
		t.Errorf("queries %+v", p.Queries)
	}
}

func TestImplicitNameTaken(t *testing.T) {
	var s Schemas
	json.Unmarshal([]byte(`{"provider_schemas": {
	  "registry.terraform.io/hashicorp/google": {"list_resource_schemas": {"google_compute_network": {"block": {}}}},
	  "registry.terraform.io/hashicorp/google-beta": {"list_resource_schemas": {"google_compute_network": {"block": {}}}}
	}}`), &s)
	// The root calls google-beta "google"; hashicorp/google is only used
	// by a child module.
	p := BuildPlan(&s, map[string]string{"registry.terraform.io/hashicorp/google-beta": "google"}, PlanOptions{})
	got := map[string]string{}
	for _, q := range p.Queries {
		got[q.Provider] = q.Type
	}
	if _, ok := got["google"]; !ok || len(p.Queries) != 1 {
		t.Errorf("google-beta lost its declared name: %+v", p.Queries)
	}
	// hashicorp/google lists the same objects through a temporary name
	// only; the root's own provider is queried instead.
	if len(p.Declare) != 0 {
		t.Errorf("temporary names %v", p.Declare)
	}
}

// A wrapper given with -terraform may run terraform as its child; cancelling
// has to stop both, or the run waits for the child to close stdout.
func TestRunCancelWrapper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups")
	}
	wrapper := filepath.Join(t.TempDir(), "terraform")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nsleep 30 | cat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, _, err := (Runner{Bin: wrapper, Dir: t.TempDir()}).run(ctx, "version"); err == nil {
		t.Error("no error after cancellation")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("returned after %v", d)
	}
}

func TestPlanOptIn(t *testing.T) {
	var s Schemas
	json.Unmarshal([]byte(`{"provider_schemas": {
	  "registry.terraform.io/hashicorp/google": {"list_resource_schemas": {"google_project": {"block": {}}, "google_compute_network": {"block": {}}}}
	}}`), &s)
	types := func(p Plan) []string {
		var out []string
		for _, q := range p.Queries {
			out = append(out, q.Type)
		}
		return out
	}
	p := BuildPlan(&s, nil, PlanOptions{})
	if got := types(p); !slices.Equal(got, []string{"google_compute_network"}) || len(p.Skipped) != 1 || !strings.Contains(p.Skipped[0].Reason, "-types") {
		t.Errorf("default: %v, skipped %+v", got, p.Skipped)
	}
	if got := types(BuildPlan(&s, nil, PlanOptions{Include: glob.CompileList([]string{"google_*"})})); !slices.Equal(got, []string{"google_compute_network"}) {
		t.Errorf("a wildcard is no opt-in: %v", got)
	}
	if got := types(BuildPlan(&s, nil, PlanOptions{Include: glob.CompileList([]string{"google_project"})})); !slices.Equal(got, []string{"google_project"}) {
		t.Errorf("named: %v", got)
	}
}

func TestScopedArg(t *testing.T) {
	a, err := ParseArg("google_compute_*:zone=z1,z2")
	if err != nil || a.Types != "google_compute_*" || a.Name != "zone" || len(a.Values) != 2 {
		t.Fatalf("%+v %v", a, err)
	}
	for _, bad := range []string{":zone=a", "zone", "x:=a"} {
		if _, err := ParseArg(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
	var s Schemas
	json.Unmarshal([]byte(`{"provider_schemas": {"registry.terraform.io/hashicorp/google": {"list_resource_schemas": {
	  "google_compute_disk": {"block": {"attributes": {"zone": {"type": "string", "optional": true}, "location": {"type": "string", "optional": true}}}},
	  "google_storage_bucket": {"block": {"attributes": {"zone": {"type": "string", "optional": true}, "location": {"type": "string", "optional": true}}}}
	}}}}`), &s)
	global, _ := ParseArg("zone=all-z")
	loc, _ := ParseArg("location=eu")
	p := BuildPlan(&s, nil, PlanOptions{Args: []Arg{global, a, loc}})
	got := map[string]string{}
	for _, q := range p.Queries {
		var parts []string
		for _, arg := range q.Args {
			parts = append(parts, arg.Name+"="+strings.Join(arg.Values, ","))
		}
		got[q.Type] = strings.Join(parts, " ")
	}
	if got["google_compute_disk"] != "zone=z1,z2 location=eu" || got["google_storage_bucket"] != "zone=all-z location=eu" {
		t.Errorf("args %v", got)
	}
	// Two scoped arguments for one type: their values are combined.
	disk, _ := ParseArg("google_compute_disk:zone=z3,z1")
	p = BuildPlan(&s, nil, PlanOptions{Args: []Arg{a, disk}})
	for _, q := range p.Queries {
		if q.Type == "google_compute_disk" && (len(q.Args) != 1 || strings.Join(q.Args[0].Values, ",") != "z1,z2,z3") {
			t.Errorf("overlapping args %+v", q.Args)
		}
	}
	if a.Values[0] != "z1" || len(a.Values) != 2 {
		t.Errorf("argument changed in place: %+v", a)
	}
}

func TestClassify(t *testing.T) {
	for _, c := range []struct{ msg, cat string }{
		{"Error: API Error: googleapi: Error 403: Cloud Run Admin API has not been used in project 123 before or it is disabled. Enable it by visiting https://console.developers.google.com/apis/api/run.googleapis.com/overview?project=123 then retry.\nDetails:\n[\n  {\n    \"@type\": \"type.googleapis.com/google.rpc.ErrorInfo\",\n    \"reason\": \"SERVICE_DISABLED\"\n  }\n]", FailAPIOff},
		{"API Error: Cannot determine zone: set in this resource, or set provider-level zone.", FailMissingArg},
		{"googleapi: Error 403: Required 'compute.disks.list' permission for 'projects/p', forbidden", FailDenied},
		{"operation error EC2: DescribeInstances, StatusCode: 403, api error UnauthorizedOperation", FailDenied},
		{"returned 100 results, its limit; there may be more", FailTruncated},
		{"googleapi: Error 404: Requested entity was not found.", FailNotFound},
		{"did not finish", FailOther},
		{"crashed: panic: inconsistent list element types", FailCrash},
		{`failed to make http request: GET "https://api.cloudflare.com/client/v4/user/tokens": 403 Forbidden`, FailDenied},
		{`failed to make http request: GET "https://api.cloudflare.com/client/v4/zones/z/filters": 410 Gone { "result": null, "success": false, "errors": [ { "code": 10020, "message": "filters.api.deprecated" } ] }`, FailRetired},
		{"unexpected resource configure type: Expected *cloudflare.Client, got: *cloudflare.Client. Please report this issue to the provider developers.", FailProvider},
		{"RFC3339 ValueRFC3339Time Error: RFC3339 string value is null", FailProvider},
		{`failed to make http request: GET "https://api.cloudflare.com/client/v4/zones/z/custom_certificates": 400 Bad Request`, FailRejected},
	} {
		f := Failure{Message: c.msg}
		f.Classify()
		if f.Category != c.cat {
			t.Errorf("%.60q: category %q, want %q", c.msg, f.Category, c.cat)
		}
		if strings.Contains(f.Message, "\n") || strings.Contains(f.Message, "@type") || len(f.Message) > 303 {
			t.Errorf("message not shortened: %q", f.Message)
		}
	}
	if got := OneLine(strings.Repeat("ж", 200), 11); got != "жжжжж..." {
		t.Errorf("cut %q", got)
	}
}

func TestProviderConfigs(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "providers.tf"), []byte(`
provider "google" {
  project = var.project
  region  = "europe-west1"
}
provider "google-beta" {
  project =   var.project
  region  = "europe-west1"
}
provider "google" {
  alias   = "other"
  project = "x"
}
provider "aws" {}
`), 0o644)
	os.WriteFile(filepath.Join(dir, "x.tf.json"), []byte(`{"provider": {"azurerm": {"features": {}}}}`), 0o644)
	got, err := ProviderConfigs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got["google"] == "" || got["google"] != got["google-beta"] || got["aws"] != "" || got["azurerm"] == "" {
		t.Errorf("configs %q", got)
	}
	if _, ok := got["aws"]; !ok {
		t.Error("empty block missing")
	}

	// Spaces inside strings count; an override file makes a block unequal.
	dir = t.TempDir()
	os.WriteFile(filepath.Join(dir, "providers.tf"), []byte(`
provider "google" {
  project = trimspace(file("project  id.txt")) # two spaces
}
provider "google-beta" {
  project = trimspace(file("project id.txt"))
}
provider "aws" {
  region = "a"
}
provider "awsx" {
  region = "a"
}
`), 0o644)
	os.WriteFile(filepath.Join(dir, "x_override.tf"), []byte(`provider "awsx" { profile = "other" }`), 0o644)
	// Terraform skips hidden files and backups; so does the comparison.
	os.WriteFile(filepath.Join(dir, ".backup.tf"), []byte(`provider "hidden" { project = "b" }`), 0o644)
	os.WriteFile(filepath.Join(dir, "#auto.tf#"), []byte(`provider "backup" { project = "b" }`), 0o644)
	got, _ = ProviderConfigs(dir)
	if got["google"] == got["google-beta"] {
		t.Errorf("string contents ignored: %q", got)
	}
	if got["aws"] == got["awsx"] {
		t.Errorf("override ignored: %q", got)
	}
	if _, ok := got["hidden"]; ok {
		t.Errorf("hidden file read: %q", got)
	}
	if _, ok := got["backup"]; ok {
		t.Errorf("backup file read: %q", got)
	}
}

func TestLaterOverrides(t *testing.T) {
	dir := t.TempDir()
	// zzzz_override.json is not configuration (only .tf.json is).
	for _, n := range []string{"main.tf", "aa_override.tf", "zzz_override.tf", "zzzz_override.json", "zzzz_override.tf.json", "override.tf.json", "zz_tforphans_1_override.tf"} {
		os.WriteFile(filepath.Join(dir, n), nil, 0o644)
	}
	got := laterOverrides(dir, "zz_tforphans_9_version_override.tf")
	if !slices.Equal(got, []string{"zzz_override.tf", "zzzz_override.tf.json"}) {
		t.Errorf("got %v", got)
	}
}

func TestRemoveStale(t *testing.T) {
	dir := t.TempDir()
	// A process that cannot exist (beyond any pid_max) left files.
	gone := filepath.Join(dir, "zz_tforphans_99999999.tfquery.hcl")
	goneOverride := filepath.Join(dir, "zz_tforphans_99999999_override.tf")
	os.WriteFile(gone, nil, 0o644)
	os.WriteFile(goneOverride, nil, 0o644)
	if err := RemoveStale(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(gone); err == nil {
		t.Error("file of a dead run kept")
	}
	if _, err := os.Stat(goneOverride); err == nil {
		t.Error("override of a dead run kept")
	}
	// The parent process (the test runner) is alive: its files stay.
	busy := filepath.Join(dir, fmt.Sprintf("zz_tforphans_%d.tfquery.hcl", os.Getppid()))
	os.WriteFile(busy, nil, 0o644)
	if err := RemoveStale(dir); err == nil {
		t.Error("files of a running process removed")
	}
	if _, err := os.Stat(busy); err != nil {
		t.Error(err)
	}
}
