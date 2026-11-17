package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/tui/theme"
)

// The functions in this file are the shared building blocks for output.
// They return strings and take the theme and a width, so the same block
// renders inside the full-screen shell and in plain output.
//
// The visual grammar, kept deliberately small:
//
//   - One left margin. Everything hangs off the same two-column gutter.
//   - Hierarchy comes from weight and dimness, not from boxes or rules:
//     titles are bold, facts are plain, commentary is dim.
//   - Color means state. Only status glyphs, and the one thing the reader
//     should do next, are colored.
//   - Columns line up. Labels in a group share a width so the eye can run
//     down the details.
//   - Detail is folded away until it matters: a passing check is one line.

// Margin is the left gutter, in columns, for plain (non full-screen) output.
const Margin = 2

// Pad indents every line of s by the standard margin.
func Pad(s string) string { return Indent(s, Margin) }

// Indent indents every non-empty line of s by n columns.
func Indent(s string, n int) string {
	if n <= 0 || s == "" {
		return s
	}
	pad := strings.Repeat(" ", n)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = pad + l
		}
	}
	return strings.Join(lines, "\n")
}

// Inline styles a line of prose, rendering `backtick` spans as commands.
// Styling is applied per line with the code state carried across lines, so
// already-wrapped text can be passed in.
func Inline(t theme.Theme, text string, base lipgloss.Style) string {
	lines := strings.Split(text, "\n")
	inCode := false
	for i, line := range lines {
		var b strings.Builder
		for j, seg := range strings.Split(line, "`") {
			if j > 0 {
				inCode = !inCode
			}
			if seg == "" {
				continue
			}
			if inCode {
				b.WriteString(t.Style.Code.Render(seg))
			} else {
				b.WriteString(base.Render(seg))
			}
		}
		lines[i] = b.String()
	}
	return strings.Join(lines, "\n")
}

// inlineWidth is the display width of text once backticks are removed.
func inlineWidth(text string) int {
	return lipgloss.Width(strings.ReplaceAll(text, "`", ""))
}

// wrapInline wraps prose to width. Backticks are zero-width once rendered,
// so lines containing them wrap slightly early, which is harmless.
func wrapInline(text string, width int) string {
	if width < 20 {
		width = 20
	}
	return ansi.Wordwrap(text, width, "")
}

// Banner renders the product mark and a command title: the first line of
// every command's output.
func Banner(t theme.Theme, command, subtitle string) string {
	s := t.Style.Brand.Render(t.Glyph.Mark+" taut") + " " + t.Style.Title.Render(command)
	if subtitle != "" {
		s += "  " + t.Style.Muted.Render(subtitle)
	}
	return s
}

// Heading renders a section title. The width is accepted for call-site
// symmetry with other blocks; titles carry no rule.
func Heading(t theme.Theme, title string, _ int) string {
	return t.Style.Heading.Render(title)
}

// HeadingNote is Heading with a dim trailing note, such as a count.
func HeadingNote(t theme.Theme, title, note string) string {
	if note == "" {
		return t.Style.Heading.Render(title)
	}
	return t.Style.Heading.Render(title) + "  " + t.Style.Muted.Render(note)
}

// markWidth is the width of a status mark in the current glyph set.
func markWidth(t theme.Theme) int {
	if t.Unicode {
		return 1
	}
	return 2
}

// CheckRow renders one status line with the label padded to labelWidth so
// that details in a group align. A detail that does not fit is truncated for
// passing rows, where it is only colour, and wrapped beneath the row for
// everything else, where it is the explanation.
func CheckRow(t theme.Theme, s theme.Status, label string, labelWidth int, detail string, width int) string {
	lw := max(labelWidth, lipgloss.Width(label))
	line := t.StatusMark(s) + " " + t.Style.Text.Render(label)
	if detail == "" {
		return line
	}
	gap := lw - lipgloss.Width(label) + 2
	indent := markWidth(t) + 1 + lw + 2
	room := width - indent
	if inlineWidth(detail) <= room {
		return line + strings.Repeat(" ", gap) + Inline(t, detail, t.Style.Muted)
	}
	if s == theme.StatusOK || s == theme.StatusInfo || s == theme.StatusSkip {
		if room > 8 {
			plain := strings.ReplaceAll(detail, "`", "")
			line += strings.Repeat(" ", gap) + t.Style.Muted.Render(ansi.Truncate(plain, room, t.Glyph.Ellipsis))
		}
		return line
	}
	// Wrap in the detail column when it leaves a readable measure;
	// otherwise beneath the label.
	if room < 32 {
		under := markWidth(t) + 1
		return line + "\n" + Indent(Inline(t, wrapInline(detail, width-under), t.Style.Muted), under)
	}
	styled := strings.Split(Inline(t, wrapInline(detail, room), t.Style.Muted), "\n")
	line += strings.Repeat(" ", gap) + styled[0]
	for _, l := range styled[1:] {
		line += "\n" + strings.Repeat(" ", indent) + l
	}
	return line
}

// CheckLine renders one status line without column alignment.
func CheckLine(t theme.Theme, s theme.Status, label, detail string, width int) string {
	return CheckRow(t, s, label, 0, detail, width)
}

// Hint renders a follow-up beneath a row: the step that resolves a warning
// or failure. It hangs off a connector so it reads as belonging to the row
// above.
func Hint(t theme.Theme, text string, width int) string {
	indent := markWidth(t) + 1
	conn := t.Glyph.Elbow + " "
	cw := lipgloss.Width(conn)
	lines := strings.Split(Inline(t, wrapInline(text, width-indent-cw), t.Style.Text), "\n")
	pad := strings.Repeat(" ", indent)
	for i, l := range lines {
		if i == 0 {
			lines[i] = pad + t.Style.Faint.Render(conn) + l
		} else {
			lines[i] = pad + strings.Repeat(" ", cw) + l
		}
	}
	return strings.Join(lines, "\n")
}

// Finding renders a blocker or warning: a status mark, the wrapped message,
// and the next step beneath it.
func Finding(t theme.Theme, s theme.Status, message, next string, width int) string {
	indent := markWidth(t) + 1
	lines := strings.Split(Inline(t, wrapInline(message, width-indent), t.Style.Text), "\n")
	for i, l := range lines {
		if i == 0 {
			lines[i] = t.StatusMark(s) + " " + l
		} else {
			lines[i] = strings.Repeat(" ", indent) + l
		}
	}
	out := strings.Join(lines, "\n")
	if next != "" {
		out += "\n" + Hint(t, next, width)
	}
	return out
}

// Next renders the closing call to action: the command to run next and what
// it does.
func Next(t theme.Theme, command, what string) string {
	s := t.Style.Muted.Render("Next") + "  " + t.Style.Code.Bold(true).Render(command)
	if what != "" {
		s += "  " + t.Style.Muted.Render(what)
	}
	return s
}

// Verdict renders the one-line outcome of a command: a status mark, a short
// bold statement, and dim supporting facts joined by separators.
func Verdict(t theme.Theme, s theme.Status, statement string, facts ...string) string {
	out := t.StatusMark(s) + " " + t.StatusStyle(s).Bold(true).Render(statement)
	var kept []string
	for _, f := range facts {
		if f != "" {
			kept = append(kept, f)
		}
	}
	if len(kept) > 0 {
		sep := " " + t.Glyph.Sep + " "
		out += t.Style.Muted.Render(sep + strings.Join(kept, sep))
	}
	return out
}

// ErrorBlock renders a structured error: what happened, then why and the
// next step hanging beneath it.
func ErrorBlock(t theme.Theme, e *fail.Error, width int) string {
	indent := markWidth(t) + 1
	var b strings.Builder
	// Backticks mark commands in prose; a headline is already emphasized,
	// so they are dropped there.
	b.WriteString(t.StatusMark(theme.StatusFail) + " " + t.Style.Danger.Bold(true).Render(strings.ReplaceAll(e.What, "`", "")))
	if e.Why != "" {
		// Wrap each source line on its own so a multi-line reason (a
		// list of problems, say) keeps its structure.
		for _, src := range strings.Split(e.Why, "\n") {
			lead := len(src) - len(strings.TrimLeft(src, " "))
			hang := 0
			if lead > 0 {
				hang = 2
			}
			text := strings.TrimLeft(src, " ")
			for i, l := range strings.Split(wrapInline(text, width-indent-lead-hang), "\n") {
				pre := indent + lead
				if i > 0 {
					pre += hang
				}
				b.WriteString("\n" + strings.Repeat(" ", pre) + Inline(t, l, t.Style.Muted))
			}
		}
	}
	if e.Next != "" {
		b.WriteString("\n" + Hint(t, e.Next, width))
	}
	return b.String()
}

// KV renders aligned key/value rows: dim keys, plain values.
func KV(t theme.Theme, rows [][2]string) string {
	w := 0
	for _, r := range rows {
		w = max(w, lipgloss.Width(r[0]))
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = t.Style.Muted.Render(pad(r[0], w)) + strings.Repeat(" ", t.Space.MD) + t.Style.Text.Render(r[1])
	}
	return strings.Join(out, "\n")
}

// Align is a column alignment.
type Align int

// Column alignments.
const (
	Left Align = iota
	Right
)

// Column describes one table column.
type Column struct {
	Title string
	Align Align
	// Flex columns absorb leftover width and truncate when space is short.
	Flex bool
}

// Table renders a borderless table that fits width. Cells may already be
// styled; widths are measured ignoring escape sequences.
func Table(t theme.Theme, cols []Column, rows [][]string, width int) string {
	n := len(cols)
	w := make([]int, n)
	for i, c := range cols {
		w[i] = lipgloss.Width(c.Title)
	}
	for _, r := range rows {
		for i := 0; i < n && i < len(r); i++ {
			w[i] = max(w[i], lipgloss.Width(r[i]))
		}
	}
	gap := t.Space.MD
	total := gap * (n - 1)
	for _, x := range w {
		total += x
	}
	// Shrink flex columns, widest first, until the table fits.
	for total > width {
		idx := -1
		for i, c := range cols {
			if c.Flex && w[i] > 8 && (idx < 0 || w[i] > w[idx]) {
				idx = i
			}
		}
		if idx < 0 {
			break
		}
		w[idx]--
		total--
	}

	cell := func(s string, i int) string {
		if lipgloss.Width(s) > w[i] {
			s = ansi.Truncate(s, w[i], t.Glyph.Ellipsis)
		}
		fill := strings.Repeat(" ", w[i]-lipgloss.Width(s))
		if cols[i].Align == Right {
			return fill + s
		}
		return s + fill
	}
	line := func(cells []string, style func(string) string) string {
		parts := make([]string, n)
		for i := range cols {
			v := ""
			if i < len(cells) {
				v = cells[i]
			}
			parts[i] = cell(style(v), i)
		}
		return strings.TrimRight(strings.Join(parts, strings.Repeat(" ", gap)), " ")
	}

	var b strings.Builder
	titles := make([]string, n)
	for i, c := range cols {
		titles[i] = c.Title
	}
	b.WriteString(line(titles, func(s string) string { return t.Style.Faint.Render(s) }))
	for _, r := range rows {
		b.WriteString("\n" + line(r, func(s string) string { return s }))
	}
	return b.String()
}

// Bar renders a proportional bar of the given cell width. It uses a line
// rather than blocks so that a column of bars stays light on the page.
func Bar(t theme.Theme, fraction float64, width int) string {
	fraction = min(max(fraction, 0), 1)
	full := int(fraction*float64(width) + 0.5)
	if fraction > 0 && full == 0 {
		full = 1
	}
	return t.Style.Brand.Render(strings.Repeat(t.Glyph.BarFull, full)) +
		t.Style.Faint.Render(strings.Repeat(t.Glyph.BarEmpty, width-full))
}

func pad(s string, w int) string {
	if d := w - lipgloss.Width(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

func wrap(s string, width int) string {
	if width < 20 {
		width = 20
	}
	return ansi.Wordwrap(s, width, "")
}

// Wrap word-wraps text to width.
func Wrap(s string, width int) string { return wrap(s, width) }
