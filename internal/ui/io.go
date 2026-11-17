// Package ui is the output layer every command writes through.
//
// A command produces one result; ui decides how it reaches the operator:
// a full-screen TUI on an interactive terminal, styled plain text when output
// is piped or --yes is set, or a single JSON document under --json. Commands
// never write to os.Stdout directly and never test for a TTY themselves.
package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/term"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/tui/theme"
)

// Mode is how results are presented.
type Mode int

const (
	// ModeInteractive: stdin and stdout are terminals; prompts and
	// full-screen views are allowed.
	ModeInteractive Mode = iota
	// ModePlain: styled (or unstyled) text, no prompts.
	ModePlain
	// ModeJSON: exactly one JSON document on stdout, nothing else.
	ModeJSON
)

// Options are the global flags that influence presentation.
type Options struct {
	JSON    bool // --json
	Yes     bool // --yes: never prompt, accept defaults
	NoColor bool // --no-color
}

// IO carries the streams, presentation mode and theme for one invocation.
type IO struct {
	In    io.Reader
	Out   io.Writer // stdout, color-downsampled for the terminal
	Err   io.Writer // stderr, color-downsampled for the terminal
	Mode  Mode
	Yes   bool
	Theme theme.Theme
	// Width is the usable line width for plain reports.
	Width int
	// Profile is the color capability of stdout.
	Profile colorprofile.Profile

	rawOut io.Writer
	env    []string
}

type fder interface{ Fd() uintptr }

func isTTY(v any) bool {
	f, ok := v.(fder)
	return ok && term.IsTerminal(f.Fd())
}

// New probes the environment and builds an IO. Pass os.Stdin, os.Stdout,
// os.Stderr and os.Environ() in production; tests pass buffers, which are
// never terminals and therefore get deterministic unstyled output.
func New(in io.Reader, out, errw io.Writer, env []string, o Options) *IO {
	inTTY, outTTY := isTTY(in), isTTY(out)

	u := &IO{In: in, rawOut: out, env: env, Yes: o.Yes, Width: 80}
	switch {
	case o.JSON:
		u.Mode = ModeJSON
	case inTTY && outTTY && !o.Yes:
		u.Mode = ModeInteractive
	default:
		u.Mode = ModePlain
	}

	outW := colorprofile.NewWriter(out, env)
	errW := colorprofile.NewWriter(errw, env)
	if o.NoColor {
		if outW.Profile > colorprofile.ASCII {
			outW.Profile = colorprofile.ASCII
		}
		if errW.Profile > colorprofile.ASCII {
			errW.Profile = colorprofile.ASCII
		}
	}
	u.Out, u.Err, u.Profile = outW, errW, outW.Profile

	if outTTY {
		if f, ok := out.(fder); ok {
			if w, _, err := term.GetSize(f.Fd()); err == nil && w > 0 {
				u.Width = w
			}
		}
	}
	if u.Width > theme.MaxContentWidth {
		u.Width = theme.MaxContentWidth
	}

	u.Theme = theme.New(theme.Options{
		Dark:    detectDark(env),
		Unicode: detectUnicode(env),
	})
	return u
}

// Test returns an IO writing unstyled output to the given writers, for use
// in tests. Mode is plain unless jsonMode is set.
func Test(out, errw io.Writer, jsonMode bool) *IO {
	u := New(strings.NewReader(""), out, errw, []string{"TERM=dumb", "TAUT_GLYPHS=unicode"}, Options{JSON: jsonMode})
	return u
}

func lookup(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], key+"="); ok {
			return v
		}
	}
	return ""
}

// detectDark reports whether to start with the dark variant.
//
// It deliberately does not query the terminal. Asking for the background
// color means writing an escape sequence and waiting for a reply, and a
// terminal that never replies (a CI pty, `script`, some multiplexers) would
// hang every command at startup. Instead:
//
//   - TAUT_THEME=light|dark always wins.
//   - COLORFGBG, which several terminals export, is used as a hint.
//   - Otherwise dark is assumed, because most terminals are.
//
// Interactive views then correct themselves: Bubble Tea asks the terminal
// asynchronously and the views restyle when (and only if) it answers.
func detectDark(env []string) bool {
	switch strings.ToLower(lookup(env, "TAUT_THEME")) {
	case "light":
		return false
	case "dark":
		return true
	}
	// COLORFGBG is "fg;bg" (sometimes "fg;default;bg") in ANSI color numbers.
	if v := lookup(env, "COLORFGBG"); v != "" {
		parts := strings.Split(v, ";")
		switch parts[len(parts)-1] {
		case "7", "15":
			return false
		}
	}
	return true
}

// ThemeLocked reports whether the operator pinned the theme with TAUT_THEME,
// in which case views must not follow the terminal's background.
func (u *IO) ThemeLocked() bool {
	v := strings.ToLower(lookup(u.env, "TAUT_THEME"))
	return v == "light" || v == "dark"
}

// SetDark switches the theme variant. Interactive views call it once the
// terminal has reported its background, so that anything printed afterwards
// matches.
func (u *IO) SetDark(dark bool) {
	if u.ThemeLocked() || dark == u.Theme.Dark {
		return
	}
	u.Theme = theme.New(theme.Options{Dark: dark, Unicode: u.Theme.Unicode})
}

// detectUnicode reports whether the locale can display non-ASCII glyphs.
// TAUT_GLYPHS=ascii|unicode overrides detection.
func detectUnicode(env []string) bool {
	switch lookup(env, "TAUT_GLYPHS") {
	case "ascii":
		return false
	case "unicode":
		return true
	}
	if lookup(env, "TERM") == "dumb" {
		return false
	}
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := strings.ToUpper(lookup(env, k)); v != "" {
			return strings.Contains(v, "UTF-8") || strings.Contains(v, "UTF8")
		}
	}
	// No locale set at all: modern terminals default to UTF-8.
	return true
}

// Interactive reports whether prompts and full-screen views are allowed.
func (u *IO) Interactive() bool { return u.Mode == ModeInteractive }

// IsJSON reports whether output must be a single JSON document.
func (u *IO) IsJSON() bool { return u.Mode == ModeJSON }

// Env returns the environment the IO was built with.
func (u *IO) Env() []string { return u.env }

// Println writes a line to stdout. It is a no-op in JSON mode, where stdout
// is reserved for the result document.
func (u *IO) Println(a ...any) {
	if u.Mode == ModeJSON {
		return
	}
	fmt.Fprintln(u.Out, a...)
}

// Status writes a progress note to stderr so it never mixes with results.
// Suppressed in JSON mode.
func (u *IO) Status(format string, a ...any) {
	if u.Mode == ModeJSON {
		return
	}
	fmt.Fprintln(u.Err, Pad(u.Theme.Style.Muted.Render(fmt.Sprintf(format, a...))))
}

// JSON writes v as an indented JSON document to stdout.
func (u *IO) JSON(v any) error {
	enc := json.NewEncoder(u.rawOut)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// Error reports err to the operator in the current mode: a JSON document on
// stdout in JSON mode, otherwise a what/why/next block on stderr.
func (u *IO) Error(err error) {
	fe := fail.From(err)
	if fe == nil || fe.Quiet {
		return
	}
	if u.Mode == ModeJSON {
		_ = u.JSON(struct {
			Error *fail.Error `json:"error"`
		}{fe})
		return
	}
	fmt.Fprintln(u.Err, "\n"+Pad(ErrorBlock(u.Theme, fe, u.Width-Margin)))
}
