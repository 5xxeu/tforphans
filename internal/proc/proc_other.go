//go:build !unix

package proc

import "os/exec"

// ownGroup is a no-op where process groups are not available; WaitDelay
// still keeps a cancelled run from waiting for leftover children.
func ownGroup(*exec.Cmd) {}

// Alive cannot tell here and assumes the process exists.
func Alive(int) bool { return true }
