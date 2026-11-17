package dashboard

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/tui/theme"
	"github.com/bgfdev/taut/internal/ui"
)

// detail is the drill-in on one lane from the problems pane.
type detail struct {
	lane string
	prob store.LaneProblem
	// resolved is true once the lane is no longer in the problem list; the
	// last known problem stays on screen.
	resolved bool

	loading   bool
	loaded    bool
	loadedAt  int // the snapshot count when the entries were fetched
	err       error
	errs      []store.LogEntry // newest first
	recent    []store.LogEntry // newest first, errors excluded
	truncated bool
	off       int
}

type logMsg struct {
	lane      string
	errs      []store.LogEntry
	recent    []store.LogEntry
	truncated bool
	err       error
}

const (
	// keepErrors and keepRecent bound what the drill-in shows.
	keepErrors = 50
	keepRecent = 100
	// recentWindow is how far back "recent activity" reaches.
	recentWindow = 10 * time.Minute
	logPageSize  = 1000
	logPages     = 4
)

// tail pages through an audit log query, which is oldest first, and keeps the
// newest entries. truncated is true when the page budget ran out before
// the end, so newer entries exist that are not shown.
func tail(ctx context.Context, src Source, f store.LogFilter, keep int) (entries []store.LogEntry, truncated bool, err error) {
	for range logPages {
		page, err := src.AuditLog(ctx, f)
		if err != nil {
			return nil, false, err
		}
		entries = append(entries, page.Entries...)
		if len(entries) > keep {
			entries = entries[len(entries)-keep:]
		}
		if page.Next == nil || len(page.Entries) == 0 {
			return entries, false, nil
		}
		f.After = *page.Next
	}
	return entries, true, nil
}

func newestFirst(in []store.LogEntry, keep func(store.LogEntry) bool) []store.LogEntry {
	out := make([]store.LogEntry, 0, len(in))
	for i := len(in) - 1; i >= 0; i-- {
		if keep == nil || keep(in[i]) {
			out = append(out, in[i])
		}
	}
	return out
}

// loadLog fetches the open lane's audit log entries: its errors first, then
// everything recent.
func (s *Screen) loadLog(first bool) tea.Cmd {
	d := s.detail
	if d == nil {
		return nil
	}
	if first {
		d.loading = true
	}
	ctx, src, lane, at := s.opts.Context, s.src, d.lane, s.snap.At
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		msg := logMsg{lane: lane}
		errs, t1, err := tail(ctx, src, store.LogFilter{Stream: lane, Outcome: "error", Limit: logPageSize}, keepErrors)
		if err != nil {
			msg.err = err
			return msg
		}
		f := store.LogFilter{Stream: lane, Limit: logPageSize}
		if !at.IsZero() {
			f.Since = at.Add(-recentWindow)
		}
		all, t2, err := tail(ctx, src, f, keepRecent)
		if err != nil {
			msg.err = err
			return msg
		}
		msg.errs = newestFirst(errs, nil)
		msg.recent = newestFirst(all, func(e store.LogEntry) bool { return e.Outcome != "error" })
		msg.truncated = t1 || t2
		return msg
	}
}

// problemState words a problem's status: when the next attempt is, on
// The controller's clock.
func (b *board) problemState(p store.LaneProblem) string {
	if isFailed(p) {
		return "failed"
	}
	if wait := p.NotBefore.Sub(b.v.At); wait > 0 && !p.NotBefore.IsZero() {
		return fmt.Sprintf("attempt %d in %s", p.Attempts+1, seconds(wait))
	}
	return fmt.Sprintf("attempt %d now", p.Attempts+1)
}

// detailLines renders the whole drill-in; the view shows a window of it.
func (s *Screen) detailLines(width int) []string {
	d, t, b := s.detail, s.theme, &s.board
	if d == nil {
		return nil
	}
	var out []string
	add := func(block string) { out = append(out, strings.Split(block, "\n")...) }

	p := d.prob
	subject := b.subject(d.lane)
	switch {
	case d.resolved:
		add(ui.Verdict(t, theme.StatusOK, subject+" is no longer failing"))
	case isFailed(p):
		add(ui.Verdict(t, theme.StatusFail, subject+" failed", plural(p.Attempts, "attempt", "attempts")))
	default:
		add(ui.Verdict(t, theme.StatusWarn, subject+" is retrying", b.problemState(p)))
	}
	out = append(out, "", t.Style.Heading.Render("Error"))
	msg := strings.TrimSpace(p.Error)
	if msg == "" {
		msg = "No error text was recorded."
	}
	add(ui.Indent(ui.Wrap(msg, max(width-2, 20)), 2))

	out = append(out, "", ui.HeadingNote(t, "Audit log errors", s.logNote(len(d.errs))))
	switch {
	case d.loading && !d.loaded:
		out = append(out, "  "+t.Style.Brand.Render(s.spinner())+" "+t.Style.Muted.Render("Reading the audit log"))
	case d.err != nil && !d.loaded:
		add(ui.Indent(ui.Finding(t, theme.StatusWarn, "The audit log could not be read: "+reason(d.err), "", max(width-2, 20)), 2))
	case len(d.errs) == 0:
		out = append(out, "  "+t.Style.Muted.Render("This lane has written no errors to the audit log."))
	default:
		for _, e := range d.errs {
			out = append(out, s.entryLines(e, width)...)
		}
	}
	if d.loaded {
		out = append(out, "", ui.HeadingNote(t, "Recent activity", "last 10 minutes, newest first"))
		if len(d.recent) == 0 {
			out = append(out, "  "+t.Style.Muted.Render("Nothing else was written in that time."))
		}
		for _, e := range d.recent {
			out = append(out, s.entryLines(e, width)...)
		}
		if d.truncated {
			out = append(out, "  "+ui.Inline(t, "The lane has more entries than are shown here; `taut audit` has them all.", t.Style.Muted))
		}
	}
	return out
}

func (s *Screen) logNote(n int) string {
	d := s.detail
	if !d.loaded {
		return ""
	}
	note := comma(n)
	if n >= keepErrors {
		note = "newest " + note
	}
	if d.err != nil {
		note += " " + s.theme.Glyph.Sep + " refresh failed"
	}
	return note
}

// opWidth is the operation column of an audit log line.
const opWidth = 24

func (s *Screen) entryLines(e store.LogEntry, width int) []string {
	t := s.theme
	status := theme.StatusOK
	if e.Outcome == "error" {
		status = theme.StatusFail
	}
	// Audit log pages reach the dashboard exactly as recorded (an export must
	// not alter them), so an entry is made safe here, where it is drawn.
	op, source, target := ui.Sanitize(e.Op), ui.Sanitize(e.SourceID), ui.Sanitize(e.TargetID)
	line := "  " + t.Style.Muted.Render(e.At.UTC().Format("15:04:05")) + "  " + t.StatusMark(status) + " " +
		fmt.Sprintf("%-*s", opWidth, op) + " " + source
	if target != "" {
		line += " " + t.Style.Faint.Render(t.Glyph.Arrow) + " " + target
	}
	out := []string{ansi.Truncate(line, width, t.Glyph.Ellipsis)}
	if detail := strings.TrimSpace(ui.SanitizeLines(e.Detail)); detail != "" && e.Outcome == "error" {
		indent := 2 + 8 + 2 + s.markW() + 1
		for _, l := range strings.Split(ui.Wrap(detail, max(width-indent, 20)), "\n") {
			out = append(out, strings.Repeat(" ", indent)+t.Style.Muted.Render(l))
		}
	}
	return out
}
