//go:build integration && unix

package cli

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestIntegrationSignalCleanup stops the binary with SIGTERM while
// terraform query waits on a provider: terraform and the provider die, the
// run ends with an error and no report, and no temporary file stays in the
// root module.
func TestIntegrationSignalCleanup(t *testing.T) {
	data, root := setup(t)
	write(t, filepath.Join(root, "main.tf"), `
terraform {
  required_providers {
    fakeloc = { source = "example.com/fake/fake" }
  }
}
resource "fake_thing" "a" {
  provider = fakeloc
  name     = "managed-a"
}
`)
	terraform(t, root, "apply", "-auto-approve", "-no-color")
	t.Setenv("FAKE_HANG", "thing")
	cmd := tool(t, "-no-config", "-format", "json", "-exclude-types", "fake_broken", root)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start(t, cmd)
	provider := waitPid(t, filepath.Join(data, "hanging"))
	killOnFailure(t, provider)
	if left, _ := filepath.Glob(filepath.Join(root, "zz_tforphans_*")); len(left) == 0 {
		t.Error("no query file while terraform query runs")
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err := waitFor(t, cmd, 20*time.Second)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != exitError || stdout.Len() != 0 || !strings.Contains(stderr.String(), "error:") {
		t.Errorf("%v\nstdout:\n%s\nstderr:\n%s", err, &stdout, &stderr)
	}
	waitGone(t, provider)
	if left, _ := filepath.Glob(filepath.Join(root, "zz_tforphans_*")); len(left) > 0 {
		t.Errorf("left behind: %v", left)
	}
	if _, err := os.Stat(filepath.Join(root, "terraform.tfstate")); err != nil {
		t.Errorf("state: %v", err)
	}
}
