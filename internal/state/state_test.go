package state

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/5xxeu/tforphans/internal/model"
)

const fixture = `{
  "version": 4,
  "resources": [
    {"mode": "managed", "type": "aws_instance", "name": "web", "instances": [
      {"identity": {"account_id": "111", "id": "i-1", "region": "eu-north-1"}, "attributes": {"id": "i-1"}}
    ]},
    {"module": "module.dns", "mode": "managed", "type": "cloudflare_dns_record", "name": "r", "instances": [
      {"index_key": "www", "attributes": {"id": "rec1", "zone_id": "z1", "ttl": 300}}
    ]},
    {"mode": "data", "type": "aws_instance", "name": "lookup", "instances": [
      {"attributes": {"id": "i-data"}}
    ]}
  ]
}`

func index(t *testing.T) *Index {
	t.Helper()
	insts, err := Parse([]byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(insts) != 2 {
		t.Fatalf("got %d instances, want 2 (data sources skipped)", len(insts))
	}
	ix := NewIndex()
	ix.Add(insts)
	return ix
}

func TestManaged(t *testing.T) {
	ix := index(t)
	cases := []struct {
		name string
		r    model.Resource
		want string
	}{
		{"identity match", model.Resource{Type: "aws_instance", RequiredKeys: []string{"id"},
			Identity: map[string]any{"account_id": "111", "id": "i-1", "region": "eu-north-1"}}, "aws_instance.web"},
		{"other region", model.Resource{Type: "aws_instance", RequiredKeys: []string{"id"},
			Identity: map[string]any{"account_id": "111", "id": "i-1", "region": "us-east-1"}}, ""},
		{"unknown id", model.Resource{Type: "aws_instance", Identity: map[string]any{"id": "i-2"}}, ""},
		{"data source does not manage", model.Resource{Type: "aws_instance", Identity: map[string]any{"id": "i-data"}}, ""},
		{"attribute fallback", model.Resource{Type: "cloudflare_dns_record", Identity: map[string]any{"id": "rec1"}},
			`module.dns.cloudflare_dns_record.r["www"]`},
		{"attribute fallback ignores keys that are not attributes", model.Resource{Type: "cloudflare_dns_record",
			RequiredKeys: []string{"id", "subscription_id"}, AttributeKeys: []string{"id"},
			Identity: map[string]any{"id": "rec1", "subscription_id": "s"}}, `module.dns.cloudflare_dns_record.r["www"]`},
		{"attribute fallback needs every attribute key", model.Resource{Type: "cloudflare_dns_record",
			RequiredKeys: []string{"id", "account"}, Identity: map[string]any{"id": "rec1", "account": "a"}}, ""},
		{"attribute conflict", model.Resource{Type: "cloudflare_dns_record", Identity: map[string]any{"id": "rec1", "zone_id": "z2"}}, ""},
		{"wrong type", model.Resource{Type: "cloudflare_record", Identity: map[string]any{"id": "rec1"}}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ix.Managed(c.r)
			if got != c.want || ok != (c.want != "") {
				t.Errorf("Managed() = %q, %v; want %q", got, ok, c.want)
			}
		})
	}
}

func TestManagedRequiresIdentityKeys(t *testing.T) {
	ix := NewIndex()
	ix.Add([]Instance{{Type: "t", Address: "t.a", Identity: map[string]any{"project": "p"}}})
	r := model.Resource{Type: "t", RequiredKeys: []string{"name"}, Identity: map[string]any{"name": "n", "project": "p"}}
	if _, ok := ix.Managed(r); ok {
		t.Error("matched although the required key is missing from the state identity")
	}
}

func TestParse(t *testing.T) {
	if insts, err := Parse([]byte("  ")); err != nil || insts != nil {
		t.Errorf("empty state: %v, %v", insts, err)
	}
	if _, err := Parse([]byte(`{"format_version":"1.0","values":{}}`)); err == nil {
		t.Error("show -json output accepted")
	}
}

func TestManagedAltTypes(t *testing.T) {
	ix := NewIndex()
	ix.Add([]Instance{{Type: "cloudflare_record", Address: "cloudflare_record.www", Attributes: map[string]any{"id": "r1", "zone_id": "z"}}})
	r := model.Resource{Type: "cloudflare_dns_record", AltTypes: []string{"cloudflare_record"}, Identity: map[string]any{"id": "r1", "zone_id": "z"}}
	if got, ok := ix.Managed(r); !ok || got != "cloudflare_record.www" {
		t.Errorf("Managed() = %q, %v", got, ok)
	}
	r.AltTypes = nil
	if _, ok := ix.Managed(r); ok {
		t.Error("matched another type without AltTypes")
	}
}

func TestManagedAltIdentity(t *testing.T) {
	ix := NewIndex()
	ix.Add([]Instance{{Type: "cloudflare_worker", Address: "cloudflare_worker.w", Attributes: map[string]any{"id": "uuid-1", "name": "api"}}})
	r := model.Resource{
		Type: "cloudflare_workers_script", Identity: map[string]any{"id": "api"},
		AltTypes:    []string{"cloudflare_worker"},
		AltIdentity: map[string]map[string]any{"cloudflare_worker": {"id": "uuid-1"}},
	}
	if got, ok := ix.Managed(r); !ok || got != "cloudflare_worker.w" {
		t.Errorf("Managed() = %q, %v", got, ok)
	}
}

func TestFetchRefusesCredentialsOverHTTP(t *testing.T) {
	t.Setenv("TF_HTTP_USERNAME", "u")
	t.Setenv("TF_HTTP_PASSWORD", "p")
	_, err := Fetch(context.Background(), "http://127.0.0.1:1/state", true)
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Errorf("err = %v", err)
	}
}

func TestFallbackNeedsPrimaryKey(t *testing.T) {
	ix := NewIndex()
	// State of a framework resource without an id attribute.
	ix.Add([]Instance{{Type: "cloudflare_thing", Address: "cloudflare_thing.a", Attributes: map[string]any{"name": "a", "zone_id": "z1"}}})
	orphan := model.Resource{Type: "cloudflare_thing", Identity: map[string]any{"id": "other", "zone_id": "z1"}, RequiredKeys: []string{"id"}}
	if _, ok := ix.Managed(orphan); ok {
		t.Error("matched on the zone alone")
	}
	orphan.RequiredKeys = nil // plugins: "id" is the primary key when present
	if _, ok := ix.Managed(orphan); ok {
		t.Error("matched on the zone alone without RequiredKeys")
	}
	ix.Add([]Instance{{Type: "cloudflare_thing", Address: "cloudflare_thing.b", Attributes: map[string]any{"id": "b", "zone_id": "z1"}}})
	if addr, ok := ix.Managed(model.Resource{Type: "cloudflare_thing", Identity: map[string]any{"id": "b", "zone_id": "z1"}, RequiredKeys: []string{"id"}}); !ok || addr != "cloudflare_thing.b" {
		t.Errorf("Managed() = %q, %v", addr, ok)
	}
}

func TestFetchRedirects(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("credentials sent over http")
		}
		fmt.Fprint(w, `{"version":4}`)
	}))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/downgrade":
			http.Redirect(w, r, plain.URL+"/state", http.StatusFound)
		case "/same":
			http.Redirect(w, r, "/state", http.StatusFound)
		default:
			fmt.Fprint(w, `{"version":4}`)
		}
	}))
	defer secure.Close()
	old := transport
	transport = secure.Client().Transport
	defer func() { transport = old }()

	t.Setenv("TF_HTTP_USERNAME", "u")
	t.Setenv("TF_HTTP_PASSWORD", "p")
	if _, err := Fetch(context.Background(), secure.URL+"/downgrade", true); err == nil || !strings.Contains(err.Error(), "refusing to follow") {
		t.Errorf("downgrade redirect: err = %v", err)
	}
	if data, err := Fetch(context.Background(), secure.URL+"/same", false); err != nil || !strings.Contains(string(data), "version") {
		t.Errorf("same-host redirect: %v", err)
	}
	// Another https host (a presigned URL) gets no credentials.
	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("credentials sent to another host")
		}
		if ref := r.Header.Get("Referer"); ref != "" {
			t.Errorf("Referer sent to another host: %s", ref)
		}
		fmt.Fprint(w, `{"version":4}`)
	}))
	defer storage.Close()
	other := strings.Replace(storage.URL, "127.0.0.1", "localhost", 1)
	presign := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other+"/blob", http.StatusFound)
	}))
	defer presign.Close()
	transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	if _, err := Fetch(context.Background(), presign.URL+"/state?X-Amz-Signature=SECRET", false); err != nil {
		t.Errorf("redirect to another https host: %v", err)
	}
	transport = secure.Client().Transport
	t.Setenv("TF_HTTP_USERNAME", "")
	// A downgrade is refused without credentials too: the state could be
	// altered in transit.
	if _, err := Fetch(context.Background(), secure.URL+"/downgrade", true); err == nil || !strings.Contains(err.Error(), "refusing to follow") {
		t.Errorf("downgrade without credentials: %v", err)
	}
	if _, err := Fetch(context.Background(), plain.URL+"/state", false); err == nil || !strings.Contains(err.Error(), "-allow-http") {
		t.Errorf("plain http without -allow-http: %v", err)
	}
	if _, err := Fetch(context.Background(), plain.URL+"/state", true); err != nil {
		t.Errorf("plain http with -allow-http: %v", err)
	}
}

func TestFallbackNeedsAllPrimaryKeys(t *testing.T) {
	ix := NewIndex()
	ix.Add([]Instance{{Type: "acme_thing", Address: "acme_thing.old", Attributes: map[string]any{"project": "p", "id": "old"}}})
	// A plugin identity without "id": all its keys are primary.
	r := model.Resource{Type: "acme_thing", Identity: map[string]any{"project": "p", "resource_id": "new"}}
	if _, ok := ix.Managed(r); ok {
		t.Error("matched on project alone")
	}
	ix.Add([]Instance{{Type: "acme_thing", Address: "acme_thing.new", Attributes: map[string]any{"project": "p", "resource_id": "new"}}})
	if addr, ok := ix.Managed(r); !ok || addr != "acme_thing.new" {
		t.Errorf("Managed() = %q, %v", addr, ok)
	}
}

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		"https://user:pass@gitlab.example.com/api/v4/state?token=abc":  "https://gitlab.example.com/api/v4/state?REDACTED",
		"https://bucket.s3.amazonaws.com/key?X-Amz-Signature=deadbeef": "https://bucket.s3.amazonaws.com/key?REDACTED",
		"https://user:SECRET@/state":                                   "https:///state",
		"state/prod.tfstate":                                           "state/prod.tfstate",
		"-":                                                            "-",
	} {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
	// Schemes are case-insensitive, and unknown ones are not read as files
	// (the file error would quote the whole string).
	for _, src := range []string{
		"https://user:pass@127.0.0.1:1/state?sig=SECRET",
		"HTTPS://user:pass@127.0.0.1:1/state?sig=SECRET",
		"Https://user:pass@127.0.0.1:1/state?sig=SECRET",
		"s3://user:pass@bucket/state?sig=SECRET",
	} {
		_, err := Fetch(context.Background(), src, false)
		if err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "pass") {
			t.Errorf("%s: error leaks credentials: %v", src, err)
		}
	}
	t.Setenv("TF_HTTP_USERNAME", "")
	if _, err := Fetch(context.Background(), "HTTP://127.0.0.1:1/state", false); err == nil || !strings.Contains(err.Error(), "plain http") {
		t.Errorf("upper-case http not refused: %v", err)
	}
}

func TestRootAddresses(t *testing.T) {
	insts, _ := Parse([]byte(fixture))
	ix := NewIndex()
	ix.Add(insts)
	got := ix.RootAddresses()
	if !got["aws_instance.web"] || len(got) != 1 {
		t.Errorf("got %v (module and data addresses must not count)", got)
	}
}

func TestMatchVerified(t *testing.T) {
	ix := NewIndex()
	// Old AWS state: no identity, and account_id is not an attribute.
	ix.Add([]Instance{{Type: "aws_iam_role", Address: "aws_iam_role.ci", Attributes: map[string]any{"id": "ci", "name": "ci"}}})
	r := model.Resource{Type: "aws_iam_role", Identity: map[string]any{"account_id": "222", "name": "ci"},
		RequiredKeys: []string{"name"}, AttributeKeys: []string{"name"}}
	if m := ix.Match(r); !m.Managed || m.Verified {
		t.Errorf("cross-account candidate: %+v, want managed but unverified", m)
	}
	// State with identity is checked strictly: another account is no match.
	ix.Add([]Instance{{Type: "aws_iam_role", Address: "aws_iam_role.deploy", Identity: map[string]any{"account_id": "111", "name": "deploy"}}})
	r.Identity = map[string]any{"account_id": "222", "name": "deploy"}
	if m := ix.Match(r); m.Managed {
		t.Errorf("identity of another account matched: %+v", m)
	}
	r.Identity = map[string]any{"account_id": "111", "name": "deploy"}
	if m := ix.Match(r); !m.Managed || !m.Verified {
		t.Errorf("own account: %+v", m)
	}
}

func TestFetchURLCredentials(t *testing.T) {
	t.Setenv("TF_HTTP_USERNAME", "")
	_, err := Fetch(context.Background(), "http://user:SECRET@127.0.0.1:1/state", true)
	if err == nil || !strings.Contains(err.Error(), "refusing") || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("user info over http: %v", err)
	}
	_, err = Fetch(context.Background(), "https://user:SECRET@host:bad/state", false)
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("invalid URL leaks: %v", err)
	}
	// User info over https is sent as Basic auth and kept off other hosts.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "user" || p != "pw" {
			t.Errorf("basic auth %q %q %v", u, p, ok)
		}
		fmt.Fprint(w, `{"version":4}`)
	}))
	defer srv.Close()
	old := transport
	transport = srv.Client().Transport
	defer func() { transport = old }()
	if _, err := Fetch(context.Background(), strings.Replace(srv.URL, "https://", "https://user:pw@", 1)+"/state", false); err != nil {
		t.Errorf("https with user info: %v", err)
	}
}

func TestMatchIncompleteStateIdentity(t *testing.T) {
	ix := NewIndex()
	ix.Add([]Instance{{Type: "aws_iam_role", Address: "aws_iam_role.ci", Identity: map[string]any{"name": "ci"}}})
	r := model.Resource{Type: "aws_iam_role", Identity: map[string]any{"name": "ci", "account_id": "222"}, RequiredKeys: []string{"name"}}
	if m := ix.Match(r); !m.Managed || m.Verified {
		t.Errorf("state identity without account_id: %+v, want unverified", m)
	}
}

func TestRedactURLs(t *testing.T) {
	got := redactURLs(`parse "https://x/y?sig=SECRET": bad, see https://u:p@h/z`)
	if strings.Contains(got, "SECRET") || strings.Contains(got, "u:p") {
		t.Errorf("got %s", got)
	}
}

func TestMatchListIdentity(t *testing.T) {
	ix := NewIndex()
	ix.Add([]Instance{{Type: "x_thing", Address: "x_thing.a", Identity: map[string]any{"account_id": "a", "ids": []any{"y"}}}})
	r := model.Resource{Type: "x_thing", Identity: map[string]any{"account_id": "a", "ids": []any{"x"}}}
	if m := ix.Match(r); m.Managed {
		t.Errorf("list values ignored: %+v", m)
	}
	r.Identity["ids"] = []any{"y"}
	if m := ix.Match(r); !m.Managed || !m.Verified {
		t.Errorf("equal lists: %+v", m)
	}
}

func TestMatchScope(t *testing.T) {
	ix := NewIndex()
	ix.Add([]Instance{
		// Cluster A: VM 100 on a node named pve1, like many clusters' nodes.
		{Type: "proxmox_virtual_environment_vm", Address: "vm.a", Attributes: map[string]any{"vm_id": json.Number("100"), "node_name": "pve1"}},
		{Type: "proxmox_virtual_environment_pool", Address: "pool.p", Attributes: map[string]any{"pool_id": "p"}},
	})
	// Cluster B is scanned; it has a pve1 too.
	nodes := &model.Scope{Attrs: map[string]string{"proxmox_virtual_environment_vm": "node_name"}, Values: map[string]bool{"pve1": true, "pve2": true}}
	vm := model.Resource{Type: "proxmox_virtual_environment_vm", Identity: map[string]any{"vm_id": json.Number("100")}, Scope: nodes}
	if m := ix.Match(vm); !m.Managed || m.Verified {
		t.Errorf("equal node names prove nothing: %+v, want unverified", m)
	}
	nodes.Values = map[string]bool{"b1": true}
	if m := ix.Match(vm); m.Managed {
		t.Errorf("node the cluster does not have: %+v, want no match", m)
	}
	nodes.Values, nodes.Trusted = map[string]bool{"pve1": true}, true
	if m := ix.Match(vm); !m.Managed || !m.Verified {
		t.Errorf("states stated to be this cluster's: %+v", m)
	}
	pool := model.Resource{Type: "proxmox_virtual_environment_pool", Identity: map[string]any{"pool_id": "p"}, Scope: &model.Scope{Unverifiable: true}}
	if m := ix.Match(pool); !m.Managed || m.Verified {
		t.Errorf("cluster-wide object: %+v, want unverified", m)
	}
	pool.Scope.Trusted = true
	if m := ix.Match(pool); !m.Verified {
		t.Errorf("trusted cluster-wide object: %+v", m)
	}
}

func TestMatchListOnlyIdentity(t *testing.T) {
	ix := NewIndex()
	ix.Add([]Instance{{Type: "x_thing", Address: "x_thing.a", Identity: map[string]any{"ids": []any{"x"}}}})
	if m := ix.Match(model.Resource{Type: "x_thing", Identity: map[string]any{"ids": []any{"x"}}}); !m.Managed || !m.Verified {
		t.Errorf("list-only identity: %+v", m)
	}
}

func TestMatchLongID(t *testing.T) {
	long := "/subscriptions/0000/resourceGroups/rg/providers/Microsoft.Network/" + strings.Repeat("x", 257)
	ix := NewIndex()
	ix.Add([]Instance{{Type: "azurerm_thing", Address: "azurerm_thing.a", Attributes: map[string]any{"id": long}}})
	if m := ix.Match(model.Resource{Type: "azurerm_thing", Identity: map[string]any{"id": long}}); !m.Managed {
		t.Error("long ID not indexed")
	}
}

// TestMatchScales: AWS identities share account_id across a whole account;
// matching must not compare every object with every other one.
func TestMatchScales(t *testing.T) {
	const n = 50000
	insts := make([]Instance, n)
	for i := range insts {
		id := fmt.Sprintf("i-%07d", i)
		insts[i] = Instance{Type: "aws_instance", Address: "aws_instance.x[" + id + "]",
			Identity: map[string]any{"account_id": "111", "id": id, "region": "eu-north-1"}}
	}
	ix := NewIndex()
	ix.Add(insts)
	start := time.Now()
	managed := 0
	for i := 0; i < n; i += 2 { // half managed, half orphans
		id := fmt.Sprintf("i-%07d", i+n/2*(i%4/2))
		r := model.Resource{Type: "aws_instance", RequiredKeys: []string{"id"},
			Identity: map[string]any{"account_id": "111", "id": id, "region": "eu-north-1"}}
		if ix.Match(r).Managed {
			managed++
		}
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("%d lookups took %s", n/2, d)
	}
	if managed == 0 {
		t.Error("nothing matched")
	}
}

func TestMatchProvider(t *testing.T) {
	insts, err := Parse([]byte(`{"version": 4, "resources": [
  {"mode": "managed", "type": "foo_thing", "name": "a", "provider": "provider[\"registry.terraform.io/acme/foo\"]", "instances": [
    {"identity": {"id": "42"}, "attributes": {"id": "42"}}
  ]},
  {"module": "module.m", "mode": "managed", "type": "google_compute_network", "name": "n", "provider": "module.m.provider[\"registry.terraform.io/hashicorp/google-beta\"].west", "instances": [
    {"attributes": {"id": "net"}}
  ]},
  {"mode": "managed", "type": "proxmox_vm_qemu", "name": "vm", "provider": "provider[\"registry.terraform.io/telmate/proxmox\"]", "instances": [
    {"attributes": {"vmid": 100}}
  ]},
  {"mode": "managed", "type": "aws_instance", "name": "old", "provider": "provider.aws", "instances": [
    {"attributes": {"id": "i-1"}}
  ]}
]}`))
	if err != nil {
		t.Fatal(err)
	}
	if insts[0].Provider != "registry.terraform.io/acme/foo" || insts[1].Provider != "registry.terraform.io/hashicorp/google-beta" || insts[3].Provider != "" {
		t.Fatalf("providers: %+v", insts)
	}
	ix := NewIndex()
	ix.Add(insts)
	for _, c := range []struct {
		r    model.Resource
		want bool
	}{
		{model.Resource{Type: "foo_thing", ProviderSource: "registry.terraform.io/acme/foo", Identity: map[string]any{"id": "42"}}, true},
		{model.Resource{Type: "foo_thing", ProviderSource: "registry.opentofu.org/acme/foo", Identity: map[string]any{"id": "42"}}, true},
		// Another provider's type of the same name is another object.
		{model.Resource{Type: "foo_thing", ProviderSource: "registry.terraform.io/other/foo", Identity: map[string]any{"id": "42"}}, false},
		{model.Resource{Type: "foo_thing", Identity: map[string]any{"id": "42"}}, true},
		{model.Resource{Type: "google_compute_network", ProviderSource: "registry.terraform.io/hashicorp/google", Identity: map[string]any{"id": "net"}}, true},
		// AltTypes may belong to another provider.
		{model.Resource{Type: "proxmox_virtual_environment_vm", ProviderSource: "registry.terraform.io/bpg/proxmox", AltTypes: []string{"proxmox_vm_qemu"},
			AltIdentity: map[string]map[string]any{"proxmox_vm_qemu": {"vmid": json.Number("100")}}, Identity: map[string]any{"vm_id": "100"}}, true},
		// Legacy state does not name the provider source.
		{model.Resource{Type: "aws_instance", ProviderSource: "registry.terraform.io/hashicorp/aws", Identity: map[string]any{"id": "i-1"}}, true},
	} {
		if _, got := ix.Managed(c.r); got != c.want {
			t.Errorf("%s from %q: managed %v, want %v", c.r.Type, c.r.ProviderSource, got, c.want)
		}
	}
}

// TestMatchEmptyValue: google lists some objects with an empty project; the
// value is unknown, so the object matches by its other keys, unverified.
func TestMatchEmptyValue(t *testing.T) {
	insts, err := Parse([]byte(`{"version": 4, "resources": [
  {"mode": "managed", "type": "google_compute_disk", "name": "d", "instances": [
    {"attributes": {"id": "projects/p1/zones/z1/disks/data", "name": "data", "zone": "z1", "project": "p1"}}
  ]},
  {"mode": "managed", "type": "google_compute_instance", "name": "vm", "instances": [
    {"identity": {"name": "vm", "zone": "z1", "project": "p1"}, "attributes": {"name": "vm"}}
  ]}
]}`))
	if err != nil {
		t.Fatal(err)
	}
	ix := NewIndex()
	ix.Add(insts)
	keys := []string{"name", "project", "zone"}
	for _, c := range []struct {
		r             model.Resource
		managed, veri bool
	}{
		{model.Resource{Type: "google_compute_disk", RequiredKeys: keys, AttributeKeys: keys,
			Identity: map[string]any{"name": "data", "zone": "z1", "project": ""}}, true, false},
		{model.Resource{Type: "google_compute_disk", RequiredKeys: keys, AttributeKeys: keys,
			Identity: map[string]any{"name": "data", "zone": "z1", "project": "p1"}}, true, true},
		{model.Resource{Type: "google_compute_disk", RequiredKeys: keys, AttributeKeys: keys,
			Identity: map[string]any{"name": "data", "zone": "z2", "project": ""}}, false, false},
		{model.Resource{Type: "google_compute_instance", RequiredKeys: keys,
			Identity: map[string]any{"name": "vm", "zone": "z1", "project": ""}}, true, false},
		// An empty own ID is no evidence: project and zone alone do not match.
		{model.Resource{Type: "google_compute_disk", RequiredKeys: keys, AttributeKeys: keys,
			Identity: map[string]any{"name": "", "zone": "z1", "project": "p1"}}, false, false},
		{model.Resource{Type: "google_compute_instance", RequiredKeys: keys,
			Identity: map[string]any{"name": "", "zone": "z1", "project": "p1"}}, false, false},
		// Nothing known but the empty value: no match.
		{model.Resource{Type: "google_compute_disk", RequiredKeys: []string{"project"}, Identity: map[string]any{"project": ""}}, false, false},
	} {
		m := ix.Match(c.r)
		if m.Managed != c.managed || m.Verified != c.veri {
			t.Errorf("%s %v: got %+v", c.r.Type, c.r.Identity, m)
		}
	}
}

func TestMatchBootDisk(t *testing.T) {
	insts, err := Parse([]byte(`{"version": 4, "resources": [
  {"mode": "managed", "type": "google_compute_instance", "name": "vm", "provider": "provider[\"registry.terraform.io/hashicorp/google\"]", "instances": [
    {"attributes": {"name": "vm", "zone": "z1", "project": "p1",
      "boot_disk": [{"source": "https://www.googleapis.com/compute/v1/projects/p1/zones/z1/disks/vm", "initialize_params": [{"image": "debian-12"}]}],
      "attached_disk": [{"source": "https://www.googleapis.com/compute/v1/projects/p1/zones/z1/disks/data"}]}}
  ]},
  {"mode": "managed", "type": "google_compute_instance", "name": "byo", "instances": [
    {"attributes": {"name": "byo", "zone": "z1", "project": "p1",
      "boot_disk": [{"source": "https://www.googleapis.com/compute/v1/projects/p1/zones/z1/disks/handmade", "initialize_params": []}]}}
  ]}
]}`))
	if err != nil {
		t.Fatal(err)
	}
	ix := NewIndex()
	ix.Add(insts)
	disk := func(name, zone, project string) model.Resource {
		return model.Resource{Type: "google_compute_disk", Identity: map[string]any{"name": name, "zone": zone, "project": project}}
	}
	for _, c := range []struct {
		r             model.Resource
		managed, veri bool
	}{
		{disk("vm", "z1", "p1"), true, true},
		{disk("vm", "z1", ""), true, false},
		{disk("vm", "z1", "p2"), false, false},
		{disk("vm", "z2", "p1"), false, false},
		// An attached disk may be someone else's.
		{disk("data", "z1", "p1"), false, false},
		// So may a boot disk the instance did not create.
		{disk("handmade", "z1", "p1"), false, false},
	} {
		m := ix.Match(c.r)
		if m.Managed != c.managed || m.Verified != c.veri || c.managed && m.Address != "google_compute_instance.vm.boot_disk" {
			t.Errorf("%v: got %+v", c.r.Identity, m)
		}
	}
	// A plugin result marked unverified stays unverified; another
	// provider's disk is not owned.
	r := disk("vm", "z1", "p1")
	r.Scope = &model.Scope{Unverifiable: true}
	if m := ix.Match(r); !m.Managed || m.Verified {
		t.Errorf("unverifiable scope: %+v", m)
	}
	r = disk("vm", "z1", "p1")
	r.ProviderSource = "registry.terraform.io/acme/google"
	if m := ix.Match(r); m.Managed {
		t.Errorf("other provider: %+v", m)
	}
	// Two states own the same disk under different providers: the later
	// one does not hide the earlier.
	other, _ := Parse([]byte(`{"version": 4, "resources": [
  {"mode": "managed", "type": "google_compute_instance", "name": "copy", "provider": "provider[\"registry.terraform.io/acme/google\"]", "instances": [
    {"attributes": {"boot_disk": [{"source": "projects/p1/zones/z1/disks/vm", "initialize_params": [{}]}]}}
  ]}
]}`))
	ix.Add(other)
	r = disk("vm", "z1", "p1")
	r.ProviderSource = "registry.terraform.io/hashicorp/google"
	if m := ix.Match(r); !m.Managed || !m.Verified || m.Address != "google_compute_instance.vm.boot_disk" {
		t.Errorf("two owners: %+v", m)
	}
	// A state instance of the disk without its project matches only
	// unverified; the owner, which names the project, still verifies it.
	partial, _ := Parse([]byte(`{"version": 4, "resources": [
  {"mode": "managed", "type": "google_compute_disk", "name": "partial", "provider": "provider[\"registry.terraform.io/hashicorp/google\"]", "instances": [
    {"identity": {"name": "vm", "zone": "z1"}, "attributes": {"name": "vm", "zone": "z1"}}
  ]}
]}`))
	ix.Add(partial)
	r.RequiredKeys = []string{"name", "zone"}
	if m := ix.Match(r); !m.Managed || !m.Verified || m.Address != "google_compute_instance.vm.boot_disk" {
		t.Errorf("owner after an unverified match: %+v", m)
	}
}
