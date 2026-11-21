package ssh

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/aditya-shrivastavv/vbox-tui/internal/config"
	"github.com/aditya-shrivastavv/vbox-tui/internal/runner"
)

// scriptedCollect stands in for runner.Collect: each call consumes one
// scripted outcome and records the exact argument vector the restart routine
// built. That is how the tests prove nothing beyond the fixed table ever
// reaches a command line, and how "stop on the first non-refusal" is checked.
type scriptedCollect struct {
	results []runnerResult
	calls   [][]string
}

type runnerResult struct {
	stdout string
	err    error
}

func (sc *scriptedCollect) collect(_ context.Context, _ runner.Runner, name string, args ...string) (string, error) {
	sc.calls = append(sc.calls, append([]string{name}, args...))
	if len(sc.calls) > len(sc.results) {
		return "", errors.New("the restart routine made more attempts than scripted")
	}
	res := sc.results[len(sc.calls)-1]
	return res.stdout, res.err
}

// noopRunner satisfies the Runner door; scriptedCollect answers instead.
type noopRunner struct{}

func (noopRunner) Command(_ context.Context, _ string, _ ...string) (*exec.Cmd, error) {
	return nil, errors.New("noop runner must not start commands")
}
func (noopRunner) Interactive(_ context.Context, _ string, _ ...string) (*exec.Cmd, error) {
	return nil, errors.New("noop runner must not start commands")
}

func exitErr(code int, stderr string) error {
	return &runner.Error{
		Args:     []string{"ssh"},
		ExitCode: code,
		Stderr:   stderr,
		Err:      &exec.ExitError{},
	}
}

// TestRestartCommandsTableIsFixed locks the security rule: the remote command
// is one of a tiny set of known strings. Anything else, including anything
// that mentions a VM name, must never be runnable.
func TestRestartCommandsTableIsFixed(t *testing.T) {
	linux, err := RestartCommands(config.GuestOSLinux)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sudo -n reboot", "reboot"}
	if strings.Join(linux, "|") != strings.Join(want, "|") {
		t.Errorf("linux commands = %v, want %v", linux, want)
	}
	windows, err := RestartCommands(config.GuestOSWindows)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(windows, "|") != `shutdown /r /t 0` {
		t.Errorf("windows commands = %v", windows)
	}
	// The returned slice is a copy: mutating it cannot poison the table.
	linux[0] = "rm -rf /"
	again, _ := RestartCommands(config.GuestOSLinux)
	if again[0] == "rm -rf /" {
		t.Error("RestartCommands handed out its internal table")
	}
	if _, err := RestartCommands(config.GuestOSUnknown); !errors.Is(err, ErrNoRestartCommand) {
		t.Errorf("unknown OS should refuse: %v", err)
	}
}

// TestBuildRestartArgsAppendsOnlyTableCommands proves the interpolation rule
// in both directions: the table entry is the last positional argument after
// the destination, and a command from outside the table is refused.
func TestBuildRestartArgsAppendsOnlyTableCommands(t *testing.T) {
	cfg := config.SSHConfig{Mode: config.SSHModeEndpoint, Host: "127.0.0.1", Port: 2222, User: "kali"}
	args, err := BuildRestartArgs(cfg, "sudo -n reboot")
	if err != nil {
		t.Fatal(err)
	}
	if args[len(args)-1] != "sudo -n reboot" {
		t.Fatalf("last argument should be the command: %q", args)
	}
	// The command sits after the "--" separator: ssh reads it as a remote
	// command, never as one of its own options.
	for i, a := range args {
		if a == "--" && i > len(args)-3 {
			t.Errorf("the -- must come before user data, not beside the command: %q", args)
		}
	}
	if _, err := BuildRestartArgs(cfg, "reboot; rm -rf /"); err == nil {
		t.Error("a command outside the table must be refused")
	}
	if _, err := BuildRestartArgs(config.SSHConfig{}, "reboot"); err == nil {
		t.Error("an unconfigured endpoint must be refused")
	}
}

// TestRestartPreflightNamesEachMissingPrerequisite: a restart attempt must
// say what to fix instead of guessing a workaround.
func TestRestartPreflightNamesEachMissingPrerequisite(t *testing.T) {
	if err := RestartPreflight(config.SSHConfig{}, config.GuestOSLinux); err == nil ||
		!strings.Contains(err.Error(), "SSH") {
		t.Errorf("missing SSH should be named: %v", err)
	}
	cfg := config.SSHConfig{Mode: config.SSHModeEndpoint, Host: "h", Port: 22}
	if err := RestartPreflight(cfg, config.GuestOSUnknown); err == nil ||
		!strings.Contains(err.Error(), "guest OS") {
		t.Errorf("missing guest OS should be named: %v", err)
	}
	if err := RestartPreflight(cfg, config.GuestOSWindows); err != nil {
		t.Errorf("a fully configured route should pass preflight: %v", err)
	}
}

func TestClassifyRestart(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want RestartOutcome
	}{
		{"clean exit", nil, RestartVerified},
		{"connection reset", exitErr(255, "Read from remote host 127.0.0.1: Connection reset by peer"), RestartUncertain},
		{"disconnect", exitErr(255, "Received disconnect from 127.0.0.1 port 2222:2: Client Disconnect"), RestartUncertain},
		{"closed by remote", exitErr(255, "Connection to 127.0.0.1 closed by remote host."), RestartUncertain},
		{"client loop", exitErr(255, "client_loop: send disconnect: Broken pipe"), RestartUncertain},
		{"auth refused", exitErr(255, "kali@127.0.0.1: Permission denied (publickey,password)."), RestartFailed},
		{"no route", exitErr(255, "ssh: connect to host 127.0.0.1 port 2222: No route to host"), RestartFailed},
		{"refused", exitErr(255, "ssh: connect to host 127.0.0.1 port 2222: Connection refused"), RestartFailed},
		{"host key", exitErr(255, "Host key verification failed."), RestartFailed},
		{"sudo asks", exitErr(1, "sudo: a password is required"), RestartFailed},
		{"not found", exitErr(127, "bash: reboot: command not found"), RestartFailed},
		{"timeout", &runner.Error{TimedOut: true, Err: context.DeadlineExceeded}, RestartUncertain},
		{"unknown 255", exitErr(255, "some completely unrecognised complaint"), RestartFailed},
		{"not an exec error", errors.New("the client binary vanished"), RestartFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := classifyRestart(tc.err)
			if got != tc.want {
				t.Errorf("classifyRestart(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

var labEndpoint = config.SSHConfig{Mode: config.SSHModeEndpoint, Host: "127.0.0.1", Port: 2222, User: "kali"}

// TestRunRestartLinuxTriesSudoThenPlain: a clean sudo refusal advances to the
// next table entry; the second tries exactly and only the plain command.
func TestRunRestartLinuxTriesSudoThenPlain(t *testing.T) {
	sc := &scriptedCollect{results: []runnerResult{
		{err: exitErr(1, "sudo: a password is required")},
		{}, // plain reboot: guest confirms
	}}
	res := runRestart(context.Background(), noopRunner{}, "ssh", labEndpoint, config.GuestOSLinux, sc.collect)
	if res.Outcome != RestartVerified {
		t.Fatalf("outcome = %v, want verified (%v)", res.Outcome, res.Err)
	}
	if res.Command != "reboot" {
		t.Errorf("command = %q", res.Command)
	}
	if len(sc.calls) != 2 {
		t.Fatalf("calls = %d, want sudo then plain reboot", len(sc.calls))
	}
	first, second := sc.calls[0], sc.calls[1]
	if !strings.Contains(strings.Join(first, " "), "sudo -n reboot") {
		t.Errorf("first attempt should be the sudo form: %v", first)
	}
	if !strings.HasSuffix(strings.Join(second, " "), "reboot") || strings.Contains(strings.Join(second, " "), "sudo") {
		t.Errorf("second attempt should be plain reboot: %v", second)
	}
}

// TestRunRestartStopsOnFirstNonRefusal: once the connection drops (the usual
// sound of a machine going down) there is no second poke.
func TestRunRestartStopsOnFirstNonRefusal(t *testing.T) {
	sc := &scriptedCollect{results: []runnerResult{
		{err: exitErr(255, "Read from remote host: Connection reset by peer")},
	}}
	res := runRestart(context.Background(), noopRunner{}, "ssh", labEndpoint, config.GuestOSLinux, sc.collect)
	if res.Outcome != RestartUncertain {
		t.Fatalf("outcome = %v, want uncertain", res.Outcome)
	}
	if len(sc.calls) != 1 {
		t.Errorf("a dropped connection must stop the sequence, got calls %v", sc.calls)
	}
}

// TestRunRestartWindowsRunsOnlyItsFixedCommand: one entry, no fallbacks.
func TestRunRestartWindowsRunsOnlyItsFixedCommand(t *testing.T) {
	sc := &scriptedCollect{results: []runnerResult{{err: exitErr(1, "Access is denied.")}}}
	res := runRestart(context.Background(), noopRunner{}, "ssh", labEndpoint, config.GuestOSWindows, sc.collect)
	if res.Outcome != RestartFailed {
		t.Fatalf("outcome = %v, want failed", res.Outcome)
	}
	if len(sc.calls) != 1 {
		t.Fatalf("calls = %v, want exactly one", sc.calls)
	}
	if !strings.Contains(strings.Join(sc.calls[0], " "), `shutdown /r /t 0`) {
		t.Errorf("command = %v", sc.calls[0])
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "Access is denied") {
		t.Errorf("the guest's own reason must survive: %v", res.Err)
	}
}

// TestRunRestartRefusesWithoutPrerequisites must not start anything at all.
func TestRunRestartRefusesWithoutPrerequisites(t *testing.T) {
	sc := &scriptedCollect{}
	res := runRestart(context.Background(), noopRunner{}, "ssh", config.SSHConfig{}, config.GuestOSLinux, sc.collect)
	if res.Outcome != RestartFailed || len(sc.calls) != 0 {
		t.Errorf("unconfigured SSH must fail without commands: %+v calls=%v", res, sc.calls)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "SSH") {
		t.Errorf("the summary should name the missing prerequisite: %v", res.Err)
	}
	res = runRestart(context.Background(), noopRunner{}, "ssh", labEndpoint, config.GuestOSUnknown, sc.collect)
	if res.Outcome != RestartFailed || len(sc.calls) != 0 {
		t.Errorf("unknown guest OS must fail without commands: %+v", res)
	}
}

// TestRunRestartAllAttemptsFailKeepsLastReason: when every table entry is
// refused, the user hears the last refusal, not nothing.
func TestRunRestartAllAttemptsFailKeepsLastReason(t *testing.T) {
	sc := &scriptedCollect{results: []runnerResult{
		{err: exitErr(1, "sudo: a password is required")},
		{err: exitErr(127, "bash: reboot: command not found"), stdout: "some note"},
	}}
	res := runRestart(context.Background(), noopRunner{}, "ssh", labEndpoint, config.GuestOSLinux, sc.collect)
	if res.Outcome != RestartFailed {
		t.Fatalf("outcome = %v, want failed", res.Outcome)
	}
	if len(sc.calls) != 2 {
		t.Errorf("calls = %v, want both table entries tried", sc.calls)
	}
	msg := res.Err.Error()
	if !strings.Contains(msg, "reboot") || !strings.Contains(msg, "command not found") {
		t.Errorf("last refusal should survive: %q", msg)
	}
}

// TestRunRestartSummaryWording locks the honest result lines: confirmed,
// uncertain and failed say exactly what is known.
func TestRunRestartSummaryWording(t *testing.T) {
	verified := RestartResult{Outcome: RestartVerified, Command: "reboot"}
	if s := verified.Summary("kali"); !strings.Contains(s, "confirmed") {
		t.Errorf("verified summary = %q", s)
	}
	uncertain := RestartResult{Outcome: RestartUncertain, Command: "reboot", Err: errors.New("dropped")}
	s := uncertain.Summary("kali")
	if !strings.Contains(s, "dropped before the guest confirmed") || !strings.Contains(s, "console") {
		t.Errorf("uncertain summary = %q", s)
	}
	failed := RestartResult{Outcome: RestartFailed, Err: errors.New("permission denied")}
	if s := failed.Summary("kali"); !strings.Contains(s, "failed") || !strings.Contains(s, "permission denied") {
		t.Errorf("failed summary = %q", s)
	}
}

// TestRestartOutcomeString completes coverage.
func TestRestartOutcomeString(t *testing.T) {
	for o, want := range map[RestartOutcome]string{
		RestartVerified:  "verified",
		RestartUncertain: "uncertain",
		RestartFailed:    "failed",
	} {
		if got := o.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", o, got, want)
		}
	}
}
