// Package theme is Taut's design system for the terminal.
//
// This file is the single source of design tokens: every color, spacing
// step, border and glyph used anywhere in the CLI or TUI is defined here and
// nowhere else. Components consume tokens through Theme (see theme.go); they
// never construct a color or pick a glyph themselves.
package theme

import "charm.land/lipgloss/v2"

// ---------------------------------------------------------------------------
// Color
//
// Each token is a light/dark pair. The palette is restrained: a teal
// brand color, a brass accent, and four status hues chosen to stay
// distinguishable for the common forms of color blindness. Status is never
// conveyed by color alone; every status also has a glyph.
// ---------------------------------------------------------------------------

type pair struct{ light, dark string }

var palette = struct {
	// Text
	ink   pair // primary text
	muted pair // secondary text, descriptions
	faint pair // rules, borders, disabled

	// Surfaces
	surface pair // selected rows, panels
	onBrand pair // text placed on a brand-colored background

	// Identity
	brand  pair // titles, focus, primary actions
	accent pair // highlights, keys in the help bar

	// Status
	success pair
	warning pair
	danger  pair
	info    pair
}{
	// Body text is never given a color: it uses the terminal's own
	// foreground, so it is always readable whatever the user's theme. ink
	// exists only for the few places that need an explicit color, such as
	// text on a filled background.
	ink: pair{"#1B2733", "#E6EDF3"},
	// The "dark" values are mid-tones on purpose. Taut cannot always know
	// the terminal's background (it is never queried at startup, since a
	// silent terminal would hang the command), and a
	// mid-tone stays legible if the guess is wrong.
	muted: pair{"#5B6B7A", "#8792A0"},
	faint: pair{"#A3AFBB", "#5E6875"},

	surface: pair{"#E9EEF2", "#262D36"},
	onBrand: pair{"#FFFFFF", "#06222A"},

	brand:  pair{"#0B7285", "#22B8CF"},
	accent: pair{"#946200", "#D9A441"},

	success: pair{"#2B8A3E", "#40B95F"},
	warning: pair{"#B35C00", "#E0932B"},
	danger:  pair{"#C92A2A", "#F0655F"},
	info:    pair{"#1864AB", "#5BA8F0"},
}

// ---------------------------------------------------------------------------
// Spacing
//
// A small scale in terminal cells. Horizontal steps are columns, vertical
// steps are rows. Layouts use these names rather than literal numbers.
// ---------------------------------------------------------------------------

// Space is the spacing scale.
type Space struct {
	None int // 0
	XS   int // 1: between a glyph and its label
	SM   int // 2: gutters, list indentation
	MD   int // 3: between columns
	LG   int // 4: section indentation
	XL   int // 6: page margins on wide terminals
}

var spacing = Space{None: 0, XS: 1, SM: 2, MD: 3, LG: 4, XL: 6}

// Layout constants shared by every full-screen view.
const (
	// MinWidth and MinHeight are the smallest terminal the TUI will draw
	// in; below this the shell shows a "terminal too small" panel.
	MinWidth  = 60
	MinHeight = 14
	// MaxContentWidth caps line length so reports stay readable on very
	// wide terminals.
	MaxContentWidth = 110
)

// ---------------------------------------------------------------------------
// Borders
// ---------------------------------------------------------------------------

var (
	borderPanel = lipgloss.RoundedBorder()
	borderASCII = lipgloss.Border{
		Top: "-", Bottom: "-", Left: "|", Right: "|",
		TopLeft: "+", TopRight: "+", BottomLeft: "+", BottomRight: "+",
	}
)

// ---------------------------------------------------------------------------
// Glyphs
//
// Two complete sets: Unicode for capable terminals and ASCII for everything
// else (TERM=dumb, non-UTF-8 locales, logs). The sets have identical fields
// so no caller ever branches on capability.
// ---------------------------------------------------------------------------

// Glyphs is the symbol set.
type Glyphs struct {
	Mark     string // brand mark shown beside the product name
	OK       string
	Fail     string
	Warn     string
	Info     string
	Skip     string
	Pending  string
	Bullet   string
	Pointer  string // selection cursor
	Arrow    string // "maps to"
	Elbow    string // connects a follow-up line to the row above it
	Ellipsis string
	Rule     string // horizontal rule
	Sep      string // inline separator
	TreeMid  string
	TreeEnd  string
	TreeBar  string
	BarFull  string
	BarEmpty string
	Spark    []string // eight levels, low to high
	Spinner  []string
}

var glyphsUnicode = Glyphs{
	Mark:     "◆",
	OK:       "✓",
	Fail:     "✗",
	Warn:     "!",
	Info:     "●",
	Skip:     "○",
	Pending:  "◌",
	Bullet:   "•",
	Pointer:  "❯",
	Arrow:    "→",
	Elbow:    "└",
	Ellipsis: "…",
	Rule:     "─",
	Sep:      "·",
	TreeMid:  "├─",
	TreeEnd:  "└─",
	TreeBar:  "│ ",
	BarFull:  "━",
	BarEmpty: "─",
	Spark:    []string{"▁", "▂", "▃", "▄", "▅", "▆", "▇", "█"},
	Spinner:  []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
}

var glyphsASCII = Glyphs{
	Mark:     "*",
	OK:       "ok",
	Fail:     "x",
	Warn:     "!",
	Info:     "i",
	Skip:     "-",
	Pending:  ".",
	Bullet:   "*",
	Pointer:  ">",
	Arrow:    "->",
	Elbow:    "`-",
	Ellipsis: "...",
	Rule:     "-",
	Sep:      "|",
	TreeMid:  "|-",
	TreeEnd:  "`-",
	TreeBar:  "| ",
	BarFull:  "=",
	BarEmpty: "-",
	Spark:    []string{"_", "_", ".", "-", "-", "=", "=", "#"},
	Spinner:  []string{"|", "/", "-", "\\"},
}
