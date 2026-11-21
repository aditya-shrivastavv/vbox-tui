// Package safedisplay turns externally-supplied text — VM names, snapshot
// names and descriptions, VBoxManage output, error strings from subprocesses —
// into text that is safe to print in a terminal.
//
// The threat is display spoofing, not code execution: metadata an attacker
// controls (an imported OVA, a machine created by another tool) can carry ESC
// sequences, carriage returns, backspaces, or Bidirectional override
// characters. Printed raw, those can move the cursor, overwrite earlier text,
// fabricate rows the application never rendered, or make a name read as
// something it is not. The backend still addresses every operation by UUID;
// what this package protects is what a human reads while deciding.
//
// The policy is deliberately blunt: every control character, every escape
// introducing byte, every C1 byte, the BiDi controls that can reorder a line,
// and BOM/zero-width joiners are visibly encoded as <NN> / <U+XXXX>. Ordinary
// Unicode — letters of any script, spaces, punctuation, emoji, combining
// marks — passes through untouched, because VM names legitimately contain
// them. The raw value stays available to callers for exact resolution and
// command arguments; only the display copy goes through Text.
//
// Sanitising happens BEFORE styling and before width calculation: lipgloss
// never receives attacker bytes that could carry their own SGR sequences, and
// ansi.StringWidth then measures exactly what is printed.
//
// One surface is deliberately excluded: the interactive SSH terminal stream.
// During a handoff OpenSSH owns the terminal, and filtering what a shell
// prints there would break the very session the user asked for.
package safedisplay

import (
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Text encodes control characters, escape-introducing bytes, C1 controls,
// invisible and direction-splicing formatting characters, and lone surrogates
// as visible <..> markers. Ordinary printable text, including legitimate
// Unicode and combining marks, is returned unchanged.
func Text(s string) string {
	if s == "" {
		return s
	}
	if !utf8.ValidString(s) {
		// Never pass raw invalid bytes to a terminal: replace them the way
		// a scanning decoder would before encoding anything else.
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	start := -1
	for i, r := range s {
		if unsafeReason(r) != "" {
			start = i
			break
		}
	}
	if start < 0 {
		// The overwhelmingly common case: plain names allocate nothing new.
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	b.WriteString(s[:start])
	for _, r := range s[start:] {
		if why := unsafeReason(r); why != "" {
			b.WriteString(why)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// unsafeReason returns the visible encoding for r, or "" when r is safe to
// print as-is.
func unsafeReason(r rune) string {
	switch {
	case r == 0x1b:
		// ESC is called out by name: it introduces every CSI/OSC sequence,
		// and a reader seeing "<ESC>" knows the next printed characters came
		// from data, not from the program.
		return "<ESC>"
	case r < 0x20 || r == 0x7f:
		// Remaining C0 (SOH..US, including CR, LF, BS, TAB — TAB is encoded
		// because a raw backtab or cursor-left can reposition output) and
		// DEL.
		return angleHex(uint16(r))
	case r >= 0x80 && r <= 0x9f:
		// C1 controls. In UTF-8 these are the two-byte C2 range; some
		// terminals treat their byte equivalents (0x80-0x9f) or their 7-bit
		// introducers as live control input. Spelled as codepoints so a
		// reader never confuses <U+0085> with a two-digit C0 marker.
		return angleUnicode(r)
	case isFormatTrick(r):
		return angleUnicode(r)
	case r > 0x10FFFF || utf16.IsSurrogate(r):
		// Lone surrogates never survive real UTF-8, but a caller could hand
		// us one from []rune decoding; never emit raw.
		return angleUnicode(r)
	}
	return ""
}

// isFormatTrick lists the characters that can rewrite how a line reads
// without being visibly present: zero-width spacing, joiners, and every
// BiDi override/embedding/isolate/align marker.
func isFormatTrick(r rune) bool {
	switch r {
	case '\u200b', // zero width space
		'\u200c', // ZWNJ
		'\u200d', // ZWJ
		'\u200e', // LRM
		'\u200f', // RLM
		'\u202a', // LRE
		'\u202b', // RLE
		'\u202c', // PDF
		'\u202d', // LRO
		'\u202e', // RLO
		'\u2060', // word joiner
		'\u2061', '\u2062', '\u2063', '\u2064',
		'\u2066', '\u2067', '\u2068', '\u2069',
		'\u206a', '\u206b', '\u206c', '\u206d', '\u206e',
		'\ufeff': // BOM
		return true
	}
	return false
}

// angleHex renders C0/C1/DEL as <NN> using two upper-case hex digits.
func angleHex(v uint16) string {
	const hex = "0123456789ABCDEF"
	return "<" + string([]byte{hex[v>>4], hex[v&0xf]}) + ">"
}

// angleUnicode renders larger codepoints as <U+XXXX> (upper-case hex, at
// least four digits).
func angleUnicode(r rune) string {
	const hex = "0123456789ABCDEF"
	v := uint32(r)
	var digits []byte
	buf := [8]byte{}
	i := len(buf)
	for ; v > 0; v >>= 4 {
		i--
		buf[i] = hex[v&0xf]
	}
	digits = buf[i:]
	for len(digits) < 4 {
		digits = append([]byte{'0'}, digits...)
	}
	return "<U+" + string(digits) + ">"
}
