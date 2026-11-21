package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/aditya-shrivastavv/vbox-tui/internal/safedisplay"
	"github.com/aditya-shrivastavv/vbox-tui/internal/vbox"
)

// Snapshot flows: list the tree, take one, restore one. Deleting snapshots is
// deliberately absent — this app controls power states and its own settings,
// and removing points in time belongs to whichever tool owns backups.
//
// Restore is the destructive member of the family, so it is wrapped: the
// machine must be verifiably powered off at the moment of the request (not
// when the list was opened), a protective snapshot of the current state is
// offered up front, a failed protective snapshot aborts the restore instead
// of silently destroying the only copy, and the confirmation names both the
// VM and the exact snapshot. The request always addresses the snapshot by
// UUID: names may repeat in the tree.

// ---------- messages ----------

type snapshotsMsg struct {
	gen  uint64
	uuid string
	tree vbox.SnapshotTree
	err  error
}

type snapOpDoneMsg struct {
	gen  uint64
	kind string // "take" or "restore"
	err  error
	// protectFor: for a take, the snapshot UUID a protective take was made
	// for — its success continues into that restore; the empty string means
	// an ordinary user-requested take. For a restore it is the protective
	// snapshot's name, for the wording.
	protectFor string
}

type snapRestoreGateMsg struct {
	gen      uint64
	vm       vbox.Machine
	snapUUID string
	err      error
}

// ---------- list ----------

// openSnapshots shows the tree. fromFocused marks the `vbx snapshot` path:
// there, closing the list completes the command.
func (m *Model) openSnapshots(fromFocused bool) tea.Cmd {
	m.snapFocused = fromFocused
	m.snapCursor = 0
	m.snaps = vbox.SnapshotTree{}
	return m.snapListCmd()
}

func (m *Model) snapListCmd() tea.Cmd {
	m.mode = modeBusy
	m.phase = fmt.Sprintf("Reading %s's snapshots…", nameOf(m.selected))
	m.genSnap++
	gen := m.genSnap
	backend := m.backend
	uuid := m.selected.UUID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), detailTimeout)
		defer cancel()
		tree, err := backend.ListSnapshots(ctx, uuid)
		return snapshotsMsg{gen: gen, uuid: uuid, tree: tree, err: err}
	}
}

func (m *Model) applySnapshots(msg snapshotsMsg) (tea.Model, tea.Cmd) {
	if msg.gen != m.genSnap {
		return m, nil
	}
	if msg.err != nil {
		return m, m.finish(OutcomeFailure,
			fmt.Sprintf("Could not read %s's snapshots: %s", quoteName(nameOf(m.selected)), oneError(msg.err, 400)))
	}
	m.snaps = msg.tree
	m.snapCursor = clamp(m.snapCursor, 0, max(len(m.snaps.Snapshots)-1, 0))
	m.mode = modeSnapList
	return m, nil
}

func (m *Model) handleSnapListKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	n := len(m.snaps.Snapshots)
	switch msg.String() {
	case "ctrl+c":
		return m, m.finish(OutcomeCancelled, "Cancelled. No snapshots were changed.")
	case "esc":
		m.note = ""
		m.snaps = vbox.SnapshotTree{}
		if m.snapFocused {
			// Focused `vbx snapshot [VM]`: the listing was the whole request.
			return m, m.finish(OutcomeSuccess,
				fmt.Sprintf("%s has %s.", quoteName(nameOf(m.selected)), countSnapshots(n)))
		}
		return m.reopenMenuForSelected()
	case "n":
		m.beginSnapTake()
		return m, nil
	case "up":
		if m.snapCursor > 0 {
			m.snapCursor--
		}
		return m, nil
	case "down":
		if n > 0 && m.snapCursor < n-1 {
			m.snapCursor++
		}
		return m, nil
	case "enter":
		if n == 0 {
			m.beginSnapTake()
			return m, nil
		}
		return m.beginSnapRestore()
	}
	return m, nil
}

func countSnapshots(n int) string {
	if n == 1 {
		return "1 snapshot"
	}
	return fmt.Sprintf("%d snapshots", n)
}

// ---------- tree rendering ----------

func (m *Model) snapLines() []string {
	if len(m.snaps.Snapshots) == 0 {
		return []string{m.styles.Muted.Render("no snapshots yet — press n to take the first one")}
	}
	out := []string{m.styles.Muted.Render("indented rows are children; (current) marks the point the machine derives from")}
	for i, s := range m.snaps.Snapshots {
		marker := "  "
		if i == m.snapCursor {
			marker = "> "
		}
		// Names and descriptions are external metadata like VM names:
		// encoded before styling and width maths, raw values kept for the
		// UUID-addressed operations behind them.
		label := strings.Repeat("  ", s.Depth) + safedisplay.Text(s.Name)
		if s.Current {
			label += "  (current)"
		}
		if s.Description != "" {
			label += " — " + truncateOneLine(safedisplay.Text(s.Description), 60)
		}
		if i == m.snapCursor {
			out = append(out, marker+m.styles.ActionSel.Render(label))
		} else {
			out = append(out, marker+label)
		}
	}
	return out
}

func truncateOneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ---------- take ----------

func (m *Model) beginSnapTake() {
	m.textPrompt = snapAskName
	m.textValue = ""
	m.textErr = ""
	m.mode = modeText
}

// snapshotNameTime is injectable so the test wording is deterministic.
var snapshotNameTime = time.Now

func defaultSnapshotName() string {
	return "vbx " + snapshotNameTime().Format("2006-01-02 15:04:05")
}

// doSnapTake runs a take through the busy door. An ordinary take continues
// into a fresh tree read; a protective take (protectFor set to the snapshot
// the restore targets) continues into the restore on success and aborts on
// failure.
func (m *Model) doSnapTake(name, description, protectFor string) tea.Cmd {
	m.mode = modeBusy
	m.phase = fmt.Sprintf("Snapshotting %s…", nameOf(m.selected))
	m.genSnap++
	gen := m.genSnap
	backend := m.backend
	uuid := m.selected.UUID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), actionTimeout)
		defer cancel()
		err := backend.TakeSnapshot(ctx, uuid, name, description)
		return snapOpDoneMsg{gen: gen, kind: "take", err: err, protectFor: protectFor}
	}
}

// ---------- restore ----------

// RestoreRefused is the shared door's verdict when the fresh read refuses
// the restore: it carries what changed since the request (the VM's new
// state, a vanished snapshot, a vanished machine) and says nothing was
// done. Everything RenameMachine-style guards report through it happened
// before any request left.
type RestoreRefused struct{ Why string }

func (e *RestoreRefused) Error() string { return e.Why }

// RestoreMachine is the single door every restore path must pass. It
// re-reads the machine by UUID, demands the powered-off state again, and
// demands the target snapshot is still in the tree — all immediately before
// backend.RestoreSnapshot, so no caller can reorder or skip the checks.
// The snapshot is addressed by the exact UUID the caller chose.
//
// The checks shrink the race window; they do not close it. VirtualBox's
// own hypervisor lock is what serialises a restore against an external
// start, and when that loses the race the restore attempt reports the
// hypervisor's refusal verbatim — this door cannot promise more than that,
// and neither can a caller.
func RestoreMachine(ctx context.Context, backend vbox.Manager, uuid, snapshotUUID string) (vbox.Machine, error) {
	if strings.TrimSpace(uuid) == "" {
		return vbox.Machine{}, &RestoreRefused{"refusing to restore a machine without a UUID"}
	}
	if strings.TrimSpace(snapshotUUID) == "" {
		return vbox.Machine{}, &RestoreRefused{"refusing to restore without the snapshot's UUID"}
	}
	inv, err := backend.List(ctx)
	if err != nil {
		return vbox.Machine{}, err
	}
	vm, ok := inv.Find(uuid)
	if !ok {
		return vbox.Machine{}, &RestoreRefused{"that machine is no longer registered with VirtualBox"}
	}
	switch vm.State.Kind {
	case vbox.KindOff, vbox.KindAborted:
	default:
		return vbox.Machine{}, &RestoreRefused{fmt.Sprintf(
			"%s is now %s, and a restore needs a powered-off machine — vbx will not discard a session or stop anything to restore",
			quoteName(nameOf(vm)), vm.State.Word())}
	}
	tree, err := backend.ListSnapshots(ctx, uuid)
	if err != nil {
		return vbox.Machine{}, err
	}
	if _, ok := tree.Find(snapshotUUID); !ok {
		return vbox.Machine{}, &RestoreRefused{"the chosen snapshot is no longer in the snapshot tree"}
	}
	if err := backend.RestoreSnapshot(ctx, uuid, snapshotUUID); err != nil {
		return vbox.Machine{}, err
	}
	return vm, nil
}

// protectiveIdentity decides, from a fresh tree read taken after a
// protective take reported success, whether a protective node really
// exists — and reports its name as VirtualBox stored it, never a timestamp
// recomputed here (VirtualBox may rename a colliding node on its own).
//
// The rule: after a successful take the new snapshot is the tree's current
// node (VirtualBox's own behaviour for powered-off machines). So the take
// is confirmed exactly when the current node exists and is neither the
// restore target nor the node that was already current before the take.
// Anything else — current unchanged, current missing, or current pointing
// at the target — means no protective node appeared, and the restore must
// abort instead of discarding the only copy on the strength of a claim.
func protectiveIdentity(tree vbox.SnapshotTree, targetUUID, preCurrentUUID string) (string, bool) {
	if tree.CurrentUUID == "" {
		return "", false
	}
	if !configUUIDNE(tree.CurrentUUID, targetUUID) {
		return "", false
	}
	if preCurrentUUID != "" && !configUUIDNE(tree.CurrentUUID, preCurrentUUID) {
		return "", false
	}
	if node, ok := tree.Find(tree.CurrentUUID); ok {
		return node.Name, true
	}
	return "", false
}

// beginSnapRestore re-verifies the machine state at the moment of intent:
// the tree was read when the machine may have been off, and state changes
// while a person reads.
func (m *Model) beginSnapRestore() (tea.Model, tea.Cmd) {
	chosen, ok := m.chosenSnapshot()
	if !ok {
		return m, nil
	}
	if m.snapFocused {
		// The focused command lists; restoring is an interactive journey.
		m.note = "Restoring is an interactive step: open vbx, pick the machine, choose Snapshots."
		return m, nil
	}
	m.restoreTarget = chosen
	m.mode = modeBusy
	m.phase = fmt.Sprintf("Checking %s's state…", nameOf(m.selected))
	m.genSnap++
	gen := m.genSnap
	backend := m.backend
	uuid := m.selected.UUID
	snapUUID := chosen.UUID
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), detailTimeout)
		defer cancel()
		inv, err := backend.List(ctx)
		if err != nil {
			return snapRestoreGateMsg{gen: gen, err: err}
		}
		vm, ok := inv.Find(uuid)
		if !ok {
			return snapRestoreGateMsg{gen: gen, err: fmt.Errorf("the machine is no longer registered with VirtualBox")}
		}
		return snapRestoreGateMsg{gen: gen, vm: vm, snapUUID: snapUUID}
	}
}

func (m *Model) applyRestoreGate(msg snapRestoreGateMsg) (tea.Model, tea.Cmd) {
	if msg.gen != m.genSnap || m.restoreTarget.UUID == "" {
		return m, nil
	}
	if msg.err != nil {
		m.restoreTarget = vbox.Snapshot{}
		return m, m.finish(OutcomeFailure,
			fmt.Sprintf("Cannot restore %s: %s. Nothing was changed.", quoteName(nameOf(m.selected)), oneError(msg.err, 300)))
	}
	if configUUIDNE(msg.snapUUID, m.restoreTarget.UUID) {
		m.restoreTarget = vbox.Snapshot{}
		return m, m.finish(OutcomeFailure, "The snapshot tree changed underneath this request. Re-open the list and choose again.")
	}
	switch msg.vm.State.Kind {
	case vbox.KindOff, vbox.KindAborted:
		// Good: restoring a powered-off machine discards only what changed
		// since the snapshot.
	default:
		// Refuse now, before anything is asked or sent. The wording tells
		// the operator exactly why, because the common belief that a
		// running machine can just roll back is the dangerous one.
		m.restoreTarget = vbox.Snapshot{}
		m.note = fmt.Sprintf("%s is %s, and a restore needs it powered off: restoring any other state first takes a saved copy of the live session, discarding it in the process.",
			quoteName(nameOf(msg.vm)), msg.vm.State.Word())
		m.snapCursor = clamp(m.snapCursor, 0, max(len(m.snaps.Snapshots)-1, 0))
		m.mode = modeSnapList
		return m, nil
	}
	m.restoreVM = msg.vm
	m.confirmSnapshot = true
	m.mode = modeConfirm
	return m, nil
}

// restoreConfirmQuestion words the restore gate. vmName arrives from nameOf
// (already encoded); the snapshot name and its UUID prefix pass through here
// so the confirmation text can never be steered by tree metadata — the
// question a user answers with a destructive 'y' must be exactly what the
// app decided, character for character.
func restoreConfirmQuestion(vmName string, snap vbox.Snapshot) string {
	return fmt.Sprintf("Restore %s to snapshot %q (%s…)?",
		vmName, safedisplay.Text(snap.Name), safedisplay.Text(snap.ShortID()))
}

func (m *Model) confirmSnapshotLines() []string {
	snap := m.restoreTarget
	out := []string{
		m.question(restoreConfirmQuestion(nameOf(m.restoreVM), snap)),
		m.styles.Muted.Render("everything done since this snapshot is discarded when it runs."),
	}
	if snap.Description != "" {
		out = append(out, m.styles.Muted.Render("description: "+truncateOneLine(safedisplay.Text(snap.Description), 100)))
	}
	out = append(out, m.styles.Muted.Render("y   save a protective snapshot first, then restore · s  restore without protecting · n / esc  cancel (nothing is changed)"))
	return out
}

// handleSnapshotConfirmKey answers the restore gate. y takes the protective
// snapshot first: if that take fails, the restore aborts, because the state
// about to be discarded would have no surviving copy.
func (m *Model) handleSnapshotConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		name := defaultSnapshotName() + " (protective)"
		return m, m.doSnapTake(name, "automatic protective snapshot before restoring an earlier snapshot", m.restoreTarget.UUID)
	case "s", "S":
		return m, m.doRestore("")
	case "n", "N", "esc", "ctrl+c":
		m.confirmSnapshot = false
		m.restoreTarget = vbox.Snapshot{}
		m.note = "Restore cancelled. Nothing was changed."
		m.snapCursor = clamp(m.snapCursor, 0, max(len(m.snaps.Snapshots)-1, 0))
		m.mode = modeSnapList
		return m, nil
	}
	return m, nil
}

// doRestore issues the restore through the shared RestoreMachine door, which
// re-reads the machine's state and the snapshot tree immediately before the
// request and addresses the snapshot by its exact UUID. protectName names
// the protective snapshot for the result line; empty means there was none.
func (m *Model) doRestore(protectName string) tea.Cmd {
	snap := m.restoreTarget
	m.mode = modeBusy
	m.phase = fmt.Sprintf("Restoring %s to %q…", nameOf(m.restoreVM), safedisplay.Text(snap.Name))
	// The door may still refuse from its fresh read (state moved, snapshot
	// vanished): nothing was issued then, and the result line will say so.
	m.opIssued = true
	m.genSnap++
	gen := m.genSnap
	backend := m.backend
	uuid := m.restoreVM.UUID
	snapUUID := snap.UUID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), actionTimeout)
		defer cancel()
		_, err := RestoreMachine(ctx, backend, uuid, snapUUID)
		return snapOpDoneMsg{gen: gen, kind: "restore", err: err, protectFor: protectName}
	}
}

// protectCheckMsg carries the protective-take aftermath: the protective
// node's name as it read back from the tree (identity, found=true when the
// evidence proves a new node became current), and whether the machine's
// fresh state still allows a restore (incompatible, non-empty when it does
// not). err means the tree or state could not be read at all.
type protectCheckMsg struct {
	gen          uint64
	err          error
	found        bool
	incompatible string
	identity     string
}

// checkProtective is the gate between a protective take and the restore.
// The take claimed success over the wire; before anything is discarded this
// re-reads the tree and proves — from the tree itself, not from a name this
// app hoped to see — that a new node exists and became current, and reports
// its actual stored name. It then re-reads the machine: a protective take
// seconds ago and a restore now are separate moments, and a machine another
// tool started in between is no longer restorable.
func (m *Model) checkProtective() tea.Cmd {
	m.mode = modeBusy
	m.phase = fmt.Sprintf("Confirming the protective snapshot for %s…", nameOf(m.restoreVM))
	m.genSnap++
	gen := m.genSnap
	backend := m.backend
	uuid := m.restoreVM.UUID
	target := m.restoreTarget.UUID
	preCurrent := m.snaps.CurrentUUID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), actionTimeout)
		defer cancel()
		tree, err := backend.ListSnapshots(ctx, uuid)
		if err != nil {
			return protectCheckMsg{gen: gen, err: err}
		}
		identity, found := protectiveIdentity(tree, target, preCurrent)
		inv, err := backend.List(ctx)
		if err != nil {
			return protectCheckMsg{gen: gen, err: err, found: found, identity: identity}
		}
		vm, ok := inv.Find(uuid)
		if !ok {
			return protectCheckMsg{gen: gen, found: found, identity: identity,
				incompatible: "the machine is no longer registered with VirtualBox"}
		}
		switch vm.State.Kind {
		case vbox.KindOff, vbox.KindAborted:
		default:
			return protectCheckMsg{gen: gen, found: found, identity: identity,
				incompatible: fmt.Sprintf("%s is now %s", quoteName(nameOf(vm)), vm.State.Word())}
		}
		return protectCheckMsg{gen: gen, found: found, identity: identity}
	}
}

func (m *Model) applyProtectCheck(msg protectCheckMsg) (tea.Model, tea.Cmd) {
	if msg.gen != m.genSnap || m.restoreTarget.UUID == "" {
		return m, nil
	}
	m.confirmSnapshot = false
	switch {
	case msg.err != nil:
		// The protective state of affairs is unknown: the restore must not
		// discard anything on the strength of an unconfirmed take.
		return m, m.finish(OutcomeFailure,
			"Restore aborted before discarding anything: the protective snapshot could not be confirmed: "+oneError(msg.err, 300)+
				". Nothing was restored.")
	case !msg.found:
		// The take answered success, yet the tree does not prove a new
		// protective node: an unproven copy cannot license a discard.
		return m, m.finish(OutcomeFailure,
			"Restore aborted: the protective snapshot did not appear in the machine's snapshot tree, "+
				"so the current state has no confirmed copy. Nothing was restored.")
	case msg.incompatible != "":
		return m, m.finish(OutcomeFailure, fmt.Sprintf(
			"Restore aborted: %s, and a restore needs it powered off. The state from just before is kept as snapshot %q; nothing was restored.",
			msg.incompatible, safedisplay.Text(msg.identity)))
	}
	return m, m.doRestore(msg.identity)
}

func (m *Model) applySnapOp(msg snapOpDoneMsg) (tea.Model, tea.Cmd) {
	if msg.gen != m.genSnap {
		return m, nil
	}
	switch msg.kind {
	case "take":
		if msg.protectFor != "" {
			// Protective take: success continues, failure aborts the whole
			// restore with nothing discarded.
			if msg.err != nil {
				m.confirmSnapshot = false
				m.restoreTarget = vbox.Snapshot{}
				return m, m.finish(OutcomeFailure,
					"The protective snapshot failed, so the restore was aborted: "+oneError(msg.err, 300)+
						". Nothing of the current state was discarded.")
			}
			// The take answered. Before the restore discards anything, the
			// protective node must show up in a fresh tree read — and its
			// identity comes from that read-back, never from a timestamp
			// recomputed here (VirtualBox may rename a colliding node).
			m.confirmSnapshot = false
			return m, m.checkProtective()
		}
		if msg.err != nil {
			m.snapCursor = clamp(m.snapCursor, 0, max(len(m.snaps.Snapshots)-1, 0))
			m.mode = modeSnapList
			m.note = "Snapshot failed: " + oneError(msg.err, 240)
			return m, nil
		}
		m.note = "Snapshot taken."
		return m, m.snapListCmd()
	case "restore":
		m.confirmSnapshot = false
		target := m.restoreTarget
		protect := msg.protectFor
		m.restoreTarget = vbox.Snapshot{}
		if msg.err != nil {
			// A door refusal means the fresh read stopped the request
			// before it left: nothing was discarded, and the line must
			// say so. Anything else is the hypervisor's own answer,
			// reported verbatim.
			var refused *RestoreRefused
			if errors.As(msg.err, &refused) {
				return m, m.finish(OutcomeFailure,
					fmt.Sprintf("Cannot restore %s: %s. Nothing was restored.",
						quoteName(nameOf(m.restoreVM)), oneError(msg.err, 400)))
			}
			return m, m.finish(OutcomeFailure,
				fmt.Sprintf("Could not restore %s: %s", quoteName(nameOf(m.restoreVM)), oneError(msg.err, 400)))
		}
		line := fmt.Sprintf("Restored %s to snapshot %q (%s…).",
			quoteName(nameOf(m.restoreVM)), safedisplay.Text(target.Name), safedisplay.Text(target.ShortID()))
		if protect != "" {
			line += fmt.Sprintf(" The state from just before was kept as snapshot %q.", safedisplay.Text(protect))
		}
		return m, m.finish(OutcomeSuccess, line)
	}
	return m, nil
}

func (m *Model) chosenSnapshot() (vbox.Snapshot, bool) {
	if len(m.snaps.Snapshots) == 0 {
		return vbox.Snapshot{}, false
	}
	return m.snaps.Snapshots[clamp(m.snapCursor, 0, len(m.snaps.Snapshots)-1)], true
}

// configUUIDNE compares UUIDs case-insensitively, tolerating braces.
func configUUIDNE(a, b string) bool {
	na := strings.ToLower(strings.Trim(strings.TrimSpace(a), "{}"))
	nb := strings.ToLower(strings.Trim(strings.TrimSpace(b), "{}"))
	return na != nb
}

// ---------- text prompts ----------

const (
	snapAskName        = "snapshot-name"
	snapAskDescription = "snapshot-description"
)

func (m *Model) textQuestion() string {
	switch m.textPrompt {
	case snapAskName:
		return fmt.Sprintf("Snapshot name for %s (Enter on an empty line gives up):", nameOf(m.selected))
	case snapAskDescription:
		return "Description, optional (Enter to continue):"
	case renameAskName:
		return renameQuestion(m.renameOld)
	}
	return ""
}

func (m *Model) handleTextKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// The rename flow owns its own keys: esc and an unchanged answer back
	// out honestly, rejected names keep what was typed. It is dispatched
	// first because its Enter handling never chains into a second prompt.
	if m.textPrompt == renameAskName {
		switch msg.String() {
		case "ctrl+c":
			return m.renameBack()
		case "esc":
			return m.renameBack()
		case "enter":
			return m.renameSubmit()
		case "backspace":
			r := []rune(m.textValue)
			if len(r) > 0 {
				m.textValue = string(r[:len(r)-1])
				m.textErr = ""
			}
			return m, nil
		}
		if msg.Type == tea.KeyRunes && !msg.Alt {
			m.textValue += msg.String()
			m.textErr = ""
		}
		return m, nil
	}
	switch msg.String() {
	case "ctrl+c":
		return m, m.finish(OutcomeCancelled, "Cancelled. Nothing was changed.")
	case "esc":
		m.textPrompt = ""
		m.textValue = ""
		m.textErr = ""
		m.snapCursor = clamp(m.snapCursor, 0, max(len(m.snaps.Snapshots)-1, 0))
		m.mode = modeSnapList
		return m, nil
	case "enter":
		switch m.textPrompt {
		case snapAskName:
			name := strings.TrimSpace(m.textValue)
			if name == "" {
				m.textPrompt = ""
				m.mode = modeSnapList
				return m, nil
			}
			if err := vbox.ValidateSnapshotName(name); err != nil {
				m.textErr = err.Error()
				return m, nil
			}
			m.snapName = name
			m.textValue = ""
			m.textErr = ""
			m.textPrompt = snapAskDescription
			return m, nil
		case snapAskDescription:
			desc := strings.TrimSpace(m.textValue)
			name := m.snapName
			m.snapName = ""
			m.textPrompt = ""
			m.textValue = ""
			return m, tea.Batch(m.doSnapTake(name, desc, ""), m.spinner.Tick)
		}
		return m, nil
	case "backspace":
		r := []rune(m.textValue)
		if len(r) > 0 {
			m.textValue = string(r[:len(r)-1])
			m.textErr = ""
		}
		return m, nil
	}
	if msg.Type == tea.KeyRunes && !msg.Alt {
		m.textValue += msg.String()
		m.textErr = ""
	}
	return m, nil
}
