//go:build unix

package tf

import (
	"os"
	"syscall"
)

// forwardedSignals are passed from the wrapper to terraform.
var forwardedSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// wrapTerragrunt: terragrunt runs terraform through tforphans, which waits
// for the init turn only around init. A cancelled read kills the whole
// process group, the wrapper included, so a held turn always comes back.
const wrapTerragrunt = true
