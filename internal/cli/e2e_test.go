//go:build e2e

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// TestE2ECloudflare scans the DNS records of a real Cloudflare zone against
// the local state in testdata/e2e/cloudflare (records imported once with a
// read-only token). Run by the e2e workflow; needs terraform,
// CLOUDFLARE_API_TOKEN and E2E_ZONE_ID.
func TestE2ECloudflare(t *testing.T) {
	zone := os.Getenv("E2E_ZONE_ID")
	if os.Getenv("CLOUDFLARE_API_TOKEN") == "" || zone == "" {
		t.Skip("CLOUDFLARE_API_TOKEN and E2E_ZONE_ID are not set")
	}
	src := filepath.Join("testdata", "e2e", "cloudflare")
	var expect struct {
		ZoneID  string   `json:"zone_id"`
		Orphans []string `json:"orphans"`
		Dropped []string `json:"dropped"` // created by Cloudflare, filtered out
	}
	readJSON(t, filepath.Join(src, "expect.json"), &expect)
	if zone != expect.ZoneID {
		t.Fatalf("E2E_ZONE_ID is %s, the fixture is for %s", zone, expect.ZoneID)
	}
	var state struct {
		Resources []struct {
			Instances []struct {
				Attributes struct {
					ID string `json:"id"`
				} `json:"attributes"`
			} `json:"instances"`
		} `json:"resources"`
	}
	readJSON(t, filepath.Join(src, "terraform.tfstate"), &state)
	var managed []string
	for _, r := range state.Resources {
		for _, i := range r.Instances {
			managed = append(managed, i.Attributes.ID)
		}
	}

	// init writes .terraform, so the root is a copy.
	root := t.TempDir()
	for _, name := range []string{"versions.tf", ".terraform.lock.hcl", "terraform.tfstate"} {
		data, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("terraform", "init", "-input=false", "-no-color", "-lockfile=readonly")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("terraform init: %v\n%s", err, out)
	}

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"-no-query", "-scan", "cloudflare", "-state-roots", root,
		"-scan-scope", "zone_id=" + zone, "-scan-scope", "account_id=",
		"-types", "cloudflare_dns_record", "-strict", "-explain", "-format", "json",
	}, &stdout, &stderr)
	var out struct {
		Orphans []struct {
			Type     string `json:"type"`
			ImportID string `json:"import_id"`
		} `json:"orphans"`
		Uncertain []json.RawMessage `json:"uncertain"`
		Explain   []struct {
			Type     string            `json:"type"`
			Identity map[string]string `json:"identity"`
			Status   string            `json:"status"`
		} `json:"explain"`
		Summary struct {
			States       int               `json:"states"`
			Complete     bool              `json:"complete"`
			UnreadStates []json.RawMessage `json:"unread_states"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("exit %d: %v\n%s\n%s", code, err, stdout.String(), stderr.String())
	}
	defer func() {
		if t.Failed() {
			t.Logf("stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
		}
	}()

	if code != exitOrphans {
		t.Errorf("exit %d, want %d", code, exitOrphans)
	}
	if out.Summary.States != 1 || len(out.Summary.UnreadStates) != 0 || !out.Summary.Complete || len(out.Uncertain) != 0 {
		t.Errorf("states %d, unread %d, complete %v, uncertain %d", out.Summary.States, len(out.Summary.UnreadStates), out.Summary.Complete, len(out.Uncertain))
	}
	var orphans []string
	for _, o := range out.Orphans {
		if o.Type != "cloudflare_dns_record" {
			t.Errorf("orphan of type %s", o.Type)
		}
		orphans = append(orphans, o.ImportID)
	}
	slices.Sort(orphans)
	if !slices.Equal(orphans, expect.Orphans) {
		t.Errorf("orphans %q, want %q", orphans, expect.Orphans)
	}
	status := map[string]string{}
	for _, e := range out.Explain {
		status[e.Identity["id"]] = e.Status
	}
	for _, id := range managed {
		if status[id] != "managed" {
			t.Errorf("record %s in the state: %q, want managed", id, status[id])
		}
	}
	for _, id := range expect.Dropped {
		// Still in the zone and still Cloudflare's, or their absence
		// would not show the filter works.
		if !cloudflareOwned(t, zone, id) {
			t.Errorf("record %s is no longer one Cloudflare creates: update expect.json", id)
		}
		if s, ok := status[id]; ok {
			t.Errorf("record %s created by Cloudflare is reported: %s", id, s)
		}
	}
	if want := len(managed) + len(expect.Orphans); len(out.Explain) != want {
		t.Errorf("%d records reported, want %d", len(out.Explain), want)
	}
}

// cloudflareOwned reads a DNS record from the API and reports whether
// Cloudflare created it (the marks the adapter filters on).
func cloudflareOwned(t *testing.T, zone, id string) bool {
	t.Helper()
	req, err := http.NewRequest("GET", "https://api.cloudflare.com/client/v4/zones/"+zone+"/dns_records/"+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+os.Getenv("CLOUDFLARE_API_TOKEN"))
	client := http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Success bool `json:"success"`
		Result  struct {
			Meta struct {
				ReadOnly       bool   `json:"read_only"`
				EmailRouting   bool   `json:"email_routing"`
				OriginWorkerID string `json:"origin_worker_id"`
			} `json:"meta"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || !body.Success {
		t.Logf("record %s: HTTP %d, %v", id, resp.StatusCode, err)
		return false
	}
	m := body.Result.Meta
	return m.ReadOnly || m.EmailRouting || m.OriginWorkerID != ""
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
