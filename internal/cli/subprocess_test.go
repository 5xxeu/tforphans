//go:build unix

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestBinaryExitCodes runs the binary: exit codes, and the report on stdout
// with everything else on stderr.
func TestBinaryExitCodes(t *testing.T) {
	dir := t.TempDir()
	st := `{"version":4,"lineage":"l","serial":1,"resources":[{"mode":"managed","type":"vault_policy","name":"p","provider":"provider[\"registry.terraform.io/hashicorp/vault\"]","instances":[{"identity":{"id":"kept"},"attributes":{"id":"kept"}}]}]}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(st), 0o600); err != nil {
		t.Fatal(err)
	}
	managed := `printf '%s\n' '{"type":"vault_policy","identity":{"id":"kept"}}'`
	orphan := `printf '%s\n' '{"type":"vault_policy","identity":{"id":"kept"}}' '{"type":"vault_policy","identity":{"id":"new"},"display_name":"new"}'`
	base := []string{"-no-config", "-no-query", "-state", "state.json"}
	for _, c := range []struct {
		name       string
		args       []string
		code       int
		stdout     string // substring; "" means empty
		stderr     string
		jsonReport bool
	}{
		{"nothing unmanaged", []string{"-plugin", managed}, exitOK, "No unmanaged resources found.", "1 live, 1 managed", false},
		{"unmanaged found", []string{"-plugin", orphan}, exitOrphans, "vault_policy", "2 live, 1 managed, 0 uncertain, 0 ignored, 1 unmanaged", false},
		{"json report", []string{"-plugin", orphan, "-format", "json"}, exitOrphans, `"orphans"`, "1 unmanaged", true},
		{"adopt writes imports", []string{"adopt", "-plugin", orphan}, exitOK, "to = vault_policy.new", "", false},
		{"failed plugin", []string{"-plugin", managed, "-plugin", "exit 3"}, exitError, "No unmanaged resources found.", "plugin2 failed", false},
		{"failed plugin keeps json", []string{"-plugin", managed, "-plugin", "exit 3", "-format", "json"}, exitError, `"failed_sources"`, "plugin2 failed", true},
		{"unknown flag", []string{"-no-such-flag"}, exitError, "", "flag provided but not defined", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := c.args
			if args[0] == "adopt" {
				args = append([]string{"adopt"}, append(base, args[1:]...)...)
			} else if args[0] != "-no-such-flag" {
				args = append(base, args...)
			}
			code, stdout, stderr := runBinary(t, dir, args...)
			if code != c.code || (c.stdout == "") != (stdout == "") || !strings.Contains(stdout, c.stdout) || !strings.Contains(stderr, c.stderr) {
				t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
			}
			if c.jsonReport {
				var v map[string]any
				if err := json.Unmarshal([]byte(stdout), &v); err != nil {
					t.Errorf("stdout is not one JSON document: %v", err)
				}
				if strings.Contains(stderr, `"orphans"`) {
					t.Errorf("report on stderr:\n%s", stderr)
				}
			}
		})
	}
}

// TestBinarySignal: SIGTERM or SIGINT during a plugin stops the run within
// seconds, with an error and no report, and the plugin's own children die
// too.
func TestBinarySignal(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "child")
			// The plugin's child sleeps with its own copy of stdout.
			plugin := `sleep 300 & echo $! > "$TFO_PIDFILE.tmp" && mv "$TFO_PIDFILE.tmp" "$TFO_PIDFILE"; wait`
			cmd := tool(t, "-no-config", "-no-query", "-format", "json", "-plugin", plugin)
			cmd.Dir = dir
			cmd.Env = append(cmd.Env, "TFO_PIDFILE="+pidFile)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			start(t, cmd)
			child := waitPid(t, pidFile)
			killOnFailure(t, child)
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			err := waitFor(t, cmd, 15*time.Second)
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != exitError || stdout.Len() != 0 || !strings.Contains(stderr.String(), "error:") {
				t.Errorf("%v\nstdout:\n%s\nstderr:\n%s", err, &stdout, &stderr)
			}
			waitGone(t, child)
		})
	}
}

// killOnFailure kills a process the run should have stopped, when the test
// fails before it is gone.
func killOnFailure(t *testing.T, pid int) {
	t.Cleanup(func() {
		if t.Failed() {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
}

// waitPid reads a pid that a child writes once it runs.
func waitPid(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no pid in %s", path)
	return 0
}

// waitGone waits for a process to exit (a zombie nobody reaps counts).
func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !mustRun(t, pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Errorf("process %d still runs", pid)
}

// TestBinaryExplain: -explain adds a line per live object to the text
// report, and is refused where there is no report to add it to.
func TestBinaryExplain(t *testing.T) {
	dir := t.TempDir()
	st := `{"version":4,"lineage":"l","serial":1,"resources":[{"mode":"managed","type":"vault_policy","name":"p","provider":"provider[\"registry.terraform.io/hashicorp/vault\"]","instances":[{"identity":{"id":"kept"},"attributes":{"id":"kept"}}]}]}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(st), 0o600); err != nil {
		t.Fatal(err)
	}
	plugin := `printf '%s\n' '{"type":"vault_policy","identity":{"id":"kept"}}' '{"type":"vault_policy","identity":{"id":"kept","namespace":"ns"},"display_name":"other"}'`
	code, stdout, stderr := runBinary(t, dir, "-no-config", "-no-query", "-state", "state.json", "-plugin", plugin, "-explain")
	if code != exitOrphans || !strings.Contains(stdout, "== Why (-explain) ") ||
		!regexp.MustCompile(`managed +vault_policy +id=kept +vault_policy.p in state.json, by resource identity`).MatchString(stdout) ||
		!regexp.MustCompile(`unmanaged +vault_policy +other +id=kept, namespace=ns +closest: vault_policy.p in state.json: its identity has no namespace`).MatchString(stdout) {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	for _, args := range [][]string{{"adopt", "-no-config", "-no-query", "-plugin", plugin, "-explain"}, {"-no-config", "-no-query", "-plugin", plugin, "-explain", "-format", "summary"}} {
		if code, _, stderr := runBinary(t, dir, args...); code != exitError || !strings.Contains(stderr, "-explain needs the text or json report") {
			t.Errorf("%v: exit %d\n%s", args, code, stderr)
		}
	}
}
