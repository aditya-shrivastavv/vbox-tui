// Command vbox-tui is the deprecated compatibility name for vbx. Everything
// here is a thin wrapper around internal/cli: the product itself and its
// configuration format are unchanged; only the preferred entry point has
// moved to cmd/vbx. Prefer building cmd/vbx.
//
// The tool is an inline terminal UI: it never leaves the alternate screen,
// prints its result after each action, and does not keep its own scrollback.
//
// Example invocations (identical to vbx):
//
//	vbox-tui                          # pick a VM, pick an action, done
//	vbox-tui --start "exact name"     # direct: start headless, print one line
//	vbox-tui --ssh "exact name"       # direct: connect over SSH
//	vbox-tui --demo                   # exercise every screen against a fake
package main

import (
	"os"

	"github.com/aditya-shrivastavv/vbox-tui/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
