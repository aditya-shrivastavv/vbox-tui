package app

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/aditya-shrivastavv/vbox-tui/internal/vbox"
)

// Styles is the compact prompt palette. It stays deliberately small: no
// borders, no panels — an understated CLI, not a dashboard. Colour never
// carries meaning on its own; every word is already text.
type Styles struct {
	// Question styles the leading "? " of each prompt.
	Question lipgloss.Style
	// Title styles the prompt text itself.
	Title lipgloss.Style
	// Muted styles hints, scroll markers and quiet context.
	Muted lipgloss.Style
	// Error styles failure lines and setup validation complaints.
	Error lipgloss.Style
	// Name styles a VM name in a row; NameSel the selected one.
	Name    lipgloss.Style
	NameSel lipgloss.Style
	// ActionSel styles the highlighted menu entry.
	ActionSel lipgloss.Style
	// StateCol styles the state column base; colour is picked per state.
	StateCol lipgloss.Style
	// Input styles the typed-text line of the SSH setup.
	Input lipgloss.Style

	stateOff     lipgloss.Style
	stateRunning lipgloss.Style
	statePaused  lipgloss.Style
	stateSaved   lipgloss.Style
	stateBusy    lipgloss.Style
	stateBad     lipgloss.Style
}

// DefaultStyles builds the palette from numbered ANSI colours, which Windows
// Terminal and the legacy console both map to the user's scheme.
func DefaultStyles() Styles {
	base := lipgloss.NewStyle()
	return Styles{
		Question:  base.Bold(true).Foreground(lipgloss.Color("39")),
		Title:     base.Bold(true),
		Muted:     base.Faint(true),
		Error:     base.Foreground(lipgloss.Color("196")),
		Name:      base,
		NameSel:   base.Bold(true),
		ActionSel: base.Bold(true),
		StateCol:  base.Faint(true),
		Input:     base,

		stateOff:     base.Faint(true),
		stateRunning: base.Foreground(lipgloss.Color("42")),
		statePaused:  base.Foreground(lipgloss.Color("214")),
		stateSaved:   base.Foreground(lipgloss.Color("39")),
		stateBusy:    base.Foreground(lipgloss.Color("220")),
		stateBad:     base.Foreground(lipgloss.Color("196")),
	}
}

// stateWord renders the state column entry for a machine.
func (s Styles) stateWord(info vbox.StateInfo) string {
	st := s.StateCol
	switch info.Kind {
	case vbox.KindOff, vbox.KindAborted:
		st = s.stateOff
	case vbox.KindRunning:
		st = s.stateRunning
	case vbox.KindPaused:
		st = s.statePaused
	case vbox.KindSaved:
		st = s.stateSaved
	case vbox.KindTransitional:
		st = s.stateBusy
	case vbox.KindUnavailable:
		st = s.stateBad
	}
	return st.Render(info.Word())
}
