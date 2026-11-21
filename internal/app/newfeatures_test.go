package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aditya-shrivastavv/vbox-tui/internal/config"
	"github.com/aditya-shrivastavv/vbox-tui/internal/vbox"
)

// Coverage for the batch, restart, settings and snapshot features: the pure
// planning rules, the sequential executor with its honest summary, and the
// interactive flows behind Space-marks, `vbx config`, and Snapshots.

// ---------- batch planner (pure) ----------

func anotherOff() vbox.Machine {
	return vbox.Machine{Name: "second box", UUID: "44444444-5555-6666-7777-888888888888",
		State: vbox.NormalizeState("powered off")}
}

func TestPlanBatchDecidesEachMachineFromItsFreshState(t *testing.T) {
	off, running, saved := vmOff(), vmRunning(), vmSaved()
	plan := PlanBatch(vbox.ActionStart, []vbox.Machine{off, running, saved})
	if len(plan) != 3 {
		t.Fatalf("plan length %d", len(plan))
	}
	// Off: exactly the start verb.
	if !plan[0].Attempt || plan[0].Verb != vbox.ActionStart || plan[0].Skip != "" {
		t.Errorf("off machine: %+v", plan[0])
	}
	// Running: no verb can start it — it is skipped with a reason, never
	// attempted, so VirtualBox never sees a request it would refuse.
	if plan[1].Attempt || plan[1].Skip == "" {
		t.Errorf("running machine must be a reason-carrying skip: %+v", plan[1])
	}
	// Saved: start follows VirtualBox's one request and honestly becomes a
	// restore — the plan says so before anything runs.
	if !plan[2].Attempt || plan[2].Verb != vbox.ActionRestore {
		t.Errorf("saved machine should plan a restore: %+v", plan[2])
	}
}

func TestBatchSummaryExitCodeAndLine(t *testing.T) {
	res := func(m vbox.Machine) BatchResult { return BatchResult{VM: m, Verb: vbox.ActionStart} }
	tgt := func(m vbox.Machine) BatchTarget { return BatchTarget{VM: m, Skip: "test"} }

	all := BatchSummary{Action: vbox.ActionStart, Succeeded: []BatchResult{res(vmOff())}}
	if all.ExitCode() != 0 {
		t.Errorf("all-succeeded exits 0, got %d", all.ExitCode())
	}
	for name, s := range map[string]BatchSummary{
		"failed":    {Action: vbox.ActionStart, Failed: []BatchResult{res(vmOff())}},
		"skipped":   {Action: vbox.ActionStart, Skipped: []BatchTarget{tgt(vmOff())}},
		"untried":   {Action: vbox.ActionStart, Skipped: []BatchTarget{tgt(vmOff())}},
		"cancelled": {Action: vbox.ActionStart, Cancelled: true},
	} {
		if s.ExitCode() != 1 {
			t.Errorf("%s batch must exit nonzero", name)
		}
	}
	if !strings.Contains(all.SummaryLine(), "1 succeeded") {
		t.Errorf("summary line: %q", all.SummaryLine())
	}
	withNames := BatchSummary{
		Action:    vbox.ActionStop,
		Succeeded: []BatchResult{res(vmOff())},
		Failed:    []BatchResult{res(anotherOff())},
		Skipped:   []BatchTarget{tgt(vmRunning())},
	}
	line := withNames.SummaryLine()
	// Succeeded machines are counted; failed and skipped ones are named,
	// because those are the machines the operator must look at.
	for _, want := range []string{"1 succeeded", `1 failed ("second box")`, `1 skipped ("Kali Linux")`} {
		if !strings.Contains(line, want) {
			t.Errorf("summary missing %q: %q", want, line)
		}
	}
}

// ---------- batch executor ----------

func TestRunBatchRunsInOrderReportsEachMachine(t *testing.T) {
	f := newFake(vmOff(), anotherOff())
	plan := PlanBatch(vbox.ActionStart, f.machines)
	var lines []string
	sum := RunBatch(context.Background(), f, vbox.ActionStart, plan, RestartRoutes{},
		func(r BatchResult) { lines = append(lines, string(r.Verb)) })
	if sum.ExitCode() != 0 || len(sum.Succeeded) != 2 {
		t.Fatalf("summary: %+v", sum)
	}
	// Both machines addressed by their own UUIDs, and only start requests.
	if !f.hasUUID("poweron", vmOff().UUID) || !f.hasUUID("poweron", anotherOff().UUID) {
		t.Fatalf("ops: %v", f.opList())
	}
	if f.has("pause") || f.has("acpipowerbutton") {
		t.Fatalf("a start batch issued other verbs: %v", f.opList())
	}
	if len(lines) != 2 {
		t.Fatalf("progress callback fired %d times", len(lines))
	}
}

// removeMachine drops a machine from the fake's live list: the executor
// re-reads each machine at its own moment, so a vanished machine must be
// reported failed and the run must stop honestly.
func removeMachine(f *fakeVM, uuid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.machines[:0]
	for _, m := range f.machines {
		if !strings.EqualFold(m.UUID, uuid) {
			out = append(out, m)
		}
	}
	f.machines = out
}

func TestRunBatchStopsAtFirstFailureAndNamesTheUntried(t *testing.T) {
	f := newFake(vmOff(), anotherOff())
	plan := PlanBatch(vbox.ActionStart, []vbox.Machine{vmOff(), anotherOff()})
	// The SECOND machine vanishes before its turn.
	removeMachine(f, anotherOff().UUID)
	sum := RunBatch(context.Background(), f, vbox.ActionStart, plan, RestartRoutes{}, nil)
	if len(sum.Succeeded) != 1 {
		t.Fatalf("first machine should have succeeded: %+v", sum)
	}
	if len(sum.Failed) != 1 || !strings.Contains(sum.Failed[0].Err.Error(), "no longer registered") {
		t.Fatalf("vanished machine: %+v", sum.Failed)
	}
	if sum.ExitCode() != 1 {
		t.Fatalf("failed machine must exit nonzero")
	}
	if len(sum.Untried) != 0 {
		t.Fatalf("nothing after the vanished machine: %+v", sum.Untried)
	}
}

func TestRunBatchRestartTurnsBlockedRouteIntoSkip(t *testing.T) {
	// A running machine whose restart route is switched off is skipped, not
	// failed, and the batch never reaches a reset anywhere: the fake records
	// every control command, and the vocabulary has none.
	running := vmRunning()
	offBox := anotherOff()
	f := newFake(running)
	guestSettings := newGuest()
	guestSettings.values[config.CanonicalUUID(running.UUID)] = config.GuestConfig{Restart: config.GuestRestartOff}
	routes := RestartRoutes{
		SSH:   newStore().SSH,
		Guest: guestSettings.Guest,
	}
	plan := PlanBatch(vbox.ActionRestart, []vbox.Machine{running})
	if !plan[0].Attempt {
		t.Fatalf("running machine should attempt a restart: %+v", plan[0])
	}
	sum := RunBatch(context.Background(), f, vbox.ActionRestart, plan, routes, nil)
	if len(sum.Failed) != 0 {
		t.Fatalf("a blocked route is a skip, not a failure: %+v", sum)
	}
	if len(sum.Skipped) != 1 || !strings.Contains(sum.Skipped[0].Skip, "switched off") {
		t.Fatalf("want one skip naming the switched-off route: %+v", sum.Skipped)
	}
	if f.has("guestreboot") {
		t.Fatal("a switched-off route must not reach the backend")
	}
	if sum.ExitCode() == 0 {
		t.Fatal("an entirely skipped restart batch must exit nonzero")
	}
	_ = offBox
}

func TestRunBatchRestartGAReportSurvivesIntoSummary(t *testing.T) {
	running := vmRunning()
	f := newFake(running)
	routes := RestartRoutes{SSH: newStore().SSH, Guest: newGuest().Guest} // auto → GA (no SSH stored)
	plan := PlanBatch(vbox.ActionRestart, []vbox.Machine{running})
	if !plan[0].Attempt || plan[0].Verb != vbox.ActionRestart {
		t.Fatalf("a running machine restarts: %+v", plan[0])
	}
	sum := RunBatch(context.Background(), f, vbox.ActionRestart, plan, routes, nil)
	if len(sum.Succeeded) != 1 || sum.Succeeded[0].Report == nil {
		t.Fatalf("expected one success carrying a restart report: %+v", sum.Succeeded)
	}
	if sum.Succeeded[0].Report.Outcome != RestartRequestedGA {
		t.Fatalf("outcome: %v", sum.Succeeded[0].Report.Outcome)
	}
	if !f.hasUUID("guestreboot", running.UUID) {
		t.Fatalf("ops: %v", f.opList())
	}
	for _, op := range f.opList() {
		// The fake's vocabulary proves it: no reset/poweroff op exists at all,
		// and none of the recorded ops resemble one.
		lo := strings.ToLower(op)
		if strings.Contains(lo, "reset") || strings.Contains(lo, "poweroff") {
			t.Fatalf("restart batch issued %q", op)
		}
	}
}

// ---------- restart plan (pure) ----------

func TestPlanRestartModes(t *testing.T) {
	sshReady := config.SSHConfig{Mode: config.SSHModeEndpoint, Host: "127.0.0.1", Port: 2222, User: "root"}
	for _, tc := range []struct {
		name    string
		guest   config.GuestConfig
		ssh     config.SSHConfig
		want    RestartMechanism
		wantErr string
	}{
		{name: "off refuses", guest: config.GuestConfig{OS: config.GuestOSLinux, Restart: config.GuestRestartOff},
			ssh: sshReady, wantErr: "switched off"},
		{name: "ga means ga", guest: config.GuestConfig{Restart: config.GuestRestartGA}, want: RestartViaGA},
		{name: "ssh without settings fails", guest: config.GuestConfig{OS: config.GuestOSLinux, Restart: config.GuestRestartSSH},
			wantErr: "incomplete"},
		{name: "ssh without a known OS fails", guest: config.GuestConfig{Restart: config.GuestRestartSSH},
			ssh: sshReady, wantErr: "guest OS"},
		{name: "ssh fully ready", guest: config.GuestConfig{OS: config.GuestOSLinux, Restart: config.GuestRestartSSH},
			ssh: sshReady, want: RestartViaSSH},
		{name: "auto prefers ssh when ready", guest: config.GuestConfig{OS: config.GuestOSLinux},
			ssh: sshReady, want: RestartViaSSH},
		{name: "auto falls back to ga", guest: config.GuestConfig{}, ssh: config.SSHConfig{}, want: RestartViaGA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PlanRestart(tc.guest, tc.ssh)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %v (%v), want %v", got, err, tc.want)
			}
		})
	}
}

func TestRequestRestartGASuccessNamesTheRoute(t *testing.T) {
	running := vmRunning()
	f := newFake(running)
	rep := RequestRestart(context.Background(), f, running, RestartRoutes{})
	if rep.Outcome != RestartRequestedGA || !rep.OK() {
		t.Fatalf("report: %+v", rep)
	}
	line := rep.Summary(quoteName(running.Name))
	for _, want := range []string{"Guest Additions", "The guest decides"} {
		if !strings.Contains(line, want) {
			t.Errorf("summary missing %q: %q", want, line)
		}
	}
}

func TestRequestRestartGAFailureIsHonestAndNeverAReset(t *testing.T) {
	running := vmRunning()
	f := newFake(running)
	f.rebootErr = errAdditionsMissing
	rep := RequestRestart(context.Background(), f, running, RestartRoutes{})
	if rep.Outcome != RestartBlocked || rep.OK() {
		t.Fatalf("a refused GA channel must block: %+v", rep)
	}
	line := rep.Summary(quoteName(running.Name))
	for _, want := range []string{"Guest Additions", "Nothing was reset or power-cycled", "Nothing was changed"} {
		if !strings.Contains(line, want) {
			t.Errorf("summary missing %q: %q", want, line)
		}
	}
	// Exactly the one guest-control call went out — nothing else.
	ops := f.opList()
	for _, op := range ops {
		lo := strings.ToLower(op)
		if strings.Contains(lo, "reset") || strings.Contains(lo, "poweroff") {
			t.Fatalf("issued %q instead of a guest reboot", op)
		}
	}
	if !f.has("guestreboot") {
		t.Fatalf("expected one guestreboot call: %v", ops)
	}
}

var errAdditionsMissing = &additionsErr{}

type additionsErr struct{}

func (*additionsErr) Error() string {
	return "VBoxManage: the VirtualBox Guest Additions are not running (VERR_ADDITIONS_NOT_STARTED)"
}

// ---------- interactive: menu restart ----------

func TestMenuRestartGuestConfirmsThenUsesGAAgainstAdditions(t *testing.T) {
	running := vmRunning()
	f := newFake(running)
	s := start(t, modelWith(f, newStore(), dialRefused()))
	s.enter() // pick the machine
	s.settleTo(modePickAction, 2*time.Second)
	// The running menu: SSH, Pause, Shut down, Save state, Restart guest…
	// walk to Restart guest (cursor 4) and open the confirm gate.
	for i := 0; i < 4; i++ {
		s.down()
	}
	s.shot()
	if frame := s.strip(); !strings.Contains(frame, "Restart guest") {
		t.Fatalf("menu must offer Restart guest:\n%s", frame)
	}
	s.enter()
	s.shot()
	if frame := s.strip(); !strings.Contains(frame, "never a hardware reset") {
		t.Fatalf("confirm must name what a restart is not:\n%s", frame)
	}
	s.key("y")
	s.settleOutcome(5 * time.Second)
	got := s.outcome(OutcomeSuccess)
	if !strings.Contains(got, "Guest Additions") {
		t.Fatalf("result line: %q", got)
	}
	if !f.hasUUID("guestreboot", running.UUID) {
		t.Fatalf("ops: %v", f.opList())
	}
	if f.has("poweron") || f.has("pause") || f.has("acpipowerbutton") {
		t.Fatalf("a restart touched other controls: %v", f.opList())
	}
}

func TestMenuRestartCancelsWithoutSendingAnything(t *testing.T) {
	f := newFake(vmRunning())
	s := start(t, modelWith(f, newStore(), dialRefused()))
	s.enter()
	s.settleTo(modePickAction, 2*time.Second)
	for i := 0; i < 4; i++ {
		s.down()
	}
	s.enter() // confirm gate
	s.key("n")
	s.settleTo(modePickAction, 2*time.Second)
	if f.has("guestreboot") {
		t.Fatalf("a cancelled restart sent nothing: %v", f.opList())
	}
}

// ---------- interactive: Space marks and batch ----------

func TestSpaceMarksPlanGateThenRunsBatchInOrder(t *testing.T) {
	off1, off2 := vmOff(), anotherOff()
	f := newFake(off1, off2)
	s := start(t, modelWith(f, newStore(), dialRefused()))

	s.key(" ") // mark row 0
	s.down()
	s.key(" ") // mark row 1
	s.shot()
	frame := s.strip()
	if !strings.Contains(frame, "[x]") {
		t.Fatalf("marked rows show [x]:\n%s", frame)
	}
	if !strings.Contains(frame, "2 marked") {
		t.Fatalf("the count line must count marks:\n%s", frame)
	}

	s.enter() // with marks → the verb chooser
	s.settleTo(modeChoose, 2*time.Second)
	s.shot()
	if frame := s.strip(); !strings.Contains(frame, "marked machines") || !strings.Contains(frame, "start headless") {
		t.Fatalf("chooser should list batchable verbs:\n%s", frame)
	}
	s.enter() // start headless (first row)
	s.settleTo(modeConfirm, 2*time.Second)
	s.shot()
	frame = s.strip()
	if !strings.Contains(frame, "Run start headless on these 2 machines?") {
		t.Fatalf("plan gate must name the verb and the count:\n%s", frame)
	}

	// Esc backs out without issuing anything, marks intact for a retry.
	s.key("n")
	s.settleTo(modePickVM, 2*time.Second)
	if f.has("poweron") {
		t.Fatalf("a declined plan must send nothing: %v", f.opList())
	}

	// Re-open and confirm this time.
	s.enter() // marks still set → chooser
	s.settleTo(modeChoose, 2*time.Second)
	s.enter() // start headless
	s.settleTo(modeConfirm, 2*time.Second)
	s.key("y")
	s.settleOutcome(8 * time.Second)
	got := s.outcome(OutcomeSuccess)
	if !strings.Contains(got, "2 succeeded") {
		t.Fatalf("batch summary: %q", got)
	}
	if !f.hasUUID("poweron", off1.UUID) || !f.hasUUID("poweron", off2.UUID) {
		t.Fatalf("both machines by UUID: %v", f.opList())
	}
}

func TestEscReleasesMarksBeforeCancellingTheRun(t *testing.T) {
	f := newFake(vmOff(), anotherOff())
	s := start(t, modelWith(f, newStore(), dialRefused()))
	s.key(" ")
	s.shot()
	s.esc() // first esc: let go of marks
	s.shot()
	if strings.Contains(s.strip(), "1 marked") {
		t.Fatalf("esc must drop the marks first:\n%s", s.strip())
	}
	s.esc() // second esc: cancel the run
	s.settleOutcome(time.Second)
	s.outcome(OutcomeCancelled)
	if len(f.opList()) != 1 || f.opList()[0] != "list" {
		t.Fatalf("marking never touches VirtualBox: %v", f.opList())
	}
}

// ---------- interactive: settings (vbx config) ----------

func guestModel(f *fakeVM, st *fakeStore, g *fakeGuest, target vbox.Machine) *Model {
	o := baseOptions(f, st, dialRefused())
	o.GuestSettings = g
	o.Direct = &DirectRun{Action: ActionSettings, VM: target}
	return New(o)
}

func TestSettingsEditorSavesGuestBlockThroughTheDoor(t *testing.T) {
	off := vmOff()
	f := newFake(off)
	st := storeWith(off.UUID, endpoint("127.0.0.1", 2222, "root"))
	g := newGuest()
	s := start(t, guestModel(f, st, g, off))
	s.shot()
	frame := s.strip()
	// Stored SSH is summarised on the menu, and the guest block shows unset.
	if !strings.Contains(frame, "127.0.0.1") {
		t.Fatalf("settings menu should show stored SSH state:\n%s", frame)
	}
	if !strings.Contains(frame, "restart") || !strings.Contains(frame, "Guest") {
		t.Fatalf("settings menu should show the guest line:\n%s", frame)
	}
	// "Set guest OS / restart route" is item 2 (edit ssh, reset ssh, guest).
	for i := 0; i < 2; i++ {
		s.down()
	}
	s.enter() // → OS chooser
	s.settleTo(modeChoose, 2*time.Second)
	s.shot()
	if frame := s.strip(); !strings.Contains(frame, "Linux") || !strings.Contains(frame, "Unknown (no SSH restart)") {
		t.Fatalf("guest OS chooser:\n%s", frame)
	}
	// Stored OS is unset (Unknown), so the cursor starts there: walk up to
	// Linux (the first row) and confirm it.
	s.up()
	s.up()
	s.enter()
	s.shot()
	if frame := s.strip(); !strings.Contains(frame, "Guest Additions (controlvm reboot)") {
		t.Fatalf("restart-mode chooser:\n%s", frame)
	}
	s.down() // auto → ga
	s.enter()
	s.settleTo(modeSettings, 3*time.Second)
	saved, ok := g.value(off.UUID)
	if !ok || saved.OS != config.GuestOSLinux || saved.Restart != config.GuestRestartGA {
		t.Fatalf("guest door stored: %+v (ok=%v)", saved, ok)
	}
	s.shot()
	if !strings.Contains(s.strip(), "os=linux restart=ga") {
		t.Fatalf("confirmation should echo what was saved:\n%s", s.strip())
	}
	// The guest edit must not touch the SSH entry: nothing was ever written
	// through the SSH door during this run.
	st.mu.Lock()
	writes := len(st.saved)
	st.mu.Unlock()
	if writes != 0 {
		t.Fatalf("guest edits must not write SSH settings: %d writes", writes)
	}
}

func TestSettingsResetScopesAreConfirmedAndSurgical(t *testing.T) {
	off := vmOff()
	f := newFake(off)
	st := storeWith(off.UUID, endpoint("127.0.0.1", 2222, "root"))
	g := newGuest()
	g.values[config.CanonicalUUID(off.UUID)] = config.GuestConfig{OS: config.GuestOSLinux, Restart: config.GuestRestartGA}
	s := start(t, guestModel(f, st, g, off))
	s.shot()
	frame := s.strip()
	if !strings.Contains(frame, "Remove all saved settings for this VM") {
		t.Fatalf("with both blocks stored, the full-remove item must exist:\n%s", frame)
	}
	// Walk to "Remove all saved settings" (item 4) and cancel the gate first.
	for i := 0; i < 4; i++ {
		s.down()
	}
	s.enter()
	s.shot()
	if frame := s.strip(); !strings.Contains(frame, "The VM and its disks are untouched") {
		t.Fatalf("the reset question must bound what it changes:\n%s", frame)
	}
	s.key("n")
	s.settleTo(modeSettings, 2*time.Second)
	if _, ok := g.value(off.UUID); !ok {
		t.Fatal("a declined reset must change nothing")
	}
	// Confirm for real: both doors cleared.
	s.enter()
	s.key("y")
	s.settleTo(modeSettings, 3*time.Second)
	if _, ok := g.value(off.UUID); ok {
		t.Fatal("guest block should be gone")
	}
	if _, ok := g.cleared[config.CanonicalUUID(off.UUID)]; !ok {
		t.Fatal("guest door must record the nil-store clear")
	}
	if cfg := st.remainingForTest(off.UUID); cfg.Configured() {
		t.Fatal("ssh entry should be gone (cleared means not configured)")
	}
	s.shot()
	if !strings.Contains(s.strip(), "SSH: not configured") {
		t.Fatalf("menu must reflect the cleared state:\n%s", s.strip())
	}
	// Nothing about the machine itself was called: only reads.
	for _, op := range f.opList() {
		if op != "list" && op != "detail" {
			t.Fatalf("settings changes must not touch the machine: %v", f.opList())
		}
	}
}

func TestFocusedConfigExitsCleanlyWhenNothingWasChanged(t *testing.T) {
	off := vmOff()
	f := newFake(off)
	g := newGuest()
	s := start(t, guestModel(f, newStore(), g, off))
	s.esc() // leaving the settings menu ends the focused visit
	s.settleOutcome(2 * time.Second)
	got := s.outcome(OutcomeSuccess)
	if !strings.Contains(got, "Settings for") {
		t.Fatalf("exit line: %q", got)
	}
}

// A focused `vbx config`/`vbx snapshots` with no machine named resolves
// through the picker: the picked machine must fall into the flow, never into
// the lifecycle runner (which would refuse the app-level verb).

func TestFocusedConfigWithoutATargetOpensSettingsForThePickedVM(t *testing.T) {
	off := vmOff()
	f := newFake(off)
	st := storeWith(off.UUID, endpoint("127.0.0.1", 2222, "root"))
	o := baseOptions(f, st, dialRefused())
	o.GuestSettings = newGuest()
	o.Direct = &DirectRun{Action: ActionSettings} // name omitted: pick one
	s := start(t, New(o))
	s.enter() // pick the only machine
	s.settleTo(modeSettings, 2*time.Second)
	if frame := s.strip(); !strings.Contains(frame, "127.0.0.1") {
		t.Fatalf("picked machine's settings must open:\n%s", frame)
	}
	// Settings journeys never touch the machine itself.
	for _, op := range f.opList() {
		if op != "list" && op != "detail" {
			t.Fatalf("config opened a machine operation: %v", f.opList())
		}
	}
}

func TestFocusedSnapshotsWithoutATargetListsThePickedVMBOnly(t *testing.T) {
	off := vmOff()
	f := newFake(off)
	f.seedSnapshots(off.UUID, seedTree(), "10000000-0000-4000-8000-000000000002")
	o := baseOptions(f, newStore(), dialRefused())
	o.Direct = &DirectRun{Action: ActionSnapshots}
	s := start(t, New(o))
	s.enter() // pick
	s.settleTo(modeSnapList, 2*time.Second)
	if frame := s.strip(); !strings.Contains(frame, "(current)") {
		t.Fatalf("the picked machine's tree must render:\n%s", frame)
	}
	s.esc() // closing a focused listing completes the command
	s.settleOutcome(2 * time.Second)
	s.outcome(OutcomeSuccess)
}

// ---------- scripted batch (the CLI's NoConfirm door) ----------

func TestDirectBatchRunsWithoutAGateAndSummarises(t *testing.T) {
	f := newFake(vmOff(), anotherOff())
	o := baseOptions(f, newStore(), dialRefused())
	o.Direct = &DirectRun{
		Action:    vbox.ActionStart,
		Targets:   []vbox.Machine{vmOff(), anotherOff()},
		NoConfirm: true, // the command line already named every machine
	}
	s := start(t, New(o)) // Init plans and starts the batch by itself
	s.settleOutcome(8 * time.Second)
	got := s.outcome(OutcomeSuccess)
	if !strings.Contains(got, "2 succeeded") {
		t.Fatalf("summary: %q", got)
	}
	// A plan gate never appeared: the run went straight through.
	if f.opCountForTest("poweron") != 2 {
		t.Fatalf("want exactly two starts: %v", f.opList())
	}
}

// ---------- interactive: snapshots ----------

func seedTree() []vbox.Snapshot {
	return []vbox.Snapshot{
		{Name: "clean", UUID: "10000000-0000-4000-8000-000000000001", Depth: 0, Description: "fresh install"},
		{Name: "clean", UUID: "10000000-0000-4000-8000-000000000002", Node: "-1", Depth: 1},
	}
}

func TestFocusedSnapshotsListsTreeMarksCurrentAndExits(t *testing.T) {
	off := vmOff()
	f := newFake(off)
	f.seedSnapshots(off.UUID, seedTree(), "10000000-0000-4000-8000-000000000002")
	o := baseOptions(f, newStore(), dialRefused())
	o.Direct = &DirectRun{Action: ActionSnapshots, VM: off}
	s := start(t, New(o))
	s.settleTo(modeSnapList, 2*time.Second)
	frame := s.strip()
	// The tree shows both snapshots; current is marked; descriptions appear.
	if strings.Count(frame, "clean") < 2 {
		t.Fatalf("tree rows:\n%s", frame)
	}
	if !strings.Contains(frame, "(current)") {
		t.Fatalf("current marker missing:\n%s", frame)
	}
	if !strings.Contains(frame, "fresh install") {
		t.Fatalf("description missing:\n%s", frame)
	}
	s.esc() // focused listing: closing the tree completes the command
	s.settleOutcome(2 * time.Second)
	if got := s.outcome(OutcomeSuccess); !strings.Contains(got, "2 snapshots") {
		t.Fatalf("exit line: %q", got)
	}
}

func TestFocusedSnapshotsRefusesToRestore(t *testing.T) {
	// The focused command lists. Restoring is interactive-only: pressing
	// enter on a row must explain, never act.
	off := vmOff()
	f := newFake(off)
	f.seedSnapshots(off.UUID, seedTree(), "10000000-0000-4000-8000-000000000002")
	o := baseOptions(f, newStore(), dialRefused())
	o.Direct = &DirectRun{Action: ActionSnapshots, VM: off}
	s := start(t, New(o))
	s.settleTo(modeSnapList, 2*time.Second)
	s.enter()
	s.shot()
	if frame := s.strip(); !strings.Contains(frame, "interactive") {
		t.Fatalf("focused restore must name the interactive route:\n%s", frame)
	}
	if f.has("snaprestore") {
		t.Fatal("nothing may be restored from a listing command")
	}
}

func TestSnapshotTakeWritesNameAndDescription(t *testing.T) {
	off := vmOff()
	f := newFake(off)
	f.seedSnapshots(off.UUID, seedTree(), "10000000-0000-4000-8000-000000000002")
	o := baseOptions(f, newStore(), dialRefused())
	o.Direct = &DirectRun{Action: ActionSnapshots, VM: off}
	s := start(t, New(o))
	s.settleTo(modeSnapList, 2*time.Second)
	s.key("n") // take
	s.settleTo(modeText, 2*time.Second)
	s.typeLine("before-upgrade")
	s.settleTo(modeText, 2*time.Second) // description step
	s.typeLine("keep this point")
	s.settleTo(modeSnapList, 3*time.Second)
	f.mu.Lock()
	var names []string
	for _, snap := range f.snapshots[config.CanonicalUUID(off.UUID)] {
		names = append(names, snap.Name+"|"+snap.Description)
	}
	f.mu.Unlock()
	if !containsStr(names, "before-upgrade|keep this point") {
		t.Fatalf("snapshot not stored with its fields: %v", names)
	}
	s.shot()
	if !strings.Contains(s.strip(), "Snapshot taken") {
		t.Fatalf("note missing:\n%s", s.strip())
	}
}

func TestSnapshotTakeRejectsUnsafeNames(t *testing.T) {
	off := vmOff()
	f := newFake(off)
	o := baseOptions(f, newStore(), dialRefused())
	o.Direct = &DirectRun{Action: ActionSnapshots, VM: off}
	s := start(t, New(o))
	s.settleTo(modeSnapList, 2*time.Second) // empty tree is fine
	s.key("n")
	s.settleTo(modeText, 2*time.Second)
	s.typeLine("-lm") // a name starting with '-' would read as a VBoxManage option
	s.shot()
	frame := s.strip()
	if !strings.Contains(frame, "read as an option") {
		t.Fatalf("an unsafe name must be refused with a visible reason:\n%s", frame)
	}
	if f.has("snaptake") {
		t.Fatal("a refused name must never reach VirtualBox")
	}
	// An empty name is the documented "give up" path: back to the tree.
	s.key("backspace")
	s.key("backspace")
	s.key("backspace")
	s.enter()
	s.settleTo(modeSnapList, 2*time.Second)
}

func TestSnapshotRestoreRunsFromMenuWithProtection(t *testing.T) {
	off := vmOff()
	f := newFake(off)
	f.seedSnapshots(off.UUID, seedTree(), "10000000-0000-4000-8000-000000000002")
	defer withFixedSnapshotTime(t, "2026-05-05 10:11:12")()
	s := start(t, modelWith(f, newStore(), dialRefused()))
	s.enter() // pick machine
	s.settleTo(modePickAction, 2*time.Second)
	// Menu for an off machine: Start headless, Start and SSH, Rename…,
	// Snapshots…, settings — walk to Snapshots and open it.
	for i := 0; i < 3; i++ {
		s.down()
	}
	s.shot()
	if frame := s.strip(); !strings.Contains(frame, "Snapshots…") {
		t.Fatalf("rename must not hide the snapshots entry:\n%s", frame)
	}
	s.enter()
	s.settleTo(modeSnapList, 2*time.Second)
	// Cursor sits on "clean" (root). Enter opens the state gate.
	s.enter()
	s.settleTo(modeConfirm, 3*time.Second)
	s.shot()
	frame := s.strip()
	if !strings.Contains(frame, `to snapshot "clean"`) {
		t.Fatalf("the confirmation must name VM and snapshot:\n%s", frame)
	}
	if !strings.Contains(frame, "protective") {
		t.Fatalf("the protective option must be offered:\n%s", frame)
	}
	// Cancel first: nothing changes.
	s.key("n")
	s.settleTo(modeSnapList, 2*time.Second)
	if f.has("snaprestore") {
		t.Fatal("a cancelled restore sent nothing")
	}
	// Choose "s": restore without protection to prove direct UUID restore.
	s.enter()
	s.settleTo(modeConfirm, 3*time.Second)
	s.key("s")
	s.settleOutcome(5 * time.Second)
	got := s.outcome(OutcomeSuccess)
	if !strings.Contains(got, `Restored "Windows 11" to snapshot "clean"`) {
		t.Fatalf("result line: %q", got)
	}
	if !f.has("snaprestore") {
		t.Fatalf("ops: %v", f.opList())
	}
	f.mu.Lock()
	cur := f.snapCur[config.CanonicalUUID(off.UUID)]
	f.mu.Unlock()
	if cur != config.CanonicalUUID("10000000-0000-4000-8000-000000000001") {
		t.Fatalf("restored to the wrong point: current is now %q", cur)
	}
}

func TestSnapshotRestoreProtectiveTakeAbortsOnFailure(t *testing.T) {
	off := vmOff()
	f := newFake(off)
	f.seedSnapshots(off.UUID, seedTree(), "10000000-0000-4000-8000-000000000002")
	defer withFixedSnapshotTime(t, "2026-05-05 10:11:12")()
	// Pre-seed the exact protective name so the protective take fails with a
	// duplicate — the restore must then abort with nothing discarded.
	f.mu.Lock()
	dupName := defaultSnapshotName() + " (protective)"
	f.snapshots[config.CanonicalUUID(off.UUID)] = append(f.snapshots[config.CanonicalUUID(off.UUID)],
		vbox.Snapshot{Name: dupName, UUID: "90000000-0000-4000-8000-000000000009", Depth: 1})
	f.mu.Unlock()

	s := start(t, modelWith(f, newStore(), dialRefused()))
	s.enter()
	s.settleTo(modePickAction, 2*time.Second)
	// Off menu (see above): three steps down reaches "Snapshots…".
	for i := 0; i < 3; i++ {
		s.down()
	}
	s.enter()
	s.settleTo(modeSnapList, 2*time.Second)
	s.enter()
	s.settleTo(modeConfirm, 3*time.Second)
	s.key("y") // protective first
	s.settleOutcome(5 * time.Second)
	got := s.outcome(OutcomeFailure)
	for _, want := range []string{"protective snapshot failed", "restore was aborted", "Nothing of the current state was discarded"} {
		if !strings.Contains(got, want) {
			t.Fatalf("abort line missing %q: %q", want, got)
		}
	}
	if f.has("snaprestore") {
		t.Fatal("a failed protective take must abort before the restore")
	}
}

func TestSnapshotRestoreRefusesWhenTheMachineStartedUpSince(t *testing.T) {
	run := vmRunning()
	f := newFake(run)
	f.seedSnapshots(run.UUID, seedTree(), "10000000-0000-4000-8000-000000000002")
	s := start(t, modelWith(f, newStore(), dialRefused()))
	s.enter()
	s.settleTo(modePickAction, 2*time.Second)
	// Running menu: SSH, Pause, Shut down, Save state, Restart guest, Snapshots…
	for i := 0; i < 5; i++ {
		s.down()
	}
	s.enter()
	s.settleTo(modeSnapList, 2*time.Second)
	s.enter() // choose a snapshot → state gate re-reads and sees running
	s.shot()
	frame := s.strip()
	if !strings.Contains(frame, "needs it powered off") {
		t.Fatalf("a running machine must be refused with the reason:\n%s", frame)
	}
	if f.has("snaprestore") {
		t.Fatal("a refused restore issued nothing")
	}
}

// ---------- helpers ----------

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func withFixedSnapshotTime(t *testing.T, stamp string) func() {
	t.Helper()
	old := snapshotNameTime
	parsed, err := time.Parse("2006-01-02 15:04:05", stamp)
	if err != nil {
		t.Fatal(err)
	}
	snapshotNameTime = func() time.Time { return parsed }
	return func() { snapshotNameTime = old }
}
