// Package checklist runs a sequence of named checks and presents them as
// they complete: an inline live list with a spinner on an interactive
// terminal, plain lines when piped, and nothing at all in JSON mode (the
// caller serializes the returned outcomes). `taut auth` and `taut doctor` are
// both checklists.
//
// The presentation follows a few rules. A passing check is exactly one line.
// Labels in a group share a column so details align. The rows beneath a
// check (one per permission, say) are folded away unless one of them needs
// attention or the operator asked for everything. The run ends with a single
// verdict line carrying the counts and the elapsed time.
package checklist

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/bgfdev/taut/internal/tui/theme"
	"github.com/bgfdev/taut/internal/ui"
)

// Result is what a check reports.
type Result struct {
	Status theme.Status `json:"status"`
	// Detail is a short factual note shown beside the label.
	Detail string `json:"detail,omitempty"`
	// Next is the exact step to take when the status is a warning or a
	// failure.
	Next string `json:"next,omitempty"`
	// Code is a stable identifier for scripts, set on failures.
	Code string `json:"code,omitempty"`
}

// OK builds a passing result.
func OK(detail string) Result { return Result{Status: theme.StatusOK, Detail: detail} }

// Info builds a neutral, informational result.
func Info(detail string) Result { return Result{Status: theme.StatusInfo, Detail: detail} }

// Skip builds a result for a check that did not apply or could not run.
func Skip(detail string) Result { return Result{Status: theme.StatusSkip, Detail: detail} }

// Warn builds a warning with the step that resolves it.
func Warn(detail, next string) Result {
	return Result{Status: theme.StatusWarn, Detail: detail, Next: next}
}

// Fail builds a failure with a stable code and the step that resolves it.
func Fail(code, detail, next string) Result {
	return Result{Status: theme.StatusFail, Code: code, Detail: detail, Next: next}
}

// Check is one step. Run may block; it is given a context that is cancelled
// if the user interrupts.
type Check struct {
	Group string
	Label string
	Run   func(ctx context.Context) Result
	// Expand, when set, is called after Run and may return further
	// outcomes that belong beneath this one (for example one row per
	// permission). They are not run; they are already decided.
	Expand func() []Outcome
}

// Outcome is a completed check.
type Outcome struct {
	Group string `json:"group"`
	Label string `json:"label"`
	Result
	DurationMS int64 `json:"durationMs"`
	// Child marks an outcome that belongs beneath the check that produced it.
	Child bool `json:"child,omitempty"`
}

// Options adjust presentation.
type Options struct {
	// Verbose shows every row beneath a check, not only those that need
	// attention.
	Verbose bool
}

// Summary counts checks by status.
type Summary struct {
	OK       int `json:"ok"`
	Warnings int `json:"warnings"`
	Failed   int `json:"failed"`
	Skipped  int `json:"skipped"`
}

// Summarize tallies checks; the rows beneath a check are not counted.
func Summarize(out []Outcome) Summary {
	var s Summary
	for _, o := range out {
		if o.Child {
			continue
		}
		switch o.Status {
		case theme.StatusOK, theme.StatusInfo:
			s.OK++
		case theme.StatusWarn:
			s.Warnings++
		case theme.StatusFail:
			s.Failed++
		default:
			s.Skipped++
		}
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// Line renders the summary as the run's verdict.
func (s Summary) Line(t theme.Theme, elapsed time.Duration) string {
	status, statement := theme.StatusOK, "All checks passed"
	switch {
	case s.Failed > 0:
		status, statement = theme.StatusFail, plural(s.Failed, "check failed", "checks failed")
	case s.Warnings > 0:
		status, statement = theme.StatusWarn, "Passed with "+plural(s.Warnings, "warning", "warnings")
	}
	facts := []string{fmt.Sprintf("%d passed", s.OK)}
	if s.Failed > 0 && s.Warnings > 0 {
		facts = append(facts, plural(s.Warnings, "warning", "warnings"))
	}
	if s.Skipped > 0 {
		facts = append(facts, fmt.Sprintf("%d skipped", s.Skipped))
	}
	if elapsed > 0 {
		facts = append(facts, Elapsed(elapsed))
	}
	return ui.Verdict(t, status, statement, facts...)
}

// Elapsed formats a duration the way a person would say it.
func Elapsed(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%.1fs", max(d.Seconds(), 0.1))
	case d < time.Minute:
		return fmt.Sprintf("%.0fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}

// maxLabel caps the label column so one long label cannot push every detail
// off to the right.
const maxLabel = 26

// layout holds what is needed to render rows consistently before any check
// has run: the label column width of each group.
type layout struct {
	width  map[string]int
	opts   Options
	indent int
}

func newLayout(checks []Check, o Options) layout {
	l := layout{width: map[string]int{}, opts: o, indent: 2}
	for _, c := range checks {
		l.width[c.Group] = min(max(l.width[c.Group], lipgloss.Width(c.Label)), maxLabel)
	}
	return l
}

// render draws the outcomes of one check: its own row, then whichever of
// its child rows are worth showing.
func (l layout) render(t theme.Theme, outs []Outcome, width int) string {
	if len(outs) == 0 {
		return ""
	}
	var b strings.Builder
	parent := outs[0]
	inner := width - l.indent
	head := ui.CheckRow(t, parent.Status, parent.Label, l.width[parent.Group], parent.Detail, inner)
	b.WriteString(ui.Indent(head, l.indent))

	// The rows beneath a check come before its next step, so the reader
	// sees what is wrong and then what to do about it.
	var shown []Outcome
	for _, c := range outs[1:] {
		if l.opts.Verbose || c.Status == theme.StatusWarn || c.Status == theme.StatusFail {
			shown = append(shown, c)
		}
	}
	cw := 0
	for _, c := range shown {
		cw = min(max(cw, lipgloss.Width(c.Label)), maxLabel+6)
	}
	childIndent := l.indent + 4
	for _, c := range shown {
		b.WriteString("\n" + ui.Indent(row(t, c, cw, width-childIndent), childIndent))
	}
	if parent.Next != "" && (parent.Status == theme.StatusWarn || parent.Status == theme.StatusFail) {
		b.WriteString("\n" + ui.Indent(ui.Hint(t, parent.Next, inner), l.indent))
	}
	return b.String()
}

func row(t theme.Theme, o Outcome, labelWidth, width int) string {
	s := ui.CheckRow(t, o.Status, o.Label, labelWidth, o.Detail, width)
	if o.Next != "" && (o.Status == theme.StatusWarn || o.Status == theme.StatusFail) {
		s += "\n" + ui.Hint(t, o.Next, width)
	}
	return s
}

// Run executes checks in order and returns their outcomes.
func Run(ctx context.Context, io *ui.IO, command, subtitle string, checks []Check, opts ...Options) []Outcome {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	lay := newLayout(checks, o)
	start := time.Now()

	switch {
	case io.IsJSON():
		return runAll(ctx, checks, nil)
	case io.Interactive():
		m := newModel(ctx, io, command, subtitle, checks, lay)
		p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(io.In), tea.WithEnvironment(io.Env()))
		final, err := p.Run()
		if fm, ok := final.(model); ok && err == nil {
			io.SetDark(fm.theme.Dark)
			return fm.done
		}
		// The terminal could not be driven; fall back to plain output.
		fallthrough
	default:
		width := io.Width - ui.Margin
		io.Println()
		io.Println(ui.Pad(ui.Banner(io.Theme, command, subtitle)))
		group := ""
		out := runAll(ctx, checks, func(outs []Outcome) {
			if g := outs[0].Group; g != group {
				group = g
				io.Println()
				io.Println(ui.Pad(ui.Heading(io.Theme, group, width)))
			}
			io.Println(ui.Pad(lay.render(io.Theme, outs, width)))
		})
		io.Println()
		io.Println(ui.Pad(Summarize(out).Line(io.Theme, time.Since(start))))
		return out
	}
}

func runOne(ctx context.Context, c Check) []Outcome {
	start := time.Now()
	var r Result
	if ctx.Err() != nil {
		r = Skip("interrupted")
	} else {
		r = c.Run(ctx)
	}
	out := []Outcome{{Group: c.Group, Label: c.Label, Result: r, DurationMS: time.Since(start).Milliseconds()}}
	if c.Expand != nil {
		for _, o := range c.Expand() {
			if o.Group == "" {
				o.Group = c.Group
			}
			o.Child = true
			out = append(out, o)
		}
	}
	return out
}

func runAll(ctx context.Context, checks []Check, each func([]Outcome)) []Outcome {
	var out []Outcome
	for _, c := range checks {
		outs := runOne(ctx, c)
		out = append(out, outs...)
		if each != nil {
			each(outs)
		}
	}
	return out
}

// --- interactive model ------------------------------------------------------

type tickMsg struct{}
type doneMsg struct{ outcomes []Outcome }

type model struct {
	ctx      context.Context
	theme    theme.Theme
	command  string
	subtitle string
	checks   []Check
	lay      layout
	done     []Outcome
	next     int
	frame    int
	width    int
	finished bool
	locked   bool   // the theme was pinned with TAUT_THEME
	group    string // the last group heading printed
	started  time.Time
	stepAt   time.Time
}

func newModel(ctx context.Context, io *ui.IO, command, subtitle string, checks []Check, lay layout) model {
	now := time.Now()
	return model{
		ctx: ctx, theme: io.Theme, command: command, subtitle: subtitle, checks: checks, lay: lay,
		width: io.Width - ui.Margin, locked: io.ThemeLocked(), started: now, stepAt: now,
	}
}

func (m model) runNext() tea.Cmd {
	if m.next >= len(m.checks) {
		return nil
	}
	c := m.checks[m.next]
	ctx := m.ctx
	return func() tea.Msg { return doneMsg{runOne(ctx, c)} }
}

func tick() tea.Cmd {
	return tea.Tick(80*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{tea.Println("\n" + ui.Pad(ui.Banner(m.theme, m.command, m.subtitle))), m.runNext(), tick()}
	if !m.locked {
		cmds = append(cmds, tea.RequestBackgroundColor)
	}
	return tea.Batch(cmds...)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = min(msg.Width, theme.MaxContentWidth) - ui.Margin
	case tea.BackgroundColorMsg:
		if !m.locked && msg.IsDark() != m.theme.Dark {
			m.theme = theme.New(theme.Options{Dark: msg.IsDark(), Unicode: m.theme.Unicode})
		}
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	case tickMsg:
		if m.finished {
			return m, nil
		}
		m.frame++
		return m, tick()
	case doneMsg:
		// Finished checks are printed above the live area, into the
		// terminal's scrollback. Only the running check stays live, so
		// the list can be longer than the window without losing its top.
		var b strings.Builder
		if g := msg.outcomes[0].Group; g != m.group {
			m.group = g
			b.WriteString("\n" + ui.Pad(ui.Heading(m.theme, g, m.width)) + "\n")
		}
		b.WriteString(ui.Pad(m.lay.render(m.theme, msg.outcomes, m.width)))
		m.done = append(m.done, msg.outcomes...)
		m.next++
		m.stepAt = time.Now()
		printed := tea.Println(b.String())
		if m.next >= len(m.checks) {
			m.finished = true
			verdict := "\n" + ui.Pad(Summarize(m.done).Line(m.theme, time.Since(m.started)))
			return m, tea.Sequence(printed, tea.Println(verdict), tea.Quit)
		}
		return m, tea.Batch(printed, m.runNext())
	}
	return m, nil
}

// View draws only the check that is running; everything finished has
// already been printed.
func (m model) View() tea.View {
	if m.finished || m.next >= len(m.checks) {
		return tea.NewView("")
	}
	t := m.theme
	c := m.checks[m.next]
	var b strings.Builder
	if c.Group != m.group {
		b.WriteString("\n" + ui.Pad(ui.Heading(t, c.Group, m.width)) + "\n")
	}
	spin := t.Glyph.Spinner[m.frame%len(t.Glyph.Spinner)]
	if !t.Unicode {
		spin += " "
	}
	line := t.Style.Brand.Render(spin) + " " + t.Style.Text.Render(c.Label)
	// A check that takes a while says how long it has been, so a slow
	// network never looks like a hang.
	if d := time.Since(m.stepAt); d >= 2*time.Second {
		line += "  " + t.Style.Muted.Render(Elapsed(d))
	}
	b.WriteString(ui.Indent(line, ui.Margin+m.lay.indent))
	return tea.NewView(b.String())
}
