// Package shell is the frame every full-screen Taut view lives in.
//
// The shell owns the chrome: the title bar, the body region, and a
// persistent contextual help bar. It also owns the things that must behave
// identically everywhere: global keys, resize handling, light/dark
// switching, the help overlay, and error presentation. Screens implement
// the small Screen interface and only ever draw their own body.
package shell

import (
	"context"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/tui/theme"
	"github.com/bgfdev/taut/internal/ui"
)

// Screen is one view inside the shell.
type Screen interface {
	// Title is shown in the title bar.
	Title() string
	// Init returns the screen's first command, if any.
	Init() tea.Cmd
	// Update handles a message. Screens receive a LayoutMsg before their
	// first View and whenever the body size or theme changes.
	Update(tea.Msg) (Screen, tea.Cmd)
	// View renders the body. The shell clips it to the body region, so a
	// screen can never push the chrome around.
	View() string
	// ShortHelp lists the screen's bindings for the help bar, most
	// important first. Global bindings are appended by the shell.
	ShortHelp() []key.Binding
	// FullHelp lists all of the screen's bindings, grouped in columns,
	// for the help overlay.
	FullHelp() [][]key.Binding
}

// InputCapturer is implemented by screens that have a focused text input.
// While it returns true the shell stops interpreting printable keys such as
// "q" and "?" as global commands.
type InputCapturer interface {
	CapturingInput() bool
}

// LayoutMsg tells a screen the size of its body region and the theme to
// draw with.
type LayoutMsg struct {
	Theme  theme.Theme
	Keys   Keys
	Width  int
	Height int
}

// PushMsg opens a screen on top of the current one.
type PushMsg struct{ Screen Screen }

// PopMsg closes the current screen and returns to the previous one.
type PopMsg struct{}

// ErrorMsg shows an error panel over the current screen.
type ErrorMsg struct{ Err error }

// Push returns a command that opens s.
func Push(s Screen) tea.Cmd { return func() tea.Msg { return PushMsg{s} } }

// Pop returns a command that closes the current screen.
func Pop() tea.Msg { return PopMsg{} }

// Fail returns a command that shows err.
func Fail(err error) tea.Cmd { return func() tea.Msg { return ErrorMsg{err} } }

// chromeRows is the number of rows the shell uses: title, rule, rule, help.
const chromeRows = 4

// Options configure the shell.
type Options struct {
	// Theme is the starting theme. The shell re-resolves light/dark when
	// the terminal reports its background color.
	Theme theme.Theme
	// Command is the command name shown beside the product name.
	Command string
	// Context is shown at the right of the title bar, e.g. the config file
	// name.
	Context string
	// LockTheme stops the shell from following the terminal's background
	// color (used by tests and when TAUT_THEME is set).
	LockTheme bool
}

// Model is the shell's Bubble Tea model.
type Model struct {
	opts   Options
	theme  theme.Theme
	keys   Keys
	help   help.Model
	stack  []Screen
	width  int
	height int

	showHelp bool
	err      *fail.Error
}

// New builds a shell showing root.
func New(root Screen, o Options) Model {
	m := Model{opts: o, theme: o.Theme, keys: DefaultKeys(), stack: []Screen{root}}
	m.help = help.New()
	m.restyle()
	return m
}

func (m *Model) restyle() {
	t := m.theme
	m.keys = NewKeys(t.Unicode)
	m.help.Styles = help.Styles{
		Ellipsis:       t.Style.Faint,
		ShortKey:       t.Style.Key,
		ShortDesc:      t.Style.Muted,
		ShortSeparator: t.Style.Faint,
		FullKey:        t.Style.Key,
		FullDesc:       t.Style.Muted,
		FullSeparator:  t.Style.Faint,
	}
	m.help.ShortSeparator = " " + t.Glyph.Sep + " "
	m.help.Ellipsis = t.Glyph.Ellipsis
}

func (m Model) top() Screen { return m.stack[len(m.stack)-1] }

func (m Model) margin() int {
	if m.width >= 100 {
		return m.theme.Space.SM
	}
	return m.theme.Space.XS
}

func (m Model) bodySize() (w, h int) {
	return max(m.width-2*m.margin(), 1), max(m.height-chromeRows, 1)
}

func (m Model) tooSmall() bool {
	return m.width < theme.MinWidth || m.height < theme.MinHeight
}

func (m Model) layoutMsg() LayoutMsg {
	w, h := m.bodySize()
	return LayoutMsg{Theme: m.theme, Keys: m.keys, Width: w, Height: h}
}

// relayout delivers the current layout to every screen on the stack, so a
// screen underneath is already the right size when it is revealed.
func (m *Model) relayout() tea.Cmd {
	if m.width == 0 {
		return nil
	}
	msg := m.layoutMsg()
	var cmds []tea.Cmd
	for i, s := range m.stack {
		var c tea.Cmd
		m.stack[i], c = s.Update(msg)
		cmds = append(cmds, c)
	}
	return tea.Batch(cmds...)
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.top().Init()}
	if !m.opts.LockTheme {
		cmds = append(cmds, tea.RequestBackgroundColor)
	}
	return tea.Batch(cmds...)
}

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.help.SetWidth(max(m.width-2*m.margin(), 1))
		return m, m.relayout()

	case tea.BackgroundColorMsg:
		if m.opts.LockTheme || msg.IsDark() == m.theme.Dark {
			return m, nil
		}
		m.theme = theme.New(theme.Options{Dark: msg.IsDark(), Unicode: m.theme.Unicode})
		m.restyle()
		return m, m.relayout()

	case PushMsg:
		m.stack = append(m.stack, msg.Screen)
		m.showHelp = false
		init := msg.Screen.Init()
		var c tea.Cmd
		if m.width > 0 {
			m.stack[len(m.stack)-1], c = msg.Screen.Update(m.layoutMsg())
		}
		return m, tea.Batch(init, c)

	case PopMsg:
		if len(m.stack) > 1 {
			m.stack = m.stack[:len(m.stack)-1]
		}
		return m, nil

	case ErrorMsg:
		m.err = fail.From(msg.Err)
		return m, nil

	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		if m.err != nil {
			if key.Matches(msg, m.keys.Back, m.keys.Select) {
				m.err = nil
			} else if key.Matches(msg, m.keys.Quit) {
				return m, tea.Quit
			}
			return m, nil
		}
		if m.showHelp {
			if key.Matches(msg, m.keys.Quit) && msg.String() == "q" {
				return m, tea.Quit
			}
			m.showHelp = false
			return m, nil
		}
		capturing := false
		if c, ok := m.top().(InputCapturer); ok {
			capturing = c.CapturingInput()
		}
		if !capturing {
			switch {
			case key.Matches(msg, m.keys.Help):
				m.showHelp = true
				return m, nil
			case key.Matches(msg, m.keys.Quit):
				return m, tea.Quit
			case key.Matches(msg, m.keys.Back) && len(m.stack) > 1:
				m.stack = m.stack[:len(m.stack)-1]
				return m, nil
			}
		}
	}

	var cmd tea.Cmd
	m.stack[len(m.stack)-1], cmd = m.top().Update(msg)
	return m, cmd
}

// ShortHelp implements help.KeyMap for the help bar.
func (m Model) ShortHelp() []key.Binding {
	if m.err != nil {
		dismiss := key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "dismiss"))
		return []key.Binding{dismiss, m.keys.Quit}
	}
	if m.showHelp {
		closeHelp := key.NewBinding(key.WithKeys("?"), key.WithHelp("any key", "close help"))
		return []key.Binding{closeHelp, m.keys.Quit}
	}
	b := append([]key.Binding{}, m.top().ShortHelp()...)
	if len(m.stack) > 1 {
		b = append(b, m.keys.Back)
	}
	return append(b, m.keys.Help, m.keys.Quit)
}

// FullHelp implements help.KeyMap for the help overlay.
func (m Model) FullHelp() [][]key.Binding {
	groups := append([][]key.Binding{}, m.top().FullHelp()...)
	global := []key.Binding{m.keys.Help, m.keys.Back, m.keys.Quit}
	return append(groups, global)
}

// View implements tea.Model.
func (m Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.WindowTitle = "taut " + m.opts.Command
	return v
}

func (m Model) render() string {
	if m.width == 0 {
		return ""
	}
	t := m.theme
	if m.tooSmall() {
		times := "×"
		if !t.Unicode {
			times = "x"
		}
		msg := t.Style.Warning.Render("Terminal too small") + "\n" +
			t.Style.Muted.Render("Taut needs at least "+itoa(theme.MinWidth)+times+itoa(theme.MinHeight)+".") + "\n" +
			t.Style.Muted.Render("Resize the window, or press q to quit.")
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, msg)
	}

	bw, bh := m.bodySize()
	pad := strings.Repeat(" ", m.margin())

	var body string
	switch {
	case m.err != nil:
		panel := t.Style.Panel.BorderForeground(t.Color.Danger).Render(ui.ErrorBlock(t, m.err, min(bw-4, 76)))
		body = lipgloss.Place(bw, bh, lipgloss.Center, lipgloss.Center, panel)
	case m.showHelp:
		body = m.helpOverlay(bw)
	default:
		body = m.top().View()
	}

	lines := make([]string, 0, m.height)
	lines = append(lines, pad+m.titleBar(bw), pad+t.Rule(bw))
	for _, l := range fit(body, bw, bh) {
		lines = append(lines, pad+l)
	}
	lines = append(lines, pad+t.Rule(bw), pad+m.help.ShortHelpView(m.ShortHelp()))
	return strings.Join(lines, "\n")
}

func (m Model) titleBar(width int) string {
	t := m.theme
	left := t.Style.Brand.Render(t.Glyph.Mark + " taut")
	if m.opts.Command != "" {
		left += " " + t.Style.Title.Render(m.opts.Command)
	}
	// Breadcrumb of screens above the root.
	for i, s := range m.stack {
		if i == 0 && s.Title() == "" {
			continue
		}
		left += " " + t.Style.Faint.Render(t.Glyph.Pointer) + " " + t.Style.Muted.Render(s.Title())
	}
	right := t.Style.Muted.Render(m.opts.Context)
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return ansi.Truncate(left, width, t.Glyph.Ellipsis)
	}
	return left + strings.Repeat(" ", gap) + right
}

func (m Model) helpOverlay(width int) string {
	t := m.theme
	h := m.help
	h.SetWidth(width)
	return ui.Heading(t, "Keys", width) + "\n\n" + h.FullHelpView(m.FullHelp())
}

// fit clips and pads s to exactly width×height cells so the chrome never
// moves, whatever a screen returns.
func fit(s string, width, height int) []string {
	lines := strings.Split(s, "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	for i, l := range lines {
		if ansi.StringWidth(l) > width {
			lines[i] = ansi.Truncate(l, width, "")
		}
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return lines
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// Run shows root in a full-screen shell until the user quits.
func Run(ctx context.Context, io *ui.IO, root Screen, o Options) error {
	if o.Theme.Glyph.Mark == "" {
		o.Theme = io.Theme
	}
	if io.ThemeLocked() {
		o.LockTheme = true
	}
	p := tea.NewProgram(New(root, o),
		tea.WithContext(ctx),
		tea.WithInput(io.In),
		tea.WithEnvironment(io.Env()),
	)
	final, err := p.Run()
	if m, ok := final.(Model); ok {
		// Carry what the terminal told us into anything printed afterwards.
		io.SetDark(m.theme.Dark)
	}
	if err != nil && ctx.Err() != nil {
		return nil //nolint:nilerr // the user interrupted; that is not a failure
	}
	return err
}
