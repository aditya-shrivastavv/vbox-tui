// Package vbox discovers VirtualBox, runs VBoxManage safely and parses its
// machine readable output.
package vbox

import (
	"strings"
)

// State is a normalised VirtualBox machine state.
type State string

// Normalised machine states. The string values are what the UI shows, so they
// are intentionally human readable.
const (
	StatePoweredOff    State = "powered off"
	StateRunning       State = "running"
	StatePaused        State = "paused"
	StateSaved         State = "saved"
	StateAborted       State = "aborted"
	StateStuck         State = "stuck"
	StateTransitioning State = "transitioning"
	StateInaccessible  State = "inaccessible"
	StateUnknown       State = "unknown"
)

// Kind classifies a raw state string the way the lifecycle rules care about it.
type Kind int

// State kinds.
const (
	KindOff Kind = iota
	KindRunning
	KindPaused
	KindSaved
	KindAborted
	KindTransitional
	KindUnavailable
	KindUnknown
)

// StateInfo is the normalised state plus its kind.
type StateInfo struct {
	State State
	Kind  Kind
	// Raw is the unmodified string reported by VirtualBox (useful for
	// "powered off (since ...)" style values and for unknown states).
	Raw string
}

// rawStates maps the lower-cased VirtualBox state token to a normalised state.
// Both the "list --long" spelling ("powered off") and the showvminfo
// VMState= token spelling ("poweroff") are accepted.
var rawStates = map[string]StateInfo{
	"powered off": {State: StatePoweredOff, Kind: KindOff},
	"poweroff":    {State: StatePoweredOff, Kind: KindOff},
	"stopped":     {State: StatePoweredOff, Kind: KindOff},
	"running":     {State: StateRunning, Kind: KindRunning},
	// A locked machine still runs, but VBoxManage refuses controlvm requests
	// until the lock is released, so it must not look like a plain "running".
	"running (locked)":      {State: StateRunning, Kind: KindTransitional, Raw: "running (locked)"},
	"paused (locked)":       {State: StatePaused, Kind: KindTransitional, Raw: "paused (locked)"},
	"paused":                {State: StatePaused, Kind: KindPaused},
	"paused (saved)":        {State: StatePaused, Kind: KindPaused},
	"saved":                 {State: StateSaved, Kind: KindSaved},
	"suspended":             {State: StateTransitioning, Kind: KindTransitional, Raw: "suspended"},
	"aborted":               {State: StateAborted, Kind: KindAborted},
	"stuck":                 {State: StateStuck, Kind: KindUnavailable, Raw: "stuck"},
	"starting":              {State: StateTransitioning, Kind: KindTransitional, Raw: "starting"},
	"stopping":              {State: StateTransitioning, Kind: KindTransitional, Raw: "stopping"},
	"saving":                {State: StateTransitioning, Kind: KindTransitional, Raw: "saving"},
	"restoring":             {State: StateTransitioning, Kind: KindTransitional, Raw: "restoring"},
	"deleting":              {State: StateTransitioning, Kind: KindTransitional, Raw: "deleting"},
	"live snapshotting":     {State: StateTransitioning, Kind: KindTransitional, Raw: "live snapshotting"},
	"teleporting":           {State: StateTransitioning, Kind: KindTransitional, Raw: "teleporting"},
	"guru meditation":       {State: StateUnknown, Kind: KindUnavailable, Raw: "guru meditation"},
	"fatal error":           {State: StateUnknown, Kind: KindUnavailable, Raw: "fatal error"},
	"query error":           {State: StateUnknown, Kind: KindUnavailable, Raw: "query error"},
	"inaccessible":          {State: StateInaccessible, Kind: KindUnavailable},
	"host reset":            {State: StatePoweredOff, Kind: KindOff},
	"host pending reboot":   {State: StateTransitioning, Kind: KindTransitional, Raw: "host pending reboot"},
	"host pending shutdown": {State: StateTransitioning, Kind: KindTransitional, Raw: "host pending shutdown"},
}

// NormalizeState turns a raw VirtualBox state string into a StateInfo.
//
// "list --long" appends provenance such as "(since 2026-10-16T16:40:30)" and
// "(paused)", so the trailing parenthesis is split off and, if the leading
// token is unrecognised, retried with it included.
func NormalizeState(raw string) StateInfo {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return StateInfo{State: StateUnknown, Kind: KindUnknown, Raw: raw}
	}
	low := strings.ToLower(trimmed)
	// The whole string is tried first: "running (locked)" means something
	// different from "running", and losing that distinction would offer
	// controlvm requests VirtualBox is going to refuse. The bare token is then
	// tried so "powered off (since ...)" still resolves.
	candidates := []string{low}
	if base, _, found := splitParen(low); found && base != low {
		candidates = append(candidates, base)
	}
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if info, ok := rawStates[c]; ok {
			out := info
			if out.Raw == "" {
				out.Raw = c
			}
			return out
		}
	}
	return StateInfo{State: StateUnknown, Kind: KindUnknown, Raw: low}
}

// splitParen returns the text before the last " (...)".
func splitParen(s string) (base, extra string, found bool) {
	idx := strings.LastIndex(s, " (")
	if idx < 0 || !strings.HasSuffix(s, ")") {
		return s, "", false
	}
	return s[:idx], s[idx+2 : len(s)-1], true
}

// StateAnnotation returns the free-form part of a "list --long" State value,
// e.g. "since 2026-10-16T16:40:30" or "paused". It is empty when absent.
//
// The marker is matched rune by rune while walking the original text, so the
// returned annotation keeps its original case. That matters: the annotation
// carries an ISO timestamp whose "T" separator must survive for parsing.
func StateAnnotation(raw string) string {
	runes := []rune(strings.TrimSpace(raw))
	for i := len(runes) - 1; i > 0; i-- {
		if runes[i] != ')' {
			continue
		}
		for j := i - 1; j > 0; j-- {
			if runes[j] != '(' {
				continue
			}
			if runes[j-1] != ' ' {
				break
			}
			return string(runes[j+1 : i])
		}
		break
	}
	return ""
}

// Action is a lifecycle operation the app can perform on a VM.
type Action string

// Lifecycle actions.
const (
	ActionStart       Action = "start"
	ActionRestore     Action = "restore"
	ActionResume      Action = "resume"
	ActionPause       Action = "pause"
	ActionStop        Action = "stop"
	ActionSaveState   Action = "save"
	ActionConnect     Action = "connect"
	ActionStartConn   Action = "start-and-connect"
	ActionViewUnknown Action = ""

	// ActionRestart asks the guest operating system to reboot itself. It
	// never resets virtual hardware and never powers the machine off; the
	// mechanism (Guest Additions or a fixed SSH command) is configured per
	// VM and must be able to reach a running guest.
	ActionRestart Action = "restart"

	// ActionRename changes a machine's display name (modifyvm --name). It
	// is not a lifecycle verb and is never part of a batch: renaming one
	// machine should be an explicit, per-machine decision, and VirtualBox
	// only performs it while the machine is powered off.
	ActionRename Action = "rename"
)

// LifecycleActions is the batch-executable set: one action applied to
// several explicitly selected machines. SSH, configuration and snapshot
// operations are deliberately absent — they are per-VM journeys.
var LifecycleActions = []Action{
	ActionStart, ActionPause, ActionResume, ActionStop, ActionRestart, ActionSaveState,
}

// Batchable reports whether the action may drive a multi-VM run.
func (a Action) Batchable() bool {
	for _, x := range LifecycleActions {
		if a == x {
			return true
		}
	}
	return false
}

// AllowedActions returns the lifecycle actions that make sense for a state.
// The prompt uses this to build the action menu, so it is the single source of
// truth for state-to-action rules: an action offered in a state where
// VirtualBox would refuse it is worse than no action at all.
func AllowedActions(info StateInfo) map[Action]bool {
	out := map[Action]bool{}
	switch info.Kind {
	case KindOff, KindAborted:
		// Aborted means the guest did not stop cleanly (a crash, or the host
		// lost power). VirtualBox starts it again exactly as a powered-off
		// machine, and nothing else is meaningful until it does.
		out[ActionStart] = true
		out[ActionStartConn] = true
		// modifyvm — the rename operation — exists precisely for the
		// powered-off state; no other state accepts it.
		out[ActionRename] = true
	case KindSaved:
		out[ActionRestore] = true
		// Restoring brings the exact previous session back, so "restore and
		// ssh" is the same bring-up-then-connect journey as starting.
		out[ActionStartConn] = true
	case KindPaused:
		out[ActionResume] = true
		// A paused guest is frozen: it cannot answer SSH and an ACPI power
		// button press reaches nothing until it runs again, so connecting
		// means "resume, then connect". VirtualBox also refuses savestate
		// while paused, so saving is not offered either — only what the
		// hypervisor actually honours appears in the menu. A graceful
		// reboot is refused for the same reason: a frozen CPU reboots
		// nothing, so restart names the resume step instead.
		out[ActionStartConn] = true
	case KindRunning:
		out[ActionConnect] = true
		out[ActionPause] = true
		out[ActionStop] = true
		out[ActionSaveState] = true
		// A graceful reboot needs a guest that is actually executing: only
		// a running machine can process it.
		out[ActionRestart] = true
	case KindTransitional, KindUnavailable, KindUnknown:
		// No mutation is safe: the machine is mid-operation, inaccessible or
		// in a state we do not understand.
	}
	return out
}

// Allowed reports whether the action is present and enabled.
func Allowed(actions map[Action]bool, a Action) bool { return actions[a] }

// Label is a short human label for an action, used in sentences.
func (a Action) Label() string {
	switch a {
	case ActionStart:
		return "start headless"
	case ActionRestore:
		return "restore"
	case ActionResume:
		return "resume"
	case ActionPause:
		return "pause"
	case ActionStop:
		return "shut down"
	case ActionSaveState:
		return "save state"
	case ActionConnect:
		return "ssh"
	case ActionStartConn:
		return "start and ssh"
	case ActionRestart:
		return "restart the guest"
	case ActionRename:
		return "rename"
	}
	return string(a)
}

// PrimaryStart returns the action that brings the machine to a running state,
// or the empty action when that is not possible.
func PrimaryStart(info StateInfo) Action {
	switch info.Kind {
	case KindOff, KindAborted:
		return ActionStart
	case KindSaved:
		return ActionRestore
	case KindPaused:
		return ActionResume
	}
	return ActionViewUnknown
}

// RunningOrPaused reports whether the machine is executing (and therefore could
// already serve SSH).
func (i StateInfo) RunningOrPaused() bool {
	return i.Kind == KindRunning || i.Kind == KindPaused
}

// Word is the compact state label shown after VM names in the picker. It is
// title case and short, so names stay the widest visual element on a row. The
// word always carries the meaning: colour only decorates it.
func (i StateInfo) Word() string {
	switch i.Kind {
	case KindOff:
		if i.State == StateAborted {
			return "Aborted"
		}
		return "Stopped"
	case KindRunning:
		return "Running"
	case KindPaused:
		return "Paused"
	case KindSaved:
		return "Saved"
	case KindTransitional:
		// Short raw states read well as-is ("Starting", "Saving"); anything
		// longer ("running (locked)") would need mid-word truncation, which no
		// column label survives with dignity. The full state stays available
		// in the sentence the UI writes about the machine.
		if i.Raw != "" && len(i.Raw) <= 12 {
			return title(i.Raw)
		}
		return "Busy"
	case KindUnavailable:
		if i.Raw != "" && len(i.Raw) <= 12 {
			return title(i.Raw)
		}
		return "Error"
	}
	if i.Raw != "" {
		return title(truncate(i.Raw, 12))
	}
	return "Unknown"
}

// title capitalises the first rune without forcing the rest to lower case, so
// a live state like "live snapshotting" reads as "Live snapshotting" rather
// than shouting.
func title(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

// truncate cuts to at most n runes without splitting a UTF-8 sequence. State
// words are short labels, never names, so a rune cap is enough here.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
