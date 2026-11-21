//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// termWidth asks the console how wide it is so the prompt can ellipsise rows
// before the first frame. On any failure it returns 0 and the model falls back
// to its default width; the real size still arrives as a WindowSizeMsg.
func termWidth() int {
	handle := windows.Handle(os.Stdout.Fd())
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(handle, &info); err != nil {
		return 0
	}
	w := int(info.Window.Right-info.Window.Left) + 1
	if w <= 0 {
		return 0
	}
	return w
}
