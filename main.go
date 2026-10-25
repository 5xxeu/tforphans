// Command tforphans finds live infrastructure that no Terraform state
// manages.
//
// For every root module it lists live resources of each list-capable
// resource type with `terraform query` (Terraform 1.14+), using the module's
// own provider configuration, and compares their resource identities with
// the module's state plus any extra states. Providers without list support
// are covered by plugins: commands that print the same data as JSON lines.
package main

import "github.com/5xxeu/tforphans/internal/cli"

// version is set by release builds (-X main.version).
var version = ""

func main() {
	cli.Main(version)
}
