//go:build unix

package state

import (
	"context"
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestFetchFIFOTimeout: opening a named pipe nobody writes to blocks; the
// timeout still ends it.
func TestFetchFIFOTimeout(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "state.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := Fetch(ctx, fifo, false, DefaultMaxSize); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Errorf("%v after %s", err, time.Since(start))
	}
}
