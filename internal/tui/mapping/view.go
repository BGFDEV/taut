package mapping

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/bgfdev/taut/internal/preflight"
	"github.com/bgfdev/taut/internal/tui/theme"
	"github.com/bgfdev/taut/internal/ui"
)

// ChannelView is one migrating channel as the map shows it. The JSON form
// is part of the `taut map --json` contract.
type ChannelView struct {
	SlackID    string `json:"slackId"`
	SlackName  string `json:"slackName"`
	Kind       string `json:"kind"`
	TargetName string `json:"targetName"`
	// Renamed is true when the Teams name differs from the Slack name,
	// whether the config file renames it or Teams' naming rules forced it.
	Renamed      bool   `json:"renamed"`
	RenameReason string `json:"renameReason,omitempty"`
	Archived     bool   `json:"archived,omitempty"`
	// Days is the number of days with messages; -1 when unknown.
	Days int `json:"days"`
	// Warnings are the things worth a second look: a name Teams forced a
	// change to, a channel claimed by several teams, an archived channel.
	Warnings []string `json:"warnings"`

	// Forced is true when Layout had to alter the name (Teams' rules or a
	// collision), as opposed to a rename written in the config file.
	Forced bool `json:"-"`
	// Claimed is the number of teams that select the channel; more than
	// one is a blocker.
	Claimed int `json:"-"`
	// Changed is true when the channel's place differs from the saved
	// config file.
	Changed bool `json:"-"`
}

// TeamView is one target team and its channels.
type TeamView struct {
	Name     string        `json:"name"`
	Owner    string        `json:"owner"`
	Exists   bool          `json:"exists,omitempty"`
	Channels []ChannelView `json:"channels"`
}

// SkippedView is a conversation that will not migrate.
type SkippedView struct {
	SlackID   string `json:"slackId,omitempty"`
	SlackName string `json:"slackName"`
	Kind      string `json:"kind"`
	Reason    string `json:"reason"`
	// Days is the number of days with messages; -1 when unknown.
	Days    int  `json:"days"`
	Changed bool `json:"-"`
}

// DMView is one direct message or group DM. It is shown, never edited: who
// is in a conversation decides whether it can go into a Teams chat.
type DMView struct {
	SlackID string `json:"slackId"`
	// Kind is "dm" or "mpim".
	Kind string `json:"kind"`
	// Label names the conversation by its participants.
	Label string `json:"label"`
	// Migrates is false when the conversation is left out; Skip is then
	// the reason's code and Reason says it in words.
	Migrates bool   `json:"migrates"`
	Skip     string `json:"skip,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// Status is what the row says: "migrates", or "skipped: " and the reason.
func (d DMView) Status() string {
	if d.Migrates {
		return "migrates"
	}
	return "skipped: " + d.Reason
}

// dmNote says what the mapping view cannot know about direct messages.
const dmNote = "Decided from the export alone. `taut preflight` also checks that each participant has a Microsoft 365 account."

// View is the whole mapping, ready to draw or to serialize.
type View struct {
	Teams   []TeamView    `json:"teams"`
	Skipped []SkippedView `json:"skipped"`
	// DirectMessages lists the export's direct messages when the config
	// file migrates them; otherwise they are one line of Skipped.
	DirectMessages []DMView            `json:"directMessages,omitempty"`
	Blockers       []preflight.Finding `json:"blockers"`
	Warnings       []preflight.Finding `json:"warnings"`
}

// Channels is the number of channels that migrate.
func (v *View) Channels() int {
	n := 0
	for _, t := range v.Teams {
		n += len(t.Channels)
	}
	return n
}

// View describes the current mapping. It only decorates what
// preflight.Layout computed; the result is cached until the next edit.
func (e *Editor) View() *View {
	if e.view != nil {
		return e.view
	}
	l := e.layout
	// Everything a view says about a channel is text from the Slack export
	// (names, IDs, and the Teams names and findings derived from them), so
	// the view is where it is made safe to draw: the tree, the plain
	// output and the JSON document are all rendered from it. The config file
	// and preflight.Layout keep the names exactly as the export has them,
	// because include/exclude patterns are matched against those.
	changed := map[string]bool{}
	for _, c := range e.unsaved {
		changed[c.SlackName] = true
	}
	daysOf := func(id string) int {
		if d, ok := e.days[id]; ok {
			return d
		}
		return -1
	}
	findings := func(in []preflight.Finding) []preflight.Finding {
		out := make([]preflight.Finding, len(in))
		for i, f := range in {
			out[i] = preflight.Finding{Code: f.Code, Message: ui.Sanitize(f.Message), Next: ui.Sanitize(f.Next)}
		}
		return out
	}

	v := &View{
		Teams: make([]TeamView, len(l.Teams)), Skipped: make([]SkippedView, 0, len(l.Skipped)),
		Blockers: findings(l.Blockers), Warnings: findings(l.Warnings),
	}
	for i, t := range l.Teams {
		spec := e.m.Target.Teams[i]
		tv := TeamView{Name: ui.Sanitize(t.Name), Owner: ui.Sanitize(t.Owner), Exists: t.Exists, Channels: make([]ChannelView, 0, len(t.Channels))}
		for _, c := range t.Channels {
			cv := ChannelView{
				SlackID: ui.Sanitize(c.SlackID), SlackName: ui.Sanitize(c.SlackName), Kind: ui.Sanitize(c.Kind), TargetName: ui.Sanitize(c.TargetName),
				Renamed: c.Renamed, Archived: c.Archived, Days: daysOf(c.SlackID), Warnings: []string{},
				Claimed: len(e.m.SelectingTeams(c.SlackName, c.Kind)), Changed: changed[ui.Sanitize(c.SlackName)],
			}
			if c.Renamed {
				cv.RenameReason = ui.Sanitize(c.RenameReason)
				// The config file asked for spec.TargetName; anything else is
				// Layout applying Teams' rules.
				cv.Forced = c.TargetName != spec.TargetName(c.SlackName)
			}
			if cv.Claimed > 1 {
				cv.Warnings = append(cv.Warnings, fmt.Sprintf("selected by %d teams", cv.Claimed))
			}
			if cv.Forced {
				cv.Warnings = append(cv.Warnings, "name changed: "+cv.RenameReason)
			}
			if c.Archived {
				cv.Warnings = append(cv.Warnings, "archived in Slack")
			}
			tv.Channels = append(tv.Channels, cv)
		}
		v.Teams[i] = tv
	}
	for _, s := range l.Skipped {
		sv := SkippedView{SlackName: ui.Sanitize(s.SlackName), Kind: ui.Sanitize(s.Kind), Reason: ui.Sanitize(s.Reason), Days: -1}
		sv.Changed = changed[sv.SlackName]
		if ch, ok := e.byName[s.SlackName]; ok && s.Kind != "dm" {
			sv.SlackID, sv.Days = ui.Sanitize(ch.ID), daysOf(ch.ID)
		}
		v.Skipped = append(v.Skipped, sv)
	}
	if e.m.Target.DirectMessages.Enabled {
		for _, d := range e.dms {
			v.DirectMessages = append(v.DirectMessages, DMView{
				SlackID: ui.Sanitize(d.SlackID), Kind: ui.Sanitize(d.Kind), Label: ui.Sanitize(d.Label),
				Migrates: d.Migrates(), Skip: d.Skip, Reason: d.Reason,
			})
		}
	}
	e.view = v
	return v
}

// --- shared row text ------------------------------------------------------------

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return preflight.Comma(int64(n)) + " " + many
}

func daysText(d int) string {
	switch {
	case d < 0:
		return ""
	case d == 0:
		return "empty"
	}
	return plural(d, "day", "days")
}

// label is "#slack" or "#slack → Teams name" when the two differ.
func (c ChannelView) label(t theme.Theme, bold bool) string {
	name := t.Style.Text
	if bold {
		name = t.Style.Title
	}
	s := name.Render("#" + c.SlackName)
	if c.TargetName != c.SlackName {
		target := t.Style.Text
		if c.Forced {
			target = t.Style.Warning
		}
		s += " " + t.Style.Faint.Render(t.Glyph.Arrow) + " " + target.Render(c.TargetName)
	}
	return s
}

// note is the trailing commentary for a channel row: its markers.
func (c ChannelView) note(t theme.Theme) string {
	var parts []string
	if c.Claimed > 1 {
		parts = append(parts, t.StatusMark(theme.StatusFail)+" "+t.Style.Text.Render(fmt.Sprintf("selected by %d teams", c.Claimed)))
	}
	if c.Forced {
		parts = append(parts, t.StatusMark(theme.StatusWarn)+" "+t.Style.Muted.Render(c.RenameReason))
	}
	if c.Kind == "private" {
		parts = append(parts, t.Style.Muted.Render("private"))
	}
	if c.Archived {
		parts = append(parts, t.Style.Muted.Render("archived"))
	}
	return strings.Join(parts, t.Style.Faint.Render(" "+t.Glyph.Sep+" "))
}

func (s SkippedView) label() string {
	if s.Kind == "dm" {
		return s.SlackName
	}
	return "#" + s.SlackName
}

func padTo(s string, w int) string {
	if d := w - lipgloss.Width(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

func clip(t theme.Theme, s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) > w {
		return ansi.Truncate(s, w, t.Glyph.Ellipsis)
	}
	return s
}

func teamHead(t theme.Theme, tv TeamView, shown int) string {
	head := t.Style.Title.Render(tv.Name) + "  " + t.Style.Muted.Render(plural(shown, "channel", "channels"))
	if tv.Owner != "" {
		head += "  " + t.Style.Muted.Render("owner "+tv.Owner)
	}
	if tv.Exists {
		head += "  " + t.Style.Warning.Render("already exists")
	}
	return head
}

// Summary is the one-line count of what migrates.
func (v *View) Summary() string {
	s := plural(v.Channels(), "channel", "channels") + " in " + plural(len(v.Teams), "team", "teams")
	if n := len(v.Skipped); n > 0 {
		s += fmt.Sprintf(", %d not migrating", n)
	}
	if n := len(v.DirectMessages); n > 0 {
		migrating := 0
		for _, d := range v.DirectMessages {
			if d.Migrates {
				migrating++
			}
		}
		s += fmt.Sprintf(", %d of %s into chats", migrating, plural(n, "direct message", "direct messages"))
	}
	return s
}

// Plain renders the mapping as an aligned tree for scrollback and pipes: no
// boxes, one margin, color only on status marks.
func (v *View) Plain(t theme.Theme, width int) string {
	labelW, daysW := 0, 0
	for _, tv := range v.Teams {
		for _, c := range tv.Channels {
			labelW = max(labelW, lipgloss.Width(c.label(t, false)))
			daysW = max(daysW, len(daysText(c.Days)))
		}
	}
	branchW := lipgloss.Width(t.Glyph.TreeMid) + 1
	gap := t.Space.MD
	// Long names give way before the commentary does.
	labelW = max(min(labelW, (width-branchW)*45/100), 12)

	var b strings.Builder
	for i, tv := range v.Teams {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(clip(t, teamHead(t, tv, len(tv.Channels)), width))
		for j, c := range tv.Channels {
			branch := t.Glyph.TreeMid
			if j == len(tv.Channels)-1 {
				branch = t.Glyph.TreeEnd
			}
			line := t.Style.Faint.Render(branch) + " " + padTo(clip(t, c.label(t, false), labelW), labelW)
			if daysW > 0 {
				d := daysText(c.Days)
				line += strings.Repeat(" ", gap+daysW-len(d)) + t.Style.Muted.Render(d)
			}
			if n := c.note(t); n != "" {
				line += strings.Repeat(" ", gap) + n
			}
			b.WriteString("\n" + clip(t, strings.TrimRight(line, " "), width))
		}
		if len(tv.Channels) == 0 {
			b.WriteString("\n" + t.Style.Muted.Render("no channels"))
		}
	}
	if len(v.Skipped) > 0 {
		if len(v.Teams) > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(ui.HeadingNote(t, "Not migrating", fmt.Sprint(len(v.Skipped))) + "\n")
		rows := make([][]string, 0, len(v.Skipped))
		for _, s := range v.Skipped {
			rows = append(rows, []string{t.Style.Muted.Render(s.label()), t.Style.Muted.Render(s.Kind), t.Style.Text.Render(s.Reason)})
		}
		b.WriteString(ui.Table(t, []ui.Column{{Title: "Conversation", Flex: true}, {Title: "Kind"}, {Title: "Why", Flex: true}}, rows, width))
	}
	if len(v.DirectMessages) > 0 {
		if len(v.Teams)+len(v.Skipped) > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(ui.HeadingNote(t, "Direct messages", fmt.Sprint(len(v.DirectMessages))) + "\n")
		rows := make([][]string, 0, len(v.DirectMessages))
		for _, d := range v.DirectMessages {
			status := t.Style.Text.Render(d.Status())
			if !d.Migrates {
				status = t.Style.Muted.Render(d.Status())
			}
			rows = append(rows, []string{t.Style.Text.Render(d.Label), t.Style.Muted.Render(d.Kind), status})
		}
		b.WriteString(ui.Table(t, []ui.Column{{Title: "Participants", Flex: true}, {Title: "Kind"}, {Title: "Into a Teams chat", Flex: true}}, rows, width))
		b.WriteString("\n" + ui.Hint(t, dmNote, width))
	}
	return b.String()
}

// Findings renders the blockers and warnings with their next steps, or ""
// when there are none.
func (v *View) Findings(t theme.Theme, width int) string {
	var b strings.Builder
	section := func(title string, status theme.Status, list []preflight.Finding) {
		if len(list) == 0 {
			return
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(t.Style.Heading.Render(title))
		for _, f := range list {
			b.WriteString("\n" + ui.Finding(t, status, f.Message, f.Next, width))
		}
	}
	section("Blockers", theme.StatusFail, v.Blockers)
	section("Warnings", theme.StatusWarn, v.Warnings)
	return b.String()
}

// ChangeList renders changes as a diff-style list: a sign, the channel, and
// what happened to it.
func ChangeList(t theme.Theme, changes []Change, width int) string {
	nameW := 0
	for _, c := range changes {
		nameW = max(nameW, lipgloss.Width(c.SlackName)+1)
	}
	nameW = min(nameW, max(width/2, 12))
	lines := make([]string, len(changes))
	for i, c := range changes {
		sign := "~"
		switch c.Action {
		case ActionExcluded:
			sign = "-"
		case ActionIncluded:
			sign = "+"
		case ActionMoved:
			sign = ">"
		}
		line := t.Style.Faint.Render(sign) + " " + padTo(clip(t, t.Style.Text.Render("#"+c.SlackName), nameW), nameW) +
			strings.Repeat(" ", t.Space.MD) + t.Style.Muted.Render(c.Describe(t.Glyph.Arrow))
		lines[i] = clip(t, line, width)
	}
	return strings.Join(lines, "\n")
}
