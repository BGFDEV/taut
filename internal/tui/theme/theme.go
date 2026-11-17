package theme

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
)

// Options selects the variant of the theme to build.
type Options struct {
	// Dark is true when the terminal background is dark.
	Dark bool
	// Unicode is false when only ASCII glyphs and borders may be used.
	Unicode bool
}

// Colors are the resolved color tokens for one background.
type Colors struct {
	Ink, Muted, Faint color.Color
	Surface, OnBrand  color.Color
	Brand, Accent     color.Color
	Success, Warning  color.Color
	Danger, Info      color.Color
}

// Styles are the named text styles components draw with. Each is derived
// only from tokens.
type Styles struct {
	Text     lipgloss.Style // body copy
	Muted    lipgloss.Style // secondary copy
	Faint    lipgloss.Style // rules, disabled
	Title    lipgloss.Style // screen and section titles
	Heading  lipgloss.Style // sub-headings inside a section
	Brand    lipgloss.Style // product name, focus
	Accent   lipgloss.Style // emphasized values
	Key      lipgloss.Style // a key cap or command to type
	Code     lipgloss.Style // literal commands, paths, IDs
	Success  lipgloss.Style
	Warning  lipgloss.Style
	Danger   lipgloss.Style
	Info     lipgloss.Style
	Badge    lipgloss.Style // filled label on brand background
	Selected lipgloss.Style // the focused row in a list
	Panel    lipgloss.Style // bordered container
}

// Theme is a fully resolved design system: tokens plus the styles derived
// from them. It is an immutable value; build a new one when the background
// changes.
type Theme struct {
	Dark    bool
	Unicode bool
	Color   Colors
	Space   Space
	Glyph   Glyphs
	Border  lipgloss.Border
	Style   Styles
}

// New resolves the tokens for the given options.
func New(o Options) Theme {
	ld := lipgloss.LightDark(o.Dark)
	c := func(p pair) color.Color { return ld(lipgloss.Color(p.light), lipgloss.Color(p.dark)) }

	t := Theme{
		Dark:    o.Dark,
		Unicode: o.Unicode,
		Space:   spacing,
		Color: Colors{
			Ink: c(palette.ink), Muted: c(palette.muted), Faint: c(palette.faint),
			Surface: c(palette.surface), OnBrand: c(palette.onBrand),
			Brand: c(palette.brand), Accent: c(palette.accent),
			Success: c(palette.success), Warning: c(palette.warning),
			Danger: c(palette.danger), Info: c(palette.info),
		},
	}
	if o.Unicode {
		t.Glyph, t.Border = glyphsUnicode, borderPanel
	} else {
		t.Glyph, t.Border = glyphsASCII, borderASCII
	}

	fg := func(col color.Color) lipgloss.Style { return lipgloss.NewStyle().Foreground(col) }
	t.Style = Styles{
		Text:     lipgloss.NewStyle(),
		Muted:    fg(t.Color.Muted),
		Faint:    fg(t.Color.Faint),
		Title:    lipgloss.NewStyle().Bold(true),
		Heading:  lipgloss.NewStyle().Bold(true),
		Brand:    fg(t.Color.Brand).Bold(true),
		Accent:   fg(t.Color.Accent),
		Key:      fg(t.Color.Accent).Bold(true),
		Code:     fg(t.Color.Brand),
		Success:  fg(t.Color.Success),
		Warning:  fg(t.Color.Warning),
		Danger:   fg(t.Color.Danger),
		Info:     fg(t.Color.Info),
		Badge:    lipgloss.NewStyle().Foreground(t.Color.OnBrand).Background(t.Color.Brand).Bold(true).Padding(0, t.Space.XS),
		Selected: lipgloss.NewStyle().Background(t.Color.Surface),
		Panel: lipgloss.NewStyle().
			Border(t.Border).
			BorderForeground(t.Color.Faint).
			Padding(0, t.Space.XS),
	}
	return t
}

// Default is the theme used before the terminal has been probed, and in
// tests: dark background, Unicode glyphs.
func Default() Theme { return New(Options{Dark: true, Unicode: true}) }

// Status is a semantic state shared by checks, jobs and report lines.
type Status int

// The statuses, in increasing order of severity.
const (
	StatusOK Status = iota
	StatusInfo
	StatusSkip
	StatusPending
	StatusWarn
	StatusFail
)

// String returns the lowercase machine name used in JSON output.
func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusInfo:
		return "info"
	case StatusSkip:
		return "skipped"
	case StatusPending:
		return "pending"
	case StatusWarn:
		return "warning"
	case StatusFail:
		return "failed"
	}
	return "unknown"
}

// MarshalText implements encoding.TextMarshaler so statuses serialize as
// their names.
func (s Status) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// StatusGlyph returns the unstyled glyph for a status.
func (t Theme) StatusGlyph(s Status) string {
	switch s {
	case StatusOK:
		return t.Glyph.OK
	case StatusInfo:
		return t.Glyph.Info
	case StatusSkip:
		return t.Glyph.Skip
	case StatusPending:
		return t.Glyph.Pending
	case StatusWarn:
		return t.Glyph.Warn
	default:
		return t.Glyph.Fail
	}
}

// StatusStyle returns the style for a status.
func (t Theme) StatusStyle(s Status) lipgloss.Style {
	switch s {
	case StatusOK:
		return t.Style.Success
	case StatusInfo:
		return t.Style.Info
	case StatusSkip, StatusPending:
		return t.Style.Muted
	case StatusWarn:
		return t.Style.Warning
	default:
		return t.Style.Danger
	}
}

// StatusMark returns the styled glyph for a status, padded to a fixed width
// so labels that follow it line up in both glyph sets.
func (t Theme) StatusMark(s Status) string {
	g := t.StatusGlyph(s)
	w := 1
	if !t.Unicode {
		w = 2
	}
	if pad := w - lipgloss.Width(g); pad > 0 {
		g += strings.Repeat(" ", pad)
	}
	return t.StatusStyle(s).Render(g)
}

// Rule returns a horizontal rule of the given width.
func (t Theme) Rule(width int) string {
	if width <= 0 {
		return ""
	}
	return t.Style.Faint.Render(strings.Repeat(t.Glyph.Rule, width))
}
