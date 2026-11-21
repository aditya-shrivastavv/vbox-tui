// Package demo provides a VirtualBox-shaped backend with fake machines so the
// interface can be reviewed without a hypervisor. It never runs VBoxManage,
// never runs ssh, never opens a network connection, and never touches the
// user's real configuration file.
package demo

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aditya-shrivastavv/vbox-tui/internal/config"
	"github.com/aditya-shrivastavv/vbox-tui/internal/vbox"
)

// Delay defaults for simulated transitions. They are long enough that the UI has
// a chance to show a transitional state, short enough to review quickly.
const (
	StartDelay    = 3 * time.Second
	ResumeDelay   = 1 * time.Second
	SaveDelay     = 2 * time.Second
	ShutdownDelay = 6 * time.Second
)

// VMSeed describes one fake machine.
type VMSeed struct {
	Name     string
	UUID     string
	State    vbox.StateInfo
	MemoryMB int
	CPUs     int
	GuestOS  string
	NICs     []vbox.NIC
	Forwards []vbox.Forward
	SSH      config.SSHConfig
	// Guest seeds the guest-control settings (OS family and restart route).
	Guest config.GuestConfig
	// LocalhostReachable mirrors VBoxManage's nat-localhostreachable setting.
	LocalhostReachable bool
	// ShutdownDelay overrides the simulated guest shutdown time.
	ShutdownDelay time.Duration
	// Unreadable makes the details request fail, exercising the error surface.
	Unreadable bool
}

// Backend is an in-memory vbox.Manager. A mutex guards everything because the
// TUI can refresh while an action is in flight.
type Backend struct {
	mu       sync.Mutex
	machines []*entry
	version  string
	clock    func() time.Time
	// sshOverrides is demo-only, in-memory SSH storage. It is never persisted.
	sshOverrides map[string]config.SSHConfig
	// guestOverrides mirrors sshOverrides for the guest-control settings.
	guestOverrides map[string]config.GuestConfig
}

type entry struct {
	seed     VMSeed
	current  vbox.StateInfo
	since    time.Time
	pending  *pendingOp
	shutdown bool // a graceful shutdown was requested but not yet completed
	reboot   bool // a guest reboot was requested but not yet observed
	snap     *snapshotState
}

type pendingOp struct {
	kind  string
	until time.Time
}

// snapshotState owns one machine's fake snapshot tree. New snapshots attach
// under the root, which is all the demo needs to exercise the tree UI.
type snapshotState struct {
	snapshots []vbox.Snapshot
	current   string // UUID of the node the machine currently derives from
}

// New returns a backend loaded with realistic fake machines.
func New() *Backend {
	now := time.Now()
	b := &Backend{
		version:        "VirtualBox 7.2.20 (demo data)",
		clock:          func() time.Time { return now },
		sshOverrides:   map[string]config.SSHConfig{},
		guestOverrides: map[string]config.GuestConfig{},
	}
	for _, seed := range Seeds() {
		b.machines = append(b.machines, &entry{seed: seed, current: seed.State, since: now.Add(-2 * time.Hour)})
	}
	// Seed one realistic tree so the snapshot flows have content to show:
	// ubuntu has a small branch, kali is mid-flight on a nested snapshot.
	b.SeedSnapshots("c907c20b-5a41-4dc2-99c5-0e9bc7773c38", []vbox.Snapshot{
		{Name: "clean-install", UUID: "a1000000-0000-4000-8000-000000000001", Node: "", Depth: 0, Description: "Fresh 24.04 install, tools only"},
		{Name: "dev-box", UUID: "a1000000-0000-4000-8000-000000000002", Node: "-1", Depth: 1, Description: "docker, go, dotfiles"},
		{Name: "broken-upgrade", UUID: "a1000000-0000-4000-8000-000000000003", Node: "-1-1", Depth: 2, Description: "apt full-upgrade that broke X"},
	}, "a1000000-0000-4000-8000-000000000002")
	b.SeedSnapshots("2d620512-4b10-45a8-b070-25435e5106a5", []vbox.Snapshot{
		{Name: "2024.1 base", UUID: "b2000000-0000-4000-8000-000000000001", Node: "", Depth: 0},
		{Name: "2024.1 base", UUID: "b2000000-0000-4000-8000-000000000002", Node: "-1", Depth: 1, Description: "same name twice: identity stays the UUID"},
	}, "b2000000-0000-4000-8000-000000000002")
	return b
}

// Seeds returns the demo machine set. The names deliberately include a space, an
// em dash, a non-ASCII letter and embedded quotes so the UI is exercised with
// awkward input.
func Seeds() []VMSeed {
	return []VMSeed{
		{
			Name:     "kali",
			UUID:     "2d620512-4b10-45a8-b070-25435e5106a5",
			State:    vbox.StateInfo{State: vbox.StatePoweredOff, Kind: vbox.KindOff, Raw: "powered off"},
			MemoryMB: 8192,
			CPUs:     4,
			GuestOS:  "Debian (64-bit)",
			NICs: []vbox.NIC{
				{Slot: 1, Type: "nat", MAC: "0800272CFDDD", Attached: true},
				{Slot: 2, Type: "hostonly", Device: "VirtualBox Host-Only Ethernet Adapter", MAC: "08002707C886", Attached: true},
			},
			Forwards: []vbox.Forward{
				{NIC: 1, Name: "ssh", Proto: "tcp", HostIP: "127.0.0.1", HostPort: 2222, GuestPort: 22},
			},
			LocalhostReachable: true,
			SSH:                config.SSHConfig{Mode: config.SSHModeEndpoint, Host: "127.0.0.1", Port: 2222, User: "kali"},
			Guest:              config.GuestConfig{OS: config.GuestOSLinux, Restart: config.GuestRestartGA},
		},
		{
			Name:     "ubuntu",
			UUID:     "c907c20b-5a41-4dc2-99c5-0e9bc7773c38",
			State:    vbox.StateInfo{State: vbox.StateRunning, Kind: vbox.KindRunning, Raw: "running"},
			MemoryMB: 6144,
			CPUs:     4,
			GuestOS:  "Ubuntu (64-bit)",
			NICs: []vbox.NIC{
				{Slot: 1, Type: "nat", MAC: "080027B2E1F0", Attached: true},
				{Slot: 2, Type: "bridged", Device: "Intel(R) Wi-Fi 6E AX211", MAC: "08002711AA22", Attached: true},
			},
			SSH:           config.SSHConfig{Mode: config.SSHModeAlias, Alias: "lab-ubuntu"},
			Guest:         config.GuestConfig{OS: config.GuestOSLinux},
			ShutdownDelay: 5 * time.Second,
		},
		{
			Name:     "Workbench — Win11",
			UUID:     "f421275d-fe89-4b28-b68b-7663ee521a5f",
			State:    vbox.StateInfo{State: vbox.StateSaved, Kind: vbox.KindSaved, Raw: "saved"},
			MemoryMB: 4096,
			CPUs:     2,
			GuestOS:  "Windows 11 (64-bit)",
			NICs:     []vbox.NIC{{Slot: 1, Type: "nat", MAC: "080027A1B2C3", Attached: true}},
		},
		{
			Name:     "Ω-legacy ñame \"quoted\"",
			UUID:     "7c3f9a11-0d45-4b3c-8f58-1a2b3c4d5e6f",
			State:    vbox.StateInfo{State: vbox.StatePaused, Kind: vbox.KindPaused, Raw: "paused"},
			MemoryMB: 2048,
			CPUs:     1,
			GuestOS:  "Other Linux (64-bit)",
			NICs:     []vbox.NIC{{Slot: 1, Type: "natnetwork", Device: "NatNetwork", MAC: "080027DEADBEEF", Attached: true}},
			Forwards: []vbox.Forward{
				{NIC: 1, Name: "http", Proto: "tcp", HostPort: 8080, GuestIP: "10.0.2.15", GuestPort: 80},
				{NIC: 1, Name: "ssh", Proto: "tcp", HostPort: 2200, GuestIP: "10.0.2.15", GuestPort: 22},
			},
		},
		{
			Name:       "broken-vm",
			UUID:       "00000000-0000-0000-0000-00000000dead",
			State:      vbox.StateInfo{State: vbox.StateInaccessible, Kind: vbox.KindUnavailable, Raw: "inaccessible"},
			GuestOS:    "unknown",
			Unreadable: true,
		},
	}
}

// Version identifies the backend.
func (b *Backend) Version(context.Context) (string, error) { return b.version, nil }

// BackendName labels the header, which is where the DEMO marker comes from.
func (b *Backend) BackendName() string { return "DEMO" }

// List returns the fake inventory after applying scheduled transitions.
func (b *Backend) List(ctx context.Context) (vbox.Inventory, error) {
	if err := ctx.Err(); err != nil {
		return vbox.Inventory{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reconcileLocked()
	return b.snapshotLocked(), nil
}

// Detail returns fake configuration, or an error for the unreadable machine.
func (b *Backend) Detail(ctx context.Context, uuid string) (vbox.Machine, error) {
	if err := ctx.Err(); err != nil {
		return vbox.Machine{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reconcileLocked()
	m := b.findLocked(uuid)
	if m == nil {
		return vbox.Machine{}, fmt.Errorf("no machine with UUID %s; it may have been unregistered elsewhere", uuid)
	}
	view := b.viewLocked(m)
	if m.seed.Unreadable {
		return view, fmt.Errorf("VirtualBox cannot read the settings file for %q: the machine is inaccessible", m.seed.Name)
	}
	return view, nil
}

// PowerOn starts a powered-off machine or restores a saved one, entering a short
// transitional window first, which is what a real start looks like from outside.
func (b *Backend) PowerOn(ctx context.Context, uuid string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reconcileLocked()
	m := b.findLocked(uuid)
	if m == nil {
		return fmt.Errorf("no machine with UUID %s", uuid)
	}
	switch m.current.Kind {
	case vbox.KindOff, vbox.KindAborted:
		b.beginLocked(m, "starting", "running", StartDelay)
		return nil
	case vbox.KindSaved:
		b.beginLocked(m, "restoring", "running", StartDelay)
		return nil
	case vbox.KindRunning, vbox.KindPaused:
		// Refusing matches VirtualBox, which will not start a running machine.
		return fmt.Errorf("%q is already %s", m.seed.Name, m.current.State)
	default:
		return fmt.Errorf("%q is %s and cannot be started right now", m.seed.Name, m.current.State)
	}
}

// Resume un-pauses a paused machine.
func (b *Backend) Resume(ctx context.Context, uuid string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reconcileLocked()
	m := b.findLocked(uuid)
	if m == nil {
		return fmt.Errorf("no machine with UUID %s", uuid)
	}
	if m.current.Kind != vbox.KindPaused {
		return fmt.Errorf("%q is %s; only a paused machine can be resumed", m.seed.Name, m.current.State)
	}
	b.beginLocked(m, "resuming", "running", ResumeDelay)
	return nil
}

// Pause freezes a running machine. Like the hypervisor, the demo applies it
// immediately: the machine reports "paused" from the next read onward.
func (b *Backend) Pause(ctx context.Context, uuid string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reconcileLocked()
	m := b.findLocked(uuid)
	if m == nil {
		return fmt.Errorf("no machine with UUID %s", uuid)
	}
	if m.current.Kind != vbox.KindRunning {
		return fmt.Errorf("%q is %s; only a running machine can be paused", m.seed.Name, m.current.State)
	}
	m.current = vbox.StateInfo{State: vbox.StatePaused, Kind: vbox.KindPaused, Raw: "paused"}
	m.since = b.clock()
	return nil
}

// ShutdownGracefully records the request and keeps the machine running
// afterwards, because that is exactly what a real guest does: VBoxManage
// returns immediately and the guest decides when it is off.
func (b *Backend) ShutdownGracefully(ctx context.Context, uuid string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reconcileLocked()
	m := b.findLocked(uuid)
	if m == nil {
		return fmt.Errorf("no machine with UUID %s", uuid)
	}
	switch m.current.Kind {
	case vbox.KindRunning, vbox.KindPaused:
		m.shutdown = true
		delay := m.seed.ShutdownDelay
		if delay <= 0 {
			delay = ShutdownDelay
		}
		b.scheduleLocked(m, "shutdown", delay)
		return nil
	default:
		return fmt.Errorf("%q is %s; a graceful shutdown needs a running machine", m.seed.Name, m.current.State)
	}
}

// SaveState suspends a running machine. This is not a shutdown: the demo entry
// ends up saved, not off, and the note says so.
func (b *Backend) SaveState(ctx context.Context, uuid string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reconcileLocked()
	m := b.findLocked(uuid)
	if m == nil {
		return fmt.Errorf("no machine with UUID %s", uuid)
	}
	if m.current.Kind != vbox.KindRunning {
		return fmt.Errorf("%q is %s; only a running machine can be saved", m.seed.Name, m.current.State)
	}
	m.shutdown = false
	b.beginLocked(m, "saving", "saved", SaveDelay)
	return nil
}

// RequestedShutdown reports whether a graceful shutdown is still outstanding.
// The UI uses it to keep showing "shutdown requested" instead of implying the
// guest already stopped.
func (b *Backend) RequestedShutdown(uuid string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	m := b.findLocked(uuid)
	return m != nil && m.shutdown
}

// GuestReboot simulates `VBoxManage controlvm reboot`: the request only goes
// through for a live machine, the machine stays running while the guest
// cycles, and the demo never power-cycles anything behind the scenes.
func (b *Backend) GuestReboot(ctx context.Context, uuid string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reconcileLocked()
	m := b.findLocked(uuid)
	if m == nil {
		return fmt.Errorf("no machine with UUID %s", uuid)
	}
	switch m.current.Kind {
	case vbox.KindRunning:
		m.reboot = true
		b.scheduleLocked(m, "reboot", ShutdownDelay)
		return nil
	default:
		return fmt.Errorf("%q is %s; a guest reboot needs a running machine", m.seed.Name, m.current.State)
	}
}

// RequestedReboot reports whether a guest reboot request is still awaiting
// its (simulated) completion.
func (b *Backend) RequestedReboot(uuid string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	m := b.findLocked(uuid)
	return m != nil && m.reboot
}

// ListSnapshots returns the fake tree; the no-snapshots machines read as the
// empty tree, exactly like the real backend.
func (b *Backend) ListSnapshots(ctx context.Context, uuid string) (vbox.SnapshotTree, error) {
	if err := ctx.Err(); err != nil {
		return vbox.SnapshotTree{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	m := b.findLocked(uuid)
	if m == nil {
		return vbox.SnapshotTree{}, fmt.Errorf("no machine with UUID %s", uuid)
	}
	if m.snap == nil {
		return vbox.SnapshotTree{}, nil
	}
	return b.treeLocked(m), nil
}

// TakeSnapshot appends a child under the current node (VirtualBox attaches a
// new snapshot to whichever node is current) and makes it current. It refuses
// a name the machine already uses and a transitional machine, both of which
// the real hypervisor refuses too.
func (b *Backend) TakeSnapshot(ctx context.Context, uuid, name, description string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("a snapshot needs a name")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reconcileLocked()
	m := b.findLocked(uuid)
	if m == nil {
		return fmt.Errorf("no machine with UUID %s", uuid)
	}
	if m.current.Kind == vbox.KindTransitional {
		return fmt.Errorf("%q is %s; wait for it to settle before taking a snapshot", m.seed.Name, m.current.State)
	}
	if m.snap == nil {
		m.snap = &snapshotState{}
	}
	for _, s := range m.snap.snapshots {
		if s.Name == name {
			return fmt.Errorf("%q already has a snapshot named %q", m.seed.Name, name)
		}
	}
	// A new snapshot attaches to whichever node is current. The very first
	// snapshot on an empty machine becomes the tree's root itself, which is
	// node "" — no leading number — exactly like VirtualBox.
	node := ""
	if len(m.snap.snapshots) > 0 {
		childNo := 1
		if cur, ok := findSnap(m.snap.snapshots, m.snap.current); ok {
			childNo = 1 + directChildrenOf(m.snap.snapshots, cur.Node)
			node = cur.Node + fmt.Sprintf("-%d", childNo)
		} else {
			node = fmt.Sprintf("-%d", len(m.snap.snapshots)+1)
		}
	}
	snap := vbox.Snapshot{
		Name:        name,
		UUID:        b.nextSnapshotUUID(),
		Description: description,
		Node:        node,
		Depth:       strings.Count(node, "-"),
	}
	m.snap.snapshots = append(m.snap.snapshots, snap)
	m.snap.current = snap.UUID
	return nil
}

// RenameVM changes a fake machine's name. Like VirtualBox, the demo honours
// a rename only while the machine is powered off, refuses a name another
// machine carries, and never stops or moves anything to make the rename
// work. In-memory only: no files exist in the demo, which is also the point.
func (b *Backend) RenameVM(ctx context.Context, uuid, newName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := vbox.ValidateVMName(newName); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reconcileLocked()
	m := b.findLocked(uuid)
	if m == nil {
		return fmt.Errorf("no machine with UUID %s", uuid)
	}
	switch m.current.Kind {
	case vbox.KindOff, vbox.KindAborted:
	default:
		return fmt.Errorf("%q is %s; VirtualBox renames only a powered-off machine", m.seed.Name, m.current.State)
	}
	key := config.CanonicalUUID(uuid)
	for _, other := range b.machines {
		if config.CanonicalUUID(other.seed.UUID) != key && other.seed.Name == newName {
			return fmt.Errorf("a machine named %q is already registered", newName)
		}
	}
	m.seed.Name = newName
	return nil
}

// RestoreSnapshot moves the machine back to the addressed snapshot. Like
// VirtualBox, the demo refuses while the machine is not powered off, and it
// only accepts the snapshot's UUID.
func (b *Backend) RestoreSnapshot(ctx context.Context, uuid, snapshotUUID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reconcileLocked()
	m := b.findLocked(uuid)
	if m == nil {
		return fmt.Errorf("no machine with UUID %s", uuid)
	}
	if m.snap == nil {
		return fmt.Errorf("%q has no snapshots", m.seed.Name)
	}
	snap, ok := findSnap(m.snap.snapshots, snapshotUUID)
	if !ok {
		return fmt.Errorf("no snapshot with UUID %s on %q", snapshotUUID, m.seed.Name)
	}
	switch m.current.Kind {
	case vbox.KindOff, vbox.KindAborted:
	default:
		// VirtualBox itself refuses a restore unless the machine is powered
		// off (a saved state or a live session would be discarded); the app
		// gate never sends anything else, and the fake must not be more
		// permissive than the hypervisor it imitates.
		return fmt.Errorf("%q is %s; a snapshot restore needs the machine powered off", m.seed.Name, m.current.State)
	}
	// Restoring discards every snapshot taken after this point along its
	// branch, the way VirtualBox prunes the tree. Nodes on other branches
	// survive.
	m.snap.snapshots = pruneAfter(m.snap.snapshots, snap.Node)
	m.snap.current = snap.UUID
	return nil
}

// SeedSnapshots replaces one machine's fake tree; tests and the demo seeds
// use it to set up interesting starting points.
func (b *Backend) SeedSnapshots(uuid string, snaps []vbox.Snapshot, current string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if m := b.findLocked(uuid); m != nil {
		if len(snaps) == 0 {
			m.snap = nil
			return
		}
		m.snap = &snapshotState{snapshots: append([]vbox.Snapshot(nil), snaps...), current: current}
	}
}

// Snapshots is a test view of one machine's current tree.
func (b *Backend) Snapshots(uuid string) vbox.SnapshotTree {
	b.mu.Lock()
	defer b.mu.Unlock()
	m := b.findLocked(uuid)
	if m == nil {
		return vbox.SnapshotTree{}
	}
	return b.treeLocked(m)
}

func (b *Backend) treeLocked(m *entry) vbox.SnapshotTree {
	if m.snap == nil {
		return vbox.SnapshotTree{}
	}
	out := vbox.SnapshotTree{
		Snapshots: append([]vbox.Snapshot(nil), m.snap.snapshots...),
	}
	for i := range out.Snapshots {
		out.Snapshots[i].Current = config.CanonicalUUID(out.Snapshots[i].UUID) == config.CanonicalUUID(m.snap.current)
		if out.Snapshots[i].Current {
			out.CurrentUUID = config.CanonicalUUID(out.Snapshots[i].UUID)
		}
	}
	return out
}

func findSnap(snaps []vbox.Snapshot, uuid string) (vbox.Snapshot, bool) {
	want := config.CanonicalUUID(uuid)
	if want == "" {
		return vbox.Snapshot{}, false
	}
	for _, s := range snaps {
		if config.CanonicalUUID(s.UUID) == want {
			return s, true
		}
	}
	return vbox.Snapshot{}, false
}

// directChildrenOf counts the snapshots one level below node. The root's node
// string is "", so a child of the root is any single-number node like "-2";
// children of "-1" look like "-1-3". Deeper descendants do not count.
func directChildrenOf(snaps []vbox.Snapshot, node string) int {
	count := 0
	for _, s := range snaps {
		if !strings.HasPrefix(s.Node, node+"-") {
			continue
		}
		if !strings.Contains(strings.TrimPrefix(s.Node, node+"-"), "-") {
			count++
		}
	}
	return count
}

// pruneAfter drops everything strictly below restored in its subtree, and
// sibling branches taken along the path back to the root: restoring means
// those later points never happened.
func pruneAfter(snaps []vbox.Snapshot, node string) []vbox.Snapshot {
	keep := make([]vbox.Snapshot, 0, len(snaps))
	for _, s := range snaps {
		if s.Node == node || strings.HasPrefix(node, s.Node+"-") {
			keep = append(keep, s)
		}
	}
	return keep
}

var demoSnapshotSeq int

func (b *Backend) nextSnapshotUUID() string {
	demoSnapshotSeq++
	return fmt.Sprintf("5d000000-0000-4000-8000-%012d", demoSnapshotSeq)
}

// SetSSH stores SSH settings for a demo VM in memory only, never on disk.
func (b *Backend) SetSSH(uuid string, ssh config.SSHConfig) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sshOverrides[config.CanonicalUUID(uuid)] = ssh.Normalise()
}

// ClearSSH removes demo SSH settings in memory by storing the zero value, the
// same convention SetSSH uses: a stored zero reads as unconfigured.
func (b *Backend) ClearSSH(uuid string) { b.SetSSH(uuid, config.SSHConfig{}) }

// ClearGuest removes demo guest-control settings the same way: the zero
// config reads as unconfigured and shadows any seeded value, exactly like the
// real config store after a reset.
func (b *Backend) ClearGuest(uuid string) { b.SetGuest(uuid, config.GuestConfig{}) }

// SSH returns the demo VM's SSH settings, preferring anything set at runtime.
func (b *Backend) SSH(uuid string) (config.SSHConfig, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := config.CanonicalUUID(uuid)
	if s, ok := b.sshOverrides[key]; ok {
		// A cleared endpoint (the zero value stored by the form's Clear action)
		// must read as unconfigured, the same way the real config store behaves.
		return s, s.Configured()
	}
	for _, m := range b.machines {
		if config.CanonicalUUID(m.seed.UUID) == key {
			return m.seed.SSH, m.seed.SSH.Configured()
		}
	}
	return config.SSHConfig{}, false
}

// SetGuest stores guest-control settings for a demo VM in memory only.
func (b *Backend) SetGuest(uuid string, guest config.GuestConfig) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.guestOverrides[config.CanonicalUUID(uuid)] = guest
}

// Guest returns the demo VM's guest-control settings, preferring anything set
// at runtime. The second result says whether anything is configured at all.
func (b *Backend) Guest(uuid string) (config.GuestConfig, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := config.CanonicalUUID(uuid)
	if g, ok := b.guestOverrides[key]; ok {
		return g, g.OS != config.GuestOSUnknown || g.Restart != config.GuestRestartAuto
	}
	for _, m := range b.machines {
		if config.CanonicalUUID(m.seed.UUID) == key {
			g := m.seed.Guest
			return g, g.OS != config.GuestOSUnknown || g.Restart != config.GuestRestartAuto
		}
	}
	return config.GuestConfig{}, false
}

// Advance moves the fake clock forward, applying scheduled transitions. Tests
// use this instead of sleeping.
func (b *Backend) Advance(d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	base := b.clock()
	now := base.Add(d)
	b.clock = func() time.Time { return now }
	b.reconcileLocked()
}

// ForceState is a test hook for wiring up a specific starting point.
func (b *Backend) ForceState(uuid string, st vbox.StateInfo) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if m := b.findLocked(uuid); m != nil {
		m.current = st
		m.since = b.clock()
		m.pending = nil
		m.shutdown = false
		m.reboot = false
	}
}

func (b *Backend) beginLocked(m *entry, rawNow, rawAfter string, delay time.Duration) {
	m.current = vbox.StateInfo{State: vbox.StateTransitioning, Kind: vbox.KindTransitional, Raw: rawNow}
	m.since = b.clock()
	b.scheduleLocked(m, rawAfter, delay)
}

func (b *Backend) scheduleLocked(m *entry, kind string, delay time.Duration) {
	m.pending = &pendingOp{kind: kind, until: b.clock().Add(delay)}
}

func (b *Backend) reconcileLocked() {
	now := b.clock()
	for _, m := range b.machines {
		if m.pending == nil || now.Before(m.pending.until) {
			continue
		}
		if m.pending.kind == "reboot" {
			// The guest finished cycling: from the hypervisor's view the
			// machine stayed running the whole time, the request simply
			// stops being outstanding.
			m.reboot = false
			m.pending = nil
			continue
		}
		st := stateForKind(m.pending.kind)
		m.current = st
		m.since = now
		m.pending = nil
		if st.Kind == vbox.KindOff || st.Kind == vbox.KindSaved {
			m.shutdown = false
		}
	}
}

func stateForKind(kind string) vbox.StateInfo {
	switch kind {
	case "running":
		return vbox.StateInfo{State: vbox.StateRunning, Kind: vbox.KindRunning, Raw: "running"}
	case "saved":
		return vbox.StateInfo{State: vbox.StateSaved, Kind: vbox.KindSaved, Raw: "saved"}
	case "shutdown", "powered off":
		return vbox.StateInfo{State: vbox.StatePoweredOff, Kind: vbox.KindOff, Raw: "powered off"}
	}
	return vbox.StateInfo{State: vbox.StateUnknown, Kind: vbox.KindUnknown, Raw: kind}
}

func (b *Backend) findLocked(uuid string) *entry {
	key := config.CanonicalUUID(uuid)
	for _, m := range b.machines {
		if config.CanonicalUUID(m.seed.UUID) == key {
			return m
		}
	}
	return nil
}

func (b *Backend) snapshotLocked() vbox.Inventory {
	var inv vbox.Inventory
	for _, m := range b.machines {
		inv.Machines = append(inv.Machines, b.viewLocked(m))
	}
	sort.SliceStable(inv.Machines, func(i, j int) bool {
		return strings.ToLower(inv.Machines[i].Name) < strings.ToLower(inv.Machines[j].Name)
	})
	return inv
}

func (b *Backend) viewLocked(m *entry) vbox.Machine {
	machine := vbox.Machine{
		Name:            m.seed.Name,
		UUID:            m.seed.UUID,
		State:           m.current,
		RawState:        m.current.Raw,
		StateChanged:    m.since,
		MemoryMB:        m.seed.MemoryMB,
		CPUs:            m.seed.CPUs,
		GuestOS:         m.seed.GuestOS,
		NICs:            append([]vbox.NIC(nil), m.seed.NICs...),
		Forwards:        append([]vbox.Forward(nil), m.seed.Forwards...),
		HasNATLocalhost: m.seed.LocalhostReachable,
		NATLocalhost:    m.seed.LocalhostReachable,
	}
	if m.shutdown {
		machine.Notes = append(machine.Notes, "graceful shutdown requested; still waiting for the guest to stop")
	}
	if m.seed.Unreadable {
		machine.Notes = append(machine.Notes, "VirtualBox could not read this machine's settings file")
	}
	return machine
}

// Names returns the demo machine names, used by the isolation test.
func (b *Backend) Names() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.machines))
	for _, m := range b.machines {
		out = append(out, m.seed.Name)
	}
	return out
}

var _ vbox.Manager = (*Backend)(nil)
