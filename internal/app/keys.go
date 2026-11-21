package app

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/aditya-shrivastavv/vbox-tui/internal/config"
	"github.com/aditya-shrivastavv/vbox-tui/internal/ssh"
	"github.com/aditya-shrivastavv/vbox-tui/internal/vbox"
)

// handleKey routes one key press to the active step. Each step owns the
// keyboard exclusively; esc means "one step back" and cancels work cleanly.
func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.mode {
	case modeLoading, modeBusy, modeWaitSSH:
		return m.handleWorkKey(msg)
	case modeBatchRun:
		return m.handleBatchRunKey(msg)
	case modePickVM:
		return m.handlePickVMKey(msg)
	case modePickAction:
		return m.handlePickActionKey(msg)
	case modeConfirm:
		if m.confirmSnapshot {
			return m.handleSnapshotConfirmKey(msg)
		}
		if m.resetScope != "" {
			return m.handleResetConfirmKey(msg)
		}
		if m.batchAction != "" {
			return m.handleBatchConfirmKey(msg)
		}
		return m.handleConfirmKey(msg)
	case modeSSHSetup:
		return m.handleSetupKey(msg)
	case modeSettings:
		return m.handleSettingsKey(msg)
	case modeChoose:
		return m.handleChooseKey(msg)
	case modeText:
		return m.handleTextKey(msg)
	case modeSnapList:
		return m.handleSnapListKey(msg)
	}
	return m, nil
}

// handleWorkKey answers keys while work is in flight. The work itself cannot
// be recalled, so cancelling says honestly what is still in motion.
func (m *Model) handleWorkKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c", "q":
		step := "the current step"
		switch {
		case m.mode == modeWaitSSH:
			step = fmt.Sprintf("waiting for SSH on %s", m.waitAddr)
		case m.phase != "":
			step = strings.TrimSuffix(m.phase, "…")
		}
		note := ""
		if m.opIssued || m.opLive.Load() {
			// opLive: the running goroutine has already handed the request to
			// VirtualBox; opIssued: an earlier step of this run already did.
			note = fmt.Sprintf(" Cancelling does not undo what VirtualBox has already started on %s; it keeps running with the last changes.",
				quoteName(nameOf(m.selected)))
		}
		return m, m.finish(OutcomeCancelled, fmt.Sprintf("Cancelled %s.%s", step, note))
	}
	return m, nil
}

// handlePickVMKey drives the VM picker: arrows move, typing filters, enter
// selects, esc (or ctrl+c) cancels the whole run.
func (m *Model) handlePickVMKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	n := len(m.selectable())
	switch msg.String() {
	case "ctrl+c":
		return m, m.finish(OutcomeCancelled, "Cancelled. Nothing was changed.")
	case "esc":
		// Esc first lets go of the marks, then leaves the run: a stray
		// space should never force a quit.
		if m.markedCount() > 0 {
			m.selectedSet = map[string]bool{}
			return m, nil
		}
		return m, m.finish(OutcomeCancelled, "Cancelled. Nothing was changed.")
	case "up":
		if m.cursor > 0 {
			m.cursor--
			m.clampListTop()
		}
		return m, nil
	case "down":
		if n > 0 && m.cursor < n-1 {
			m.cursor++
			m.clampListTop()
		}
		return m, nil
	case "home":
		m.cursor = 0
		m.clampListTop()
		return m, nil
	case "end":
		if n > 0 {
			m.cursor = n - 1
		}
		m.clampListTop()
		return m, nil
	}
	// Backspace is matched by key type, not by name: tea calls several
	// distinct keys "backspace", and string matching the name here would
	// steal characters like "." from the filter.
	if msg.Type == tea.KeyBackspace {
		r := []rune(m.filter)
		if len(r) > 0 {
			m.filter = string(r[:len(r)-1])
			m.reconcileCursor()
		}
		return m, nil
	}
	// Space marks the highlighted machine for a batch run. It deliberately
	// does not reach the filter: multi-selection needs a key nobody would
	// argue about, and filtering still matches every word of a name ("Work"
	// finds "Workbench — Win11").
	if msg.Type == tea.KeySpace || (msg.Type == tea.KeyRunes && msg.String() == " ") {
		return m.handlePickerSpace()
	}
	if msg.Type == tea.KeyEnter {
		if m.markedCount() > 0 {
			return m.pickerEnterWithMarks()
		}
		return m.pickVM()
	}
	s := msg.String()
	if msg.Type == tea.KeyRunes && !msg.Alt {
		// Typing filters, every printable rune included: j, k, g and friends
		// are letters of VM names ("Kali", "git-server"), so binding them to
		// movement would steal keystrokes from the very thing the picker is
		// for. Navigation is arrows, home and end.
		m.filter += s
		m.reconcileCursor()
	}
	return m, nil
}

// pickVM applies the picker selection.
func (m *Model) pickVM() (tea.Model, tea.Cmd) {
	rows := m.selectable()
	if len(rows) == 0 {
		return m, nil
	}
	m.cursor = clamp(m.cursor, 0, len(rows)-1)
	vm := m.inv.Machines[rows[m.cursor]]
	m.selected = vm
	m.filter = ""
	if m.focusAction != "" {
		// Direct command: no second menu. App-level journeys open their flow
		// for the picked machine; a lifecycle verb re-checks the state and
		// runs exactly that one action.
		switch m.focusAction {
		case ActionSnapshots:
			return m, tea.Batch(m.openSnapshots(true), m.spinner.Tick)
		case ActionSettings:
			m.settingsFocused = true
			m.openSettings()
			return m, nil
		case ActionRename:
			// `vbx rename` with the picker open: prefilled prompt, the run
			// ends with the answer.
			m.renameFocused = true
			m.beginRename(vm, true, false)
			return m, nil
		}
		return m, tea.Batch(m.runStep(m.focusAction), m.spinner.Tick)
	}
	m.mode = modeBusy
	m.phase = fmt.Sprintf("Checking %s…", nameOf(vm))
	// The tick is seeded because this busy step follows an idle picker whose
	// spinner chain has already stopped.
	return m, tea.Batch(m.readMenu(vm.UUID), m.spinner.Tick)
}

// handlePickActionKey drives the state-aware action menu.
func (m *Model) handlePickActionKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	n := len(m.actions)
	switch msg.String() {
	case "ctrl+c":
		return m, m.finish(OutcomeCancelled, "Cancelled. Nothing was changed.")
	case "esc":
		// One step back: the picker, with the same inventory.
		m.note = ""
		m.mode = modePickVM
		m.reconcileCursor()
		return m, nil
	case "up", "k":
		if m.actCursor > 0 {
			m.actCursor--
		}
		return m, nil
	case "down", "j":
		if n > 0 && m.actCursor < n-1 {
			m.actCursor++
		}
		return m, nil
	case "enter":
		if n == 0 {
			return m, nil
		}
		return m.chooseAction(m.actions[clamp(m.actCursor, 0, n-1)])
	}
	return m, nil
}

// chooseAction runs a menu selection, first asking about the operations whose
// outcome is easy to mistake (a shutdown request that has not yet stopped the
// guest; a saved state that keeps the session alive).
func (m *Model) chooseAction(a vbox.Action) (tea.Model, tea.Cmd) {
	switch a {
	case ActionSnapshots:
		return m, tea.Batch(m.openSnapshots(false), m.spinner.Tick)
	case ActionSettings:
		m.settingsReturn = settingsReturnAction
		m.openSettings()
		return m, nil
	case ActionRename:
		// The inline prefilled prompt; esc or an unchanged answer leaves
		// everything as it was.
		m.beginRename(m.selected, false, true)
		return m, nil
	}
	if needsConfirm(a) {
		m.confirmAction = a
		m.mode = modeConfirm
		return m, nil
	}
	return m.runChoice(a)
}

// reopenMenuForSelected re-reads one machine's state and puts its action menu
// up again. Used when a flow that was entered from the menu finishes (the
// snapshot list closes, say) and the operator should land back where they
// were, with fresh state.
func (m *Model) reopenMenuForSelected() (tea.Model, tea.Cmd) {
	if m.selected.UUID == "" {
		m.mode = modePickVM
		m.reconcileCursor()
		return m, nil
	}
	m.mode = modeBusy
	m.phase = fmt.Sprintf("Checking %s…", nameOf(m.selected))
	return m, tea.Batch(m.readMenu(m.selected.UUID), m.spinner.Tick)
}

// runChoice starts one chosen action. Explicit subcommands reach this without
// the confirm gate above: the command line is already the confirmation.
func (m *Model) runChoice(a vbox.Action) (tea.Model, tea.Cmd) {
	m.confirmAction = ""
	// SSH journeys without settings fall into the setup flow after the bring-
	// up step lands; everything else is the same single-door recheck.
	return m, m.startStep(a)
}

func (m *Model) handleConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	a := m.confirmAction
	switch msg.String() {
	case "y", "Y":
		m.mode = modeBusy // runStep installs its own phase text
		return m.runChoice(a)
	case "n", "N", "esc", "ctrl+c":
		m.confirmAction = ""
		m.mode = modePickAction
		return m, nil
	}
	return m, nil
}

// handleResetConfirmKey answers the settings editor's reset gate.
func (m *Model) handleResetConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		return m.confirmReset()
	case "n", "N", "esc", "ctrl+c":
		m.resetScope = ""
		m.mode = modeSettings
		return m, nil
	}
	return m, nil
}

// ---------- ssh setup keys ----------

// handleSetupKey drives the short sequential SSH setup: mode → alias or
// host → port → user. Each step asks exactly one question; esc steps back
// one question or out of setup entirely.
func (m *Model) handleSetupKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// The editor's step 0 carries one extra answer: "0" forgets the saved
	// connection (the plain first-use flow has nothing stored yet, so it
	// never sees this branch).
	if m.setupEditing && m.setupStep == 0 && msg.String() == "0" {
		if _, configured := m.sshSet.SSH(m.selected.UUID); !configured {
			m.setupErr = "nothing is saved to clear"
			return m, nil
		}
		return m, m.commitStore(func() error { return m.sshSet.Clear(m.selected.UUID) }, func() tea.Cmd {
			m.setupEditing = false
			m.note = "SSH settings removed."
			m.openSettings()
			return nil
		})
	}
	// Only the chooser step interprets arrows; in a text step every printable
	// rune belongs to the answer — "127.0.0.1" is a host, and a username can
	// contain any letter, so keys with special meaning on step 0 must stay
	// plain text on the other steps.
	if m.setupStep == 0 {
		switch msg.String() {
		case "up", "down":
			if m.setupMode == config.SSHModeAlias {
				m.setupMode = config.SSHModeEndpoint
			} else {
				m.setupMode = config.SSHModeAlias
			}
			m.setupErr = ""
			return m, nil
		case "1", "2":
			if msg.String() == "1" {
				m.setupMode = config.SSHModeEndpoint
			} else {
				m.setupMode = config.SSHModeAlias
			}
			m.setupErr = ""
			return m, nil
		}
	}
	switch msg.String() {
	case "ctrl+c":
		return m, m.finish(OutcomeCancelled, "Cancelled. No SSH settings were changed for this machine.")
	case "esc":
		return m.setupBack()
	case "enter":
		return m.setupAdvance()
	case "backspace":
		r := []rune(m.setupBuf)
		if len(r) > 0 {
			m.setupBuf = string(r[:len(r)-1])
			m.setupErr = ""
		}
		return m, nil
	}
	// Free typing belongs to the text steps only, and never to a control or
	// meta combination.
	if m.setupStep > 0 && msg.Type == tea.KeyRunes && !msg.Alt {
		m.setupBuf += msg.String()
		m.setupErr = ""
	}
	return m, nil
}

// setupBack steps one question back, or out of the SSH phase back to the
// action menu when already at the first question.
func (m *Model) setupBack() (tea.Model, tea.Cmd) {
	switch m.setupStep {
	case 0:
		m.mode = modePickAction
		return m, nil
	case 1:
		// Forget what was typed for the deeper steps: they follow the mode.
		m.setupStep = 0
		m.setupErr = ""
	case 2:
		m.setupStep = 1
		m.setupBuf = m.setupHost
		m.setupErr = ""
	case 3:
		m.setupStep = 2
		m.setupBuf = m.setupPort
		m.setupErr = ""
	}
	return m, nil
}

// setupAdvance validates the current answer and moves on, committing at the
// last step.
func (m *Model) setupAdvance() (tea.Model, tea.Cmd) {
	value := strings.TrimSpace(m.setupBuf)
	switch m.setupStep {
	case 0:
		if m.setupMode == "" {
			m.setupMode = config.SSHModeEndpoint
		}
		// Entering the text steps: keep stored text where the editor wants
		// it prefilled (endpoint host; the alias was prefilled at open).
		if m.setupEditing && m.setupMode == config.SSHModeEndpoint {
			m.setupBuf = m.setupHost
		} else {
			m.setupBuf = ""
		}
		m.setupStep = 1
		m.setupErr = ""
		return m, nil
	case 1:
		if m.setupMode == config.SSHModeAlias {
			cfg := config.SSHConfig{Mode: config.SSHModeAlias, Alias: value}
			if err := config.ValidateSSH(cfg); err != nil {
				m.setupErr = err.Error()
				return m, nil
			}
			return m, m.commitSetup(cfg)
		}
		if err := config.ValidateSSH(config.SSHConfig{Mode: config.SSHModeEndpoint, Host: value, Port: 22}); err != nil {
			m.setupErr = err.Error()
			return m, nil
		}
		m.setupHost = value
		def := m.suggestedPort()
		m.setupBuf = def
		m.setupStep = 2
		m.setupErr = ""
		return m, nil
	case 2:
		port, err := ssh.ParsePort(value)
		if err != nil {
			m.setupErr = err.Error()
			return m, nil
		}
		m.setupPort = fmt.Sprint(port)
		m.setupBuf = m.defaultUser()
		if m.setupBuf == "" {
			// No guess to prefill: skip straight to the optional user answer.
			m.setupBuf = ""
		}
		m.setupStep = 3
		m.setupErr = ""
		return m, nil
	case 3:
		user := strings.TrimSpace(value)
		if user != "" {
			if err := config.ValidateSSH(config.SSHConfig{
				Mode: config.SSHModeEndpoint, Host: m.setupHost, Port: mustPort(m.setupPort), User: user,
			}); err != nil {
				m.setupErr = err.Error()
				return m, nil
			}
		}
		return m, m.commitSetup(config.SSHConfig{
			Mode: config.SSHModeEndpoint,
			Host: m.setupHost,
			Port: mustPort(m.setupPort),
			User: user,
		})
	}
	return m, nil
}

// suggestedPort pre-fills the endpoint port: the stored port in the settings
// editor, otherwise an existing NAT forwarding rule to guest 22 when
// VirtualBox reports one, and 22 otherwise.
func (m *Model) suggestedPort() string {
	if m.setupEditing && m.setupPort != "" {
		return m.setupPort
	}
	if m.detail != nil {
		if f, ok := m.detail.NATForwardFor(22); ok && f.HostPort > 0 {
			return fmt.Sprint(f.HostPort)
		}
	}
	return "22"
}

// defaultUser pre-fills the user prompt from stored settings; empty lets the
// user type or accept OpenSSH's own default.
func (m *Model) defaultUser() string {
	if cfg, ok := m.sshSet.SSH(m.selected.UUID); ok && cfg.User != "" {
		return cfg.User
	}
	return ""
}

func mustPort(s string) int {
	p, err := ssh.ParsePort(s)
	if err != nil {
		return 0
	}
	return p
}
