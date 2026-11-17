package cli

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/graph"
	"github.com/bgfdev/taut/internal/identity"
	"github.com/bgfdev/taut/internal/preflight"
	"github.com/bgfdev/taut/internal/slack/export"
	"github.com/bgfdev/taut/internal/tui/report"
	"github.com/bgfdev/taut/internal/tui/shell"
	"github.com/bgfdev/taut/internal/tui/theme"
	"github.com/bgfdev/taut/internal/ui"
)

// directory reads everything preflight needs from Microsoft Graph: the user
// index, the permission table and existing team names. Failures to read the
// optional parts are tolerated; a failure to read users is returned.
func directory(ctx context.Context, m *config.Config) (*identity.Directory, []graph.PermissionStatus, []string, error) {
	cred, _, err := entraCredential(m)
	if err != nil {
		return nil, nil, nil, err
	}
	sess, err := connectGraph(m, cred)
	if err != nil {
		return nil, nil, nil, err
	}
	tok, err := sess.Tokens.Token(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	var perms []graph.PermissionStatus
	if claims, err := graph.ParseClaims(tok); err == nil {
		private, dms := features(m)
		perms = graph.Compare(claims.Roles, graph.FeatureSet{PrivateChannels: private, DirectMessages: dms})
	}

	var users []identity.DirUser
	for u, err := range sess.Client.Users(ctx, graph.UserQuery{PageSize: 999}) {
		if err != nil {
			return nil, perms, nil, err
		}
		users = append(users, identity.DirUser{
			Guest: strings.EqualFold(u.UserType, "Guest"),
			ID:    u.ID, DisplayName: u.DisplayName, Mail: u.Mail, UPN: u.UserPrincipalName,
			Enabled: !u.Disabled(), Aliases: u.ProxyAddresses,
		})
	}

	// Team names are only used to warn about collisions, and reading them
	// needs a permission Taut merely recommends, so a failure is not fatal.
	var teams []string
	for t, err := range sess.Client.Teams(ctx) {
		if err != nil {
			teams = nil
			break
		}
		teams = append(teams, t.DisplayName)
	}
	return identity.NewDirectory(users), perms, teams, nil
}

// preflight runs the read-only analysis behind `taut preflight`. `taut migrate`
// runs the same analysis as its gate, so nothing is written that preflight would
// have refused.
func (a *App) preflight(ctx context.Context, m *config.Config, offline bool) (*preflight.Report, error) {
	io := a.IO
	ex, err := export.Open(m.ResolvePath(m.Source.Slack.Export))
	if err != nil {
		return nil, err
	}
	defer ex.Close()

	opts := preflight.Options{Config: m, Export: ex, Offline: offline}
	if m.Identity.Overrides != "" {
		ov, err := identity.LoadOverrides(m.ResolvePath(m.Identity.Overrides))
		if err != nil {
			return nil, err
		}
		opts.Overrides = ov
	}
	if !offline {
		io.Status("Reading the Entra directory…")
		opts.Directory, opts.Permissions, opts.ExistingTeams, opts.DirectoryErr = directory(ctx, m)
	}
	io.Status("Reading the Slack export…")
	return preflight.Run(ctx, opts)
}

func newPreflightCmd(app *App) *cobra.Command {
	var offline bool
	var failOn string
	cmd := &cobra.Command{
		Use:     "preflight",
		Short:   "See exactly what will and won't migrate",
		GroupID: groupPlan,
		Long: `Check the migration before anything is written. preflight reads the Slack
export and the Entra directory and reports what the migration would do:
channel and message counts, how each channel maps to Teams, who cannot be
mapped, which content Teams cannot import and how Taut handles it, and how
long the import will take.

preflight is strictly read-only. It exits 0 when the migration is ready, and 3
when something must be fixed first.`,
		Example: `  taut preflight
  taut preflight --json > preflight.json
  taut preflight --offline            # analyze the export without contacting Microsoft
  taut preflight --fail-on warnings   # stricter gate for CI`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			io := app.IO
			if failOn != "blockers" && failOn != "warnings" {
				return fail.Usagef("Use --fail-on blockers or --fail-on warnings.", "Unknown --fail-on value %q", failOn)
			}
			m, err := app.loadConfig()
			if err != nil {
				return err
			}
			rep, err := app.preflight(cmd.Context(), m, offline)
			if err != nil {
				return err
			}

			switch {
			case io.IsJSON():
				if err := io.JSON(rep); err != nil {
					return err
				}
			case io.Interactive():
				doc := preflightDocument(rep)
				if err := shell.Run(cmd.Context(), io, report.NewScreen(doc), shell.Options{Command: "preflight", Context: m.Metadata.Name}); err != nil {
					return err
				}
				// Leave the verdict in the scrollback after the full-screen
				// view closes.
				io.Println(verdict(io.Theme, rep, io.Width))
			default:
				io.Println(preflightDocument(rep).Plain(io.Theme, io.Width))
			}

			if rep.Verdict == preflight.VerdictBlocked {
				return fail.Exit(fail.ExitBlockers, "preflight found blockers")
			}
			if failOn == "warnings" && rep.Verdict == preflight.VerdictWarnings {
				return fail.Exit(fail.ExitBlockers, "preflight found warnings")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&offline, "offline", false, "analyze the export only; do not contact Microsoft")
	cmd.Flags().StringVar(&failOn, "fail-on", "blockers", "exit non-zero on: blockers, warnings")
	return cmd
}

// --- rendering ----------------------------------------------------------------

// displayReport returns a copy of r whose text is safe to draw. A report
// repeats what the Slack export and the Entra directory say (channel names
// and IDs, people's names and addresses, error text), and none of that is
// ours. The report itself is left as it is: the names in it are the ones
// the config file's patterns match and `--json` reports. Everything the
// full-screen and plain views draw goes through here instead.
func displayReport(r *preflight.Report) *preflight.Report {
	d := *r
	findings := func(in []preflight.Finding) []preflight.Finding {
		out := make([]preflight.Finding, len(in))
		for i, f := range in {
			out[i] = preflight.Finding{Code: f.Code, Message: ui.Sanitize(f.Message), Next: ui.Sanitize(f.Next)}
		}
		return out
	}
	d.Config = ui.Sanitize(r.Config)
	d.Blockers, d.Warnings, d.Notes = findings(r.Blockers), findings(r.Warnings), findings(r.Notes)
	d.Teams = make([]preflight.TeamPlan, len(r.Teams))
	for i, t := range r.Teams {
		t.Name, t.Owner = ui.Sanitize(t.Name), ui.Sanitize(t.Owner)
		channels := make([]preflight.ChannelPlan, len(t.Channels))
		for j, c := range t.Channels {
			c.SlackID, c.SlackName, c.Kind = ui.Sanitize(c.SlackID), ui.Sanitize(c.SlackName), ui.Sanitize(c.Kind)
			c.TargetName, c.RenameReason = ui.Sanitize(c.TargetName), ui.Sanitize(c.RenameReason)
			channels[j] = c
		}
		t.Channels = channels
		d.Teams[i] = t
	}
	d.Skipped = make([]preflight.SkippedChannel, len(r.Skipped))
	for i, s := range r.Skipped {
		d.Skipped[i] = preflight.SkippedChannel{SlackName: ui.Sanitize(s.SlackName), Kind: ui.Sanitize(s.Kind), Reason: ui.Sanitize(s.Reason)}
	}
	d.Identity.Account = ui.Sanitize(r.Identity.Account)
	d.Identity.UnmappedUsers = make([]preflight.UnmappedUser, len(r.Identity.UnmappedUsers))
	for i, u := range r.Identity.UnmappedUsers {
		u.SlackID, u.Name, u.Email = ui.Sanitize(u.SlackID), ui.Sanitize(u.Name), ui.Sanitize(u.Email)
		u.Reason, u.Detail = ui.Sanitize(u.Reason), ui.Sanitize(u.Detail)
		d.Identity.UnmappedUsers[i] = u
	}
	return &d
}

func preflightDocument(r *preflight.Report) report.Document {
	r = displayReport(r)
	doc := report.Document{Command: "preflight", Subtitle: r.Config}

	issues := len(r.Blockers) + len(r.Warnings)
	overview := report.Section{Title: "Overview", Render: func(t theme.Theme, w int) string { return renderOverview(t, r, w) }}
	if issues > 0 {
		overview.Count = fmt.Sprint(issues)
		overview.Status = theme.StatusWarn
		if len(r.Blockers) > 0 {
			overview.Status = theme.StatusFail
		}
	}

	people := report.Section{Title: "People", Render: func(t theme.Theme, w int) string { return renderPeople(t, r, w) }}
	if n := r.Identity.Unmapped; n > 0 && r.Identity.Checked {
		people.Count, people.Status = fmt.Sprint(n), theme.StatusWarn
	}

	doc.Sections = []report.Section{
		overview,
		{Title: "Plan", Count: fmt.Sprint(r.Totals.Channels), Status: theme.StatusSkip, Render: func(t theme.Theme, w int) string { return renderPlan(t, r, w) }},
		people,
	}
	// Direct messages have a section of their own when they are migrated:
	// what goes where, and what does not go at all.
	if dm := r.DirectMessages; dm != nil && dm.Enabled {
		sec := report.Section{Title: "Direct messages", Count: fmt.Sprint(dm.OneOnOne + dm.Group), Status: theme.StatusWarn,
			Render: func(t theme.Theme, w int) string { return renderDirectMessages(t, r, w) }}
		doc.Sections = append(doc.Sections, sec)
	}
	doc.Sections = append(doc.Sections, []report.Section{
		{Title: "Content", Count: fmt.Sprint(len(r.Content)), Status: theme.StatusSkip, Render: func(t theme.Theme, w int) string { return renderContent(t, r, w) }},
		{Title: "Estimate", Render: func(t theme.Theme, w int) string { return renderEstimate(t, r, w) }},
	}...)
	if len(r.Permissions) > 0 {
		sec := report.Section{Title: "Permissions", Render: func(t theme.Theme, w int) string { return renderPermissions(t, r, w) }}
		if n := len(graph.MissingRequired(r.Permissions)); n > 0 {
			sec.Count, sec.Status = fmt.Sprint(n), theme.StatusFail
		}
		doc.Sections = append(doc.Sections, sec)
	}
	doc.Footer = func(t theme.Theme, w int) string { return verdict(t, r, w) }
	return doc
}

func verdict(t theme.Theme, r *preflight.Report, width int) string {
	facts := []string{
		countOf(r.Totals.Channels, "channel", "channels"),
		countOf(r.Totals.Messages, "message", "messages"),
	}
	if r.Totals.Chats > 0 {
		facts = append(facts, countOf(r.Totals.Chats, "direct-message conversation", "direct-message conversations"))
	}
	facts = append(facts, "about "+duration(r.Estimate.ConservativeSeconds)+" to import")
	switch r.Verdict {
	case preflight.VerdictBlocked:
		return ui.Verdict(t, theme.StatusFail, "Not ready", countOf(len(r.Blockers), "blocker", "blockers")+" to fix") +
			"\n" + ui.Hint(t, "Fix the blockers above, then run `taut preflight` again.", width)
	case preflight.VerdictWarnings:
		return ui.Verdict(t, theme.StatusWarn, "Ready, with "+countOf(len(r.Warnings), "warning", "warnings"), facts...) +
			"\n\n" + ui.Next(t, "taut migrate", "run the migration")
	}
	return ui.Verdict(t, theme.StatusOK, "Ready to migrate", facts...) +
		"\n\n" + ui.Next(t, "taut migrate", "run the migration")
}

func renderOverview(t theme.Theme, r *preflight.Report, w int) string {
	var b strings.Builder
	tot := r.Totals
	span := "no messages"
	if tot.First != nil {
		span = tot.First.Format("2 Jan 2006") + " " + t.Glyph.Arrow + " " + tot.Last.Format("2 Jan 2006")
	}
	rows := [][2]string{
		{"Teams", comma(tot.Teams)},
		{"Channels", comma(tot.Channels) + skippedNote(r)},
		{"Messages", fmt.Sprintf("%s  (%s in %s)", comma(tot.Messages), countOf(tot.Replies, "reply", "replies"), countOf(tot.ThreadParents, "thread", "threads"))},
		{"Files", fmt.Sprintf("%s  (%s)", comma(tot.Files), bytesize(tot.FileBytes))},
	}
	if dm := r.DirectMessages; dm != nil && dm.Enabled {
		line := fmt.Sprintf("%s, %s", countOf(dm.OneOnOne+dm.Group, "conversation", "conversations"), countOf(dm.Messages, "message", "messages"))
		if dm.Skipped > 0 {
			line += fmt.Sprintf("  (%d not migrated)", dm.Skipped)
		}
		rows = append(rows, [2]string{"Direct messages", line})
	}
	rows = append(rows, [][2]string{
		{"Authors", comma(tot.Authors)},
		{"Date range", span},
		{"Import time", estimateRange(r.Estimate)},
		{"Load strategy", r.Strategy},
	}...)
	b.WriteString(ui.KV(t, rows))

	findings := func(title string, status theme.Status, list []preflight.Finding) {
		if len(list) == 0 {
			return
		}
		b.WriteString("\n\n" + t.Style.Heading.Render(title))
		for _, f := range list {
			b.WriteString("\n" + ui.Finding(t, status, f.Message, f.Next, w))
		}
	}
	findings("Blockers", theme.StatusFail, r.Blockers)
	findings("Warnings", theme.StatusWarn, r.Warnings)
	if len(r.Blockers)+len(r.Warnings) == 0 {
		b.WriteString("\n\n" + ui.CheckLine(t, theme.StatusOK, "No blockers or warnings", "", w))
	}
	// Consequences of the config file worth knowing before starting;
	// nothing to fix.
	findings("Notes", theme.StatusInfo, r.Notes)
	return b.String()
}

func skippedNote(r *preflight.Report) string {
	if len(r.Skipped) == 0 {
		return ""
	}
	return fmt.Sprintf("  (%d not migrated)", len(r.Skipped))
}

func renderPlan(t theme.Theme, r *preflight.Report, w int) string {
	var b strings.Builder
	for i, team := range r.Teams {
		if i > 0 {
			b.WriteString("\n\n")
		}
		head := t.Style.Title.Render(team.Name) + "  " + t.Style.Muted.Render("owner "+team.Owner)
		if team.Exists {
			head += "  " + t.Style.Warning.Render("already exists")
		}
		b.WriteString(head + "\n")

		rows := make([][]string, 0, len(team.Channels))
		for j, c := range team.Channels {
			branch := t.Glyph.TreeMid
			if j == len(team.Channels)-1 {
				branch = t.Glyph.TreeEnd
			}
			target := t.Style.Text.Render(c.TargetName)
			if c.Renamed {
				target = t.Style.Warning.Render(c.TargetName)
			}
			note := ""
			switch {
			case c.Renamed:
				note = c.RenameReason
			case c.Archived:
				note = "archived in Slack"
			case c.Kind == "private":
				note = "private"
			}
			rows = append(rows, []string{
				t.Style.Faint.Render(branch) + " " + t.Style.Muted.Render("#"+c.SlackName),
				t.Style.Faint.Render(t.Glyph.Arrow) + " " + target,
				t.Style.Text.Render(comma(c.Messages)),
				t.Style.Text.Render(comma(c.Files)),
				t.Style.Muted.Render(note),
			})
		}
		b.WriteString(ui.Table(t, []ui.Column{
			{Title: "Slack channel", Flex: true}, {Title: "Teams channel", Flex: true},
			{Title: "Messages", Align: ui.Right}, {Title: "Files", Align: ui.Right}, {Title: "Note", Flex: true},
		}, rows, w))
	}
	if len(r.Skipped) > 0 {
		b.WriteString("\n\n" + t.Style.Heading.Render("Not migrated") + "\n")
		rows := make([][]string, 0, len(r.Skipped))
		for _, s := range r.Skipped {
			name := s.SlackName
			if s.Kind != "dm" {
				name = "#" + name
			}
			rows = append(rows, []string{t.Style.Muted.Render(name), t.Style.Muted.Render(s.Kind), t.Style.Text.Render(s.Reason)})
		}
		b.WriteString(ui.Table(t, []ui.Column{{Title: "Conversation", Flex: true}, {Title: "Kind"}, {Title: "Why", Flex: true}}, rows, w))
	}
	return b.String()
}

// renderDirectMessages says which direct messages go into Teams chats and
// which do not. It gives counts only: who talks to whom is not something a
// report should spell out, and `--json` has the full list for whoever
// needs it.
func renderDirectMessages(t theme.Theme, r *preflight.Report, w int) string {
	dm := r.DirectMessages
	var b strings.Builder
	b.WriteString(ui.KV(t, [][2]string{
		{"One-on-one", countOf(dm.OneOnOne, "conversation", "conversations")},
		{"Group", countOf(dm.Group, "conversation", "conversations")},
		{"Messages", fmt.Sprintf("%s  (%s, imported as ordinary messages)", comma(dm.Messages), countOf(dm.Replies, "thread reply", "thread replies"))},
		{"Files", fmt.Sprintf("%s  (not moved)", comma(dm.Files))},
		{"Not migrated", countOf(dm.Skipped, "conversation", "conversations")},
	}))
	if !dm.IdentityChecked {
		b.WriteString("\n\n" + ui.CheckLine(t, theme.StatusWarn, "People were not matched against Entra, so nobody was ruled out for having no Microsoft 365 account", "", w))
	}

	b.WriteString("\n\n" + t.Style.Heading.Render("What happens"))
	for _, line := range []string{
		"These are private conversations. Each is imported into a Teams chat with exactly its Slack participants.",
		"A chat is visible to its participants while the import runs, with a notice that a migration is in progress.",
		"One-on-one history goes into the chat the two people already have in Teams, if they have one.",
		"Files are not moved; each message that had one says so. Thread replies become ordinary messages, in time order.",
		"A conversation is imported with all of its participants as themselves, or not at all.",
	} {
		b.WriteString("\n" + ui.Finding(t, theme.StatusInfo, line, "", w))
	}

	if len(dm.SkippedByReason) > 0 {
		b.WriteString("\n\n" + t.Style.Heading.Render("Not migrated") + "\n")
		rows := make([][]string, 0, len(dm.SkippedByReason))
		for _, s := range dm.SkippedByReason {
			rows = append(rows, []string{t.Style.Text.Render(comma(s.Count)), t.Style.Text.Render(s.Reason)})
		}
		b.WriteString(ui.Table(t, []ui.Column{{Title: "Conversations", Align: ui.Right}, {Title: "Why", Flex: true}}, rows, w))
	}
	return b.String()
}

func renderPeople(t theme.Theme, r *preflight.Report, w int) string {
	id := r.Identity
	var b strings.Builder
	if !id.Checked {
		b.WriteString(ui.CheckLine(t, theme.StatusWarn, "People were not matched against Entra", "", w) + "\n\n")
	}
	policy := map[string]string{
		config.PolicyFormerMember:   "attribute to " + id.Account + ", original name kept in the message",
		config.PolicyServiceAccount: "attribute to the service account " + id.Account,
		config.PolicyBlock:          "block the migration until everyone is mapped",
	}[id.Policy]
	rows := [][2]string{
		{"Slack users", comma(id.Users)},
		{"Matched by email", comma(id.MappedByEmail)},
		{"Matched by override", comma(id.MappedByOverride)},
		{"Unmapped", fmt.Sprintf("%s  (%s affected)", comma(id.Unmapped), countOf(id.UnmappedMessages, "message", "messages"))},
		{"Policy", policy},
	}
	b.WriteString(ui.KV(t, rows))

	if len(id.UnmappedUsers) > 0 {
		b.WriteString("\n\n" + t.Style.Heading.Render("Unmapped") + "\n")
		trows := make([][]string, 0, len(id.UnmappedUsers))
		for _, u := range id.UnmappedUsers {
			email := u.Email
			if email == "" {
				email = t.Glyph.Skip
			}
			trows = append(trows, []string{
				t.Style.Text.Render(u.Name), t.Style.Muted.Render(email),
				t.Style.Text.Render(comma(u.Messages)), t.Style.Muted.Render(u.Detail),
			})
		}
		b.WriteString(ui.Table(t, []ui.Column{
			{Title: "Name", Flex: true}, {Title: "Slack email", Flex: true},
			{Title: "Messages", Align: ui.Right}, {Title: "Why", Flex: true},
		}, trows, w))
		b.WriteString("\n\n" + ui.Hint(t, "To map someone by hand, add a row to the override CSV (identity.overrides): slack_user_id,slack_email,entra", w))
	}
	return b.String()
}

func renderContent(t theme.Theme, r *preflight.Report, w int) string {
	if len(r.Content) == 0 {
		return ui.CheckLine(t, theme.StatusOK, "Everything in the selected channels migrates as-is", "", w)
	}
	var b strings.Builder
	b.WriteString(t.Style.Muted.Render(ui.Wrap("Microsoft Graph cannot import everything Slack stores. This is what Taut found and what it will do with each.", w)) + "\n\n")
	width := 0
	for _, c := range r.Content {
		width = max(width, len(comma(c.Count)))
	}
	for i, c := range r.Content {
		if i > 0 {
			b.WriteString("\n")
		}
		status := theme.StatusInfo
		if c.Lossy {
			status = theme.StatusWarn
		}
		n := comma(c.Count)
		b.WriteString(t.StatusMark(status) + " " + t.Style.Title.Render(strings.Repeat(" ", width-len(n))+n) + "  " + t.Style.Text.Render(c.Label) + "\n")
		indent := strings.Repeat(" ", width+4)
		if !t.Unicode {
			indent += " "
		}
		for _, l := range strings.Split(ui.Wrap(c.Handling, w-len(indent)), "\n") {
			b.WriteString(indent + t.Style.Muted.Render(l) + "\n")
		}
	}
	b.WriteString("\n" + t.StatusMark(theme.StatusInfo) + " " + t.Style.Muted.Render("transformed") + "   " + t.StatusMark(theme.StatusWarn) + " " + t.Style.Muted.Render("information is lost"))
	return b.String()
}

func renderEstimate(t theme.Theme, r *preflight.Report, w int) string {
	e := r.Estimate
	var b strings.Builder
	b.WriteString(ui.KV(t, [][2]string{
		{"Posts to import", comma(e.Imports)},
		{"At 5 requests/s per channel", duration(e.OptimisticSeconds)},
		{"At 1 request/s per channel", duration(e.ConservativeSeconds)},
		{"Limited by", e.BoundBy},
		{"Files to stage", bytesize(e.StagingBytes)},
	}))

	// The channels that dominate the run, so the operator knows where the
	// time goes.
	type row struct {
		name string
		n    int
	}
	var top []row
	for _, team := range r.Teams {
		for _, c := range team.Channels {
			top = append(top, row{c.SlackName, c.Messages})
		}
	}
	for i := range top {
		for j := i + 1; j < len(top); j++ {
			if top[j].n > top[i].n {
				top[i], top[j] = top[j], top[i]
			}
		}
	}
	if len(top) > 8 {
		top = top[:8]
	}
	if len(top) > 0 && top[0].n > 0 {
		b.WriteString("\n\n" + t.Style.Heading.Render("Largest channels") + "\n")
		nameW := 0
		for _, x := range top {
			nameW = max(nameW, len(x.name)+1)
		}
		nameW = min(nameW, 28)
		barW := max(min(w-nameW-14, 40), 8)
		for _, x := range top {
			name := "#" + x.name
			if len(name) > nameW {
				name = name[:nameW-1] + t.Glyph.Ellipsis
			}
			b.WriteString(t.Style.Muted.Render(fmt.Sprintf("%-*s", nameW, name)) + "  " +
				ui.Bar(t, float64(x.n)/float64(top[0].n), barW) + "  " + t.Style.Text.Render(comma(x.n)) + "\n")
		}
	}
	b.WriteString("\n")
	for _, n := range e.Notes {
		for i, l := range strings.Split(ui.Wrap(n, w-2), "\n") {
			prefix := "  "
			if i == 0 {
				prefix = t.Style.Faint.Render(t.Glyph.Bullet) + " "
			}
			b.WriteString(prefix + t.Style.Muted.Render(l) + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderPermissions(t theme.Theme, r *preflight.Report, w int) string {
	var lines []string
	lw := 0
	for _, p := range r.Permissions {
		lw = max(lw, len(p.Name))
	}
	for _, p := range r.Permissions {
		switch p.State {
		case graph.PermissionGranted:
			lines = append(lines, ui.CheckRow(t, theme.StatusOK, p.Name, lw, p.Purpose, w))
		case graph.PermissionMissingRequired:
			lines = append(lines, ui.CheckRow(t, theme.StatusFail, p.Name, lw, "required: "+p.Purpose, w))
		case graph.PermissionMissingOptional:
			lines = append(lines, ui.CheckRow(t, theme.StatusWarn, p.Name, lw, "recommended: "+p.Purpose, w))
		default:
			lines = append(lines, ui.CheckRow(t, theme.StatusSkip, p.Name, lw, "not needed for this config file", w))
		}
	}
	return strings.Join(lines, "\n")
}

// --- formatting helpers -------------------------------------------------------

func comma(n int) string { return preflight.Comma(int64(n)) }

func countOf(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return comma(n) + " " + many
}

func bytesize(n int64) string {
	const unit = 1000.0
	if n < 1000 {
		return fmt.Sprintf("%d B", n)
	}
	v := float64(n)
	for _, u := range []string{"KB", "MB", "GB", "TB"} {
		v /= unit
		if v < unit {
			if v >= 100 {
				return fmt.Sprintf("%.0f %s", v, u)
			}
			return fmt.Sprintf("%.1f %s", v, u)
		}
	}
	return fmt.Sprintf("%.1f PB", v/unit)
}

func duration(seconds float64) string {
	d := time.Duration(math.Ceil(seconds)) * time.Second
	switch {
	case d <= 0:
		return "none"
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(math.Ceil(d.Minutes())))
	case d < 48*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return fmt.Sprintf("%d h", h)
		}
		return fmt.Sprintf("%d h %d min", h, m)
	}
	return fmt.Sprintf("%.1f days", d.Hours()/24)
}

func estimateRange(e preflight.Estimate) string {
	lo, hi := duration(e.OptimisticSeconds), duration(e.ConservativeSeconds)
	if lo == hi {
		return hi
	}
	return lo + " to " + hi
}
