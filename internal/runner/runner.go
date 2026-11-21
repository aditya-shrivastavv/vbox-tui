// Package runner builds external commands safely: argument arrays only, bounded
// runtimes, captured output and no stray console windows on Windows.
package runner

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// MaxCapture bounds how much of one stream a helper retains.
const MaxCapture = 512 << 10

// Visible caps how much of a captured stream goes into an error message.
const Visible = 4000

// DefaultTimeout is the bound applied to ad-hoc commands.
const DefaultTimeout = 15 * time.Second

// Runner creates external commands. Injecting it lets the TUI hand a command to
// Bubble Tea's process mechanism instead of running it directly.
type Runner interface {
	// Command returns an unstarted, non-interactive command: stdin closed,
	// output meant to be captured, and no console window of its own.
	Command(ctx context.Context, name string, args ...string) (*exec.Cmd, error)
	// Interactive returns an unstarted command that is meant to own the
	// terminal. It deliberately applies no window attributes: child stdio must
	// stay attached to the caller's console so password prompts, host-key
	// confirmation and Ctrl-C keep working.
	Interactive(ctx context.Context, name string, args ...string) (*exec.Cmd, error)
}

// CLI is the default Runner.
type CLI struct{}

// New returns the default runner.
func New() Runner { return CLI{} }

// LookPath resolves a program name, refusing an empty one.
func LookPath(name string) (string, error) {
	if name == "" {
		return "", errors.New("empty program name")
	}
	return exec.LookPath(name)
}

func (CLI) build(ctx context.Context, name string, args []string) (*exec.Cmd, error) {
	path, err := LookPath(name)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	// CommandContext takes Args[0] from the name argument; pin it to the
	// resolved path so a quoted path containing spaces is passed through intact.
	cmd.Args = append([]string{path}, args...)
	return cmd, nil
}

// Command implements Runner.
func (c CLI) Command(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
	cmd, err := c.build(ctx, name, args)
	if err != nil {
		return nil, err
	}
	cmd.Stdin = nil
	suppressWindow(cmd)
	return cmd, nil
}

// Interactive implements Runner.
func (CLI) Interactive(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
	return CLI{}.build(ctx, name, args)
}

// Collect runs a command to completion and returns its stdout, converting a
// non-zero exit into an error that carries the trimmed stderr.
func Collect(ctx context.Context, r Runner, name string, args ...string) (string, error) {
	cmd, err := r.Command(ctx, name, args...)
	if err != nil {
		return "", err
	}
	// A bounded writer means a chatty or looping child cannot grow memory without
	// limit; the remainder is discarded.
	stdout := &bounded{limit: MaxCapture}
	stderr := &bounded{limit: MaxCapture}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err = cmd.Run()
	if ctxErr := ctx.Err(); err != nil && ctxErr != nil {
		return stdout.String(), &Error{Args: cmd.Args, Err: errors.Join(err, ctxErr), Stderr: stderr.String(), TimedOut: true}
	}
	if err != nil {
		exit := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		}
		return stdout.String(), &Error{Args: cmd.Args, ExitCode: exit, Err: err, Stderr: stderr.String()}
	}
	return stdout.String(), nil
}

// Error is a failed external command.
type Error struct {
	Args     []string
	ExitCode int
	Stderr   string
	Err      error
	TimedOut bool
}

func (e *Error) Error() string {
	var b strings.Builder
	if len(e.Args) > 0 {
		b.WriteString(strings.Join(e.Args, " "))
	} else {
		b.WriteString("command")
	}
	switch {
	case e.TimedOut:
		b.WriteString(": timed out")
	case e.ExitCode >= 0:
		fmt.Fprintf(&b, ": exited with code %d", e.ExitCode)
	default:
		b.WriteString(": could not be started")
	}
	if msg := strings.TrimSpace(e.Stderr); msg != "" {
		b.WriteString("\noutput: " + Trim(msg))
	}
	return b.String()
}

// Unwrap exposes the underlying error, including context errors.
func (e *Error) Unwrap() error { return e.Err }

// Trim shortens text for display without splitting a UTF-8 sequence.
func Trim(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= Visible {
		return s
	}
	return string(r[:Visible]) + "…"
}

// bounded collects a child's output up to a fixed byte limit. Without it a
// misbehaving or looping child process could grow the parent's memory without
// bound. Bytes past the limit are counted but dropped, and truncated records
// that so the caller can tell output was cut.
type bounded struct {
	limit      int
	buf        strings.Builder
	overflowed bool
}

func (b *bounded) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		b.limit = MaxCapture
	}
	room := b.limit - b.buf.Len()
	if room <= 0 {
		b.overflowed = true
		return len(p), nil
	}
	if len(p) > room {
		b.buf.Write(p[:room])
		b.overflowed = true
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *bounded) String() string {
	s := b.buf.String()
	if b.overflowed {
		s += "\n… output truncated"
	}
	return s
}
