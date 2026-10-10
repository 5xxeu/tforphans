// Package proc runs child commands so that cancelling them stops
// everything they started.
package proc

import (
	"os/exec"
	"time"
)

// Group makes cancellation of cmd stop the processes it started too (a
// shell pipeline, a terraform wrapper and its terraform), and bounds the
// wait for children that still hold stdout or stderr open after cmd has
// exited or been cancelled.
func Group(cmd *exec.Cmd) {
	ownGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
}
