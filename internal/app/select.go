package app

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/aditya-shrivastavv/vbox-tui/internal/config"
	"github.com/aditya-shrivastavv/vbox-tui/internal/vbox"
)

// Multi-select lives on the picker: space marks the highlighted machine, the
// count line says what is marked, and Enter acts on the marks when there are
// any. Identity is the canonical UUID, so filtering the list, a machine
// re-appearing after a state change, or a duplicate display name can never
// move a mark to the wrong machine.

func (m *Model) marked(uuid string) bool {
	return m.selectedSet[config.CanonicalUUID(uuid)]
}

func (m *Model) toggleMark(uuid string) {
	if m.selectedSet == nil {
		m.selectedSet = map[string]bool{}
	}
	key := config.CanonicalUUID(uuid)
	if m.selectedSet[key] {
		delete(m.selectedSet, key)
		return
	}
	m.selectedSet[key] = true
}

func (m *Model) markedCount() int { return len(m.selectedSet) }

// selectionSnapshot lists marked machines in current inventory order, so a
// batch runs in the order the user sees, never in the accident of marking
// order.
func (m *Model) selectionSnapshot() []string {
	var out []string
	for _, vm := range m.inv.Machines {
		if m.marked(vm.UUID) {
			out = append(out, config.CanonicalUUID(vm.UUID))
		}
	}
	return out
}

// selectionNames remembers, per canonical UUID, the name shown when the mark
// was made — the name to quote in the plan when the machine has since
// vanished from the inventory.
func (m *Model) selectionNames() map[string]string {
	out := map[string]string{}
	for _, vm := range m.inv.Machines {
		if m.marked(vm.UUID) {
			out[config.CanonicalUUID(vm.UUID)] = nameOf(vm)
		}
	}
	return out
}

// pruneSelection drops marks for machines the current inventory no longer
// contains: a mark that cannot resolve must be visible as its absence (the
// plan reports it), while marks kept across filter edits stay put. The
// pruning is of the set, not of names the inventory merely does not show.
func (m *Model) pruneSelection() {
	if len(m.selectedSet) == 0 {
		return
	}
	present := map[string]bool{}
	for _, vm := range m.inv.Machines {
		present[config.CanonicalUUID(vm.UUID)] = true
	}
	for k := range m.selectedSet {
		if !present[k] {
			delete(m.selectedSet, k)
		}
	}
}

// selectionLine renders the count row under the picker: what is marked, and
// how many of the marked rows are outside the current scroll window, so a
// filtered or scrolled list never hides part of a plan.
func (m *Model) selectionLine() string {
	n := m.markedCount()
	if n == 0 {
		return m.styles.Muted.Render("space marks machines for a batch action · enter opens a machine")
	}
	hidden := 0
	visible := map[string]bool{}
	rows := m.selectable()
	for i := m.listTop; i < min(m.listTop+min(visibleRows, len(rows)), len(rows)); i++ {
		visible[config.CanonicalUUID(m.inv.Machines[rows[i]].UUID)] = true
	}
	for uuid := range m.selectedSet {
		if !visible[uuid] {
			hidden++
		}
	}
	line := fmt.Sprintf("%d marked", n)
	if hidden > 0 {
		line += fmt.Sprintf(" (%d hidden by filter or scroll)", hidden)
	}
	return m.styles.Muted.Render(line + " — enter runs an action on the marked machines")
}

// handlePickerSpace is the picker's space binding: mark or unmark the
// highlighted row. It runs before filtering sees the space so typed filters
// containing a space still work after the mark (the mark consumes only the
// first space at an unmarked row).
func (m *Model) handlePickerSpace() (tea.Model, tea.Cmd) {
	rows := m.selectable()
	if len(rows) == 0 {
		return m, nil
	}
	vm := m.inv.Machines[rows[clamp(m.cursor, 0, len(rows)-1)]]
	m.toggleMark(vm.UUID)
	return m, nil
}

// pickerEnter acts on marks when there are any: an actionable verb is asked
// for; nothing is decided about machines here, only what happens next.
// pickerEnterWithMarks is Enter with at least one mark: the marked machines
// become the batch target set. With a focused verb (`vbx start` with the
// picker open) that verb is used directly; otherwise the operator picks one
// of the batchable actions. Enter with no marks keeps the normal
// open-a-machine behaviour, which is why the caller checks first.
func (m *Model) pickerEnterWithMarks() (tea.Model, tea.Cmd) {
	m.filter = ""
	if action := m.focusAction; action != "" {
		if !action.Batchable() {
			// A single-machine journey (`vbx ssh`, `vbx config`, `vbx
			// snapshot`) with marks left over: honour the highlighted row
			// rather than invent a batch of something that has no batch form.
			return m.pickVM()
		}
		return m, m.requestBatchPlan(action)
	}
	m.batchChoiceReturn = modePickVM
	m.chooseKind = chooseBatchVerb
	m.chooseTitle = "Run which action on the marked machines?"
	m.chooseCursor = 0
	m.mode = modeChoose
	return m, nil
}

// chooseKind selects which generic-chooser list modeChoose renders; each step
// of the guest editor owns its own keys and fields, so modeChoose stays a
// small switch instead of a framework.
type chooseKind int

const (
	chooseGuest chooseKind = iota
	chooseBatchVerb
)

func (m *Model) handleChooseKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.chooseKind {
	case chooseBatchVerb:
		return m.handleBatchVerbKey(msg)
	default:
		return m.handleGuestEditKey(msg)
	}
}

func (m *Model) handleBatchVerbKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	opts := vbox.LifecycleActions
	switch msg.String() {
	case "esc":
		m.mode = m.batchChoiceReturn
		m.reconcileCursor()
		return m, nil
	case "up", "k":
		if m.chooseCursor > 0 {
			m.chooseCursor--
		}
		return m, nil
	case "down", "j":
		if m.chooseCursor < len(opts)-1 {
			m.chooseCursor++
		}
		return m, nil
	case "enter":
		action := opts[clamp(m.chooseCursor, 0, len(opts)-1)]
		return m, m.requestBatchPlan(action)
	}
	return m, nil
}

func (m *Model) chooseVerbLines() []string {
	out := []string{m.question(m.chooseTitle)}
	for i, a := range vbox.LifecycleActions {
		out = append(out, m.chooseMarker(i, m.chooseCursor, a.Label()))
	}
	out = append(out, m.styles.Muted.Render("one action, run machine by machine in list order"))
	return out
}
