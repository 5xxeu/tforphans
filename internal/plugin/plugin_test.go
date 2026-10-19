package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/5xxeu/tforphans/internal/proc"
)

func TestRun(t *testing.T) {
	res, warns, _, err := Run(context.Background(), `printf '%s\n' '{"type":"vault_policy","identity":{"id":"ci"},"display_name":"ci"}' '' '{"error":"sys/auth: permission denied"}'`, "plugin1")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Type != "vault_policy" || !slices.Equal(res[0].RequiredKeys, []string{"id"}) || res[0].Source == "" {
		t.Errorf("res = %+v", res)
	}
	if len(warns) != 1 {
		t.Errorf("warns = %v", warns)
	}
}

func TestRunErrors(t *testing.T) {
	for _, cmd := range []string{
		`echo '{"type":"x"}'`, // no identity
		`echo '{"type":"x y","identity":{"id":"1"}}'`,
		`echo '{"type":"x","provider":"a b","identity":{"id":"1"}}'`,
		`echo 'not json'`,
		`exit 3`,
	} {
		if _, _, _, err := Run(context.Background(), cmd, "plugin1"); err == nil {
			t.Errorf("%s: no error", cmd)
		}
	}
}

func TestRunDoesNotPrintTheCommand(t *testing.T) {
	res, _, _, err := Run(context.Background(), `AUTH='Bearer SECRET' printf '%s\n' '{"type":"t","identity":{"id":"x"}}'`, "plugin1")
	if err != nil || res[0].Source != "plugin1" {
		t.Errorf("source %+v %v", res, err)
	}
	if _, _, _, err := Run(context.Background(), "AUTH='Bearer SECRET' false", "plugin2"); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("error leaks the command: %v", err)
	}
	if _, _, _, err := Run(context.Background(), `printf 'not json SECRET'`, "plugin3"); err == nil || !strings.HasPrefix(err.Error(), "plugin3 line 1") {
		t.Errorf("parse error: %v", err)
	}
}

// Cancelling must stop the whole pipeline, not only sh: otherwise the run
// waits for "sleep | cat" to close stdout.
func TestRunCancelPipeline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, _, err := Run(ctx, "sleep 30 | cat", "plugin1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("cancellation error: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("returned after %v", d)
	}
}

func TestRunWaitDelay(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	t.Cleanup(func() {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			t.Error(err)
			return
		}
		var pid int
		if _, err := fmt.Sscan(string(data), &pid); err != nil {
			t.Error(err)
			return
		}
		if child, err := os.FindProcess(pid); err == nil {
			child.Kill()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := time.Now()
	command := fmt.Sprintf(`printf '%%s\n' '{"type":"t","identity":{"id":"x"}}'; sleep 30 & echo $! > '%s'; exit 0`, pidFile)
	res, warns, _, err := Run(ctx, command, "plugin1")
	if !errors.Is(err, exec.ErrWaitDelay) || !strings.HasPrefix(err.Error(), "plugin1:") || res != nil || warns != nil {
		t.Errorf("inherited stdout: %v, %v, %v", res, warns, err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("returned after %v", d)
	}
}

func TestParseLimits(t *testing.T) {
	oldLine, oldOutput, oldResources := maxLineBytes, maxOutputBytes, maxResources
	t.Cleanup(func() { maxLineBytes, maxOutputBytes, maxResources = oldLine, oldOutput, oldResources })
	resource := `{"type":"t","identity":{"id":"x"}}` + "\n"
	warning := `{"error":"permission denied"}` + "\n"
	maxResources = 2
	if res, warns, _, err := Parse([]byte(resource+warning+"\n{\"done\":true}\n{\"done\":true}\n"), "plugin1"); err != nil || len(res) != 1 || len(warns) != 1 {
		t.Fatalf("resource boundary: %d resources, %d warnings, %v", len(res), len(warns), err)
	}
	for _, out := range []string{strings.Repeat(resource, 3), resource + warning + resource, warning + resource + warning} {
		if res, warns, _, err := Parse([]byte(out), "plugin1"); err == nil || !strings.Contains(err.Error(), "plugin1: output exceeds 2 resource limit") || res != nil || warns != nil {
			t.Errorf("resource limit: %v, %v, %v", res, warns, err)
		}
	}
	maxOutputBytes = int64(len(resource))
	if res, _, _, err := Parse([]byte(resource), "plugin2"); err != nil || len(res) != 1 {
		t.Errorf("byte boundary: %v, %v", res, err)
	}
	for _, out := range []string{resource + "\n", resource + warning, strings.Repeat(" ", len(resource)+1)} {
		if res, warns, _, err := Parse([]byte(out), "plugin2"); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("plugin2: output exceeds %d byte limit", maxOutputBytes)) || res != nil || warns != nil {
			t.Errorf("byte limit: %v, %v, %v", res, warns, err)
		}
	}
	if res, _, _, err := parseReader(iotest.OneByteReader(strings.NewReader(resource)), "plugin2"); err != nil || len(res) != 1 {
		t.Errorf("streamed byte boundary: %v, %v", res, err)
	}
	maxOutputBytes = int64(len(resource) + 5)
	r := strings.NewReader(resource + warning)
	if _, _, _, err := parseReader(iotest.OneByteReader(r), "plugin2"); err == nil || !strings.Contains(err.Error(), "byte limit") || r.Len() != len(warning)-6 {
		t.Errorf("stream read beyond limit+1 or reported truncated JSON: %d bytes left, %v", r.Len(), err)
	}
	maxOutputBytes = oldOutput
	maxLineBytes = 128
	if _, _, _, err := Parse([]byte(resource), "plugin3"); err != nil {
		t.Error(err)
	}
	if _, _, _, err := Parse([]byte(strings.Repeat(" ", maxLineBytes)), "plugin3"); err == nil || !strings.Contains(err.Error(), "plugin3: output line exceeds 128 byte limit") {
		t.Errorf("line limit: %v", err)
	}
}

func TestRunStopsOnInvalidOutputAndLimits(t *testing.T) {
	oldLine, oldOutput, oldResources := maxLineBytes, maxOutputBytes, maxResources
	t.Cleanup(func() { maxLineBytes, maxOutputBytes, maxResources = oldLine, oldOutput, oldResources })
	for _, c := range []struct {
		name, output, want string
		lineLimit          int
		outputLimit        int64
		resourceLimit      int
	}{
		{"invalid", "not json", "plugin1 line 1", oldLine, oldOutput, oldResources},
		{"line", strings.Repeat("x", 128), "line exceeds 128 byte limit", 128, oldOutput, oldResources},
		{"bytes", strings.Repeat(" ", 129), "output exceeds 128 byte limit", oldLine, 128, oldResources},
		{"resources", "{\"type\":\"t\",\"identity\":{\"id\":\"x\"}}\n{\"type\":\"t\",\"identity\":{\"id\":\"y\"}}", "output exceeds 1 resource limit", oldLine, oldOutput, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			maxLineBytes, maxOutputBytes, maxResources = c.lineLimit, c.outputLimit, c.resourceLimit
			pidFile := filepath.Join(t.TempDir(), "pid")
			// The child holds stdout open, so the parser must cancel the group.
			command := fmt.Sprintf("sleep 30 & child=$!; echo $child > '%s'; printf '%%s\\n' '%s'; wait", pidFile, c.output)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			start := time.Now()
			res, warns, _, err := Run(ctx, command, "plugin1")
			if err == nil || !strings.Contains(err.Error(), c.want) || res != nil || warns != nil {
				t.Fatalf("result: %v, %v, %v", res, warns, err)
			}
			if time.Since(start) > 3*time.Second {
				t.Errorf("plugin did not stop promptly: %v", time.Since(start))
			}
			data, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatal(err)
			}
			var pid int
			if _, err := fmt.Sscan(string(data), &pid); err != nil {
				t.Fatal(err)
			}
			// Reaping a killed grandchild can lag behind the shell's exit.
			for deadline := time.Now().Add(time.Second); proc.Alive(pid) && time.Now().Before(deadline); {
				time.Sleep(10 * time.Millisecond)
			}
			if proc.Alive(pid) {
				t.Errorf("plugin child %d still alive", pid)
			}
		})
	}
}

func TestRunExitAfterParse(t *testing.T) {
	for _, out := range []string{`{"type":"t","identity":{"id":"x"}}`, `{"error":"permission denied"}`} {
		res, warns, _, err := Run(context.Background(), fmt.Sprintf("printf '%%s' '%s'; exit 7", out), "plugin1")
		if err == nil || !strings.Contains(err.Error(), "plugin1: exit status 7") || res != nil || warns != nil {
			t.Errorf("exit after parse: %v, %v, %v", res, warns, err)
		}
	}
	_, _, _, err := Run(context.Background(), "printf '%s' 'not json'; exit 7", "plugin1")
	if err == nil || !strings.HasPrefix(err.Error(), "plugin1 line 1") {
		t.Errorf("invalid output should precede exit error: %v", err)
	}
}

// "unverified" marks objects whose cluster state cannot confirm.
func TestParseUnverified(t *testing.T) {
	res, _, _, err := Parse([]byte(`{"type":"vault_policy","identity":{"id":"ci"},"unverified":true}
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
	if _, _, _, err := Parse([]byte(`{"type":"t","identity":{"id":"x"}}{"error":"permission denied"}`), "plugin1"); err == nil {
		t.Error("no error")
	}
}

// TestParseNumberRange: JSON allows numbers HCL cannot read; one of them in
// an import block would make the whole file invalid. Large but readable
// numbers stay exact.
func TestParseNumberRange(t *testing.T) {
	for _, bad := range []string{`1e7000000000`, `[1, {"n": -1e-7000000000}]`} {
		if _, _, _, err := Parse([]byte(`{"type":"t","identity":{"id":"x","v":`+bad+`}}`), "plugin1"); err == nil || !strings.Contains(err.Error(), "out of range for Terraform") {
			t.Errorf("%s: %v", bad, err)
		}
	}
	res, _, _, err := Parse([]byte(`{"type":"t","identity":{"n":123456789012345678901234567890,"f":1e300}}`), "plugin1")
	if err != nil || len(res) != 1 || res[0].Identity["n"].(json.Number) != "123456789012345678901234567890" {
		t.Errorf("%+v %v", res, err)
	}
}

func TestRunStopsEndlessOutput(t *testing.T) {
	oldOutput, oldResources := maxOutputBytes, maxResources
	t.Cleanup(func() { maxOutputBytes, maxResources = oldOutput, oldResources })
	for _, c := range []struct {
		name, command, want string
		outputLimit         int64
		resourceLimit       int
	}{
		{"blank lines", "yes ''", "output exceeds 65536 byte limit", 1 << 16, oldResources},
		{"resources", `yes '{"type":"t","identity":{"id":"x"}}'`, "output exceeds 1000 resource limit", oldOutput, 1000},
		{"warnings", `yes '{"error":"x"}'`, "output exceeds 1000 resource limit", oldOutput, 1000},
	} {
		t.Run(c.name, func(t *testing.T) {
			maxOutputBytes, maxResources = c.outputLimit, c.resourceLimit
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _, _, err := Run(ctx, c.command, "plugin1")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestRunReturnsAfterParentExit(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	// The shell exits first; its child prints invalid output later and
	// keeps stdout open. The child is not signalled once the shell is
	// reaped (its group ID could be reused), but the error comes back.
	command := fmt.Sprintf("(sleep 1; printf 'invalid\\n'; exec sleep 30) & echo $! > '%s'; exit 0", pidFile)
	t.Cleanup(func() {
		data, err := os.ReadFile(pidFile)
		var pid int
		if err == nil {
			if _, err := fmt.Sscan(string(data), &pid); err == nil && pid > 0 {
				if child, err := os.FindProcess(pid); err == nil {
					child.Kill()
				}
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	if _, _, _, err := Run(ctx, command, "plugin1"); err == nil || !strings.Contains(err.Error(), "plugin1 line 1") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("returned after %v", time.Since(start))
	}
}

// TestParseNullIdentity: jq gives null for a missing field; such a line is
// an error, not an object named by its other keys.
func TestParseNullIdentity(t *testing.T) {
	if _, _, _, err := Parse([]byte(`{"type":"t","identity":{"account_id":"1","id":null}}`), "plugin1"); err == nil || !strings.Contains(err.Error(), `"id" is null`) {
		t.Errorf("%v", err)
	}
}

func TestParseDone(t *testing.T) {
	resource := `{"type":"t","identity":{"id":"x"}}`
	for _, c := range []struct {
		name, output string
		resources    int
		done         bool
		invalid      bool
	}{
		{"empty", "\n \n", 0, false, false},
		{"done", `{"done": true}`, 0, true, false},
		{"escaped done", `{"d\u006fne":true}`, 0, true, false},
		{"escaped false with resource", `{"d\u006fne":false,"type":"t","identity":{"id":"x"}}`, 0, false, true},
		{"uppercase with resource", `{"DONE":false,"type":"t","identity":{"id":"x"}}`, 0, false, true},
		{"mixed case", `{"Done":true}`, 0, false, true},
		{"both cases", `{"done":true,"DONE":true}`, 0, false, true},
		{"escaped uppercase", `{"D\u004fNE":true}`, 0, false, true},
		{"nested done", `{"type":"t","identity":{"id":"x","done":false}}`, 1, false, false},
		{"escaped identity", `{"type":"t","identity":{"id":"x\ny"}}`, 1, false, false},
		{"repeat", "{\"done\":true}\n{\"done\":true}", 0, true, false},
		{"before resource", "{\"done\":true}\n" + resource, 1, true, false},
		{"after resource", resource + "\n{\"done\":true}", 1, true, false},
		{"resource", resource, 1, false, false},
		{"false", `{"done":false}`, 0, false, true},
		{"null", `{"done":null}`, 0, false, true},
		{"string", `{"done":"true"}`, 0, false, true},
		{"number", `{"done":1}`, 0, false, true},
		{"with type", `{"done":true,"type":"x"}`, 0, false, true},
		{"with error", `{"done":true,"error":"denied"}`, 0, false, true},
		{"with unknown", `{"done":true,"other":null}`, 0, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, warns, done, err := Parse([]byte(c.output), "plugin1")
			if (err != nil) != c.invalid || len(res) != c.resources || len(warns) != 0 || done != c.done {
				t.Fatalf("result: %v, %v, %v, %v", res, warns, done, err)
			}
			if err != nil && !strings.HasPrefix(err.Error(), "plugin1 line 1:") {
				t.Errorf("error: %v", err)
			}
		})
	}
}
