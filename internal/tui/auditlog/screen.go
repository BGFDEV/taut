package auditlog

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/tui/shell"
	"github.com/bgfdev/taut/internal/tui/theme"
)

// Options configure the browser.
type Options struct {
	// Context bounds every request the screen makes. Nil means background.
	Context context.Context
	// Filter is the starting filter. Outcome, Op and Contains can be
	// changed in the browser; Stream, Since and Until are fixed for the
	// session.
	Filter store.LogFilter
	// Scope describes the fixed part of the filter for the reader, e.g.
	// "#general" or "since 09:00".
	Scope string
	// ExportPath is the file name offered when exporting.
	ExportPath string
	// PageSize is how many entries are fetched at a time; zero means 200.
	PageSize int
}

type mode int

const (
	modeList    mode = iota // the table
	modeDetail              // one entry, every field in full
	modeInfo                // the result of an export or a failed verification
	modeOps                 // choosing an operation to filter by
	modeFilter              // editing the text filter
	modeExport              // typing an export path
	modeConfirm             // an export file exists: replace it?
)

type notice struct {
	status theme.Status
	text   string
}

type keymap struct {
	Outcome, Op, Export, Verify, Retry key.Binding
	Apply, Cancel, Choose, Close       key.Binding
	PrevEntry, NextEntry               key.Binding
	Yes, No                            key.Binding
}

func newKeymap(unicode bool) keymap {
	prevNext := "←/→"
	if !unicode {
		prevNext = "left/right"
	}
	return keymap{
		Outcome:   key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "outcome")),
		Op:        key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "op")),
		Export:    key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "export")),
		Verify:    key.NewBinding(key.WithKeys("v"), key.WithHelp("v", "verify")),
		Retry:     key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "retry")),
		Apply:     key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "apply")),
		Cancel:    key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel")),
		Choose:    key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "choose")),
		Close:     key.NewBinding(key.WithKeys("esc", "enter"), key.WithHelp("esc", "back")),
		PrevEntry: key.NewBinding(key.WithKeys("left", "h"), key.WithHelp(prevNext, "prev/next")),
		NextEntry: key.NewBinding(key.WithKeys("right", "l")),
		Yes:       key.NewBinding(key.WithKeys("y", "Y"), key.WithHelp("y", "replace")),
		No:        key.NewBinding(key.WithKeys("n", "N", "esc"), key.WithHelp("n", "keep")),
	}
}

// Screen is the full-screen audit log browser.
type Screen struct {
	ctx  context.Context
	src  Source
	opts Options

	theme  theme.Theme
	keys   shell.Keys
	own    keymap
	width  int
	height int
	ready  bool

	// The editable part of the filter.
	text, outcome, op string

	// What has been loaded for the current filter. gen changes whenever
	// the filter does, so a page that arrives for an old filter is dropped.
	entries []store.LogEntry
	after   store.LogCursor
	done    bool
	loading bool
	loadErr *fail.Error
	gen     int

	cursor, offset int
	mode           mode

	seenOps  map[string]struct{}
	opCursor int

	input textinput.Model
	vp    viewport.Model
	// info renders the body of modeInfo.
	info      func(t theme.Theme, width int) string
	infoTitle string

	busy        string // a foreground job: verifying or exporting
	note        *notice
	pendingPath string // the export awaiting a yes to replace

	frame    int
	spinning bool
}

// NewScreen returns a browser reading src.
func NewScreen(src Source, o Options) *Screen {
	if o.Context == nil {
		o.Context = context.Background()
	}
	if o.PageSize <= 0 {
		o.PageSize = 200
	}
	if o.ExportPath == "" {
		o.ExportPath = "audit.ndjson"
	}
	vp := viewport.New()
	// Left and right step between entries, so the viewport must not
	// scroll sideways on them.
	vp.KeyMap.Left = key.NewBinding()
	vp.KeyMap.Right = key.NewBinding()
	s := &Screen{
		ctx: o.Context, src: src, opts: o,
		theme: theme.Default(), keys: shell.DefaultKeys(), own: newKeymap(true),
		text: o.Filter.Contains, outcome: o.Filter.Outcome, op: o.Filter.Op,
		seenOps: map[string]struct{}{},
		input:   textinput.New(),
		vp:      vp,
		gen:     1,
	}
	if s.op != "" {
		s.seenOps[s.op] = struct{}{}
	}
	s.restyle()
	return s
}

func (s *Screen) restyle() {
	t := s.theme
	s.own = newKeymap(t.Unicode)
	state := textinput.StyleState{
		Text: t.Style.Text, Placeholder: t.Style.Faint, Suggestion: t.Style.Faint, Prompt: t.Style.Muted,
	}
	st := textinput.Styles{Focused: state, Blurred: state}
	st.Cursor.Color = t.Color.Brand
	st.Cursor.Shape = tea.CursorBlock
	s.input.SetStyles(st)
	s.input.Prompt = ""
}

// Title implements shell.Screen.
func (s *Screen) Title() string {
	switch s.mode {
	case modeDetail:
		return "entry"
	case modeInfo:
		return s.infoTitle
	}
	return ""
}

// CapturingInput implements shell.InputCapturer.
func (s *Screen) CapturingInput() bool {
	return s.mode == modeFilter || s.mode == modeExport || s.mode == modeConfirm
}

// Init implements shell.Screen.
func (s *Screen) Init() tea.Cmd {
	if s.loading || len(s.entries) > 0 || s.done {
		return nil // a layout arrived first and already started the read
	}
	return s.fetch()
}

// --- loading ---------------------------------------------------------------

type (
	pageMsg struct {
		gen int
		// after is the position the page was asked to follow.
		after store.LogCursor
		page  controller.LogPage
		err   error
	}
	verifyMsg struct {
		ok      bool
		problem string
		err     error
	}
	exportMsg struct {
		res ExportResult
		err error
	}
	tickMsg struct{}
)

// query is the filter for the server: the fixed scope plus what the reader
// has chosen here.
func (s *Screen) query() store.LogFilter {
	f := s.opts.Filter
	f.Contains, f.Outcome, f.Op = s.text, s.outcome, s.op
	f.After, f.Limit = store.LogCursor{}, 0
	return f
}

func (s *Screen) fetch() tea.Cmd {
	s.loading, s.loadErr = true, nil
	f := s.query()
	f.After, f.Limit = s.after, s.opts.PageSize
	gen, src, ctx := s.gen, s.src, s.ctx
	return tea.Batch(func() tea.Msg {
		page, err := src.AuditLog(ctx, f)
		return pageMsg{gen: gen, after: f.After, page: page, err: err}
	}, s.spin())
}

// reload starts over after the filter changed.
func (s *Screen) reload() tea.Cmd {
	s.gen++
	s.entries, s.after, s.done = nil, store.LogCursor{}, false
	s.cursor, s.offset = 0, 0
	s.note = nil
	return s.fetch()
}

// more fetches the next page when the cursor is close to the end of what
// is loaded. An error stops automatic fetching until the reader retries.
func (s *Screen) more() tea.Cmd {
	if s.done || s.loading || s.loadErr != nil {
		return nil
	}
	ahead := max(2*s.tableHeight(), 20)
	if s.cursor+ahead < len(s.entries) {
		return nil
	}
	return s.fetch()
}

// spinEvery is the spinner's frame time. Tests shorten it.
var spinEvery = 100 * time.Millisecond

func (s *Screen) spin() tea.Cmd {
	if s.spinning {
		return nil
	}
	s.spinning = true
	return tea.Tick(spinEvery, func(time.Time) tea.Msg { return tickMsg{} })
}

func (s *Screen) spinner() string {
	f := s.theme.Glyph.Spinner
	return f[s.frame%len(f)]
}

func (s *Screen) onPage(m pageMsg) tea.Cmd {
	if m.gen != s.gen || m.after != s.after {
		// For a filter the reader has since changed, or a position
		// already passed: appending it would list entries twice.
		return nil
	}
	s.loading = false
	if m.err != nil {
		s.loadErr = fail.From(m.err)
		return nil
	}
	for _, e := range m.page.Entries {
		s.seenOps[e.Op] = struct{}{}
	}
	s.entries = append(s.entries, m.page.Entries...)
	switch {
	case m.page.Next == nil:
		s.done = true
	case *m.page.Next == s.after:
		s.loadErr = fail.From(stalled(s.after))
	default:
		s.after = *m.page.Next
	}
	s.clamp()
	return s.more()
}

// --- update ----------------------------------------------------------------

// Update implements shell.Screen.
func (s *Screen) Update(msg tea.Msg) (shell.Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case shell.LayoutMsg:
		s.theme, s.keys, s.width, s.height, s.ready = msg.Theme, msg.Keys, msg.Width, msg.Height, true
		s.restyle()
		s.layout()
		return s, s.more()

	case pageMsg:
		return s, s.onPage(msg)

	case tickMsg:
		s.spinning = false
		if s.loading || s.busy != "" {
			s.frame++
			return s, s.spin()
		}
		return s, nil

	case verifyMsg:
		s.busy = ""
		switch {
		case msg.err != nil:
			s.showError("verify", fail.From(msg.err))
		case !msg.ok:
			s.showError("verify", Corrupt(msg.problem))
		default:
			s.note = &notice{theme.StatusOK, "Audit log verified " + s.theme.Glyph.Sep + " every hash chain is intact"}
		}
		return s, nil

	case exportMsg:
		s.busy = ""
		switch {
		case msg.err != nil && fail.From(msg.err).Code == CodeExists:
			s.pendingPath, s.mode = msg.res.Path, modeConfirm
		case msg.err != nil:
			s.showError("export", fail.From(msg.err))
		default:
			res := msg.res
			s.show("export", func(t theme.Theme, w int) string { return exportView(t, res, w) })
		}
		return s, nil

	case tea.KeyPressMsg:
		return s, s.onKey(msg)
	}

	// Anything else (a paste, a cursor blink) belongs to whichever
	// component has focus.
	var cmd tea.Cmd
	switch s.mode {
	case modeFilter, modeExport:
		s.input, cmd = s.input.Update(msg)
	case modeDetail, modeInfo:
		s.vp, cmd = s.vp.Update(msg)
	}
	return s, cmd
}

func (s *Screen) onKey(msg tea.KeyPressMsg) tea.Cmd {
	switch s.mode {
	case modeFilter, modeExport:
		return s.onInputKey(msg)
	case modeConfirm:
		switch {
		case key.Matches(msg, s.own.Yes):
			s.mode = modeList
			return s.export(s.pendingPath, true)
		case key.Matches(msg, s.own.No):
			s.mode, s.pendingPath = modeList, ""
			s.note = &notice{theme.StatusInfo, "Export cancelled; the existing file was kept"}
		}
		return nil
	case modeOps:
		return s.onOpsKey(msg)
	case modeDetail, modeInfo:
		return s.onPaneKey(msg)
	}

	page := max(s.tableHeight(), 1)
	switch {
	case key.Matches(msg, s.keys.Up):
		s.cursor--
	case key.Matches(msg, s.keys.Down):
		s.cursor++
	case key.Matches(msg, s.keys.PageUp):
		s.cursor -= page
	case key.Matches(msg, s.keys.PageDown):
		s.cursor += page
	case key.Matches(msg, s.keys.Top):
		s.cursor = 0
	case key.Matches(msg, s.keys.Bottom):
		s.cursor = len(s.entries) - 1
	case key.Matches(msg, s.keys.Select):
		if len(s.entries) > 0 {
			s.mode = modeDetail
			s.layout()
			s.vp.GotoTop()
		}
		return nil
	case key.Matches(msg, s.keys.Filter):
		return s.edit(modeFilter, s.text, "text in a source ID, target ID or detail")
	case key.Matches(msg, s.own.Outcome):
		switch s.outcome {
		case "":
			s.outcome = "ok"
		case "ok":
			s.outcome = "error"
		default:
			s.outcome = ""
		}
		return s.reload()
	case key.Matches(msg, s.own.Op):
		s.mode = modeOps
		s.opCursor = max(indexOf(s.opChoices(), s.op), 0)
		return nil
	case key.Matches(msg, s.own.Export):
		if s.busy != "" {
			return nil
		}
		return s.edit(modeExport, s.opts.ExportPath, "file name (.ndjson or .csv)")
	case key.Matches(msg, s.own.Verify):
		if s.busy != "" {
			return nil
		}
		s.busy, s.note = "Verifying the hash chains", nil
		src, ctx := s.src, s.ctx
		return tea.Batch(func() tea.Msg {
			ok, problem, err := src.Verify(ctx)
			return verifyMsg{ok: ok, problem: problem, err: err}
		}, s.spin())
	case key.Matches(msg, s.own.Retry):
		if s.loadErr != nil && !s.loading {
			return s.fetch()
		}
		return nil
	case key.Matches(msg, s.own.Cancel):
		s.note = nil
		return nil
	default:
		return nil
	}
	s.clamp()
	return s.more()
}

func (s *Screen) edit(m mode, value, placeholder string) tea.Cmd {
	s.mode = m
	s.note = nil
	s.input.Placeholder = placeholder
	s.input.SetValue(value)
	s.input.CursorEnd()
	s.layout()
	return s.input.Focus()
}

func (s *Screen) onInputKey(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case key.Matches(msg, s.own.Cancel):
		s.input.Blur()
		s.mode = modeList
		return nil
	case key.Matches(msg, s.own.Apply):
		value := strings.TrimSpace(s.input.Value())
		was := s.mode
		s.input.Blur()
		s.mode = modeList
		if was == modeFilter {
			if value == s.text {
				return nil
			}
			s.text = value
			return s.reload()
		}
		if value == "" {
			return nil
		}
		return s.export(expandHome(value), false)
	}
	var cmd tea.Cmd
	s.input, cmd = s.input.Update(msg)
	return cmd
}

func (s *Screen) export(path string, overwrite bool) tea.Cmd {
	s.busy, s.note = "Exporting to "+Sanitize(path), nil
	src, ctx, f := s.src, s.ctx, s.query()
	return tea.Batch(func() tea.Msg {
		res, err := Export(ctx, src, f, ExportOptions{Path: path, Overwrite: overwrite})
		return exportMsg{res: res, err: err}
	}, s.spin())
}

func expandHome(path string) string {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return path
}

// opChoices lists the operations seen so far, with "" (all) first.
func (s *Screen) opChoices() []string {
	ops := make([]string, 0, len(s.seenOps)+1)
	for op := range s.seenOps {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	return append([]string{""}, ops...)
}

func (s *Screen) onOpsKey(msg tea.KeyPressMsg) tea.Cmd {
	choices := s.opChoices()
	switch {
	case key.Matches(msg, s.keys.Up):
		s.opCursor--
	case key.Matches(msg, s.keys.Down):
		s.opCursor++
	case key.Matches(msg, s.keys.Top):
		s.opCursor = 0
	case key.Matches(msg, s.keys.Bottom):
		s.opCursor = len(choices) - 1
	case key.Matches(msg, s.own.Cancel):
		s.mode = modeList
		return nil
	case key.Matches(msg, s.own.Choose):
		s.mode = modeList
		if picked := choices[s.opCursor]; picked != s.op {
			s.op = picked
			return s.reload()
		}
		return nil
	}
	s.opCursor = min(max(s.opCursor, 0), len(choices)-1)
	return nil
}

func (s *Screen) onPaneKey(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case key.Matches(msg, s.own.Close):
		s.mode, s.info = modeList, nil
		return nil
	case key.Matches(msg, s.keys.Top):
		s.vp.GotoTop()
		return nil
	case key.Matches(msg, s.keys.Bottom):
		s.vp.GotoBottom()
		return nil
	}
	if s.mode == modeDetail {
		step := 0
		switch {
		case key.Matches(msg, s.own.PrevEntry):
			step = -1
		case key.Matches(msg, s.own.NextEntry):
			step = 1
		}
		if step != 0 {
			s.cursor += step
			s.clamp()
			s.layout()
			s.vp.GotoTop()
			return s.more()
		}
	}
	var cmd tea.Cmd
	s.vp, cmd = s.vp.Update(msg)
	return cmd
}

func (s *Screen) show(title string, render func(theme.Theme, int) string) {
	s.mode, s.infoTitle, s.info = modeInfo, title, render
	s.layout()
	s.vp.GotoTop()
}

func (s *Screen) showError(title string, fe *fail.Error) {
	s.show(title, func(t theme.Theme, w int) string { return errorView(t, fe, w) })
}

// --- geometry --------------------------------------------------------------

// The list uses three rows of its own: the filter line, the column titles
// and the status line.
const listChrome = 3

func (s *Screen) tableHeight() int { return max(s.height-listChrome, 1) }

// clamp keeps the cursor on an entry and the entry on screen.
func (s *Screen) clamp() {
	s.cursor = max(min(s.cursor, len(s.entries)-1), 0)
	h := s.tableHeight()
	if s.cursor < s.offset {
		s.offset = s.cursor
	}
	if s.cursor >= s.offset+h {
		s.offset = s.cursor - h + 1
	}
	s.offset = max(min(s.offset, len(s.entries)-h), 0)
}

// layout sizes the components for the current body and mode.
func (s *Screen) layout() {
	if !s.ready {
		return
	}
	s.clamp()
	s.input.SetWidth(max(s.width-inputLabelWidth-1, 8))
	s.vp.SetWidth(s.width)
	s.vp.SetHeight(max(s.height-paneChrome, 1))
	w := min(s.width, theme.MaxContentWidth)
	switch {
	case s.mode == modeDetail && s.cursor < len(s.entries):
		s.vp.SetContent(entryView(s.theme, s.entries[s.cursor], w))
	case s.mode == modeInfo && s.info != nil:
		s.vp.SetContent(s.info(s.theme, w))
	}
}

func indexOf(list []string, v string) int {
	for i, x := range list {
		if x == v {
			return i
		}
	}
	return -1
}

// --- help ------------------------------------------------------------------

// ShortHelp implements shell.Screen. The bar has to keep "? help" and
// "q quit" in view, so narrower terminals get fewer hints; the overlay
// always lists everything.
func (s *Screen) ShortHelp() []key.Binding {
	switch s.mode {
	case modeFilter, modeExport:
		return []key.Binding{s.own.Apply, s.own.Cancel}
	case modeConfirm:
		return []key.Binding{s.own.Yes, s.own.No}
	case modeOps:
		return []key.Binding{s.keys.Scroll, s.own.Choose, s.own.Cancel}
	case modeDetail:
		return []key.Binding{s.own.Close, s.own.PrevEntry, s.keys.Scroll}
	case modeInfo:
		return []key.Binding{s.own.Close, s.keys.Scroll}
	}
	open := s.keys.Select
	wide := s.width >= 104
	switch {
	case s.loadErr != nil && !wide:
		return []key.Binding{s.own.Retry, open, s.keys.Filter}
	case s.width < 76:
		return []key.Binding{s.keys.Scroll, open, s.keys.Filter}
	case !wide:
		return []key.Binding{open, s.keys.Filter, s.own.Outcome, s.own.Op, s.own.Export}
	}
	b := []key.Binding{s.keys.Scroll, open, s.keys.Filter, s.own.Outcome, s.own.Op, s.own.Export, s.own.Verify}
	if s.loadErr != nil {
		b = append([]key.Binding{s.own.Retry}, b...)
	}
	return b
}

// FullHelp implements shell.Screen.
func (s *Screen) FullHelp() [][]key.Binding {
	bottom := key.NewBinding(key.WithKeys("G"), key.WithHelp("G", "bottom (loads more)"))
	return [][]key.Binding{
		{s.keys.Up, s.keys.Down, s.keys.PageUp, s.keys.PageDown, s.keys.Top, bottom},
		{s.keys.Select, s.own.PrevEntry, s.keys.Filter, s.own.Outcome, s.own.Op},
		{s.own.Export, s.own.Verify, s.own.Retry},
	}
}
