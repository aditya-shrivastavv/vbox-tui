package safedisplay

import (
	"strings"
	"testing"
)

// The package contract: attacker text must never carry live terminal
// instructions, and legitimate text must survive untouched.

func TestPlainNamesPassThroughUnchanged(t *testing.T) {
	for _, s := range []string{
		"Kali Linux",
		"开发机 🐧 dev-box",
		"Workbench — Win11",
		"Ünïcødé 日本語虚拟机",
		"é",    // combining marks are ordinary text
		"café", // precomposed
		`back\slash:colon`,
		"quote\"inside",
		"(parens) [brackets] {braces}",
		"trailing punctuation!",
	} {
		if got := Text(s); got != s {
			t.Errorf("Text(%q) = %q, want unchanged", s, got)
		}
	}
}

func TestControlCharactersAreEncoded(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"a\x1b[2Jb", "a<ESC>[2Jb"},
		{"name\r\nnew line", "name<0D><0A>new line"},
		{"over\x08write", "over<08>write"},
		{"tab\there", "tab<09>here"},
		{"bell\x07", "bell<07>"},
		{"nul\x00end", "nul<00>end"},
		{"del\x7f", "del<7F>"},
		{"osc\x1b]52;c;aGVsbG8=\x07clip", "osc<ESC>]52;c;aGVsbG8=<07>clip"},
		{"c1\u0085next", "c1<U+0085>next"},
	}
	for _, c := range cases {
		if got := Text(c.raw); got != c.want {
			t.Errorf("Text(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestBiDiAndZeroWidthControlsAreEncoded(t *testing.T) {
	// The classic trojan-source trick: an override that makes the tail of a
	// name render as its front.
	hostile := "safe\u202emalicious-suffix"
	got := Text(hostile)
	if !strings.Contains(got, "<U+202E>") {
		t.Errorf("RLO must be visible, got %q", got)
	}
	for _, r := range []rune{'\u200b', '\u200c', '\u200d', '\u200e', '\u200f',
		'\u202a', '\u202b', '\u202c', '\u202d',
		'\u2066', '\u2067', '\u2068', '\u2069', '\ufeff'} {
		g := Text("x" + string(r) + "y")
		if strings.ContainsAny(g, "\u200b\u200c\u200d\u200e\u200f\u202a\u202b\u202c\u202d\u2066\u2067\u2068\u2069\ufeff") {
			t.Errorf("%U survived in %q", r, g)
		}
	}
}

func TestNoESCByteEverSurvives(t *testing.T) {
	// The single hard invariant: the encoded output contains no live ESC,
	// no C0 other than nothing (all encoded), and no valid CSI/OSC head.
	hostile := "\x1b]0;poisoned title\x07\x1b[1;1Hfaked row\x1b[2K\x1b[?25l\u202e"
	got := Text(hostile)
	if strings.ContainsAny(got, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f"+
		"\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f\x7f") {
		t.Fatalf("a control byte survived into %q", got)
	}
	if strings.Contains(got, "\x1b[") || strings.Contains(got, "\x1b]") {
		t.Fatalf("a live escape sequence survived into %q", got)
	}
}

func TestTextIsIdempotent(t *testing.T) {
	for _, s := range []string{"plain", "a\x1b[31mred", "\u202emixed\x07", "<ESC>already marked", "开发 🐧"} {
		once := Text(s)
		if twice := Text(once); twice != once {
			t.Errorf("Text not idempotent for %q: %q vs %q", s, once, twice)
		}
	}
}

func TestInvalidUTF8CannotSmuggleControl(t *testing.T) {
	// 0x5b('[') after a truncated multibyte lead: the raw byte string would
	// contain a live '[' and U+FFFD; more importantly a truncated lead must
	// never leave the ESC byte (0x1b) intact, whichever position it hides in.
	raw := "a\xc3\x1b[" // invalid 2-byte lead followed by ESC
	got := Text(raw)
	if strings.Contains(got, "\x1b") {
		t.Fatalf("ESC smuggled through invalid UTF-8: %q", got)
	}
}

func TestLoneSurrogateRuneIsEncoded(t *testing.T) {
	// Surrogates never arrive through []byte strings, but unsafeReason is
	// defensive for direct rune callers.
	if unsafeReason(0xD800) == "" {
		t.Fatal("a lone surrogate must be encoded")
	}
}

func TestFastPathMatchesEncoder(t *testing.T) {
	// Every string with an unsafe rune must take the encoding path: compare
	// against a forced slow walk.
	samples := []string{
		"", "clean", "esc\x1bmiddle", "\x00", "tail\x1b",
		"日本語", "\u202e", "a\rb", "ok:127.0.0.1",
	}
	for _, s := range samples {
		got := Text(s)
		walked := forceEncode(s)
		if got != walked {
			t.Errorf("fast path disagrees with walker for %q: %q vs %q", s, got, walked)
		}
	}
}

// forceEncode encodes unconditionally, mirroring Text's slow path without the
// early exit, so the two implementations must agree.
func forceEncode(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	var b strings.Builder
	for _, r := range s {
		if why := unsafeReason(r); why != "" {
			b.WriteString(why)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
