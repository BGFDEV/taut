package dashboard

import (
	"context"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/tui/shell"
	"github.com/bgfdev/taut/internal/tui/theme"
)

// Options configure the dashboard.
type Options struct {
	// Context bounds every request the dashboard makes.
	Context context.Context
	// Interval is the time between snapshots. Default one second.
	Interval time.Duration
	// Initial, when set, is drawn at once instead of waiting for the first
	// fetch.
	Initial *controller.DashboardView
	// Verdict renders the one-line outcome of a run that has stopped, so
	// the dashboard words it exactly as the rest of the CLI does. When nil
	// the run's status is shown.
	Verdict func(t theme.Theme, v controller.RunView) string
	// Now is the operator's clock, used only to say how old a stale
	// snapshot is. Default time.Now.
	Now func() time.Time
}

const (
	defaultInterval = time.Second
	// maxBackoff caps the wait between fetches while the controller is not
	// answering.
	maxBackoff = 30 * time.Second
	// idleFactor slows fetching once the run has stopped; it can still
	// change (a retry, `taut finalize`), just not quickly.
	idleFactor = 5
	frameEvery = 120 * time.Millisecond
	// logRefresh is how many snapshots pass between refreshes of an open
	// lane's audit log entries.
	logRefresh = 5
)

type pane int

const (
	paneChannels pane = iota
	paneCluster
	paneProblems
	paneCount
)

type (
	snapMsg struct {
		v   controller.DashboardView
		err error
	}
	pollMsg  struct{}
	frameMsg struct{}
	retryMsg struct {
		n   int
		err error
	}
)

// Screen is the dashboard. It implements shell.Screen.
type Screen struct {
	src   Source
	opts  Options
	after func(time.Duration, tea.Msg) tea.Cmd

	theme  theme.Theme
	keys   shell.Keys
	width  int
	height int
	ready  bool

	// Data.
	have     bool
	snap     controller.DashboardView
	board    board
	trk      tracker
	snaps    int
	lastOK   time.Time // operator's clock at the last good snapshot
	failures int
	pollErr  error
	inFlight bool
	frame    int
	ticking  bool

	// Interaction.
	focus      pane
	byName     bool
	filter     string
	filtering  bool
	chanOff    int
	clusterOff int
	probCur    int
	probOff    int
	probLane   string
	detail     *detail
	confirm    bool
	retrying   bool
	notice     string
	noticeAs   theme.Status
}

// New returns the dashboard for src.
func New(src Source, o Options) *Screen {
	if o.Context == nil {
		o.Context = context.Background()
	}
	if o.Interval <= 0 {
		o.Interval = defaultInterval
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	s := &Screen{src: src, opts: o, theme: theme.Default(), keys: shell.DefaultKeys()}
	s.after = func(d time.Duration, msg tea.Msg) tea.Cmd {
		return tea.Tick(d, func(time.Time) tea.Msg { return msg })
	}
	if o.Initial != nil {
		s.ingest(*o.Initial)
	}
	return s
}

// Last returns the most recent snapshot, and false if there never was one.
func (s *Screen) Last() (controller.DashboardView, bool) { return s.snap, s.have }

// Title implements shell.Screen.
func (s *Screen) Title() string {
	if s.detail != nil {
		return s.board.subject(s.detail.lane)
	}
	return ""
}

// CapturingInput implements shell.InputCapturer.
func (s *Screen) CapturingInput() bool { return s.filtering || s.confirm }

// Init implements shell.Screen.
func (s *Screen) Init() tea.Cmd {
	var first tea.Cmd
	if s.have {
		first = s.after(s.opts.Interval, pollMsg{})
	} else {
		s.inFlight = true
		first = s.fetch()
	}
	return tea.Batch(first, s.tick())
}

func (s *Screen) fetch() tea.Cmd {
	ctx, src, limit := s.opts.Context, s.src, max(5*s.opts.Interval, 10*time.Second)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, limit)
		defer cancel()
		v, err := src.Dashboard(ctx)
		return snapMsg{v, err}
	}
}

func (s *Screen) retry() tea.Cmd {
	ctx, src := s.opts.Context, s.src
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		n, err := src.Retry(ctx)
		return retryMsg{n, err}
	}
}

// animating reports whether anything on screen moves between snapshots.
func (s *Screen) animating() bool {
	return !s.have || !s.board.stopped || s.failures > 0 || s.retrying ||
		(s.detail != nil && s.detail.loading)
}

// tick keeps the spinner (and the age of a stale snapshot) moving, and
// stops by itself when nothing needs it.
func (s *Screen) tick() tea.Cmd {
	if s.ticking || !s.animating() {
		return nil
	}
	s.ticking = true
	return s.after(frameEvery, frameMsg{})
}

// backoff is the wait before the next fetch after n consecutive failures.
func backoff(interval time.Duration, n int) time.Duration {
	d := interval
	for range min(n, 16) {
		if d *= 2; d >= maxBackoff {
			return maxBackoff
		}
	}
	return d
}

func (s *Screen) ingest(v controller.DashboardView) {
	s.trk.observe(v)
	s.snap, s.have, s.failures, s.pollErr, s.lastOK = v, true, 0, nil, s.opts.Now()
	s.snaps++
	s.rebuild()
}

// rebuild derives the board again from the snapshot in hand, after a new
// snapshot or a change of sort or filter.
func (s *Screen) rebuild() {
	s.board = buildBoard(s.snap, s.byName, s.filter)
	// Keep the cursor on the same lane as the list changes under it.
	if s.probLane != "" {
		for i, p := range s.board.problems {
			if p.LaneID == s.probLane {
				s.probCur = i
				break
			}
		}
	}
	if d := s.detail; d != nil {
		d.resolved = true
		for _, p := range s.board.problems {
			if p.LaneID == d.lane {
				d.prob, d.resolved = p, false
				break
			}
		}
	}
	s.clamp()
}

// clamp keeps every scroll position and cursor inside its list.
func (s *Screen) clamp() {
	l := s.layout()
	s.chanOff = min(max(s.chanOff, 0), max(len(s.board.lines)-l.chanRows, 0))
	s.clusterOff = min(max(s.clusterOff, 0), max(len(s.clusterLines(l.sideW))-l.clusterRows, 0))
	s.probCur = min(max(s.probCur, 0), max(len(s.board.problems)-1, 0))
	if s.probCur < len(s.board.problems) {
		s.probLane = s.board.problems[s.probCur].LaneID
	} else {
		s.probLane = ""
	}
	if rows := l.probRows; rows > 0 {
		s.probOff = min(s.probOff, s.probCur)
		s.probOff = max(s.probOff, s.probCur-rows+1)
	}
	s.probOff = min(max(s.probOff, 0), max(len(s.board.problems)-l.probRows, 0))
	if d := s.detail; d != nil {
		d.off = min(max(d.off, 0), max(len(s.detailLines(s.width))-l.detailRows, 0))
	}
}

// Update implements shell.Screen.
func (s *Screen) Update(msg tea.Msg) (shell.Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case shell.LayoutMsg:
		s.theme, s.keys, s.width, s.height, s.ready = msg.Theme, msg.Keys, msg.Width, msg.Height, true
		s.clamp()
		return s, nil

	case snapMsg:
		s.inFlight = false
		if msg.err != nil {
			if s.opts.Context.Err() != nil {
				return s, nil
			}
			s.failures++
			s.pollErr = msg.err
			return s, tea.Batch(s.after(backoff(s.opts.Interval, s.failures), pollMsg{}), s.tick())
		}
		s.ingest(msg.v)
		wait := s.opts.Interval
		if s.board.stopped {
			wait *= idleFactor
		}
		cmds := []tea.Cmd{s.after(wait, pollMsg{}), s.tick()}
		if d := s.detail; d != nil && !d.loading && s.snaps-d.loadedAt >= logRefresh {
			cmds = append(cmds, s.loadLog(false))
		}
		return s, tea.Batch(cmds...)

	case pollMsg:
		if s.inFlight {
			return s, nil
		}
		s.inFlight = true
		return s, s.fetch()

	case frameMsg:
		s.ticking = false
		s.frame++
		return s, s.tick()

	case logMsg:
		if d := s.detail; d != nil && d.lane == msg.lane {
			d.loading, d.loadedAt = false, s.snaps
			d.err = msg.err
			if msg.err == nil {
				d.errs, d.recent, d.truncated, d.loaded = msg.errs, msg.recent, msg.truncated, true
			}
			s.clamp()
		}
		return s, nil

	case retryMsg:
		s.retrying = false
		switch {
		case msg.err != nil:
			s.notice, s.noticeAs = "Retry failed: "+reason(msg.err), theme.StatusFail
		case msg.n == 0:
			s.notice, s.noticeAs = "Nothing to retry: no lane has failed", theme.StatusInfo
		default:
			s.notice, s.noticeAs = "Returned "+plural(msg.n, "lane", "lanes")+" to the queue", theme.StatusOK
		}
		s.clamp()
		var cmds []tea.Cmd
		if !s.inFlight {
			s.inFlight = true
			cmds = append(cmds, s.fetch())
		}
		if s.detail != nil {
			cmds = append(cmds, s.loadLog(false))
		}
		return s, tea.Batch(cmds...)

	case tea.KeyPressMsg:
		cmd := s.key(msg)
		s.clamp()
		return s, cmd
	}
	return s, nil
}

func (s *Screen) key(msg tea.KeyPressMsg) tea.Cmd {
	k := msg.String()
	if s.confirm {
		s.confirm = false
		if k == "y" || k == "Y" || k == "enter" {
			s.retrying = true
			return tea.Batch(s.retry(), s.tick())
		}
		return nil
	}
	s.notice = ""
	if s.filtering {
		switch {
		case k == "enter":
			s.filtering = false
		case k == "esc":
			s.filtering, s.filter = false, ""
		case k == "backspace":
			if r := []rune(s.filter); len(r) > 0 {
				s.filter = string(r[:len(r)-1])
			} else {
				s.filtering = false
			}
		case msg.Text != "":
			s.filter += msg.Text
		default:
			return nil
		}
		s.chanOff = 0
		s.rebuild()
		return nil
	}

	l := s.layout()
	if d := s.detail; d != nil {
		switch {
		case key.Matches(msg, s.keys.Back):
			s.detail = nil
		case k == "r":
			return s.askRetry()
		default:
			d.off = scroll(msg, s.keys, d.off, l.detailRows, len(s.detailLines(s.width)))
		}
		return nil
	}

	switch {
	case key.Matches(msg, s.keys.Next):
		s.focus = (s.focus + 1) % paneCount
	case key.Matches(msg, s.keys.Prev):
		s.focus = (s.focus + paneCount - 1) % paneCount
	case k == "1", k == "2", k == "3":
		s.focus = pane(k[0] - '1')
	case key.Matches(msg, s.keys.Filter):
		s.focus, s.filtering = paneChannels, true
	case k == "o":
		s.byName = !s.byName
		s.rebuild()
	case k == "r":
		return s.askRetry()
	case key.Matches(msg, s.keys.Back):
		if s.filter != "" {
			s.filter, s.chanOff = "", 0
			s.rebuild()
		}
	case key.Matches(msg, s.keys.Select):
		if s.focus == paneProblems && s.probCur < len(s.board.problems) {
			p := s.board.problems[s.probCur]
			s.detail = &detail{lane: p.LaneID, prob: p}
			return tea.Batch(s.loadLog(true), s.tick())
		}
	default:
		switch s.focus {
		case paneChannels:
			s.chanOff = scroll(msg, s.keys, s.chanOff, l.chanRows, len(s.board.lines))
		case paneCluster:
			s.clusterOff = scroll(msg, s.keys, s.clusterOff, l.clusterRows, len(s.clusterLines(l.sideW)))
		case paneProblems:
			// The cursor moves a line at a time; scroll treats it as a
			// one-row window so paging still works.
			n := len(s.board.problems)
			switch {
			case key.Matches(msg, s.keys.PageDown):
				s.probCur += max(l.probRows, 1)
			case key.Matches(msg, s.keys.PageUp):
				s.probCur -= max(l.probRows, 1)
			default:
				s.probCur = scroll(msg, s.keys, s.probCur, 1, n)
			}
			s.probCur = min(max(s.probCur, 0), max(n-1, 0))
			s.probLane = ""
		}
	}
	return nil
}

// scroll applies a navigation key to an offset into total lines shown rows
// at a time.
func scroll(msg tea.KeyPressMsg, k shell.Keys, off, rows, total int) int {
	switch {
	case key.Matches(msg, k.Up):
		off--
	case key.Matches(msg, k.Down):
		off++
	case key.Matches(msg, k.PageUp):
		off -= max(rows, 1)
	case key.Matches(msg, k.PageDown):
		off += max(rows, 1)
	case key.Matches(msg, k.Top):
		off = 0
	case key.Matches(msg, k.Bottom):
		off = total
	}
	return min(max(off, 0), max(total-rows, 0))
}

func (s *Screen) askRetry() tea.Cmd {
	switch {
	case s.retrying:
	case s.board.failedLanes() == 0:
		s.notice, s.noticeAs = "Nothing to retry: no lane has failed", theme.StatusInfo
	default:
		s.confirm = true
	}
	return nil
}

// ShortHelp implements shell.Screen.
func (s *Screen) ShortHelp() []key.Binding {
	b := s.bindings()
	switch {
	case s.confirm:
		return []key.Binding{b.yes, b.no}
	case s.filtering:
		return []key.Binding{b.apply, b.clear}
	case s.detail != nil:
		return []key.Binding{s.keys.Scroll, b.retry, s.keys.Back}
	}
	switch s.focus {
	case paneChannels:
		return []key.Binding{s.keys.Scroll, s.keys.Filter, b.sort, b.pane}
	case paneCluster:
		return []key.Binding{s.keys.Scroll, b.pane}
	}
	return []key.Binding{s.keys.Scroll, s.keys.Select, b.retry, b.pane}
}

// FullHelp implements shell.Screen.
func (s *Screen) FullHelp() [][]key.Binding {
	b := s.bindings()
	return [][]key.Binding{
		{s.keys.Up, s.keys.Down, s.keys.PageUp, s.keys.PageDown, s.keys.Top, s.keys.Bottom},
		{b.pane, b.prev, b.jump, s.keys.Filter, b.sort, s.keys.Select, b.retry},
	}
}

type bindings struct {
	pane, prev, jump, sort, retry, yes, no, apply, clear key.Binding
}

func (s *Screen) bindings() bindings {
	nb := func(k, help string) key.Binding { return key.NewBinding(key.WithKeys(k), key.WithHelp(k, help)) }
	return bindings{
		pane:  nb("tab", "next pane"),
		prev:  nb("shift+tab", "previous pane"),
		jump:  key.NewBinding(key.WithKeys("1", "2", "3"), key.WithHelp("1-3", "jump to pane")),
		sort:  nb("o", "sort by "+map[bool]string{false: "name", true: "state"}[s.byName]),
		retry: nb("r", "retry failed lanes"),
		yes:   nb("y", "retry"),
		no:    nb("n", "cancel"),
		apply: nb("enter", "keep filter"),
		clear: nb("esc", "clear"),
	}
}

// stale is how old the snapshot on screen is, when fetches are failing.
func (s *Screen) stale() (time.Duration, bool) {
	if s.failures == 0 || !s.have {
		return 0, false
	}
	return max(s.opts.Now().Sub(s.lastOK), 0), true
}

func (s *Screen) spinner() string {
	g := s.theme.Glyph.Spinner
	return g[s.frame%len(g)]
}

// oneLine flattens an error message for a single-row cell.
func oneLine(text string) string { return strings.Join(strings.Fields(text), " ") }

var _ interface {
	shell.Screen
	shell.InputCapturer
} = (*Screen)(nil)
