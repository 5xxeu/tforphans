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
// binary alone leaves the plugin and its children behind.
func killDescendants(pid int) {
	var all []int
	for next := []int{pid}; len(next) > 0; {
		var found []int
		for _, p := range next {
			out, _ := exec.Command("pgrep", "-P", strconv.Itoa(p)).Output()
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
}

// running tells whether pid is a process that has not exited. A zombie has
// exited: in a container without an init that reaps orphans it stays in
// the process table, and kill(pid, 0) still succeeds.
func running(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false // ps exits 1 when there is no such process
	}
	stat := strings.TrimSpace(string(out))
	return stat != "" && !strings.HasPrefix(stat, "Z")
}

// TestProcessHelpers: an exited child that is not waited for yet is a
// zombie and does not run; killDescendants reaches a grandchild.
func TestProcessHelpers(t *testing.T) {
	zombie := exec.Command("true")
	if err := zombie.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for running(zombie.Process.Pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(zombie.Process.Pid, 0); err != nil || running(zombie.Process.Pid) {
		t.Errorf("zombie: kill %v, running %v", err, running(zombie.Process.Pid))
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
	t.Cleanup(func() { _ = sh.Process.Kill(); _ = sh.Wait() })
	var sleep int
	if _, err := fmt.Fscan(out, &sleep); err != nil {
		t.Fatal(err)
	}
	killDescendants(sh.Process.Pid)
	if err := sh.Wait(); err != nil {
		t.Logf("sh: %v", err) // wait returns once sleep is killed
	}
	for running(sleep) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if running(sleep) {
		_ = syscall.Kill(sleep, syscall.SIGKILL)
		t.Error("grandchild still runs")
	}
}
