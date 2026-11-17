package dashboard

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/sidecar/budget"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/tui/checklist"
	"github.com/bgfdev/taut/internal/tui/theme"
	"github.com/bgfdev/taut/internal/ui"
)

// wideAt is the body width from which the panes sit side by side: a
// 110-column terminal less the shell's margins.
const wideAt = 106

// layout is where everything goes at the current size. It depends only on
// the size, the focused pane and the shape of the snapshot, never on the
// numbers in it, so nothing reflows from one snapshot to the next.
type layout struct {
	wide        bool
	header      int // rows of header
	gap         int // blank rows under the header
	message     int // rows of message line at the bottom
	spaced      bool
	chanW       int
	sideW       int
	chanHead    bool
	chanRows    int
	clusterRows int
	probRows    int
	detailRows  int
}

func (s *Screen) hasMessage() bool {
	_, stale := s.stale()
	return s.confirm || s.retrying || s.notice != "" || stale
}

func (s *Screen) layout() layout {
	var l layout
	w, h := s.width, s.height
	if s.hasMessage() {
		l.message = 1
	}
	if h >= 16 {
		l.gap = 1
	}
	if s.detail != nil {
		l.header = 1
		l.detailRows = max(h-l.header-l.gap-l.message-1, 0)
		return l
	}
	l.header = 3 + len(s.stageLines(w))
	body := h - l.header - l.gap - l.message
	l.wide = w >= wideAt
	if l.wide {
		l.sideW = min(max(w*2/5, 46), 64)
		l.chanW = w - l.sideW - 3
		l.chanHead = body >= 6
		l.chanRows = body - 1
		if l.chanHead {
			l.chanRows--
		}
		l.clusterRows = min(len(s.clusterLines(l.sideW)), max((body-3)/2, 1))
		l.probRows = body - 3 - l.clusterRows
	} else {
		l.chanW, l.sideW = w, w
		rows := body - int(paneCount)
		// Decided before the message line is taken out, so a message
		// appearing never closes the gaps between panes.
		if l.spaced = body+l.message >= 14; l.spaced {
			rows -= 2
		}
		switch s.focus {
		case paneChannels:
			if l.chanHead = rows >= 6; l.chanHead {
				rows--
			}
			l.chanRows = rows
		case paneCluster:
			l.clusterRows = rows
		case paneProblems:
			l.probRows = rows
		}
	}
	l.chanRows, l.clusterRows, l.probRows = max(l.chanRows, 0), max(l.clusterRows, 0), max(l.probRows, 0)
	return l
}

// View implements shell.Screen.
func (s *Screen) View() string {
	if !s.ready {
		return ""
	}
	t, w := s.theme, s.width
	if !s.have {
		lines := []string{t.Style.Brand.Render(s.spinner()) + " " + t.Style.Text.Render("Waiting for the first snapshot from the controller")}
		if s.pollErr != nil {
			lines = append(lines, "", ui.ErrorBlock(t, fail.From(s.pollErr), min(w, 76)))
		}
		return strings.Join(lines, "\n")
	}
	l := s.layout()
	var lines []string
	if s.detail != nil {
		lines = append(lines, s.statusLine(w))
		lines = append(lines, blank(l.gap)...)
		all := s.detailLines(w)
		lines = append(lines, s.paneTitle("Problem", true, t.Style.Muted.Render(s.detail.lane), position(s.detail.off, l.detailRows, len(all)), w))
		lines = append(lines, window(all, s.detail.off, l.detailRows)...)
	} else {
		lines = append(lines, s.header(w)...)
		lines = append(lines, blank(l.gap)...)
		if l.wide {
			left := s.channelsPane(l, l.chanW)
			right := s.clusterPane(l, l.sideW)
			right = append(right, "")
			right = append(right, s.problemsPane(l, l.sideW)...)
			for i := range max(len(left), len(right)) {
				a, b := "", ""
				if i < len(left) {
					a = left[i]
				}
				if i < len(right) {
					b = right[i]
				}
				lines = append(lines, fit(a, l.chanW)+"   "+b)
			}
		} else {
			lines = append(lines, s.channelsPane(l, w)...)
			lines = append(lines, blank(btoi(l.spaced))...)
			lines = append(lines, s.clusterPane(l, w)...)
			lines = append(lines, blank(btoi(l.spaced))...)
			lines = append(lines, s.problemsPane(l, w)...)
		}
	}
	if l.message > 0 {
		for len(lines) < s.height-1 {
			lines = append(lines, "")
		}
		lines = append(lines[:min(len(lines), s.height-1)], s.messageLine(w))
	}
	for i, line := range lines {
		if ansi.StringWidth(line) > w {
			lines[i] = ansi.Truncate(line, w, "")
		}
	}
	return strings.Join(lines, "\n")
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func blank(n int) []string { return make([]string, max(n, 0)) }

// fit pads or clips s to exactly w cells.
func fit(s string, w int) string {
	n := ansi.StringWidth(s)
	if n > w {
		return ansi.Truncate(s, w, "")
	}
	return s + strings.Repeat(" ", w-n)
}

// window returns rows lines of all starting at off.
func window[T any](all []T, off, rows int) []T {
	off = min(max(off, 0), len(all))
	return all[off:min(off+max(rows, 0), len(all))]
}

// position says which lines of a scrolling list are on screen, when not
// all of them are.
func position(off, rows, total int) string {
	if total <= rows || rows <= 0 {
		return ""
	}
	return fmt.Sprintf("%d-%d of %s", off+1, min(off+rows, total), comma(total))
}

// spread puts right at the right-hand edge of a line of the given width,
// dropping it when there is no room.
func spread(left, right string, width int) string {
	gap := width - ansi.StringWidth(left) - ansi.StringWidth(right)
	if right == "" || gap < 2 {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}

func (s *Screen) markW() int {
	if s.theme.Unicode {
		return 1
	}
	return 2
}

// spinMark is the spinner in the status-mark column.
func (s *Screen) spinMark() string {
	g := s.spinner()
	return s.theme.Style.Brand.Render(g + strings.Repeat(" ", max(s.markW()-ansi.StringWidth(g), 0)))
}

func (s *Screen) dash() string {
	if s.theme.Unicode {
		return noValue
	}
	return "-"
}

// bar draws a progress bar. Unlike ui.Bar it rounds down and holds the last
// cell back until the work is finished; see barCells.
func (s *Screen) bar(frac float64, width int, final bool) string {
	t := s.theme
	n := barCells(frac, width, final)
	return t.Style.Brand.Render(strings.Repeat(t.Glyph.BarFull, n)) +
		t.Style.Faint.Render(strings.Repeat(t.Glyph.BarEmpty, max(width-n, 0)))
}

// --- header --------------------------------------------------------------------

var runningWords = map[store.RunStatus]string{
	store.RunAwaitingExport: "Waiting for the export upload",
	store.RunQueued:         "Planning",
	store.RunRunning:        "Importing",
	store.RunFinalizing:     "Finalizing",
}

var stoppedWords = map[store.RunStatus]struct {
	status theme.Status
	text   string
}{
	store.RunCompleted: {theme.StatusOK, "Import complete"},
	store.RunFinalized: {theme.StatusOK, "Migration finalized"},
	store.RunFailed:    {theme.StatusFail, "Import stopped"},
	store.RunCancelled: {theme.StatusSkip, "Run cancelled"},
}

// statusLine is the first line of the header: the run's state, identity,
// elapsed time and ETA, and how stale the snapshot is.
func (s *Screen) statusLine(w int) string {
	t, b := s.theme, &s.board
	run := b.v.Run
	var left string
	switch {
	case b.stopped && s.opts.Verdict != nil:
		left = s.opts.Verdict(t, run)
	case b.stopped:
		sw := stoppedWords[run.Status]
		left = ui.Verdict(t, sw.status, sw.text)
	default:
		word := runningWords[run.Status]
		if word == "" {
			word = string(run.Status)
		}
		left = s.spinMark() + " " + t.Style.Brand.Render(word)
	}
	sep := " " + t.Glyph.Sep + " "
	facts := []string{}
	facts = append(facts, "run "+shortID(run.ID))
	if e := b.elapsed(); e > 0 {
		facts = append(facts, checklist.Elapsed(e.Truncate(time.Second))+" elapsed")
	}
	if text := s.etaText(); text != "" {
		facts = append(facts, "ETA "+text)
	}
	left += t.Style.Muted.Render(sep + strings.Join(facts, sep))
	if b.p.Failed > 0 {
		left += t.Style.Muted.Render(sep) + t.Style.Danger.Render(comma(b.p.Failed)+" failed")
	}
	if b.p.Held > 0 {
		left += t.Style.Muted.Render(sep) + t.Style.Warning.Render(comma(b.p.Held)+" held")
	}
	right := ""
	if age, stale := s.stale(); stale {
		right = t.Style.Warning.Render(t.Glyph.Warn + " stale " + seconds(age))
	}
	// Clip the facts, never the stale marker.
	room := w
	if right != "" {
		room -= ansi.StringWidth(right) + 2
	}
	return spread(ansi.Truncate(left, max(room, 0), t.Glyph.Ellipsis), right, w)
}

// etaText is the estimate of the time left, "" when the run is not
// importing. A trailing "+" marks a floor: totals are still growing.
func (s *Screen) etaText() string {
	b := &s.board
	if b.v.Run.Status != store.RunRunning {
		return ""
	}
	if b.posts.counting {
		return s.dash()
	}
	// Posts are the long pole: files are moved ahead of the posts that
	// link to them. Only when every post is settled do the files left
	// decide the estimate.
	d, ok := eta(float64(b.posts.total-b.posts.done), s.trk.posts.rate())
	if b.posts.total-b.posts.done <= 0 {
		d, ok = eta(float64(b.files.total-b.files.done), s.trk.files.rate())
	}
	text := formatETA(d, ok, s.dash())
	if text != s.dash() && !b.final {
		text += "+"
	}
	return text
}

type measure struct {
	label, pct, count, rate string
	frac                    float64
	final                   bool
	samples                 []float64
}

func (s *Screen) measures(w int) [2]measure {
	t, b := s.theme, &s.board
	live := b.v.Run.Status == store.RunRunning

	posts := measure{label: "Posts", frac: b.posts.frac, final: b.posts.final, pct: s.dash()}
	switch {
	case b.posts.counting:
		posts.count = comma(b.posts.done) + " of " + t.Glyph.Ellipsis
	case b.posts.estimated:
		posts.count = comma(b.posts.done) + " of ~" + comma(max(roundEstimate(b.posts.total), b.posts.done))
	default:
		posts.count = comma(b.posts.done) + " of " + comma(b.posts.total)
	}
	if !b.posts.counting && b.posts.total > 0 {
		posts.pct = fmt.Sprintf("%d%%", percent(b.posts.frac, b.posts.final))
	}

	files := measure{label: "Files", frac: b.files.frac, final: b.files.final, pct: s.dash()}
	files.count = comma(b.files.done) + " of " + comma(b.files.total)
	if b.files.total == 0 && !b.files.final {
		files.count = "0 of " + t.Glyph.Ellipsis
	}
	if b.files.total > 0 {
		files.pct = fmt.Sprintf("%d%%", percent(b.files.frac, b.files.final))
		if w >= 96 && b.p.FileBytes > 0 {
			files.count += " " + t.Glyph.Sep + " " + byteSize(b.p.FileBytesDone) + " of " + byteSize(b.p.FileBytes)
		}
	}
	if live {
		posts.rate, posts.samples = perSecond(s.trk.posts.rate()), s.trk.posts.samples(ringSize)
		files.rate, files.samples = byteSize(int64(s.trk.bytes.rate()))+"/s", s.trk.bytes.samples(ringSize)
	}
	return [2]measure{posts, files}
}

// rateW fits "999.9 MB/s".
const rateW = 10

func (s *Screen) header(w int) []string {
	t := s.theme
	ms := s.measures(w)
	countW := max(ansi.StringWidth(ms[0].count), ansi.StringWidth(ms[1].count))
	// label, bar, percentage, counts, rate, sparkline.
	rest := w - (5 + 2) - (2 + 4) - (2 + countW) - (2 + rateW)
	barW, sparkW := min(max(rest, 6), 40), 0
	if rest >= 24+2+8 {
		barW = min(max(rest/2, 24), 40)
		sparkW = min(rest-barW-2, ringSize)
	}
	out := []string{s.statusLine(w)}
	for _, m := range ms {
		line := t.Style.Muted.Render(fmt.Sprintf("%-5s", m.label)) + "  " + s.bar(m.frac, barW, m.final) +
			"  " + fmt.Sprintf("%4s", m.pct) + "  " + fmt.Sprintf("%-*s", countW, m.count) +
			"  " + t.Style.Muted.Render(fmt.Sprintf("%*s", rateW, m.rate))
		if sparkW > 0 {
			line += "  " + t.Style.Brand.Render(sparkline(t.Glyph.Spark, m.samples, sparkW))
		}
		out = append(out, line)
	}
	return append(out, s.stageLines(w)...)
}

// stageLines is the stages strip: one cell per stage, of fixed width for a
// given plan so counts changing never shift a neighbor.
func (s *Screen) stageLines(w int) []string {
	t, b := s.theme, &s.board
	if len(b.stages) == 0 {
		return []string{t.Style.Muted.Render("Stages") + "  " + t.Style.Faint.Render("not planned yet")}
	}
	cells := func(roomy bool) []string {
		out := make([]string, len(b.stages))
		for i, st := range b.stages {
			digits := len(fmt.Sprint(st.Lanes))
			var mark string
			switch {
			case st.Failed > 0:
				mark = t.StatusMark(theme.StatusFail)
			case st.Done >= st.Lanes:
				mark = t.StatusMark(theme.StatusOK)
			case st.Leased > 0:
				mark = s.spinMark()
			default:
				mark = t.StatusMark(theme.StatusSkip)
			}
			cell := mark + " " + st.Stage + " " + fmt.Sprintf("%*d/%d", digits, st.Done, st.Lanes)
			if st.Done < st.Lanes {
				var note string
				switch {
				case roomy && st.Leased > 0:
					note = fmt.Sprintf("%d active", st.Leased)
				case roomy:
					note = "idle"
				case st.Leased > 0:
					note = fmt.Sprint(st.Leased)
				}
				width := digits
				if roomy {
					width += len(" active")
				}
				cell += " " + t.Style.Faint.Render(t.Glyph.Sep) + " " + t.Style.Muted.Render(fmt.Sprintf("%-*s", width, note))
			}
			out[i] = cell
		}
		return out
	}
	const gap = "   "
	label := t.Style.Muted.Render("Stages") + "  "
	indent := ansi.StringWidth(label)
	flow := func(cs []string) []string {
		lines, cur := []string{}, label
		for i, c := range cs {
			switch {
			case ansi.StringWidth(cur) == indent:
				cur += c
			case ansi.StringWidth(cur)+len(gap)+ansi.StringWidth(c) <= w:
				cur += gap + c
			default:
				lines = append(lines, cur)
				cur = strings.Repeat(" ", indent) + c
			}
			if i == len(cs)-1 {
				lines = append(lines, cur)
			}
		}
		return lines
	}
	if lines := flow(cells(true)); len(lines) == 1 {
		return lines
	}
	lines := flow(cells(false))
	if len(lines) > 2 {
		lines = lines[:2]
	}
	return lines
}

// --- panes ---------------------------------------------------------------------

// paneTitle is the line that heads a pane: its name (filled when the pane
// has focus), a summary, and at the right a scroll position or mode.
func (s *Screen) paneTitle(name string, focused bool, summary, right string, w int) string {
	t := s.theme
	var title string
	if focused {
		title = t.Style.Badge.Render(name)
	} else {
		title = " " + t.Style.Muted.Render(name) + " "
	}
	left := title
	if summary != "" {
		left += " " + summary
	}
	if right != "" {
		right = t.Style.Faint.Render(right)
	}
	return spread(left, right, w)
}

func (s *Screen) join(parts []string) string {
	return strings.Join(parts, s.theme.Style.Faint.Render(" "+s.theme.Glyph.Sep+" "))
}

func (s *Screen) channelsPane(l layout, w int) []string {
	t, b := s.theme, &s.board
	focused := s.focus == paneChannels
	parts := []string{t.Style.Muted.Render(comma(b.channels))}
	if s.filter != "" || s.filtering {
		parts = []string{t.Style.Muted.Render(fmt.Sprintf("%s of %s match", comma(b.shown), comma(b.channels)))}
	}
	if b.active > 0 {
		parts = append(parts, t.Style.Brand.Render(comma(b.active)+" active"))
	}
	if b.failed > 0 {
		parts = append(parts, t.Style.Danger.Render(comma(b.failed)+" failed"))
	}
	if b.done > 0 {
		parts = append(parts, t.Style.Muted.Render(comma(b.done)+" done"))
	}
	summary := s.join(parts)
	if s.filtering {
		cursor := "▏"
		if !t.Unicode {
			cursor = "_"
		}
		summary += "  " + t.Style.Key.Render("/") + s.filter + t.Style.Brand.Render(cursor)
	} else if s.filter != "" {
		summary += "  " + t.Style.Key.Render("/") + s.filter
	}
	right := "by state"
	if s.byName {
		right = "by name"
	}
	if len(b.lines) > l.chanRows && l.chanRows > 0 {
		// Count channels, not lines: team headings scroll too but are not
		// what the reader is counting.
		first, last := 0, 0
		for i, ln := range b.lines[:min(s.chanOff+l.chanRows, len(b.lines))] {
			if ln.row < 0 {
				continue
			}
			if last++; i < s.chanOff {
				first++
			}
		}
		right += " " + t.Glyph.Sep + " " + fmt.Sprintf("%d-%d of %s", min(first+1, last), last, comma(b.shown))
	}
	out := []string{s.paneTitle("Channels", focused, summary, right, w)}
	if !l.wide && !focused {
		return out
	}

	c := s.chanCols(w)
	if l.chanHead {
		out = append(out, t.Style.Faint.Render(c.head()))
	}
	switch {
	case b.channels == 0:
		if l.chanRows > 0 {
			out = append(out, "  "+t.Style.Muted.Render("No channels yet: the run is still being planned."))
		}
		return out
	case b.shown == 0:
		if l.chanRows > 0 {
			out = append(out, "  "+t.Style.Muted.Render("No channel matches the filter. Press esc to clear it."))
		}
		return out
	}
	for _, ln := range window(b.lines, s.chanOff, l.chanRows) {
		tm := b.teams[ln.team]
		if ln.row < 0 {
			name := tm.name
			if name == "" {
				name = "No team"
			}
			one, many := "channel", "channels"
			if tm.chats {
				one, many = "conversation", "conversations"
			}
			out = append(out, t.Style.Heading.Render(name)+"  "+
				t.Style.Muted.Render(plural(len(tm.rows), one, many)+", "+comma(tm.done)+" done"))
			continue
		}
		out = append(out, s.channelRow(tm.rows[ln.row], c))
	}
	return out
}

// chanCols are the column widths of the channels table at one pane width.
type chanCols struct {
	mark, name, bar, posts, files int
	showFiles                     bool
}

const maxNameW = 26

func (s *Screen) chanCols(w int) chanCols {
	b := &s.board
	c := chanCols{mark: s.markW(), name: min(max(b.nameW, 8), maxNameW), posts: max(b.postsW, len("posts")), files: b.filesW, showFiles: true}
	extras := 0
	if b.extrasW > 0 {
		extras = 2 + b.extrasW
	}
	fixed := func() int {
		n := 2 + c.mark + 1 + c.name + 2 + 2 + 4 + 2 + c.posts + extras
		if c.showFiles {
			n += 2 + c.files
		}
		return n
	}
	// When the pane is narrow, give up name width first, then the files
	// column. The failed and held counts are never dropped.
	const minBar, minName = 6, 12
	if short := minBar - (w - fixed()); short > 0 {
		c.name = max(c.name-short, min(c.name, minName))
	}
	if w-fixed() < minBar {
		c.showFiles = false
	}
	// The bar is the one column that repeats another (the percentage), so
	// it goes entirely before a count is clipped.
	if c.bar = min(w-fixed(), 30); c.bar < 4 {
		c.bar = 0
	}
	return c
}

func (c chanCols) head() string {
	h := strings.Repeat(" ", 2+c.mark+1) + fmt.Sprintf("%-*s", c.name, "channel") + "  "
	if c.bar > 0 {
		h += fmt.Sprintf("%-*s", c.bar+2+4, "imported")
	} else {
		h += "    "
	}
	h += "  " + fmt.Sprintf("%*s", c.posts, "posts")
	if c.showFiles {
		h += "  " + fmt.Sprintf("%*s", c.files, "files")
	}
	return h
}

func (s *Screen) channelRow(r chanRow, c chanCols) string {
	t, b := s.theme, &s.board
	var mark string
	switch {
	case r.class == classFailed:
		mark = t.StatusMark(theme.StatusFail)
	case r.class == classActive:
		mark = s.spinMark()
	case r.attention:
		mark = t.StatusMark(theme.StatusWarn)
	case r.class == classDone:
		mark = t.StatusMark(theme.StatusOK)
	default:
		mark = t.StatusMark(theme.StatusSkip)
	}
	// By width on screen, not by character: the names of a direct
	// message's participants need not be ASCII.
	name := display(r.c)
	if ansi.StringWidth(name) > c.name {
		name = ansi.Truncate(name, c.name, t.Glyph.Ellipsis)
	}
	name += strings.Repeat(" ", max(c.name-ansi.StringWidth(name), 0))
	nameStyle := t.Style.Text
	if r.class == classDone {
		nameStyle = t.Style.Muted
	}
	pad := func(text string, w int) string {
		return strings.Repeat(" ", max(w-ansi.StringWidth(text), 0)) + text
	}
	pct, posts := t.Style.Faint.Render(pad(s.dash(), 4)), t.Style.Faint.Render(pad(s.dash(), c.posts))
	if r.c.Posts > 0 {
		pct = pad(fmt.Sprintf("%d%%", percent(r.frac, b.final)), 4)
		posts = pad(comma(r.c.Posted)+"/"+comma(r.c.Posts), c.posts)
	}
	if r.c.State == store.ChannelSkipped {
		// Microsoft refused a chat for this conversation: none of it was
		// imported, and none of it will be.
		pct = t.Style.Warning.Render(pad("skip", 4))
	}
	line := "  " + mark + " " + nameStyle.Render(name) + "  "
	if c.bar > 0 {
		line += s.bar(r.frac, c.bar, b.final) + "  "
	}
	line += pct + "  " + posts
	if c.showFiles {
		files := t.Style.Faint.Render(pad(s.dash(), c.files))
		if r.c.Files > 0 {
			files = pad(comma(min(r.c.FilesDone, r.c.Files))+"/"+comma(r.c.Files), c.files)
		}
		line += "  " + files
	}
	var extra []string
	if r.c.Failed > 0 {
		extra = append(extra, t.Style.Danger.Render(comma(r.c.Failed)+" failed"))
	}
	if r.c.Held > 0 {
		extra = append(extra, t.Style.Warning.Render(comma(r.c.Held)+" held"))
	}
	if len(extra) > 0 {
		line += "  " + s.join(extra)
	}
	return line
}

// --- cluster ---------------------------------------------------------------------

func (s *Screen) throttleText() (theme.Status, string) {
	th := throttles(s.board.v)
	sep := " " + s.theme.Glyph.Sep + " "
	recent := "none in the last 5 min"
	if th.recent > 0 {
		recent = comma(th.recent)
		if th.capped {
			recent += "+"
		}
		recent += " in the last 5 min"
	}
	if len(th.scopes) == 0 {
		return theme.StatusOK, "clear" + sep + recent
	}
	scopes := make([]string, 0, maxThrottleScopes+1)
	for _, scope := range th.scopes[:min(len(th.scopes), maxThrottleScopes)] {
		scopes = append(scopes, s.clipName(scope))
	}
	if n := len(th.scopes) - len(scopes); n > 0 {
		scopes = append(scopes, s.more(n))
	}
	return theme.StatusWarn, "throttled " + strings.Join(scopes, ", ") + sep + seconds(th.left) + " left" + sep + recent
}

func (s *Screen) clusterPane(l layout, w int) []string {
	t, b := s.theme, &s.board
	focused := s.focus == paneCluster
	n := len(b.v.Cluster.Workers)
	status, throttle := s.throttleText()
	summary := t.Style.Muted.Render(plural(n, "worker", "workers"))
	if status != theme.StatusOK {
		summary = s.join([]string{summary, t.StatusStyle(status).Render(strings.SplitN(throttle, " "+t.Glyph.Sep+" ", 3)[0])})
	}
	lines := s.clusterLines(w)
	out := []string{s.paneTitle("Cluster", focused, summary, position(s.clusterOff, l.clusterRows, len(lines)), w)}
	if !l.wide && !focused {
		return out
	}
	return append(out, window(lines, s.clusterOff, l.clusterRows)...)
}

const maxHolderW = 22

// What the cluster pane will draw of the budgets and throttles the controller
// reports, however many there are and however long their names.
const (
	maxBudgetRows     = 8
	maxThrottleScopes = 8
	maxScopeW         = 24
)

// clipName shortens a tenant or scope name to maxScopeW cells.
func (s *Screen) clipName(name string) string {
	return ansi.Truncate(name, maxScopeW, s.theme.Glyph.Ellipsis)
}

// more says how many items a capped list left out.
func (s *Screen) more(n int) string {
	return s.theme.Glyph.Ellipsis + " " + comma(n) + " more"
}

func (s *Screen) clusterLines(w int) []string {
	t, b := s.theme, &s.board
	v := b.v
	var out []string

	out = append(out, t.Style.Heading.Render("Workers"))
	if len(v.Cluster.Workers) == 0 {
		if v.Run.Status == store.RunRunning {
			out = append(out, "  "+t.StatusMark(theme.StatusWarn)+" "+t.Style.Text.Render("No worker holds a lease on this run."))
		} else {
			out = append(out, "  "+t.Style.Muted.Render("None at work."))
		}
	}
	nameW, lanesW, stagesW := 0, 0, 0
	for _, h := range v.Cluster.Workers {
		nameW = max(nameW, min(len([]rune(h.Holder)), maxHolderW))
		lanesW = max(lanesW, len(plural(h.Lanes, "lane", "lanes")))
		stagesW = max(stagesW, len(strings.Join(h.Stages, ", ")))
	}
	leases := make([]string, len(v.Cluster.Workers))
	leaseW := 0
	for i, h := range v.Cluster.Workers {
		left := h.LeaseExpires.Sub(v.At)
		seen := s.trk.leases[h.Holder]
		switch {
		case left <= 0:
			leases[i] = t.Style.Danger.Render(t.Glyph.Fail + " lease expired")
		case s.trk.leaseExpiring(h.Holder, left):
			leases[i] = t.Style.Warning.Render(t.Glyph.Warn + " lease expires in " + seconds(left))
		case seen.renewed:
			leases[i] = t.Style.Muted.Render("renewed " + seconds(v.At.Sub(seen.changedAt)) + " ago")
		default:
			leases[i] = t.Style.Muted.Render("lease " + seconds(left) + " left")
		}
		leaseW = max(leaseW, ansi.StringWidth(leases[i]))
	}
	stagesW = min(stagesW, max(w-2-nameW-2-lanesW-2-2-leaseW, 0))
	for i, h := range v.Cluster.Workers {
		name := []rune(h.Holder)
		if len(name) > nameW {
			name = append(name[:nameW-ansi.StringWidth(t.Glyph.Ellipsis)], []rune(t.Glyph.Ellipsis)...)
		}
		line := "  " + fmt.Sprintf("%-*s", nameW, string(name)) + "  " +
			t.Style.Muted.Render(fmt.Sprintf("%-*s", lanesW, plural(h.Lanes, "lane", "lanes")))
		if stagesW > 0 {
			line += "  " + t.Style.Muted.Render(fit(strings.Join(h.Stages, ", "), stagesW))
		}
		out = append(out, line+"  "+leases[i])
	}

	out = append(out, t.Style.Heading.Render("Budgets"))
	if len(v.Cluster.Budgets) == 0 {
		out = append(out, "  "+t.Style.Muted.Render("No egress proxy is asking for a share."))
	}
	// Budgets are built from lease requests on the bus, which anyone who
	// can reach it may send: only a bounded number of rows is drawn, names
	// are clipped, and a budget's holders are counted, never listed.
	budgets := v.Cluster.Budgets[:min(len(v.Cluster.Budgets), maxBudgetRows)]
	tenants := map[string]bool{}
	for _, k := range budgets {
		tenants[k.Key.Tenant] = true
	}
	scopeW, totalW, govW := 0, 0, 0
	label := func(k budget.Key) string {
		if len(tenants) > 1 {
			return s.clipName(k.Scope) + " " + shortID(k.Tenant)
		}
		return s.clipName(k.Scope)
	}
	for _, k := range budgets {
		scopeW = max(scopeW, ansi.StringWidth(label(k.Key)))
		totalW = max(totalW, len(rps(k.TotalRPS)))
		govW = max(govW, len(plural(len(k.Holders), "proxy", "proxies")))
	}
	for _, k := range budgets {
		out = append(out, "  "+fit(label(k.Key), scopeW)+"  "+
			fmt.Sprintf("%*s rps", totalW, rps(k.TotalRPS))+"  "+
			t.Style.Muted.Render(fmt.Sprintf("%-*s", govW, plural(len(k.Holders), "proxy", "proxies"))+"  "+rps(k.ShareRPS)+" rps each"))
	}
	if more := len(v.Cluster.Budgets) - len(budgets); more > 0 {
		out = append(out, "  "+t.Style.Muted.Render(s.more(more)))
	}

	status, text := s.throttleText()
	out = append(out, t.Style.Heading.Render("Throttling"), "  "+t.StatusMark(status)+" "+t.Style.Text.Render(text))
	return out
}

// --- problems --------------------------------------------------------------------

func (s *Screen) problemsPane(l layout, w int) []string {
	t, b := s.theme, &s.board
	focused := s.focus == paneProblems
	failed := b.failedLanes()
	var parts []string
	if failed > 0 {
		parts = append(parts, t.Style.Danger.Render(comma(failed)+" failed"))
	}
	if n := len(b.problems) - failed; n > 0 {
		parts = append(parts, t.Style.Warning.Render(comma(n)+" retrying"))
	}
	if len(parts) == 0 {
		parts = append(parts, t.Style.Muted.Render("none"))
	}
	out := []string{s.paneTitle("Problems", focused, s.join(parts), position(s.probOff, l.probRows, len(b.problems)), w)}
	if !l.wide && !focused {
		return out
	}
	if len(b.problems) == 0 {
		if l.probRows > 0 {
			out = append(out, "  "+t.Style.Muted.Render("No lane is failing or retrying."))
		}
		return out
	}
	subjW, stateW := 0, 0
	for _, p := range b.problems {
		subjW = max(subjW, len([]rune(b.subject(p.LaneID))))
		stateW = max(stateW, len(b.problemState(p)))
	}
	subjW = min(subjW, max(w/3, 12))
	for i, p := range b.problems {
		if i < s.probOff || i >= s.probOff+l.probRows {
			continue
		}
		cursor := "  "
		if focused && i == s.probCur {
			cursor = t.Style.Brand.Render(fit(t.Glyph.Pointer, 2))
		}
		status := theme.StatusWarn
		if isFailed(p) {
			status = theme.StatusFail
		}
		subject := []rune(b.subject(p.LaneID))
		if len(subject) > subjW {
			subject = append(subject[:subjW-ansi.StringWidth(t.Glyph.Ellipsis)], []rune(t.Glyph.Ellipsis)...)
		}
		line := cursor + t.StatusMark(status) + " " + fmt.Sprintf("%-*s", subjW, string(subject)) + "  " +
			t.StatusStyle(status).Render(fmt.Sprintf("%-*s", stateW, b.problemState(p))) + "  "
		room := w - ansi.StringWidth(line)
		if room > 4 {
			line += t.Style.Muted.Render(ansi.Truncate(oneLine(p.Error), room, t.Glyph.Ellipsis))
		}
		out = append(out, line)
	}
	return out
}

// --- message line ----------------------------------------------------------------

func (s *Screen) messageLine(w int) string {
	t := s.theme
	var line string
	switch {
	case s.confirm:
		line = t.Style.Warning.Render(t.Glyph.Warn) + " " +
			t.Style.Title.Render("Return "+plural(s.board.failedLanes(), "failed lane", "failed lanes")+" to the queue?") + "  " +
			t.Style.Key.Render("y") + t.Style.Muted.Render(" retry "+t.Glyph.Sep+" ") + t.Style.Key.Render("n") + t.Style.Muted.Render(" cancel")
	case s.retrying:
		line = s.spinMark() + " " + t.Style.Text.Render("Returning failed lanes to the queue")
	case s.notice != "":
		line = t.StatusMark(s.noticeAs) + " " + t.Style.Text.Render(s.notice)
	default:
		line = t.StatusMark(theme.StatusWarn) + " " + t.Style.Text.Render(reason(s.pollErr)) +
			t.Style.Muted.Render(" "+t.Glyph.Sep+" showing the last snapshot")
	}
	return ansi.Truncate(line, w, t.Glyph.Ellipsis)
}

// Summary describes the cluster in a few label/value rows, for output that
// is not a terminal.
func Summary(t theme.Theme, v controller.DashboardView) [][2]string {
	s := &Screen{theme: t, board: board{v: v}}
	workers := "none at work"
	if n := len(v.Cluster.Workers); n > 0 {
		lanes := 0
		for _, h := range v.Cluster.Workers {
			lanes += h.Lanes
		}
		workers = plural(n, "worker", "workers") + " on " + plural(lanes, "lane", "lanes")
	}
	rows := [][2]string{{"Workers", workers}}
	var budgets []string
	for _, k := range v.Cluster.Budgets {
		if len(budgets) == maxBudgetRows+1 {
			break
		}
		budgets = append(budgets, s.clipName(k.Key.Scope)+" "+rps(k.TotalRPS)+" rps across "+plural(len(k.Holders), "egress proxy", "egress proxies"))
	}
	if len(budgets) > 0 {
		if extra := len(v.Cluster.Budgets) - maxBudgetRows; extra > 0 {
			budgets = append(budgets[:maxBudgetRows], s.more(extra))
		}
		rows = append(rows, [2]string{"Budgets", strings.Join(budgets, ", ")})
	}
	_, throttle := s.throttleText()
	return append(rows, [2]string{"Throttling", throttle})
}

// reason is an error in one line: what happened and, when it is short
// enough to help, why.
func reason(err error) string {
	fe := fail.From(err)
	switch {
	case fe.Code == "unexpected" && fe.Why != "":
		return oneLine(fe.Why)
	case fe.Why != "" && len(fe.Why) <= 60:
		return oneLine(fe.What) + " (" + oneLine(fe.Why) + ")"
	}
	return oneLine(fe.What)
}
