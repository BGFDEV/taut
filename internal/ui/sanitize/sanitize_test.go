package sanitize_test

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/bgfdev/taut/internal/ui/sanitize"
)

func TestText(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain", "graph: 403 Forbidden", "graph: 403 Forbidden"},
		{"empty", "", ""},
		{"sgr", "\x1b[31mred\x1b[0m alert", "red alert"},
		{"cursor and clear", "a\x1b[2J\x1b[Hb", "ab"},
		{"osc title", "x\x1b]0;pwned\x07y", "xy"},
		{"osc 52 clipboard", "x\x1b]52;c;AAAA\x07y", "xy"},
		{"osc 8 link", "\x1b]8;;http://evil.example\x1b\\click\x1b]8;;\x1b\\", "click"},
		{"dcs", "x\x1bPq#0;2;0;0;0\x1b\\y", "xy"},
		{"apc and pm", "a\x1b_apc\x1b\\b\x1b^pm\x07c", "abc"},
		{"osc ended by c1 st", "x\x1b]0;t\u009cy", "xy"},
		{"charset selection", "a\x1b(Bb", "ab"},
		{"two-character escape", "a\x1bcb", "ab"},
		{"c1 csi", "a\u009b31mb", "a31mb"},
		{"raw c1 byte", "a\x9b2Jb", "a2Jb"},
		{"bare controls", "a\x00b\x07c\x08d\x7fe", "abcde"},
		{"bare carriage return", "safe\rEVIL", "safe EVIL"},
		{"line breaks", "one\r\ntwo\n\tthree", "one two three"},
		{"bidi override", "user\u202Etxt.exe", "usertxt.exe"},
		{"bidi isolate", "a\u2066b\u2069c", "abc"},
		{"zero width", "ad\u200Bmin\uFEFF", "admin"},
		{"soft hyphen", "pass\u00ADword", "password"},
		{"deprecated format characters", "a\u206Ab\u206Fc", "abc"},
		{"arabic letter mark", "a\u061Cb", "ab"},
		{"tag characters", "a\U000E0041b", "ab"},
		{"invalid utf8", "a\xffb", "ab"},
		{"unicode kept", "Zoë → 東京 ✓ 🎉", "Zoë → 東京 ✓ 🎉"},
		{"arabic number sign kept", "\u0600123", "\u0600123"},
		{"spaces collapsed", "  a   b  ", "a b"},
		// A sequence that never ends costs only its introducer: the text
		// after it is still there to read.
		{"unterminated osc", "x\x1b]0;never ended", "x0;never ended"},
		{"unterminated dcs", "x\x1bPq rest of the field", "xq rest of the field"},
		{"unterminated csi", "x\x1b[12;", "x12;"},
		{"osc cut short by another escape", "x\x1b]0;t\x1b[2Jy", "x0;ty"},
		{"lone escape", "tail\x1b", "tail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitize.Text(tc.in); got != tc.want {
				t.Errorf("Text(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A control string longer than any real one is treated as unterminated, so
// a terminator far away cannot make a whole field disappear.
func TestTextBoundsControlStrings(t *testing.T) {
	long := strings.Repeat("a", 5000)
	if got := sanitize.Text("x\x1b]0;" + long + "\x07y"); got != "x0;"+long+"y" {
		t.Errorf("a %d-byte OSC was swallowed: got %d bytes", len(long), len(got))
	}
	short := strings.Repeat("a", 1000)
	if got := sanitize.Text("x\x1b]0;" + short + "\x07y"); got != "xy" {
		t.Errorf("an ordinary OSC was kept: %q", got)
	}
}

func TestLines(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain", "one\ntwo", "one\ntwo"},
		{"indentation kept", "problems:\n  - first\n  - second", "problems:\n  - first\n  - second"},
		{"crlf", "one\r\ntwo", "one\ntwo"},
		{"bare carriage return", "safe\rEVIL", "safe EVIL"},
		{"escapes", "a\x1b[2J\nb\x1b]0;t\x07", "a\nb"},
		{"tab", "a\tb", "a b"},
		{"line separators", "a\u2028b\u2029c\u0085d", "a b c d"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitize.Lines(tc.in); got != tc.want {
				t.Errorf("Lines(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Whatever goes in, nothing that can drive a terminal comes out, and
// sanitizing twice changes nothing more.
func TestNeverEmitsControls(t *testing.T) {
	inputs := []string{
		"\x1b", "\x1b[", "\x1b]0;never terminated", "\x1bP1$r\x1b\\", "\x9b1;1H", "\u009b1;1H", "\x1b[38;2;1;2;3m",
		"tail\x1b", "\x1b\x1b\x1b[[[", "a\u2028b\u2029c", "\x1b_apc\x1b\\rest", "\x1b^pm\x07x", "\x1b]\x1b]\x1b]", "\x1b \x1b#\x1b(",
		"\x1b]0;a\x18b", "\u0090q\u009c", "\u009d0;t\u0007", "\x1b[\x1b[2J", "\x1bP\x1bP\x1b\\", "a\x1b]52;c;" + strings.Repeat("QUJD", 2000),
	}
	var all strings.Builder
	for r := rune(0); r < 0x3000; r++ {
		all.WriteRune(r)
	}
	inputs = append(inputs, all.String(), "\U000E0001\U000E007F\uFFF9\uFFFA\uFFFB\uFEFF")
	for _, in := range inputs {
		for name, fn := range map[string]func(string) string{"Text": sanitize.Text, "Lines": sanitize.Lines} {
			out := fn(in)
			if !utf8.ValidString(out) {
				t.Errorf("%s(%.20q) is not valid UTF-8", name, in)
			}
			for _, r := range out {
				keep := r == ' ' || (r == '\n' && name == "Lines")
				if !keep && (unicode.IsControl(r) || unicode.IsSpace(r) || (r >= 0x200B && r <= 0x200F) || (r >= 0x202A && r <= 0x202E) ||
					(r >= 0x2060 && r <= 0x206F) || r == 0x00AD || r == 0x061C || r == 0xFEFF) {
					t.Errorf("%s(%.20q) kept %U", name, in, r)
				}
			}
			if again := fn(out); again != out {
				t.Errorf("%s is not idempotent on %.20q: %q then %q", name, in, out, again)
			}
		}
	}
}

func TestHasControl(t *testing.T) {
	for in, want := range map[string]bool{
		"general": false, "": false, "Zoë 東京": false, "dev\u200Dops": false,
		"a\x1b[2J": true, "a\rb": true, "a\nb": true, "a\x7f": true, "a\u009bb": true, "a\xffb": true, "tab\there": true,
	} {
		if got := sanitize.HasControl(in); got != want {
			t.Errorf("HasControl(%q) = %v, want %v", in, got, want)
		}
	}
}
