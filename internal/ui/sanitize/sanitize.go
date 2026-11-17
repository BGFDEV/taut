// Package sanitize makes text Taut did not write safe to draw on a terminal.
//
// Channel and user names come from a Slack export, error text from Microsoft
// Graph, holder and scope names from the bus: none of it is ours, and all of
// it ends up on an operator's screen. Sanitizing happens once, where such
// text crosses into the part of Taut that renders it, not at every place it
// is drawn.
//
// The package depends on nothing but the standard library so the control
// plane can use it without linking the terminal UI.
package sanitize

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxString bounds how much text a terminated control string (OSC, DCS,
// SOS, PM, APC) may swallow, in runes. Real ones are window titles, links
// and clipboard payloads; anything longer is treated as unterminated.
const maxString = 4096

// maxCSI bounds the parameters of a CSI sequence, in runes.
const maxCSI = 256

// Text returns s as a single line that is safe to draw: escape sequences
// are removed, as are control characters and the invisible Unicode format
// characters that reorder or hide text, so nothing in s can move the
// cursor, restyle the screen, set the window title, write the clipboard or
// disguise what a row says. Line breaks, tabs and runs of spaces become one
// space, and the result is trimmed. Bytes that are not UTF-8 are dropped.
//
// A control string that is never terminated loses only its introducer; the
// text after it is kept, so a stray escape cannot swallow the rest of a
// field.
func Text(s string) string {
	if clean(s) {
		return s
	}
	return strings.Join(strings.Fields(scrub(s, false)), " ")
}

// Lines is Text for prose whose line structure matters, such as an error
// with one problem per line: line breaks and leading indentation are kept,
// everything else is treated as Text treats it. A carriage return never
// survives.
func Lines(s string) string {
	if clean(s) {
		return s
	}
	return scrub(s, true)
}

// HasControl reports whether s contains a control character (C0, DEL or
// C1, which includes ESC) or is not valid UTF-8. It is the test for "this
// name was not typed by a person": Slack accepts no such character in a
// channel name, a user name or an ID. Invisible format characters are not
// counted, because names in some scripts legitimately contain them.
func HasControl(s string) bool {
	if !utf8.ValidString(s) {
		return true
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// clean is the fast path: printable ASCII with single interior spaces needs
// no work.
func clean(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c >= 0x7f {
			return false
		}
		if c == ' ' && (i == 0 || i == len(s)-1 || s[i+1] == ' ') {
			return false
		}
	}
	return true
}

func scrub(s string, keepLines bool) string {
	// Bytes that are not UTF-8 are dropped, not shown as U+FFFD: a raw C1
	// byte is a control, not a character.
	rs := []rune(strings.ToValidUTF8(s, ""))
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(rs); {
		r := rs[i]
		if n := escape(rs[i:]); n > 0 {
			i += n
			continue
		}
		i++
		switch {
		case r == '\n' && keepLines:
			b.WriteByte('\n')
		case r == '\r' && keepLines && i < len(rs) && rs[i] == '\n':
			// CRLF is one line break.
		case unicode.IsSpace(r):
			// Includes \r, \t, NEL and the Unicode line and paragraph
			// separators.
			b.WriteByte(' ')
		case unicode.IsControl(r), invisible(r):
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// escape returns the length in runes of the escape sequence at the start of
// rs, or 0 if there is none. A sequence that is malformed or never ends has
// the length of its introducer alone. The C1 forms of the introducers
// (U+009B and friends) are not parsed: they are dropped as controls, and
// what followed them stays visible as the plain text it then is.
func escape(rs []rune) int {
	if rs[0] != 0x1b {
		return 0
	}
	if len(rs) == 1 {
		return 1
	}
	switch next := rs[1]; {
	case next == '[':
		return 2 + csi(rs[2:])
	case next == ']' || next == 'P' || next == 'X' || next == '^' || next == '_':
		return 2 + control(rs[2:])
	case next >= 0x20 && next <= 0x2f:
		// ESC, intermediates, final: charset selection and the like.
		for i := 2; i < len(rs) && i < 8; i++ {
			if rs[i] >= 0x30 && rs[i] <= 0x7e {
				return i + 1
			}
			if rs[i] < 0x20 || rs[i] > 0x2f {
				break
			}
		}
		return 1
	case next >= 0x30 && next <= 0x7e:
		return 2 // a two-character function such as ESC c or ESC \
	}
	return 1
}

// csi returns the length of the parameters, intermediates and final byte
// that follow a CSI introducer, or 0 if they do not form a sequence.
func csi(rs []rune) int {
	i := 0
	for i < len(rs) && i < maxCSI && rs[i] >= 0x30 && rs[i] <= 0x3f {
		i++
	}
	for i < len(rs) && i < maxCSI && rs[i] >= 0x20 && rs[i] <= 0x2f {
		i++
	}
	if i < len(rs) && rs[i] >= 0x40 && rs[i] <= 0x7e {
		return i + 1
	}
	return 0
}

// control returns the length of a control string's payload and terminator:
// BEL or ST (ESC \ or U+009C). It returns 0, so that only the introducer is
// dropped and the payload stays visible as text, when the string has no
// terminator within maxString or another escape starts before it ends.
func control(rs []rune) int {
	for i := 0; i < len(rs) && i < maxString; i++ {
		switch rs[i] {
		case 0x07, 0x9c:
			return i + 1
		case 0x1b:
			if i+1 < len(rs) && rs[i+1] == '\\' {
				return i + 2
			}
			return 0
		}
	}
	return 0
}

// invisible reports whether r is a character that draws nothing but can
// reorder, hide or join the text around it: the whole Unicode format
// category (soft hyphen, zero-width characters, directional marks,
// embeddings, overrides and isolates, deprecated format characters, tags,
// the byte order mark, interlinear annotation), except the few format
// characters that are visible signs in the scripts that use them.
func invisible(r rune) bool {
	if !unicode.Is(unicode.Cf, r) {
		// Unassigned code points inside the invisible blocks, so a later
		// Unicode version cannot open a gap.
		return r == 0x2065 || (r >= 0xFFF0 && r <= 0xFFF8) || (r >= 0xE0000 && r <= 0xE007F)
	}
	switch {
	case r >= 0x0600 && r <= 0x0605, r == 0x06DD, r == 0x070F, r == 0x08E2, // Arabic and Syriac number signs and marks
		r == 0x0890, r == 0x0891,
		r == 0x110BD, r == 0x110CD: // Kaithi number signs
		return false
	}
	return true
}
