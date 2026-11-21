package ssh

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aditya-shrivastavv/vbox-tui/internal/config"
	"github.com/aditya-shrivastavv/vbox-tui/internal/runner"
)

// Restart over SSH runs exactly one reboot command per guest family, chosen
// from the fixed table below. vbox-tui never builds a remote command from VM
// names, user input, or anything else that varies, and it never sends a
// generic "do whatever reboots this thing" probe. Authentication belongs to
// OpenSSH entirely: keys and agents work, an interactive password prompt does
// not exist for this non-interactive call, and that is a documented
// prerequisite rather than a silent limitation.

// RestartTimeout bounds one remote reboot attempt. A reboot either takes the
// connection with it or errors, both within moments; a minute means something
// is wedged.
const RestartTimeout = 60 * time.Second

// ErrNoRestartCommand means the guest family has no fixed reboot command
// (unknown OS), so the SSH route must not be attempted at all.
var ErrNoRestartCommand = errors.New("no restart command is defined for this guest OS")

// Fixed reboot commands, by guest family. These are the only strings this
// package will ever place on a remote command line. The linux list is tried
// in order: sudo -n succeeds when the user may reboot without a password,
// and plain reboot covers root sessions. Neither prompts, so nothing can
// hang waiting for a terminal this call does not have.
var restartCommands = map[config.GuestOS][]string{
	config.GuestOSLinux:   {"sudo -n reboot", "reboot"},
	config.GuestOSWindows: {`shutdown /r /t 0`},
}

// RestartCommands returns the fixed reboot commands for a guest family, in
// attempt order. An unknown family has none: refusing is safer than guessing
// which binary reboots this machine.
func RestartCommands(guestOS config.GuestOS) ([]string, error) {
	cmds, ok := restartCommands[guestOS]
	if !ok || len(cmds) == 0 {
		return nil, fmt.Errorf("%w (%s)", ErrNoRestartCommand, describeGuestOS(guestOS))
	}
	return append([]string(nil), cmds...), nil
}

func describeGuestOS(guestOS config.GuestOS) string {
	if guestOS == config.GuestOSUnknown {
		return "not set"
	}
	return string(guestOS)
}

// RestartPreflight reports whether a restart over SSH could be attempted at
// all, naming the missing prerequisite. It changes nothing.
func RestartPreflight(s config.SSHConfig, guestOS config.GuestOS) error {
	if !s.Configured() {
		return errors.New("SSH settings for this VM are incomplete: configure them first (vbx config)")
	}
	if _, err := RestartCommands(guestOS); err != nil {
		return fmt.Errorf("%w: set the guest OS in the VM's config (vbx config)", err)
	}
	return nil
}

// BuildRestartArgs returns the ssh(1) argument vector that runs one fixed
// reboot command on the guest. The command is the last positional element,
// after the "--" that ends option parsing and after the destination, so a
// remote command can never be mistaken for an option, and it always comes
// from the fixed table, never from interpolation.
func BuildRestartArgs(s config.SSHConfig, command string) ([]string, error) {
	if !isFixedRestartCommand(command) {
		return nil, fmt.Errorf("refusing to run %q: not a known restart command", command)
	}
	args, err := BuildArgs(s)
	if err != nil {
		return nil, err
	}
	return append(args, command), nil
}

func isFixedRestartCommand(command string) bool {
	for _, cmds := range restartCommands {
		for _, c := range cmds {
			if c == command {
				return true
			}
		}
	}
	return false
}

// BuildRestartSpec resolves the client and one fixed reboot command into a
// runnable (non-interactive) command line.
func BuildRestartSpec(exe string, s config.SSHConfig, command string) (ExecSpec, error) {
	if exe == "" {
		exe = ClientExe
	}
	args, err := BuildRestartArgs(s, command)
	if err != nil {
		return ExecSpec{}, err
	}
	path, lookErr := runner.LookPath(exe)
	if lookErr != nil {
		return ExecSpec{Args: args}, fmt.Errorf("%w (looked for %q on PATH)", ErrNoOpenSSH, exe)
	}
	return ExecSpec{
		Path: path,
		Args: args,
		// The label names only fixed parts: the command is from the table and
		// the destination comes from the user's own stored settings.
		Label: strings.Join(append([]string{exe}, args...), " "),
	}, nil
}

// RestartOutcome is what one SSH restart attempt ended as. The distinction
// matters: a reboot almost always drops the connection before OpenSSH can
// report an exit status, and claiming more certainty than the wire gave us
// would be a lie.
type RestartOutcome int

const (
	// RestartFailed means the guest refused or could not be reached; nothing
	// indicates a reboot started.
	RestartFailed RestartOutcome = iota
	// RestartVerified means the reboot command exited successfully, so the
	// guest OS accepted it.
	RestartVerified
	// RestartUncertain means the connection dropped mid-command, which is
	// what a rebooting guest looks like from outside, but a network failure
	// looks the same and cannot be told apart honestly.
	RestartUncertain
)

func (o RestartOutcome) String() string {
	switch o {
	case RestartVerified:
		return "verified"
	case RestartUncertain:
		return "uncertain"
	}
	return "failed"
}

// RestartResult reports the end of a full attempt (each fixed command gets
// one try, in order, until one is not a clean refusal).
type RestartResult struct {
	Outcome RestartOutcome
	// Command is the table entry that produced the outcome.
	Command string
	// Err explains a failure or an uncertain disconnect.
	Err error
}

// Summary is the honest one-line outcome for the result screen.
func (r RestartResult) Summary(name string) string {
	switch r.Outcome {
	case RestartVerified:
		return fmt.Sprintf("Restart requested for %s: the guest confirmed the reboot command.", name)
	case RestartUncertain:
		return fmt.Sprintf("Restart requested for %s, but the connection dropped before the guest confirmed. Check its console to be sure.", name)
	}
	why := "unknown reason"
	if r.Err != nil {
		why = r.Err.Error()
	}
	return fmt.Sprintf("Restart of %s failed: %s", name, why)
}

// collectFunc matches runner.Collect; tests inject a scripted version.
type collectFunc func(ctx context.Context, r runner.Runner, name string, args ...string) (string, error)

// RunRestart reaches the guest over SSH and runs its fixed reboot commands in
// table order. A successful or connection-dropped attempt stops the sequence
// immediately (the machine is going, or gone, down); only a clean refusal
// (bad sudo rights, missing binary) tries the next entry. It never falls back
// to a power cycle or a reset.
func RunRestart(ctx context.Context, r runner.Runner, exe string, s config.SSHConfig, guestOS config.GuestOS) RestartResult {
	return runRestart(ctx, r, exe, s, guestOS, runner.Collect)
}

func runRestart(ctx context.Context, r runner.Runner, exe string, s config.SSHConfig, guestOS config.GuestOS, collect collectFunc) RestartResult {
	if err := RestartPreflight(s, guestOS); err != nil {
		return RestartResult{Err: err}
	}
	if r == nil {
		r = runner.New()
	}
	cmds, _ := RestartCommands(guestOS)
	var lastErr error
	for _, command := range cmds {
		args, err := BuildRestartArgs(s, command)
		if err != nil {
			return RestartResult{Err: err}
		}
		if exe == "" {
			exe = ClientExe
		}
		runCtx, cancel := context.WithTimeout(ctx, RestartTimeout)
		out, runErr := collect(runCtx, r, exe, args...)
		cancel()
		outcome, classified := classifyRestart(runErr)
		switch outcome {
		case RestartVerified:
			return RestartResult{Outcome: RestartVerified, Command: command}
		case RestartUncertain:
			// The connection went away mid-command. Do not poke a machine
			// that may already be going down.
			return RestartResult{Outcome: RestartUncertain, Command: command, Err: classified}
		default:
			// Clean refusal: report which command was refused and try the
			// next table entry if there is one.
			detail := runErr
			if detail == nil {
				detail = classified
			}
			lastErr = fmt.Errorf("%s: %w", command, trimRestartOutput(detail, out))
		}
	}
	return RestartResult{Outcome: RestartFailed, Err: lastErr}
}

// trimRestartOutput keeps the guest's own explanation short enough for a
// result line.
func trimRestartOutput(err error, stdout string) error {
	msg := strings.TrimSpace(stdout)
	if e := asSSHError(err); e != "" {
		if msg != "" {
			msg += "; "
		}
		msg += e
	}
	if msg == "" {
		if err != nil {
			return err
		}
		return errors.New("the command reported nothing")
	}
	r := []rune(msg)
	if len(r) > 300 {
		msg = string(r[:300]) + "…"
	}
	return errors.New(msg)
}

func asSSHError(err error) string {
	e, ok := err.(*runner.Error)
	if !ok {
		return ""
	}
	return strings.TrimSpace(e.Stderr)
}

// classifyRestart maps an OpenSSH run's error to an outcome.
//
// Exit status is OpenSSH's own convention: 0 means the remote command
// completed; 255 means ssh itself failed, which includes "the connection
// dropped before I could report the remote exit status" — the situation a
// reboot causes. Any other status is the guest command refusing (sudo needs
// a password, permission denied, binary not found), a genuine failure.
func classifyRestart(err error) (RestartOutcome, error) {
	if err == nil {
		return RestartVerified, nil
	}
	cmdErr, ok := err.(*runner.Error)
	if !ok {
		// The client could not even be started by the runner.
		return RestartFailed, err
	}
	switch {
	case cmdErr.TimedOut:
		// A wedged session that outlived the bounded wait says nothing about
		// whether the guest rebooted.
		return RestartUncertain, errors.New("the session neither completed nor dropped within the time limit")
	case cmdErr.ExitCode == 255:
		text := strings.ToLower(cmdErr.Stderr)
		if hasFailureMarker(text) {
			return RestartFailed, cmdErr
		}
		if hasDropMarker(text) {
			return RestartUncertain, errors.New("the connection dropped while the reboot command was running")
		}
		// 255 with unrecognised output: say what ssh said rather than guess.
		return RestartFailed, cmdErr
	default:
		return RestartFailed, cmdErr
	}
}

// failureMarkers are OpenSSH outputs that can only mean "this attempt never
// reached a running guest": authentication, routing, name resolution or host
// key trouble. None of them can be the sound of a machine rebooting.
var failureMarkers = []string{
	"permission denied",
	"host key verification failed",
	"host key for",
	"no route to host",
	"connection refused",
	"name or service not known",
	"could not resolve hostname",
	"timed out",
	"too many authentication failures",
	"no supported authentication",
	"operation not supported by network protocol",
}

// dropMarkers are outputs that describe an established connection going away,
// which is what a rebooting guest does mid-command.
var dropMarkers = []string{
	"connection reset",
	"connection closed",
	"closed by remote host",
	"received disconnect",
	"client_loop: send disconnect",
	"broken pipe",
}

func hasFailureMarker(lower string) bool {
	for _, m := range failureMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

func hasDropMarker(lower string) bool {
	for _, m := range dropMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}
