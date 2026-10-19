//go:build !unix

package tf

import "os"

// forwardedSignals are passed from the wrapper to terraform.
var forwardedSignals = []os.Signal{os.Interrupt}

// wrapTerragrunt: a cancel kills only terragrunt here, not the wrapper and
// terraform below it, which would keep the init turn. Each terragrunt read
// takes the turn as a whole instead (units are read one at a time).
const wrapTerragrunt = false
