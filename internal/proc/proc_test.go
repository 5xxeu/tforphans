//go:build unix

package proc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
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
	var got []string
	err := Stream(ctx, cmd, func(r io.Reader) error {
		// Leave stdout buffered well past the parent's exit and WaitDelay.
		time.Sleep(10 * cmd.WaitDelay)
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			time.Sleep(2 * cmd.WaitDelay)
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

func TestStreamDetachedDescendant(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStreamHelperProcess$")
	// Skip the race runtime's exit sleep in the helper.
	cmd.Env = append(os.Environ(), "TFORPHANS_PROC_HELPER=parent", "GORACE="+os.Getenv("GORACE")+" atexit_sleep_ms=0")
	Group(cmd)
	cmd.WaitDelay = 100 * time.Millisecond
	var pid int
	t.Cleanup(func() {
		if pid != 0 {
			if child, err := os.FindProcess(pid); err == nil {
				child.Kill()
			}
		}
	})
	var got []string
	start := time.Now()
	err := Stream(ctx, cmd, func(r io.Reader) error {
		sc := bufio.NewScanner(r)
		if sc.Scan() {
			if _, err := fmt.Sscan(sc.Text(), &pid); err != nil {
				return err
			}
		}
		for sc.Scan() {
			got = append(got, sc.Text())
		}
		return sc.Err()
	})
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Errorf("detached descendant: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 6*cmd.WaitDelay {
		t.Errorf("returned after %v", elapsed)
	}
	if !slices.Equal(got, []string{"first", "second"}) {
		t.Errorf("output: %v", got)
	}
	if pid == 0 || cmd.ProcessState == nil || !cmd.ProcessState.Success() {
		t.Fatalf("parent did not exit successfully: %d, %v", pid, cmd.ProcessState)
	}
}

func TestStreamHelperProcess(t *testing.T) {
	switch os.Getenv("TFORPHANS_PROC_HELPER") {
	case "parent":
		cmd := exec.Command(os.Args[0], "-test.run=^TestStreamHelperProcess$")
		cmd.Env = append(os.Environ(), "TFORPHANS_PROC_HELPER=child")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stdout, cmd.Process.Pid)
		fmt.Fprintln(os.Stdout, "first\nsecond")
		cmd.Process.Release()
		os.Exit(0)
	case "child":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
}

func TestStreamStderrDescendant(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30 >/dev/null & echo $!; printf 'first\\nsecond\\n' >&2; exit 0")
	Group(cmd)
	cmd.WaitDelay = time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	var pid int
	t.Cleanup(func() {
		if pid != 0 {
			if child, err := os.FindProcess(pid); err == nil {
				child.Kill()
			}
		}
	})
	var waited time.Duration
	start := time.Now()
	err := Stream(ctx, cmd, func(r io.Reader) error {
		data, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		if _, err := fmt.Sscan(string(data), &pid); err != nil {
			return err
		}
		// Draining starts only after Stream receives the Wait result.
		reader := r.(*drainReader)
		for !reader.draining.Load() {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			time.Sleep(time.Millisecond)
		}
		waited = time.Since(start)
		return nil
	})
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Errorf("inherited stderr: %v", err)
	}
	if waited > cmd.WaitDelay/2 {
		t.Errorf("Wait returned after %v", waited)
	}
	if elapsed := time.Since(start); elapsed < cmd.WaitDelay || elapsed > 2*cmd.WaitDelay {
		t.Errorf("Stream returned after %v", elapsed)
	}
	if got := stderr.String(); got != "first\nsecond\n" {
		t.Errorf("stderr: %q", got)
	}
	if pid == 0 || cmd.ProcessState == nil || !cmd.ProcessState.Success() {
		t.Fatalf("parent did not exit successfully: %d, %v", pid, cmd.ProcessState)
	}
}

func TestStreamCancelAfterParentExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30 & echo $!; exit 0")
	Group(cmd)
	cmd.WaitDelay = time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	var signals atomic.Int32
	kill := cmd.Cancel
	cmd.Cancel = func() error {
		signals.Add(1)
		return kill()
	}
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
	if got := signals.Load(); got != 0 {
		t.Errorf("signalled after parent exit: %d", got)
	}
}
