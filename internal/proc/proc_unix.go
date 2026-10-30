//go:build unix

package proc

import (
	"os/exec"
	"syscall"
)

// ownGroup runs the command in its own process group and has cancellation
// kill the whole group: killing only the direct child would leave its
// children (sleep | cat) running and holding stdout open.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// CanTellAlive reports whether Alive can see that a process has exited.
const CanTellAlive = true

// Alive reports whether a process with this ID exists.
func Alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
