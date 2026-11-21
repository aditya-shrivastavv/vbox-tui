package app

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/aditya-shrivastavv/vbox-tui/internal/config"
	"github.com/aditya-shrivastavv/vbox-tui/internal/safedisplay"
	"github.com/aditya-shrivastavv/vbox-tui/internal/vbox"
)

// Adversarial display tests. The threat: VM or snapshot metadata an
// attacker controls (an imported OVA, a machine made by another tool)
// carries escape sequences, carriage returns, or BiDi overrides that —
// printed raw — would move the cursor, erase rows, write the clipboard,
// or make a name read as something else (display spoofing; the backend
// still addresses every operation by UUID).
//
// What is asserted, at byte level, on the raw in-memory output of the REAL
// render pipeline: the tests force the Ascii colour profile first — the
// very switch --no-color uses — so lipgloss itself emits zero escape
// bytes. Any control byte then present anywhere in View() output can only
// have come
// from data, which is exactly the leak the safe-display policy promises
// cannot happen. Nothing is ever written to a real terminal here; every
// assertion runs on captured strings, so a defect fails the test instead
// of trashing a developer's scrollback.

// forceAscii pins the renderer to plain text for one test: the app's own
// styling then contributes no escape bytes at all.
func forceAscii(t *testing.T) {
	t.Helper()
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })
}

// forbiddenLineBytes: every C0 control plus DEL. LF never appears inside
// one frame line (frames are split on it first); CR included: an encoded
// name shows <0D>, never a live rewind.
const forbiddenLineBytes = "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0b\x0c\x0d\x0e\x0f" +
	"\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1c\x1d\x1e\x1f\x7f"

const escByte = "\x1b"

// assertNoHostileBytes examines a raw frame rendered under the Ascii
// profile: no ESC byte, no C0/DEL control byte (LF per line only as
// structure), no BiDi/zero-width/C1 formatting rune, nowhere.
func assertNoHostileBytes(t *testing.T, what string, raw string) {
	t.Helper()
	if strings.Contains(raw, escByte) {
		t.Fatalf("%s: an ESC byte is present in Ascii-profile output — it cannot be app styling: %q",
			what, firstEscContext(raw))
	}
	for _, line := range strings.Split(raw, "\n") {
		if i := strings.IndexAny(line, forbiddenLineBytes); i >= 0 {
			t.Fatalf("%s: live control byte %q leaked into a rendered line: %q", what, line[i], line)
		}
	}
	for _, r := range raw {
		switch r {
		case '\u200b', '\u200c', '\u200d', '\u200e', '\u200f',
			'\u202a', '\u202b', '\u202c', '\u202d', '\u202e',
			'\u2066', '\u2067', '\u2068', '\u2069', '\ufeff':
			t.Fatalf("%s: invisible/formatting rune %U survived rendering", what, r)
		}
		if r >= 0x80 && r <= 0x9f {
			t.Fatalf("%s: C1 control rune %U survived rendering", what, r)
		}
	}
}

func firstEscContext(raw string) string {
	i := strings.Index(raw, escByte)
	lo := max(i-20, 0)
	hi := min(i+20, len(raw))
	return raw[lo:hi]
}

func hostileMachine(name, state string) vbox.Machine {
	return vbox.Machine{Name: name, UUID: "44444444-5555-6666-7777-888888888888",
		State: vbox.NormalizeState(state)}
}

func TestHostileVMNameCannotForgeRowsInPicker(t *testing.T) {
	forceAscii(t)
	// ESC clears and repositions, a BEL pings, CR rewinds the row to
	// overwrite the state column with a fake success line, OSC 52 reaches
	// for the clipboard, and RLO flips the visual order of the tail.
	hostile := "\x1b[2J\x1b[1;1Htrusted\x1b[0m\x07victim\rStarted OK\x1b]52;c;aGVsbG8=\x07\u202eevil-kali"
	f := newFake(hostileMachine(hostile, "powered off"))
	s := start(t, modelWith(f, newStore(), dialRefused()))
	s.shot()
	assertNoHostileBytes(t, "picker frame", s.frame)
	stripped := s.frame // Ascii profile: nothing here is styling
	// Visible disarming, not silent dropping: the encoded markers must be
	// printed. Width ellipsis may cut the row's tail — the leading ESC
	// marker is inside the first cells either way, and the whole string
	// plus the truncation policy are covered by the unit test below.
	if !strings.Contains(stripped, "<ESC>") {
		t.Fatalf("the hostile name must render as visible markers:\n%s", stripped)
	}
	// Row-count honesty: exactly one machine row exists and no line of the
	// frame is the injected "Started OK" row.
	rows := 0
	for _, ln := range strings.Split(stripped, "\n") {
		if strings.HasPrefix(ln, "> ") {
			rows++
		}
		if strings.HasPrefix(strings.TrimSpace(ln), "Started OK") {
			t.Fatalf("the CR-injected fake row rendered as a row:\n%s", stripped)
		}
	}
	if rows != 1 {
		t.Fatalf("exactly one machine row expected, got %d:\n%s", rows, stripped)
	}
}

func TestHostileNameInMenuAndConfirmationLines(t *testing.T) {
	forceAscii(t)
	// A running hostile machine: the state-aware menu draws its name every
	// frame, and the restart confirmation — the question an Enter answers
	// with a request — must carry the app's sentence plus literally
	// encoded data, nothing live.
	hostile := "safe\u202e-suffix\x1b[31mRED"
	f := newFake(hostileMachine(hostile, "running"))
	s := start(t, modelWith(f, newStore(), dialRefused()))
	s.enter()
	s.settleTo(modePickAction, 2*time.Second)
	s.shot()
	assertNoHostileBytes(t, "action menu", s.frame)
	if !strings.Contains(s.frame, "<U+202E>") || !strings.Contains(s.frame, "<ESC>") {
		t.Fatalf("menu must show the encoded name:\n%s", s.frame)
	}
	// Running menu: SSH, Pause, Shut down, Save state, Restart guest.
	for i := 0; i < 4; i++ {
		s.down()
	}
	s.enter() // the restart confirmation gate
	s.settleTo(modeConfirm, 2*time.Second)
	s.shot()
	assertNoHostileBytes(t, "confirm question", s.frame)
	// The frame ellipsizes the long question, so assert on text that
	// survives truncation: the app's verb is intact, and the hostile name
	// sits inside it fully encoded.
	if !strings.Contains(s.frame, "? Ask ") || !strings.Contains(s.frame, "guest to reboot itself") {
		t.Fatalf("the confirmation must keep the app's own sentence:\n%s", s.frame)
	}
	if !strings.Contains(s.frame, "safe<U+202E>-suffix<ESC>[31mRED") {
		t.Fatalf("the confirm line must quote the encoded name:\n%s", s.frame)
	}
	s.key("n") // cancel: nothing sent
	s.drain(ms(60))
	for _, op := range f.opList() {
		if op != "list" && op != "detail" {
			t.Fatalf("reading and declining must change nothing: %v", f.opList())
		}
	}
}

func TestHostileSnapshotMetadataCannotSpoofTheTree(t *testing.T) {
	forceAscii(t)
	off := hostileMachine("lab", "powered off")
	f := newFake(off)
	f.seedSnapshots(off.UUID, []vbox.Snapshot{
		{Name: "clean\r\x1b[2Kinvented", UUID: "10000000-0000-4000-8000-000000000001", Depth: 0,
			Description: "decoy \u202e \x1b]0;poisoned title\x07"},
	}, "10000000-0000-4000-8000-000000000001")
	s := start(t, modelWith(f, newStore(), dialRefused()))
	s.enter()
	s.settleTo(modePickAction, 2*time.Second)
	// Off menu: start, start+ssh, rename, snapshots.
	for i := 0; i < 3; i++ {
		s.down()
	}
	s.enter()
	s.settleTo(modeSnapList, 2*time.Second)
	s.shot()
	assertNoHostileBytes(t, "snapshot tree", s.frame)
	// The row shows the target visibly, disarmed: CR, ESC, and the BiDi
	// override all carry markers; nothing is silently dropped.
	if !strings.Contains(s.frame, "<0D>") || !strings.Contains(s.frame, "<ESC>") || !strings.Contains(s.frame, "<U+202E>") {
		t.Fatalf("snapshot rows must render the target visibly encoded:\n%s", s.frame)
	}
	// The restore confirmation — the question a destructive 'y' answers —
	// carries the encoded identity, never the live sequence.
	s.enter()
	s.settleTo(modeConfirm, 3*time.Second)
	s.shot()
	assertNoHostileBytes(t, "restore question", s.frame)
	if !strings.Contains(s.frame, `snapshot "clean<0D>`) {
		t.Fatalf("the question must still name the target, encoded:\n%s", s.frame)
	}
	s.key("n")
	s.settleTo(modeSnapList, 2*time.Second)
}

type multilineError struct{}

func (multilineError) Error() string {
	return "VirtualBox reports:\r\n\x1b[1;1HVBoxManage: error: cannot touch \x1b]52;c;dGhpcyBpcyBub3QgYSB0ZXN0\x07\u202e\u0085"
}

func TestHostileErrorFromBackendIsFlattenedAndEncoded(t *testing.T) {
	// VBoxManage errors quote VM names and host paths an attacker
	// influences straight into their text. oneError flattens and encodes:
	// the summary the CLI prints for the whole run must carry the payload
	// strictly as markers (that stream carries no styling beyond the
	// "+"/"!" mark, so these are the exact bytes a user reads).
	off := vmOff()
	f := newFake(off)
	f.mu.Lock()
	f.renameErr = multilineError{}
	f.mu.Unlock()
	o := baseOptions(f, newStore(), dialRefused())
	o.Direct = &DirectRun{Action: ActionRename, VM: off, RenameTo: "innocent"}
	s := start(t, New(o))
	s.settleOutcome(3 * time.Second)
	got := s.outcome(OutcomeFailure)
	if strings.ContainsAny(got, "\x1b\x00\r\n\t") {
		t.Fatalf("a backend error must never carry live controls to the terminal: %q", got)
	}
	for _, r := range got {
		if r == '\u202e' || r == '\u0085' {
			t.Fatalf("hostile rune %U survived into the result line", r)
		}
	}
	if !strings.Contains(got, "<ESC>") || !strings.Contains(got, "<U+202E>") {
		t.Fatalf("the hostile error must be shown, encoded: %q", got)
	}
}

func TestEchoedUserInputIsEncoded(t *testing.T) {
	forceAscii(t)
	// A pasted filter carrying an escape sequence echoes as visible text.
	f := newFake(vmOff())
	s := start(t, modelWith(f, newStore(), dialRefused()))
	s.runes("\x1b[2Jpaste")
	s.shot()
	assertNoHostileBytes(t, "filter echo", s.frame)
	if !strings.Contains(s.frame, "<ESC>") {
		t.Fatalf("the pasted escape must echo as a marker:\n%s", s.frame)
	}
}

func TestHostileStoredSSHValueNeverReachesTheScreen(t *testing.T) {
	forceAscii(t)
	// Defense in depth: SSH values are validated on the way in, so an
	// alias carrying control bytes can never pass Configured() and be
	// offered anywhere. This test pins BOTH layers: the gate keeps the
	// hostile string off the screen entirely, and should any door ever
	// hand back hostile text anyway, the summary renders it through the
	// safe-display encoder rather than raw.
	off := vmOff()
	f := newFake(off)
	hostileAlias := "good\x1b[31mbad\u202e"
	st := storeWith(off.UUID, config.SSHConfig{Mode: config.SSHModeAlias, Alias: hostileAlias})
	g := newGuest()
	s := start(t, guestModel(f, st, g, off))
	s.shot()
	assertNoHostileBytes(t, "settings summary", s.frame)
	if strings.Contains(s.frame, "good") {
		t.Fatalf("a value ssh(1) could never use must not be rendered at all:\n%s", s.frame)
	}
	if !strings.Contains(s.frame, "not configured") {
		t.Fatalf("an unusable stored value must read as unset:\n%s", s.frame)
	}
	// The raw stored bytes survive untouched for the person fixing their
	// file: nothing here rewrote or sanitised the settings themselves.
	st.mu.Lock()
	raw := st.entries[config.CanonicalUUID(off.UUID)]
	st.mu.Unlock()
	if raw.Alias != hostileAlias {
		t.Fatalf("display policy must not mutate stored data: %+v", raw)
	}

	// And if a door DID hand the model hostile text (a future door, a
	// garbage-implementing one), the chokepoint encodes it before render.
	o := baseOptions(f, st, dialRefused())
	o.GuestSettings = g
	o.SSHSettings = hostileSSHDoor{}
	o.Direct = &DirectRun{Action: ActionSettings, VM: off}
	s2 := start(t, New(o))
	s2.shot()
	assertNoHostileBytes(t, "hostile-door summary", s2.frame)
	if !strings.Contains(s2.frame, "<ESC>") || !strings.Contains(s2.frame, "<U+202E>") {
		t.Fatalf("the summary must render hostile door text encoded:\n%s", s2.frame)
	}
}

// hostileSSHDoor pretends to be a settings source that ignores all input
// validation: it hands the model exactly what a hostile file or buggy
// door might contain, so the display layer's own defence can be pinned.
type hostileSSHDoor struct{}

func (hostileSSHDoor) SSH(string) (config.SSHConfig, bool) {
	return config.SSHConfig{Mode: config.SSHModeAlias, Alias: "alias\x1b[2Jevil\u202e"}, true
}
func (hostileSSHDoor) Store(string, string, config.SSHConfig) error { return nil }
func (hostileSSHDoor) Clear(string) error                           { return nil }

func TestSafeDisplayPolicyComposesIdempotently(t *testing.T) {
	// Every helper that turns external text into app-owned text shares the
	// one encoder: idempotent, complete, and composable — and the full
	// hostile string (the part the picker column may ellipsize) stays
	// fully marker-encoded end to end.
	hostile := "x\x1b[31m\x07y\u202e\r\nz\x00\u0085"
	lines := map[string]string{
		"nameOf":    nameOf(hostileMachine(hostile, "powered off")),
		"quoteName": quoteName(hostile),
		"oneError":  oneError(multilineError{}, 300),
		"raw":       safedisplay.Text(hostile),
	}
	for name, got := range lines {
		if strings.ContainsAny(got, "\x1b\x00\r\n\x07\x09") {
			t.Errorf("%s leaked control text: %q", name, got)
		}
		for _, r := range got {
			switch r {
			case '\u202e', '\u0085':
				t.Errorf("%s leaked hostile rune %U: %q", name, r, got)
			}
		}
	}
	once := safedisplay.Text(hostile)
	if twice := safedisplay.Text(once); twice != once {
		t.Errorf("encoding twice must be stable: %q vs %q", once, twice)
	}
	// ellip measures and cuts encoded text; a truncated hostile row can
	// never re-arm a sequence, because there is none left to slice.
	short := ellip(safedisplay.Text(hostile), 6)
	if strings.ContainsAny(short, "\x1b\x00\r") {
		t.Errorf("ellip must never reintroduce controls: %q", short)
	}
	// And the encoded full form proves nothing was silently dropped:
	// every hostile element has its marker.
	full := safedisplay.Text(hostile)
	for _, want := range []string{"<ESC>", "<07>", "<0D>", "<0A>", "<00>", "<U+0085>", "<U+202E>"} {
		if !strings.Contains(full, want) {
			t.Errorf("encoder dropped %s from %q", want, full)
		}
	}
}
