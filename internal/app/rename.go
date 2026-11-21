package app

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/aditya-shrivastavv/vbox-tui/internal/config"
	"github.com/aditya-shrivastavv/vbox-tui/internal/safedisplay"
	"github.com/aditya-shrivastavv/vbox-tui/internal/vbox"
)

// Renaming changes one machine's display name — nothing else. It is kept
// single-VM on purpose: a name change is cheap to do and cheap to undo, but
// a batch of them is easy to regret. The flow:
//
//   - the machine is targeted by UUID; the state is re-read at the moment of
//     the request and must be powered off, because VirtualBox itself renames
//     only a powered-off machine. vbx never stops, restarts, unregisters,
//     clones, or moves anything to make a rename possible;
//   - the new name is validated before it is sent (ValidateVMName) and the
//     name is never created twice on purpose: an existing machine with that
//     exact name stops the request with an explicit refusal. VirtualBox
//     honours duplicate names only as an accident of stale registrations,
//     and vbx never adds to that pile;
//   - SSH/restart settings survive untouched because they are stored by
//     UUID and the rename does not change one;
//   - the stored display-name hint is cosmetic refresh only: if VirtualBox
//     renamed the machine but the hint write fails, the result line says so
//     honestly instead of pretending the bookkeeping succeeded.

// ActionRename aliases the vbox action so the menus reference it locally.
const ActionRename = vbox.ActionRename

// ---------- shared door ----------

// RenameRefused reports a state that will not accept a rename, with the
// reason — the hypervisor's rule (powered off only) surfaced before the
// hypervisor is asked.
type RenameRefused struct{ Why string }

func (e *RenameRefused) Error() string { return e.Why }

// RenameNameTaken reports another registered machine already using the
// proposed name. Explicit policy: vbx does not knowingly create a second
// machine with the same name, because everything name-addressed would go
// ambiguous from that day on.
type RenameNameTaken struct{ Name string }

func (e *RenameNameTaken) Error() string {
	return fmt.Sprintf("another machine is already named %q; every name-addressed command would be ambiguous afterwards", e.Name)
}

// RenameOutcome is the verdict of the shared rename door.
type RenameOutcome struct {
	VM   vbox.Machine // the freshly-read machine after the rename
	Old  string       // the name it had when the request was made
	New  string       // the name it carries now (or was already carrying)
	Same bool         // the machine already had the wanted name: nothing was sent
}

// RenameMachine performs one rename through the single set of checks every
// path (interactive prompt, direct command, future callers) must pass:
// fresh read by UUID, powered-off state, valid and unique name, then exactly
// one modifyvm request addressed by UUID. It never performs any other
// operation, and a refusal means nothing was sent.
func RenameMachine(ctx context.Context, backend vbox.Manager, uuid, newName string) (RenameOutcome, error) {
	out := RenameOutcome{}
	if strings.TrimSpace(uuid) == "" {
		return out, fmt.Errorf("refusing to rename a machine without a UUID")
	}
	if err := vbox.ValidateVMName(newName); err != nil {
		return out, err
	}
	inv, err := backend.List(ctx)
	if err != nil {
		return out, err
	}
	vm, ok := inv.Find(uuid)
	if !ok {
		return out, fmt.Errorf("that machine is no longer registered with VirtualBox (UUID %s)", uuid)
	}
	out.Old = vm.Name
	if verb, why := DecideVerb(ActionRename, vm.State); verb == VerbRefused {
		return out, &RenameRefused{Why: why}
	}
	if strings.EqualFold(strings.TrimSpace(vm.Name), strings.TrimSpace(newName)) {
		// Identity is case-exact for names, but a rename that only changes
		// case still goes through VirtualBox like any other — EqualFold is
		// used only to catch "the user pressed enter on the prefilled
		// name", where the exact string is identical below.
		if vm.Name == newName {
			out.VM, out.New, out.Same = vm, vm.Name, true
			return out, nil
		}
	}
	// The requested name belongs to no other machine. (Two machines sharing
	// the OLD name is fine for a rename: the target is a UUID.)
	for _, other := range inv.Machines {
		if other.UUID != vm.UUID && other.Name == newName {
			return out, &RenameNameTaken{Name: newName}
		}
	}
	if err := backend.RenameVM(ctx, vm.UUID, newName); err != nil {
		return out, err
	}
	fresh := vm
	fresh.Name = newName
	// Best effort: pick up exactly what VirtualBox now reports (it trims and
	// may adjust the name). A failed re-read must not rewrite the verdict.
	if inv2, err := backend.List(ctx); err == nil {
		if m2, ok := inv2.Find(uuid); ok {
			fresh = m2
		}
	}
	out.VM = fresh
	out.New = fresh.Name
	return out, nil
}

// ---------- display name hint ----------

// nameCache refreshes the cosmetic display-name hint stored with one VM's
// settings. A rename that never had a settings entry updates nothing; the
// name hint is bookkeeping that lives inside real settings.
type nameCache struct {
	cfg     *config.Config
	persist bool
}

// set attempts the hint refresh and reports (attempted, failure). A machine
// with no stored entry has no hint to refresh: that returns attempted=false
// with no error and never touches the file. When an entry exists but the
// write refuses (a quarantined settings file, a concurrent writer), the
// attempted flag is true and the failure is the caller's to report: the
// rename itself already happened, and hiding that mismatch would be the
// dishonest part.
func (n nameCache) set(uuid, name string) (bool, error) {
	if n.cfg == nil {
		return false, nil
	}
	if _, stored := n.cfg.VM(uuid); !stored {
		return false, nil
	}
	if !n.persist {
		return n.cfg.SetName(uuid, name)
	}
	err := n.cfg.Edit(func(c *config.Config) error {
		_, err := c.SetName(uuid, name)
		return err
	})
	return true, err
}

// RefreshNameHint updates the cosmetic display-name hint stored under one
// UUID. The CLI's scripted path (`vbx rename "Old" "New"`) uses it after a
// successful rename; the interactive model keeps the same door through
// nameCache. A machine without stored settings has no hint to keep current,
// which is not an error; a refused write (a quarantined settings file, say)
// is, and its caller must report the partial success honestly.
func RefreshNameHint(cfg *config.Config, persist bool, uuid, name string) error {
	_, err := nameCache{cfg: cfg, persist: persist}.set(uuid, name)
	return err
}

// ---------- interactive flow ----------

const renameAskName = "rename-name"

// beginFocusedRename starts the rename journey for `vbx rename [VM]`:
// prefilled prompt, run exits when the answer (or the cancellation) lands.
// With a second name on the command line the prompt is skipped — the
// command line already answered it. Either way the machine's current state
// gets a look first: a running machine cannot be renamed by anything vbx
// would send, and saying so here beats asking a person to type a new name
// for a request VirtualBox would refuse. (The door re-verifies anyway.)
func (m *Model) beginFocusedRename() tea.Cmd {
	m.renameFocused = true
	if refusal := renameStateRefusal(m.selected.State); refusal != "" {
		return m.finish(OutcomeFailure, fmt.Sprintf("Cannot rename %s: %s. Nothing was changed.",
			quoteName(nameOf(m.selected)), refusal))
	}
	if newName := strings.TrimSpace(m.renameTo); newName != "" {
		return tea.Batch(m.doRename(m.renameTo), m.spinner.Tick)
	}
	m.beginRename(m.selected, true, false)
	return nil
}

// renameStateRefusal words the powered-off rule for a focused run; empty
// means the state would accept a rename.
func renameStateRefusal(info vbox.StateInfo) string {
	verb, why := DecideVerb(ActionRename, info)
	if verb != VerbRefused {
		return ""
	}
	return why
}

func (m *Model) beginRename(target vbox.Machine, focused, fromMenu bool) {
	m.renameOld = target.Name
	m.renameFocused = focused
	m.renameFromMenu = fromMenu
	m.textPrompt = renameAskName
	m.textValue = target.Name
	m.textErr = ""
	m.mode = modeText
}

// renameQuestion words the prompt; the current name is in the edit line, so
// the question says what Enter on an unchanged line means.
func renameQuestion(current string) string {
	return fmt.Sprintf("New name for %s — edit and press enter:", quoteName(safedisplay.Text(current)))
}

// renameBack leaves the rename flow: a focused `vbx rename` ends the run;
// from the action menu it steps back with fresh state.
func (m *Model) renameBack() (tea.Model, tea.Cmd) {
	m.textPrompt = ""
	m.textValue = ""
	m.textErr = ""
	if m.renameFocused {
		return m, m.finish(OutcomeCancelled, "Cancelled. Nothing was changed.")
	}
	return m.reopenMenuForSelected()
}

// renameSubmit answers Enter in the rename prompt. Rejected input keeps
// exactly what was typed — a rejected name is a question, not a loss.
func (m *Model) renameSubmit() (tea.Model, tea.Cmd) {
	value := m.textValue
	if strings.TrimSpace(value) == "" {
		m.textErr = "a VM needs a name — type one, or press esc to leave it alone"
		return m, nil
	}
	if err := vbox.ValidateVMName(value); err != nil {
		m.textErr = err.Error()
		return m, nil
	}
	if value == m.renameOld {
		// The prefilled name, unchanged: nothing to send.
		m.textPrompt = ""
		m.textValue = ""
		note := fmt.Sprintf("%s keeps its name: nothing was changed.", quoteName(safedisplay.Text(m.renameOld)))
		if m.renameFocused {
			return m, m.finish(OutcomeSuccess, note)
		}
		m.note = note
		return m.reopenMenuForSelected()
	}
	return m, m.doRename(value)
}

// renameDoneMsg carries the rename answer from the command goroutine.
type renameDoneMsg struct {
	gen     uint64
	out     RenameOutcome
	err     error
	sent    bool // the request was handed to the backend
	focused bool
}

func (m *Model) doRename(newName string) tea.Cmd {
	m.mode = modeBusy
	m.phase = fmt.Sprintf("Renaming %s…", nameOf(m.selected))
	m.genRename++
	gen := m.genRename
	live := &atomic.Bool{}
	m.opLive = live
	backend := m.backend
	uuid := m.selected.UUID
	focused := m.renameFocused
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), actionTimeout)
		defer cancel()
		// The door re-reads and re-verifies at the moment of the request, so
		// a machine another tool started while this prompt was open gets a
		// refusal here, never a doomed modifyvm. From here a request may be
		// in flight: honest cancellation wording must assume it left.
		live.Store(true)
		out, err := RenameMachine(ctx, backend, uuid, newName)
		return renameDoneMsg{gen: gen, out: out, err: err, sent: err == nil && !out.Same, focused: focused}
	}
}

func (m *Model) applyRename(msg renameDoneMsg) (tea.Model, tea.Cmd) {
	if msg.gen != m.genRename {
		return m, nil
	}
	// The name columns of every line come from the door's fresh read
	// (out.Old/out.New) where it got that far; a refusal before the read
	// can only name what the app had when the request left.
	name := quoteName(nameOf(m.selected))
	if msg.err != nil {
		// Everything RenameMachine refuses happens before a request leaves:
		// the refusal is always a complete truth.
		line := fmt.Sprintf("Cannot rename %s: %s. Nothing was changed.", name, oneError(msg.err, 300))
		if msg.focused {
			return m, m.finish(OutcomeFailure, line)
		}
		m.textPrompt = ""
		m.textValue = ""
		return m.reopenMenuWithNote(line)
	}
	oldName := quoteName(safedisplay.Text(msg.out.Old))
	if msg.out.Same {
		// Defensively: the name matched exactly on the fresh read inside the
		// door. Nothing needed sending.
		line := fmt.Sprintf("%s is already named %s: nothing was changed.",
			oldName, quoteName(safedisplay.Text(msg.out.New)))
		if msg.focused {
			return m, m.finish(OutcomeSuccess, line)
		}
		m.textPrompt = ""
		m.textValue = ""
		return m.reopenMenuWithNote(line)
	}
	// The rename succeeded. Refresh the model's view and the cosmetic hint.
	m.selected = msg.out.VM
	m.textPrompt = ""
	m.textValue = ""
	line := fmt.Sprintf("Renamed %s to %s.", oldName, quoteName(safedisplay.Text(msg.out.New)))
	if attempted, err := m.names.set(msg.out.VM.UUID, msg.out.New); attempted && err != nil {
		// VirtualBox did its part; vbx's own bookkeeping did not. Say so.
		line += fmt.Sprintf(" vbx renamed the machine, but could not refresh its stored display-name hint: %s", oneError(err, 200))
	}
	if msg.focused {
		return m, m.finish(OutcomeSuccess, line)
	}
	m.note = line
	return m.reopenMenuForSelected()
}

// reopenMenuWithNote reopens the action menu carrying a result line visible
// above it — the rename answer stays in the same prompt the user left.
func (m *Model) reopenMenuWithNote(note string) (tea.Model, tea.Cmd) {
	m.note = ""
	cmd := m.readMenu(m.selected.UUID)
	m.note = note
	return m, m.ticked(cmd)
}
