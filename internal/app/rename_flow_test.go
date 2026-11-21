package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aditya-shrivastavv/vbox-tui/internal/config"
)

// blockPath is the deterministic temp name writeAtomic gives this process;
// pre-creating it with O_EXCL semantics makes the next save fail for real,
// without touching anything else in the directory.
func blockPath(configPath string) string {
	return configPath + ".vbx-tui-write-" + itoaForTest(os.Getpid()) + ".tmp"
}

func itoaForTest(n int) string {
	if n == 0 {
		return "0"
	}
	buf := []byte{}
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}

// Rename: the menu flow, the prefilled prompt, the shared door's rules, and
// the honest partial-success wording. Everything runs against fakes; no
// VirtualBox, no real settings file (writes, where relevant, go to a temp
// path created by the test).

// ---------- the shared door ----------

func TestRenameDoorTargetsByUUIDAndChecksFreshState(t *testing.T) {
	off := vmOff()
	f := newFake(off)
	out, err := RenameMachine(t.Context(), f, off.UUID, "win blue")
	if err != nil {
		t.Fatalf("door: %v", err)
	}
	if out.Old != "Windows 11" || out.New != "win blue" || out.Same {
		t.Fatalf("outcome: %+v", out)
	}
	if !f.hasUUID("rename", off.UUID) {
		t.Fatalf("the rename must address the UUID: %v", f.opList())
	}
}

func TestRenameDoorRefusesWithoutTouching(t *testing.T) {
	run := vmRunning()
	f := newFake(run)
	_, err := RenameMachine(t.Context(), f, run.UUID, "nope")
	var refused *RenameRefused
	if !isRefused(err, &refused) {
		t.Fatalf("running must be refused before anything: %v", err)
	}
	if !strings.Contains(refused.Error(), "powered-off") {
		t.Errorf("refusal must point at the operator's next step: %v", refused)
	}
	if f.has("rename") {
		t.Fatal("a refused rename sends nothing")
	}
}

func TestRenameDoorExactSameNameIsNoOp(t *testing.T) {
	off := vmOff()
	f := newFake(off)
	out, err := RenameMachine(t.Context(), f, off.UUID, "Windows 11")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Same {
		t.Fatalf("pressing enter on the prefilled name must not send: %+v", out)
	}
	if f.has("rename") {
		t.Fatal("a no-op rename must not call the backend")
	}
	// A case-only change still goes through VirtualBox like any rename:
	// names are case-exact identities.
	out, err = RenameMachine(t.Context(), f, off.UUID, "windows 11")
	if err != nil || out.Same {
		t.Fatalf("case-only rename: %+v %v", out, err)
	}
	if !f.has("rename") {
		t.Fatal("the case change should have been sent")
	}
}

func TestRenameDoorRefusesDuplicateNames(t *testing.T) {
	off, other := vmOff(), anotherOff()
	other.Name = "existing"
	f := newFake(off, other)
	_, err := RenameMachine(t.Context(), f, off.UUID, "existing")
	var taken *RenameNameTaken
	if !isRefused(err, &taken) {
		t.Fatalf("another machine holding the name must stop the rename: %v", err)
	}
	if !strings.Contains(err.Error(), `named "existing"`) || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("the refusal must explain the ambiguity cost: %v", err)
	}
	if f.has("rename") {
		t.Fatal("an ambiguous name collision sends nothing")
	}
}

func TestRenameDoorRejectsInvalidNamesBeforeTheRead(t *testing.T) {
	f := newFake(vmOff())
	if _, err := RenameMachine(t.Context(), f, "not-a-uuid", "fine"); err == nil {
		t.Fatal("the door needs a UUID, never a name-shaped guess")
	}
	if _, err := RenameMachine(t.Context(), f, vmOff().UUID, "bad:char"); err == nil {
		t.Fatal("an invalid name never reaches the hypervisor")
	}
}

// ---------- the interactive menu flow ----------

func TestMenuRenameWalksPromptRenamesAndShowsResult(t *testing.T) {
	off := vmOff()
	f := newFake(off)
	s := start(t, modelWith(f, newStore(), dialRefused()))
	s.enter() // pick the machine
	s.settleTo(modePickAction, 2*time.Second)
	// Off menu: start, start-and-ssh, Rename… (index 2). Rename only ever
	// appears when VirtualBox would honour it.
	s.frameMustContain(t, "Rename…")
	s.down()
	s.down()
	s.enter()
	s.settleTo(modeText, 2*time.Second)
	s.shot()
	frame := s.strip()
	// The prompt is prefilled with the current name and says what Enter
	// on it would mean.
	if !strings.Contains(frame, "New name for \"Windows 11\"") || !strings.Contains(strings.ToLower(frame), "enter") {
		t.Fatalf("prefilled prompt:\n%s", frame)
	}
	if !strings.Contains(frame, "> Windows 11") {
		t.Fatalf("the edit line must hold the current name:\n%s", frame)
	}
	// Edit it away and submit: backspaces then a new tail.
	for i := 0; i < len("Windows 11"); i++ {
		s.key("backspace")
	}
	s.runes("win lab")
	s.enter()
	s.settleTo(modePickAction, 3*time.Second)
	s.shot()
	frame = s.strip()
	if !strings.Contains(frame, `Renamed "Windows 11" to "win lab".`) {
		t.Fatalf("result line above the menu:\n%s", frame)
	}
	if !f.hasUUID("rename", off.UUID) {
		t.Fatalf("renamed by UUID: %v", f.opList())
	}
}

func TestMenuRenameEscapeChangesNothing(t *testing.T) {
	off := vmOff()
	f := newFake(off)
	s := start(t, modelWith(f, newStore(), dialRefused()))
	s.enter()
	s.settleTo(modePickAction, 2*time.Second)
	s.down()
	s.down()
	s.enter()
	s.settleTo(modeText, 2*time.Second)
	// Type something first: esc must not send it and must not keep it for
	// next time.
	s.runes("half-typed")
	s.esc()
	s.settleTo(modePickAction, 2*time.Second)
	if f.has("rename") {
		t.Fatalf("esc sends nothing: %v", f.opList())
	}
	// Walk back in: the prompt is freshly prefilled, not holding the ghost.
	s.down()
	s.down()
	s.enter()
	s.settleTo(modeText, 2*time.Second)
	s.shot()
	if strings.Contains(s.strip(), "half-typed") {
		t.Fatalf("a cancelled name must not leak into the next prompt:\n%s", s.strip())
	}
	s.esc()
	s.settleTo(modePickAction, 2*time.Second)
}

func TestRenamePromptKeepsRejectedInput(t *testing.T) {
	off := vmOff()
	f := newFake(off)
	s := start(t, modelWith(f, newStore(), dialRefused()))
	s.enter()
	s.settleTo(modePickAction, 2*time.Second)
	s.down()
	s.down()
	s.enter()
	s.settleTo(modeText, 2*time.Second)
	// Clear the prefilled name, then type one VirtualBox itself would
	// refuse. The refusal names the rule, and the edit line still holds
	// every character typed: a rejected name is a question, not a loss.
	for i := 0; i < len("Windows 11"); i++ {
		s.key("backspace")
	}
	s.runes("bad:char")
	s.enter()
	s.shot()
	frame := s.strip()
	if !strings.Contains(frame, `VM name cannot contain ":"`) {
		t.Fatalf("the refusal must name the rule:\n%s", frame)
	}
	if !strings.Contains(frame, "> bad:char") {
		t.Fatalf("rejected input must not be destroyed:\n%s", frame)
	}
	if f.has("rename") {
		t.Fatal("an invalid name never reaches VirtualBox")
	}
	// Fix it in place and the rename proceeds from the same line.
	for i := 0; i < len("bad:char"); i++ {
		s.key("backspace")
	}
	s.runes("fixed name")
	s.enter()
	s.settleTo(modePickAction, 3*time.Second)
	s.shot()
	if !strings.Contains(s.strip(), `Renamed "Windows 11" to "fixed name".`) {
		t.Fatalf("menu result line:\n%s", s.strip())
	}
}

func TestRenamePromptUnchangedEnterIsNoOp(t *testing.T) {
	f := newFake(vmOff())
	s := start(t, modelWith(f, newStore(), dialRefused()))
	s.enter()
	s.settleTo(modePickAction, 2*time.Second)
	s.down()
	s.down()
	s.enter()
	s.settleTo(modeText, 2*time.Second)
	s.enter() // Enter on the prefilled name: nothing to send
	s.settleTo(modePickAction, 2*time.Second)
	s.shot()
	frame := s.strip()
	if !strings.Contains(frame, "keeps its name") {
		t.Fatalf("an unchanged answer must say so: %s", frame)
	}
	if f.has("rename") {
		t.Fatal("the no-op rename sent nothing")
	}
}

func TestFocusedRenameRunsToResultAndCancelsCleanly(t *testing.T) {
	off := vmOff()
	f := newFake(off)
	o := baseOptions(f, newStore(), dialRefused())
	o.Direct = &DirectRun{Action: ActionRename, VM: off, RenameTo: "renamed by line"}
	s := start(t, New(o))
	s.settleOutcome(3 * time.Second)
	got := s.outcome(OutcomeSuccess)
	if !strings.Contains(got, `Renamed "Windows 11" to "renamed by line".`) {
		t.Fatalf("focused result: %q", got)
	}
	if !f.hasUUID("rename", off.UUID) {
		t.Fatalf("ops: %v", f.opList())
	}

	// Cancel from the prefilled prompt ends the run with the honest
	// nothing-was-changed line.
	f2 := newFake(vmOff())
	o2 := baseOptions(f2, newStore(), dialRefused())
	o2.Direct = &DirectRun{Action: ActionRename, VM: vmOff()}
	s2 := start(t, New(o2))
	s2.settleTo(modeText, 2*time.Second)
	s2.esc()
	s2.settleOutcome(2 * time.Second)
	got = s2.outcome(OutcomeCancelled)
	if !strings.Contains(got, "Nothing was changed") {
		t.Fatalf("cancel line: %q", got)
	}
	if f2.has("rename") {
		t.Fatal("cancelling sent nothing")
	}
}

func TestFocusedRenameRefusesRunningMachineUpFront(t *testing.T) {
	run := vmRunning()
	f := newFake(run)
	o := baseOptions(f, newStore(), dialRefused())
	o.Direct = &DirectRun{Action: ActionRename, VM: run, RenameTo: "nope"}
	s := start(t, New(o))
	s.settleOutcome(2 * time.Second)
	got := s.outcome(OutcomeFailure)
	// The refusal states the rule and the operator's next step; no prompt
	// for typing, no request.
	for _, want := range []string{"Cannot rename", "powered-off", "Nothing was changed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("refusal missing %q: %q", want, got)
		}
	}
	if f.has("rename") {
		t.Fatal("a running machine's rename never left the app")
	}
}

// ---------- the cosmetic hint and its honest failure ----------

func TestRenameRefreshesStoredHint(t *testing.T) {
	off := vmOff()
	f := newFake(off)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// A stored entry (SSH settings) carries the display-name hint; an
	// entry-less machine has none, and rename must not invent one.
	cfg := config.Defaults()
	if err := cfg.SetVMSSH(off.UUID, "Windows 11",
		config.SSHConfig{Mode: config.SSHModeEndpoint, Host: "10.0.0.1", Port: 22}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	o := baseOptions(f, newStore(), dialRefused())
	o.Config = cfg // persist: the model writes through to path
	o.Direct = &DirectRun{Action: ActionRename, VM: off, RenameTo: "win new"}
	s := start(t, New(o))
	s.settleOutcome(3 * time.Second)
	got := s.outcome(OutcomeSuccess)
	if !strings.Contains(got, `Renamed "Windows 11" to "win new".`) {
		t.Fatalf("rename line: %q", got)
	}
	if strings.Contains(got, "display-name hint") {
		t.Fatalf("the hint write should have succeeded quietly: %q", got)
	}
	disk, err := config.LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if vm, ok := disk.VM(off.UUID); !ok || vm.Name != "win new" {
		t.Fatalf("hint not refreshed on disk: %+v", disk.VMs)
	}
	// The SSH settings themselves survived the rename untouched: they are
	// keyed by UUID, and a rename changes no UUID.
	if vm, _ := disk.VM(off.UUID); vm.SSH.Host != "10.0.0.1" {
		t.Fatalf("SSH settings must survive a rename: %+v", vm)
	}
}

func TestRenameReportsPartialSuccessWhenHintWriteFails(t *testing.T) {
	off := vmOff()
	f := newFake(off)
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Defaults()
	if err := cfg.SetVMSSH(off.UUID, off.Name,
		config.SSHConfig{Mode: config.SSHModeEndpoint, Host: "10.0.0.1", Port: 22}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	// This process' next settings write must fail for real: block the
	// deterministic temp file (O_EXCL refuses it), while the machine
	// rename itself still succeeds through the fake backend.
	block, err := os.OpenFile(blockPath(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	block.Close()
	o := baseOptions(f, newStore(), dialRefused())
	o.Config = cfg
	o.Direct = &DirectRun{Action: ActionRename, VM: off, RenameTo: "win new"}
	s := start(t, New(o))
	s.settleOutcome(3 * time.Second)
	got := s.outcome(OutcomeSuccess) // the machine IS renamed: success with a caveat
	for _, want := range []string{`Renamed "Windows 11" to "win new".`, "could not refresh its stored display-name hint"} {
		if !strings.Contains(got, want) {
			t.Fatalf("partial success missing %q: %q", want, got)
		}
	}
	// VirtualBox renamed it; the disk still carries the old hint — the
	// honest state, and exactly what the line says.
	disk, err := config.LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if vm, ok := disk.VM(off.UUID); !ok || vm.Name != off.Name {
		t.Fatalf("the failed hint write must have changed nothing on disk: %+v", disk.VMs)
	}
}

// ---------- helpers ----------

// The cosmetic hint door at unit level: no stored entry means the rename
// touches no file at all — a hint must never fabricate a settings entry.
func TestNameCacheNeverFabricatesSettingsEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := config.LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	attempted, err := (nameCache{cfg: cfg, persist: true}).set(vmOff().UUID, "win new")
	if attempted || err != nil {
		t.Fatalf("no stored entry means no write: attempted=%v err=%v", attempted, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("nothing may be written for an entry-less machine: err=%v", err)
	}
}

func (s *session) frameMustContain(t *testing.T, text string) {
	t.Helper()
	s.shot()
	if !strings.Contains(s.strip(), text) {
		t.Fatalf("frame missing %q:\n%s", text, s.strip())
	}
}

func isRefused[T error](err error, target *T) bool {
	return errors.As(err, target)
}
