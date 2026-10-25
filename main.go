// Command tforphans finds live resources that no Terraform state manages.
package main

import "github.com/5xxeu/tforphans/internal/cli"

// version is set by release builds (-X main.version).
var version = ""

func main() {
	cli.Main(version)
}
