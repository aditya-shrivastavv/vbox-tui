package app

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/aditya-shrivastavv/vbox-tui/internal/config"
	"github.com/aditya-shrivastavv/vbox-tui/internal/safedisplay"
	"github.com/aditya-shrivastavv/vbox-tui/internal/ssh"
	"github.com/aditya-shrivastavv/vbox-tui/internal/vbox"
)

// ---------- messages ----------

// inventoryMsg carries a full machine-list read for the picker.
type inventoryMsg struct {
	gen uint64
	inv vbox.Inventory
	err error
}

// menuMsg carries a fresh single-machine read used to build the action menu,
// so the menu reflects state as it is now, not as it was when the picker was
// drawn.
type menuMsg struct {
	gen  uint64
	uuid string
	vm   vbox.Machine
	err  error
}

// stepMsg reports one focused operation: the command re-reads the machine,
// and unless its current state refuses the action, performs it. The refusing
// decision is made from the data the command carries; the goroutine never
// touches the model.
type stepMsg struct {
	gen       uint64
	action    vbox.Action
	vm        vbox.Machine // the freshly-read machine
	performed vbox.Action  // the lifecycle verb actually sent, empty for pure-ssh steps
	refused   string       // non-empty: the fresh state does not allow the action
	err       error
	// report carries the restart verdict when the verb was a graceful
	// restart; every other verb leaves it nil.
	report *RestartReport
}

// sshReadyMsg reports the bounded readiness probe result.
type sshReadyMsg struct {
	gen  uint64
	addr string
	err  error
	// cancelled distinguishes "the user left" from "the guest never answered"
	// only for wording; both end the run when they arrive.
	cancelled bool
}

// waitTickMsg advances the elapsed display; the generation retires ticks the
// moment the wait ends.
type waitTickMsg struct{ gen uint64 }

// settingsSavedMsg is the answer from a settings-store write. then is the
// continuation the caller asked for (continue the SSH journey, reopen the
// settings menu, …) and runs only on success.
type settingsSavedMsg struct {
	gen  uint64
	err  error
	then func() tea.Cmd
}

// detailMsg carries one machine's details, fetched only to suggest an
// existing NAT forwarding endpoint during SSH setup. The details inspector is
// gone; this is the detail reader's single remaining job.
type detailMsg struct {
	gen uint64
	m   vbox.Machine
	err error
}

// ---------- init ----------

// Init kicks off the run: the focused flow for a direct subcommand whose
// target was resolved on the command line, otherwise the picker.
func (m *Model) Init() tea.Cmd {
	if m.batchTargets != nil {
		// A multi-target CLI batch: names were resolved before the program
		// started; plan once, then confirm (interactive) or run (a scripted
		// run answered for the user when the command line named every target).
		m.batchAction = m.focusAction
		m.batchPlan = PlanBatch(m.focusAction, m.batchTargets)
		m.batchDone = nil
		if m.noConfirm {
			m.genBatch++
			_, cmd := m.startBatch()
			return tea.Batch(cmd, m.spinner.Tick)
		}
		m.mode = modeConfirm
		return nil
	}
	if m.initVM.UUID != "" {
		m.selected = m.initVM
		switch m.focusAction {
		case ActionSettings:
			m.settingsFocused = true
			m.openSettings()
			return nil
		case ActionSnapshots:
			return tea.Batch(m.openSnapshots(true), m.spinner.Tick)
		case ActionRename:
			return m.beginFocusedRename()
		}
		return tea.Batch(m.runStep(m.focusAction), m.spinner.Tick)
	}
	m.mode = modeLoading
	return tea.Batch(m.loadInventory(), m.spinner.Tick)
}

// startStep begins an operation with the spinner alive; the tick chain dies
// again on the next idle frame (see the TickMsg handler), so idle prompts stay
// perfectly still.
func (m *Model) startStep(action vbox.Action) tea.Cmd {
	return tea.Batch(m.runStep(action), m.spinner.Tick)
}

// ticked appends a spinner seed to a command; nil commands become just the
// seed.
func (m *Model) ticked(cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return m.spinner.Tick
	}
	return tea.Batch(cmd, m.spinner.Tick)
}

// loadInventory reads the registered machines.
func (m *Model) loadInventory() tea.Cmd {
	m.mode = modeLoading
	m.genList++
	gen := m.genList
	backend := m.backend
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
		defer cancel()
		inv, err := backend.List(ctx)
		return inventoryMsg{gen: gen, inv: inv, err: err}
	}
}

// readMenu re-reads one machine's state for the menu.
func (m *Model) readMenu(uuid string) tea.Cmd {
	m.genList++
	gen := m.genList
	backend := m.backend
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
		defer cancel()
		inv, err := backend.List(ctx)
		if err != nil {
			return menuMsg{gen: gen, uuid: uuid, err: err}
		}
		vm, ok := inv.Find(uuid)
		if !ok {
			return menuMsg{gen: gen, uuid: uuid,
				err: fmt.Errorf("that machine is no longer registered with VirtualBox (UUID %s)", uuid)}
		}
		return menuMsg{gen: gen, uuid: uuid, vm: vm}
	}
}

// runStep re-checks the selected machine and, unless its current state refuses
// the action, performs exactly that action — for the SSH journeys only their
// bring-up step, whose continuation (readiness, handoff) happens in
// applyStep. Every path that may change a VM runs through this single door:
// a state change made through the VirtualBox GUI since the menu was drawn can
// never lead to an operation VirtualBox would refuse, or to the wrong
// operation.
func (m *Model) runStep(action vbox.Action) tea.Cmd {
	m.mode = modeBusy
	m.phase = m.phaseFor(action, m.selected)
	m.genAct++
	gen := m.genAct
	// A fresh flag per step: a retired command goroutine keeps the old one
	// and can no longer make a live step look dirtied by an obsolete request.
	live := &atomic.Bool{}
	m.opLive = live
	backend := m.backend
	uuid := m.selected.UUID
	routes := m.routes()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), actionTimeout)
		defer cancel()
		msg := stepMsg{gen: gen, action: action}
		inv, err := backend.List(ctx)
		if err != nil {
			msg.err = err
			return msg
		}
		vm, ok := inv.Find(uuid)
		if !ok {
			msg.err = fmt.Errorf("%s is no longer registered with VirtualBox", uuid)
			return msg
		}
		msg.vm = vm
		verb, why := verbForAction(action, vm.State)
		if verb == VerbRefused {
			msg.refused = why
			return msg
		}
		msg.performed = verb
		if verb == vbox.ActionRestart {
			// The restart door answers with a report, not a bare error: the
			// difference between "the guest confirmed", "the connection
			// dropped", and "the route is blocked" must survive into the
			// result line.
			live.Store(true)
			report := RequestRestart(ctx, backend, vm, routes)
			msg.report = &report
			if !report.OK() {
				msg.err = fmt.Errorf("%s", report.Why)
			}
			return msg
		}
		if verb != "" {
			// The request is leaving now: a cancel from here on cannot recall
			// it, and the honest cancellation wording must know that.
			live.Store(true)
			err = Perform(ctx, backend, verb, uuid)
			if err != nil {
				msg.err = fmt.Errorf("%s", oneError(err, 300))
			}
		}
		return msg
	}
}

// VerbRefused is the marker for "the fresh state does not allow this action",
// with the reason; the empty verb means the machine is already where the
// action starts from, so nothing is sent.
const VerbRefused = vbox.Action("!refused")

// DecideVerb is the exported door to the single verb rule, so the direct
// subcommands in main answer with exactly the same state logic (and the same
// refusal reasons) as the interactive prompt.
func DecideVerb(action vbox.Action, info vbox.StateInfo) (vbox.Action, string) {
	return verbForAction(action, info)
}

// verbForAction decides, against the machine's current state, which lifecycle
// verb a chosen action must actually send. It is the single place where the
// menu wording, the direct-ssh remap, and the refusal rules meet, so the
// quick command and the interactive flow cannot disagree about what an action
// means:
//
//   - `ssh` against a machine that must come up first becomes the
//     start-and-ssh journey (the menu offers them separately; `vbox-tui ssh`
//     always means "get me a shell");
//   - start/restore follow VirtualBox's one request for both: start means
//     headless start when off, restore when saved, and the result will say
//     which actually happened;
//   - everything else must be explicitly allowed for the state, or the
//     reason is returned. Transitional and inaccessible states allow nothing.
func verbForAction(action vbox.Action, info vbox.StateInfo) (vbox.Action, string) {
	allowed := vbox.AllowedActions(info)
	if action == vbox.ActionConnect && !allowed[action] && allowed[vbox.ActionStartConn] {
		action = vbox.ActionStartConn
	}
	switch action {
	case vbox.ActionStartConn:
		switch info.Kind {
		case vbox.KindOff, vbox.KindAborted:
			return vbox.ActionStart, ""
		case vbox.KindSaved:
			return vbox.ActionRestore, ""
		case vbox.KindPaused:
			return vbox.ActionResume, ""
		}
		return "", "" // running: ssh can connect immediately
	case vbox.ActionStart:
		switch info.Kind {
		case vbox.KindSaved:
			return vbox.ActionRestore, ""
		case vbox.KindOff, vbox.KindAborted:
			return action, ""
		}
	case vbox.ActionRestore:
		switch info.Kind {
		case vbox.KindOff, vbox.KindAborted:
			// A restore against a powered-off machine is a start; one request
			// covers both. The result line will say which actually happened.
			return vbox.ActionStart, ""
		}
	case vbox.ActionConnect:
		if allowed[action] {
			return "", ""
		}
		return VerbRefused, stateBlockReason(info)
	case vbox.ActionRename:
		// VirtualBox renames a machine (modifyvm) only while it is powered
		// off, and vbx never stops anything to make that true. The refusal
		// says exactly which step the operator can take instead.
		if allowed[action] {
			return action, ""
		}
		switch info.Kind {
		case vbox.KindRunning:
			return VerbRefused, "it is running, and VirtualBox renames only a powered-off machine — shut it down first (vbx never stops a machine to rename it)"
		case vbox.KindPaused:
			return VerbRefused, "it is paused, and VirtualBox renames only a powered-off machine — resume and shut it down first (vbx never stops a machine to rename it)"
		case vbox.KindSaved:
			return VerbRefused, "its state is saved, and VirtualBox renames only a powered-off machine — restore it and shut it down first (vbx never stops a machine to rename it)"
		}
		return VerbRefused, stateBlockReason(info)
	case vbox.ActionRestart:
		if allowed[action] {
			return action, ""
		}
		if info.Kind == vbox.KindPaused {
			return VerbRefused, "a frozen guest cannot process a reboot — resume it first"
		}
		if info.Kind == vbox.KindSaved {
			return VerbRefused, "it is saved, not running: a graceful restart needs a live guest (restore it first, or start it)"
		}
		if info.Kind == vbox.KindOff || info.Kind == vbox.KindAborted {
			return VerbRefused, "it is not running: a graceful restart is something a live guest does, not something a powered-off machine can fake (a reset would be a hardware shock, which this app never sends)"
		}
		return VerbRefused, stateBlockReason(info)
	}
	if !allowed[action] {
		return VerbRefused, stateBlockReason(info)
	}
	return action, ""
}

// sshAction reports whether the journey ends in SSH rather than in a state
// change.
func sshAction(a vbox.Action) bool {
	return a == vbox.ActionConnect || a == vbox.ActionStartConn
}

// stateBlockReason briefly explains why a state offers nothing. Transitional
// and unknown states quote VirtualBox's own words in full: the short column
// label may be generalised, but the explanation never hides what the
// hypervisor actually reported.
func stateBlockReason(info vbox.StateInfo) string {
	switch info.Kind {
	case vbox.KindTransitional:
		raw := info.Raw
		if raw == "" {
			raw = string(info.State)
		}
		return fmt.Sprintf("it is %s, so there is nothing safe to do right now — try again once that settles", raw)
	case vbox.KindUnavailable:
		return fmt.Sprintf("VirtualBox reports it as %s, so there is nothing safe to do right now — open VirtualBox to repair or re-register it", info.Word())
	case vbox.KindUnknown:
		return fmt.Sprintf("VirtualBox reports an unrecognised state (%s), so there is nothing safe to do right now", info.Word())
	}
	return fmt.Sprintf("VirtualBox reports its state as %s, so there is nothing safe to do right now", info.Word())
}

// ---------- update ----------

// Update never blocks: every branch mutates local state or returns commands.
// Bubble Tea delivers all messages (including command results) through this
// one method on one goroutine, so plain field access is safe; only values a
// command goroutine writes mid-call (opLive) need atomics.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		// Only the width is kept: it tells the renderer where to ellipsise.
		// The height is deliberately dropped — the frame is sized by its
		// content, never by the terminal.
		if msg.Width > 0 {
			m.width = msg.Width
		}
		return m, nil

	case inventoryMsg:
		return m.applyInventory(msg)

	case menuMsg:
		return m.applyMenu(msg)

	case stepMsg:
		return m.applyStep(msg)

	case sshReadyMsg:
		return m.applySSHReady(msg)

	case settingsSavedMsg:
		return m.applySettingsSaved(msg)

	case snapshotsMsg:
		return m.applySnapshots(msg)

	case snapOpDoneMsg:
		return m.applySnapOp(msg)

	case protectCheckMsg:
		return m.applyProtectCheck(msg)

	case renameDoneMsg:
		return m.applyRename(msg)

	case snapRestoreGateMsg:
		return m.applyRestoreGate(msg)

	case batchProgressMsg:
		m.drainBatchPending()
		return m, nil

	case batchDoneMsg:
		return m.applyBatchDone(msg)

	case batchPlanMsg:
		return m.applyBatchPlan(msg)

	case batchTickMsg:
		return m.applyBatchTick(msg)

	case detailMsg:
		return m.applyDetail(msg)

	case waitTickMsg:
		if msg.gen != m.genWait || m.mode != modeWaitSSH {
			return m, nil
		}
		m.waitElapsed++
		return m, m.waitTick()

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		switch m.mode {
		default:
			// Idle surfaces don't show the spinner: letting the chain die here
			// keeps the inline frame still while the user reads or types —
			// every busy entry below re-seeds it.
			cmd = nil
		case modeBusy, modeWaitSSH, modeBatchRun:
			// visible here: keep turning
		}
		return m, cmd
	}
	if key, ok := msg.(tea.KeyMsg); ok {
		return m.handleKey(key)
	}
	return m, nil
}

func (m *Model) applyInventory(msg inventoryMsg) (tea.Model, tea.Cmd) {
	if msg.gen != m.genList {
		return m, nil // superseded by a newer read
	}
	if msg.err != nil && len(msg.inv.Machines) == 0 {
		return m, m.finish(OutcomeFailure, oneError(msg.err, 500))
	}
	if msg.err != nil {
		return m, m.finish(OutcomeFailure,
			"Could not read the VirtualBox machine list: "+oneError(msg.err, 400))
	}
	m.inv = msg.inv
	if len(m.inv.Machines) == 0 {
		return m, m.finish(OutcomeFailure,
			"No registered virtual machines. Create a machine in VirtualBox first.")
	}
	m.pruneSelection()
	m.mode = modePickVM
	m.cursor = 0
	m.clampCursor()
	m.clampListTop()
	if m.focusAction != "" && len(m.selectable()) == 0 {
		return m, m.finish(OutcomeFailure,
			fmt.Sprintf("None of your %d machines can %s right now.",
				len(m.inv.Machines), m.focusAction.Label()))
	}
	return m, nil
}

func (m *Model) applyMenu(msg menuMsg) (tea.Model, tea.Cmd) {
	if msg.gen != m.genList {
		return m, nil
	}
	if msg.err != nil {
		return m, m.finish(OutcomeFailure,
			fmt.Sprintf("%s: %s", quoteName(nameOf(m.selected)), oneError(msg.err, 300)))
	}
	m.selected = msg.vm
	lifecycle := menuActions(msg.vm.State)
	if len(lifecycle) == 0 {
		// The state moved between the picker and this re-read (a guest that
		// finished shutting down, a lock, a crash): explain briefly and put
		// the picker back rather than offer operations VirtualBox would
		// refuse.
		m.note = stateBlockReason(msg.vm.State)
		m.mode = modePickVM
		m.reconcileCursor()
		return m, nil
	}
	// Snapshot and settings are appended to every real menu: they change no
	// power state, and a powered-off machine is exactly when a restore
	// matters most.
	m.actions = append(lifecycle, ActionSnapshots, ActionSettings)
	m.actCursor = 0
	m.confirmAction = ""
	m.mode = modePickAction
	return m, nil
}

func (m *Model) applyStep(msg stepMsg) (tea.Model, tea.Cmd) {
	if msg.gen != m.genAct {
		return m, nil
	}
	name := quoteName(nameOf(m.selected))
	if msg.refused != "" {
		return m, m.finish(OutcomeFailure,
			fmt.Sprintf("Cannot %s %s: %s. Nothing was changed.",
				msg.action.Label(), name, msg.refused))
	}
	if msg.report != nil {
		// Wording from the restart door: it knows whether the guest
		// confirmed, whether the connection dropped, or whether the route
		// was blocked before anything was sent. The generic error path below
		// would flatten those three into one sentence, so the report wins.
		line := msg.report.Summary(quoteName(nameOf(m.selected)))
		outcome := OutcomeSuccess
		if !msg.report.OK() {
			outcome = OutcomeFailure
		}
		return m, m.finish(outcome, line)
	}
	if msg.err != nil {
		return m, m.finish(OutcomeFailure,
			fmt.Sprintf("Could not %s %s: %s", msg.action.Label(), name, oneError(msg.err, 400)))
	}
	reported := msg.action
	if msg.performed != "" {
		// A request reached the hypervisor: the machine is the freshly-read
		// one from here on, and a later cancellation must admit what is in
		// motion. The result line names the verb actually sent, so a
		// `start` that became a restore honestly says "Restored …".
		m.opIssued = true
		m.selected = msg.vm
		reported = msg.performed
	}
	if sshAction(msg.action) {
		return m.sshPhase()
	}
	if msg.performed == "" {
		// Defensive: the verb rule decided no request was needed. Say exactly
		// that — a line claiming something "Started" when nothing was sent
		// would be a lie of commission.
		return m, m.finish(OutcomeSuccess,
			fmt.Sprintf("%s is already %s: nothing was sent.", name, msg.vm.State.Word()))
	}
	return m, m.finish(OutcomeSuccess, SuccessLine(reported, quoteName(nameOf(m.selected))))
}

func (m *Model) applySSHReady(msg sshReadyMsg) (tea.Model, tea.Cmd) {
	if msg.gen != m.genWait {
		return m, nil
	}
	m.cancelWait = nil
	if msg.err != nil {
		note := ""
		if m.opIssued {
			note = fmt.Sprintf(" %s was already started — cancelling the wait cannot undo that.",
				quoteName(nameOf(m.selected)))
		}
		return m, m.finish(OutcomeFailure,
			fmt.Sprintf("SSH did not become ready on %s: %s.%s", safedisplay.Text(msg.addr), oneError(msg.err, 300), note))
	}
	return m.buildHandoff()
}

func (m *Model) applyDetail(msg detailMsg) (tea.Model, tea.Cmd) {
	if msg.gen != m.genDet {
		return m, nil
	}
	if msg.err == nil {
		m.detail = &msg.m
	}
	// A missing detail only removes the NAT suggestion; the setup still asks.
	// The mode is text entry, so no spinner and no follow-up command here.
	return m, nil
}

// ---------- ssh phase ----------

// sshPhase continues an SSH journey once the machine is running (or was
// already): connect when settings exist, otherwise run the short inline setup
// first.
func (m *Model) sshPhase() (tea.Model, tea.Cmd) {
	m.note = ""
	if _, ok := m.sshSet.SSH(m.selected.UUID); ok {
		return m.sshWait()
	}
	m.setupStep = 0
	m.setupMode = ""
	m.setupBuf, m.setupHost, m.setupPort = "", "", ""
	m.setupErr = ""
	m.mode = modeSSHSetup
	return m, m.loadDetail(m.selected.UUID)
}

// sshWait starts the bounded, cancellable readiness check. An ssh-config
// alias may hide a ProxyJump or ProxyCommand route a direct TCP probe cannot
// verify, so for aliases OpenSSH itself becomes the judge and the handoff is
// immediate.
func (m *Model) sshWait() (tea.Model, tea.Cmd) {
	sshCfg, ok := m.sshSet.SSH(m.selected.UUID)
	if !ok {
		return m, m.finish(OutcomeFailure,
			fmt.Sprintf("No SSH settings for %s after all — run \"vbox-tui ssh %s\" to set them up.",
				quoteName(nameOf(m.selected)), nameOf(m.selected)))
	}
	sshCfg = sshCfg.Normalise()
	if sshCfg.Mode == config.SSHModeAlias {
		return m.buildHandoff()
	}
	host, port, needsOpenSSH, err := ssh.ProbeTarget(sshCfg, nil)
	if err != nil || needsOpenSSH {
		return m.buildHandoff()
	}
	addr := ssh.JoinHostPort(host, port)
	timeout := m.cfg.SSHReadyTimeout()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	m.cancelWait = cancel
	m.waitAddr = addr
	m.waitTimeout = timeout
	m.waitElapsed = 0
	m.mode = modeWaitSSH
	m.genWait++
	gen := m.genWait
	prober := m.prober
	// ticked: the wait can begin from a dead chain (setup saved) as well as a
	// live one; a duplicate seed dies by itself on the next idle frame.
	return m, m.ticked(tea.Batch(func() tea.Msg {
		_, err := prober.WaitForSSH(ctx, host, port)
		return sshReadyMsg{gen: gen, addr: addr, err: err,
			cancelled: errors.Is(ctx.Err(), context.Canceled)}
	}, m.waitTick()))
}

func (m *Model) waitTick() tea.Cmd {
	gen := m.genWait
	delay := m.tickDelay
	return func() tea.Msg {
		time.Sleep(delay)
		return waitTickMsg{gen: gen}
	}
}

// buildHandoff resolves the OpenSSH command and ends the interactive part of
// the run with OutcomeSSHHandoff. main then releases the terminal to ssh and
// exits with its status — the picker never reopens. In demo mode no process
// is ever launched: main prints what would have run.
func (m *Model) buildHandoff() (tea.Model, tea.Cmd) {
	sshCfg, _ := m.sshSet.SSH(m.selected.UUID)
	spec, err := ssh.BuildSpecWith(m.sshClient, sshCfg)
	if err != nil {
		return m, m.finish(OutcomeFailure, oneError(err, 400))
	}
	m.sshSpec = spec
	m.handoffDemo = m.demo
	// The label names the stored endpoint or alias: metadata, encoded for
	// display. The ExecSpec behind it carries the raw arguments — only what
	// is printed here is sanitised.
	return m, m.finish(OutcomeSSHHandoff, "Connecting via "+safedisplay.Text(spec.Label))
}

// ---------- SSH setup ----------

// commitSetup stores the settings assembled across the setup steps. The
// first-use journey continues into the readiness wait; the settings editor
// returns to its menu. commitStore keeps the write off the UI goroutine in
// the same way either way.
func (m *Model) commitSetup(cfg config.SSHConfig) tea.Cmd {
	editing := m.setupEditing
	cfg = cfg.Normalise()
	store := func() error { return m.sshSet.Store(m.selected.UUID, m.selected.Name, cfg) }
	if editing {
		return m.commitStore(store, func() tea.Cmd {
			m.setupEditing = false
			m.note = "SSH settings saved: " + safedisplay.Text(cfg.Display())
			m.openSettings()
			return nil
		})
	}
	return m.commitStore(store, func() tea.Cmd {
		_, cmd := m.sshWait()
		return cmd
	})
}

func (m *Model) loadDetail(uuid string) tea.Cmd {
	if m.detail != nil && config.CanonicalUUID(m.detail.UUID) == config.CanonicalUUID(uuid) {
		return nil
	}
	m.genDet++
	gen := m.genDet
	backend := m.backend
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), detailTimeout)
		defer cancel()
		machine, err := backend.Detail(ctx, uuid)
		return detailMsg{gen: gen, m: machine, err: err}
	}
}

// ---------- shared helpers ----------

// menuActions turns the allowed set into an ordered menu for one machine.
// Only operations the current state actually supports appear: no greyed-out
// clutter, no impossible requests.
func menuActions(info vbox.StateInfo) []vbox.Action {
	allowed := vbox.AllowedActions(info)
	order := []vbox.Action{
		vbox.ActionConnect, vbox.ActionStartConn,
		vbox.ActionStart, vbox.ActionRestore, vbox.ActionResume,
		vbox.ActionPause, vbox.ActionStop, vbox.ActionSaveState,
		vbox.ActionRestart, vbox.ActionRename,
	}
	var out []vbox.Action
	for _, a := range order {
		if allowed[a] {
			out = append(out, a)
		}
	}
	return out
}

// menuLabel renders an action for the menu, in the context of the selected
// machine's state: the one bring-up-then-ssh action adapts its wording to
// what will actually happen; the rest keep fixed labels.
func menuLabel(info vbox.StateInfo, a vbox.Action) string {
	switch a {
	case vbox.ActionStart:
		return "Start headless"
	case vbox.ActionRestore:
		return "Restore"
	case vbox.ActionResume:
		return "Resume"
	case vbox.ActionPause:
		return "Pause"
	case vbox.ActionStop:
		// Deliberate: "stop" would suggest the machine is already off when
		// this returns. The action requests a graceful ACPI shutdown, and
		// the menu says so.
		return "Shut down"
	case vbox.ActionSaveState:
		return "Save state"
	case vbox.ActionConnect:
		return "SSH"
	case vbox.ActionStartConn:
		switch info.Kind {
		case vbox.KindSaved:
			return "Restore and SSH"
		case vbox.KindPaused:
			return "Resume and SSH"
		}
		return "Start and SSH"
	case vbox.ActionRestart:
		return "Restart guest"
	case vbox.ActionRename:
		// The menu only shows it where VirtualBox honours it (powered off),
		// so the label can promise the operation plainly.
		return "Rename…"
	case ActionSnapshots:
		return "Snapshots…"
	case ActionSettings:
		return "SSH and restart settings…"
	}
	return a.Label()
}

// needsConfirm marks the interactive-only safeguards: choosing these in the
// menu asks once; explicit action subcommands never do. A guest restart is
// listed because people expect it to behave like a reboot they can take back
// — it requests one from the guest, and the mechanism's prerequisite is worth
// naming before it runs.
func needsConfirm(a vbox.Action) bool {
	return a == vbox.ActionStop || a == vbox.ActionSaveState || a == vbox.ActionRestart
}

// phaseFor words the busy line.
func (m *Model) phaseFor(a vbox.Action, vm vbox.Machine) string {
	name := nameOf(vm)
	switch a {
	case vbox.ActionConnect:
		return fmt.Sprintf("Connecting to %s…", name)
	case vbox.ActionStartConn:
		switch vm.State.Kind {
		case vbox.KindSaved:
			return fmt.Sprintf("Restoring %s…", name)
		case vbox.KindPaused:
			return fmt.Sprintf("Resuming %s…", name)
		}
		return fmt.Sprintf("Starting %s…", name)
	}
	verb := map[vbox.Action]string{
		vbox.ActionStart:     "Starting",
		vbox.ActionRestore:   "Restoring",
		vbox.ActionResume:    "Resuming",
		vbox.ActionPause:     "Pausing",
		vbox.ActionStop:      "Requesting shutdown of",
		vbox.ActionSaveState: "Saving",
	}[a]
	if verb == "" {
		verb = "Working on"
	}
	return fmt.Sprintf("%s %s…", verb, name)
}

// quoteName wraps a name for a sentence. Encoding the argument (again, if
// the caller already went through nameOf — the encoder is idempotent) makes
// every result line's quoting honest: what is between the quotes is literal
// text, never live terminal instructions.
func quoteName(name string) string { return `"` + safedisplay.Text(name) + `"` }
