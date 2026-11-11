package tf

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestExecLimited: output above the limit stops the command and names the
// flag that raises it; output within it comes back whole.
func TestExecLimited(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	bin := filepath.Join(t.TempDir(), "fake-terraform")
	script := "#!/bin/sh\nhead -c \"$FAKE_BYTES\" /dev/zero | tr '\\0' x\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_BYTES", "5000")
	r := Runner{Bin: bin, Dir: t.TempDir()}
	out, _, err := r.ExecLimited(context.Background(), 10000, "apply")
	if err != nil || len(out) != 5000 {
		t.Fatalf("within limit: %d bytes, %v", len(out), err)
	}
	_, _, err = r.ExecLimited(context.Background(), 1000, "apply")
	if err == nil || !strings.Contains(err.Error(), "-max-query-size") {
		t.Fatalf("above limit: %v", err)
	}
}
