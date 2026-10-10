package plugin

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRun(t *testing.T) {
	res, warns, err := Run(context.Background(), `printf '%s\n' '{"type":"vault_policy","identity":{"id":"ci"},"display_name":"ci"}' '' '{"error":"sys/auth: permission denied"}'`, "plugin1")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Type != "vault_policy" || res[0].RequiredKeys != nil || res[0].Source == "" {
		t.Errorf("res = %+v", res)
	}
	if len(warns) != 1 {
		t.Errorf("warns = %v", warns)
	}
}

func TestRunErrors(t *testing.T) {
	for _, cmd := range []string{
		`echo '{"type":"x"}'`, // no identity
		`echo 'not json'`,
		`exit 3`,
	} {
		if _, _, err := Run(context.Background(), cmd, "plugin1"); err == nil {
			t.Errorf("%s: no error", cmd)
		}
	}
}

func TestRunDoesNotPrintTheCommand(t *testing.T) {
	res, _, err := Run(context.Background(), `AUTH='Bearer SECRET' printf '%s\n' '{"type":"t","identity":{"id":"x"}}'`, "plugin1")
	if err != nil || res[0].Source != "plugin1" {
		t.Errorf("source %+v %v", res, err)
	}
	if _, _, err := Run(context.Background(), "AUTH='Bearer SECRET' false", "plugin2"); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("error leaks the command: %v", err)
	}
	if _, _, err := Run(context.Background(), `printf 'not json SECRET'`, "plugin3"); err == nil || !strings.HasPrefix(err.Error(), "plugin3 line 1") {
		t.Errorf("parse error: %v", err)
	}
}

// Cancelling must stop the whole pipeline, not only sh: otherwise the run
// waits for "sleep | cat" to close stdout.
func TestRunCancelPipeline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := Run(ctx, "sleep 30 | cat", "plugin1")
	if err == nil {
		t.Error("no error after cancellation")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("returned after %v", d)
	}
}

// "unverified" marks objects whose cluster state cannot confirm.
func TestParseUnverified(t *testing.T) {
	res, _, err := Parse([]byte(`{"type":"vault_policy","identity":{"id":"ci"},"unverified":true}
{"type":"vault_policy","identity":{"id":"ops"}}`), "plugin1")
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Scope == nil || !res[0].Scope.Unverifiable || res[0].Scope.Trusted || res[1].Scope != nil {
		t.Errorf("scopes %+v %+v", res[0].Scope, res[1].Scope)
	}
}

// Two objects on one line: the second (an error line) must not be dropped.
func TestParseTwoObjectsOnALine(t *testing.T) {
	if _, _, err := Parse([]byte(`{"type":"t","identity":{"id":"x"}}{"error":"permission denied"}`), "plugin1"); err == nil {
		t.Error("no error")
	}
}
