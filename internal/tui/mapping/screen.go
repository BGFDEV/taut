package mapping

import (
	"errors"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/tui/shell"
	"github.com/bgfdev/taut/internal/tui/theme"
	"github.com/bgfdev/taut/internal/ui"
)

// Options configure the mapping screen.
type Options struct {
	// Save writes the config file. It is called with the editor's working
	// copy when the operator saves.
	Save func(*config.Config) error
	// File is the config file's name as shown to the operator.
	File string
}

type mode int

const (
	modeTree mode = iota
	modeFilter
	modeRename
	modePicker
	modeConfirmQuit
	modeHelp
	modeIssues
)

type rowKind int

const (
	rowTeam rowKind = iota
	rowChannel
	rowSkipHead
	rowSkipped
	// The group of direct messages, shown when the config file migrates
	// them. Its rows are read-only.
	rowDMHead
	rowDM
)

// dmGroup keys the "Direct messages" group in the collapsed set.
const dmGroup = "dms"

// skippedGroup keys the "Not migrating" group in the collapsed set; team
// groups are keyed by "team:<index>".
const skippedGroup = "skipped"

type row struct {
	kind  rowKind
	team  int // index into View.Teams
	idx   int // index into the team's channels, View.Skipped or View.DirectMessages
	last  bool
	shown int // for a header: how many children pass the filter
}

type keymap struct {
	Up, Down, PageUp, PageDown, Top, Bottom key.Binding
	Collapse, Expand, Fold                  key.Binding
	Toggle, Rename, Move, Filter            key.Binding
	Undo, Save, Issues, Clear               key.Binding
	// Help-bar summaries.
	Navigate, Folds key.Binding
	// Modal bindings.
	Apply, Cancel, SaveQuit, Discard key.Binding
}

func newKeymap(k shell.Keys, unicode bool) keymap {
	nav, folds := "↑/↓", "←/→"
	if !unicode {
		nav, folds = "up/down", "left/right"
	}
	return keymap{
		Up: k.Up, Down: k.Down, Top: k.Top, Bottom: k.Bottom,
		// Space toggles a channel here, so paging does not use it.
		PageUp:   key.NewBinding(key.WithKeys("pgup", "ctrl+b"), key.WithHelp("pgup", "page up")),
		PageDown: key.NewBinding(key.WithKeys("pgdown", "ctrl+f"), key.WithHelp("pgdn", "page down")),
		Collapse: key.NewBinding(key.WithKeys("left", "h"), key.WithHelp(strings.Split(folds, "/")[0]+"/h", "collapse")),
		Expand:   key.NewBinding(key.WithKeys("right", "l"), key.WithHelp(strings.Split(folds, "/")[1]+"/l", "expand")),
		Fold:     key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "fold/unfold")),
		Toggle:   key.NewBinding(key.WithKeys("space"), key.WithHelp("space", "toggle")),
		Rename:   key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "rename")),
		Move:     key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "move")),
		Filter:   k.Filter,
		Undo:     key.NewBinding(key.WithKeys("u"), key.WithHelp("u", "undo")),
		Save:     key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "save")),
		Issues:   key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "issues")),
		Clear:    key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "clear filter")),
		Navigate: key.NewBinding(key.WithKeys("up", "down"), key.WithHelp(nav, "move")),
		Folds:    key.NewBinding(key.WithKeys("left", "right"), key.WithHelp(folds, "fold")),
		Apply:    key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "apply")),
		Cancel:   key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel")),
		SaveQuit: key.NewBinding(key.WithKeys("s", "y"), key.WithHelp("s", "save")),
		Discard:  key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "discard")),
	}
}

type picker struct {
	channel string
	teams   []string
	cursor  int
}

// Screen is the interactive map. It implements shell.Screen.
type Screen struct {
	ed   *Editor
	opts Options

	theme  theme.Theme
	keys   keymap
	width  int
	height int
	ready  bool

	view      *View
	rows      []row
	collapsed map[string]bool
	filter    string
	cursor    int
	offset    int
	labelW    int // label column of channel rows
	skipW     int // label column of "Not migrating" rows
	dmW       int // label column of "Direct messages" rows
	daysW     int

	mode     mode
	input    lineInput
	inputErr string
	renaming string // Slack name of the channel being renamed
	picker   picker

	flash       string
	flashStatus theme.Status
	saves       int
}

// NewScreen returns the mapping screen for an edit session.
func NewScreen(ed *Editor, o Options) *Screen {
	t := theme.Default()
	s := &Screen{ed: ed, opts: o, theme: t, keys: newKeymap(shell.DefaultKeys(), true), collapsed: map[string]bool{}}
	s.rebuild()
	return s
}

// Saves is the number of times the operator saved.
func (s *Screen) Saves() int { return s.saves }

// Title implements shell.Screen.
func (s *Screen) Title() string { return "" }

// Init implements shell.Screen.
func (s *Screen) Init() tea.Cmd { return nil }

// CapturingInput implements shell.InputCapturer. Besides text entry, the
// screen takes the keys while there are unsaved changes, so that q asks
// before discarding them.
func (s *Screen) CapturingInput() bool { return s.mode != modeTree || s.ed.Dirty() }

// --- rows -----------------------------------------------------------------------

func (s *Screen) matches(names ...string) bool {
	if s.filter == "" {
		return true
	}
	f := strings.ToLower(s.filter)
	for _, n := range names {
		if strings.Contains(strings.ToLower(n), f) {
			return true
		}
	}
	return false
}

func teamKey(i int) string { return fmt.Sprintf("team:%d", i) }

// rebuild recomputes the visible rows. It runs when something changed (an
// edit, a fold, the filter), never per frame.
func (s *Screen) rebuild() {
	v := s.ed.View()
	s.view = v
	s.rows = s.rows[:0]
	s.labelW, s.skipW, s.dmW, s.daysW = 0, 0, 0, 0
	for ti, tv := range v.Teams {
		var kids []int
		for ci, c := range tv.Channels {
			if s.matches(c.SlackName, c.TargetName) {
				kids = append(kids, ci)
			}
			s.labelW = max(s.labelW, lipgloss.Width("#"+c.SlackName))
			if c.TargetName != c.SlackName {
				s.labelW = max(s.labelW, lipgloss.Width("#"+c.SlackName+" "+s.theme.Glyph.Arrow+" "+c.TargetName))
			}
			s.daysW = max(s.daysW, len(daysText(c.Days)))
		}
		if s.filter != "" && len(kids) == 0 {
			continue
		}
		s.rows = append(s.rows, row{kind: rowTeam, team: ti, shown: len(kids)})
		if s.collapsed[teamKey(ti)] && s.filter == "" {
			continue
		}
		for n, ci := range kids {
			s.rows = append(s.rows, row{kind: rowChannel, team: ti, idx: ci, last: n == len(kids)-1})
		}
	}
	var kids []int
	for i, sk := range v.Skipped {
		if s.matches(sk.SlackName) {
			kids = append(kids, i)
		}
		s.skipW = max(s.skipW, lipgloss.Width(sk.label()))
	}
	if len(kids) > 0 {
		s.rows = append(s.rows, row{kind: rowSkipHead, shown: len(kids)})
		if !s.collapsed[skippedGroup] || s.filter != "" {
			for n, i := range kids {
				s.rows = append(s.rows, row{kind: rowSkipped, idx: i, last: n == len(kids)-1})
			}
		}
	}
	kids = nil
	for i, d := range v.DirectMessages {
		if s.matches(d.Label, d.SlackID) {
			kids = append(kids, i)
		}
		s.dmW = max(s.dmW, lipgloss.Width(d.Label))
	}
	if len(kids) > 0 {
		s.rows = append(s.rows, row{kind: rowDMHead, shown: len(kids)})
		if !s.collapsed[dmGroup] || s.filter != "" {
			for n, i := range kids {
				s.rows = append(s.rows, row{kind: rowDM, idx: i, last: n == len(kids)-1})
			}
		}
	}
	s.clamp()
}

func (s *Screen) treeHeight() int { return max(s.height-2, 1) }

func (s *Screen) clamp() {
	s.cursor = max(min(s.cursor, len(s.rows)-1), 0)
	h := s.treeHeight()
	if s.cursor < s.offset {
		s.offset = s.cursor
	}
	if s.cursor >= s.offset+h {
		s.offset = s.cursor - h + 1
	}
	s.offset = max(min(s.offset, len(s.rows)-h), 0)
}

func (s *Screen) current() (row, bool) {
	if s.cursor < 0 || s.cursor >= len(s.rows) {
		return row{}, false
	}
	return s.rows[s.cursor], true
}

func (s *Screen) groupKey(r row) string {
	switch r.kind {
	case rowSkipHead, rowSkipped:
		return skippedGroup
	case rowDMHead, rowDM:
		return dmGroup
	}
	return teamKey(r.team)
}

// gotoHeader puts the cursor on the header of r's group.
func (s *Screen) gotoHeader(r row) {
	want := s.groupKey(r)
	for i, x := range s.rows {
		if (x.kind == rowTeam || x.kind == rowSkipHead || x.kind == rowDMHead) && s.groupKey(x) == want {
			s.cursor = i
			break
		}
	}
	s.clamp()
}

// gotoChannel puts the cursor on the row for a Slack channel, if visible.
func (s *Screen) gotoChannel(name string) {
	for i, r := range s.rows {
		switch r.kind {
		case rowChannel:
			if s.view.Teams[r.team].Channels[r.idx].SlackName == name {
				s.cursor = i
			}
		case rowSkipped:
			if s.view.Skipped[r.idx].SlackName == name {
				s.cursor = i
			}
		}
	}
	s.clamp()
}

func (s *Screen) say(status theme.Status, format string, a ...any) {
	s.flash, s.flashStatus = fmt.Sprintf(format, a...), status
}

// --- update ---------------------------------------------------------------------

// Update implements shell.Screen.
func (s *Screen) Update(msg tea.Msg) (shell.Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case shell.LayoutMsg:
		s.theme, s.width, s.height, s.ready = msg.Theme, msg.Width, msg.Height, true
		s.keys = newKeymap(msg.Keys, msg.Theme.Unicode)
		s.rebuild()
		return s, nil
	case tea.PasteMsg:
		if s.mode == modeFilter || s.mode == modeRename {
			s.input.insert(strings.NewReplacer("\n", " ", "\r", "").Replace(msg.Content))
			s.afterInput()
		}
		return s, nil
	case tea.KeyPressMsg:
		switch s.mode {
		case modeFilter:
			s.updateFilter(msg)
			return s, nil
		case modeRename:
			s.updateRename(msg)
			return s, nil
		case modePicker:
			s.updatePicker(msg)
			return s, nil
		case modeConfirmQuit:
			return s, s.updateConfirm(msg)
		case modeHelp, modeIssues:
			s.mode = modeTree
			return s, nil
		}
		return s, s.updateTree(msg)
	}
	return s, nil
}

func (s *Screen) updateTree(msg tea.KeyPressMsg) tea.Cmd {
	s.flash = ""
	k := s.keys
	r, ok := s.current()
	page := max(s.treeHeight()-1, 1)
	switch {
	case key.Matches(msg, k.Up):
		s.cursor--
	case key.Matches(msg, k.Down):
		s.cursor++
	case key.Matches(msg, k.PageUp):
		s.cursor -= page
	case key.Matches(msg, k.PageDown):
		s.cursor += page
	case key.Matches(msg, k.Top):
		s.cursor = 0
	case key.Matches(msg, k.Bottom):
		s.cursor = len(s.rows) - 1

	case key.Matches(msg, k.Collapse) && ok:
		if r.kind == rowChannel || r.kind == rowSkipped || r.kind == rowDM {
			s.gotoHeader(r)
		} else if s.filter == "" {
			s.collapsed[s.groupKey(r)] = true
			s.rebuild()
		}
	case key.Matches(msg, k.Expand) && ok:
		if r.kind == rowTeam || r.kind == rowSkipHead || r.kind == rowDMHead {
			delete(s.collapsed, s.groupKey(r))
			s.rebuild()
		}
	case key.Matches(msg, k.Fold) && ok:
		if (r.kind == rowTeam || r.kind == rowSkipHead || r.kind == rowDMHead) && s.filter == "" {
			if g := s.groupKey(r); s.collapsed[g] {
				delete(s.collapsed, g)
			} else {
				s.collapsed[g] = true
			}
			s.rebuild()
		}

	case key.Matches(msg, k.Toggle) && ok:
		s.toggle(r)
	case key.Matches(msg, k.Rename) && ok:
		s.startRename(r)
	case key.Matches(msg, k.Move) && ok:
		s.startMove(r)
	case key.Matches(msg, k.Filter):
		s.mode = modeFilter
		s.input.set(s.filter)
	case key.Matches(msg, k.Clear):
		if s.filter != "" {
			s.filter = ""
			s.rebuild()
		}
	case key.Matches(msg, k.Undo):
		if what, ok := s.ed.Undo(); ok {
			s.rebuild()
			s.say(theme.StatusInfo, "Undid: %s", strings.ToLower(what[:1])+what[1:])
		} else {
			s.say(theme.StatusSkip, "Nothing to undo")
		}
	case key.Matches(msg, k.Save):
		return s.save(false)
	case key.Matches(msg, k.Issues):
		s.mode = modeIssues

	// The shell does not see these while there are unsaved changes.
	case msg.String() == "?":
		s.mode = modeHelp
	case msg.String() == "q":
		if s.ed.Dirty() {
			s.mode = modeConfirmQuit
		} else {
			return tea.Quit
		}
	}
	s.clamp()
	return nil
}

// channelAt returns the Slack name on a channel or skipped row.
func (s *Screen) channelAt(r row) (name string, migrating, ok bool) {
	switch r.kind {
	case rowChannel:
		return s.view.Teams[r.team].Channels[r.idx].SlackName, true, true
	case rowSkipped:
		sk := s.view.Skipped[r.idx]
		if sk.Kind == "dm" {
			s.say(theme.StatusSkip, "Direct messages cannot be edited here: %s", sk.Reason)
			return "", false, false
		}
		return sk.SlackName, false, true
	case rowDM:
		s.say(theme.StatusSkip, "Direct messages cannot be edited here: who is in a conversation decides whether it migrates")
		return "", false, false
	}
	s.say(theme.StatusSkip, "Select a channel first")
	return "", false, false
}

// edited reports the outcome of an edit in the status line.
func (s *Screen) edited(err error, done string) {
	switch {
	case err == nil:
		s.rebuild()
		s.say(theme.StatusOK, "%s %s u to undo", done, s.theme.Glyph.Sep)
	case errors.Is(err, ErrNoChange):
		s.say(theme.StatusSkip, "Nothing to change")
	default:
		fe := fail.From(err)
		s.say(theme.StatusFail, "%s", strings.ReplaceAll(strings.TrimRight(fe.What, ".")+". "+fe.Why, "`", ""))
	}
}

func (s *Screen) toggle(r row) {
	name, migrating, ok := s.channelAt(r)
	if !ok {
		return
	}
	if migrating {
		s.edited(s.ed.Exclude(name), "Excluded #"+name)
		return
	}
	err := s.ed.Include(name, "")
	if fe := fail.From(err); err != nil && !errors.Is(err, ErrNoChange) && fe.Code == "map.team_required" {
		s.openPicker(name)
		return
	}
	s.edited(err, "Included #"+name)
}

func (s *Screen) startRename(r row) {
	name, migrating, ok := s.channelAt(r)
	if !ok {
		return
	}
	if !migrating {
		s.say(theme.StatusSkip, "#%s is not migrating; include it with space before renaming it", name)
		return
	}
	c := s.view.Teams[r.team].Channels[r.idx]
	s.mode, s.renaming, s.inputErr = modeRename, name, ""
	s.input.set(c.TargetName)
}

func (s *Screen) startMove(r row) {
	if name, _, ok := s.channelAt(r); ok {
		s.openPicker(name)
	}
}

// openPicker offers the teams the channel can actually go to. If there are
// none it says why instead of offering choices that would all be refused.
func (s *Screen) openPicker(name string) {
	teams, err := s.ed.Destinations(name)
	switch {
	case err != nil:
		s.edited(err, "")
	case len(teams) == 0:
		s.say(theme.StatusSkip, "The config file has one team; add another under target.teams to move channels")
	default:
		s.mode, s.picker = modePicker, picker{channel: name, teams: teams}
	}
}

func (s *Screen) updatePicker(msg tea.KeyPressMsg) {
	p := &s.picker
	switch {
	case key.Matches(msg, s.keys.Up):
		p.cursor = max(p.cursor-1, 0)
	case key.Matches(msg, s.keys.Down):
		p.cursor = min(p.cursor+1, len(p.teams)-1)
	case key.Matches(msg, s.keys.Cancel), msg.String() == "q":
		s.mode = modeTree
	case key.Matches(msg, s.keys.Apply):
		s.mode = modeTree
		team := p.teams[p.cursor]
		s.edited(s.ed.Move(p.channel, team), "Moved #"+p.channel+" to "+team)
		s.gotoChannel(p.channel)
	}
}

func (s *Screen) afterInput() {
	if s.mode == modeFilter {
		s.filter = s.input.value()
		s.cursor, s.offset = 0, 0
		s.rebuild()
		// Start on the first match rather than on its team.
		if len(s.rows) > 1 && s.filter != "" {
			s.cursor = 1
		}
	}
	s.inputErr = ""
}

func (s *Screen) updateFilter(msg tea.KeyPressMsg) {
	switch {
	case key.Matches(msg, s.keys.Cancel):
		s.mode, s.filter = modeTree, ""
		s.rebuild()
	case key.Matches(msg, s.keys.Apply):
		s.mode = modeTree
	default:
		if s.input.update(msg) {
			s.afterInput()
		}
	}
}

func (s *Screen) updateRename(msg tea.KeyPressMsg) {
	switch {
	case key.Matches(msg, s.keys.Cancel):
		s.mode, s.inputErr = modeTree, ""
	case key.Matches(msg, s.keys.Apply):
		name := s.renaming
		err := s.ed.Rename(name, s.input.value())
		if err != nil && !errors.Is(err, ErrNoChange) {
			// Stay in the field: the operator fixes the name in place.
			fe := fail.From(err)
			s.inputErr = strings.ReplaceAll(strings.TrimRight(fe.What, ".")+". "+fe.Why+" "+fe.Next, "`", "")
			return
		}
		s.mode = modeTree
		s.edited(err, "Renamed #"+name)
		s.gotoChannel(name)
	default:
		if s.input.update(msg) {
			s.afterInput()
		}
	}
}

func (s *Screen) updateConfirm(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case key.Matches(msg, s.keys.SaveQuit):
		return s.save(true)
	case key.Matches(msg, s.keys.Discard):
		return tea.Quit
	case key.Matches(msg, s.keys.Cancel), msg.String() == "n", msg.String() == "q":
		s.mode = modeTree
	}
	return nil
}

func (s *Screen) save(quit bool) tea.Cmd {
	if !s.ed.Dirty() {
		s.mode = modeTree
		s.say(theme.StatusSkip, "Nothing to save")
		if quit {
			return tea.Quit
		}
		return nil
	}
	n := len(s.ed.Unsaved())
	if s.opts.Save != nil {
		if err := s.opts.Save(s.ed.Config()); err != nil {
			s.mode = modeTree
			return shell.Fail(err)
		}
	}
	s.ed.MarkSaved()
	s.saves++
	s.mode = modeTree
	s.rebuild()
	s.say(theme.StatusOK, "Saved %s to %s", plural(n, "change", "changes"), s.opts.File)
	if quit {
		return tea.Quit
	}
	return nil
}

// --- help -----------------------------------------------------------------------

// ShortHelp implements shell.Screen.
func (s *Screen) ShortHelp() []key.Binding {
	k := s.keys
	switch s.mode {
	case modeFilter, modeRename:
		return []key.Binding{k.Apply, k.Cancel}
	case modePicker:
		return []key.Binding{k.Navigate, key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "move here")), k.Cancel}
	case modeConfirmQuit:
		return []key.Binding{k.SaveQuit, k.Discard, key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "keep editing"))}
	case modeHelp, modeIssues:
		return []key.Binding{key.NewBinding(key.WithKeys("esc"), key.WithHelp("any key", "close"))}
	}
	// The bar must never push "? help" and "q quit" off a narrow terminal.
	switch {
	case s.width < 72:
		return []key.Binding{k.Toggle, k.Rename, k.Save}
	case s.width < 96:
		return []key.Binding{k.Toggle, k.Rename, k.Move, k.Filter, k.Save}
	}
	return []key.Binding{k.Toggle, k.Rename, k.Move, k.Filter, k.Undo, k.Save}
}

// FullHelp implements shell.Screen. Two columns, so that with the shell's
// own column the overlay fits the narrowest terminal the shell draws in.
func (s *Screen) FullHelp() [][]key.Binding {
	k := s.keys
	return [][]key.Binding{
		{k.Up, k.Down, k.PageUp, k.PageDown, k.Top, k.Bottom, k.Collapse, k.Expand},
		{
			key.NewBinding(key.WithKeys("space"), key.WithHelp("space", "include/exclude")),
			k.Rename, k.Move, k.Filter, k.Clear, k.Undo, k.Save, k.Issues,
		},
	}
}

// helpOverlay mirrors the shell's own help overlay, for when this screen is
// holding the keys.
func (s *Screen) helpOverlay() string {
	t := s.theme
	h := help.New()
	h.Styles = help.Styles{
		Ellipsis: t.Style.Faint, ShortKey: t.Style.Key, ShortDesc: t.Style.Muted, ShortSeparator: t.Style.Faint,
		FullKey: t.Style.Key, FullDesc: t.Style.Muted, FullSeparator: t.Style.Faint,
	}
	h.Ellipsis = t.Glyph.Ellipsis
	h.SetWidth(s.width)
	sk := shell.NewKeys(t.Unicode)
	groups := append(s.FullHelp(), []key.Binding{sk.Help, sk.Back, sk.Quit})
	return ui.Heading(t, "Keys", s.width) + "\n\n" + h.FullHelpView(groups)
}

// --- view -----------------------------------------------------------------------

// View implements shell.Screen.
func (s *Screen) View() string {
	if !s.ready {
		return ""
	}
	var top []string
	switch s.mode {
	case modeHelp:
		top = strings.Split(s.helpOverlay(), "\n")
	case modeIssues:
		body := s.view.Findings(s.theme, min(s.width, theme.MaxContentWidth))
		if body == "" {
			body = ui.CheckLine(s.theme, theme.StatusOK, "No blockers or warnings", "", s.width)
		}
		top = strings.Split(body, "\n")
	case modePicker:
		top = s.pickerLines()
	default:
		top = s.treeLines()
	}
	h := s.treeHeight()
	if len(top) > h {
		top = top[:h]
	}
	for len(top) < h {
		top = append(top, "")
	}
	if s.height < 2 {
		return strings.Join(top, "\n")
	}
	if s.height == 2 {
		return strings.Join(append(top, s.statusLine()), "\n")
	}
	return strings.Join(append(top, s.detailLine(), s.statusLine()), "\n")
}

func (s *Screen) treeLines() []string {
	if len(s.rows) == 0 {
		msg := "The export has no channels."
		if s.filter != "" {
			msg = fmt.Sprintf("No channel matches %q.", s.filter)
		}
		return []string{s.theme.Style.Muted.Render(msg)}
	}
	end := min(s.offset+s.treeHeight(), len(s.rows))
	lines := make([]string, 0, end-s.offset)
	for i := s.offset; i < end; i++ {
		lines = append(lines, s.rowLine(s.rows[i], i == s.cursor))
	}
	return lines
}

func (s *Screen) rowLine(r row, selected bool) string {
	t := s.theme
	// Two gutter columns: the cursor, then a mark on rows with unsaved
	// changes.
	gutter := func(changed bool) string {
		g := " "
		if selected {
			g = t.Style.Brand.Render(t.Glyph.Pointer)
		}
		if changed {
			return g + t.Style.Accent.Render(t.Glyph.Bullet) + " "
		}
		return g + "  "
	}
	fold := func(key string) string {
		if s.collapsed[key] && s.filter == "" {
			return t.Style.Faint.Render("+") + " "
		}
		return t.Style.Faint.Render("-") + " "
	}
	width := s.width - 3
	gap := t.Space.SM
	branchW := lipgloss.Width(t.Glyph.TreeMid) + 1
	// A very long name must not squeeze out the notes: the detail line
	// beneath the tree spells the selected row out in full.
	room := width - branchW
	labelW := max(min(s.labelW, room*45/100), 10)
	skipW := max(min(s.skipW, room*45/100), 10)

	switch r.kind {
	case rowTeam:
		tv := s.view.Teams[r.team]
		head := teamHead(t, tv, r.shown)
		if s.filter != "" && r.shown != len(tv.Channels) {
			head = t.Style.Title.Render(tv.Name) + "  " + t.Style.Muted.Render(fmt.Sprintf("%d of %s", r.shown, plural(len(tv.Channels), "channel", "channels")))
		}
		return gutter(false) + clip(t, fold(teamKey(r.team))+head, width)
	case rowSkipHead:
		head := t.Style.Title.Render("Not migrating") + "  " + t.Style.Muted.Render(fmt.Sprint(r.shown))
		return gutter(false) + clip(t, fold(skippedGroup)+head, width)
	case rowDMHead:
		head := t.Style.Title.Render("Direct messages") + "  " + t.Style.Muted.Render(fmt.Sprint(r.shown))
		return gutter(false) + clip(t, fold(dmGroup)+head, width)
	}

	branch := t.Glyph.TreeMid
	if r.last {
		branch = t.Glyph.TreeEnd
	}
	line := t.Style.Faint.Render(branch) + " "
	days := func(d int) string {
		if s.daysW == 0 {
			return ""
		}
		txt := daysText(d)
		return strings.Repeat(" ", gap+s.daysW-len(txt)) + t.Style.Muted.Render(txt)
	}
	if r.kind == rowDM {
		d := s.view.DirectMessages[r.idx]
		dmW := max(min(s.dmW, room*45/100), 10)
		status := t.Style.Text.Render(d.Status())
		if !d.Migrates {
			status = t.Style.Muted.Render(d.Status())
		}
		line += padTo(clip(t, t.Style.Muted.Render(d.Label), dmW), dmW) + strings.Repeat(" ", gap) + status
		return gutter(false) + clip(t, line, width)
	}
	if r.kind == rowSkipped {
		sk := s.view.Skipped[r.idx]
		line += padTo(clip(t, t.Style.Muted.Render(sk.label()), skipW), skipW) +
			strings.Repeat(" ", gap) + t.Style.Muted.Render(sk.Reason)
		return gutter(sk.Changed) + clip(t, line, width)
	}

	c := s.view.Teams[r.team].Channels[r.idx]
	if selected && s.mode == modeRename {
		field := t.Style.Title.Render("#"+c.SlackName) + " " + t.Style.Faint.Render(t.Glyph.Arrow) + " "
		return gutter(c.Changed) + line + field + s.input.view(t, max(room-lipgloss.Width(field), 4))
	}
	line += padTo(clip(t, c.label(t, selected), labelW), labelW) + days(c.Days)
	if n := c.note(t); n != "" {
		line += strings.Repeat(" ", gap) + n
	}
	return gutter(c.Changed) + clip(t, strings.TrimRight(line, " "), width)
}

func (s *Screen) pickerLines() []string {
	t := s.theme
	lines := []string{t.Style.Heading.Render("Move #" + s.picker.channel + " to"), ""}
	for i, name := range s.picker.teams {
		if i == s.picker.cursor {
			lines = append(lines, t.Style.Brand.Render(t.Glyph.Pointer)+" "+t.Style.Title.Render(name))
		} else {
			lines = append(lines, "  "+t.Style.Text.Render(name))
		}
	}
	return lines
}

// detailLine is the line under the tree. It carries whatever the operator
// is doing right now: a prompt, the outcome of the last action, or the
// selected row spelled out in full, since the tree truncates.
func (s *Screen) detailLine() string {
	t := s.theme
	switch s.mode {
	case modeHelp, modeIssues, modePicker:
		return ""
	case modeFilter:
		return clip(t, t.Style.Muted.Render("Filter ")+s.input.view(t, max(s.width-8, 4)), s.width)
	case modeRename:
		if s.inputErr != "" {
			return clip(t, t.StatusMark(theme.StatusFail)+" "+t.Style.Text.Render(s.inputErr), s.width)
		}
		return clip(t, t.Style.Muted.Render("Type the Teams channel name. Leave it empty to use the Slack name."), s.width)
	case modeConfirmQuit:
		n := len(s.ed.Unsaved())
		what := plural(n, "unsaved change", "unsaved changes")
		if n == 0 {
			what = "Unsaved changes"
		}
		return clip(t, t.StatusMark(theme.StatusWarn)+" "+t.Style.Warning.Bold(true).Render(what+".")+" "+
			t.Style.Text.Render("Save before quitting?"), s.width)
	}
	if s.flash != "" {
		return clip(t, t.StatusMark(s.flashStatus)+" "+t.Style.Text.Render(s.flash), s.width)
	}
	r, ok := s.current()
	if !ok {
		return ""
	}
	sep := " " + t.Glyph.Sep + " "
	var text string
	switch r.kind {
	case rowTeam:
		tv := s.view.Teams[r.team]
		text = tv.Name + sep + "owner " + tv.Owner
	case rowSkipHead:
		text = "Conversations the config file does not select"
	case rowDMHead:
		text = strings.ReplaceAll(dmNote, "`", "")
	case rowDM:
		d := s.view.DirectMessages[r.idx]
		text = strings.Join([]string{d.Label, d.Kind, d.SlackID, d.Status()}, sep)
	case rowSkipped:
		sk := s.view.Skipped[r.idx]
		parts := []string{sk.label(), sk.Kind}
		if d := daysText(sk.Days); d != "" {
			parts = append(parts, d)
		}
		text = strings.Join(append(parts, sk.Reason), sep)
	case rowChannel:
		c := s.view.Teams[r.team].Channels[r.idx]
		parts := []string{"#" + c.SlackName + " " + t.Glyph.Arrow + " " + s.view.Teams[r.team].Name + " / " + c.TargetName, c.Kind}
		if c.Renamed && c.RenameReason != "" {
			parts = append(parts, c.RenameReason)
		}
		if c.Archived {
			parts = append(parts, "archived in Slack")
		}
		text = strings.Join(parts, sep)
	}
	return clip(t, t.Style.Faint.Render(text), s.width)
}

// statusLine is the last line of the body and always the same thing: the
// live counts of blockers, warnings and unsaved changes.
func (s *Screen) statusLine() string {
	t := s.theme
	sep := t.Style.Faint.Render(" " + t.Glyph.Sep + " ")
	var parts []string
	if n := len(s.view.Blockers); n > 0 {
		parts = append(parts, t.Style.Danger.Render(plural(n, "blocker", "blockers")))
	}
	if n := len(s.view.Warnings); n > 0 {
		parts = append(parts, t.Style.Warning.Render(plural(n, "warning", "warnings")))
	}
	if s.ed.Dirty() {
		n := len(s.ed.Unsaved())
		txt := plural(n, "unsaved change", "unsaved changes")
		if n == 0 {
			txt = "unsaved changes"
		}
		parts = append(parts, t.Style.Accent.Render(txt))
	}
	if s.filter != "" {
		parts = append(parts, t.Style.Muted.Render("filter ")+t.Style.Text.Render(s.filter))
	}
	parts = append(parts, t.Style.Muted.Render(s.view.Summary()))
	return clip(t, strings.Join(parts, sep), s.width)
}

// --- line input -----------------------------------------------------------------

// lineInput is a one-line text field. The screen draws it inline in a tree
// row, so it is deliberately small: no blinking and no styling of its own.
type lineInput struct {
	runes []rune
	pos   int
}

func (in *lineInput) set(v string)  { in.runes = []rune(v); in.pos = len(in.runes) }
func (in *lineInput) value() string { return string(in.runes) }

func (in *lineInput) insert(text string) {
	add := []rune(text)
	in.runes = append(in.runes[:in.pos], append(add, in.runes[in.pos:]...)...)
	in.pos += len(add)
}

// update applies a key and reports whether the text changed.
func (in *lineInput) update(msg tea.KeyPressMsg) bool {
	switch msg.String() {
	case "left", "ctrl+b":
		in.pos = max(in.pos-1, 0)
	case "right", "ctrl+f":
		in.pos = min(in.pos+1, len(in.runes))
	case "home", "ctrl+a":
		in.pos = 0
	case "end", "ctrl+e":
		in.pos = len(in.runes)
	case "backspace":
		if in.pos > 0 {
			in.runes = append(in.runes[:in.pos-1], in.runes[in.pos:]...)
			in.pos--
			return true
		}
	case "delete", "ctrl+d":
		if in.pos < len(in.runes) {
			in.runes = append(in.runes[:in.pos], in.runes[in.pos+1:]...)
			return true
		}
	case "ctrl+u":
		in.runes, in.pos = in.runes[:0], 0
		return true
	default:
		if msg.Text != "" {
			in.insert(msg.Text)
			return true
		}
	}
	return false
}

// view draws the field within room cells, keeping the cursor visible.
func (in *lineInput) view(t theme.Theme, room int) string {
	start := 0
	if in.pos >= room {
		start = in.pos - room + 1
	}
	end := min(len(in.runes), start+room)
	cursor := lipgloss.NewStyle().Reverse(true)
	var b strings.Builder
	b.WriteString(t.Style.Text.Render(string(in.runes[start:in.pos])))
	if in.pos < end {
		b.WriteString(cursor.Render(string(in.runes[in.pos])))
		b.WriteString(t.Style.Text.Render(string(in.runes[in.pos+1 : end])))
	} else {
		b.WriteString(cursor.Render(" "))
	}
	return b.String()
}
