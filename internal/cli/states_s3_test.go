package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/s3fake"
)

// An s3:// prefix reads every .tfstate under it: an object that cannot be
// read is an unread state and the others still count; a prefix with no
// state stops the run.
func TestStateS3Prefix(t *testing.T) {
	t.Chdir(t.TempDir())
	managed := `{"version":4,"serial":1,"lineage":"a","resources":[{"mode":"managed","type":"vault_policy","name":"ci","provider":"provider[\"registry.terraform.io/hashicorp/vault\"]","instances":[{"schema_version":0,"attributes":{"id":"ci","name":"ci"}}]}]}`
	fake := &s3fake.Server{Bucket: "states", Objects: map[string]string{
		"prod/vault.tfstate":     managed,
		"prod/denied.tfstate":    `{}`,
		"prod/vault.tfstate.tmp": `{}`,
	}, Denied: map[string]bool{"prod/denied.tfstate": true}}
	fake.Start(t)
	plugin := `echo '{"type": "vault_policy", "identity": {"id": "ci"}, "import_id": "ci"}'`
	var stdout, stderr bytes.Buffer
	run(context.Background(), []string{"-no-config", "-no-query", "-format", "json", "-state", "s3://states/prod/", "-plugin", plugin}, &stdout, &stderr)
	var out struct {
		Summary struct {
			States       int `json:"states"`
			Managed      int `json:"managed"`
			UnreadStates []struct {
				Root, Error string
			} `json:"unread_states"`
			Notes []string `json:"notes"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("%v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	s := out.Summary
	if s.States != 1 || s.Managed != 1 || len(s.UnreadStates) != 1 || s.UnreadStates[0].Root != "s3://states/prod/denied.tfstate" || !strings.Contains(s.UnreadStates[0].Error, "AccessDenied") {
		t.Errorf("summary: %+v\nstderr:\n%s", s, stderr.String())
	}
	if !strings.Contains(strings.Join(s.Notes, "\n"), "s3://states/prod/: 1 objects without the .tfstate suffix were not read") {
		t.Errorf("notes: %q", s.Notes)
	}

	stderr.Reset()
	code := run(context.Background(), []string{"-no-config", "-no-query", "-state", "s3://states/dev/", "-plugin", plugin}, &stdout, &stderr)
	if code != exitError || !strings.Contains(stderr.String(), "no object ending in .tfstate under s3://states/dev/") {
		t.Errorf("empty prefix: exit %d\n%s", code, stderr.String())
	}
}

// doctor reads the states under an s3:// prefix one by one, and a prefix
// it cannot list is a failed source.
func TestDoctorStateS3Prefix(t *testing.T) {
	t.Chdir(t.TempDir())
	fake := &s3fake.Server{Bucket: "states", Objects: map[string]string{
		"prod/a.tfstate":      `{"version":4,"serial":1,"lineage":"a","resources":[]}`,
		"prod/denied.tfstate": `{}`,
	}, Denied: map[string]bool{"prod/denied.tfstate": true}}
	fake.Start(t)
	var stdout, stderr bytes.Buffer
	run(context.Background(), []string{"doctor", "-no-config", "-no-query", "-format", "json", "-state", "s3://states/prod/", "-state", "s3://missing/", "-plugin", "echo done"}, &stdout, &stderr)
	var out struct {
		Checks []struct {
			ID, Status, Message, Root string
			Read, Total               *int
		} `json:"checks"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("%v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	var failed []string
	summary := ""
	for _, c := range out.Checks {
		if c.ID != "states.read" {
			continue
		}
		if c.Root != "" {
			failed = append(failed, c.Root)
		} else {
			summary = c.Message
		}
	}
	if strings.Join(failed, " ") != "s3://states/prod/denied.tfstate s3://missing/" || summary != "1 of 3 states read" {
		t.Errorf("failed %q, summary %q\n%s", failed, summary, stdout.String())
	}
}
