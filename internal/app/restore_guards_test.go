package app

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aditya-shrivastavv/vbox-tui/internal/config"
	"github.com/aditya-shrivastavv/vbox-tui/internal/vbox"
)

// The shared restore door and the gates around the destructive path: state
// and target are rechecked immediately before the request, the protective
// snapshot's identity is read back instead of recomputed, and any doubt at
// either gate aborts before a single snapshot is discarded.

const targetSnapUUID = "10000000-0000-4000-8000-000000000001"

// ---------- the shared door, at unit level ----------

func TestRestoreDoorRefusesByFreshState(t *testing.T) {
	run := vmRunning()
	f := newFake(run)
	f.seedSnapshots(run.UUID, seedTree(), targetSnapUUID)
	_, err := RestoreMachine(t.Context(), f, run.UUID, targetSnapUUID)
	var refused *RestoreRefused
	if !errors.As(err, &refused) {
		t.Fatalf("a running machine must be refused by the door: %v", err)
	}
	if f.has("snaprestore") {
		t.Fatal("the door refused before the restore call")
	}
	// The refusal names the state and the operator's reality: no auto-stop.
	if !strings.Contains(err.Error(), "Running") || !strings.Contains(err.Error(), "powered-off") {
		t.Errorf("refusal wording: %v", err)
	}
}

func TestRestoreDoorRefusesWhenTargetVanished(t *testing.T) {
	off := vmOff()
	f := newFake(off)
	f.seedSnapshots(off.UUID, seedTree()[:1], targetSnapUUID) // only the other node exists
	_, err := RestoreMachine(t.Context(), f, off.UUID, "00000000-0000-4000-8000-000000000000")
	if err == nil {
		t.Fatal("a snapshot that is not in the tree must stop the restore")
	}
	if !strings.Contains(err.Error(), "no longer in the snapshot tree") {
		t.Errorf("wording: %v", err)
	}
	if f.has("snaprestore") {
		t.Fatal("nothing may be restored for a vanished target")
	}
}

func TestRestoreDoorRestoresExactUUIDWhenOff(t *testing.T) {
	off := vmOff()
	f := newFake(off)
	f.seedSnapshots(off.UUID, seedTree(), "10000000-0000-4000-8000-000000000002")
	if _, err := RestoreMachine(t.Context(), f, off.UUID, targetSnapUUID); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	cur := f.snapCur[config.CanonicalUUID(off.UUID)]
	f.mu.Unlock()
	if cur != config.CanonicalUUID(targetSnapUUID) {
		t.Fatalf("the chosen UUID, not a name match, must be restored: %q", cur)
	}
}

// ---------- the interactive gates ----------

// startWhileConfirmOpen flips the machine to running between the state gate
// and the restore: exactly the race where a precheck cannot promise, so the
// door's fresh read is what must stop it.
func TestRestoreDoorStopsWhenStateMovesAfterTheGate(t *testing.T) {
	off := vmOff()
	f := newFake(off)
	f.seedSnapshots(off.UUID, seedTree(), "10000000-0000-4000-8000-000000000002")
	// List reads: 1 = initial inventory, 2 = the action-menu read, 3 = the
	// gate's fresh state read, 4 = the door's read after 's'. Flipping at
	// four lets the gate and confirmation happen on an off machine and
	// moves the state during the confirmation window — the door must stop
	// it there.
	f.onList = func(calls int, inv *vbox.Inventory) {
		if calls >= 4 {
			for i := range inv.Machines {
				inv.Machines[i].State = vbox.NormalizeState("running")
			}
		}
	}
	s := start(t, modelWith(f, newStore(), dialRefused()))
	s.enter()
	s.settleTo(modePickAction, 2*time.Second)
	for i := 0; i < 3; i++ { // to Snapshots on the off menu
		s.down()
	}
	s.enter()
	s.settleTo(modeSnapList, 2*time.Second)
	s.enter() // the state gate also reads List (call 2, while off)
	s.settleTo(modeConfirm, 3*time.Second)
	s.key("s") // restore without protection
	s.settleOutcome(5 * time.Second)
	got := s.outcome(OutcomeFailure)
	for _, want := range []string{"Cannot restore", "Running", "Nothing was restored"} {
		if !strings.Contains(got, want) {
			t.Fatalf("door refusal missing %q: %q", want, got)
		}
	}
	if f.has("snaprestore") {
		t.Fatal("the door stopped the restore before the hypervisor")
	}
}

// A protective take that claims success but leaves no new node in the tree
// must abort the restore: discarding the only copy on an unproven promise
// is the failure mode the read-back exists to catch.
func TestProtectiveTakeThatLeavesNoNodeAbortsRestore(t *testing.T) {
	off := vmOff()
	f := newFake(off)
	f.seedSnapshots(off.UUID, seedTree(), "10000000-0000-4000-8000-000000000002")
	// The take reports success, then silently removes the node it created:
	// the read-back sees the old current still current, not a protective
	// node.
	f.afterTake = func(uuid string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		key := config.CanonicalUUID(uuid)
		list := f.snapshots[key]
		if len(list) > 0 {
			list = list[:len(list)-1]
		}
		f.snapshots[key] = list
		f.snapCur[key] = config.CanonicalUUID("10000000-0000-4000-8000-000000000002")
	}
	s := start(t, modelWith(f, newStore(), dialRefused()))
	s.enter()
	s.settleTo(modePickAction, 2*time.Second)
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
	for _, want := range []string{"Restore aborted", "did not appear", "Nothing was restored"} {
		if !strings.Contains(got, want) {
			t.Fatalf("abort line missing %q: %q", want, got)
		}
	}
	if f.has("snaprestore") {
		t.Fatal("an unproven protective copy must stop the restore")
	}
}

// A protective take confirmed in the tree, then the machine starts before
// the restore line is reached: the confirm gate aborts on the fresh state
// read, names the protective snapshot it found, and restores nothing.
func TestStateChangeAfterProtectiveTakeAbortsRestore(t *testing.T) {
	off := vmOff()
	f := newFake(off)
	f.seedSnapshots(off.UUID, seedTree(), "10000000-0000-4000-8000-000000000002")
	// The protective take succeeds, then another tool starts the machine:
	// the confirm gate's List sees running.
	f.afterTake = func(uuid string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		for i := range f.machines {
			if strings.EqualFold(f.machines[i].UUID, uuid) {
				f.machines[i].State = vbox.NormalizeState("running")
			}
		}
	}
	s := start(t, modelWith(f, newStore(), dialRefused()))
	s.enter()
	s.settleTo(modePickAction, 2*time.Second)
	for i := 0; i < 3; i++ {
		s.down()
	}
	s.enter()
	s.settleTo(modeSnapList, 2*time.Second)
	s.enter()
	s.settleTo(modeConfirm, 3*time.Second)
	s.key("y")
	s.settleOutcome(5 * time.Second)
	got := s.outcome(OutcomeFailure)
	for _, want := range []string{"Restore aborted", "Running", "is kept as snapshot"} {
		if !strings.Contains(got, want) {
			t.Fatalf("abort line missing %q: %q", want, got)
		}
	}
	if f.has("snaprestore") {
		t.Fatal("a running machine must not be restored beneath the gate")
	}
}

// The reported protective identity is whatever the tree actually shows, not
// a timestamp vbx hoped to store: the fake renames the protective node and
// the success line must repeat the stored name.
func TestProtectiveIdentityIsReportedFromTheTree(t *testing.T) {
	defer withFixedSnapshotTime(t, "2026-05-05 10:11:12")()
	off := vmOff()
	f := newFake(off)
	f.seedSnapshots(off.UUID, seedTree(), "10000000-0000-4000-8000-000000000002")
	// VirtualBox stored the protective node under a different name than
	// asked (it renames colliding nodes on its own): the success line must
	// quote the stored name, never the recomputed timestamp name.
	f.afterTake = func(uuid string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		key := config.CanonicalUUID(uuid)
		list := f.snapshots[key]
		if len(list) > 0 {
			list[len(list)-1].Name = "autosave-2 (renamed by VirtualBox)"
			f.snapshots[key] = list
		}
	}
	s := start(t, modelWith(f, newStore(), dialRefused()))
	s.enter()
	s.settleTo(modePickAction, 2*time.Second)
	for i := 0; i < 3; i++ {
		s.down()
	}
	s.enter()
	s.settleTo(modeSnapList, 2*time.Second)
	s.enter()
	s.settleTo(modeConfirm, 3*time.Second)
	s.key("y")
	s.settleOutcome(6 * time.Second)
	got := s.outcome(OutcomeSuccess)
	if !strings.Contains(got, "kept as snapshot \"autosave-2 (renamed by VirtualBox)\"") {
		t.Fatalf("the line must name the actual stored snapshot: %q", got)
	}
	if strings.Contains(got, "vbx 2026-05-05 10:11:12 (protective)") {
		t.Fatalf("the recomputed timestamp name must not appear: %q", got)
	}
}

// With a protective take requested, the door is still the last check:
// RestoreMachine revalidates state and target again after the protective
// confirmation, so a hypervisor that starts refusing mid-flight is caught
// rather than blindly asked (the door's own List sees the failure).
func TestProtectivePathStillPassesThroughTheDoor(t *testing.T) {
	off := vmOff()
	f := newFake(off)
	f.seedSnapshots(off.UUID, seedTree(), "10000000-0000-4000-8000-000000000002")
	// The door re-reads the snapshot tree immediately before restoring;
	// making that read fail proves the enforcement sits in the shared
	// layer: its failure surfaces verbatim, and no restore is attempted.
	// Snapshot reads: 1 = opening the tree, 2 = the protective check,
	// 3 = the door's own fresh read.
	s := start(t, modelWith(f, newStore(), dialRefused()))
	s.enter()
	s.settleTo(modePickAction, 2*time.Second)
	for i := 0; i < 3; i++ {
		s.down()
	}
	s.enter()
	s.settleTo(modeSnapList, 2*time.Second) // snaplist 1
	s.enter()
	s.settleTo(modeConfirm, 3*time.Second)
	// Fail every snapshot read from the third onward — the door's read.
	f.mu.Lock()
	f.snapErr = errors.New("snapshot tree unreadable")
	f.snapErrFrom = 3
	f.mu.Unlock()
	s.key("y")
	s.settleOutcome(6 * time.Second)
	got := s.outcome(OutcomeFailure)
	// The restore attempt happened through the door, and the hypervisor-
	// side error is surfaced verbatim rather than silently swallowed.
	if !strings.Contains(got, "Could not restore") || !strings.Contains(got, "snapshot tree unreadable") {
		t.Fatalf("the door's read failure must reach the line: %q", got)
	}
	if f.has("snaprestore") {
		t.Fatal("a door read failure means nothing was sent")
	}
}
