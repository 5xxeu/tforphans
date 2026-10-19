//go:build unix

package tf

import (
	"os"
	"syscall"
)

// forwardedSignals are passed from the wrapper to terraform.
var forwardedSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}
