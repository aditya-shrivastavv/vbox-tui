//go:build !windows

package main

// termWidth reports no console width off Windows; the model waits for the
// program's own WindowSizeMsg instead.
func termWidth() int { return 0 }
