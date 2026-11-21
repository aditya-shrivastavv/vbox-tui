package app

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/aditya-shrivastavv/vbox-tui/internal/config"
	"github.com/aditya-shrivastavv/vbox-tui/internal/safedisplay"
	"github.com/aditya-shrivastavv/vbox-tui/internal/vbox"
)

// selectable returns the inventory indices the picker currently offers, in
// order: filtered by the typed text and, for a direct command, restricted to
// machines whose current state can serve that action. Callers index into this
// slice and map back through it, so a filter or a restriction can never point
// at the wrong machine.
func (m *Model) selectable() []int {
	needle := strings.ToLower(strings.TrimSpace(m.filter))
	var idx []int
	for i, vm := range m.inv.Machines {
		// A direct command lists only machines it could actually serve right
		// now: the same verb decision the run itself will make, so a focused
		// picker never contains a machine that would fail when chosen. The
		// app-level focus actions (settings, snapshots) serve any machine.
		if m.focusAction != "" && m.focusAction != ActionSettings && m.focusAction != ActionSnapshots {
			if verb, _ := verbForAction(m.focusAction, vm.State); verb == VerbRefused {
				continue
			}
		}
		if needle != "" && !vmMatches(vm, needle) {
			continue
		}
		idx = append(idx, i)
	}
	return idx
}

// vmMatches filters over name, state and UUID so "kali", "saved" and "2d62"
// all find the row they mean. The haystack is the full stored name: filtering
// never reads or mutates a shortened form of it.
func vmMatches(vm vbox.Machine, needle string) bool {
	hay := strings.ToLower(strings.Join([]string{vm.Name, string(vm.State.State), vm.State.Word(), vm.UUID}, " "))
	return strings.Contains(hay, needle)
}

// clampCursor keeps the cursor inside the selectable set after any change.
func (m *Model) clampCursor() {
	n := len(m.selectable())
	if n == 0 {
		m.cursor = 0
		return
	}
	m.cursor = clamp(m.cursor, 0, n-1)
}

// reconcileCursor re-establishes the cursor after the selectable set changes
// (a filter edit, or a machine returning from a blocked state): keep the same
// machine when it is still present, otherwise stay near the same position.
func (m *Model) reconcileCursor() {
	rows := m.selectable()
	if len(rows) == 0 {
		m.cursor = 0
		m.listTop = 0
		return
	}
	if m.selected.UUID != "" {
		for i, inv := range rows {
			if config.CanonicalUUID(m.inv.Machines[inv].UUID) == config.CanonicalUUID(m.selected.UUID) {
				m.cursor = i
				m.clampListTop()
				return
			}
		}
	}
	m.cursor = clamp(m.cursor, 0, len(rows)-1)
	m.clampListTop()
}

// clampListTop keeps the scroll window around the cursor.
func (m *Model) clampListTop() {
	n := len(m.selectable())
	visible := min(visibleRows, n)
	if visible <= 0 {
		m.listTop = 0
		return
	}
	if m.cursor < m.listTop {
		m.listTop = m.cursor
	}
	if m.cursor >= m.listTop+visible {
		m.listTop = m.cursor - visible + 1
	}
	m.listTop = clamp(m.listTop, 0, n-visible)
}

// vmLines renders one screenful of picker rows plus scroll markers. Names lead
// every row; state words sit in one subdued column two cells after the widest
// name on screen. Nothing else decorates a row — no [ssh] suffixes, no
// badges — and the frame is exactly as tall as its content.
func (m *Model) vmLines() []string {
	rows := m.selectable()
	if len(rows) == 0 {
		return []string{m.styles.Muted.Render("no machines match the filter — backspace edits, esc clears")}
	}
	m.clampCursor()
	m.clampListTop()
	visible := min(visibleRows, len(rows))
	start := m.listTop
	end := min(start+visible, len(rows))
	cursorInv := rows[clamp(m.cursor, 0, len(rows)-1)]

	var window []vbox.Machine
	for i := start; i < end; i++ {
		window = append(window, m.inv.Machines[rows[i]])
	}

	var out []string
	if start > 0 {
		out = append(out, m.styles.Muted.Render(fmt.Sprintf("  ↑ %d more above", start)))
	}
	for i, vm := range window {
		isCursor := rows[start+i] == cursorInv
		out = append(out, m.vmLine(vm, isCursor, window))
	}
	if below := len(rows) - end; below > 0 {
		out = append(out, m.styles.Muted.Render(fmt.Sprintf("  ↓ %d more below", below)))
	}
	return out
}

// nameColumnWidth is the aligned name column: the widest name in the visible
// window, shrunk only when the terminal truly cannot accommodate it alongside
// a state word.
func (m *Model) nameColumnWidth(window []vbox.Machine) int {
	nameW := 0
	for _, vm := range window {
		if w := ansi.StringWidth(displayName(vm)); w > nameW {
			nameW = w
		}
	}
	// Budget: marker (3) + name column + gap (2) + state word.
	const markerW, gapW, stateW = 3, 2, 12
	if nameW > m.width-markerW-gapW-stateW {
		nameW = max(m.width-markerW-gapW-stateW, 4)
	}
	return nameW
}

// displayName is what the row shows: the stored name as VirtualBox reports
// it, or — for the single case where there genuinely is no name — the UUID
// (VirtualBox lists inaccessible machines by UUID). Names are never
// shortened, trimmed or normalised for layout; the truncation below is only
// ever visual, always marked, and only when the row cannot fit.
//
// The returned string is safe-display encoded: width and alignment computed
// from it measure exactly what the terminal will see, so a hostile name can
// neither move the cursor nor fake a row while still keeping the column
// honest. Resolution keeps using the raw Machine.Name elsewhere.
func displayName(vm vbox.Machine) string {
	return safedisplay.Text(rawDisplayName(vm))
}

// rawDisplayName is displayName without the display encoding, for the exact
// comparisons resolution and duplicate detection rely on.
func rawDisplayName(vm vbox.Machine) string {
	if strings.TrimSpace(vm.Name) != "" {
		return vm.Name
	}
	return vm.UUID
}

func (m *Model) vmLine(vm vbox.Machine, selected bool, window []vbox.Machine) string {
	// Every marker is exactly three cells so the name column stays aligned
	// whether a row is plain, marked, or both. A marked row keeps its own
	// column: [x] stays put while the > cursor moves, so every member of a
	// pending plan is visible even unhighlighted.
	marker := "   "
	nameSt := m.styles.Name
	switch {
	case m.marked(vm.UUID) && selected:
		marker = ">x "
		nameSt = m.styles.NameSel
	case m.marked(vm.UUID):
		marker = "[x]"
		nameSt = m.styles.NameSel
	case selected:
		marker = ">  "
		nameSt = m.styles.NameSel
	}
	nameCol := m.nameColumnWidth(window)
	shown := displayName(vm)
	if ansi.StringWidth(shown) > nameCol {
		shown = ellip(shown, nameCol)
	}
	pad := strings.Repeat(" ", max(nameCol-ansi.StringWidth(shown), 2))
	return marker + nameSt.Render(shown) + pad + m.styles.stateWord(vm.State)
}

// actionLines renders the state-aware action menu.
func (m *Model) actionLines() []string {
	var out []string
	for i, a := range m.actions {
		marker := "  "
		if i == m.actCursor {
			marker = "> "
		}
		label := menuLabel(m.selected.State, a)
		if i == m.actCursor {
			out = append(out, marker+m.styles.ActionSel.Render(label))
		} else {
			out = append(out, marker+label)
		}
	}
	return out
}

// setupLines renders the current SSH setup step: one question, one answer
// line, optionally a detected-NAT hint. One small prompt, never a form panel.
func (m *Model) setupLines() []string {
	var out []string
	out = append(out, m.question(m.setupQuestion()))
	switch m.setupStep {
	case 0:
		out = append(out, m.setupOptions()...)
	default:
		// The echoed answer is encoded like any external text: typed
		// control characters (pasted, for instance) must not steer the
		// terminal from their own echo line.
		out = append(out, "> "+safedisplay.Text(m.setupBuf))
	}
	if hint := m.setupHint(); hint != "" {
		out = append(out, m.styles.Muted.Render(hint))
	}
	if m.setupErr != "" {
		out = append(out, m.styles.Error.Render("! "+m.setupErr))
	}
	return out
}

// setupQuestion words the current SSH setup step.
func (m *Model) setupQuestion() string {
	switch m.setupStep {
	case 0:
		return fmt.Sprintf("How should %s be reached over SSH?", nameOf(m.selected))
	case 1:
		if m.setupMode == config.SSHModeAlias {
			return "SSH config alias (an entry from ~/.ssh/config):"
		}
		return "Host or address (often 127.0.0.1 for a NAT-forwarded guest):"
	case 2:
		return fmt.Sprintf("Port on %s (Enter accepts the suggested port):", m.setupHost)
	case 3:
		return "SSH username (Enter accepts your ssh default):"
	}
	return ""
}

// setupOptions renders the two-mode chooser.
func (m *Model) setupOptions() []string {
	type opt struct {
		mode config.SSHMode
		text string
	}
	opts := []opt{
		{config.SSHModeEndpoint, "Host and port (e.g. 127.0.0.1, port 2222)"},
		{config.SSHModeAlias, "SSH alias (an entry from ~/.ssh/config)"},
	}
	current := m.setupMode
	if current == "" {
		current = config.SSHModeEndpoint // up/down starts from the first option
	}
	var out []string
	for _, o := range opts {
		if o.mode == current {
			out = append(out, "> "+m.styles.ActionSel.Render(o.text))
		} else {
			out = append(out, "  "+o.text)
		}
	}
	return out
}

// setupHint offers an existing NAT forwarding endpoint when VirtualBox reports
// one. Suggestions only — vbox-tui never edits VM configuration.
func (m *Model) setupHint() string {
	if m.setupStep == 1 && m.setupMode != config.SSHModeAlias && m.detail != nil {
		if f, ok := m.detail.NATForwardFor(22); ok && f.HostPort > 0 {
			host := strings.TrimSpace(f.HostIP)
			if host == "" {
				host = "127.0.0.1"
			}
			return fmt.Sprintf("VirtualBox already forwards %s:%d to this guest's ssh port — host %s, port %d",
				host, f.HostPort, host, f.HostPort)
		}
	}
	return ""
}
