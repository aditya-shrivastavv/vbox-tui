package vbox

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// Snapshot control surface: what the guest restart may and may not issue, how
// the machine-readable snapshot listing parses, and the exact argument
// vectors for the snapshot verbs.

// ---------- guest reboot ----------

func TestGuestRebootIsControlvmRebootNeverReset(t *testing.T) {
	cli, r := newCLIWithFixtures(t, fixtures{})
	const uuid = "2d620512-4b10-45a8-b070-25435e5106a5"
	if err := cli.GuestReboot(context.Background(), uuid); err != nil {
		t.Fatalf("GuestReboot: %v", err)
	}
	calls := r.recorded()
	if len(calls) != 1 {
		t.Fatalf("one call expected: %v", calls)
	}
	want := []string{cli.Path(), "--nologo", "controlvm", uuid, "reboot"}
	if strings.Join(calls[0], "|") != strings.Join(want, "|") {
		t.Errorf("args = %v\nwant %v", calls[0], want)
	}
	for _, a := range calls[0] {
		for _, forbidden := range []string{"reset", "poweroff", "saveState", "discardstate"} {
			if strings.EqualFold(a, forbidden) {
				t.Errorf("%q issued: a graceful restart must never %s", a, forbidden)
			}
		}
	}
}

func TestGuestRebootRefusesEmptyUUID(t *testing.T) {
	cli, r := newCLIWithFixtures(t, fixtures{})
	if err := cli.GuestReboot(context.Background(), "  "); err == nil {
		t.Fatal("an empty UUID must be refused before anything runs")
	}
	if len(r.recorded()) != 0 {
		t.Fatalf("nothing may run: %v", r.recorded())
	}
}

// ---------- snapshot list parsing ----------

const machineReadableTree = `Name="lab"
Groups=
SnapshotName="clean-install"
SnapshotUUID="a1000000-0000-4000-8000-000000000001"
SnapshotDescription="Fresh 24.04 install, tools only"
SnapshotName-1="dev-box"
SnapshotUUID-1="a1000000-0000-4000-8000-000000000002"
SnapshotDescription-1="docker, go, dotfiles"
SnapshotName-1-1="broken-upgrade"
SnapshotUUID-1-1="a1000000-0000-4000-8000-000000000003"
CurrentSnapshotName="broken-upgrade"
CurrentSnapshotUUID="a1000000-0000-4000-8000-000000000003"
CurrentSnapshotNode="SnapshotName-1-1"
`

func TestParseSnapshotListReadsTreeOrderAndCurrent(t *testing.T) {
	tree, err := ParseSnapshotList(machineReadableTree)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Snapshots) != 3 {
		t.Fatalf("three nodes expected: %+v", tree.Snapshots)
	}
	if tree.Snapshots[0].Name != "clean-install" || tree.Snapshots[0].Depth != 0 {
		t.Errorf("root: %+v", tree.Snapshots[0])
	}
	if tree.Snapshots[1].Name != "dev-box" || tree.Snapshots[1].Depth != 1 {
		t.Errorf("child: %+v", tree.Snapshots[1])
	}
	if tree.Snapshots[2].Name != "broken-upgrade" || tree.Snapshots[2].Depth != 2 || !tree.Snapshots[2].Current {
		t.Errorf("current grandchild: %+v", tree.Snapshots[2])
	}
	if tree.CurrentUUID != "a1000000-0000-4000-8000-000000000003" {
		t.Errorf("current uuid: %q", tree.CurrentUUID)
	}
	if got := tree.Snapshots[0].Description; got != "Fresh 24.04 install, tools only" {
		t.Errorf("description: %q", got)
	}
}

func TestParseSnapshotListNoSnapshotsIsAnEmptyTree(t *testing.T) {
	tree, err := ParseSnapshotList("Oracle VM VirtualBox Manager Edition 7.2.0\n" +
		"ERROR: The VM \"lab\" does not have any snapshots\n")
	if err != nil {
		t.Fatalf("the no-snapshots message must not be an error: %v", err)
	}
	if !tree.Empty() {
		t.Fatalf("empty tree expected: %+v", tree.Snapshots)
	}
}

func TestParseSnapshotListKeepsDuplicateNamesApart(t *testing.T) {
	out := `SnapshotName="base"
SnapshotUUID="b0000000-0000-4000-8000-000000000001"
SnapshotName-1="base"
SnapshotUUID-1="b0000000-0000-4000-8000-000000000002"
CurrentSnapshotNode="SnapshotName-1"
`
	tree, err := ParseSnapshotList(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Snapshots) != 2 {
		t.Fatalf("two nodes expected: %+v", tree.Snapshots)
	}
	// A repeated name cannot resolve to one node: only UUIDs may address it.
	if _, err := tree.Resolve("base"); err == nil {
		t.Fatal("an ambiguous name must never resolve")
	} else if !strings.Contains(err.Error(), "UUID") {
		t.Errorf("the ambiguity error must name UUIDs as the way out: %v", err)
	}
	if snap, err := tree.Resolve("b0000000-0000-4000-8000-000000000002"); err != nil || snap.Name != "base" {
		t.Errorf("resolve by uuid: %+v %v", snap, err)
	}
}

// ---------- snapshot verbs ----------

// snapshotRunner answers different per-argument fixtures in one session: a
// snapshot take first asks which machines run, so one canned response cannot
// serve both calls.
type snapshotRunner struct {
	mu      sync.Mutex
	calls   [][]string
	answers map[string]fixtures // keyed by the first argument of interest
}

func (r *snapshotRunner) Command(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string{name}, args...))
	f := r.lookupLocked(args)
	r.mu.Unlock()
	return helper(ctx, f), nil
}

func (r *snapshotRunner) Interactive(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
	return r.Command(ctx, name, args...)
}

func (r *snapshotRunner) lookupLocked(args []string) fixtures {
	joined := strings.Join(args, " ")
	for key, f := range r.answers {
		if strings.Contains(joined, key) {
			return f
		}
	}
	return fixtures{}
}

func (r *snapshotRunner) recorded() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.calls))
	for i, c := range r.calls {
		out[i] = append([]string(nil), c...)
	}
	return out
}

func newCLIWithRouting(t *testing.T, answers map[string]fixtures) (*CLI, *snapshotRunner) {
	t.Helper()
	r := &snapshotRunner{answers: answers}
	c := NewCLI(`C:\Program Files\Oracle\VirtualBox\VBoxManage.exe`)
	c.SetRunner(r)
	return c, r
}

func TestTakeSnapshotOnOffMachineOmitsLive(t *testing.T) {
	const uuid = "2d620512-4b10-45a8-b070-25435e5106a5"
	cli, r := newCLIWithRouting(t, map[string]fixtures{
		"list runningvms": {}, // empty output: nothing runs
		"snapshot":        {},
	})
	if err := cli.TakeSnapshot(context.Background(), uuid, "before-upgrade", "keep this point"); err != nil {
		t.Fatal(err)
	}
	var take []string
	for _, call := range r.recorded() {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "take") {
			take = call
			break
		}
		if strings.Contains(joined, "--live") {
			t.Fatalf("--live appeared outside the take call: %v", call)
		}
	}
	if take == nil {
		t.Fatalf("take never ran: %v", r.recorded())
	}
	want := []string{cli.Path(), "--nologo", "snapshot", uuid, "take", "before-upgrade", "--description=keep this point"}
	if strings.Join(take, "|") != strings.Join(want, "|") {
		t.Errorf("args = %v\nwant %v", take, want)
	}
}

func TestTakeSnapshotOnRunningMachineUsesLive(t *testing.T) {
	const uuid = "2d620512-4b10-45a8-b070-25435e5106a5"
	running := writeFixture(t, "running.txt", `"Kali Linux" {2d620512-4b10-45a8-b070-25435e5106a5}\n`)
	cli, r := newCLIWithRouting(t, map[string]fixtures{
		"list runningvms": {stdoutFile: running},
		"snapshot":        {},
	})
	if err := cli.TakeSnapshot(context.Background(), uuid, "now", ""); err != nil {
		t.Fatal(err)
	}
	var take []string
	for _, call := range r.recorded() {
		if strings.Contains(strings.Join(call, " "), "take") {
			take = call
		}
	}
	if take == nil {
		t.Fatalf("take never ran: %v", r.recorded())
	}
	joined := strings.Join(take, " ")
	if !strings.Contains(joined, "--live") {
		t.Errorf("a running machine takes a live snapshot: %v", take)
	}
	if strings.Contains(joined, "--description") {
		t.Errorf("an empty description must not be sent: %v", take)
	}
}

func TestTakeSnapshotRefusesUnsafeNamesBeforeAnyCall(t *testing.T) {
	cli, r := newCLIWithRouting(t, map[string]fixtures{})
	const uuid = "2d620512-4b10-45a8-b070-25435e5106a5"
	if err := cli.TakeSnapshot(context.Background(), uuid, "-rf", ""); err == nil {
		t.Fatal("a leading-dash name must be refused")
	}
	if len(r.recorded()) != 0 {
		t.Fatalf("nothing may run before the name passes: %v", r.recorded())
	}
}

func TestRestoreSnapshotRequiresUUID(t *testing.T) {
	cli, r := newCLIWithRouting(t, map[string]fixtures{})
	const uuid = "2d620512-4b10-45a8-b070-25435e5106a5"
	if err := cli.RestoreSnapshot(context.Background(), uuid, "clean-install"); err == nil {
		t.Fatal("restoring by NAME must be refused: names can repeat in a tree")
	}
	if len(r.recorded()) != 0 {
		t.Fatalf("refusal sends nothing: %v", r.recorded())
	}
	if err := cli.RestoreSnapshot(context.Background(), uuid, "a1000000-0000-4000-8000-000000000001"); err != nil {
		t.Fatal(err)
	}
	calls := r.recorded()
	if len(calls) != 1 {
		t.Fatalf("one call expected: %v", calls)
	}
	want := []string{cli.Path(), "--nologo", "snapshot", uuid, "restore", "a1000000-0000-4000-8000-000000000001"}
	if strings.Join(calls[0], "|") != strings.Join(want, "|") {
		t.Errorf("args = %v\nwant %v", calls[0], want)
	}
}

func TestListSnapshotsMapsErrorMessageToEmptyTree(t *testing.T) {
	errOut := writeFixture(t, "err.txt", "ERROR: The VM \"lab\" does not have any snapshots\nRC: 1\n")
	cli, _ := newCLIWithRouting(t, map[string]fixtures{
		"snapshot": {stderrFile: errOut, code: 1},
	})
	tree, err := cli.ListSnapshots(context.Background(), "2d620512-4b10-45a8-b070-25435e5106a5")
	if err != nil {
		t.Fatalf("the documented 'no snapshots' message is the empty tree, not an error: %v", err)
	}
	if !tree.Empty() {
		t.Fatalf("empty tree expected: %+v", tree.Snapshots)
	}
}
