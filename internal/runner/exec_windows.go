//go:build windows

package runner

import (
	"os/exec"
	"syscall"
)

// Prepare stops a background child process from flashing its own console window
// when the parent has no console of its own. Interactive SSH is started by the
// UI framework with its own rules and must keep using the terminal, which is why
// this is deliberately limited: HideWindow only suppresses window creation, it
// does not detach stdio, so ssh still talks to the terminal it was given.
func (CLI) Prepare(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	// Creating a new process group would break keyboard input for ssh, so
	// CreationFlags is left untouched apart from the hide-window behaviour that
	// HideWindow already implies.
}
