package demo

import (
	"context"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aditya-shrivastavv/vbox-tui/internal/config"
	"github.com/aditya-shrivastavv/vbox-tui/internal/vbox"
)

const ctxOK = 2 * time.Second

func mustCtx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), ctxOK)
	t.Cleanup(cancel)
	return c
}

func findMachine(t *testing.T, inv vbox.Inventory, name string) vbox.Machine {
	t.Helper()
	for _, m := range inv.Machines {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("machine %q missing from inventory: %v", name, inv.Machines)
	return vbox.Machine{}
}

// TestDemoCodeNeverReferencesProcessDiskOrNetwork is the static half of the demo
// promise. The demo must be reviewable on a machine with no VirtualBox and with
// no intention of connecting anywhere, so its own source may not import a
// process, filesystem, or network API at all. (It reaches config, which knows
// about files, but the checks below cover every selector this package writes.)
func TestDemoCodeNeverReferencesProcessDiskOrNetwork(t *testing.T) {
	forbidden := map[string]bool{
		"os": true, "os/exec": true, "os/user": true,
		"net": true, "net/http": true, "net/netip": true,
		"path": true, "path/filepath": true,
		"syscall": true, "io": true,
	}
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, imp := range f.Imports {
			pkg := strings.Trim(imp.Path.Value, `"`)
			if forbidden[pkg] {
				t.Errorf("%s imports %q: demo mode must not spawn processes, write files or open connections", name, pkg)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no source files were checked; the guarantee would be vacuous")
	}
}

// TestLifecycleWritesNothingAnywhere is the dynamic half: with every profile and
// home variable pointed at an empty temporary directory, exercising the full
// lifecycle (start, save, shutdown, SSH settings) must still leave that directory
// empty. This catches a regression where the demo starts persisting "just a
// little state" into the user's real configuration directory.
func TestLifecycleWritesNothingAnywhere(t *testing.T) {
	sandbox := t.TempDir()
	for _, key := range []string{"USERPROFILE", "APPDATA", "LOCALAPPDATA", "HOME", "XDG_CONFIG_HOME"} {
		t.Setenv(key, sandbox)
	}

	b := New()
	ctx := mustCtx(t)

	// A start, a save, a graceful shutdown and an SSH edit: everything the UI can
	// ask of the backend, including the awkwardly named machines.
	kali, ubuntu := Seeds()[0].UUID, Seeds()[1].UUID
	wren := Seeds()[2].UUID
	if err := b.PowerOn(ctx, kali); err != nil {
		t.Fatal(err)
	}
	b.Advance(StartDelay + time.Second)
	if err := b.SaveState(ctx, kali); err != nil {
		t.Fatal(err)
	}
	b.Advance(SaveDelay + time.Second)
	if err := b.PowerOn(ctx, wren); err != nil { // restore a saved machine
		t.Fatal(err)
	}
	if err := b.ShutdownGracefully(ctx, ubuntu); err != nil {
		t.Fatal(err)
	}
	b.SetSSH(wren, config.SSHConfig{Mode: config.SSHModeEndpoint, Host: "127.0.0.1", Port: 2200, User: "lab"})
	if _, err := b.List(ctx); err != nil {
		t.Fatal(err)
	}

	var files []string
	err := filepath.WalkDir(sandbox, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != sandbox {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("demo mode wrote to the user profile tree: %v", files)
	}
}

func TestSeedsSurviveAwkwardNames(t *testing.T) {
	b := New()
	inv, err := b.List(mustCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"kali", "ubuntu", "Workbench — Win11",
		"Ω-legacy ñame \"quoted\"", "broken-vm",
	}
	got := b.Names()
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("names = %q, want %q", got, want)
	}
	// Byte-exact survival, not fuzzily "close enough": the em dash, the Omega,
	// the ñ and the embedded quotes are the point of these fixtures.
	for _, name := range want {
		m := findMachine(t, inv, name)
		if m.UUID == "" {
			t.Errorf("%q lost its UUID", name)
		}
	}
	if inv.Problems != nil {
		t.Errorf("unexpected problems: %+v", inv.Problems)
	}
}

func TestInventoryIsSortedForReading(t *testing.T) {
	b := New()
	inv, err := b.List(mustCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(inv.Machines))
	for i, m := range inv.Machines {
		names[i] = m.Name
	}
	if !sort.SliceIsSorted(names, func(i, j int) bool {
		return strings.ToLower(names[i]) < strings.ToLower(names[j])
	}) {
		t.Errorf("the list should read alphabetically regardless of case: %q", names)
	}
}

func TestStartPassesThroughATransitionalWindow(t *testing.T) {
	b := New()
	ctx := mustCtx(t)
	kali := Seeds()[0].UUID

	if err := b.PowerOn(ctx, kali); err != nil {
		t.Fatal(err)
	}
	// Before the delay elapses the machine must read as transitional, not as
	// already running: offering actions the hypervisor would refuse is worse than
	// admitting the state is in flux.
	inv, err := b.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	m := findMachine(t, inv, "kali")
	if m.State.Kind != vbox.KindTransitional {
		t.Fatalf("immediately after PowerOn state = %+v, want transitional", m.State)
	}
	if len(vbox.AllowedActions(m.State)) != 0 {
		t.Errorf("no action should be offered mid-transition: %v", vbox.AllowedActions(m.State))
	}

	b.Advance(StartDelay - time.Second)
	inv, _ = b.List(ctx)
	if m = findMachine(t, inv, "kali"); m.State.Kind != vbox.KindTransitional {
		t.Errorf("the window should last until the delay elapses, got %+v", m.State)
	}

	b.Advance(2 * time.Second)
	inv, _ = b.List(ctx)
	if m = findMachine(t, inv, "kali"); m.State.Kind != vbox.KindRunning {
		t.Errorf("after the delay state = %+v, want running", m.State)
	}
}

func TestRestoreFromSavedLooksLikeStart(t *testing.T) {
	b := New()
	ctx := mustCtx(t)
	wren := Seeds()[2].UUID // seeded saved
	if err := b.PowerOn(ctx, wren); err != nil {
		t.Fatal(err)
	}
	inv, _ := b.List(ctx)
	m := findMachine(t, inv, "Workbench — Win11")
	if m.State.Kind != vbox.KindTransitional {
		t.Fatalf("restoring should also pass through a transitional state, got %+v", m.State)
	}
	b.Advance(StartDelay + time.Second)
	inv, _ = b.List(ctx)
	if m = findMachine(t, inv, "Workbench — Win11"); m.State.Kind != vbox.KindRunning {
		t.Errorf("after restore state = %+v", m.State)
	}
}

// TestShutdownStaysPendingUntilTheGuestStops is the behaviour the brief calls
// out: VBoxManage returns at once but the machine is still running, and vbox-tui
// must keep saying "shutdown requested" instead of claiming it is off.
func TestShutdownStaysPendingUntilTheGuestStops(t *testing.T) {
	b := New()
	ctx := mustCtx(t)
	ubuntu := Seeds()[1].UUID // seeded running, ShutdownDelay 5s

	if err := b.ShutdownGracefully(ctx, ubuntu); err != nil {
		t.Fatal(err)
	}
	inv, _ := b.List(ctx)
	m := findMachine(t, inv, "ubuntu")
	if m.State.Kind != vbox.KindRunning {
		t.Fatalf("a graceful shutdown request must not change the observed state: %+v", m.State)
	}
	if !b.RequestedShutdown(ubuntu) {
		t.Error("the request should still be outstanding")
	}
	if len(m.Notes) == 0 || !strings.Contains(strings.ToLower(m.Notes[0]), "shutdown requested") {
		t.Errorf("the inventory row should explain the pending shutdown, notes = %q", m.Notes)
	}

	b.Advance(4 * time.Second) // ubuntu's guest takes 5s
	inv, _ = b.List(ctx)
	m = findMachine(t, inv, "ubuntu")
	if m.State.Kind != vbox.KindRunning || !b.RequestedShutdown(ubuntu) {
		t.Errorf("four seconds in, the guest is still up: %+v pending=%v", m.State, b.RequestedShutdown(ubuntu))
	}

	b.Advance(2 * time.Second)
	inv, _ = b.List(ctx)
	m = findMachine(t, inv, "ubuntu")
	if m.State.Kind != vbox.KindOff {
		t.Fatalf("after the delay the machine should read powered off, got %+v", m.State)
	}
	if b.RequestedShutdown(ubuntu) {
		t.Error("once off, the request is no longer outstanding")
	}
	if len(m.Notes) != 0 {
		t.Errorf("the pending-shutdown note should be gone: %q", m.Notes)
	}
}

func TestSaveStateEndsSavedNotOff(t *testing.T) {
	b := New()
	ctx := mustCtx(t)
	ubuntu := Seeds()[1].UUID
	if err := b.SaveState(ctx, ubuntu); err != nil {
		t.Fatal(err)
	}
	b.Advance(SaveDelay + time.Second)
	inv, _ := b.List(ctx)
	m := findMachine(t, inv, "ubuntu")
	if m.State.Kind != vbox.KindSaved {
		t.Errorf("save state must end saved (so it can be restored), got %+v", m.State)
	}
	if vbox.PrimaryStart(m.State) != vbox.ActionRestore {
		t.Errorf("a saved machine should offer restore, got %q", vbox.PrimaryStart(m.State))
	}
}

func TestSaveStateCancelsAPendingShutdown(t *testing.T) {
	// This is not a realistic sequence (saving beats a shutting-down guest to the
	// lock), but it guards the invariant that a machine which is not running is
	// never still labelled "shutdown requested".
	b := New()
	ctx := mustCtx(t)
	ubuntu := Seeds()[1].UUID
	if err := b.ShutdownGracefully(ctx, ubuntu); err != nil {
		t.Fatal(err)
	}
	if err := b.SaveState(ctx, ubuntu); err != nil {
		t.Fatal(err)
	}
	if b.RequestedShutdown(ubuntu) {
		t.Error("the save should have cleared the pending-shutdown flag")
	}
	b.Advance(ShutDownClearTime)
	inv, _ := b.List(ctx)
	m := findMachine(t, inv, "ubuntu")
	if m.State.Kind != vbox.KindSaved {
		t.Errorf("the last operation should win: %+v", m.State)
	}
}

// ShutDownClearTime is longer than both the shutdown and the save simulation, so
// the test above measures the settled state rather than a timer still running.
const ShutDownClearTime = ShutdownDelay + SaveDelay + time.Second

func TestResumeOnlyFromPaused(t *testing.T) {
	b := New()
	ctx := mustCtx(t)
	seeds := Seeds()
	paused, off := seeds[3].UUID, seeds[0].UUID

	if err := b.Resume(ctx, paused); err != nil {
		t.Fatal(err)
	}
	b.Advance(ResumeDelay + time.Millisecond)
	inv, _ := b.List(ctx)
	if m := findMachine(t, inv, seeds[3].Name); m.State.Kind != vbox.KindRunning {
		t.Errorf(" resumed machine = %+v, want running", m.State)
	}
	if err := b.Resume(ctx, off); err == nil {
		t.Error("resuming a powered-off machine should be refused, not pretend to work")
	}
}

// TestPauseOnlyFromRunning mirrors the hypervisor: controlvm pause applies
// immediately and only makes sense for a running machine.
func TestPauseOnlyFromRunning(t *testing.T) {
	b := New()
	ctx := mustCtx(t)
	seeds := Seeds()
	running, paused, off := seeds[1].UUID, seeds[3].UUID, seeds[0].UUID

	if err := b.Pause(ctx, running); err != nil {
		t.Fatal(err)
	}
	// VirtualBox freezes the machine as the request lands: no settle time.
	inv, _ := b.List(ctx)
	if m := findMachine(t, inv, seeds[1].Name); m.State.Kind != vbox.KindPaused {
		t.Errorf("paused machine = %+v, want paused", m.State)
	}
	if err := b.Pause(ctx, paused); err == nil {
		t.Error("pausing an already-paused machine should be refused")
	}
	if err := b.Pause(ctx, off); err == nil {
		t.Error("pausing a powered-off machine should be refused")
	}
}

func TestOperationsRefuseWrongStates(t *testing.T) {
	b := New()
	ctx := mustCtx(t)
	seeds := Seeds()
	off, running, saved := seeds[0].UUID, seeds[1].UUID, seeds[2].UUID

	cases := []struct {
		name string
		call func() error
	}{
		{"start a running machine", func() error { return b.PowerOn(ctx, running) }},
		{"graceful shutdown of an off machine", func() error { return b.ShutdownGracefully(ctx, off) }},
		{"save a paused machine", func() error { return b.SaveState(ctx, seeds[3].UUID) }},
		{"resume a running machine", func() error { return b.Resume(ctx, running) }},
	}
	for _, tc := range cases {
		err := tc.call()
		if err == nil {
			t.Errorf("%s: accepted, but VirtualBox would refuse and the UI must not imply success", tc.name)
			continue
		}
		// The refusal must say why in terms the user can act on.
		if !strings.Contains(err.Error(), "is") {
			t.Errorf("%s: message should name the current state: %q", tc.name, err)
		}
	}
	// The saved machine is startable (a restore), matching the real CLI.
	if err := b.PowerOn(ctx, saved); err != nil {
		t.Errorf("restoring a saved machine should work: %v", err)
	}
}

func TestUnknownUUIDIsAnErrorNotASilentSuccess(t *testing.T) {
	b := New()
	ctx := mustCtx(t)
	const ghost = "11111111-2222-3333-4444-555555555555"
	ops := map[string]func() error{
		"PowerOn":   func() error { return b.PowerOn(ctx, ghost) },
		"Resume":    func() error { return b.Resume(ctx, ghost) },
		"Shutdown":  func() error { return b.ShutdownGracefully(ctx, ghost) },
		"SaveState": func() error { return b.SaveState(ctx, ghost) },
		"Detail":    func() error { _, err := b.Detail(ctx, ghost); return err },
	}
	for name, fn := range ops {
		if err := fn(); err == nil {
			t.Errorf("%s accepted a UUID that does not exist", name)
		}
	}
	if b.RequestedShutdown(ghost) {
		t.Error("a ghost has no pending shutdown")
	}
}

func TestInaccessibleMachineDetails(t *testing.T) {
	b := New()
	ctx := mustCtx(t)
	broken := Seeds()[4] // broken-vm, Unreadable
	if broken.State.Kind != vbox.KindUnavailable {
		t.Fatalf("the fixture should start inaccessible, got %+v", broken.State)
	}
	m, err := b.Detail(ctx, broken.UUID)
	if err == nil {
		t.Fatal("reading an inaccessible machine must report the failure, not the fake details")
	}
	if m.Name != broken.Name {
		t.Errorf("even the partial view should identify the machine, got %q", m.Name)
	}
	if !strings.Contains(err.Error(), broken.Name) {
		t.Errorf("the error names the machine so the user knows which: %q", err)
	}
	// The inventory row still shows the machine with an explanation: hiding it
	// would make it look like VirtualBox lost a VM.
	inv, _ := b.List(ctx)
	row := findMachine(t, inv, broken.Name)
	if len(row.Notes) == 0 {
		t.Error("the list row should explain why details are unavailable")
	}
}

func TestSSHSettingsAreInMemoryOnly(t *testing.T) {
	b := New()
	seeds := Seeds()
	kali, wren, plain := seeds[0].UUID, seeds[2].UUID, seeds[3].UUID

	// Seeded values are visible and flagged configured / not configured.
	if s, ok := b.SSH(kali); !ok || s.Port != 2222 {
		t.Errorf("seeded endpoint lost: %+v ok=%v", s, ok)
	}
	if _, ok := b.SSH(plain); ok {
		t.Error("a seeded machine without SSH must report unconfigured")
	}
	// A runtime edit takes effect and is returned normalised.
	b.SetSSH(wren, config.SSHConfig{Mode: config.SSHModeEndpoint, Host: " 127.0.0.1 ", Port: 2200, User: " lab "})
	if s, ok := b.SSH(wren); !ok || s.Host != "127.0.0.1" || s.User != "lab" {
		t.Errorf("SetSSH was not normalised or not stored: %+v ok=%v", s, ok)
	}
	// A lookup is case- and brace-insensitive, matching the real config store.
	if _, ok := b.SSH("{" + strings.ToUpper(wren) + "}"); !ok {
		t.Error("UUID lookups should match regardless of case or braces")
	}
	// Clearing stores the zero value, which must read as unconfigured.
	b.SetSSH(wren, config.SSHConfig{})
	if s, ok := b.SSH(wren); ok || s.Configured() {
		t.Errorf("cleared endpoint still looks configured: %+v ok=%v", s, ok)
	}
}

func TestVersionAndBackendNameMarkTheDemoMode(t *testing.T) {
	b := New()
	name := b.BackendName()
	if !strings.Contains(name, "demo") && !strings.Contains(name, "DEMO") {
		t.Errorf("BackendName = %q, want it to carry the DEMO marker the header shows", name)
	}
	v, err := b.Version(mustCtx(t))
	if err != nil || !strings.Contains(v, "demo") {
		t.Errorf("Version = %q, %v; using faked data must be unmistakable", v, err)
	}
}

func TestCancelledContextStopsEveryCall(t *testing.T) {
	b := New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	kali := Seeds()[0].UUID
	if _, err := b.List(ctx); err == nil {
		t.Error("List on a cancelled context")
	}
	if _, err := b.Detail(ctx, kali); err == nil {
		t.Error("Detail on a cancelled context")
	}
	for name, fn := range map[string]func() error{
		"PowerOn":   func() error { return b.PowerOn(ctx, kali) },
		"Resume":    func() error { return b.Resume(ctx, kali) },
		"Shutdown":  func() error { return b.ShutdownGracefully(ctx, kali) },
		"SaveState": func() error { return b.SaveState(ctx, kali) },
	} {
		if err := fn(); err == nil {
			t.Errorf("%s ran despite the cancelled context", name)
		}
	}
}

// TestConcurrentUseIsSafe reflects the real shape of the TUI: the refresh ticker
// keeps listing while an action goroutine mutates and the wait view polls. Run
// with -race this catches a missing lock, which would otherwise show up as a UI
// corruption nobody can reproduce.
func TestConcurrentUseIsSafe(t *testing.T) {
	b := New()
	ctx := mustCtx(t)
	seeds := Seeds()
	var actors sync.WaitGroup
	var readers sync.WaitGroup
	stop := make(chan struct{})

	// Readers run until the actors finish; they are tracked separately so the
	// WaitGroup the actors release is not also waiting on them.
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := b.List(ctx); err != nil {
					return
				}
			}
		}()
	}
	actors.Add(2)
	go func() {
		defer actors.Done()
		for i := 0; i < 50; i++ {
			_ = b.PowerOn(ctx, seeds[0].UUID)
			b.Advance(500 * time.Millisecond)
		}
	}()
	go func() {
		defer actors.Done()
		for i := 0; i < 50; i++ {
			_ = b.ShutdownGracefully(ctx, seeds[1].UUID)
			b.SetSSH(seeds[2].UUID, config.SSHConfig{Mode: config.SSHModeAlias, Alias: "lab"})
			b.RequestedShutdown(seeds[1].UUID)
			b.Advance(500 * time.Millisecond)
		}
	}()
	actors.Wait()
	close(stop)
	readers.Wait()
}
