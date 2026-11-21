package vbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The lifecycle tests need a child process that prints chosen output and exits
// with a chosen code. Rather than depending on a shell, or on a compiler being on
// PATH, the test binary re-invokes itself: TestHelperProcess below is the child.
// That keeps the real exec path (argument arrays, pipes, exit statuses,
// Kill-on-context) under test while the "hypervisor" stays deterministic.
//
// The canned output travels in files, not environment variables, because VBoxManage
// output is multi-line and a Windows environment block is a poor place for that.

const (
	helperEnv      = "VBOX_TUI_HELPER_KIND"
	helperStdout   = "VBOX_TUI_HELPER_STDOUT_FILE"
	helperStderr   = "VBOX_TUI_HELPER_STDERR_FILE"
	helperExit     = "VBOX_TUI_HELPER_EXIT"
	helperSleep    = "VBOX_TUI_HELPER_SLEEP"
	helperSentinel = "child"
)

// TestHelperProcess is the fake VBoxManage. It is not a test: it only does
// something when the environment says it was started as a child.
func TestHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) != helperSentinel {
		return // an ordinary `go test` run of this package: nothing to do
	}
	write := func(envKey string, w *os.File) {
		path := os.Getenv(envKey)
		if path == "" {
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			// A missing fixture must be visible on the child's stream rather
			// than silently producing empty output.
			_, _ = w.WriteString("helper: cannot read " + path + ": " + err.Error() + "\n")
			return
		}
		_, _ = w.Write(data)
	}
	write(helperStdout, os.Stdout)
	write(helperStderr, os.Stderr)
	if s := os.Getenv(helperSleep); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			time.Sleep(d)
		}
	}
	os.Exit(exitFromEnv())
}

func exitFromEnv() int {
	n, err := strconv.Atoi(os.Getenv(helperExit))
	if err != nil {
		return 0
	}
	return n
}

// helper builds the child command.
func helper(ctx context.Context, fixtures fixtures) *exec.Cmd {
	args := []string{"-test.run=TestHelperProcess"}
	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	cmd.Env = append(os.Environ(), helperEnv+"="+helperSentinel)
	if fixtures.stdoutFile != "" {
		cmd.Env = append(cmd.Env, helperStdout+"="+fixtures.stdoutFile)
	}
	if fixtures.stderrFile != "" {
		cmd.Env = append(cmd.Env, helperStderr+"="+fixtures.stderrFile)
	}
	if fixtures.code != 0 {
		cmd.Env = append(cmd.Env, helperExit+"="+fixtures.codeText())
	}
	if fixtures.delay > 0 {
		cmd.Env = append(cmd.Env, helperSleep+"="+fixtures.delay.String())
	}
	return cmd
}

type fixtures struct {
	stdoutFile string
	stderrFile string
	code       int
	delay      time.Duration
}

func (f fixtures) codeText() string { return strconv.Itoa(f.code) }

// writeFixture stores canned output for the child to print.
func writeFixture(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	return p
}

// recordingRunner logs every argument vector it was asked to run, so a test can
// assert exactly what vbox-tui requested of VirtualBox.
type recordingRunner struct {
	fix fixtures

	mu    sync.Mutex
	calls [][]string
}

func (r *recordingRunner) Command(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string{name}, args...))
	r.mu.Unlock()
	return helper(ctx, r.fix), nil
}

func (r *recordingRunner) Interactive(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
	return r.Command(ctx, name, args...)
}

func (r *recordingRunner) recorded() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.calls))
	for i, c := range r.calls {
		out[i] = append([]string(nil), c...)
	}
	return out
}

func newCLIWithFixtures(t *testing.T, fix fixtures) (*CLI, *recordingRunner) {
	t.Helper()
	r := &recordingRunner{fix: fix}
	c := NewCLI(`C:\Program Files\Oracle\VirtualBox\VBoxManage.exe`)
	c.SetRunner(r)
	return c, r
}

// TestCLIStartUsesUUIDAndHeadless pins the exact argument vector for the most
// consequential action, and shows why arguments are passed as an array: the
// executable path itself contains a space and needs no quoting.
func TestCLIStartUsesUUIDAndHeadless(t *testing.T) {
	cli, r := newCLIWithFixtures(t, fixtures{})
	const uuid = "2d620512-4b10-45a8-b070-25435e5106a5"
	if err := cli.PowerOn(context.Background(), uuid); err != nil {
		t.Fatalf("PowerOn: %v", err)
	}
	calls := r.recorded()
	if len(calls) != 1 {
		t.Fatalf("want one invocation, got %d: %v", len(calls), calls)
	}
	want := []string{cli.Path(), "--nologo", "startvm", uuid, "--type", "headless"}
	if strings.Join(calls[0], "|") != strings.Join(want, "|") {
		t.Errorf("args = %v\nwant %v", calls[0], want)
	}
}

func TestCLILifecycleArgumentVectors(t *testing.T) {
	const uuid = "c907c20b-5a41-4dc2-99c5-0e9bc7773c38"
	tests := []struct {
		name string
		call func(*CLI) error
		want []string
	}{
		{
			name: "resume un-pauses",
			call: func(c *CLI) error { return c.Resume(context.Background(), uuid) },
			want: []string{"controlvm", uuid, "resume"},
		},
		{
			name: "graceful shutdown is the ACPI power button",
			call: func(c *CLI) error { return c.ShutdownGracefully(context.Background(), uuid) },
			want: []string{"controlvm", uuid, "acpipowerbutton"},
		},
		{
			name: "save state is suspend",
			call: func(c *CLI) error { return c.SaveState(context.Background(), uuid) },
			want: []string{"controlvm", uuid, "savestate"},
		},
		{
			name: "restore is the same request as start",
			call: func(c *CLI) error { return c.PowerOn(context.Background(), uuid) },
			want: []string{"startvm", uuid, "--type", "headless"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cli, r := newCLIWithFixtures(t, fixtures{})
			if err := tc.call(cli); err != nil {
				t.Fatalf("call: %v", err)
			}
			calls := r.recorded()
			if len(calls) != 1 {
				t.Fatalf("want exactly one VBoxManage call, got %d: %v", len(calls), calls)
			}
			want := append([]string{cli.Path(), "--nologo"}, tc.want...)
			if strings.Join(calls[0], "|") != strings.Join(want, "|") {
				t.Errorf("args = %v\nwant %v", calls[0], want)
			}
			for _, a := range calls[0] {
				// The whole point of the safety rule: no forced power off, ever.
				for _, forbidden := range []string{"poweroff", "discardstate", "unregistervm", "modifyvm", "deletevm"} {
					if strings.EqualFold(a, forbidden) {
						t.Errorf("%q was issued; this operation is out of scope and unsafe", a)
					}
				}
			}
		})
	}
}

// TestCLIRefusesEmptyUUID is the guard against addressing the wrong machine: an
// empty or whitespace UUID could make VBoxManage act on an unintended target.
func TestCLIRefusesEmptyUUID(t *testing.T) {
	cli, r := newCLIWithFixtures(t, fixtures{})
	ctx := context.Background()
	ops := map[string]func() error{
		"PowerOn":     func() error { return cli.PowerOn(ctx, "  ") },
		"Resume":      func() error { return cli.Resume(ctx, "") },
		"Shutdown":    func() error { return cli.ShutdownGracefully(ctx, "") },
		"SaveState":   func() error { return cli.SaveState(ctx, "") },
		"Detail":      func() error { _, err := cli.Detail(ctx, ""); return err },
		"RunningUUID": func() error { _, err := cli.RunningUUIDs(ctx); return err },
	}
	for name, fn := range ops {
		if name == "RunningUUID" {
			continue // not a mutation, so no UUID is needed
		}
		if err := fn(); err == nil {
			t.Errorf("%s accepted an empty UUID", name)
		}
	}
	if got := r.recorded(); len(got) != 0 {
		t.Errorf("a refused operation must not reach VBoxManage; recorded %v", got)
	}
}

// TestCLIListPassesMachineReadableFlags keeps --machinereadable where it is
// valid. It is not a global flag, so putting it on list vms would fail.
func TestCLIListPassesMachineReadableFlags(t *testing.T) {
	cli, r := newCLIWithFixtures(t, fixtures{
		stdoutFile: writeFixture(t, "detail.txt", showVminfoActual),
	})
	if _, err := cli.Detail(context.Background(), "2d620512-4b10-45a8-b070-25435e5106a5"); err != nil {
		t.Fatalf("Detail: %v", err)
	}
	calls := r.recorded()
	if len(calls) != 1 {
		t.Fatalf("want one call: %v", calls)
	}
	got := calls[0]
	wantSuffix := []string{"showvminfo", "2d620512-4b10-45a8-b070-25435e5106a5", "--machinereadable"}
	if strings.Join(got[len(got)-3:], "|") != strings.Join(wantSuffix, "|") {
		t.Errorf("args = %v, want them to end in %v", got, wantSuffix)
	}
}

func TestCLIErrorCarriesStderrAndExitCode(t *testing.T) {
	stderr := "VBoxManage.exe: error: The machine 'kali' is already locked for a session\r\n" +
		"VBoxManage.exe: error: Details: code E_FAIL (0x80004005), Console 1, main\r\n"
	cli, _ := newCLIWithFixtures(t, fixtures{stderrFile: writeFixture(t, "err.txt", stderr), code: 1})
	err := cli.PowerOn(context.Background(), "2d620512-4b10-45a8-b070-25435e5106a5")
	if err == nil {
		t.Fatal("expected an error")
	}
	var ce *CommandError
	if !errors.As(err, &ce) {
		t.Fatalf("want *CommandError, got %T", err)
	}
	if ce.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1", ce.ExitCode)
	}
	if !strings.Contains(ce.Stderr, "already locked") {
		t.Errorf("raw stderr should be preserved: %q", ce.Stderr)
	}
	msg := err.Error()
	for _, want := range []string{"startvm", "already locked"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message should name %q so the user knows what failed:\n%s", want, msg)
		}
	}
}

func TestCLIErrorMarksTimeout(t *testing.T) {
	cli, _ := newCLIWithFixtures(t, fixtures{
		stdoutFile: writeFixture(t, "out.txt", ""),
		delay:      3 * time.Second,
	})
	cli.SetTimeout(120 * time.Millisecond)
	start := time.Now()
	_, err := cli.Run(context.Background(), "list", "vms")
	if err == nil {
		t.Fatal("a command that outlives its deadline must not be reported as success")
	}
	var ce *CommandError
	if !errors.As(err, &ce) {
		t.Fatalf("want *CommandError, got %T", err)
	}
	if !ce.TimedOut {
		t.Errorf("the error should be identified as a timeout: %v", err)
	}
	// exec.CommandContext kills the child, so the wait must end near the deadline
	// rather than after the child's own sleep.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("the timeout was not honoured: waited %s", elapsed)
	}
}

func TestCLIListReconcilesWithRunningVMS(t *testing.T) {
	long := `Name:            kali
Groups:          /
UUID:            2d620512-4b10-45a8-b070-25435e5106a5
State:           running

Name:            ubuntu
Groups:          /
UUID:            c907c20b-5a41-4dc2-99c5-0e9bc7773c38
State:           powered off
`
	// runningvms says ubuntu is up while --long still says powered off: the
	// machine is mid-transition, which is the honest and the safe answer.
	running := `"kali" {2d620512-4b10-45a8-b070-25435e5106a5}
"ubuntu" {c907c20b-5a41-4dc2-99c5-0e9bc7773c38}
`
	r := &routingRunner{
		canned: map[string]fixtures{
			"list vms --long": {stdoutFile: writeFixture(t, "long.txt", long)},
			"list runningvms": {stdoutFile: writeFixture(t, "running.txt", running)},
		},
	}
	cli := NewCLI("VBoxManage")
	cli.SetRunner(r)

	inv, err := cli.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(inv.Problems) != 0 {
		t.Errorf("unexpected problems: %+v", inv.Problems)
	}
	kali, ok := inv.Find("2d620512-4b10-45a8-b070-25435e5106a5")
	if !ok {
		t.Fatal("kali missing")
	}
	if kali.State.State != StateRunning {
		t.Errorf("kali = %q; both lists agree it runs, so it should read running", kali.State.State)
	}
	ubuntu, ok := inv.Find("c907c20b-5a41-4dc2-99c5-0e9bc7773c38")
	if !ok {
		t.Fatal("ubuntu missing")
	}
	if ubuntu.State.Kind != KindTransitional {
		t.Errorf("ubuntu = %q (kind %d): a disagreement must not be presented as an actionable state",
			ubuntu.State.State, ubuntu.State.Kind)
	}
	if len(ubuntu.Notes) == 0 {
		t.Error("the disagreement should be explained rather than silently corrected")
	}
	if AllowedActions(ubuntu.State)[ActionStop] {
		t.Error("no action should be offered while the state is uncertain")
	}
}

func TestCLIListKeepsMachinesOnPartialFailure(t *testing.T) {
	long := `Name:            kali
Groups:          /
UUID:            2d620512-4b10-45a8-b070-25435e5106a5
State:           running
`
	r := &routingRunner{
		canned: map[string]fixtures{
			"list vms --long": {stdoutFile: writeFixture(t, "long2.txt", long)},
			"list runningvms": {
				stderrFile: writeFixture(t, "boom.txt", "VBoxManage.exe: error: VDpipeCreate error\n"),
				code:       1,
			},
		},
	}
	cli := NewCLI("VBoxManage")
	cli.SetRunner(r)

	inv, err := cli.List(context.Background())
	// An empty inventory would look like the VMs had been deleted, so a partial
	// failure must keep the machines that were read and report the gap.
	if err != nil {
		t.Fatalf("a partial failure should not discard the inventory: %v", err)
	}
	if len(inv.Machines) != 1 {
		t.Fatalf("machines were lost on a partial failure: %+v", inv.Machines)
	}
	if len(inv.Problems) == 0 {
		t.Error("the partial failure must be reported as a problem")
	}
}

func TestCLIListReportsHardFailure(t *testing.T) {
	r := &routingRunner{
		canned: map[string]fixtures{
			"list vms --long": {
				stderrFile: writeFixture(t, "hard.txt", "VBoxManage.exe: error: VirtualBox COM server is not running\n"),
				code:       1,
			},
		},
	}
	cli := NewCLI("VBoxManage")
	cli.SetRunner(r)
	if _, err := cli.List(context.Background()); err == nil {
		t.Error("a complete failure to read the list must be reported as an error")
	}
}

func TestCLIRunRejectsEmptyPath(t *testing.T) {
	cli := NewCLI("")
	if _, err := cli.Run(context.Background(), "list", "vms"); err == nil {
		t.Error("running with no executable configured should be an error, not a silent success")
	}
}

func TestCLIVersionTrims(t *testing.T) {
	cli, r := newCLIWithFixtures(t, fixtures{stdoutFile: writeFixture(t, "ver.txt", "7.2.20r175154\r\n")})
	got, err := cli.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if got != "7.2.20r175154" {
		t.Errorf("version = %q", got)
	}
	calls := r.recorded()
	if len(calls) != 1 || calls[0][len(calls[0])-1] != "--version" {
		t.Errorf("args = %v, want the last argument to be --version", calls)
	}
}

// routingRunner answers by subcommand so one fake can serve several fixtures.
type routingRunner struct {
	canned map[string]fixtures

	mu    sync.Mutex
	calls [][]string
}

func (r *routingRunner) Command(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string{name}, args...))
	r.mu.Unlock()
	// CLI.Run always prepends --nologo, so the fixture is keyed on the real
	// subcommand rather than on the full argument list.
	key := strings.TrimSpace(strings.TrimPrefix(strings.Join(args, " "), "--nologo"))
	fix, ok := r.canned[key]
	if !ok {
		// An unexpected request is a test failure worth surfacing rather than a
		// mysteriously empty response.
		return nil, errors.New("unexpected VBoxManage request: " + key)
	}
	return helper(ctx, fix), nil
}

func (r *routingRunner) Interactive(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
	return r.Command(ctx, name, args...)
}

func TestVboxErrorMessageExtractsTheReason(t *testing.T) {
	stderr := "Oracle VM VirtualBox Command Line Management Interface\r\n" +
		"Copyright (C) 2005-2026 Oracle Corporation\r\n" +
		"\r\n" +
		"VBoxManage.exe: error: Could not rename the settings file 'C:\\VirtualBox VMs\\vm\\vm.vbox'\r\n" +
		"VBoxManage.exe: error: Details: code VBOX_E_FILE_ERROR (0x80bb0004), MachineImpl 0x1\r\n"
	got := vboxErrorMessage(stderr)
	if !strings.Contains(got, "Could not rename the settings file") {
		t.Errorf("the actionable line should survive, got %q", got)
	}
	for _, noise := range []string{"Copyright", "Management Interface"} {
		if strings.Contains(got, noise) {
			t.Errorf("banner noise should be dropped: %q", got)
		}
	}
	if got := vboxErrorMessage(""); got != "" {
		t.Errorf("empty stderr should give no message, got %q", got)
	}
	if got := vboxErrorMessage("just some text"); !strings.Contains(got, "just some text") {
		t.Errorf("unrecognised output should still be shown rather than swallowed: %q", got)
	}
}

// ---------- discovery ----------

func TestFindVBoxManageUsesOverride(t *testing.T) {
	dir := t.TempDir()
	name := exeName("VBoxManage")
	real := filepath.Join(dir, name)
	if err := os.WriteFile(real, []byte("#!x\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Hide PATH and the standard location so only the override can succeed, which
	// is what makes this a test of priority rather than of this machine.
	t.Setenv("PATH", "")
	d, err := FindVBoxManage(real)
	if err != nil {
		t.Fatalf("a valid override must be accepted: %v", err)
	}
	if d.Source != "override" {
		t.Errorf("source = %q, want override: the UI shows this so a user can tell", d.Source)
	}
	if !strings.EqualFold(filepath.Base(d.Path), name) {
		t.Errorf("path = %q", d.Path)
	}
}

func TestFindVBoxManageFallsBackToPATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		// LookPath on Windows also considers the current directory and known
		// extensions, so assert only that the fallback is reported honestly.
		t.Skip("PATH semantics differ on Windows; the override and not-found cases are asserted instead")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "VBoxManage")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	d, err := FindVBoxManage("")
	if err != nil {
		t.Fatalf("PATH discovery: %v", err)
	}
	if d.Source != "PATH" {
		t.Errorf("source = %q, want PATH", d.Source)
	}
}

// TestFindVBoxManageRejectsADirectory covers the most common mistake: pointing at
// the install folder instead of the executable.
func TestFindVBoxManageRejectsADirectory(t *testing.T) {
	dir := t.TempDir()
	if _, err := resolveExplicit(dir); err == nil {
		t.Error("a directory is not an executable and must be rejected")
	}
	if _, err := resolveExplicit(filepath.Join(dir, "absent.exe")); err == nil {
		t.Error("a path that does not exist must be rejected")
	}
	if _, err := resolveExplicit("   "); err == nil {
		t.Error("whitespace is not a path")
	}
}

// TestFindVBoxManageAddsTheExeSuffix lets `--vboxmanage C:\...\VBoxManage` work.
func TestFindVBoxManageAddsTheExeSuffix(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the .exe suffix rule is Windows specific")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "VBoxManage.exe"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := resolveExplicit(filepath.Join(dir, "VBoxManage"))
	if err != nil {
		t.Fatalf("suffix should be appended: %v", err)
	}
	if !strings.HasSuffix(strings.ToLower(got), ".exe") {
		t.Errorf("resolved %q", got)
	}
}

func TestFindVBoxManageReportsWhereItLooked(t *testing.T) {
	t.Setenv("PATH", "")
	d, err := FindVBoxManage(filepath.Join(t.TempDir(), "absent"))
	if err == nil {
		// On a machine that genuinely has VirtualBox in a standard location the
		// lookup succeeds, which is correct behaviour, not a failure.
		if d.Path == "" {
			t.Fatal("a successful lookup must name the path")
		}
		return
	}
	if len(d.Candidates) == 0 {
		t.Error("an error should come with the list of places that were tried")
	}
	joined := strings.Join(d.Candidates, "\n")
	if !strings.Contains(joined, "override") {
		t.Errorf("the rejected override should appear in the candidates: %s", joined)
	}
	hint := InstallHint(err)
	for _, want := range []string{"virtualbox.org", "--vboxmanage", "PATH"} {
		if !strings.Contains(hint, want) {
			t.Errorf("the remediation hint should mention %q:\n%s", want, hint)
		}
	}
}

func TestInstallHintNamesTheUnderlyingError(t *testing.T) {
	hint := InstallHint(errors.New("permission denied"))
	if !strings.Contains(hint, "permission denied") {
		t.Errorf("the underlying cause should be included:\n%s", hint)
	}
	if !strings.Contains(InstallHint(nil), "VirtualBox was not found") {
		t.Error("the hint should still make sense without a cause")
	}
}
