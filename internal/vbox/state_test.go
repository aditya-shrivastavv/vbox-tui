package vbox

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeState(t *testing.T) {
	tests := []struct {
		raw      string
		want     State
		wantKind Kind
	}{
		// Real "list --long" spellings, including the since annotation.
		{"powered off (since 2026-10-16T16:40:30.000000000)", StatePoweredOff, KindOff},
		{"running", StateRunning, KindRunning},
		{"paused (since 2026-01-01T00:00:00)", StatePaused, KindPaused},
		{"saved", StateSaved, KindSaved},
		{"aborted", StateAborted, KindAborted},
		// showvminfo VMState= tokens.
		{"poweroff", StatePoweredOff, KindOff},
		{"stuck", StateStuck, KindUnavailable},
		// Transitional states must not look actionable.
		{"saving", StateTransitioning, KindTransitional},
		{"restoring", StateTransitioning, KindTransitional},
		{"deleting", StateTransitioning, KindTransitional},
		{"live snapshotting", StateTransitioning, KindTransitional},
		{"teleporting", StateTransitioning, KindTransitional},
		{"suspended", StateTransitioning, KindTransitional},
		// Locked is transitional even though the machine runs, because controlvm
		// requests are refused while a session is locked.
		{"running (locked)", StateRunning, KindTransitional},
		// Unavailable.
		{"inaccessible", StateInaccessible, KindUnavailable},
		{"guru meditation", StateUnknown, KindUnavailable},
		// Unknown stays unknown rather than being guessed.
		{"frobnicated", StateUnknown, KindUnknown},
		{"", StateUnknown, KindUnknown},
	}
	for _, tc := range tests {
		got := NormalizeState(tc.raw)
		if got.State != tc.want || got.Kind != tc.wantKind {
			t.Errorf("NormalizeState(%q) = %q/%d, want %q/%d", tc.raw, got.State, got.Kind, tc.want, tc.wantKind)
		}
	}
}

func TestNormalizeStateIsCaseInsensitive(t *testing.T) {
	if got := NormalizeState("POWERED OFF"); got.State != StatePoweredOff {
		t.Errorf("case: got %q", got.State)
	}
}

func TestStateAnnotation(t *testing.T) {
	if got := StateAnnotation("powered off (since 2026-10-16T16:40:30)"); !strings.HasPrefix(got, "since ") {
		t.Errorf("annotation = %q, want a since clause", got)
	}
	if got := StateAnnotation("running"); got != "" {
		t.Errorf("annotation for plain running = %q, want empty", got)
	}
}

// TestAllowedActions pins the state-to-action rules: an action offered in the
// wrong state could change a VM the user did not intend to change, or offer a
// request VirtualBox would refuse.
func TestAllowedActions(t *testing.T) {
	cases := []struct {
		name    string
		info    StateInfo
		allowed []Action
		denied  []Action
	}{
		{
			name:    "powered off",
			info:    NormalizeState("powered off"),
			allowed: []Action{ActionStart, ActionStartConn},
			denied:  []Action{ActionStop, ActionSaveState, ActionResume, ActionPause, ActionConnect, ActionRestore},
		},
		{
			name:    "aborted can be started",
			info:    NormalizeState("aborted"),
			allowed: []Action{ActionStart},
			denied:  []Action{ActionStop, ActionSaveState},
		},
		{
			name:    "saved is restored, not started",
			info:    NormalizeState("saved"),
			allowed: []Action{ActionRestore, ActionStartConn},
			denied:  []Action{ActionResume, ActionStop, ActionSaveState, ActionConnect},
		},
		{
			name:    "running: ssh, pause, graceful shutdown, save",
			info:    NormalizeState("running"),
			allowed: []Action{ActionConnect, ActionPause, ActionStop, ActionSaveState},
			denied:  []Action{ActionStart, ActionRestore, ActionResume, ActionStartConn},
		},
		{
			// A frozen guest cannot answer SSH, cannot service ACPI, and
			// VirtualBox refuses savestate while paused: only resume (or
			// resume-then-ssh) is honest.
			name:    "paused can only resume (with an ssh follow-up)",
			info:    NormalizeState("paused"),
			allowed: []Action{ActionResume, ActionStartConn},
			denied:  []Action{ActionStart, ActionRestore, ActionStop, ActionSaveState, ActionConnect, ActionPause},
		},
		{
			name:    "transitional allows nothing",
			info:    NormalizeState("saving"),
			allowed: nil,
			denied:  []Action{ActionStart, ActionRestore, ActionResume, ActionStop, ActionSaveState, ActionConnect, ActionPause},
		},
		{
			name:    "locked allows nothing",
			info:    NormalizeState("running (locked)"),
			allowed: nil,
			denied:  []Action{ActionStop, ActionSaveState, ActionConnect, ActionPause},
		},
		{
			name:    "inaccessible allows nothing",
			info:    NormalizeState("inaccessible"),
			allowed: nil,
			denied:  []Action{ActionStart, ActionStop, ActionSaveState},
		},
		{
			name:    "unknown allows nothing",
			info:    NormalizeState("frobnicated"),
			allowed: nil,
			denied:  []Action{ActionStart, ActionStop, ActionSaveState},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AllowedActions(tc.info)
			for _, a := range tc.allowed {
				if !got[a] {
					t.Errorf("%s: action %q should be allowed", tc.name, a)
				}
			}
			for _, a := range tc.denied {
				if got[a] {
					t.Errorf("%s: action %q must NOT be allowed", tc.name, a)
				}
			}
		})
	}
}

func TestPrimaryStart(t *testing.T) {
	cases := []struct {
		raw  string
		want Action
	}{
		{"powered off", ActionStart},
		{"aborted", ActionStart},
		{"saved", ActionRestore},
		{"paused", ActionResume},
		{"running", ActionViewUnknown},
		{"saving", ActionViewUnknown},
		{"inaccessible", ActionViewUnknown},
	}
	for _, tc := range cases {
		if got := PrimaryStart(NormalizeState(tc.raw)); got != tc.want {
			t.Errorf("PrimaryStart(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestRunningOrPaused(t *testing.T) {
	if !NormalizeState("running").RunningOrPaused() {
		t.Error("running should count as executing")
	}
	if !NormalizeState("paused").RunningOrPaused() {
		t.Error("a paused machine still holds memory and could serve a socket")
	}
	if NormalizeState("saved").RunningOrPaused() {
		t.Error("saved is not executing")
	}
}

// TestStateWordsCarryMeaning asserts every state has a non-empty ASCII word:
// the picker never depends on colour or symbols to say what a machine is doing.
func TestStateWordsCarryMeaning(t *testing.T) {
	raws := []string{
		"powered off", "running", "paused", "saved", "aborted", "saving",
		"restoring", "inaccessible", "guru meditation", "frobnicated", "",
	}
	seen := map[string]bool{}
	for _, raw := range raws {
		w := NormalizeState(raw).Word()
		if w == "" {
			t.Errorf("state %q has an empty word", raw)
		}
		if strings.ContainsAny(w, "\n\t ") && raw != "" {
			// Transitional words may contain a space ("Live snapshotting");
			// every base word must not.
			switch NormalizeState(raw).Kind {
			case KindTransitional, KindUnavailable, KindUnknown:
			default:
				t.Errorf("word %q for %q contains layout characters", w, raw)
			}
		}
		for _, r := range w {
			if r > 0x7f {
				t.Errorf("state word %q for %q uses non-ASCII %q", w, raw, r)
			}
		}
		seen[w] = true
	}
	// The words that must be distinct are: a picker reader must not confuse
	// two states by their label.
	for _, pair := range [][2]string{{"Running", "Stopped"}, {"Saved", "Paused"}} {
		if !seen[pair[0]] || !seen[pair[1]] {
			t.Errorf("expected distinct words %q and %q, words were %v", pair[0], pair[1], seen)
		}
	}
}

// TestActionLabelsUnderstated checks the menu wording contract: a shutdown is
// labelled by its behaviour (ACPI request), not as a completed stop, and the
// verbs keep their plain names.
func TestActionLabelsUnderstated(t *testing.T) {
	cases := map[Action]string{
		ActionStart:     "start headless",
		ActionStop:      "shut down",
		ActionSaveState: "save state",
		ActionPause:     "pause",
		ActionResume:    "resume",
		ActionConnect:   "ssh",
	}
	for a, want := range cases {
		if got := a.Label(); got != want {
			t.Errorf("Label(%q) = %q, want %q", a, got, want)
		}
	}
}

// TestInventoryResolve covers the argument-resolution contract: UUID and exact
// names only, ambiguity is an error carrying every candidate, and no fuzzy
// match is ever taken silently.
func TestInventoryResolve(t *testing.T) {
	inv := Inventory{Machines: []Machine{
		{Name: "Kali Linux", UUID: "2D620512-4B10-45A8-B070-25435E5106A5"},
		{Name: "Ubuntu Dev", UUID: "c907c20b-5a41-4dc2-99c5-0e9bc7773c38"},
		{Name: "dup", UUID: "11111111-1111-1111-1111-111111111111"},
		{Name: "dup", UUID: "22222222-2222-2222-2222-222222222222"},
	}}

	// Exact name.
	if m, err := inv.Resolve("Kali Linux"); err != nil || m.UUID != inv.Machines[0].UUID {
		t.Errorf("exact name: %v %v", m, err)
	}
	// UUID, case- and brace-insensitive.
	if m, err := inv.Resolve("{2d620512-4b10-45a8-b070-25435e5106a5}"); err != nil || m.Name != "Kali Linux" {
		t.Errorf("uuid: %v %v", m, err)
	}
	// Case differences in names matter: VirtualBox names are case-sensitive,
	// and a near-miss must not silently resolve.
	if _, err := inv.Resolve("kali linux"); err == nil {
		t.Error("name case changed: must not resolve silently")
	}
	// A near match is not a match: no fuzzy fallback.
	if _, err := inv.Resolve("Kali"); err == nil {
		t.Error("prefix match accepted: the picker requires an exact name")
	}
	// Unknown.
	if _, err := inv.Resolve("nonexistent"); err == nil {
		t.Error("unknown name accepted")
	}
	// Unknown UUID.
	if _, err := inv.Resolve("33333333-3333-3333-3333-333333333333"); err == nil {
		t.Error("unknown uuid accepted")
	}
	// Duplicates must refuse and list candidates.
	_, err := inv.Resolve("dup")
	var amb *ErrAmbiguousName
	if !errors.As(err, &amb) {
		t.Fatalf("duplicate name: want ErrAmbiguousName, got %v", err)
	}
	if len(amb.Machines) != 2 {
		t.Errorf("ambiguous error should list both machines, got %d", len(amb.Machines))
	}
	if !strings.Contains(err.Error(), "11111111") || !strings.Contains(err.Error(), "22222222") {
		t.Errorf("the ambiguity message should show every UUID so the user can disambiguate: %q", err)
	}
	// Empty input is a usage error, not a panic or a silent pick.
	if _, err := inv.Resolve("   "); err == nil {
		t.Error("empty target accepted")
	}
}
