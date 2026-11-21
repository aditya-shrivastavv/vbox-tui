package app

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/aditya-shrivastavv/vbox-tui/internal/config"
	"github.com/aditya-shrivastavv/vbox-tui/internal/safedisplay"
)

// The settings editor (vbx config) manages what this app has itself stored for
// one VM: its SSH connection settings and its guest-control block. It reuses
// the exact prompts the first-SSH-use flow asks — the flow is the editor —
// and never touches anything else: no passwords are stored or asked, the
// user's own ~/.ssh/config is never read or written, and every write is
// addressed by the VM's UUID.

// settingsItem is one line of the settings menu.
type settingsItem int

const (
	settingEditSSH settingsItem = iota
	settingResetSSH
	settingEditGuest
	settingResetGuest
	settingResetAll
)

func (it settingsItem) label() string {
	switch it {
	case settingEditSSH:
		return "Edit SSH settings"
	case settingResetSSH:
		return "Reset SSH settings"
	case settingEditGuest:
		return "Set guest OS / restart route"
	case settingResetGuest:
		return "Reset guest settings"
	case settingResetAll:
		return "Remove all saved settings for this VM"
	}
	return ""
}

// settingsItems lists only what exists: resetting something that was never
// configured is noise, not a choice.
func (m *Model) settingsItems() []settingsItem {
	_, hasSSH := m.sshSet.SSH(m.selected.UUID)
	_, hasGuest := m.guestDoor.get(m.selected.UUID)
	var out []settingsItem
	out = append(out, settingEditSSH)
	if hasSSH {
		out = append(out, settingResetSSH)
	}
	out = append(out, settingEditGuest)
	if hasGuest || hasSSH {
		out = append(out, settingResetGuest)
	}
	if hasSSH || hasGuest {
		out = append(out, settingResetAll)
	}
	return out
}

func (m *Model) openSettings() {
	m.note = ""
	m.settingsCursor = 0
	m.mode = modeSettings
}

// ---------- view ----------

func (m *Model) settingsLines() []string {
	var out []string
	items := m.settingsItems()
	for i, it := range items {
		marker := "  "
		if i == m.settingsCursor {
			marker = "> "
		}
		label := it.label()
		if i == m.settingsCursor {
			out = append(out, marker+m.styles.ActionSel.Render(label))
		} else {
			out = append(out, marker+label)
		}
	}
	return out
}

func (m *Model) settingsSummary() string {
	sshCfg, has := m.sshSet.SSH(m.selected.UUID)
	line := "SSH: not configured (set up on first use or with Edit)"
	if has {
		// Stored settings are metadata: the host, alias, or user written
		// into a hand-edited file is encoded before it reaches the screen.
		line = "SSH: " + safedisplay.Text(sshCfg.Normalise().Display())
	}
	return line
}

func (m *Model) guestSummary() string {
	g, has := m.guestDoor.get(m.selected.UUID)
	if !has {
		return "Guest: not set (restarts use Guest Additions)"
	}
	// Same policy as the SSH summary above: whatever a door hands back is
	// external data, encoded before it can reach the screen. The app's own
	// writer only stores values from the fixed tables, so normal runs
	// change nothing here; a hand-edited file through a config-backed
	// door, or any future door, stays visible-but-disarmed.
	osWord := string(g.OS)
	if g.OS == config.GuestOSUnknown {
		osWord = "unknown"
	}
	mode := string(g.Restart)
	if mode == "" {
		mode = "auto"
	}
	return fmt.Sprintf("Guest: os=%s restart=%s", safedisplay.Text(osWord), safedisplay.Text(mode))
}

// ---------- keys ----------

func (m *Model) handleSettingsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	items := m.settingsItems()
	switch msg.String() {
	case "ctrl+c":
		return m, m.finish(OutcomeCancelled, "Cancelled. No settings were changed.")
	case "esc":
		m.note = ""
		if m.settingsFocused {
			// `vbx config [VM]`: the menu was the whole visit; leaving it is
			// a clean exit — the run changed nothing on its own.
			return m, m.finish(OutcomeSuccess,
				fmt.Sprintf("Settings for %s: %s", quoteName(nameOf(m.selected)), m.guestSummary()))
		}
		if m.settingsReturn == settingsReturnPicker {
			m.mode = modePickVM
			m.reconcileCursor()
		} else {
			m.mode = modePickAction
		}
		return m, nil
	case "up":
		if m.settingsCursor > 0 {
			m.settingsCursor--
		}
		return m, nil
	case "down":
		if len(items) > 0 && m.settingsCursor < len(items)-1 {
			m.settingsCursor++
		}
		return m, nil
	case "enter":
		if len(items) == 0 {
			return m, nil
		}
		return m.chooseSetting(items[clamp(m.settingsCursor, 0, len(items)-1)])
	}
	return m, nil
}

func (m *Model) chooseSetting(it settingsItem) (tea.Model, tea.Cmd) {
	switch it {
	case settingEditSSH:
		m.beginSSHEdit()
		return m, nil
	case settingEditGuest:
		m.beginGuestEdit()
		return m, nil
	case settingResetSSH, settingResetGuest, settingResetAll:
		m.resetScope = resetScopeFor(it)
		m.mode = modeConfirm
		return m, nil
	}
	return m, nil
}

func resetScopeFor(it settingsItem) config.ResetScope {
	switch it {
	case settingResetSSH:
		return config.ResetSSH
	case settingResetGuest:
		return config.ResetGuest
	default:
		return config.ResetEverything
	}
}

// resetQuestion bounds each scope, reassurance first: on an 80-column
// terminal the sentence must keep its meaning even if a very long VM name
// eats the end of the line (same rule as confirmQuestion).
func (m *Model) resetQuestion() string {
	name := nameOf(m.selected)
	switch m.resetScope {
	case config.ResetSSH:
		return fmt.Sprintf("The guest is untouched: forget the saved SSH settings for %s?", name)
	case config.ResetGuest:
		return fmt.Sprintf("Restarts fall back to Guest Additions: forget %s's guest settings?", name)
	default:
		return fmt.Sprintf("The VM and its disks are untouched: remove every vbx setting for %s?", name)
	}
}

func (m *Model) confirmReset() (tea.Model, tea.Cmd) {
	scope := m.resetScope
	m.resetScope = ""
	switch scope {
	case config.ResetSSH:
		if err := m.sshSet.Clear(m.selected.UUID); err != nil {
			return m, m.finish(OutcomeFailure, "Could not reset the SSH settings: "+oneError(err, 300))
		}
		m.note = "SSH settings removed."
	case config.ResetGuest:
		if err := m.guestDoor.store(m.selected.UUID, m.selected.Name, nil); err != nil {
			return m, m.finish(OutcomeFailure, "Could not reset the guest settings: "+oneError(err, 300))
		}
		m.note = "Guest settings removed."
	default:
		if err := m.sshSet.Clear(m.selected.UUID); err != nil {
			return m, m.finish(OutcomeFailure, "Could not reset the SSH settings: "+oneError(err, 300))
		}
		if err := m.guestDoor.store(m.selected.UUID, m.selected.Name, nil); err != nil {
			return m, m.finish(OutcomeFailure, "Could not reset the guest settings: "+oneError(err, 300))
		}
		m.note = "All saved settings removed."
	}
	m.openSettings()
	return m, nil
}

// ---------- ssh editor ----------

// beginSSHEdit opens the SSH prompt sequence with stored values prefilled —
// the same questions the first-SSH-use flow asks, answering with Enter keeps
// what is already saved.
func (m *Model) beginSSHEdit() {
	stored, _ := m.sshSet.SSH(m.selected.UUID)
	stored = stored.Normalise()
	m.setupEditing = true
	m.setupStep = 0
	m.setupMode = stored.Mode
	if m.setupMode == config.SSHModeNone {
		m.setupMode = config.SSHModeEndpoint
	}
	m.setupHost = stored.Host
	m.setupPort = ""
	if stored.Port != 0 {
		m.setupPort = fmt.Sprint(stored.Port)
	}
	m.setupUser = stored.User
	m.setupBuf = ""
	m.setupErr = ""
	if stored.Mode == config.SSHModeAlias {
		m.setupBuf = stored.Alias
	}
	m.mode = modeSSHSetup
}

// ---------- ssh editor ----------

var guestOSChoices = []config.GuestOS{config.GuestOSLinux, config.GuestOSWindows, config.GuestOSUnknown}
var restartModeChoices = []config.GuestRestartMode{
	config.GuestRestartAuto, config.GuestRestartGA, config.GuestRestartSSH, config.GuestRestartOff,
}

func (m *Model) beginGuestEdit() {
	stored, _ := m.guestDoor.get(m.selected.UUID)
	m.guestStep = 0
	m.guestOSCursor = indexOr(guestOSChoices, stored.OS, 0)
	m.restartCursor = indexOr(restartModeChoices, stored.Restart, 0)
	// The chooser's identity must be set on entry: handleChooseKey routes by
	// chooseKind, and a stale chooseBatchVerb left over from an abandoned
	// batch would send these keys — and this view — to the batch handler.
	m.chooseKind = chooseGuest
	m.mode = modeChoose
	m.chooseTitle = "What guest OS runs in " + nameOf(m.selected) + "?"
	m.chooseHint = "the reboot command is chosen from a fixed table for this family; unknown means no SSH reboot"
}

func indexOr[T comparable](set []T, want T, def int) int {
	for i, v := range set {
		if v == want {
			return i
		}
	}
	return def
}

func (m *Model) handleGuestEditKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.guestStep {
	case 0:
		switch msg.String() {
		case "up", "k":
			m.guestOSCursor = max(m.guestOSCursor-1, 0)
		case "down", "j":
			m.guestOSCursor = min(m.guestOSCursor+1, len(guestOSChoices)-1)
		case "esc":
			// The first step has nowhere to step back to inside the editor:
			// esc leaves it, exactly as it does from the settings menu it
			// returns to. Nothing has been stored — the write happens only
			// when the last step commits.
			return m.guestEditBack()
		case "ctrl+c":
			return m.guestEditCancel()
		case "enter":
			m.guestStep = 1
			m.chooseTitle = "How should this guest be restarted?"
			m.chooseHint = "ga needs Guest Additions; ssh needs working key-based SSH and a known OS; off refuses restarts"
		}
		return m, nil
	case 1:
		switch msg.String() {
		case "up", "k":
			m.restartCursor = max(m.restartCursor-1, 0)
		case "down", "j":
			m.restartCursor = min(m.restartCursor+1, len(restartModeChoices)-1)
		case "ctrl+c":
			return m.guestEditCancel()
		case "enter":
			cfg := config.GuestConfig{
				OS:      guestOSChoices[clamp(m.guestOSCursor, 0, len(guestOSChoices)-1)],
				Restart: restartModeChoices[clamp(m.restartCursor, 0, len(restartModeChoices)-1)],
			}
			backToSettings := func(note string) func() tea.Cmd {
				return func() tea.Cmd {
					m.note = note
					m.openSettings()
					return nil
				}
			}
			if cfg.OS == config.GuestOSUnknown && cfg.Restart == "" {
				// Nothing meaningful to store: keep the door clean.
				return m, m.commitStore(func() error {
					return m.guestDoor.store(m.selected.UUID, m.selected.Name, nil)
				}, backToSettings("Guest settings cleared."))
			}
			return m, m.commitStore(func() error {
				return m.guestDoor.store(m.selected.UUID, m.selected.Name, &cfg)
			}, backToSettings(fmt.Sprintf("Guest settings saved: os=%s restart=%s.", cfg.OS, modeWord(cfg.Restart))))
		case "esc":
			// One step back, unsaved: the OS choice is kept as the cursor,
			// not committed.
			m.guestStep = 0
			m.chooseTitle = "What guest OS runs in " + nameOf(m.selected) + "?"
			m.chooseHint = "the reboot command is chosen from a fixed table for this family; unknown means no SSH reboot"
		}
		return m, nil
	}
	return m, nil
}

// guestEditBack leaves the guest editor for the settings menu without
// writing anything. The editor never stores a partial choice — the only
// store is the final Enter on the last step — so "back" is free.
func (m *Model) guestEditBack() (tea.Model, tea.Cmd) {
	m.guestStep = 0
	m.note = ""
	m.openSettings()
	// Land the cursor on the item that opened the editor, not at the top.
	for i, it := range m.settingsItems() {
		if it == settingEditGuest {
			m.settingsCursor = i
			break
		}
	}
	return m, nil
}

// guestEditCancel is Ctrl+C inside the guest editor: the whole run ends,
// and — like every other cancellation before the final commit — no setting
// is written.
func (m *Model) guestEditCancel() (tea.Model, tea.Cmd) {
	m.guestStep = 0
	return m, m.finish(OutcomeCancelled, "Cancelled. No settings were changed.")
}

func modeWord(mo config.GuestRestartMode) string {
	if mo == config.GuestRestartAuto {
		return "auto"
	}
	return string(mo)
}

func (m *Model) guestEditLines() []string {
	var out []string
	out = append(out, m.question(m.chooseTitle))
	if m.guestStep == 0 {
		for i, osv := range guestOSChoices {
			out = append(out, m.chooseMarker(i, m.guestOSCursor, guestOSLabel(osv)))
		}
	} else {
		for i, mo := range restartModeChoices {
			out = append(out, m.chooseMarker(i, m.restartCursor, restartModeLabel(mo)))
		}
	}
	if m.chooseHint != "" {
		out = append(out, m.styles.Muted.Render(m.chooseHint))
	}
	return out
}

func guestOSLabel(o config.GuestOS) string {
	switch o {
	case config.GuestOSLinux:
		return "Linux"
	case config.GuestOSWindows:
		return "Windows"
	}
	return "Unknown (no SSH restart)"
}

func restartModeLabel(mo config.GuestRestartMode) string {
	switch mo {
	case config.GuestRestartGA:
		return "Guest Additions (controlvm reboot)"
	case config.GuestRestartSSH:
		return "SSH (fixed reboot command)"
	case config.GuestRestartOff:
		return "Off (refuse graceful restarts)"
	}
	return "Auto (SSH when fully configured, else Guest Additions)"
}

func (m *Model) chooseMarker(i, cursor int, label string) string {
	marker := "  "
	if i == cursor {
		marker = "> "
	}
	if i == cursor {
		return marker + m.styles.ActionSel.Render(label)
	}
	return marker + label
}

// ---------- storage ----------

// commitStore writes through a door off the UI goroutine, then runs the
// continuation the caller supplies.
func (m *Model) commitStore(write func() error, then func() tea.Cmd) tea.Cmd {
	m.mode = modeBusy
	m.phase = "Saving settings…"
	m.genSet++
	gen := m.genSet
	return tea.Batch(func() tea.Msg {
		return settingsSavedMsg{gen: gen, err: write(), then: then}
	}, m.spinner.Tick)
}

func (m *Model) applySettingsSaved(msg settingsSavedMsg) (tea.Model, tea.Cmd) {
	if msg.gen != m.genSet {
		return m, nil
	}
	if msg.err != nil {
		return m, m.finish(OutcomeFailure, "Could not save the settings: "+oneError(msg.err, 300))
	}
	if msg.then == nil {
		return m, nil
	}
	return m, msg.then()
}

// settingsReturn marks where esc leaves you from the settings menu.
type settingsReturn int

const (
	settingsReturnAction settingsReturn = iota
	settingsReturnPicker
)
