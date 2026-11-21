package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/aditya-shrivastavv/vbox-tui/internal/config"
	"github.com/aditya-shrivastavv/vbox-tui/internal/ssh"
	"github.com/aditya-shrivastavv/vbox-tui/internal/vbox"
)

// The tests drive the model through the same message surface the Bubble Tea
// event loop uses: keys and command results go into Update, commands the model
// returns run on worker goroutines, and their results are drained back into
// Update — so generation retirement, chained steps and cancellation all run
// for real. Only the world outside the app (VirtualBox, the settings store,
// the network, the ssh binary) is faked.

// ---------- scripted session ----------

type session struct {
	t     *testing.T
	m     *Model
	res   chan tea.Msg // command results from worker goroutines
	frame string       // last View(), captured only on the test goroutine
}

func start(t *testing.T, m *Model) *session {
	t.Helper()
	s := &session{t: t, m: m, res: make(chan tea.Msg, 256)}
	s.spawn(m.Init())
	// Give the initial read a moment to land and settle the frame.
	s.drain(ms(300))
	s.shot()
	return s
}

// settleThen lets the run reach an outcome after a keypress path (esc/finish)
// that ends with Quit: it just waits for the model state without expecting a
// specific frame.
func (s *session) settleThen(timeout time.Duration) {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.m.outcomeSeenForTest() != OutcomeNone {
			s.shot()
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	s.shot()
	s.t.Fatalf("no outcome within %s (mode %v):\n%s", timeout, s.m.modeForTest(), s.strip())
}

func (s *session) spawn(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() { s.res <- cmd() }()
}

// drain keeps feeding results from the worker channel into Update until the
// channel settles quiet or the budget runs out. Tea's BatchMsg results are
// expanded here because Update itself never runs commands.
func (s *session) drain(budget time.Duration) {
	s.t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		active := false
	exhaust:
		for {
			select {
			case msg := <-s.res:
				active = true
				s.deliver(msg)
			default:
				break exhaust
			}
		}
		if active {
			continue // handlers may have queued follow-up work
		}
		select {
		case msg := <-s.res:
			s.deliver(msg)
		case <-time.After(3 * time.Millisecond):
			select {
			case msg := <-s.res:
				s.deliver(msg)
				continue
			default:
				return
			}
		}
	}
}

func (s *session) deliver(msg tea.Msg) {
	s.t.Helper()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			s.spawn(c)
		}
		return
	}
	_, cmd := s.m.Update(msg)
	s.spawn(cmd)
}

// ms is a drain budget helper: s.drain(ms(20)).
func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// settleTo drains until the prompt reaches the wanted step (or any outcome).
func (s *session) settleTo(want Mode, budget time.Duration) {
	s.t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if s.m.outcomeSeenForTest() != OutcomeNone {
			s.shot()
			return
		}
		if s.m.modeForTest() == want {
			s.shot()
			return
		}
		s.drain(ms(20))
	}
	s.shot()
	s.t.Fatalf("prompt never reached step %v (at %v, frame:\n%s)", want, s.m.modeForTest(), s.strip())
}

// settleOutcome drains until the run ends — commands with real delays (the
// spinner's 125 ms frames, the shortened readiness timeouts) all resolve well
// inside the budget.
func (s *session) settleOutcome(budget time.Duration) {
	s.t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if s.m.outcomeSeenForTest() != OutcomeNone {
			// let any late answer land too: it must not rewrite the outcome
			s.drain(ms(150))
			s.shot()
			return
		}
		s.drain(ms(30))
	}
	s.shot()
	s.t.Fatalf("run never ended (mode %v, frame:\n%s)", s.m.modeForTest(), s.strip())
}

// waitForFrame drains until View contains text (for "the wait line is up").
func (s *session) waitForFrame(text string, budget time.Duration) {
	s.t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if strings.Contains(s.strip(), text) {
			return
		}
		s.drain(ms(20))
		s.shot()
	}
	s.t.Fatalf("frame never showed %q (last frame:\n%s)", text, s.strip())
}

func (s *session) shot() { s.frame = s.m.View() }

func (s *session) strip() string { return ansi.Strip(s.frame) }

func (s *session) send(msg tea.Msg) {
	s.t.Helper()
	_, cmd := s.m.Update(msg)
	s.spawn(cmd)
	s.drain(ms(5))
	s.shot()
}

// keys maps the names the model's String() comparisons use to the real key
// messages a terminal would deliver.
func (s *session) key(name string) {
	s.t.Helper()
	var msg tea.KeyMsg
	switch name {
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	case "down":
		msg = tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		msg = tea.KeyMsg{Type: tea.KeyUp}
	case "backspace":
		msg = tea.KeyMsg{Type: tea.KeyBackspace}
	default:
		// a single printable rune: 'y', 'n', 'q', '1' …
		if len([]rune(name)) != 1 {
			s.t.Fatalf("harness: unsupported key name %q", name)
		}
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(name)}
	}
	s.send(msg)
}

// runes types a whole string one rune at a time — filtering and fields take
// characters exactly as keys arrive.
func (s *session) runes(str string) {
	s.t.Helper()
	for _, r := range str {
		s.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}
func (s *session) enter() { s.key("enter") }
func (s *session) esc()   { s.key("esc") }
func (s *session) down()  { s.key("down") }
func (s *session) up()    { s.key("up") }

// typeLine types a value and confirms it — the way a person answers a field.
func (s *session) typeLine(text string) {
	s.t.Helper()
	s.runes(text)
	s.enter()
}

// outcome asserts the run ended with want and returns the result line.
func (s *session) outcome(want Outcome) string {
	s.t.Helper()
	if got := s.m.Outcome(); got != want {
		s.t.Fatalf("outcome = %v (%q), want %v", got, s.m.Summary(), want)
	}
	return s.m.Summary()
}

// ---------- fakes ----------

type opCall struct {
	op   string
	uuid string
}

type fakeVM struct {
	mu       sync.Mutex
	machines []vbox.Machine
	ops      []opCall
	listErr  error
	// gate blocks lifecycle calls while open-but-unfilled, so a test can press
	// esc while a request is genuinely in flight, then close it.
	gate    chan struct{}
	arrived chan struct{}
	once    sync.Once
	// snapshots per canonical UUID; rebootErr/guestReboot calls recorded too.
	snapshots map[string][]vbox.Snapshot
	snapCur   map[string]string
	rebootErr error
	renameErr error
	// onList, when set, sees every Inventory the fake is about to return,
	// with a call count: a test for "the state changed while the
	// confirmation was open" answers one later read differently — exactly
	// what another tool starting the machine does in the real world,
	// without teaching the model anything about test worlds.
	onList func(calls int, inv *vbox.Inventory)
	// afterTake runs at the end of a successful snaptake (with the lock
	// released): it stands in for what the hypervisor really did with the
	// protective snapshot — stored it, renamed it, or showed no node at
	// all despite the answer.
	afterTake func(uuid string)
	// snapErr, when set, makes ListSnapshots fail; snapErrFrom limits that
	// to reads from the Nth call on, so a test can fail exactly the
	// protective confirmation read and nothing before it.
	snapErr     error
	snapErrFrom int
	snapList    int
	// listCalls counts List calls for onList.
	listCalls int
}

func newFake(machines ...vbox.Machine) *fakeVM {
	return &fakeVM{machines: machines, snapshots: map[string][]vbox.Snapshot{}, snapCur: map[string]string{}}
}

func (f *fakeVM) record(op, uuid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, opCall{op: op, uuid: uuid})
}

func (f *fakeVM) opList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.ops))
	for _, o := range f.ops {
		out = append(out, o.op)
	}
	return out
}

// opCountForTest counts one operation: "exactly two starts" is a different
// promise than "at least one".
func (f *fakeVM) opCountForTest(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, o := range f.ops {
		if o.op == op {
			n++
		}
	}
	return n
}

func (f *fakeVM) has(op string) bool {
	for _, o := range f.opList() {
		if o == op {
			return true
		}
	}
	return false
}

// hasUUID proves an operation addressed exactly one machine: with two
// same-named VMs, the UUID is the only identity that matters.
func (f *fakeVM) hasUUID(op, uuid string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, o := range f.ops {
		if o.op == op && strings.EqualFold(o.uuid, uuid) {
			return true
		}
	}
	return false
}

func (f *fakeVM) List(context.Context) (vbox.Inventory, error) {
	f.record("list", "")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.listErr != nil {
		return vbox.Inventory{}, f.listErr
	}
	inv := vbox.Inventory{Machines: append([]vbox.Machine(nil), f.machines...)}
	if f.onList != nil {
		f.onList(f.listCalls, &inv)
	}
	return inv, nil
}

// onList lets one test shape a specific List answer ("the third read shows
// the machine running — the state gate must survive that"). nil in every
// ordinary test.
// set during a confirmation the machine another tool started, without
// teaching the model anything about test worlds.

func (f *fakeVM) Detail(_ context.Context, uuid string) (vbox.Machine, error) {
	f.record("detail", uuid)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.machines {
		if strings.EqualFold(m.UUID, uuid) {
			return m, nil
		}
	}
	return vbox.Machine{}, fmt.Errorf("uuid %s not found", uuid)
}

func (f *fakeVM) gateOp() {
	f.mu.Lock()
	gate := f.gate
	arrived := f.arrived
	f.mu.Unlock()
	if gate == nil {
		return
	}
	f.once.Do(func() {
		if arrived != nil {
			close(arrived)
		}
	})
	<-gate
}

func (f *fakeVM) transition(op, uuid string, st vbox.StateInfo) error {
	f.record(op, uuid)
	f.gateOp()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.machines {
		if strings.EqualFold(f.machines[i].UUID, uuid) {
			f.machines[i].State = st
		}
	}
	return nil
}

func (f *fakeVM) PowerOn(_ context.Context, uuid string) error {
	return f.transition("poweron", uuid, vbox.NormalizeState("running"))
}
func (f *fakeVM) Resume(_ context.Context, uuid string) error {
	return f.transition("resume", uuid, vbox.NormalizeState("running"))
}
func (f *fakeVM) Pause(_ context.Context, uuid string) error {
	return f.transition("pause", uuid, vbox.NormalizeState("paused"))
}
func (f *fakeVM) SaveState(_ context.Context, uuid string) error {
	return f.transition("savestate", uuid, vbox.NormalizeState("saved"))
}
func (f *fakeVM) ShutdownGracefully(_ context.Context, uuid string) error {
	// A delivered ACPI request leaves the guest running until it powers off
	// by itself; the fake never pretends the machine already stopped.
	f.record("acpipowerbutton", uuid)
	f.gateOp()
	return nil
}
func (f *fakeVM) Version(context.Context) (string, error) { return "7.1.x-fake", nil }

// GuestReboot records the request; rebootErr lets a test simulate VirtualBox
// refusing because the guest has no Additions channel.
func (f *fakeVM) GuestReboot(_ context.Context, uuid string) error {
	f.record("guestreboot", uuid)
	f.gateOp()
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rebootErr
}

// seedSnapshots gives one fake machine a snapshot tree.
func (f *fakeVM) seedSnapshots(uuid string, snaps []vbox.Snapshot, current string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := config.CanonicalUUID(uuid)
	f.snapshots[key] = append([]vbox.Snapshot(nil), snaps...)
	f.snapCur[key] = config.CanonicalUUID(current)
}

func (f *fakeVM) ListSnapshots(_ context.Context, uuid string) (vbox.SnapshotTree, error) {
	f.record("snaplist", uuid)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapList++
	if f.snapErr != nil && f.snapList >= f.snapErrFrom {
		return vbox.SnapshotTree{}, f.snapErr
	}
	key := config.CanonicalUUID(uuid)
	tree := vbox.SnapshotTree{}
	for _, s := range f.snapshots[key] {
		s.Current = config.CanonicalUUID(s.UUID) == f.snapCur[key]
		if s.Current {
			tree.CurrentUUID = config.CanonicalUUID(s.UUID)
		}
		tree.Snapshots = append(tree.Snapshots, s)
	}
	return tree, nil
}

func (f *fakeVM) TakeSnapshot(_ context.Context, uuid, name, description string) error {
	f.record("snaptake", uuid)
	err := f.takeLocked(uuid, name, description)
	if err != nil {
		return err
	}
	f.mu.Lock()
	hook := f.afterTake
	f.mu.Unlock()
	if hook != nil {
		hook(uuid) // what the hypervisor really left in the tree
	}
	return nil
}

func (f *fakeVM) takeLocked(uuid, name, description string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := config.CanonicalUUID(uuid)
	for _, s := range f.snapshots[key] {
		if s.Name == name {
			return fmt.Errorf("snapshot %q already exists", name)
		}
	}
	node := fmt.Sprintf("-%d", len(f.snapshots[key])+1)
	snap := vbox.Snapshot{
		Name:        name,
		UUID:        fmt.Sprintf("e0000000-0000-4000-8000-%012d", len(f.snapshots[key])+1),
		Node:        node,
		Depth:       strings.Count(node, "-"),
		Description: description,
	}
	f.snapshots[key] = append(f.snapshots[key], snap)
	f.snapCur[key] = config.CanonicalUUID(snap.UUID)
	return nil
}

func (f *fakeVM) RestoreSnapshot(_ context.Context, uuid, snapshotUUID string) error {
	f.record("snaprestore", uuid)
	f.mu.Lock()
	defer f.mu.Unlock()
	key := config.CanonicalUUID(uuid)
	for _, s := range f.snapshots[key] {
		if config.CanonicalUUID(s.UUID) == config.CanonicalUUID(snapshotUUID) {
			f.snapCur[key] = config.CanonicalUUID(snapshotUUID)
			return nil
		}
	}
	return fmt.Errorf("no snapshot %s", snapshotUUID)
}

// RenameVM applies the powered-off rule faithfully: renaming a machine in
// any other state fails before any change, like modifyvm itself.
func (f *fakeVM) RenameVM(_ context.Context, uuid, newName string) error {
	f.record("rename", uuid)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.renameErr != nil {
		return f.renameErr
	}
	for _, m := range f.machines {
		if strings.EqualFold(m.UUID, uuid) && m.State.Kind != vbox.KindOff && m.State.Kind != vbox.KindAborted {
			return fmt.Errorf("%q is %s; VirtualBox renames only a powered-off machine", m.Name, m.State.Word())
		}
	}
	for i := range f.machines {
		if strings.EqualFold(f.machines[i].UUID, uuid) {
			f.machines[i].Name = newName
		}
	}
	return nil
}

type fakeStore struct {
	mu       sync.Mutex
	entries  map[string]config.SSHConfig
	saved    map[string]config.SSHConfig
	storeErr error
}

func newStore() *fakeStore {
	return &fakeStore{entries: map[string]config.SSHConfig{}, saved: map[string]config.SSHConfig{}}
}

func storeWith(uuid string, cfg config.SSHConfig) *fakeStore {
	s := newStore()
	s.entries[config.CanonicalUUID(uuid)] = cfg
	return s
}

func (s *fakeStore) SSH(uuid string) (config.SSHConfig, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Read back anything stored during the run first: what was just saved in
	// the setup must be readable for the very wait that follows it.
	key := config.CanonicalUUID(uuid)
	cfg, ok := s.saved[key]
	if !ok {
		cfg, ok = s.entries[key]
	}
	return cfg, ok && cfg.Configured()
}

func (s *fakeStore) Store(uuid, _ string, cfg config.SSHConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.storeErr != nil {
		return s.storeErr
	}
	s.saved[config.CanonicalUUID(uuid)] = cfg
	return nil
}

func (s *fakeStore) savedFor(uuid string) (config.SSHConfig, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, ok := s.saved[config.CanonicalUUID(uuid)]
	return cfg, ok
}

// fakeGuest is the guest-settings door for tests: in memory only, recording
// what was stored (including the nil that means "cleared").
type fakeGuest struct {
	mu      sync.Mutex
	values  map[string]config.GuestConfig
	cleared map[string]bool
	writes  int // every StoreGuest call, including the nil that means "clear"
}

func newGuest() *fakeGuest {
	return &fakeGuest{values: map[string]config.GuestConfig{}, cleared: map[string]bool{}}
}

func (g *fakeGuest) writeCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.writes
}

func (g *fakeGuest) Guest(uuid string) (config.GuestConfig, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	cfg, ok := g.values[config.CanonicalUUID(uuid)]
	return cfg, ok
}

func (g *fakeGuest) StoreGuest(uuid, _ string, cfg *config.GuestConfig) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.writes++
	key := config.CanonicalUUID(uuid)
	if cfg == nil {
		delete(g.values, key)
		g.cleared[key] = true
		return nil
	}
	g.values[key] = *cfg
	return nil
}

func (g *fakeGuest) value(uuid string) (config.GuestConfig, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	cfg, ok := g.values[config.CanonicalUUID(uuid)]
	return cfg, ok
}

// Clear removes a stored entry; the harness asserts resets through it.
func (s *fakeStore) Clear(uuid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := config.CanonicalUUID(uuid)
	delete(s.saved, key)
	delete(s.entries, key)
	return nil
}

// remainingForTest reports what a UUID still holds across both maps after a
// reset: the empty config means the entry is gone entirely.
func (s *fakeStore) remainingForTest(uuid string) config.SSHConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := config.CanonicalUUID(uuid)
	if cfg, ok := s.saved[key]; ok {
		return cfg
	}
	return s.entries[key]
}

func endpoint(host string, port int, user string) config.SSHConfig {
	return config.SSHConfig{Mode: config.SSHModeEndpoint, Host: host, Port: port, User: user}
}

func aliasCfg(name string) config.SSHConfig {
	return config.SSHConfig{Mode: config.SSHModeAlias, Alias: name}
}

// probers for the network side of the ssh journey.

func dialRefused() ssh.DialFunc {
	return func(ctx context.Context, addr string) (net.Conn, error) {
		return nil, errors.New("connectex: No connection could be made")
	}
}

// dialWrongService answers with a non-SSH banner: the prober gives up quickly
// (a wrong service is permanent), which keeps failure-path tests fast.
func dialWrongService() ssh.DialFunc {
	return func(ctx context.Context, addr string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			_, _ = server.Write([]byte("HTTP/1.1 400 Bad Request\r\n"))
			time.Sleep(300 * time.Millisecond)
			_ = server.Close()
		}()
		return client, nil
	}
}

// dialMustNotDial fails the test if the prober connects at all: used where the
// journey must hand off straight to OpenSSH instead of probing.
func dialMustNotDial(t *testing.T) ssh.DialFunc {
	return func(ctx context.Context, addr string) (net.Conn, error) {
		t.Errorf("ready probe dialled %s where it must hand off directly", addr)
		return nil, errors.New("probe suppressed")
	}
}

func dialBanner(line string) ssh.DialFunc {
	return func(ctx context.Context, addr string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			_, _ = server.Write([]byte(line))
			time.Sleep(400 * time.Millisecond)
			_ = server.Close()
		}()
		return client, nil
	}
}

// dialHolds never answers until release is called: the wait stays "up", so
// esc arrives while the check genuinely runs.
func dialHolds() (ssh.DialFunc, chan struct{}) {
	release := make(chan struct{})
	fn := func(ctx context.Context, addr string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			select {
			case <-release:
				_, _ = server.Write([]byte("SSH-2.0-eventually\r\n"))
				time.Sleep(300 * time.Millisecond)
			case <-ctx.Done():
			}
			_ = server.Close()
		}()
		return client, nil
	}
	return fn, release
}

func fastProber(dial ssh.DialFunc) *ssh.Prober {
	return &ssh.Prober{Dial: dial, Interval: 5 * time.Millisecond,
		AttemptTimeout: 500 * time.Millisecond, NonSSHFailures: 2}
}

// sshExe is a client name LookPath is guaranteed to find on this platform;
// the spec tests only read the command, they never run it.
func sshExe() string {
	if runtime.GOOS == "windows" {
		return "cmd"
	}
	return "sh"
}

// ---------- fixtures ----------

func vmRunning() vbox.Machine {
	return vbox.Machine{Name: "Kali Linux", UUID: "2d620512-4b10-45a8-b070-25435e5106a5",
		State: vbox.NormalizeState("running")}
}
func vmOff() vbox.Machine {
	return vbox.Machine{Name: "Windows 11", UUID: "c907c20b-5a41-4dc2-99c5-0e9bc7773c3a",
		State: vbox.NormalizeState("powered off")}
}
func vmSaved() vbox.Machine {
	return vbox.Machine{Name: "Old Debian", UUID: "a1b2c3d4-e5f6-4a1b-8c2d-3e4f5a6b7c8d",
		State: vbox.NormalizeState("saved")}
}
func vmPausedKali() vbox.Machine {
	// Same display name as vmRunning, different UUID: the exact duplicate-name
	// hazard the spec calls out.
	return vbox.Machine{Name: "Kali Linux", UUID: "99999999-aaaa-bbbb-cccc-dddddddddddd",
		State: vbox.NormalizeState("paused")}
}
func vmWideName() vbox.Machine {
	return vbox.Machine{Name: "开发机 🐧 dev-box", UUID: "77777777-1111-2222-3333-444444444444",
		State: vbox.NormalizeState("running (locked)")}
}

func threeMachines() []vbox.Machine {
	return []vbox.Machine{vmRunning(), vmOff(), vmPausedKali()}
}

func fastConfig() *config.Config {
	c := config.Defaults()
	c.SSHReadyTimeoutSeconds = config.MinSSHTimeoutSeconds // 5s: bounded, not tested to expiry
	return c
}

func baseOptions(f *fakeVM, st *fakeStore, dial ssh.DialFunc) Options {
	return Options{
		Backend:       f,
		Info:          BackendInfo{Name: "VirtualBox", Path: `C:\VirtualBox\VBoxManage.exe`},
		Config:        fastConfig(),
		SSHSettings:   st,
		SSHClient:     sshExe(),
		Prober:        fastProber(dial),
		Width:         80,
		waitTickDelay: 10 * time.Millisecond,
	}
}

func modelWith(f *fakeVM, st *fakeStore, dial ssh.DialFunc) *Model {
	return New(baseOptions(f, st, dial))
}

// directModel is what a quick command hands the prompt after it resolved the
// named machine: one focused action, no second menu.
func directModel(f *fakeVM, st *fakeStore, dial ssh.DialFunc, a vbox.Action, target vbox.Machine) *Model {
	o := baseOptions(f, st, dial)
	o.Direct = &DirectRun{Action: a, VM: target}
	return New(o)
}

// focusModel is a quick command with no VM argument: the action is known, the
// machine must still be picked from the ones that can serve it.
func focusModel(f *fakeVM, st *fakeStore, dial ssh.DialFunc, a vbox.Action) *Model {
	o := baseOptions(f, st, dial)
	o.Direct = &DirectRun{Action: a}
	return New(o)
}
