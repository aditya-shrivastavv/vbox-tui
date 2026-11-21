package app

import (
	"fmt"
	"strings"

	"github.com/aditya-shrivastavv/vbox-tui/internal/safedisplay"
	"github.com/aditya-shrivastavv/vbox-tui/internal/vbox"
)

// View renders the current prompt purely from state. Nothing here mutates the
// model or starts a command.
//
// The contract with the terminal is as deliberate as the code:
//   - the frame is content-sized, one line per element — never padded to the
//     terminal height, because this app owns a prompt, not a screen;
//   - the caller runs it without tea.WithAltScreen, so nothing clears the
//     scrollback and previous output simply stays above it.
func (m *Model) View() string {
	var lines []string
	switch m.mode {
	case modeLoading:
		lines = []string{
			m.question("Select a virtual machine"),
			m.styles.Muted.Render("reading VirtualBox…"),
		}
	case modePickVM:
		lines = append(lines, m.question("Select a virtual machine"))
		if m.note != "" {
			lines = append(lines, m.styles.Muted.Render(m.note))
		}
		if m.filter != "" {
			// Echoed keystrokes go through the same encoder as machine
			// metadata: a pasted escape sequence must not run from the
			// filter line.
			lines = append(lines, m.styles.Muted.Render("filter: "+safedisplay.Text(m.filter)+"  (backspace edits, esc clears)"))
		}
		lines = append(lines, m.vmLines()...)
		lines = append(lines, m.selectionLine())
		lines = append(lines, m.pickHints())
	case modePickAction:
		lines = append(lines, m.question(fmt.Sprintf("%s — choose an action", truncateForPrompt(nameOf(m.selected), m.width))))
		if m.note != "" {
			lines = append(lines, m.styles.Muted.Render(m.note))
		}
		lines = append(lines, m.actionLines()...)
		lines = append(lines, m.actionHints())
	case modeConfirm:
		switch {
		case m.confirmSnapshot:
			lines = append(lines, m.confirmSnapshotLines()...)
		case m.batchAction != "":
			lines = append(lines, m.batchConfirmLines()...)
		case m.resetScope != "":
			lines = append(lines, m.question(m.resetQuestion()))
			lines = append(lines, m.styles.Muted.Render("y   confirm · n / esc  cancel (the VM itself is never touched)"))
		default:
			lines = append(lines, m.question(confirmQuestion(m.confirmAction, nameOf(m.selected))))
			lines = append(lines, m.styles.Muted.Render("y   confirm · n / esc  cancel (nothing is sent to VirtualBox)"))
		}
	case modeSSHSetup:
		lines = append(lines, m.setupLines()...)
		lines = append(lines, m.styles.Muted.Render("enter next · esc back"))
	case modeBusy:
		lines = []string{m.spinner.View() + " " + m.phase}
		if m.note != "" {
			lines = append(lines, m.styles.Muted.Render(m.note))
		}
		lines = append(lines, m.styles.Muted.Render("esc cancel"))
	case modeWaitSSH:
		total := int(m.waitTimeout / 1_000_000_000) // seconds, kept integer-simple
		lines = []string{fmt.Sprintf("%s Waiting for SSH on %s… %ds / %ds  (esc cancels)",
			m.spinner.View(), safedisplay.Text(m.waitAddr), m.waitElapsed, total)}
		if m.note != "" {
			lines = append(lines, m.styles.Muted.Render(m.note))
		}
	case modeBatchRun:
		lines = []string{m.spinner.View() + " " + m.phase}
		lines = append(lines, m.batchLines()...)
		if m.note != "" {
			lines = append(lines, m.styles.Muted.Render(m.note))
		}
		if !m.batchCancelReq {
			lines = append(lines, m.styles.Muted.Render("esc stops after the machine now running"))
		}
	case modeSettings:
		lines = append(lines, m.question(fmt.Sprintf("%s — settings", truncateForPrompt(nameOf(m.selected), m.width))))
		lines = append(lines, m.styles.Muted.Render(m.settingsSummary()))
		lines = append(lines, m.styles.Muted.Render(m.guestSummary()))
		if m.note != "" {
			lines = append(lines, m.styles.Muted.Render(m.note))
		}
		lines = append(lines, m.settingsLines()...)
		lines = append(lines, m.styles.Muted.Render("esc back"))
	case modeChoose:
		switch m.chooseKind {
		case chooseBatchVerb:
			lines = append(lines, m.chooseVerbLines()...)
		default:
			lines = append(lines, m.guestEditLines()...)
		}
		lines = append(lines, m.styles.Muted.Render("↑/↓ choose · enter next · esc back"))
	case modeText:
		lines = append(lines, m.question(m.textQuestion()))
		lines = append(lines, "> "+safedisplay.Text(m.textValue))
		if m.textErr != "" {
			lines = append(lines, m.styles.Error.Render("! "+m.textErr))
		}
		lines = append(lines, m.styles.Muted.Render("enter continue · esc back"))
	case modeSnapList:
		vmName := nameOf(m.restoreVM)
		if m.restoreVM.UUID == "" {
			vmName = nameOf(m.selected)
		}
		lines = append(lines, m.question(fmt.Sprintf("%s — snapshots", truncateForPrompt(vmName, m.width))))
		if m.note != "" {
			lines = append(lines, m.styles.Muted.Render(m.note))
		}
		lines = append(lines, m.snapLines()...)
		if len(m.snaps.Snapshots) == 0 {
			lines = append(lines, m.styles.Muted.Render("n new snapshot · esc back"))
		} else {
			lines = append(lines, m.styles.Muted.Render("n new snapshot · enter restore chosen (needs the machine powered off) · esc back"))
		}
	case modeDone:
		// One frame between the decision and the renderer shutting down. The
		// result line itself is printed by main once this frame is erased.
		return ""
	}
	// Content-sized, and each line kept within the terminal width so nothing
	// wraps into the next prompt.
	for i, l := range lines {
		lines[i] = ellip(l, m.width)
	}
	return strings.Join(lines, "\n")
}

func (m *Model) question(text string) string {
	return m.styles.Question.Render("? ") + m.styles.Title.Render(text)
}

func (m *Model) pickHints() string {
	return m.styles.Muted.Render("↑/↓ navigate · type to filter · enter select · esc cancel")
}

func (m *Model) actionHints() string {
	return m.styles.Muted.Render("↑/↓ navigate · enter run · esc back")
}

// confirmQuestion is the inline safeguard for the two operations people most
// often misjudge: a shutdown request that has not stopped anything yet, and a
// saved state that preserves rather than ends the session. The reassurance
// comes first: on an 80-column terminal the sentence must keep its meaning
// even if a very long VM name eats the end of the line.
func confirmQuestion(a vbox.Action, name string) string {
	switch a {
	case vbox.ActionStop:
		return fmt.Sprintf("Nothing is forced: send a shutdown request to %s? The guest decides.", name)
	case vbox.ActionSaveState:
		return fmt.Sprintf("This is not a shutdown: save %s's state (suspend to disk)?", name)
	case vbox.ActionRestart:
		return fmt.Sprintf("Ask %s's guest to reboot itself? This is never a hardware reset or a power cycle.", name)
	}
	return fmt.Sprintf("Perform %s on %s?", a.Label(), name)
}

// truncateForPrompt keeps a machine's name inside the question line.
func truncateForPrompt(name string, width int) string {
	return ellip(name, max(width-30, 12))
}
