// Command vbx is the primary entry point: a compact inline prompt for the
// VirtualBox machines you already have. Everything of substance lives in
// internal/cli; this file exists so the binary has a home and the two command
// names (vbx and the legacy vbox-tui) cannot drift apart.
//
// The tool is an inline terminal UI: it never leaves the alternate screen,
// prints its result after each action, and does not keep its own scrollback.
//
// Example invocations:
//
//	vbx                                # pick VMs, pick an action, done
//	vbx start "My VM" "Other"          # direct batch: start both, one summary
//	vbx ssh "My VM"                    # direct: connect over SSH
//	vbx config "My VM"                 # edit that VM's saved SSH/restart settings
//	vbx snapshots "My VM"              # list a snapshot tree
//	vbx --demo                         # exercise every screen against a fake
package main

import (
	"os"

	"github.com/aditya-shrivastavv/vbox-tui/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
