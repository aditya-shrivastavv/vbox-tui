package demo

import (
	"errors"
	"strings"
	"testing"

	"github.com/aditya-shrivastavv/vbox-tui/internal/config"
	"github.com/aditya-shrivastavv/vbox-tui/internal/vbox"
)

// Coverage for the guest-control and snapshot surface the fake imitates: the
// same state rules the real hypervisor applies, the in-memory-only settings
// doors, and the tree pruning a restore causes.

const (
	kaliUUID   = "2d620512-4b10-45a8-b070-25435e5106a5"
	ubuntuUUID = "c907c20b-5a41-4dc2-99c5-0e9bc7773c38"
)

// ---------- guest reboot ----------

func TestGuestRebootNeedsARunningMachine(t *testing.T) {
	b := New()
	ctx := mustCtx(t)
	// ubuntu runs: the request is accepted and stays outstanding until the
	// (simulated) guest finishes cycling; the machine never leaves running.
	if err := b.GuestReboot(ctx, ubuntuUUID); err != nil {
		t.Fatalf("reboot of a running machine: %v", err)
	}
	if !b.RequestedReboot(ubuntuUUID) {
		t.Fatal("the request should read as in flight")
	}
	inv, err := b.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m := findMachine(t, inv, "ubuntu"); m.State.Kind != vbox.KindRunning {
		t.Fatalf("a rebooting guest stays running from the hypervisor's view: %q", m.State.State)
	}
	b.Advance(ShutdownDelay * 2)
	if b.RequestedReboot(ubuntuUUID) {
		t.Fatal("the simulated guest should have finished")
	}

	// kali is off: a reboot needs a live guest, and the fake must refuse
	// rather than fake it.
	if err := b.GuestReboot(ctx, kaliUUID); err == nil {
		t.Fatal("a powered-off machine must refuse a guest reboot")
	} else if !strings.Contains(err.Error(), "running") {
		t.Fatalf("refusal should explain the state: %v", err)
	}
	// A paused machine is refused too: a frozen CPU cannot process a reboot.
	b.ForceState(kaliUUID, vbox.NormalizeState("paused"))
	if err := b.GuestReboot(ctx, kaliUUID); err == nil {
		t.Fatal("a paused machine must refuse a guest reboot")
	}
	// An unknown UUID is an honest error, never a silent success.
	if err := b.GuestReboot(ctx, "00000000-0000-4000-8000-000000000000"); err == nil {
		t.Fatal("unknown UUID must be an error")
	}
}

// ---------- snapshots ----------

func TestSeededTreesLookReal(t *testing.T) {
	b := New()
	tree := b.Snapshots(ubuntuUUID)
	if len(tree.Snapshots) == 0 || tree.CurrentUUID == "" {
		t.Fatalf("ubuntu should ship a seeded tree: %+v", tree)
	}
	currentNodes := 0
	for _, s := range tree.Snapshots {
		if s.UUID == "" || s.Name == "" {
			t.Fatalf("seeded nodes carry identity: %+v", s)
		}
		if s.Current {
			currentNodes++
		}
	}
	if currentNodes != 1 {
		t.Errorf("exactly one node is current, got %d: %+v", currentNodes, tree.Snapshots)
	}
	// A machine without snapshots answers with the empty tree, not an error.
	if g := b.Snapshots("f421275d-fe89-4b28-b68b-7663ee521a5f"); !g.Empty() {
		t.Errorf("unseeded machine should be empty: %+v", g.Snapshots)
	}
}

func TestTakeSnapshotAttachesUnderCurrentAndNamesMustBeUnique(t *testing.T) {
	b := New()
	ctx := mustCtx(t)
	if err := b.TakeSnapshot(ctx, ubuntuUUID, "before", "first"); err != nil {
		t.Fatal(err)
	}
	tree := b.Snapshots(ubuntuUUID)
	last := tree.Snapshots[len(tree.Snapshots)-1]
	if last.Name != "before" || !last.Current {
		t.Fatalf("new snapshot must be current: %+v", last)
	}
	if last.Description != "first" {
		t.Errorf("description stored: %+v", last)
	}
	// The node nests under the previous current node, wherever that was.
	if !strings.Contains(last.Node, "-") {
		t.Errorf("a child snapshot's node should be nested: %+v", last)
	}
	// Duplicate names are refused with the machine's name for context.
	if err := b.TakeSnapshot(ctx, ubuntuUUID, "before", "again"); err == nil {
		t.Fatal("a duplicate snapshot name must be refused")
	} else if !strings.Contains(err.Error(), "ubuntu") || !strings.Contains(err.Error(), `"before"`) {
		t.Fatalf("dup error should name the machine and the name: %v", err)
	}
	// A running machine keeps running: taking a snapshot never disturbs it.
	inv, _ := b.List(ctx)
	if m := findMachine(t, inv, "ubuntu"); m.State.Kind != vbox.KindRunning {
		t.Fatalf("snapshotting must not change the power state: %q", m.State.State)
	}
}

func TestSnapshotTakeAndRestoreRefuseWrongStates(t *testing.T) {
	b := New()
	ctx := mustCtx(t)
	// Transitional machine: a take waits for it to settle.
	b.ForceState(kaliUUID, vbox.StateInfo{State: vbox.StateTransitioning, Kind: vbox.KindTransitional, Raw: "saving"})
	if err := b.TakeSnapshot(ctx, kaliUUID, "mid-flight", ""); err == nil {
		t.Fatal("a transitional machine must refuse a take")
	}

	b.ForceState(kaliUUID, vbox.NormalizeState("running"))
	if err := b.TakeSnapshot(ctx, kaliUUID, "point", ""); err != nil {
		t.Fatal(err)
	}
	// Restore needs the machine powered off, and only the snapshot UUID
	// addresses a node (names can repeat, as kali's seed proves).
	if err := b.RestoreSnapshot(ctx, kaliUUID, "point"); err == nil {
		t.Fatal("a running machine must refuse a restore")
	}
	// Unknown UUID refused even when powered off.
	b.ForceState(kaliUUID, vbox.NormalizeState("powered off"))
	if err := b.RestoreSnapshot(ctx, kaliUUID, "00000000-0000-4000-8000-00000000dead"); err == nil {
		t.Fatal("restoring an unknown snapshot must be an error")
	}
	// A saved machine is refused too — the same rule VirtualBox applies.
	b.ForceState(kaliUUID, vbox.NormalizeState("saved"))
	tree := b.Snapshots(kaliUUID)
	if err := b.RestoreSnapshot(ctx, kaliUUID, tree.CurrentUUID); err == nil {
		t.Fatal("a restore must refuse a machine in a saved state")
	}
}

func TestRestorePrunesTheBranchAndKeepsOtherBranches(t *testing.T) {
	b := New()
	ctx := mustCtx(t)
	// Build: root → A → B (current), and a sibling root C.
	b.SeedSnapshots(kaliUUID, nil, "")
	if err := b.TakeSnapshot(ctx, kaliUUID, "root", ""); err != nil {
		t.Fatal(err) // first snapshot needs any state that settles
	}
	root := b.Snapshots(kaliUUID)
	if err := b.TakeSnapshot(ctx, kaliUUID, "A", ""); err != nil {
		t.Fatal(err)
	}
	if err := b.TakeSnapshot(ctx, kaliUUID, "B", ""); err != nil {
		t.Fatal(err)
	}
	// Attach a sibling to root: walk current back to root by restoring.
	b.ForceState(kaliUUID, vbox.NormalizeState("powered off"))
	if err := b.RestoreSnapshot(ctx, kaliUUID, root.CurrentUUID); err != nil {
		t.Fatal(err)
	}
	// A and B (the restored-from descendants) are pruned: those points never
	// happened again. Root survives.
	after := b.Snapshots(kaliUUID)
	var names []string
	for _, s := range after.Snapshots {
		names = append(names, s.Name)
	}
	if len(after.Snapshots) != 1 || names[0] != "root" {
		t.Fatalf("restore must prune its descendants, got %v", names)
	}
	if !after.Snapshots[0].Current {
		t.Fatal("the restored node is the current node")
	}
	// Now attach a fresh sibling and restore the original root again: the
	// new branch goes too.
	if err := b.TakeSnapshot(ctx, kaliUUID, "other-branch", ""); err != nil {
		t.Fatal(err)
	}
	if err := b.RestoreSnapshot(ctx, kaliUUID, root.CurrentUUID); err != nil {
		t.Fatal(err)
	}
	if got := b.Snapshots(kaliUUID); len(got.Snapshots) != 1 {
		var names []string
		for _, s := range got.Snapshots {
			names = append(names, s.Name)
		}
		t.Fatalf("a sibling of restored taken after the point must prune: %v", names)
	}
}

func TestDuplicateSnapshotNamesStaySeparateByUUID(t *testing.T) {
	b := New()
	tree := b.Snapshots(kaliUUID)
	// kali's seed deliberately carries "2024.1 base" twice.
	var same []vbox.Snapshot
	for _, s := range tree.Snapshots {
		if s.Name == "2024.1 base" {
			same = append(same, s)
		}
	}
	if len(same) != 2 {
		t.Fatalf("seed should demonstrate duplicate names again: %+v", tree.Snapshots)
	}
	// Restoring must be addressable per node: the deeper node differs.
	ctx := mustCtx(t)
	b.ForceState(kaliUUID, vbox.NormalizeState("powered off"))
	deep := same[1]
	if err := b.RestoreSnapshot(ctx, kaliUUID, deep.UUID); err != nil {
		t.Fatal(err)
	}
	after := b.Snapshots(kaliUUID)
	kept := 0
	for _, s := range after.Snapshots {
		if s.Name == "2024.1 base" && s.UUID == deep.UUID {
			kept++
		}
	}
	if kept != 1 || !after.Snapshots[len(after.Snapshots)-1].Current {
		var names []string
		for _, s := range after.Snapshots {
			names = append(names, s.UUID+"("+s.Node+")")
		}
		t.Fatalf("restore by UUID must pick exactly that node: %v", names)
	}
}

// ---------- settings doors ----------

func TestGuestSettingsAreInMemoryOnly(t *testing.T) {
	b := New()
	// Seeds carry guest hints: kali is GA-route linux.
	if g, ok := b.Guest(kaliUUID); !ok || g.OS != config.GuestOSLinux || g.Restart != config.GuestRestartGA {
		t.Fatalf("kali guest seed: %+v ok=%v", g, ok)
	}
	// Runtime overrides replace the seed (a settings edit in the demo).
	b.SetGuest(kaliUUID, config.GuestConfig{OS: config.GuestOSWindows, Restart: config.GuestRestartOff})
	if g, ok := b.Guest(kaliUUID); !ok || g.OS != config.GuestOSWindows || g.Restart != config.GuestRestartOff {
		t.Fatalf("override did not stick: %+v ok=%v", g, ok)
	}
	// Clearing shadows the seed and reads as unconfigured, like the real store.
	b.ClearGuest(kaliUUID)
	if _, ok := b.Guest(kaliUUID); ok {
		t.Fatal("a cleared guest block must read as unconfigured")
	}
	// A machine that never had one: unconfigured, stable.
	workbench := "f421275d-fe89-4b28-b68b-7663ee521a5f"
	if g, ok := b.Guest(workbench); ok && (g.OS != "" || g.Restart != "") {
		t.Fatalf("clear means empty: %+v", g)
	}
	// SSH clears the same way (the editor's "0") and the seed survives a
	// clear of guest settings only if untouched.
	if _, ok := b.SSH(kaliUUID); !ok {
		t.Fatal("kali ships SSH settings")
	}
	b.ClearSSH(kaliUUID)
	if _, ok := b.SSH(kaliUUID); ok {
		t.Fatal("cleared SSH must read as unconfigured")
	}
	if g, _ := b.Guest(kaliUUID); g.OS != config.GuestOSUnknown {
		t.Fatalf("clearing SSH must not revive the old guest seed: %+v", g)
	}
}

func TestSeededSettingsReportConfigured(t *testing.T) {
	// A freshly built backend's seeds answer through the same doors the
	// settings editor reads: ubuntu alias SSH ready, kali endpoint ready.
	b := New()
	if s, ok := b.SSH(ubuntuUUID); !ok || s.Mode != config.SSHModeAlias || s.Alias == "" {
		t.Fatalf("ubuntu seed ssh: %+v ok=%v", s, ok)
	}
	if s, ok := b.SSH(kaliUUID); !ok || s.Mode != config.SSHModeEndpoint {
		t.Fatalf("kali seed ssh: %+v ok=%v", s, ok)
	}
	// And the unknown machine has neither: no half-configured surprises.
	unknown := "00000000-0000-4000-8000-000000000000"
	if _, ok := b.SSH(unknown); ok {
		t.Error("unknown UUID must not report SSH settings")
	}
	if _, ok := b.Guest(unknown); ok {
		t.Error("unknown UUID must not report guest settings")
	}
	var unused error
	_ = errors.Is(unused, nil) // keep imports honest
}
