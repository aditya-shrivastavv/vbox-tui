package app

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Regression tests for the guest editor's Escape/Ctrl+C behaviour and for
// chooser identity (bug: handleGuestEditKey never handled Escape at step 0,
// and beginGuestEdit left chooseKind whatever the previous chooser had
// been, so a stale batch selection could route these keys elsewhere).

// openGuestEditor walks the focused settings menu to the guest editor's
// OS step: the guest item is index 2 of the menu (edit ssh, reset ssh,
// guest …).
func openGuestEditor(s *session) {
	s.down()
	s.down()
	s.enter()
	s.settleTo(modeChoose, 2*time.Second)
}

func TestGuestEditorEscapeAtOSStepReturnsToSettingsNoWrite(t *testing.T) {
	off := vmOff()
	f := newFake(off)
	st := storeWith(off.UUID, endpoint("127.0.0.1", 2222, "root"))
	g := newGuest()
	s := start(t, guestModel(f, st, g, off))
	openGuestEditor(s)
	// Move the cursor first: esc must abandon even a changed selection.
	s.down()
	s.esc() // the previously dead key at step 0
	if got := s.m.modeForTest(); got != modeSettings {
		s.shot()
		t.Fatalf("esc at the OS step must return to the settings menu, at %v:\n%s", got, s.strip())
	}
	// The cursor lands on the item that opened the editor — the selection
	// state is honestly restored, not dumped at the top.
	if s.m.settingsCursor != 2 {
		t.Fatalf("settings cursor must rest on the guest item, got %d", s.m.settingsCursor)
	}
	if n := g.writeCount(); n != 0 {
		t.Fatalf("esc through the guest editor must write nothing: %d writes", n)
	}
	if _, ok := g.value(off.UUID); ok {
		t.Fatal("no guest settings may exist after esc")
	}
	// The menu is alive again: esc from it exits the focused run cleanly.
	s.esc()
	s.settleOutcome(2 * time.Second)
	s.outcome(OutcomeSuccess)
	if n := g.writeCount(); n != 0 {
		t.Fatalf("still nothing written: %d", n)
	}
}

func TestGuestEditorEscapeAtSecondStepReturnsToOSStepNoWrite(t *testing.T) {
	off := vmOff()
	f := newFake(off)
	st := storeWith(off.UUID, endpoint("127.0.0.1", 2222, "root"))
	g := newGuest()
	s := start(t, guestModel(f, st, g, off))
	openGuestEditor(s)
	s.enter() // step 1: the restart route
	s.waitForFrame("How should this guest be restarted?", 2*time.Second)
	s.down() // change the restart choice, then step back: it must be abandoned
	s.esc()
	s.waitForFrame("What guest OS runs in", 2*time.Second)
	if s.m.guestStep != 0 {
		t.Fatalf("esc from the restart step must sit back at the OS step, got %d", s.m.guestStep)
	}
	if n := g.writeCount(); n != 0 {
		t.Fatalf("stepping back writes nothing: %d", n)
	}
	// From the OS step, esc now leaves the editor entirely — still nothing.
	s.esc()
	if got := s.m.modeForTest(); got != modeSettings {
		t.Fatalf("second esc must reach the settings menu, at %v", got)
	}
	if n := g.writeCount(); n != 0 {
		t.Fatalf("two esc presses wrote %d times", n)
	}
}

func TestGuestEditorCtrlCCancelsFromBothStepsWithoutWriting(t *testing.T) {
	for _, step := range []int{0, 1} {
		off := vmOff()
		f := newFake(off)
		st := storeWith(off.UUID, endpoint("127.0.0.1", 2222, "root"))
		g := newGuest()
		s := start(t, guestModel(f, st, g, off))
		openGuestEditor(s)
		if step == 1 {
			s.enter()
			s.waitForFrame("How should this guest be restarted?", 2*time.Second)
		}
		s.send(tea.KeyMsg{Type: tea.KeyCtrlC})
		s.settleOutcome(2 * time.Second)
		got := s.outcome(OutcomeCancelled)
		if got != "Cancelled. No settings were changed." {
			t.Fatalf("step %d ctrl+c: %q", step, got)
		}
		if n := g.writeCount(); n != 0 {
			t.Fatalf("step %d: ctrl+c wrote %d times", step, n)
		}
		if _, ok := g.value(off.UUID); ok {
			t.Fatalf("step %d: guest settings exist after a cancel", step)
		}
	}
}

// The chooser-identity regression: after visiting AND abandoning the batch
// chooser (which sets chooseKind=chooseBatchVerb), entering the guest editor
// must re-establish its own chooser identity. If it did not, the OS list
// would render as a verb list and the keys would plan a batch.
func TestGuestEditorAfterAbandonedBatchUsesGuestRouting(t *testing.T) {
	off1, off2 := vmOff(), anotherOff()
	f := newFake(off1, off2)
	g := newGuest()
	o := baseOptions(f, newStore(), dialRefused())
	o.GuestSettings = g
	s := start(t, New(o))
	// Mark both machines and reach the batch action chooser…
	s.key(" ")
	s.down()
	s.key(" ")
	s.enter()
	s.settleTo(modeChoose, 2*time.Second)
	s.shot()
	if !strings.Contains(s.strip(), "marked machines") {
		t.Fatalf("expected the batch chooser:\n%s", s.strip())
	}
	// …and abandon it with esc. Stale chooser state stays in the model;
	// that is exactly what must not leak into the next chooser.
	s.esc() // back to the picker, marks still set
	s.drain(ms(50))
	if got := s.m.modeForTest(); got != modePickVM {
		t.Fatalf("esc from the batch chooser returns to the picker, at %v", got)
	}
	s.esc() // release the marks so Enter picks a machine instead of a batch
	s.drain(ms(50))
	if s.m.markedCount() != 0 {
		t.Fatalf("esc must release the marks, %d still set", s.m.markedCount())
	}
	s.enter() // pick the highlighted machine → action menu
	s.settleTo(modePickAction, 2*time.Second)
	// Off menu: start, start+ssh, rename, snapshots, settings.
	for i := 0; i < 4; i++ {
		s.down()
	}
	s.shot()
	if !strings.Contains(s.strip(), "SSH and restart settings…") {
		t.Fatalf("the cursor must sit on the settings item:\n%s", s.strip())
	}
	s.enter()
	s.settleTo(modeSettings, 2*time.Second)
	openGuestEditor(s)
	s.shot()
	frame := s.strip()
	// The routing proof: the guest question, not a batch verb list.
	if !strings.Contains(frame, "What guest OS runs in") {
		t.Fatalf("guest editor must show its own chooser after an abandoned batch:\n%s", frame)
	}
	// Escape must leave to settings — the original dead key — and no batch
	// may have been planned or run on the way.
	s.esc()
	if got := s.m.modeForTest(); got != modeSettings {
		s.shot()
		t.Fatalf("esc must return to settings, at %v:\n%s", got, s.strip())
	}
	if n := g.writeCount(); n != 0 {
		t.Fatalf("routing through stale batch state must not write: %d", n)
	}
	for _, op := range f.opList() {
		if op != "list" && op != "detail" {
			t.Fatalf("the journey changed a machine (%s): %v", op, f.opList())
		}
	}
}
