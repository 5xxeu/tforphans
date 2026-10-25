//go:build unix

package main

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// killDescendants kills every process below pid. It must run while pid is
// alive: once it dies, its children belong to init and pgrep -P no longer
// finds them. A plugin runs in its own process group, so killing the
// binary alone leaves the plugin and its children behind. An error means
// that pgrep could not list children; what was found is killed anyway.
func killDescendants(pid int) error {
	var all []int
	var failed error
	for next := []int{pid}; len(next) > 0; {
		var found []int
		for _, p := range next {
			out, err := exec.Command("pgrep", "-P", strconv.Itoa(p)).Output()
			var exit *exec.ExitError
			if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) { // 1: no children
				failed = errors.Join(failed, fmt.Errorf("pgrep -P %d: %w", p, err))
			}
			for _, f := range strings.Fields(string(out)) {
				if c, err := strconv.Atoi(f); err == nil {
					found = append(found, c)
				}
			}
		}
		all = append(all, found...)
		next = found
	}
	for _, p := range all {
		_ = syscall.Kill(p, syscall.SIGKILL)
	}
	return failed
}

// running tells whether pid is a process that has not exited. A zombie has
// exited: in a container without an init that reaps orphans it stays in
// the process table, and kill(pid, 0) still succeeds. An error means that
// ps could not tell, which must fail the test rather than pass it.
func running(pid int) (bool, error) {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		// ps exits 1 for a pid that is gone; it may have exited just now.
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return false, nil
		}
		return false, fmt.Errorf("ps -p %d: %w", pid, err)
	}
	stat := strings.TrimSpace(string(out))
	if stat == "" {
		return false, fmt.Errorf("ps -p %d: no state", pid)
	}
	return !strings.HasPrefix(stat, "Z"), nil
}

// mustRun is running for tests, failing them when ps cannot tell.
func mustRun(t *testing.T, pid int) bool {
	t.Helper()
	alive, err := running(pid)
	if err != nil {
		t.Fatal(err)
	}
	return alive
}

// TestProcessHelpers: an exited child that is not waited for yet is a
// zombie and does not run; killDescendants reaches a grandchild.
func TestProcessHelpers(t *testing.T) {
	zombie := exec.Command("true")
	if err := zombie.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for mustRun(t, zombie.Process.Pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(zombie.Process.Pid, 0); err != nil || mustRun(t, zombie.Process.Pid) {
		t.Errorf("zombie: kill %v, running %v", err, mustRun(t, zombie.Process.Pid))
	}
	_ = zombie.Wait()

	sh := exec.Command("sh", "-c", "sleep 300 & echo $!; wait")
	out, err := sh.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sh.Start(); err != nil {
		t.Fatal(err)
	}
	var sleep int
	t.Cleanup(func() {
		if sleep > 0 {
			_ = syscall.Kill(sleep, syscall.SIGKILL)
		}
		_ = sh.Process.Kill()
		_ = sh.Wait()
	})
	if _, err := fmt.Fscan(out, &sleep); err != nil {
		t.Fatal(err)
	}
	if err := killDescendants(sh.Process.Pid); err != nil {
		t.Fatal(err)
	}
	// sleep runs for 300 s unless it was killed; sh is not waited for,
	// since its wait would end only with sleep.
	for deadline := time.Now().Add(10 * time.Second); mustRun(t, sleep); {
		if time.Now().After(deadline) {
			t.Fatal("grandchild still runs")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := killDescendants(1 << 30); err != nil {
		t.Errorf("pid without children: %v", err)
	}
}
