//go:build windows

package runner

import (
	"os/exec"
	"syscall"
)

// suppressWindow stops a background child process from flashing its own console
// window when vbox-tui runs detached from a console of its own.
//
// It sets HideWindow only. That suppresses window creation and nothing else, so
// stdio still points at the parent terminal. Creating a new process group, or
// setting creation flags such as DETACHED_PROCESS, would break keyboard input
// for the interactive SSH session, so interactive commands skip this function
// entirely (see Runner.Interactive).
func suppressWindow(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
}
