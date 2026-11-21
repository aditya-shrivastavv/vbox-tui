// Package app is the compact inline interaction for vbox-tui: a VM picker, a
// state-aware action picker, one short progress line, and one result line.
//
// It is deliberately not a dashboard: the interaction renders inline under the
// shell prompt, never enters the alternate screen, never clears the terminal,
// never pads to the terminal height, and returns to the shell as soon as the
// chosen task is done.
//
// Update never blocks and View stays pure: every external call is a tea.Cmd,
// and background results carry the generation they were issued under so a
// slow answer can never overwrite newer state. Model fields are only ever
// mutated inside Update; command goroutines deliver values through messages.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/aditya-shrivastavv/vbox-tui/internal/config"
	"github.com/aditya-shrivastavv/vbox-tui/internal/runner"
	"github.com/aditya-shrivastavv/vbox-tui/internal/safedisplay"
	"github.com/aditya-shrivastavv/vbox-tui/internal/ssh"
	"github.com/aditya-shrivastavv/vbox-tui/internal/vbox"
)

// visibleRows is how many VM rows the picker shows before scrolling inside
// itself. A few VMs produce a few rows; longer inventories scroll.
const visibleRows = 8

// Bounded timeouts so a wedged VirtualBox cannot hang the prompt.
const (
	listTimeout   = 25 * time.Second
	actionTimeout = 30 * time.Second
	detailTimeout = 20 * time.Second
)

// Mode is the step of the interaction the model is on.
type Mode int

// Interaction steps.
const (
	modeLoading      Mode = iota // reading the inventory
	modePickVM                   // choosing a VM (type to filter, space to select)
	modePickAction               // choosing an action for one VM
	modeBusy                     // one operation in flight, or a chained step
	modeConfirm                  // inline y/N for disruptive actions
	modeSSHSetup                 // short sequential SSH settings prompts
	modeWaitSSH                  // bounded, cancellable readiness wait
	modeBatchConfirm             // y/N for a plan that touches several machines
	modeBatchRun                 // sequential batch execution, one line per VM
	modeSettings                 // the per-VM settings menu (vbx config)
	modeChoose                   // a generic list choice (guest OS, restart mode, …)
	modeText                     // a generic text prompt (snapshot name, …)
	modeSnapList                 // the snapshot tree with current marked
	modeDone                     // final result installed; the program quits
)

// App-level focus actions: not lifecycle verbs, so they live here rather than
// in the vbox package's action list. A focused run with one of these opens
// that flow for the picked machine.
const (
	ActionSnapshots vbox.Action = "snapshots"
	ActionSettings  vbox.Action = "settings"
)

// Outcome is how the run ended. main maps it to an exit code and prints the
// single result line, so wording lives in one place and tests read accessors
// instead of scraping rendered frames.
type Outcome int

// Run outcomes.
const (
	// OutcomeNone: the run has not finished.
	OutcomeNone Outcome = iota
	// OutcomeSuccess: the requested change happened, or was honestly
	// requested (a shutdown the guest has not finished executing).
	OutcomeSuccess
	// OutcomeFailure: an error ended the run.
	OutcomeFailure
	// OutcomeCancelled: the user cancelled. The app changed nothing at this
	// step; if an earlier step of a chain already issued a VM operation, the
	// summary says so because cancelling the wait cannot recall it.
	OutcomeCancelled
	// OutcomeSSHHandoff: the interaction ended and OpenSSH should now own the
	// terminal. main runs SSHCommand() and exits with its status.
	OutcomeSSHHandoff
)

// BackendInfo describes the active backend for short notices.
type BackendInfo struct {
	Name string
	Path string
	// Demo marks the demo backend, which never touches a hypervisor.
	Demo bool
	// Unavailable is true when no VirtualBox installation could be found.
	Unavailable bool
	// Hint carries remediation text for an unavailable backend.
	Hint string
}

// SSHSettings is where per-VM SSH settings live: the JSON config file for
// normal runs, an in-memory map for the demo, behind one door. Identity is the
// VM UUID, so settings survive a rename and duplicate display names cannot
// collide.
type SSHSettings interface {
	SSH(uuid string) (config.SSHConfig, bool)
	Store(uuid, name string, sshCfg config.SSHConfig) error
	// Clear removes saved SSH settings for one VM (the settings editor's
	// reset). A VM with nothing stored clears without error.
	Clear(uuid string) error
}

// GuestSettings is the same door for the guest-control block (OS family and
// restart route). StoreGuest with a nil cfg clears the block.
type GuestSettings interface {
	Guest(uuid string) (config.GuestConfig, bool)
	StoreGuest(uuid, name string, cfg *config.GuestConfig) error
}

// DirectRun focuses the interaction for an action subcommand. Action is
// always set; VM is the machine resolved from the command line by exact name
// or UUID, empty when one still has to be picked. A focused run restricts the
// picker to machines that can serve the action, skips the action menu, and
// exits after the single operation.
//
// Targets holds a batch: several machines resolved ahead of time from the
// command line. The command line is already an explicit confirmation for
// every name it contains, so NoConfirm runs the plan without the y/N gate.
type DirectRun struct {
	Action    vbox.Action
	VM        vbox.Machine
	Targets   []vbox.Machine
	NoConfirm bool
	// RenameTo carries the new name for `vbx rename "Old" "New"`: with it,
	// the focused run performs the rename immediately; without it the run
	// asks inline, prefilled with the machine's current name.
	RenameTo string
}

// Options configures New.
type Options struct {
	Backend     vbox.Manager
	Info        BackendInfo
	Config      *config.Config
	Demo        bool
	SSHSettings SSHSettings
	// GuestSettings carries the guest-control block; nil means guest settings
	// are read from (and written to) the run's Config directly.
	GuestSettings GuestSettings
	SSHClient     string
	Prober        *ssh.Prober
	Runner        runner.Runner
	// Stderr receives configuration warnings at construction time, while the
	// caller still owns the screen.
	Stderr io.Writer
	// Width is the terminal width as far as the caller can tell; the program
	// also delivers a WindowSizeMsg once it can measure the real one.
	Width int
	// Direct, when non-nil, focuses the run on one action.
	Direct *DirectRun
	// Now is injectable for deterministic tests.
	Now func() time.Time

	// waitTickDelay overrides the one-second elapsed counter of the readiness
	// wait; only tests set it.
	waitTickDelay time.Duration
}

// Model is the inline prompt model.
type Model struct {
	backend   vbox.Manager
	info      BackendInfo
	cfg       *config.Config
	sshSet    SSHSettings
	guestDoor guestDoor
	demo      bool

	// settingsFocused marks `vbx config [VM]`: leaving the settings menu
	// ends the run because the menu was the whole request.
	settingsFocused bool

	sshClient string
	prober    *ssh.Prober
	run       runner.Runner
	now       func() time.Time

	// Generations retire background results the user has moved past.
	genList uint64
	genAct  uint64
	genDet  uint64
	genWait uint64
	genSet  uint64

	mode  Mode
	phase string // busy-line text, e.g. `Starting Kali Linux…`

	outcome Outcome
	summary string
	// sshSpec is the OpenSSH command main runs after the program releases the
	// terminal (OutcomeSSHHandoff); handoffDemo marks a simulated demo session
	// that must never launch a real client.
	sshSpec     ssh.ExecSpec
	handoffDemo bool

	// width is the printed width available for lines. There is no height
	// because there is nothing to fill: frames are content-sized.
	width int

	spinner spinner.Model

	// styles is fixed at construction; --no-color is handled globally by
	// lipgloss.SetColorProfile at the edge of main.
	styles Styles

	// inventory / picker
	inv      vbox.Inventory
	note     string // one-line explanation shown above the picker rows
	filter   string
	cursor   int
	listTop  int
	selected vbox.Machine
	// initVM is a machine already resolved from the command line before the
	// program started; the run focuses on it without a picker round.
	initVM vbox.Machine
	// batchTargets/noConfirm come from a multi-target CLI batch.
	batchTargets []vbox.Machine
	noConfirm    bool
	// focusAction, when non-empty (direct commands), restricts the picker to
	// machines that can serve this action and runs it on selection without a
	// second menu.
	focusAction vbox.Action

	// action menu
	actions   []vbox.Action
	actCursor int

	// inline confirm: confirmAction holds the action awaiting a y/N answer.
	confirmAction vbox.Action

	// opIssued records that a state-changing request reached VirtualBox, so a
	// later cancellation says honestly what is still in motion. opLive marks
	// a request the running command goroutine has already handed to
	// VirtualBox; it is atomic because the cancel handler may read it while
	// that goroutine writes.
	opIssued bool
	opLive   *atomic.Bool

	// ssh setup (sequential: mode → alias or host → port → user)
	detail    *vbox.Machine // details for the selected VM (NAT suggestion)
	setupStep int           // 0 = mode, 1 = alias or host, 2 = port, 3 = user
	setupMode config.SSHMode
	setupBuf  string // current field text
	setupHost string // saved between steps
	setupPort string
	setupUser string // kept between steps so the editor can prefill again
	setupErr  string // validation error shown above the current field
	// setupEditing marks the settings editor's pass through the same steps:
	// values carry over prefilled and the sequence can clear instead of save.
	setupEditing bool

	// settings menu (vbx config)
	settingsCursor int
	settingsReturn settingsReturn
	resetScope     config.ResetScope

	// rename flow (single machine)
	genRename      uint64
	renameOld      string
	renameFocused  bool
	renameFromMenu bool
	renameTo       string
	names          nameCache

	// guest editor (two chooser steps inside modeChoose)
	guestStep     int
	guestOSCursor int
	restartCursor int

	// generic text step (snapshot name/description)
	textPrompt string
	textValue  string
	textErr    string
	snapName   string

	// generic chooser (batch verb, guest editor steps)
	chooseKind        chooseKind
	chooseTitle       string
	chooseHint        string
	chooseCursor      int
	batchChoiceReturn Mode

	// snapshot flow
	genSnap         uint64
	snaps           vbox.SnapshotTree
	snapCursor      int
	snapFocused     bool // entered by `vbx snapshot [VM]`: esc completes the listing
	confirmSnapshot bool // the y/s/n restore gate is live
	restoreTarget   vbox.Snapshot
	restoreVM       vbox.Machine

	// batch flow
	genBatch       uint64
	batchAction    vbox.Action
	batchPlan      []BatchTarget
	batchDone      []string // progress lines, one per finished machine
	batchSum       *BatchSummary
	batchCancel    context.CancelFunc
	batchCancelReq bool // esc pressed while a machine is still in flight
	batchMu        sync.Mutex
	batchPending   []batchProgressMsg
	batchPollDelay time.Duration // tests shorten the progress poll
	// selection marks picked VMs (space) for a batch run; keyed by canonical
	// UUID so a re-read with the same identity keeps its mark.
	selectedSet map[string]bool

	// readiness wait
	cancelWait  context.CancelFunc
	waitAddr    string
	waitTimeout time.Duration
	waitElapsed int
	tickDelay   time.Duration
}

var _ tea.Model = (*Model)(nil)

// New builds the model.
func New(opts Options) *Model {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	cfg := opts.Config
	if cfg == nil {
		cfg = config.Defaults()
	}
	prober := opts.Prober
	if prober == nil {
		prober = ssh.NewProber()
	}
	r := opts.Runner
	if r == nil {
		r = runner.New()
	}
	sp := spinner.New()
	sp.Spinner = spinner.Spinner{
		// Plain ASCII frames: the legacy Windows console renders them and no
		// patched font is required anywhere else.
		Frames: []string{"|", "/", "-", "\\"},
		FPS:    time.Second / 8,
	}
	width := opts.Width
	if width <= 0 {
		width = 80
	}
	m := &Model{
		opLive:    &atomic.Bool{},
		backend:   opts.Backend,
		info:      opts.Info,
		cfg:       cfg,
		sshSet:    opts.SSHSettings,
		demo:      opts.Demo,
		sshClient: orDefault(opts.SSHClient, ssh.ClientExe),
		prober:    prober,
		run:       r,
		now:       now,
		spinner:   sp,
		width:     width,
	}
	m.tickDelay = opts.waitTickDelay
	if m.tickDelay <= 0 {
		m.tickDelay = time.Second
	}
	if opts.Direct != nil {
		m.focusAction = opts.Direct.Action
		m.initVM = opts.Direct.VM
		m.batchTargets = opts.Direct.Targets
		m.noConfirm = opts.Direct.NoConfirm
		m.renameTo = opts.Direct.RenameTo
	}
	m.styles = DefaultStyles()
	if m.sshSet == nil {
		m.sshSet = &configSettings{cfg: cfg, persist: !opts.Demo}
	}
	m.guestDoor = guestDoor{direct: opts.GuestSettings, cfg: cfg, persist: !opts.Demo}
	m.names = nameCache{cfg: cfg, persist: !opts.Demo}
	if problems := cfg.Validate(); len(problems) > 0 && opts.Stderr != nil {
		// Told, not fatal: the run continues with repaired values, and the
		// message names the file so the user can fix it.
		fmt.Fprintf(opts.Stderr, "vbox-tui: configuration: %s\n", oneError(errors.Join(problems...), 400))
	}
	return m
}

// configSettings adapts the JSON config file to SSHSettings.
type configSettings struct {
	cfg     *config.Config
	persist bool
}

// ConfigSettings exposes a configuration file through the SSHSettings door so
// callers outside the package (the direct subcommands in main) read and write
// the very same per-VM settings the prompt uses.
func ConfigSettings(cfg *config.Config, persist bool) SSHSettings {
	return &configSettings{cfg: cfg, persist: persist}
}

func (s *configSettings) SSH(uuid string) (config.SSHConfig, bool) {
	vm, _ := s.cfg.VM(uuid)
	return vm.SSH, vm.SSH.Configured()
}

func (s *configSettings) Store(uuid, name string, sshCfg config.SSHConfig) error {
	if !s.persist {
		return s.cfg.SetVMSSH(uuid, name, sshCfg)
	}
	return s.cfg.Edit(func(c *config.Config) error {
		return c.SetVMSSH(uuid, name, sshCfg)
	})
}

// Clear drops the saved SSH block. The entry (with any guest block it holds)
// survives; only SSH settings go.
func (s *configSettings) Clear(uuid string) error {
	if !s.persist {
		s.cfg.ResetVM(uuid, config.ResetSSH)
		return nil
	}
	return s.cfg.Edit(func(c *config.Config) error {
		c.ResetVM(uuid, config.ResetSSH)
		return nil
	})
}

// guestDoor is the model's guest-settings door: a caller-provided
// implementation (the demo, or main's config-backed door) or the run's own
// config file directly.
type guestDoor struct {
	direct  GuestSettings
	cfg     *config.Config
	persist bool
}

func (d guestDoor) get(uuid string) (config.GuestConfig, bool) {
	if d.direct != nil {
		return d.direct.Guest(uuid)
	}
	g := d.cfg.VMGuest(uuid)
	if g == nil {
		return config.GuestConfig{}, false
	}
	return *g, true
}

func (d guestDoor) store(uuid, name string, cfg *config.GuestConfig) error {
	if d.direct != nil {
		return d.direct.StoreGuest(uuid, name, cfg)
	}
	if !d.persist {
		return d.cfg.SetVMGuest(uuid, name, cfg)
	}
	return d.cfg.Edit(func(c *config.Config) error {
		return c.SetVMGuest(uuid, name, cfg)
	})
}

// ConfigGuestSettings exposes the config file's guest block through the
// GuestSettings door for callers outside the package.
func ConfigGuestSettings(cfg *config.Config, persist bool) GuestSettings {
	return configGuest{cfg: cfg, persist: persist}
}

type configGuest struct {
	cfg     *config.Config
	persist bool
}

func (g configGuest) Guest(uuid string) (config.GuestConfig, bool) {
	gc := g.cfg.VMGuest(uuid)
	if gc == nil {
		return config.GuestConfig{}, false
	}
	return *gc, true
}

func (g configGuest) StoreGuest(uuid, name string, cfg *config.GuestConfig) error {
	if !g.persist {
		return g.cfg.SetVMGuest(uuid, name, cfg)
	}
	return g.cfg.Edit(func(c *config.Config) error {
		return c.SetVMGuest(uuid, name, cfg)
	})
}

// routes assembles the two restart mechanisms from the doors and the runner.
func (m *Model) routes() RestartRoutes {
	return RestartRoutes{
		SSH:    m.sshSet.SSH,
		Guest:  m.guestDoor.get,
		Run:    m.run,
		Client: m.sshClient,
	}
}

// ---------- accessors ----------

// Outcome reports how the run ended.
func (m *Model) Outcome() Outcome { return m.outcome }

// Summary is the single result line, meaningful once Outcome != OutcomeNone.
// It carries no tick or cross: main adds those so the symbol choice stays in
// one place.
func (m *Model) Summary() string { return m.summary }

// SSHCommand is the OpenSSH command main must run once the program has
// returned and released the terminal (OutcomeSSHHandoff).
func (m *Model) SSHCommand() ssh.ExecSpec { return m.sshSpec }

// SSHSimulated reports that this handoff was produced by the demo backend:
// main must print what would have run instead of launching a client.
func (m *Model) SSHSimulated() bool { return m.handoffDemo }

// SSHRouted reports whether the interaction ended in a terminal handoff (real
// or simulated).
func (m *Model) SSHRouted() bool { return m.outcome == OutcomeSSHHandoff }

// ExitCode is the process exit code main should propagate.
func (m *Model) ExitCode() int {
	switch m.outcome {
	case OutcomeSuccess, OutcomeSSHHandoff:
		return 0
	case OutcomeFailure, OutcomeCancelled:
		return 1
	}
	return 0
}

// modeForTest/outcomeSeenForTest let the scripted test driver observe the
// step the prompt is on. The driver never starts the event loop, so reading
// these fields between its own sends cannot race anything.
func (m *Model) modeForTest() Mode           { return m.mode }
func (m *Model) outcomeSeenForTest() Outcome { return m.outcome }

// finish ends the run and returns control to the shell. The final line itself
// is printed by main once the renderer is gone, so the renderer's teardown
// cannot erase it. An outcome, once recorded, is never overwritten: a slow
// command answering after the user already cancelled must not rewrite the
// result line.
func (m *Model) finish(outcome Outcome, summary string) tea.Cmd {
	if m.mode == modeDone {
		return tea.Quit
	}
	m.mode = modeDone
	m.outcome = outcome
	m.summary = summary
	if m.cancelWait != nil {
		m.cancelWait()
		m.cancelWait = nil
	}
	return tea.Quit
}

// nameOf describes a machine in a sentence. A name is never invented,
// shortened or normalised for display convenience; only a genuinely nameless
// entry (VirtualBox reports inaccessible machines by UUID) falls back to it.
//
// Every sentence the app renders is built through here, which makes nameOf
// the display chokepoint: the returned text is safe-display encoded
// (safedisplay.Text), so a name carrying escape codes, carriage returns, or
// BiDi overrides shows as visible <ESC>/<0D>/<U+202E> markers instead of
// steering the terminal. Resolution, UUID handling and command arguments
// everywhere else continue to use the raw name or the UUID — only what a
// human reads goes through this encoding.
func nameOf(vm vbox.Machine) string {
	return safedisplay.Text(rawNameOf(vm))
}

// rawNameOf is nameOf without the display encoding, for comparison paths
// that must see the exact registered name.
func rawNameOf(vm vbox.Machine) string {
	if strings.TrimSpace(vm.Name) != "" {
		return vm.Name
	}
	if strings.TrimSpace(vm.UUID) != "" {
		return vm.UUID
	}
	return "(unnamed machine)"
}

// Perform issues exactly one lifecycle request against a backend. The inline
// flow and the direct subcommands share this one implementation so the verbs
// cannot drift apart, and every request addresses the machine by UUID.
//
// ActionStop only requests a graceful ACPI shutdown: success means the guest
// received the request, not that it has stopped.
//
// ActionRestart needs the restart routes (SSH settings door, guest door,
// runner) and a fresh machine view; pass them as the optional fifth argument
// when issuing a restart, through PerformRestart with a plan-time re-read
// instead.
func Perform(ctx context.Context, backend vbox.Manager, action vbox.Action, uuid string) error {
	if err := performLifecycle(ctx, backend, action, uuid); err != nil {
		return err
	}
	return nil
}

func performLifecycle(ctx context.Context, backend vbox.Manager, action vbox.Action, uuid string) error {
	if strings.TrimSpace(uuid) == "" {
		return errors.New("refusing to run a VM operation without a UUID")
	}
	switch action {
	case vbox.ActionStart, vbox.ActionRestore:
		// VirtualBox resumes a saved state with the same request it uses to
		// start a machine, so both map to one call and neither can be issued
		// as the wrong verb.
		return backend.PowerOn(ctx, uuid)
	case vbox.ActionResume:
		return backend.Resume(ctx, uuid)
	case vbox.ActionPause:
		return backend.Pause(ctx, uuid)
	case vbox.ActionStop:
		return backend.ShutdownGracefully(ctx, uuid)
	case vbox.ActionSaveState:
		return backend.SaveState(ctx, uuid)
	case vbox.ActionRestart:
		return errors.New("a graceful restart needs its routes; use PerformRestart")
	default:
		return fmt.Errorf("%q is not a lifecycle request", action)
	}
}

// PerformRestart asks one machine's guest OS to reboot itself through its
// configured route. It is separate from the lifecycle switch on purpose: the
// mechanism is chosen per VM from stored settings, and a restart can neither
// reset hardware nor power-cycle anything, so it must never look like just
// another verb on a Manager call list.
func PerformRestart(ctx context.Context, backend vbox.Manager, target vbox.Machine, routes RestartRoutes) RestartReport {
	return RequestRestart(ctx, backend, target, routes)
}

// SuccessLine is the concise honest completion line for a lifecycle action.
func SuccessLine(action vbox.Action, name string) string {
	switch action {
	case vbox.ActionStart:
		return fmt.Sprintf("Started %s in headless mode.", name)
	case vbox.ActionRestore:
		return fmt.Sprintf("Restored %s from its saved state.", name)
	case vbox.ActionResume:
		return fmt.Sprintf("Resumed %s.", name)
	case vbox.ActionPause:
		return fmt.Sprintf("Paused %s.", name)
	case vbox.ActionStop:
		// Accurate: the ACPI request was delivered; the guest decides from
		// there. A delivered request is never worded as a completed stop.
		return fmt.Sprintf("Shutdown requested for %s.", name)
	case vbox.ActionSaveState:
		return fmt.Sprintf("Saved %s's state.", name)
	}
	return fmt.Sprintf("%s completed for %s.", action.Label(), name)
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
