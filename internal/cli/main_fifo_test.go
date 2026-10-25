//go:build unix

package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestBaselineFIFOTimeout: a -baseline nobody writes to blocks on open;
// -timeout still ends the run with an error and no report.
func TestBaselineFIFOTimeout(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "old.json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip(err)
	}
	plugin := `printf '%s\n' '{"type":"vault_policy","identity":{"id":"x"}}'`
	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := run(context.Background(), []string{"-no-config", "-no-query", "-plugin", plugin, "-baseline", fifo, "-timeout", "300ms", "-format", "json"}, &stdout, &stderr)
	if code != exitError || stdout.Len() != 0 || !strings.Contains(stderr.String(), "stopped after 300ms (-timeout)") || time.Since(start) > 10*time.Second {
		t.Errorf("exit %d after %s\n%s\n%s", code, time.Since(start), &stdout, &stderr)
	}
}

// TestIgnoreFIFOTimeout: the same for -ignore, and for the ignore file read
// without the flag.
func TestIgnoreFIFOTimeout(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, defaultIgnoreFile)
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip(err)
	}
	t.Chdir(dir)
	plugin := `printf '%s\n' '{"type":"vault_policy","identity":{"id":"x"}}'`
	for _, args := range [][]string{{"-ignore", fifo}, nil} {
		var stdout, stderr bytes.Buffer
		start := time.Now()
		code := run(context.Background(), append([]string{"-no-config", "-no-query", "-plugin", plugin, "-timeout", "300ms", "-format", "json"}, args...), &stdout, &stderr)
		if code != exitError || stdout.Len() != 0 || !strings.Contains(stderr.String(), "stopped after 300ms (-timeout)") || time.Since(start) > 10*time.Second {
			t.Errorf("%v: exit %d after %s\n%s\n%s", args, code, time.Since(start), &stdout, &stderr)
		}
	}
}
