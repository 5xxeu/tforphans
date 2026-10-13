//go:build unix

package proc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStreamSlowConsumer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lines := []string{
		`{"type":"t","identity":{"id":"x"}}`,
		`{"type":"t","identity":{"id":"y"}}`,
		`{"error":"permission denied"}`,
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", "printf '%s\\n' '"+strings.Join(lines, "' '")+"'")
	Group(cmd)
	cmd.WaitDelay = 20 * time.Millisecond
	expired := make(chan struct{})
	var once sync.Once
	kill := cmd.Cancel
	cmd.Cancel = func() error {
		err := kill()
		once.Do(func() { close(expired) })
		return err
	}
	var got []string
	err := Stream(ctx, cmd, func(r io.Reader) error {
		// Leave stdout buffered until the parent exited and the timer fired.
		select {
		case <-expired:
		case <-ctx.Done():
			return ctx.Err()
		}
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			time.Sleep(cmd.WaitDelay)
			var object map[string]any
			if err := json.Unmarshal(sc.Bytes(), &object); err != nil {
				return err
			}
			got = append(got, sc.Text())
		}
		return sc.Err()
	})
	if err != nil || !slices.Equal(got, lines) {
		t.Fatalf("slow consumer: %v, %v", got, err)
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Success() {
		t.Errorf("parent did not exit successfully: %v", cmd.ProcessState)
	}
}

func TestStreamCancelAfterParentExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30 & echo $!; exit 0")
	Group(cmd)
	cmd.WaitDelay = time.Second
	var pid int
	t.Cleanup(func() {
		if pid != 0 {
			if child, err := os.FindProcess(pid); err == nil {
				child.Kill()
			}
		}
	})
	start := time.Now()
	err := Stream(ctx, cmd, func(r io.Reader) error {
		sc := bufio.NewScanner(r)
		if sc.Scan() {
			if _, err := fmt.Sscan(sc.Text(), &pid); err != nil {
				return err
			}
		}
		for sc.Scan() {
		}
		return sc.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("returned after %v", time.Since(start))
	}
	if pid == 0 || cmd.ProcessState == nil || !cmd.ProcessState.Success() {
		t.Fatalf("parent did not exit before cancellation: %d, %v", pid, cmd.ProcessState)
	}
	for deadline := time.Now().Add(time.Second); Alive(pid) && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if Alive(pid) {
		t.Errorf("child %d still running", pid)
	}
}
