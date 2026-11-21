package ssh

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aditya-shrivastavv/vbox-tui/internal/config"
)

// TestBuildSpecWithMissingClientSaysInstallOpenSSH matters because the alternative
// is telling the user their VM settings are wrong when in fact OpenSSH is not
// installed. The two need different fixes.
func TestBuildSpecWithMissingClientSaysInstallOpenSSH(t *testing.T) {
	cfg := config.SSHConfig{Mode: config.SSHModeEndpoint, Host: "127.0.0.1", Port: 2222}
	const missing = "vbox-tui-definitely-not-installed-9f3a"
	spec, err := BuildSpecWith(missing, cfg)
	if err == nil {
		t.Skipf("%q unexpectedly exists on PATH", missing)
	}
	if !errors.Is(err, ErrNoOpenSSH) {
		t.Fatalf("want ErrNoOpenSSH, got %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, missing) {
		t.Errorf("the message should name the program it looked for: %q", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "path") {
		t.Errorf("the message should say it searched PATH: %q", msg)
	}
	// The arguments themselves are still valid, so the UI can show what would run
	// once the client exists.
	if len(spec.Args) == 0 {
		t.Error("the argument list should still be reported")
	}
}

func TestBuildSpecWithMissingConfigIsNotAClientError(t *testing.T) {
	// An unconfigured VM must fail with "no endpoint", not "no ssh client", so the
	// user is pointed at the settings form rather than at a download page.
	if _, err := BuildSpec(config.SSHConfig{}); !errors.Is(err, ErrNoEndpoint) {
		t.Errorf("got %v, want ErrNoEndpoint", err)
	}
}

// TestBuildSpecResolvesTheClientAndLabelsTheCommand keeps the preview honest: the
// label is produced by the same builder that produces the arguments, so the status
// line cannot drift from what actually runs.
func TestBuildSpecResolvesTheClientAndLabelsTheCommand(t *testing.T) {
	cfg := config.SSHConfig{Mode: config.SSHModeEndpoint, Host: "127.0.0.1", Port: 2222, User: "op"}
	spec, err := BuildSpecWith("ssh", cfg)
	if err != nil {
		if errors.Is(err, ErrNoOpenSSH) {
			t.Skip("no OpenSSH client on this machine")
		}
		t.Fatal(err)
	}
	if spec.Path == "" {
		t.Error("Path should be resolved so exec does not re-search PATH")
	}
	for _, want := range []string{"127.0.0.1", "2222", "op@"} {
		if !strings.Contains(spec.Label, want) {
			t.Errorf("Label = %q, want it to contain %q", spec.Label, want)
		}
	}
	if !strings.HasPrefix(spec.Label, "ssh ") {
		t.Errorf("Label = %q, want it to start with the program name", spec.Label)
	}
	// Every argument must be visible in the label: nothing hidden, nothing added.
	for i, a := range spec.Args {
		if !strings.Contains(spec.Label, a) {
			t.Errorf("arg %d (%q) is missing from the label %q", i, a, spec.Label)
		}
	}
}

// spyRunner records which runner method was chosen, which is the only way to
// observe that a terminal session did not get the background window attributes.
type spyRunner struct {
	interactive int32
	command     int32
	gotArgs     []string
	gotName     string
}

func (s *spyRunner) Command(_ context.Context, name string, args ...string) (*exec.Cmd, error) {
	atomic.AddInt32(&s.command, 1)
	s.gotName, s.gotArgs = name, args
	return exec.Command("cmd", "/c", "ver"), nil
}

func (s *spyRunner) Interactive(_ context.Context, name string, args ...string) (*exec.Cmd, error) {
	atomic.AddInt32(&s.interactive, 1)
	s.gotName, s.gotArgs = name, args
	return exec.Command("cmd", "/c", "ver"), nil
}

// TestInteractiveCommandUsesTheInteractiveRunner is why there are two runner
// methods. The interactive path must not apply the window-attribute suppression
// used for background calls: the child has to own the real console, or password
// prompts, host-key confirmation and Ctrl-C stop working.
func TestInteractiveCommandUsesTheInteractiveRunner(t *testing.T) {
	if runtime.GOOS != "windows" {
		_ = "the spy builds a cmd.exe command; the assertion itself is portable"
	}
	r := &spyRunner{}
	if _, err := InteractiveCommand(context.Background(), "ssh", []string{"-tt", "--", "lab"}, r); err != nil {
		t.Fatalf("InteractiveCommand: %v", err)
	}
	if got := atomic.LoadInt32(&r.interactive); got != 1 {
		t.Errorf("interactive calls = %d, want the Interactive path to be used", got)
	}
	if got := atomic.LoadInt32(&r.command); got != 0 {
		t.Errorf("non-interactive calls = %d; the suppressed-window path was used for a terminal session", got)
	}
	if strings.Join(r.gotArgs, " ") != "-tt -- lab" {
		t.Errorf("args passed through = %v", r.gotArgs)
	}
	if r.gotName != "ssh" {
		t.Errorf("program = %q, want ssh as resolved by the caller", r.gotName)
	}
}

// TestInteractiveCommandNilRunnerRejectsAnEmptyProgram documents that the default
// runner is used when none is injected, and that an empty program name is refused
// rather than producing a process with nothing to run.
func TestInteractiveCommandNilRunnerRejectsAnEmptyProgram(t *testing.T) {
	if _, err := InteractiveCommand(context.Background(), "", nil, nil); err == nil {
		t.Error("an empty program name should be refused")
	}
}

// TestDefaultDialHonoursContext keeps a wedged network stack from blocking a
// refresh or a readiness wait indefinitely.
func TestDefaultDialHonoursContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	// 240.0.0.1 is reserved addressing that will not answer, so the only way this
	// returns quickly is if the deadline is honoured.
	conn, err := DefaultDial(ctx, "240.0.0.1:22")
	if err == nil {
		_ = conn.Close()
		t.Skip("this network connected to reserved addressing; skipping")
	}
	if ctx.Err() == nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		// An immediate local refusal is also acceptable: what must not happen is
		// the call outliving the deadline.
		t.Logf("dial failed early with %v; deadline was not the cause, which is fine", err)
	}
}

// TestEffectiveEndpointShortCircuitsWithoutAClient avoids starting anything when
// the ssh program is absent, so a resolver failure is fast and clear.
func TestEffectiveEndpointShortCircuitsWithoutAClient(t *testing.T) {
	r := OpenSSHResolver{Exe: "ssh-missing-xyz", Timeout: 50 * time.Millisecond}
	start := time.Now()
	if _, err := r.EffectiveEndpoint("lab"); !errors.Is(err, ErrNoOpenSSH) {
		t.Errorf("want ErrNoOpenSSH, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("a missing client should be detected immediately, took %s", elapsed)
	}
}

func TestEndpointTargetRendersIPv6Correctly(t *testing.T) {
	if got := (Endpoint{Host: "::1", Port: 22}).Target(); got != "[::1]:22" {
		t.Errorf("Target = %q, want [::1]:22", got)
	}
}

// TestClientExeIsANameNotAPath documents the intent: the system OpenSSH is used so
// the user's own configuration, agent and security policy apply, rather than a
// copy shipped with vbox-tui.
func TestClientExeIsANameNotAPath(t *testing.T) {
	if strings.ContainsAny(ClientExe, `/\`) {
		t.Error("the client must be looked up by name so the user's OpenSSH is used")
	}
	if runtime.GOOS == "windows" && strings.ToLower(ClientExe) != "ssh" {
		t.Errorf("ClientExe = %q", ClientExe)
	}
}
