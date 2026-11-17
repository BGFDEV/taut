// Package report presents a multi-section document, such as the output of
// `taut preflight`, in every mode from one definition: as a tabbed, scrollable
// screen in the shell, or as a plain top-to-bottom report.
package report

import (
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/bgfdev/taut/internal/tui/shell"
	"github.com/bgfdev/taut/internal/tui/theme"
	"github.com/bgfdev/taut/internal/ui"
)

// Section is one part of a document.
type Section struct {
	// Title is the tab label and the plain-report heading.
	Title string
	// Count, when non-empty, is shown beside the title (e.g. "3").
	Count string
	// Status colors the count: a warning or failure draws the eye to the
	// sections that need attention.
	Status theme.Status
	// Render draws the section body at the given width.
	Render func(t theme.Theme, width int) string
}

// Document is a titled list of sections.
type Document struct {
	Command  string // e.g. "preflight"
	Subtitle string // e.g. the config name
	Sections []Section
	// Footer is rendered after the last section in plain mode only, for a
	// closing verdict and next step.
	Footer func(t theme.Theme, width int) string
}

// Plain renders the whole document top to bottom, inside the standard
// margin.
func (d Document) Plain(t theme.Theme, width int) string {
	inner := width - ui.Margin
	var b strings.Builder
	b.WriteString("\n" + ui.Banner(t, d.Command, d.Subtitle))
	for _, s := range d.Sections {
		note := ""
		if s.Count != "" {
			note = t.StatusStyle(s.Status).Render(s.Count)
		}
		head := ui.Heading(t, s.Title, inner)
		if note != "" {
			head += "  " + note
		}
		b.WriteString("\n\n" + head + "\n")
		b.WriteString(ui.Indent(strings.TrimRight(s.Render(t, inner-2), "\n"), 2))
	}
	if d.Footer != nil {
		b.WriteString("\n\n" + strings.TrimRight(d.Footer(t, inner), "\n"))
	}
	return ui.Pad(b.String())
}

// Screen is the interactive form of a Document.
type Screen struct {
	doc    Document
	active int
	vp     viewport.Model
	theme  theme.Theme
	keys   shell.Keys
	width  int
	height int
	ready  bool
}

// NewScreen returns a shell screen for d.
func NewScreen(d Document) *Screen {
	vp := viewport.New()
	// Left/right switch sections here, so the viewport must not scroll
	// horizontally on them.
	vp.KeyMap.Left = key.NewBinding()
	vp.KeyMap.Right = key.NewBinding()
	return &Screen{doc: d, vp: vp, theme: theme.Default(), keys: shell.DefaultKeys()}
}

// Title implements shell.Screen.
func (s *Screen) Title() string {
	if len(s.doc.Sections) == 0 {
		return ""
	}
	return s.doc.Sections[s.active].Title
}

// Init implements shell.Screen.
func (s *Screen) Init() tea.Cmd { return nil }

// tabRows is the height of the tab strip plus the blank line under it.
const tabRows = 2

func (s *Screen) refresh(resetScroll bool) {
	if !s.ready || len(s.doc.Sections) == 0 {
		return
	}
	w := min(s.width, theme.MaxContentWidth)
	s.vp.SetWidth(s.width)
	s.vp.SetHeight(max(s.height-tabRows, 1))
	s.vp.SetContent(strings.TrimRight(s.doc.Sections[s.active].Render(s.theme, w), "\n"))
	if resetScroll {
		s.vp.GotoTop()
	}
}

// Update implements shell.Screen.
func (s *Screen) Update(msg tea.Msg) (shell.Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case shell.LayoutMsg:
		s.theme, s.keys, s.width, s.height, s.ready = msg.Theme, msg.Keys, msg.Width, msg.Height, true
		s.refresh(false)
		return s, nil
	case tea.KeyPressMsg:
		n := len(s.doc.Sections)
		switch {
		case n == 0:
			return s, nil
		case key.Matches(msg, s.keys.Next):
			s.active = (s.active + 1) % n
			s.refresh(true)
			return s, nil
		case key.Matches(msg, s.keys.Prev):
			s.active = (s.active - 1 + n) % n
			s.refresh(true)
			return s, nil
		case key.Matches(msg, s.keys.Top):
			s.vp.GotoTop()
			return s, nil
		case key.Matches(msg, s.keys.Bottom):
			s.vp.GotoBottom()
			return s, nil
		}
		if k := msg.String(); len(k) == 1 && k[0] >= '1' && k[0] <= '9' {
			if i := int(k[0] - '1'); i < n {
				s.active = i
				s.refresh(true)
			}
			return s, nil
		}
	}
	var cmd tea.Cmd
	s.vp, cmd = s.vp.Update(msg)
	return s, cmd
}

// View implements shell.Screen.
func (s *Screen) View() string {
	if !s.ready {
		return ""
	}
	return s.tabs() + "\n\n" + s.vp.View()
}

func (s *Screen) tabs() string {
	t := s.theme
	var parts []string
	for i, sec := range s.doc.Sections {
		label := sec.Title
		var tab string
		if i == s.active {
			tab = t.Style.Badge.Render(label)
		} else {
			tab = lipgloss.NewStyle().Padding(0, t.Space.XS).Render(t.Style.Muted.Render(label))
		}
		if sec.Count != "" {
			tab += t.StatusStyle(sec.Status).Render(sec.Count) + " "
		}
		parts = append(parts, tab)
	}
	strip := strings.Join(parts, " ")
	// Scroll position, right-aligned, so long sections show there is more.
	pos := ""
	if s.vp.TotalLineCount() > s.vp.VisibleLineCount() {
		pos = t.Style.Faint.Render(percent(s.vp.ScrollPercent()))
	}
	gap := s.width - lipgloss.Width(strip) - lipgloss.Width(pos)
	if gap < 1 {
		return strip
	}
	return strip + strings.Repeat(" ", gap) + pos
}

func percent(f float64) string {
	n := int(f*100 + 0.5)
	digits := []byte{}
	if n == 0 {
		digits = []byte{'0'}
	}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits) + "%"
}

// ShortHelp implements shell.Screen.
func (s *Screen) ShortHelp() []key.Binding {
	return []key.Binding{s.keys.Scroll, s.keys.Next}
}

// FullHelp implements shell.Screen.
func (s *Screen) FullHelp() [][]key.Binding {
	jump := key.NewBinding(key.WithKeys("1"), key.WithHelp("1-9", "jump to section"))
	return [][]key.Binding{
		{s.keys.Up, s.keys.Down, s.keys.PageUp, s.keys.PageDown, s.keys.Top, s.keys.Bottom},
		{s.keys.Next, s.keys.Prev, jump},
	}
}
