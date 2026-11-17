package auditlog

import (
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/tui/theme"
	"github.com/bgfdev/taut/internal/ui"
)

// OutcomeStatus maps an entry's outcome onto the shared status vocabulary.
// An outcome this version does not know is shown as information rather than
// guessed at.
func OutcomeStatus(outcome string) theme.Status {
	switch outcome {
	case "ok":
		return theme.StatusOK
	case "error":
		return theme.StatusFail
	}
	return theme.StatusInfo
}

// Endpoints renders "source → target" for an entry, sanitized.
func Endpoints(t theme.Theme, e store.LogEntry) string {
	src, dst := Sanitize(e.SourceID), Sanitize(e.TargetID)
	if dst == "" {
		return src
	}
	return src + " " + t.Glyph.Arrow + " " + dst
}

// Corrupt is the error for an audit log whose hash chains do not verify.
func Corrupt(problem string) *fail.Error {
	problem = Sanitize(problem)
	if problem == "" {
		problem = "The controller found a broken hash chain but did not say where."
	}
	return fail.New("audit.corrupt",
		"The audit log does not verify",
		problem,
		"Do not rely on this audit trail as evidence. Keep the database as it is and compare it with a backup; `taut audit --stream <stream>` lists the affected chain.")
}

// Comma formats n with thousands separators.
func Comma(n int) string {
	s := itoa(int64(n))
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// Times are shown in UTC: an audit trail is read by people in different
// places, and must say the same thing to each of them.
const (
	timeWide   = "2006-01-02T15:04:05Z"
	timeNarrow = "01-02 15:04:05"
)

type columns struct {
	time, timeLayout  string
	timeW, markW, opW int
	endsW, detailW    int
}

func (s *Screen) columns() columns {
	c := columns{time: "TIME (UTC)", timeLayout: timeNarrow, timeW: len(timeNarrow), opW: 16, markW: 1}
	if !s.theme.Unicode {
		c.markW = 2
	}
	switch {
	case s.width >= 100:
		c.timeLayout, c.timeW, c.opW = timeWide, len(timeWide), 26
	case s.width >= 90:
		c.opW = 23
	}
	rest := s.width - pointerW - c.timeW - colGap - c.markW - 1 - c.opW - colGap
	if rest >= 44 {
		c.endsW = min(rest*3/5, 46)
		c.detailW = rest - c.endsW - colGap
	} else {
		c.endsW = max(rest, 1)
	}
	return c
}

const (
	pointerW        = 2
	colGap          = 2
	inputLabelWidth = 11 // "Export to" and its gap
	// paneChrome is the heading and the blank line above a pane's body.
	paneChrome = 2
)

// cell truncates text to w cells with an ellipsis and pads it to exactly w.
func cell(text string, w int, ellipsis string) string {
	if w <= 0 {
		return ""
	}
	if ansi.StringWidth(text) > w {
		text = ansi.Truncate(text, w, ellipsis)
	}
	return text + strings.Repeat(" ", max(w-ansi.StringWidth(text), 0))
}

// View implements shell.Screen.
func (s *Screen) View() string {
	if !s.ready {
		return ""
	}
	switch s.mode {
	case modeDetail, modeInfo:
		return s.paneHeading() + "\n\n" + s.vp.View()
	}
	lines := make([]string, 0, s.height)
	lines = append(lines, s.filterLine())
	if s.mode == modeOps {
		lines = append(lines, s.opsRows()...)
	} else {
		lines = append(lines, s.tableRows()...)
	}
	return strings.Join(append(lines, s.statusLine()), "\n")
}

func (s *Screen) filterLine() string {
	t := s.theme
	on, off := t.Style.Accent, t.Style.Muted
	var parts []string
	switch s.outcome {
	case "":
		parts = append(parts, off.Render("all outcomes"))
	case "error":
		parts = append(parts, on.Render("errors only"))
	default:
		parts = append(parts, on.Render(Sanitize(s.outcome)+" only"))
	}
	if s.op == "" {
		parts = append(parts, off.Render("all operations"))
	} else {
		parts = append(parts, on.Render(Sanitize(s.op)))
	}
	if s.text != "" {
		parts = append(parts, on.Render(`"`+Sanitize(s.text)+`"`))
	}
	if s.opts.Scope != "" {
		parts = append(parts, off.Render(Sanitize(s.opts.Scope)))
	}
	left := strings.Join(parts, t.Style.Faint.Render(" "+t.Glyph.Sep+" "))

	var right string
	if n := len(s.entries); n > 0 {
		total := Comma(n)
		if !s.done {
			total += "+" // more can be loaded; the real total is not known
		}
		right = t.Style.Muted.Render(Comma(s.cursor+1) + " of " + total)
	}
	if s.loading && len(s.entries) > 0 {
		right = t.Style.Brand.Render(s.spinner()) + " " + t.Style.Muted.Render("loading more") + "  " + right
	}
	room := s.width - lipgloss.Width(right) - 2
	if lipgloss.Width(left) > room {
		left = ansi.Truncate(left, max(room, 0), t.Glyph.Ellipsis)
	}
	gap := s.width - lipgloss.Width(left) - lipgloss.Width(right)
	return left + strings.Repeat(" ", max(gap, 1)) + right
}

func (s *Screen) tableRows() []string {
	t, c, h := s.theme, s.columns(), s.tableHeight()
	head := strings.Repeat(" ", pointerW) +
		cell(c.time, c.timeW, "") + strings.Repeat(" ", colGap) +
		strings.Repeat(" ", c.markW+1) + cell("OPERATION", c.opW, "") + strings.Repeat(" ", colGap) +
		cell("SOURCE "+t.Glyph.Arrow+" TARGET", c.endsW, t.Glyph.Ellipsis)
	if c.detailW > 0 {
		head += strings.Repeat(" ", colGap) + cell("DETAIL", c.detailW, "")
	}
	rows := []string{t.Style.Faint.Render(strings.TrimRight(head, " "))}

	switch {
	case len(s.entries) == 0 && s.loading:
		rows = append(rows, strings.Repeat(" ", pointerW)+t.Style.Brand.Render(s.spinner())+" "+t.Style.Text.Render("Reading the audit log"))
	case len(s.entries) == 0 && s.done:
		rows = append(rows,
			strings.Repeat(" ", pointerW)+t.Style.Text.Render("No entries match this filter."),
			strings.Repeat(" ", pointerW)+ui.Inline(t, "Press `/`, `o` or `p` to change it.", t.Style.Muted))
	}
	for i := s.offset; i < len(s.entries) && i < s.offset+h; i++ {
		rows = append(rows, s.row(s.entries[i], c, i == s.cursor))
	}
	for len(rows) < h+1 {
		rows = append(rows, "")
	}
	return rows
}

func (s *Screen) row(e store.LogEntry, c columns, selected bool) string {
	t := s.theme
	paint := func(base lipgloss.Style, text string) string {
		if selected {
			base = base.Background(t.Color.Surface)
		}
		return base.Render(text)
	}
	gap := paint(t.Style.Text, strings.Repeat(" ", colGap))
	status := OutcomeStatus(e.Outcome)

	ptr := strings.Repeat(" ", pointerW)
	if selected {
		ptr = cell(t.Glyph.Pointer, pointerW, "")
	}
	text := t.Style.Text
	if selected {
		text = text.Bold(true)
	}
	line := paint(t.Style.Brand, ptr) +
		paint(t.Style.Muted, e.At.UTC().Format(c.timeLayout)) + gap +
		paint(t.StatusStyle(status), cell(t.StatusGlyph(status), c.markW, "")) + paint(t.Style.Text, " ") +
		paint(text, cell(Sanitize(e.Op), c.opW, t.Glyph.Ellipsis)) + gap +
		paint(text, cell(Endpoints(t, e), c.endsW, t.Glyph.Ellipsis))
	if c.detailW > 0 {
		line += gap + paint(t.Style.Muted, cell(Sanitize(e.Detail), c.detailW, t.Glyph.Ellipsis))
	}
	return line
}

func (s *Screen) opsRows() []string {
	t, h := s.theme, s.tableHeight()
	rows := []string{t.Style.Heading.Render("Show one operation") + "  " + t.Style.Muted.Render("from those seen so far")}
	choices := s.opChoices()
	first := max(min(s.opCursor-h+1, len(choices)-h), 0)
	if s.opCursor < first {
		first = s.opCursor
	}
	for i := first; i < len(choices) && i < first+h; i++ {
		label := Sanitize(choices[i])
		if choices[i] == "" {
			label = "All operations"
		}
		label = cell(label, max(s.width-pointerW-12, 8), t.Glyph.Ellipsis)
		line := strings.Repeat(" ", pointerW) + t.Style.Text.Render(label)
		if i == s.opCursor {
			line = t.Style.Brand.Render(cell(t.Glyph.Pointer, pointerW, "")) + t.Style.Text.Bold(true).Render(label)
		}
		if choices[i] == s.op {
			line += t.Style.Muted.Render("  current")
		}
		rows = append(rows, strings.TrimRight(line, " "))
	}
	for len(rows) < h+1 {
		rows = append(rows, "")
	}
	return rows
}

// statusLine is the last row of the list: whatever most needs saying.
func (s *Screen) statusLine() string {
	t, w := s.theme, s.width
	sep := " " + t.Glyph.Sep + " "
	switch {
	case s.mode == modeFilter:
		return t.Style.Muted.Render("Filter") + "  " + s.input.View()
	case s.mode == modeExport:
		return t.Style.Muted.Render("Export to") + "  " + s.input.View()
	case s.mode == modeConfirm:
		ask := " already exists. Replace it?  "
		keys := t.Style.Key.Render("y") + t.Style.Muted.Render(" replace"+sep) + t.Style.Key.Render("n") + t.Style.Muted.Render(" keep")
		room := w - markW(t) - 1 - len(ask) - lipgloss.Width(keys)
		return t.StatusMark(theme.StatusWarn) + " " +
			t.Style.Text.Render(ansi.Truncate(Sanitize(s.pendingPath), max(room, 8), t.Glyph.Ellipsis)+ask) + keys
	case s.busy != "":
		return t.Style.Brand.Render(s.spinner()) + " " + t.Style.Text.Render(ansi.Truncate(s.busy, max(w-3, 1), t.Glyph.Ellipsis))
	case s.loadErr != nil:
		// The retry hint must survive truncation: it is the way out.
		hint := t.Style.Muted.Render(sep) + t.Style.Key.Render("r") + t.Style.Muted.Render(" retry")
		why := Sanitize(strings.ReplaceAll(s.loadErr.What, "`", ""))
		if s.loadErr.Why != "" {
			why += ": " + Sanitize(strings.ReplaceAll(s.loadErr.Why, "`", ""))
		}
		room := w - markW(t) - 1 - lipgloss.Width(hint)
		return t.StatusMark(theme.StatusFail) + " " + t.Style.Danger.Render(ansi.Truncate(why, max(room, 1), t.Glyph.Ellipsis)) + hint
	case s.note != nil:
		return t.StatusMark(s.note.status) + " " + t.Style.Text.Render(ansi.Truncate(s.note.text, max(w-markW(t)-1, 1), t.Glyph.Ellipsis))
	case s.cursor < len(s.entries):
		e := s.entries[s.cursor]
		line := Sanitize(e.Stream) + " #" + itoa(e.Seq)
		if d := Sanitize(e.Detail); d != "" {
			line += sep + d
		}
		return t.Style.Muted.Render(ansi.Truncate(line, w, t.Glyph.Ellipsis))
	}
	return ""
}

func markW(t theme.Theme) int {
	if t.Unicode {
		return 1
	}
	return 2
}

func (s *Screen) paneHeading() string {
	t := s.theme
	// When the body does not fit, say how far through it the view is, so
	// it is plain there is more to scroll to.
	scroll := ""
	if s.vp.TotalLineCount() > s.vp.VisibleLineCount() {
		scroll = t.Style.Faint.Render(itoa(int64(s.vp.ScrollPercent()*100+0.5)) + "%")
	}
	if s.mode == modeInfo {
		left := t.Style.Heading.Render(strings.ToUpper(s.infoTitle[:1]) + s.infoTitle[1:])
		return left + strings.Repeat(" ", max(s.width-lipgloss.Width(left)-lipgloss.Width(scroll), 1)) + scroll
	}
	if s.cursor >= len(s.entries) {
		return ""
	}
	e := s.entries[s.cursor]
	total := Comma(len(s.entries))
	if !s.done {
		total += "+"
	}
	right := t.Style.Muted.Render(Comma(s.cursor+1) + " of " + total)
	if scroll != "" {
		right = scroll + "  " + right
	}
	left := t.Style.Heading.Render("Entry") + "  " +
		t.Style.Muted.Render(ansi.Truncate(Sanitize(e.Stream)+" #"+itoa(e.Seq), max(s.width-lipgloss.Width(right)-10, 8), t.Glyph.Ellipsis))
	gap := s.width - lipgloss.Width(left) - lipgloss.Width(right)
	return left + strings.Repeat(" ", max(gap, 1)) + right
}

// fields renders aligned label/value rows whose values wrap, in full,
// inside the value column.
func fields(t theme.Theme, rows [][2]string, width int) string {
	lw := 0
	for _, r := range rows {
		lw = max(lw, lipgloss.Width(r[0]))
	}
	gap := t.Space.MD
	vw := max(width-lw-gap, 16)
	var b strings.Builder
	for i, r := range rows {
		if i > 0 {
			b.WriteByte('\n')
		}
		label := t.Style.Muted.Render(cell(r[0], lw, "")) + strings.Repeat(" ", gap)
		if r[1] == "" {
			b.WriteString(label + t.Style.Faint.Render("none"))
			continue
		}
		for j, l := range strings.Split(ansi.Wrap(r[1], vw, ""), "\n") {
			if j > 0 {
				b.WriteString("\n" + strings.Repeat(" ", lw+gap))
			}
			if j == 0 {
				b.WriteString(label)
			}
			b.WriteString(l)
		}
	}
	return b.String()
}

// entryView shows every field of an entry, nothing truncated.
func entryView(t theme.Theme, e store.LogEntry, width int) string {
	status := OutcomeStatus(e.Outcome)
	return fields(t, [][2]string{
		{"Stream", Sanitize(e.Stream)},
		{"Seq", itoa(e.Seq)},
		{"Time", e.At.UTC().Format(time.RFC3339Nano)},
		{"Actor", Sanitize(e.Actor)},
		{"Operation", Sanitize(e.Op)},
		{"Outcome", t.StatusMark(status) + " " + Sanitize(e.Outcome)},
		{"Source ID", Sanitize(e.SourceID)},
		{"Target ID", Sanitize(e.TargetID)},
		{"Content hash", Sanitize(e.ContentHash)},
		{"Detail", Sanitize(e.Detail)},
		{"Previous hash", Sanitize(e.PrevHash)},
		{"Entry hash", Sanitize(e.EntryHash)},
	}, width)
}

// ExportSummary is the verdict line for a finished export.
func ExportSummary(t theme.Theme, r ExportResult) string {
	n := Comma(r.Entries) + " entries"
	if r.Entries == 1 {
		n = "1 entry"
	}
	facts := []string{string(r.Format), Comma(int(r.Bytes)) + " bytes"}
	if r.Truncated {
		facts = append(facts, "stopped at --limit; more entries match")
	}
	return ui.Verdict(t, theme.StatusOK, "Exported "+n, facts...)
}

func exportView(t theme.Theme, r ExportResult, width int) string {
	head := ExportSummary(t, r)
	body := fields(t, [][2]string{
		{"File", Sanitize(r.Path)},
		{"SHA-256", r.SHA256},
	}, width)
	note := "Record the SHA-256 with the file: it shows later that the export has not changed."
	if r.Entries == 0 {
		note = "Nothing matched the filter, so the file holds no entries. " + note
	}
	return head + "\n\n" + body + "\n\n" + t.Style.Muted.Render(ui.Wrap(note, width))
}

func errorView(t theme.Theme, fe *fail.Error, width int) string {
	clean := *fe
	clean.What, clean.Why = Sanitize(fe.What), Sanitize(fe.Why)
	return ui.ErrorBlock(t, &clean, width)
}
