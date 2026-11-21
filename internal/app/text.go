package app

import (
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/aditya-shrivastavv/vbox-tui/internal/safedisplay"
)

// clamp constrains v to [lo, hi].
func clamp(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ellip truncates s to at most n printed cells, appending "~" when anything was
// removed. ansi.Truncate is grapheme- and wide-character-aware: an emoji or a
// CJK glyph is never sliced in half. The caller's contract is that s has
// already passed through safedisplay.Text, so no live escape is left: the
// encoder turns control characters into visible <..> markers first, and only
// then does anything measure or cut — an escape sequence is never "preserved
// intact" through a truncation, because that is precisely the spoofing path
// this app refuses. Ellipsis is the only place a VM name is ever cut, and
// only when the line genuinely cannot fit the terminal.
func ellip(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= n {
		return s
	}
	if n == 1 {
		return "~"
	}
	return ansi.Truncate(s, n, "~")
}

// oneError flattens a (possibly multi-line, from VBoxManage) error onto one
// readable line, bounded so a stack of VirtualBox chatter cannot flood the
// scrollback. The result is safe-display encoded: subprocess stderr is
// external text, and VirtualBox quotes machine paths and names into its
// messages, so an error string must not reach the terminal as live control
// instructions either.
func oneError(err error, max int) string {
	if err == nil {
		return ""
	}
	s := strings.Join(strings.Fields(err.Error()), " ")
	s = safedisplay.Text(s)
	r := []rune(s)
	if len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}
