//go:build !unix

package proc

import "os/exec"

// ownGroup keeps direct-child cancellation where groups are unavailable.
func ownGroup(*exec.Cmd) {}

// Alive cannot tell here and assumes the process exists.
func Alive(int) bool { return true }

// CanTellAlive reports whether Alive can see that a process has exited.
const CanTellAlive = false
