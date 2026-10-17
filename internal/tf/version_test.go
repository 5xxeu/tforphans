//go:build unix

package tf

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestQueryVersionTimeout: a terraform whose version cannot be read is
// accepted, one that runs out of time is not.
func TestQueryVersionTimeout(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "terraform")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (Runner{Bin: bin, Dir: dir}).QueryVersion(context.Background()); err != nil {
		t.Errorf("failing version: %v", err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := (Runner{Bin: bin, Dir: dir}).QueryVersion(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("hanging version: %v", err)
	}
}
