package cli

import (
	"bytes"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/5xxeu/tforphans/internal/tf"
)

// TestMain lets tests run the real Main, with its signal handling and exit
// codes, as a child process of this test binary.
func TestMain(m *testing.M) {
	tf.RunWrapper()
	if os.Getenv("TFORPHANS_TEST_MAIN") == "1" {
		Main("")
	}
	os.Exit(m.Run())
}

// tool is tforphans as a process: this test binary running Main.
func tool(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), "TFORPHANS_TEST_MAIN=1")
	return cmd
}

// runBinary runs tool to the end and returns its exit code and output.
func runBinary(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	cmd := tool(t, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start(t, cmd)
	err := waitFor(t, cmd, time.Minute)
	if _, ok := err.(*exec.ExitError); err != nil && !ok {
		t.Fatal(err)
	}
	return cmd.ProcessState.ExitCode(), stdout.String(), stderr.String()
}

// start starts cmd and makes sure it does not outlive the test.
func start(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			if err := killDescendants(cmd.Process.Pid); err != nil {
				t.Log(err)
			}
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
}

// waitFor waits for cmd at most limit, then kills it and fails: a run or
// a stop that hangs must not hang the test run.
func waitFor(t *testing.T, cmd *exec.Cmd, limit time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(limit):
		if err := killDescendants(cmd.Process.Pid); err != nil {
			t.Log(err)
		}
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("still running after %s", limit)
		return nil
	}
}
