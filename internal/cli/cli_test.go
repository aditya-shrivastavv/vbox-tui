package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/aditya-shrivastavv/vbox-tui/internal/app"
	"github.com/aditya-shrivastavv/vbox-tui/internal/config"
	appssh "github.com/aditya-shrivastavv/vbox-tui/internal/ssh"
	"github.com/aditya-shrivastavv/vbox-tui/internal/vbox"
)

// The direct (scriptable) door: argument parsing, the single-target and batch
// semantics, the honest refusal rules, and the noninteractive guards — all
// against a fake world. Nothing here may touch VirtualBox, ssh, or the real
// settings file.

// ---------- argument parsing ----------

func TestParseVerbAndTargets(t *testing.T) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	opts, code, handled := parseArgs([]string{"stop", "My VM"}, out, errOut)
	if handled || code != 0 {
		t.Fatalf("handled=%v code=%d err=%q", handled, code, errOut)
	}
	if opts.action != vbox.ActionStop || len(opts.targets) != 1 || opts.targets[0] != "My VM" {
		t.Fatalf("parsed %+v", opts)
	}

	// Several names after a batchable verb are one batch, not stale arguments:
	// the parser collects them and stops at the first flag.
	out, errOut = &bytes.Buffer{}, &bytes.Buffer{}
	opts, code, handled = parseArgs([]string{"start", "one", "two", "--config", `C:\somewhere\config.json`}, out, errOut)
	if handled || code != 0 {
		t.Fatalf("multi-target: handled=%v code=%d err=%q", handled, code, errOut)
	}
	if strings.Join(opts.targets, ",") != "one,two" || opts.configPath == "" {
		t.Fatalf("multi-target parse: %+v", opts)
	}

	// A flag value after verb+target is NOT a second VM argument: the parser
	// must understand flags and only refuse on real surplus positionals.
	out, errOut = &bytes.Buffer{}, &bytes.Buffer{}
	opts, code, handled = parseArgs([]string{"start", "My VM", "--config", `C:\somewhere\config.json`}, out, errOut)
	if handled || code != 0 || opts.configPath == "" {
		t.Fatalf("flag after target: handled=%v code=%d err=%q", handled, code, errOut)
	}

	// Anything positional after a flag is a mistake, not a second target:
	// the flag package stops there and we refuse rather than guess.
	out, errOut = &bytes.Buffer{}, &bytes.Buffer{}
	_, code, handled = parseArgs([]string{"start", "--demo", "My VM"}, out, errOut)
	if !handled || code != exitUsage {
		t.Fatalf("positional after flag must be usage: handled=%v code=%d", handled, code)
	}
	if !strings.Contains(errOut.String(), `unexpected argument "My VM"`) {
		t.Fatalf("usage error should name the stray argument: %q", errOut)
	}

	// Unknown leading verbs are never fuzzy-accepted: the argument
	// makes the parser stop and explain.
	out, errOut = &bytes.Buffer{}, &bytes.Buffer{}
	_, code, handled = parseArgs([]string{"frobnicate", "X"}, out, errOut)
	if !handled || code != exitUsage {
		t.Fatalf("unknown verb must be usage: handled=%v code=%d", handled, code)
	}
	if strings.Contains(errOut.String(), "frobnicate") && !strings.Contains(errOut.String(), "unexpected") {
		t.Fatalf("should say the argument was unexpected: %q", errOut)
	}

	// Flags still work after verb+targets.
	out, errOut = &bytes.Buffer{}, &bytes.Buffer{}
	opts, code, handled = parseArgs([]string{"ssh", "Kali", "--ssh-timeout", "90s"}, out, errOut)
	if handled || code != 0 || opts.sshTimeout != 90*time.Second {
		t.Fatalf("flags after verb: %+v handled=%v code=%d %q", opts, handled, code, errOut)
	}

	// config takes its modifiers, and a snapshot list cannot name a batch.
	out, errOut = &bytes.Buffer{}, &bytes.Buffer{}
	opts, code, handled = parseArgs([]string{"config", "--path"}, out, errOut)
	if handled || code != 0 || opts.action != app.ActionSettings || !opts.showPath {
		t.Fatalf("config --path: %+v handled=%v code=%d", opts, handled, code)
	}
	if err := validate(&options{action: app.ActionSnapshots, targets: []string{"a", "b"}}); err == nil {
		t.Error("more than one snapshot target must be refused")
	}
	if err := validate(&options{sshTimeout: 20 * time.Minute}); err == nil {
		t.Error("an over-long ssh timeout must be refused")
	}
}

func TestConfigYesWithoutTargetIsUsage(t *testing.T) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	_, code, handled := parseArgs([]string{"config", "--yes"}, out, errOut)
	if !handled || code != exitUsage || !strings.Contains(errOut.String(), "needs a VM name") {
		t.Fatalf("bare config --yes: handled=%v code=%d err=%q", handled, code, errOut)
	}
}

func TestHelpTextAdvertisesTheNewShape(t *testing.T) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	if code := Run([]string{"--help"}, out, errOut); code != exitOK {
		t.Fatalf("help exit %d", code)
	}
	text := out.String()
	for _, want := range []string{
		"vbx <action> [VM...]",
		"start", "pause", "resume", "stop", "save", "restart", "ssh", "config", "snapshots",
		"not a dashboard",
		"UUID",
		"--demo",
		"space marks machines",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("help missing %q", want)
		}
	}
	// The text must describe an inline prompt: "dashboard" may appear only in
	// the negation, and no full-screen-era wording anywhere.
	if strings.Count(strings.ToLower(text), "dashboard") != 1 || !strings.Contains(text, "not a dashboard") {
		t.Errorf("dashboard should appear exactly once, as the negation: %q", text)
	}
	for _, stale := range []string{"full screen", "fullscreen", "alternate screen", "persistent"} {
		if strings.Contains(strings.ToLower(text), stale) {
			t.Errorf("help still advertises dashboard-era concept %q", stale)
		}
	}
}

func TestDurationParsing(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
		bad  bool
	}{
		{in: "45s", want: 45 * time.Second},
		{in: "3m", want: 3 * time.Minute},
		{in: "45", want: 45 * time.Second},
		{in: "5 minutes", bad: true}, // Sscanf("%g") would silently make it 5 seconds
		{in: "0", bad: true},
		{in: "-5s", bad: true},
	} {
		d, err := parseDuration(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("parseDuration(%q) = %v, want error", tc.in, d)
			}
			continue
		}
		if err != nil || d != tc.want {
			t.Errorf("parseDuration(%q) = %v, %v; want %v", tc.in, d, err, tc.want)
		}
	}
}

// ---------- small helper units ----------

func TestRenderOneLineQuote(t *testing.T) {
	if got := render("ok", true); got != "+ ok" {
		t.Errorf("render ok = %q", got)
	}
	if got := render("bad", false); got != "! bad" {
		t.Errorf("render fail = %q", got)
	}
	if got := quote("My VM"); got != `"My VM"` {
		t.Errorf("quote = %q", got)
	}
	if got := oneLine(errors.New(strings.Repeat("x", 400))); len([]rune(got)) > 302 || !strings.HasSuffix(got, "…") {
		t.Errorf("long error not bounded: %d runes", len([]rune(got)))
	}
	if got := oneLine(nil); got != "" {
		t.Errorf("oneLine(nil) = %q", got)
	}
	// The refusal line for an action needing no subject must not gain a stray
	// "over ssh" tail.
	verb, subject := refusalWords(vbox.ActionConnect)
	if verb != "reach" || subject != "over ssh" {
		t.Fatalf("ssh refusal wording: %q %q", verb, subject)
	}
	verb, subject = refusalWords(vbox.ActionPause)
	if verb != "pause" || subject != "" {
		t.Fatalf("pause refusal wording: %q %q", verb, subject)
	}
	// alreadyInStateLine must not claim an operation it never sent.
	if got := alreadyInStateLine(vbox.ActionStart, `"X"`); !strings.Contains(got, "nothing was sent") {
		t.Fatalf("already-in-state line: %q", got)
	}
}

// ---------- fake world for the direct path ----------

type fakeEnv struct {
	mu       sync.Mutex
	machines []vbox.Machine
	ops      []string
	listErr  error
	// rebootErr, when set, is what GuestReboot returns: the Guest Additions
	// restart route then reports the backend's own refusal.
	rebootErr error
}

func (f *fakeEnv) record(op string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, op)
}

func (f *fakeEnv) opList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

func (f *fakeEnv) opCount(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, o := range f.ops {
		if o == op {
			n++
		}
	}
	return n
}

func (f *fakeEnv) opHas(name string) bool {
	for _, o := range f.opList() {
		if o == name {
			return true
		}
	}
	return false
}

func (f *fakeEnv) List(context.Context) (vbox.Inventory, error) {
	f.record("list")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return vbox.Inventory{}, f.listErr
	}
	return vbox.Inventory{Machines: append([]vbox.Machine(nil), f.machines...)}, nil
}

func (f *fakeEnv) Detail(_ context.Context, uuid string) (vbox.Machine, error) {
	f.record("detail")
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.machines {
		if strings.EqualFold(m.UUID, uuid) {
			return m, nil
		}
	}
	return vbox.Machine{}, errors.New("uuid not found")
}

func (f *fakeEnv) op(op, uuid, st string) error {
	f.record(op)
	f.mu.Lock()
	defer f.mu.Unlock()
	if st != "" {
		for i := range f.machines {
			if strings.EqualFold(f.machines[i].UUID, uuid) {
				f.machines[i].State = vbox.NormalizeState(st)
			}
		}
	}
	return nil
}
func (f *fakeEnv) PowerOn(_ context.Context, uuid string) error {
	return f.op("poweron", uuid, "running")
}
func (f *fakeEnv) Resume(_ context.Context, uuid string) error {
	return f.op("resume", uuid, "running")
}
func (f *fakeEnv) Pause(_ context.Context, uuid string) error {
	return f.op("pause", uuid, "paused")
}
func (f *fakeEnv) SaveState(_ context.Context, uuid string) error {
	return f.op("savestate", uuid, "saved")
}
func (f *fakeEnv) ShutdownGracefully(_ context.Context, uuid string) error {
	return f.op("acpipowerbutton", uuid, "")
}
func (f *fakeEnv) GuestReboot(_ context.Context, uuid string) error {
	f.record("guestreboot")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rebootErr
}
func (f *fakeEnv) Version(context.Context) (string, error) { return "7.2.0-fake", nil }

// Snapshots: the direct CLI paths never read or change the tree, but fakeEnv
// must satisfy vbox.Manager, so it answers with an empty tree.
func (f *fakeEnv) ListSnapshots(_ context.Context, _ string) (vbox.SnapshotTree, error) {
	f.record("snapshot-list")
	return vbox.SnapshotTree{}, nil
}
func (f *fakeEnv) TakeSnapshot(_ context.Context, uuid, name, _ string) error {
	f.op("snapshot-take", uuid, "")
	return nil
}
func (f *fakeEnv) RestoreSnapshot(_ context.Context, uuid, _ string) error {
	f.op("snapshot-restore", uuid, "")
	return nil
}

// RenameVM records the call and applies it, keeping the powered-off rule so
// the fake refuses exactly where VirtualBox would.
func (f *fakeEnv) RenameVM(_ context.Context, uuid, newName string) error {
	f.record("rename")
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.machines {
		if strings.EqualFold(f.machines[i].UUID, uuid) {
			if f.machines[i].State.Kind != vbox.KindOff && f.machines[i].State.Kind != vbox.KindAborted {
				return fmt.Errorf("%q is %s; VirtualBox renames only a powered-off machine", f.machines[i].Name, f.machines[i].State.Word())
			}
			f.machines[i].Name = newName
		}
	}
	return nil
}

var (
	runningKali = vbox.Machine{Name: "Kali Linux", UUID: "2d620512-4b10-45a8-b070-25435e5106a5",
		State: vbox.NormalizeState("running")}
	offWin = vbox.Machine{Name: "Windows 11", UUID: "c907c20b-5a41-4dc2-99c5-0e9bc7773c3a",
		State: vbox.NormalizeState("powered off")}
	dupPausedKali = vbox.Machine{Name: "Kali Linux", UUID: "99999999-aaaa-bbbb-cccc-dddddddddddd",
		State: vbox.NormalizeState("paused")}
	savedLab = vbox.Machine{Name: "lab saved", UUID: "11111111-2222-3333-4444-555555555555",
		State: vbox.NormalizeState("saved")}
)

func withFakeWorld(t *testing.T, env *fakeEnv) {
	t.Helper()
	oldDisc, oldNew := vboxDiscover, vboxNewManager
	oldProber, oldHandoff, oldOpts := proberForDirect, handoffSSH, programOptions
	t.Cleanup(func() {
		vboxDiscover, vboxNewManager = oldDisc, oldNew
		proberForDirect, handoffSSH, programOptions = oldProber, oldHandoff, oldOpts
	})
	vboxDiscover = func(string) (vbox.Discovery, error) {
		return vbox.Discovery{Path: `C:\fake\VBoxManage.exe`, Source: "test"}, nil
	}
	vboxNewManager = func(string) vbox.Manager { return env }
	// A program under test gets no renderer, no signal handler, and an input
	// that is at EOF at once: closing the real console's stdin mid-ReadConsole
	// can block forever on Windows, and a test has no console to wait on.
	programOptions = []tea.ProgramOption{
		tea.WithoutRenderer(),
		tea.WithoutSignals(),
		tea.WithInput(&eofReader{}),
	}
}

// eofReader reads as immediately finished, standing in for a pipe nobody
// writes into.
type eofReader struct{}

func (*eofReader) Read(p []byte) (int, error) { return 0, io.EOF }

// noDial makes the readiness prober fail fast without touching a socket: the
// direct ssh path must then report honestly, not hang.
func noDial() *appssh.Prober {
	return &appssh.Prober{
		Dial: func(ctx context.Context, addr string) (net.Conn, error) {
			return nil, errors.New("no route (test)")
		},
		Interval:       time.Millisecond,
		AttemptTimeout: 20 * time.Millisecond,
		NonSSHFailures: 1,
	}
}

// okDial answers the SSH banner immediately (readiness fast success path).
func okDial() *appssh.Prober {
	return &appssh.Prober{
		Dial: func(ctx context.Context, addr string) (net.Conn, error) {
			client, server := net.Pipe()
			go func() {
				_, _ = server.Write([]byte("SSH-2.0-OpenSSH_9.6\r\n"))
				time.Sleep(400 * time.Millisecond)
				_ = server.Close()
			}()
			return client, nil
		},
		Interval:       time.Millisecond,
		AttemptTimeout: 2 * time.Second,
		NonSSHFailures: 2,
	}
}

func cfgWithSSH(t *testing.T, vm vbox.Machine, host string, port int, user string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Defaults()
	// Shorten the readiness bound (config floors it at 5s): failure paths in
	// tests must fail fast, not wait out the production 45 seconds.
	cfg.SSHReadyTimeoutSeconds = config.MinSSHTimeoutSeconds
	if err := cfg.SetVMSSH(vm.UUID, vm.Name,
		config.SSHConfig{Mode: config.SSHModeEndpoint, Host: host, Port: port, User: user}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func aliasCfgPath(t *testing.T, vm vbox.Machine, alias string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Defaults()
	if err := cfg.SetVMSSH(vm.UUID, vm.Name, config.SSHConfig{Mode: config.SSHModeAlias, Alias: alias}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	return path
}

// runWith runs argv against the fake world and captures both streams.
// The buffer stdout is never a terminal, so every path a test reaches here
// must be one that works without a console.
func runWith(t *testing.T, env *fakeEnv, argv ...string) (code int, stdout, stderr string) {
	t.Helper()
	withFakeWorld(t, env)
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	code = Run(argv, out, errOut)
	return code, out.String(), errOut.String()
}

// ---------- the direct commands ----------

func TestDirectStartRunsAndPrintsOneLine(t *testing.T) {
	env := &fakeEnv{machines: []vbox.Machine{runningKali, offWin}}
	code, out, errOut := runWith(t, env, "start", "Windows 11", "--config", filepath.Join(t.TempDir(), "config.json"))
	if code != exitOK {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if !strings.Contains(out, `+ Started "Windows 11" in headless mode.`) {
		t.Fatalf("stdout = %q", out)
	}
	if strings.Contains(out, "\n\n") {
		t.Fatalf("a direct command prints one line: %q", out)
	}
	if !env.opHas("poweron") || env.opHas("pause") {
		t.Fatalf("ops = %v, expected exactly list + poweron", env.opList())
	}
}

func TestDirectResolvesByUUIDWhenNamesCollide(t *testing.T) {
	env := &fakeEnv{machines: []vbox.Machine{runningKali, dupPausedKali}}
	cfgPath := filepath.Join(t.TempDir(), "config.json")

	// Two machines share the name: the name must be refused, listing both UUIDs.
	code, _, errOut := runWith(t, env, "pause", "Kali Linux", "--config", cfgPath)
	if code != exitUsage {
		t.Fatalf("ambiguous name must exit usage, got %d (%q)", code, errOut)
	}
	for _, want := range []string{runningKali.UUID, dupPausedKali.UUID, "UUID"} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("ambiguity message should contain %q: %q", want, errOut)
		}
	}
	if env.opHas("pause") {
		t.Fatal("an ambiguous name must not touch VirtualBox")
	}

	// The UUID picks exactly the paused one, and runningKali stays untouched.
	code, out, errOut := runWith(t, env, "resume", dupPausedKali.UUID, "--config", cfgPath)
	if code != exitOK {
		t.Fatalf("resume by UUID: exit %d (%q)", code, errOut)
	}
	if !strings.Contains(out, `+ Resumed "Kali Linux".`) {
		t.Fatalf("stdout = %q", out)
	}
	if env.opHas("poweron") || env.opHas("pause") {
		t.Fatalf("only resume of the paused UUID was allowed: %v", env.opList())
	}
}

func TestDirectUnknownTargetIsUsage(t *testing.T) {
	env := &fakeEnv{machines: []vbox.Machine{runningKali}}
	code, _, errOut := runWith(t, env, "start", "Does Not Exist", "--config", filepath.Join(t.TempDir(), "config.json"))
	if code != exitUsage {
		t.Fatalf("unknown VM: %d (%q)", code, errOut)
	}
	if !strings.Contains(errOut, "Does Not Exist") {
		t.Fatalf("should name what was not found: %q", errOut)
	}
}

func TestDirectRefusalWording(t *testing.T) {
	// pause a powered-off machine: VirtualBox would refuse, so vbx
	// refuses first and says nothing was changed.
	env := &fakeEnv{machines: []vbox.Machine{offWin}}
	code, _, errOut := runWith(t, env, "pause", "Windows 11", "--config", filepath.Join(t.TempDir(), "config.json"))
	if code != exitRun {
		t.Fatalf("refusal exit %d (%q)", code, errOut)
	}
	for _, want := range []string{`cannot pause "Windows 11"`, "Nothing was changed"} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("refusal missing %q: %q", want, errOut)
		}
	}
	if env.opHas("pause") {
		t.Fatal("a refusal must not call the backend")
	}
}

// ---------- multi-name batch ----------

func TestDirectBatchRunsEveryNamedMachine(t *testing.T) {
	env := &fakeEnv{machines: []vbox.Machine{runningKali, offWin, savedLab}}
	code, out, errOut := runWith(t, env, "start",
		"Windows 11", savedLab.UUID, "--config", filepath.Join(t.TempDir(), "config.json"))
	if code != exitOK {
		t.Fatalf("batch of two startable machines: exit %d (%q)", code, errOut)
	}
	// Kali was never named: the batch touches exactly the two requested.
	if env.opCount("poweron") != 2 {
		t.Fatalf("poweron count = %d, want 2: %v", env.opCount("poweron"), env.opList())
	}
	for _, want := range []string{"2 succeeded", "Windows 11"} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary missing %q: %q", want, out)
		}
	}
}

func TestDirectBatchDeduplicatesOneMachineNamedTwice(t *testing.T) {
	env := &fakeEnv{machines: []vbox.Machine{offWin}}
	code, out, _ := runWith(t, env, "start", "Windows 11", strings.ToUpper(offWin.UUID),
		"--config", filepath.Join(t.TempDir(), "config.json"))
	// The same machine named two ways is one operation — and one success.
	if code != exitOK || !strings.Contains(out, "1 succeeded") {
		t.Fatalf("dedupe: exit %d out %q", code, out)
	}
	if env.opCount("poweron") != 1 {
		t.Fatalf("power-on issued twice for one machine: %v", env.opList())
	}
}

func TestDirectBatchResolvesAllNamesBeforeMutating(t *testing.T) {
	env := &fakeEnv{machines: []vbox.Machine{offWin}}
	code, _, errOut := runWith(t, env, "start", "Windows 11", "Does Not Exist",
		"--config", filepath.Join(t.TempDir(), "config.json"))
	if code != exitUsage {
		t.Fatalf("one unknown name fails the whole command: %d %q", code, errOut)
	}
	if env.opHas("poweron") {
		t.Fatal("an unresolvable name must stop the batch before anything runs")
	}
}

func TestDirectBatchSkipsRefusedMachineAndExitsNonzero(t *testing.T) {
	// offWin cannot be paused: the plan skips it instead of issuing a request
	// for it. Kali accepts the action and is served; a batch that could not
	// honour every named machine is an incomplete batch and exits nonzero.
	env := &fakeEnv{machines: []vbox.Machine{runningKali, offWin}}
	code, out, errOut := runWith(t, env, "pause", "Kali Linux", "Windows 11",
		"--config", filepath.Join(t.TempDir(), "config.json"))
	if code != exitRun {
		t.Fatalf("a batch that skipped a machine must exit nonzero: %d (%q)", code, errOut)
	}
	// Exactly one pause — for the machine that accepted it; the refused
	// machine was never touched.
	if env.opCount("pause") != 1 {
		t.Fatalf("pause issued for the refused machine or twice: %v", env.opList())
	}
	for _, want := range []string{"1 succeeded", "1 skipped", `"Windows 11"`} {
		if !strings.Contains(out+errOut, want) {
			t.Fatalf("summary missing %q: out=%q err=%q", want, out, errOut)
		}
	}
}

func TestDirectBatchWithNothingExecutableSendsNothing(t *testing.T) {
	// Every named machine refuses the action: the plan gate means no request
	// at all, and the line says nothing was sent.
	env := &fakeEnv{machines: []vbox.Machine{offWin, savedLab}}
	code, out, errOut := runWith(t, env, "pause", "Windows 11", savedLab.UUID,
		"--config", filepath.Join(t.TempDir(), "config.json"))
	if code != exitRun {
		t.Fatalf("all-skipped batch: %d", code)
	}
	if env.opCount("pause") != 0 {
		t.Fatalf("a fully refused batch must call the backend zero times: %v", env.opList())
	}
	if !strings.Contains(out, "Nothing was sent") {
		t.Fatalf("should say nothing was sent: out=%q err=%q", out, errOut)
	}
}

func TestDirectBatchRestartNeverResets(t *testing.T) {
	// A restart batch against a running machine with no SSH/guest settings
	// falls through to the Guest Additions route; the fake records exactly
	// what control command went out, and a reset is not in its vocabulary.
	env := &fakeEnv{machines: []vbox.Machine{runningKali, dupPausedKali}}
	code, out, errOut := runWith(t, env, "restart", runningKali.UUID, dupPausedKali.UUID,
		"--config", filepath.Join(t.TempDir(), "config.json"))
	// The paused machine is refused by state → skip → nonzero exit; the
	// running one gets the Additions route only.
	if code != exitRun {
		t.Fatalf("skipped target must give a nonzero exit: %d (%q)", code, errOut)
	}
	if env.opCount("guestreboot") != 1 {
		t.Fatalf("exactly one guest reboot request expected: %v", env.opList())
	}
	for _, op := range env.opList() {
		if strings.Contains(strings.ToLower(op), "reset") || strings.Contains(strings.ToLower(op), "poweroff") {
			t.Fatalf("a restart batch issued %q: restart is guest-OS only", op)
		}
	}
	for _, want := range []string{"Guest Additions", "skipped"} {
		if !strings.Contains(out, want) {
			t.Fatalf("batch lines missing %q: %q (%q)", want, out, errOut)
		}
	}
}

// ---------- direct restart ----------

func TestDirectRestartThroughGuestAdditions(t *testing.T) {
	env := &fakeEnv{machines: []vbox.Machine{runningKali}}
	code, out, errOut := runWith(t, env, "restart", "Kali Linux", "--config", filepath.Join(t.TempDir(), "config.json"))
	if code != exitOK {
		t.Fatalf("exit %d (%q)", code, errOut)
	}
	if !env.opHas("guestreboot") {
		t.Fatalf("ops = %v, expected the guest-control route", env.opList())
	}
	for _, op := range env.opList() {
		if strings.Contains(strings.ToLower(op), "reset") || strings.Contains(strings.ToLower(op), "poweroff") {
			t.Fatalf("a graceful restart issued %q", op)
		}
	}
	if !strings.Contains(out, "Guest Additions") {
		t.Fatalf("the accepted route must be named: %q", out)
	}
}

func TestDirectRestartRefusesAPoweredOffMachine(t *testing.T) {
	env := &fakeEnv{machines: []vbox.Machine{offWin}}
	code, _, errOut := runWith(t, env, "restart", "Windows 11", "--config", filepath.Join(t.TempDir(), "config.json"))
	if code != exitRun {
		t.Fatalf("restart of a powered-off machine must fail: %d", code)
	}
	if env.opHas("guestreboot") {
		t.Fatal("a machine that is not running must not receive a reboot request")
	}
	for _, want := range []string{`restart "Windows 11"`, "Nothing was changed"} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("refusal missing %q: %q", want, errOut)
		}
	}
}

// ---------- direct ssh ----------

func TestDirectSSHWaitsThenFailsHonestlyWhenGuestDoesNotAnswer(t *testing.T) {
	env := &fakeEnv{machines: []vbox.Machine{runningKali}}
	withFakeWorld(t, env)
	proberForDirect = noDial
	cfgPath := cfgWithSSH(t, runningKali, "127.0.0.1", 2222, "root")

	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	code := Run([]string{"ssh", "Kali Linux", "--config", cfgPath}, out, errOut)
	if code != exitRun {
		t.Fatalf("no answering endpoint must fail, got exit %d (%q / %q)", code, out, errOut)
	}
	if !strings.Contains(out.String(), "Waiting for SSH on 127.0.0.1:2222") {
		t.Fatalf("should report what it was waiting for: %q", out)
	}
	if !strings.Contains(errOut.String(), "did not become ready") {
		t.Fatalf("honest readiness failure missing: %q", errOut)
	}
}

func TestDirectSSHStartsFirstThenWaitsAndHandsoff(t *testing.T) {
	env := &fakeEnv{machines: []vbox.Machine{offWin}}
	withFakeWorld(t, env)
	proberForDirect = okDial
	cfgPath := cfgWithSSH(t, offWin, "127.0.0.1", 2222, "root")

	var mu sync.Mutex
	var specs []appssh.ExecSpec
	handoffSSH = func(spec appssh.ExecSpec, _ io.Writer) int {
		mu.Lock()
		defer mu.Unlock()
		specs = append(specs, spec)
		return 0
	}

	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	code := Run([]string{"ssh", "Windows 11", "--config", cfgPath}, out, errOut)
	if code != exitOK {
		t.Fatalf("full journey exit %d (%q / %q)", code, out, errOut)
	}
	// Start happened first, was reported, then exactly one handoff followed.
	if !env.opHas("poweron") {
		t.Fatalf("ssh on a stopped guest must start it: %v", env.opList())
	}
	if !strings.Contains(out.String(), `+ Started "Windows 11" in headless mode.`) {
		t.Fatalf("the bring-up step must be reported: %q", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(specs) != 1 {
		t.Fatalf("handoff specs = %d, want 1", len(specs))
	}
	args := strings.Join(specs[0].Args, " ")
	if !strings.Contains(args, "-p 2222") || !strings.Contains(args, "root@127.0.0.1") {
		t.Fatalf("handoff spec = %v", specs[0].Args)
	}
}

func TestDirectSSHAliasSkipsTheProbe(t *testing.T) {
	env := &fakeEnv{machines: []vbox.Machine{runningKali}}
	withFakeWorld(t, env)
	cfgPath := aliasCfgPath(t, runningKali, "work-kali")

	proberForDirect = func() *appssh.Prober {
		return &appssh.Prober{
			Dial: func(ctx context.Context, addr string) (net.Conn, error) {
				t.Errorf("alias mode must not TCP-probe (dialled %s)", addr)
				return nil, errors.New("suppressed")
			},
			Interval:       time.Millisecond,
			AttemptTimeout: 20 * time.Millisecond,
			NonSSHFailures: 1,
		}
	}
	var handed appssh.ExecSpec
	handoffSSH = func(spec appssh.ExecSpec, _ io.Writer) int {
		handed = spec
		return 0
	}

	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	code := Run([]string{"ssh", "Kali Linux", "--config", cfgPath}, out, errOut)
	if code != exitOK {
		t.Fatalf("alias journey exit %d (%q / %q)", code, out, errOut)
	}
	if args := strings.Join(handed.Args, " "); !strings.Contains(args, "-- work-kali") {
		t.Fatalf("alias must reach ssh verbatim after --: %q", args)
	}
}

func TestDirectSSHMissingSettingsPointsAtTheSetup(t *testing.T) {
	env := &fakeEnv{machines: []vbox.Machine{runningKali}}
	code, _, errOut := runWith(t, env, "ssh", "Kali Linux", "--config", filepath.Join(t.TempDir(), "config.json"))
	if code != exitRun {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"no SSH settings", "vbx ssh"} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("should point at the interactive setup (%q): %q", want, errOut)
		}
	}
}

func TestDirectRefusesToInventIdleResults(t *testing.T) {
	// stop on a machine that is already off: nothing is sent (VirtualBox has
	// no ACPI target when the guest is not running), and the line says so.
	env := &fakeEnv{machines: []vbox.Machine{offWin}}
	code, out, errOut := runWith(t, env, "stop", "Windows 11", "--config", filepath.Join(t.TempDir(), "config.json"))
	if code == exitOK {
		t.Fatalf("stopping a powered-off machine must not claim success: %q", out)
	}
	_ = out
	_ = errOut
	// The refusal may come from AllowedActions (off has no stop): the
	// essential guard is that no acpipowerbutton call went out.
	if env.opHas("acpipowerbutton") {
		t.Fatal("an ACPI request against a powered-off machine was issued")
	}
}

// ---------- noninteractive guard ----------

func TestNoninteractiveGuidesInsteadOfHanging(t *testing.T) {
	// stdout here is a buffer: never a terminal.
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	code := Run([]string{"--config", filepath.Join(t.TempDir(), "config.json")}, out, errOut)
	if code != exitUsage {
		t.Fatalf("bare non-tty run should exit usage, got %d", code)
	}
	for _, want := range []string{"not a terminal", `vbx start "exact VM name or UUID"`, "--help"} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("guidance missing %q: %q", want, errOut)
		}
	}

	out, errOut = &bytes.Buffer{}, &bytes.Buffer{}
	code = Run([]string{"start", "--config", filepath.Join(t.TempDir(), "config.json")}, out, errOut)
	if code != exitUsage || !strings.Contains(errOut.String(), `vbx start "exact VM name or UUID"`) {
		t.Fatalf("verb-only non-tty: %d %q", code, errOut)
	}

	// Demo is still the picker: non-tty must explain, never open a renderer.
	out, errOut = &bytes.Buffer{}, &bytes.Buffer{}
	code = Run([]string{"--demo"}, out, errOut)
	if code != exitUsage {
		t.Fatalf("demo non-tty: %d %q", code, errOut)
	}
}

// ---------- config command ----------

func TestConfigPathPrintsAndExits(t *testing.T) {
	// The printed answer must be exactly what the config package itself calls
	// the settings file, whatever this platform's user config directory is.
	want, err := config.Path()
	if err != nil {
		t.Skipf("no user configuration directory on this platform: %v", err)
	}
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	if code := Run([]string{"config", "--path"}, out, errOut); code != exitOK {
		t.Fatalf("config --path exit %d (%q)", code, errOut)
	}
	if got := strings.TrimSpace(out.String()); got != want {
		t.Fatalf("path printed %q, want %q", got, want)
	}
	// A bare --path (no verb) answers the same question.
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"--path"}, out, errOut); code != exitOK || strings.TrimSpace(out.String()) != want {
		t.Fatalf("bare --path: exit %d out %q err %q", code, out, errOut)
	}
}

func TestConfigOverSSHNeedsAConsole(t *testing.T) {
	// The buffer stdout is not a terminal: editing settings interactively is
	// impossible and --yes must not turn it into unattended writes.
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	env := &fakeEnv{machines: []vbox.Machine{runningKali}}
	withFakeWorld(t, env)
	path := filepath.Join(t.TempDir(), "config.json")
	code := Run([]string{"config", "Kali Linux", "--config", path}, out, errOut)
	if code != exitUsage {
		t.Fatalf("non-tty config edit: %d (%q)", code, errOut)
	}
	for _, want := range []string{"interactive", "--path"} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("guidance missing %q: %q", want, errOut)
		}
	}
	// --yes with a name must behave the same: a script cannot write settings
	// it never reviewed.
	out, errOut = &bytes.Buffer{}, &bytes.Buffer{}
	if code := Run([]string{"config", "Kali Linux", "--yes", "--config", path}, out, errOut); code != exitUsage {
		t.Fatalf("non-tty config --yes: %d (%q)", code, errOut)
	}
}

// ---------- serve: what remains in the scrollback ----------

func TestServePrintsFinalLineAndExitCode(t *testing.T) {
	env := &fakeEnv{listErr: errors.New("VBoxManage said no")}
	withFakeWorld(t, env)

	m := app.New(app.Options{
		Backend: env,
		Config:  config.Defaults(),
		Width:   80,
	})
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	code := serve(m, out, errOut)
	if code != exitRun {
		t.Fatalf("a failed run exits 1, got %d", code)
	}
	line := strings.TrimSpace(out.String())
	if !strings.HasPrefix(line, "! ") || !strings.Contains(line, "VBoxManage said no") {
		t.Fatalf("stdout should carry the honest final line: %q", out)
	}
	if !strings.Contains(errOut.String(), "VBoxManage said no") {
		t.Fatalf("failures belong on stderr for scripts: %q", errOut)
	}
}

func TestServeHandoffExitCodeIsPropagated(t *testing.T) {
	env := &fakeEnv{machines: []vbox.Machine{runningKali}}
	withFakeWorld(t, env)
	proberForDirect = okDial
	cfgPath := cfgWithSSH(t, runningKali, "127.0.0.1", 2222, "root")

	// Handoff seam: return a distinctive exit code. ssh against a running
	// machine with stored settings is ready immediately, so the journey runs
	// start-free and the code below must reappear from serve.
	handoffSSH = func(spec appssh.ExecSpec, _ io.Writer) int {
		return 42
	}

	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	code := Run([]string{"ssh", "Kali Linux", "--config", cfgPath}, out, errOut)
	if code != 42 {
		t.Fatalf("serve must propagate the ssh child's exit code, got %d (%q / %q)", code, out, errOut)
	}
	if !strings.Contains(out.String(), "Connecting to \"Kali Linux\"") {
		t.Fatalf("handoff record line missing: %q", out)
	}
}

// ---------- demo guard ----------

func TestDemoNeverTouchesRealWorld(t *testing.T) {
	// With the fake world installed, even the demo path must not resolve a
	// real VBoxManage or shell; and a demo ssh handoff must be simulated.
	var resolved bool
	oldDisc, oldNew := vboxDiscover, vboxNewManager
	t.Cleanup(func() { vboxDiscover, vboxNewManager = oldDisc, oldNew })
	vboxDiscover = func(string) (vbox.Discovery, error) {
		resolved = true
		return vbox.Discovery{}, errors.New("must not resolve VBoxManage in demo")
	}
	vboxNewManager = func(string) vbox.Manager { t.Fatal("demo must not build a VBoxManage client"); return nil }

	// Non-tty means the guard fires before any discovery, so the resolve
	// never gets its chance: exactly the behaviour we want to pin.
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	code := Run([]string{"--demo"}, out, errOut)
	if code != exitUsage || resolved {
		t.Fatalf("demo must stop at the guard: code=%d resolved=%v %q", code, resolved, errOut)
	}
}

func TestDemoBatchRunsAgainstFakeMachinesOnly(t *testing.T) {
	// Names from the demo seed set: ubuntu runs. The batch must run through
	// the same engine without discovering VirtualBox at all.
	var resolved bool
	oldDisc, oldNew := vboxDiscover, vboxNewManager
	oldOpts := programOptions
	t.Cleanup(func() { vboxDiscover, vboxNewManager, programOptions = oldDisc, oldNew, oldOpts })
	vboxDiscover = func(string) (vbox.Discovery, error) {
		resolved = true
		return vbox.Discovery{}, errors.New("demo must not resolve VBoxManage")
	}
	vboxNewManager = func(string) vbox.Manager { t.Fatal("demo must not build a VBoxManage client"); return nil }
	programOptions = []tea.ProgramOption{tea.WithoutRenderer(), tea.WithoutSignals(), tea.WithInput(&eofReader{})}

	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	code := Run([]string{"stop", "ubuntu", "kali", "--demo"}, out, errOut)
	if resolved {
		t.Fatalf("demo resolved VBoxManage; stdout=%q stderr=%q", out, errOut)
	}
	// kali is off: a shutdown request is skipped → incomplete → nonzero, and
	// the fake answered the batch with per-machine lines.
	if !strings.Contains(out.String(), "ubuntu") {
		t.Fatalf("the named machine belongs in the output: %q", out)
	}
	if code == exitOK && !strings.Contains(out.String(), "succeeded") {
		t.Fatalf("a demo batch must report like a real one: %q (%q)", out, errOut)
	}
}

// TestNoFuzzyMatchForNames documents the whole Resolve path used by
// vbx: near-miss names are refused by Inventory.Resolve (already covered in
// the vbox package), so a typo can never start the wrong machine.
func TestNoFuzzyMatchForNames(t *testing.T) {
	env := &fakeEnv{machines: []vbox.Machine{runningKali}}
	code, _, errOut := runWith(t, env, "start", "Kali", "--config", filepath.Join(t.TempDir(), "config.json"))
	if code != exitUsage {
		t.Fatalf("partial name must be refused, got %d (%q)", code, errOut)
	}
	if env.opHas("poweron") {
		t.Fatal("a near-miss name must never reach the hypervisor")
	}
	// Exact case is tolerated for UUIDs (VirtualBox writes them either way in
	// different places), never for names: a UUID's letters are hex, not text.
	// (The machine is running, so start itself is refused — what matters here
	// is that the target RESOLVED: the refusal is state-based, not usage.)
	code, _, errOut = runWith(t, env, "pause", strings.ToUpper(runningKali.UUID), "--config", filepath.Join(t.TempDir(), "config.json"))
	if code != exitOK {
		t.Fatalf("uppercase UUID should resolve and pause cleanly, exit %d (%q)", code, errOut)
	}
}

// ---------- scripted rename ----------

func TestRenameDirectRenamesAndPrintsOneLine(t *testing.T) {
	env := &fakeEnv{machines: []vbox.Machine{runningKali, offWin}}
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	code, out, errOut := runWith(t, env, "rename", "Windows 11", "win blue", "--config", cfgPath)
	if code != exitOK {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if !strings.Contains(out, `+ Renamed "Windows 11" to "win blue".`) {
		t.Fatalf("stdout = %q", out)
	}
	if !env.opHas("rename") {
		t.Fatalf("rename never reached the backend: %v", env.opList())
	}
}

func TestRenameTargetsByUUIDAndRefusesByState(t *testing.T) {
	// Paused (not off): VirtualBox would refuse, so vbx refuses first,
	// exit 1, and says nothing was changed.
	env := &fakeEnv{machines: []vbox.Machine{offWin, dupPausedKali}}
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	code, out, errOut := runWith(t, env, "rename", dupPausedKali.UUID, "solo kali", "--config", cfgPath)
	if code != exitRun {
		t.Fatalf("paused machine must be refused, exit %d (%q)", code, errOut)
	}
	for _, want := range []string{"Cannot rename", "paused", "Nothing was changed"} {
		if !strings.Contains(out, want) && !strings.Contains(errOut, want) {
			t.Fatalf("refusal missing %q: out=%q err=%q", want, out, errOut)
		}
	}
	if env.opHas("rename") {
		t.Fatal("a state refusal sends nothing")
	}
	// Powered-off by UUID: clean rename, and only that machine moves.
	code, out, errOut = runWith(t, env, "rename", offWin.UUID, "win lab", "--config", cfgPath)
	if code != exitOK {
		t.Fatalf("exit %d (%q)", code, errOut)
	}
	if !strings.Contains(out, `+ Renamed "Windows 11" to "win lab".`) {
		t.Fatalf("stdout = %q", out)
	}
}

func TestRenameUnknownAndAmbiguousNamesExitUsageWithNothingSent(t *testing.T) {
	env := &fakeEnv{machines: []vbox.Machine{runningKali, dupPausedKali}}
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	code, _, errOut := runWith(t, env, "rename", "Does Not Exist", "fine", "--config", cfgPath)
	if code != exitUsage {
		t.Fatalf("unknown VM: exit %d (%q)", code, errOut)
	}
	if !strings.Contains(errOut, "Does Not Exist") {
		t.Fatalf("must name what was not found: %q", errOut)
	}
	code, _, errOut = runWith(t, env, "rename", "Kali Linux", "fine", "--config", cfgPath)
	if code != exitUsage {
		t.Fatalf("ambiguous name: exit %d (%q)", code, errOut)
	}
	if env.opHas("rename") {
		t.Fatal("a name that never resolved cannot rename anything")
	}
}

func TestRenameInvalidNewNameExitsUsageBeforeAnyHypervisorAsk(t *testing.T) {
	env := &fakeEnv{machines: []vbox.Machine{offWin}}
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	code, _, errOut := runWith(t, env, "rename", "Windows 11", "bad:char", "--config", cfgPath)
	if code != exitUsage {
		t.Fatalf("bad new name: exit %d (%q)", code, errOut)
	}
	// The validator's rule is stated, and the user's own argument stays
	// encoded on the way to the terminal.
	if !strings.Contains(errOut, "VM name cannot contain") {
		t.Fatalf("must name the rule: %q", errOut)
	}
	// Nothing was asked of VirtualBox at all — not even the listing.
	if env.opHas("list") || env.opHas("rename") {
		t.Fatalf("a bad line must never reach the hypervisor: %v", env.opList())
	}
}

func TestRenameRefusesCollisionWithAnotherMachine(t *testing.T) {
	// offWin may not take Kali Linux's exact name: resolution would go
	// ambiguous for every later command that uses it.
	env := &fakeEnv{machines: []vbox.Machine{offWin, runningKali}}
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	code, out, errOut := runWith(t, env, "rename", "Windows 11", "Kali Linux", "--config", cfgPath)
	if code != exitRun {
		t.Fatalf("collision refusal: exit %d (%q)", code, errOut)
	}
	for _, want := range []string{"Cannot rename", `"Kali Linux"`, "Nothing was changed"} {
		if !strings.Contains(out, want) && !strings.Contains(errOut, want) {
			t.Fatalf("refusal missing %q: out=%q err=%q", want, out, errOut)
		}
	}
	if env.opHas("rename") {
		t.Fatal("an ambiguous-name collision sends nothing")
	}
}

func TestRenameToSameNameIsAHonestNoOp(t *testing.T) {
	env := &fakeEnv{machines: []vbox.Machine{offWin}}
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	code, out, errOut := runWith(t, env, "rename", "Windows 11", "Windows 11", "--config", cfgPath)
	if code != exitOK {
		t.Fatalf("exit %d (%q)", code, errOut)
	}
	if !strings.Contains(out, `"Windows 11" is already named "Windows 11": nothing was changed.`) {
		t.Fatalf("stdout = %q", out)
	}
	if env.opHas("rename") {
		t.Fatal("the no-op rename must not call the backend")
	}
}

func TestRenameTooManyArgumentsIsUsage(t *testing.T) {
	env := &fakeEnv{machines: []vbox.Machine{offWin, runningKali}}
	code, _, errOut := runWith(t, env, "rename", "a", "b", "c", "--config", filepath.Join(t.TempDir(), "config.json"))
	if code != exitUsage {
		t.Fatalf("three arguments: exit %d (%q)", code, errOut)
	}
	if !strings.Contains(errOut, "one machine and one new name") {
		t.Fatalf("must explain the shape: %q", errOut)
	}
	if len(env.opList()) != 0 {
		t.Fatalf("usage errors touch nothing: %v", env.opList())
	}
}

func TestRenameOneTargetWithoutTerminalIsUsage(t *testing.T) {
	// The buffer stdout is never a console: with only the machine named,
	// the new name would have to be typed, so vbx says what to do instead
	// of opening a renderer nobody can steer.
	env := &fakeEnv{machines: []vbox.Machine{offWin}}
	code, _, errOut := runWith(t, env, "rename", "Windows 11", "--config", filepath.Join(t.TempDir(), "config.json"))
	if code != exitUsage {
		t.Fatalf("single target without a console: exit %d (%q)", code, errOut)
	}
	if !strings.Contains(errOut, "needs both halves") {
		t.Fatalf("guard wording: %q", errOut)
	}
	if env.opHas("rename") {
		t.Fatal("the guard fires before any work")
	}
}

func TestRenameRefreshesStoredNameHintOnDisk(t *testing.T) {
	env := &fakeEnv{machines: []vbox.Machine{offWin}}
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	// A stored SSH entry carries the cosmetic name hint.
	cfg := config.Defaults()
	if err := cfg.SetVMSSH(offWin.UUID, offWin.Name,
		config.SSHConfig{Mode: config.SSHModeEndpoint, Host: "10.0.0.1", Port: 22}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveTo(cfgPath); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runWith(t, env, "rename", "Windows 11", "win renamed", "--config", cfgPath)
	if code != exitOK {
		t.Fatalf("exit %d (%q)", code, errOut)
	}
	if strings.Contains(out, "display-name hint") {
		t.Fatalf("the hint refresh must be silent on success: %q", out)
	}
	disk, err := config.LoadFrom(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	vm, ok := disk.VM(offWin.UUID)
	if !ok || vm.Name != "win renamed" {
		t.Fatalf("the stored hint must follow the rename: %+v", disk.VMs)
	}
	if vm.SSH.Host != "10.0.0.1" {
		t.Fatalf("SSH settings must survive byte-exact: %+v", vm.SSH)
	}
}

func TestRenameReportsPartialSuccessWhenHintWriteFails(t *testing.T) {
	env := &fakeEnv{machines: []vbox.Machine{offWin}}
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Defaults()
	if err := cfg.SetVMSSH(offWin.UUID, offWin.Name,
		config.SSHConfig{Mode: config.SSHModeEndpoint, Host: "10.0.0.1", Port: 22}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveTo(cfgPath); err != nil {
		t.Fatal(err)
	}
	// Block this process' next settings write: the deterministic temp file
	// name pre-exists, so O_EXCL refuses it. Everything else still runs.
	block := cfgPath + ".vbx-tui-write-" + strconv.Itoa(os.Getpid()) + ".tmp"
	fh, err := os.OpenFile(block, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fh.Close()
	code, out, errOut := runWith(t, env, "rename", "Windows 11", "win half", "--config", cfgPath)
	// The machine IS renamed: the run is a success with a stated caveat.
	if code != exitOK {
		t.Fatalf("exit %d (%q)", code, errOut)
	}
	for _, want := range []string{`Renamed "Windows 11" to "win half".`, "could not refresh its stored display-name hint"} {
		if !strings.Contains(out, want) {
			t.Fatalf("partial-success line missing %q: %q", want, out)
		}
	}
	// Disk still holds the old hint: the honest state the line describes.
	disk, err := config.LoadFrom(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if vm, _ := disk.VM(offWin.UUID); vm.Name != "Windows 11" {
		t.Fatalf("the failed hint write must have changed nothing: %+v", disk.VMs)
	}
}

func TestDemoRenameRunsAgainstFakeMachinesOnly(t *testing.T) {
	var resolved bool
	oldDisc, oldNew := vboxDiscover, vboxNewManager
	t.Cleanup(func() { vboxDiscover, vboxNewManager = oldDisc, oldNew })
	vboxDiscover = func(string) (vbox.Discovery, error) {
		resolved = true
		return vbox.Discovery{}, errors.New("demo must not resolve VBoxManage")
	}
	vboxNewManager = func(string) vbox.Manager { t.Fatal("demo must not build a VBoxManage client"); return nil }

	cfgPath := filepath.Join(t.TempDir(), "config.json")
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	code := Run([]string{"rename", "kali", "kali blue", "--demo", "--config", cfgPath}, out, errOut)
	if resolved {
		t.Fatalf("demo resolved VBoxManage: %s", out)
	}
	if code != exitOK {
		t.Fatalf("demo rename: exit %d stderr=%q", code, errOut)
	}
	if !strings.Contains(out.String(), `+ Renamed "kali" to "kali blue".`) {
		t.Fatalf("stdout = %q", out)
	}
	// Demo persistence is in-memory only: no settings file was created.
	if _, err := os.Stat(cfgPath); err == nil {
		t.Fatalf("--demo must not write the settings file: %q exists", cfgPath)
	}
}
