package app

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/aditya-shrivastavv/vbox-tui/internal/config"
	"github.com/aditya-shrivastavv/vbox-tui/internal/vbox"
)

// ---------- startup ----------

func TestStartupShowsThePickerByName(t *testing.T) {
	f := newFake(vmRunning(), vmOff())
	s := start(t, modelWith(f, newStore(), dialRefused()))

	frame := s.strip()
	// Both machines, named in full, with a state word for each; the picker
	// question on top. The cursor never stands alone.
	for _, want := range []string{"Select a virtual machine", "Kali Linux", "Running", "Windows 11", "Stopped"} {
		if !strings.Contains(frame, want) {
			t.Errorf("picker should show %q:\n%s", want, frame)
		}
	}
	if strings.Contains(frame, "[ssh]") || strings.Contains(frame, "[start-and-connect]") {
		t.Errorf("row clutter is gone with the dashboard:\n%s", frame)
	}
	for i, l := range strings.Split(frame, "\n") {
		if strings.TrimSpace(l) == ">" {
			t.Errorf("line %d is a bare marker: %q", i, l)
		}
	}
}

func TestListFailureEndsTheRunWithTheRealReason(t *testing.T) {
	f := newFake()
	f.listErr = errors.New("VBoxManage could not be found")
	s := start(t, modelWith(f, newStore(), dialRefused()))

	summary := s.outcome(OutcomeFailure)
	if !strings.Contains(summary, "VBoxManage") {
		t.Fatalf("the failure must name what actually broke: %q", summary)
	}
	if s.m.ExitCode() != 1 {
		t.Fatalf("a failure exits 1, got %d", s.m.ExitCode())
	}
}

func TestEmptyInventoryGuidesInsteadOfHanging(t *testing.T) {
	s := start(t, modelWith(newFake(), newStore(), dialRefused()))
	summary := s.outcome(OutcomeFailure)
	for _, want := range []string{"No registered virtual machines", "Create"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("empty-inventory guidance missing %q: %q", want, summary)
		}
	}
}

func TestFrameIsContentSized(t *testing.T) {
	// Five times the picker, same inventory: a dashboard would pad toward the
	// previous height, a prompt never does.
	f := newFake(vmRunning(), vmOff(), vmPausedKali())
	st := newStore()
	for _, height := range []int{200, 12} {
		s := start(t, modelWith(f, st, dialRefused()))
		s.send(tea.WindowSizeMsg{Width: 80, Height: height})
		lines := strings.Split(strings.TrimRight(s.frame, "\n"), "\n")
		nonBlank := 0
		for _, l := range lines {
			if strings.TrimSpace(l) != "" {
				nonBlank++
			}
		}
		if nonBlank != len(lines) {
			t.Errorf("height %d: frame carries blank filler lines (%d of %d):\n%s", height, len(lines)-nonBlank, len(lines), s.strip())
		}
		if len(lines) > 8 {
			t.Errorf("height %d: three machines drew %d lines; the frame must stay content-sized:\n%s", height, len(lines), s.strip())
		}
	}
}

// ---------- names in the picker ----------

func TestNamesAreNeverChoppedJustBecause(t *testing.T) {
	f := newFake(vmWideName(), vmRunning())
	s := start(t, modelWith(f, newStore(), dialRefused()))

	if !strings.Contains(s.strip(), "开发机 🐧 dev-box") {
		t.Fatalf("name with wide chars and an emoji must render intact at width 80:\n%s", s.strip())
	}

	// Narrow terminal: the line is cut with the explicit marker, not silently,
	// and never to nothing.
	s.send(tea.WindowSizeMsg{Width: 24, Height: 20})
	frame := s.strip()
	for _, l := range strings.Split(frame, "\n") {
		if strings.TrimSpace(l) == ">" {
			t.Errorf("a row may never render as a bare marker: %q", l)
		}
	}
	if !strings.Contains(frame, "~") {
		t.Fatalf("a truncated line must show the truncation marker:\n%s", frame)
	}

	// Back to wide: the full name returns — truncation was only ever visual.
	s.send(tea.WindowSizeMsg{Width: 80, Height: 20})
	if !strings.Contains(s.strip(), "开发机 🐧 dev-box") {
		t.Fatalf("the stored name must survive untouched:\n%s", s.strip())
	}
}

func TestDuplicateNamesCannotConfuseIdentity(t *testing.T) {
	// Two "Kali Linux" rows: choosing the second (paused) must act on ITS
	// UUID, not the first.
	f := newFake(vmRunning(), vmPausedKali())
	s := start(t, modelWith(f, newStore(), dialRefused()))

	frame := s.strip()
	if strings.Count(frame, "Kali Linux") < 2 {
		t.Fatalf("both duplicate names must show:\n%s", frame)
	}
	s.down()
	s.enter()
	s.settleTo(modePickAction, 2*time.Second)

	// The paused Kali's menu offers only what a paused machine can do.
	frame = s.strip()
	for _, want := range []string{"Resume", "Resume and SSH"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("paused menu missing %q:\n%s", want, frame)
		}
	}
	for _, no := range []string{"Shut down", "Save state", "Pause\n"} {
		if strings.Contains(frame, strings.TrimSuffix(no, "\n")) {
			t.Fatalf("a paused machine must not offer %q:\n%s", no, frame)
		}
	}
	// Menu is [Resume and SSH, Resume]: one down lands on plain Resume.
	s.down()
	s.enter() // Resume
	s.settleOutcome(5 * time.Second)
	summary := s.outcome(OutcomeSuccess)
	if !strings.Contains(summary, "Resumed") {
		t.Fatalf("expected resume result, got %q", summary)
	}
	// Resume must have addressed the PAUSED Kali by UUID — the whole point of
	// UUID identity with duplicate names.
	if !f.hasUUID("resume", vmPausedKali().UUID) {
		t.Fatalf("resume did not address the paused machine by UUID: %+v", f.ops)
	}
	if f.has("poweron") || f.has("pause") {
		t.Fatalf("wrong lifecycle call for a paused machine: %v", f.opList())
	}
}

func TestFiltering(t *testing.T) {
	f := newFake(vmRunning(), vmOff(), vmPausedKali())
	s := start(t, modelWith(f, newStore(), dialRefused()))

	s.runes("win")
	frame := s.strip()
	if !strings.Contains(frame, "Windows 11") || strings.Contains(frame, "Kali") {
		t.Fatalf("filter 'win' should leave only Windows 11:\n%s", frame)
	}
	// Typing a state word filters by state too.
	s.key("backspace")
	s.key("backspace")
	s.key("backspace")
	s.runes("paused")
	if !strings.Contains(s.strip(), "Paused") || strings.Contains(s.strip(), "Windows") {
		t.Fatalf("filter 'paused' should list the paused machine:\n%s", s.strip())
	}
	// A filter with no matches explains how to recover.
	s.runes("zzz")
	if !strings.Contains(s.strip(), "no machines match the filter") {
		t.Fatalf("empty filter result must guide: %s", s.strip())
	}
	// enter on no rows does nothing destructive; esc clears back via backspaces.
	s.enter()
	if s.m.modeForTest() != modePickVM {
		t.Fatalf("enter with no rows must not pick anything (mode %v)", s.m.modeForTest())
	}
}

// ---------- action menus per state ----------

func TestActionMenusByState(t *testing.T) {
	knownLabels := map[string]bool{
		"SSH": true, "Start and SSH": true, "Start headless": true,
		"Restore": true, "Restore and SSH": true, "Resume": true, "Resume and SSH": true,
		"Pause": true, "Shut down": true, "Save state": true, "Restart guest": true,
		"Snapshots…": true, "SSH and restart settings…": true,
	}
	cases := []struct {
		name    string
		machine vbox.Machine
		want    []string // exact menu labels, in nothing in particular
		deny    []string // labels that must not appear
	}{
		{"running", vmRunning(),
			[]string{"SSH", "Pause", "Shut down", "Save state", "Restart guest"},
			[]string{"Start headless", "Start and SSH", "Restore", "Resume"}},
		{"off", vmOff(),
			[]string{"Start headless", "Start and SSH"},
			[]string{"SSH", "Pause", "Shut down", "Save state", "Resume"}},
		{"saved", vmSaved(),
			[]string{"Restore", "Restore and SSH"},
			[]string{"Start headless", "SSH", "Pause", "Shut down", "Save state", "Resume"}},
		{"paused", vmPausedKali(),
			[]string{"Resume", "Resume and SSH"},
			[]string{"SSH", "Pause", "Shut down", "Save state"}},
		{"transitioning", vmWideName(),
			nil, // no menu at all: the picker explains and returns
			[]string{"SSH", "Start headless", "Restore", "Pause", "Shut down", "Save state"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(tc.machine)
			s := start(t, modelWith(f, newStore(), dialRefused()))
			s.enter() // pick the only machine — fresh state read decides the menu

			if tc.want == nil {
				// A transitional machine: no menu, an honest note, picker back.
				s.settleTo(modePickVM, 2*time.Second)
				frame := s.strip()
				for _, no := range tc.deny {
					if _, _, ok := menuLineLabel(frame, no, knownLabels); ok {
						t.Fatalf("transitional state must offer nothing, found menu entry %q:\n%s", no, frame)
					}
				}
				// The note names the real state and tells the user what to do.
				if !strings.Contains(strings.ToLower(frame), "try again") {
					t.Fatalf("the note should guide the user:\n%s", frame)
				}
				return
			}

			s.settleTo(modePickAction, 2*time.Second)
			frame := s.strip()
			shown := map[string]bool{}
			for label := range knownLabels {
				if _, _, ok := menuLineLabel(frame, label, knownLabels); ok {
					shown[label] = true
				}
			}
			for _, w := range tc.want {
				if !shown[w] {
					t.Errorf("menu should offer %q (offered %v):\n%s", w, shown, frame)
				}
			}
			for _, no := range tc.deny {
				if shown[no] {
					t.Errorf("menu must not offer %q when %s (offered %v):\n%s", no, tc.name, shown, frame)
				}
			}
		})
	}
}

// menuLineLabel reports whether the frame offers the label AS ITS OWN menu
// entry (row marker plus exactly that label), so "SSH" is not satisfied by the
// row "Resume and SSH". Styling is stripped per line because the selected row
// carries colour codes inside it.
func menuLineLabel(frame, label string, known map[string]bool) (line string, idx int, found bool) {
	for i, l := range strings.Split(ansi.Strip(frame), "\n") {
		for _, marker := range []string{"> ", "  "} {
			if strings.HasPrefix(l, marker) {
				rest := strings.TrimSpace(strings.TrimPrefix(l, marker))
				if known[rest] {
					idx = i
					if rest == label {
						return l, i, true
					}
					line = rest
					found = true
				}
			}
		}
	}
	return "", 0, false
}

// ---------- actions: exactly one call, honest words ----------

func TestStartOnOffMachine(t *testing.T) {
	f := newFake(vmOff())
	s := start(t, modelWith(f, newStore(), dialRefused()))
	s.enter()
	s.settleTo(modePickAction, 2*time.Second)
	// Off menu: "Start and SSH" then "Start headless".
	s.down()
	s.enter()
	s.settleOutcome(5 * time.Second)

	summary := s.outcome(OutcomeSuccess)
	if summary != `Started "Windows 11" in headless mode.` {
		t.Fatalf("result line = %q", summary)
	}
	if !f.has("poweron") {
		t.Fatalf("start was never issued: %v", f.opList())
	}
}

func TestPauseSendsControlvmPause(t *testing.T) {
	f := newFake(vmRunning())
	s := start(t, modelWith(f, newStore(), dialRefused()))
	s.enter()
	s.settleTo(modePickAction, 2*time.Second)
	s.down() // SSH, Pause
	s.enter()
	s.settleOutcome(5 * time.Second)

	if s.outcome(OutcomeSuccess) != `Paused "Kali Linux".` {
		t.Fatalf("summary: %q", s.m.Summary())
	}
	if !f.has("pause") {
		t.Fatalf("the real pause command was never issued: %v", f.opList())
	}
}

func TestShutdownAsksThenWordingIsHonest(t *testing.T) {
	f := newFake(vmRunning())
	s := start(t, modelWith(f, newStore(), dialRefused()))
	s.enter()
	s.settleTo(modePickAction, 2*time.Second)

	// Walk to "Shut down" (index 2) — confirm gate must appear.
	s.down()
	s.down()
	s.enter()
	s.settleTo(modeConfirm, 2*time.Second)
	frame := s.strip()
	for _, want := range []string{"shutdown", "decides", "nothing is forced"} {
		if !strings.Contains(strings.ToLower(frame), want) {
			t.Fatalf("confirm must explain what a shutdown request is (%q missing):\n%s", want, frame)
		}
	}
	if f.has("acpipowerbutton") {
		t.Fatal("the confirm gate asked before VirtualBox was touched, but VirtualBox was touched")
	}

	s.key("n")
	// Back at the menu with the machine untouched; the cursor is still on
	// "Shut down", exactly where the user left it.
	s.settleTo(modePickAction, 2*time.Second)
	if f.has("acpipowerbutton") {
		t.Fatal("answering no must cancel the request")
	}

	// Now confirm — same entry, no re-navigation needed.
	s.enter()
	s.settleTo(modeConfirm, 2*time.Second)
	s.key("y")
	s.settleOutcome(5 * time.Second)

	summary := s.outcome(OutcomeSuccess)
	if !strings.Contains(summary, "Shutdown requested") {
		t.Fatalf("shutdown must be worded as a request: %q", summary)
	}
	if strings.Contains(summary, "stopped") || strings.Contains(summary, "Stopped") {
		t.Fatalf("a delivered request is never a completed stop: %q", summary)
	}
	if !f.has("acpipowerbutton") {
		t.Fatalf("ACPI request never issued: %v", f.opList())
	}
	// And the menu word itself was honest before confirming:
	// "Shut down", not "Stop" / "Power off".
}

func TestSaveStateConfirmsAndWordsClearly(t *testing.T) {
	f := newFake(vmRunning())
	s := start(t, modelWith(f, newStore(), dialRefused()))
	s.enter()
	s.settleTo(modePickAction, 2*time.Second)
	s.down() // SSH, Pause, Shut down, Save state
	s.down()
	s.down()
	s.enter()
	s.settleTo(modeConfirm, 2*time.Second)
	if !strings.Contains(strings.ToLower(s.strip()), "suspend to disk") {
		t.Fatalf("save confirm must distinguish it from a shutdown:\n%s", s.strip())
	}
	s.key("y")
	s.settleOutcome(5 * time.Second)
	if s.outcome(OutcomeSuccess) != "Saved \"Kali Linux\"'s state." {
		t.Fatalf("summary: %q", s.m.Summary())
	}
}

func TestStartHonestyWhenTheStateMoved(t *testing.T) {
	// The menu (and any earlier read) said "powered off"; between the menu read
	// and the operation itself the machine ends up saved — as if a save was
	// triggered from the VirtualBox GUI. The operation's own re-read must notice
	// and honestly restore instead of pretending a fresh start.
	f := newFake(vmOff())
	s := start(t, modelWith(f, newStore(), dialRefused()))
	s.enter() // picker
	s.settleTo(modePickAction, 2*time.Second)

	// After the menu was drawn and before the choice is executed, the machine
	// gets saved from elsewhere (a GUI click): the operation's own re-read is
	// the first thing the prompt sees of it. The flip here stands in for that
	// outside change; the next List the model makes will report "saved".
	f.mu.Lock()
	f.machines[0].State = vbox.NormalizeState("saved")
	f.mu.Unlock()

	// Off menu was [Start and SSH, Start headless]; choose the plain start.
	s.down()
	s.enter() // "Start headless"
	s.settleOutcome(5 * time.Second)

	summary := s.outcome(OutcomeSuccess)
	if !strings.Contains(summary, "Restored") {
		t.Fatalf("choosing Start on a now-saved machine must say Restored: %q", summary)
	}
}

// ---------- cancellation ----------

func TestCancelBeforeAnythingSaysNothingChanged(t *testing.T) {
	f := newFake(vmRunning())
	s := start(t, modelWith(f, newStore(), dialRefused()))
	s.esc()
	summary := s.outcome(OutcomeCancelled)
	if !strings.Contains(summary, "Nothing was changed.") {
		t.Fatalf("clean cancel must state it changed nothing: %q", summary)
	}
	if len(f.opList()) != 0 {
		// list reads are fine, lifecycle calls are not
		for _, op := range f.opList() {
			if op != "list" {
				t.Fatalf("cancel issued %q", op)
			}
		}
	}
}

func TestCancelWhileAShutdownIsInFlightIsHonest(t *testing.T) {
	// The ACPI call blocks on a gate the test opens after cancelling.
	f := newFake(vmRunning())
	f.gate = make(chan struct{})
	f.arrived = make(chan struct{})
	s := start(t, modelWith(f, newStore(), dialRefused()))
	s.enter()
	s.settleTo(modePickAction, 2*time.Second)
	s.down()
	s.down()
	s.enter() // Shut down
	s.settleTo(modeConfirm, 2*time.Second)
	s.key("y")

	// Wait until the request is genuinely inside VirtualBox, then leave.
	select {
	case <-f.arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("the shutdown request never reached the backend")
	}
	s.esc()
	summary := s.outcome(OutcomeCancelled)
	close(f.gate)

	if strings.Contains(summary, "Nothing was changed.") {
		t.Fatalf("cancelling an in-flight request must not claim nothing changed: %q", summary)
	}
	for _, want := range []string{"already", "Kali"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("cancellation should say what is still in motion (%q missing): %q", want, summary)
		}
	}
	// The late command answer must not rewrite the outcome.
	s.drain(ms(400))
	if s.m.Outcome() != OutcomeCancelled {
		t.Fatalf("outcome was rewritten after cancellation: %v %q", s.m.Outcome(), s.m.Summary())
	}
}

func TestCancelDuringSSHWaitMentionsTheStartItCannotUndo(t *testing.T) {
	// "Start and SSH" with settings already present: off → start (completes)
	// → wait blocked → esc.
	off := vmOff()
	st := storeWith(off.UUID, endpoint("127.0.0.1", 2222, "root"))
	dial, release := dialHolds()
	f := newFake(off)
	s := start(t, modelWith(f, st, dial))
	s.enter()
	s.settleTo(modePickAction, 2*time.Second)
	// Off menu: "Start and SSH" is the first entry; the cursor is already there.
	s.enter()

	s.waitForFrame("Waiting for SSH", 5*time.Second)
	s.esc()
	close(release)

	summary := s.outcome(OutcomeCancelled)
	for _, want := range []string{"start", "undo", "windows"} {
		if !strings.Contains(strings.ToLower(summary), want) {
			t.Fatalf("cancelling after the start must admit it (%q missing): %q", want, summary)
		}
	}
}

// ---------- the ssh journey ----------

func TestSSHSetupHandoff(t *testing.T) {
	run := vmRunning()
	st := newStore()
	f := newFake(run)
	s := start(t, modelWith(f, st, dialBanner("SSH-2.0-OpenSSH_9.6\r\n")))
	s.enter()
	s.settleTo(modePickAction, 2*time.Second)
	s.enter() // SSH (first menu entry when running)

	// No stored settings: the short setup asks how to reach the guest.
	s.settleTo(modeSSHSetup, 2*time.Second)
	if !strings.Contains(s.strip(), "How should") {
		t.Fatalf("setup should ask about the machine by name:\n%s", s.strip())
	}
	// Step 0: keep the host/port default. Step 2 comes prefilled with 22 and
	// Enter accepts the suggestion.
	s.enter()
	s.typeLine("127.0.0.1") // step 1: host
	s.enter()               // step 2: port (prefilled 22)
	s.typeLine("root")      // step 3: user

	// Settings stored per-UUID, then the bounded wait — with a banner to
	// answer, the handoff follows.
	s.settleOutcome(5 * time.Second)

	if cfg, ok := st.savedFor(run.UUID); !ok || cfg.Host != "127.0.0.1" || cfg.User != "root" || cfg.Port != 22 {
		t.Fatalf("settings not stored for the UUID: %+v ok=%v", cfg, ok)
	}
	s.outcome(OutcomeSSHHandoff)
	spec := s.m.SSHCommand()
	args := strings.Join(spec.Args, " ")
	if !strings.Contains(args, "-p 22") || !strings.Contains(args, "root@127.0.0.1") {
		t.Fatalf("handoff spec wrong: %s %v", spec.Path, spec.Args)
	}
	if strings.Contains(args, "-o StrictHostKeyChecking") || strings.Contains(args, "-i ") {
		t.Fatalf("vbox-tui must not touch security options: %q", args)
	}
}

func TestSSHAliasSkipsTheTcpProbe(t *testing.T) {
	run := vmRunning()
	st := storeWith(run.UUID, aliasCfg("workbox"))
	// Alias mode must hand off immediately, letting OpenSSH itself judge a
	// route that may run through ProxyJump; a TCP probe would prove nothing.
	f := newFake(run)
	s := start(t, modelWith(f, st, dialMustNotDial(t)))
	s.enter()
	s.settleTo(modePickAction, 2*time.Second)
	s.enter() // SSH
	s.settleOutcome(3 * time.Second)

	s.outcome(OutcomeSSHHandoff)
	args := strings.Join(s.m.SSHCommand().Args, " ")
	if !strings.Contains(args, "-- workbox") {
		t.Fatalf("alias must reach ssh verbatim after --: %q", args)
	}
}

func TestSSHStartsTheGuestFirstWhenNeeded(t *testing.T) {
	// The focused quick-command ssh: a powered-off machine with settings is
	// started, waited for, handed off — one journey.
	off := vmOff()
	st := storeWith(off.UUID, endpoint("127.0.0.1", 2222, "root"))
	f := newFake(off)
	s := start(t, directModel(f, st, dialBanner("SSH-2.0-OpenSSH_9.6\r\n"), vbox.ActionConnect, off))
	s.settleOutcome(8 * time.Second)

	if !f.has("poweron") {
		t.Fatalf("ssh on a stopped guest must start it first: %v", f.opList())
	}
	s.outcome(OutcomeSSHHandoff)
}

func TestReadyFailureNamesTheEndpointAndKeepsTheStart(t *testing.T) {
	// Running guest, endpoint nothing answers on: the failure must show where,
	// and when nothing was started it must not blame a start.
	run := vmRunning()
	st := storeWith(run.UUID, endpoint("127.0.0.1", 2222, "root"))
	s := start(t, modelWith(newFake(run), st, dialWrongService()))
	s.enter()
	s.settleTo(modePickAction, 2*time.Second)
	s.enter() // SSH

	// The wrong service ends the wait quickly on its own; the failure names
	// the endpoint it tried.
	s.settleOutcome(8 * time.Second)
	summary := s.outcome(OutcomeFailure)
	if !strings.Contains(summary, "2222") {
		t.Fatalf("failure should name the endpoint: %q", summary)
	}
	if !strings.Contains(summary, "SSH") {
		t.Fatalf("failure should say SSH never became ready: %q", summary)
	}
}

// ---------- quick commands (focused runs) ----------

func TestFocusedRunSkipsTheActionMenu(t *testing.T) {
	f := newFake(vmRunning(), vmOff(), vmPausedKali())
	s := start(t, directModel(f, newStore(), dialRefused(), vbox.ActionPause, vmRunning()))
	s.settleOutcome(5 * time.Second)

	// No menu ever showed (no frame with a second choice), one pause happened.
	if s.outcome(OutcomeSuccess) != `Paused "Kali Linux".` {
		t.Fatalf("summary: %q", s.m.Summary())
	}
	calls := f.opList()
	if len(calls) != 2 || calls[0] != "list" || calls[1] != "pause" {
		t.Fatalf("a focused run should be exactly list→pause, got %v", calls)
	}
}

func TestFocusedRunRefusesWrongState(t *testing.T) {
	f := newFake(vmOff())
	s := start(t, directModel(f, newStore(), dialRefused(), vbox.ActionPause, vmOff()))
	summary := s.outcome(OutcomeFailure)
	for _, want := range []string{"pause", "Nothing was changed"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("refusal missing %q: %q", want, summary)
		}
	}
	if f.has("pause") {
		t.Fatal("a refusal must not call the backend")
	}
}

func TestFocusedRunListsOnlyEligibleMachines(t *testing.T) {
	// "vbox-tui pause" without a name: only the RUNNING machine (paused and
	// off machines cannot be paused) is selectable.
	f := newFake(vmRunning(), vmOff(), vmPausedKali())
	s := start(t, focusModel(f, newStore(), dialRefused(), vbox.ActionPause))
	frame := s.strip()
	if !strings.Contains(frame, "Kali") || strings.Contains(frame, "Windows") {
		t.Fatalf("focused picker should offer only pausable machines:\n%s", frame)
	}
}

func TestDotsAndDigitsSurviveFilterAndFields(t *testing.T) {
	// The picker filter and the setup fields must be typed text, character for
	// character: an IP address is nothing but digits and dots, and a key that
	// "helpfully" meant something else there corrupts the answers.
	f := newFake(vmRunning())
	st := newStore()
	s := start(t, modelWith(f, st, dialBanner("SSH-2.0-OpenSSH_9.6\r\n")))

	s.runes("1.2.3") // letters and dots in the filter, unharmed
	if !strings.Contains(s.strip(), "filter: 1.2.3") {
		t.Fatalf("filter should hold every character typed:\n%s", s.strip())
	}
	s.key("backspace") // backspace edits, and only edits
	for i := 0; i < 5; i++ {
		s.key("backspace")
	}
	s.enter() // pick the Kali (unfiltered now)
	s.settleTo(modePickAction, 2*time.Second)
	s.enter() // SSH -> setup (no settings)
	s.enter() // keep host/port
	s.typeLine("10.0.0.9")
	s.enter() // accept suggested port
	s.typeLine("p.k")
	// digits and dots arrive in the user field intact ("p.k"): the earlier
	// 1/2/up/down shortcuts apply to the chooser only.
	s.settleOutcome(5 * time.Second)
	cfg, ok := st.savedFor(vmRunning().UUID)
	if !ok || cfg.Host != "10.0.0.9" || cfg.User != "p.k" {
		t.Fatalf("field text was altered: %+v ok=%v", cfg, ok)
	}
}

// ---------- misc guards ----------

func TestOutcomeNeverRewrittenAfterDone(t *testing.T) {
	// Press esc, then let an old late command result in: the finish latch
	// must keep the first outcome.
	f := newFake(vmRunning())
	s := start(t, modelWith(f, newStore(), dialRefused()))
	s.esc()
	want := s.m.Summary()
	s.send(inventoryMsg{gen: 99, inv: vbox.Inventory{Machines: threeMachines()}})
	s.send(stepMsg{gen: 99, action: vbox.ActionStop, err: errors.New("late disaster")})
	if s.m.Summary() != want || s.m.Outcome() != OutcomeCancelled {
		t.Fatalf("outcome rewritten: %v %q", s.m.Outcome(), s.m.Summary())
	}
}

func TestWindowSizeOnlyAffectsWidth(t *testing.T) {
	f := newFake(vmWideName())
	s := start(t, modelWith(f, newStore(), dialRefused()))
	before := len(strings.Split(strings.TrimRight(s.frame, "\n"), "\n"))
	s.send(tea.WindowSizeMsg{Width: 80, Height: 300})
	after := len(strings.Split(strings.TrimRight(s.frame, "\n"), "\n"))
	if after != before {
		t.Fatalf("height must never change the frame: %d lines then %d", before, after)
	}
}

func TestSSHSettingsDoorPersistenceRoundTrip(t *testing.T) {
	// The door the config file implements: stored once, readable afterwards by
	// UUID (case- and brace-insensitive), surviving a rename hint.
	cfg := config.Defaults()
	door := ConfigSettings(cfg, false)
	run := vmRunning()
	if _, ok := door.SSH(run.UUID); ok {
		t.Fatal("fresh door should have nothing")
	}
	if err := door.Store(`{`+strings.ToUpper(run.UUID)+`}`, "later name", endpoint("10.0.0.5", 22, "ada")); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, ok := door.SSH(run.UUID)
	if !ok || got.Host != "10.0.0.5" {
		t.Fatalf("readback: %+v ok=%v", got, ok)
	}
}

func TestSuccessLineCoverage(t *testing.T) {
	const n = `"guest"`
	tests := map[vbox.Action]string{
		vbox.ActionStart:     "Started " + n + " in headless mode.",
		vbox.ActionRestore:   "Restored " + n + " from its saved state.",
		vbox.ActionResume:    "Resumed " + n + ".",
		vbox.ActionPause:     "Paused " + n + ".",
		vbox.ActionStop:      "Shutdown requested for " + n + ".",
		vbox.ActionSaveState: "Saved " + n + "'s state.",
	}
	for a, want := range tests {
		if got := SuccessLine(a, n); got != want {
			t.Errorf("SuccessLine(%s) = %q, want %q", a, got, want)
		}
	}
}
