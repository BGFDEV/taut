package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/preflight"
	"github.com/bgfdev/taut/internal/slack/export"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/tui/checklist"
	"github.com/bgfdev/taut/internal/tui/forms"
	"github.com/bgfdev/taut/internal/tui/theme"
	"github.com/bgfdev/taut/internal/ui"
)

// controllerClient builds a client for the config file's control plane. The API
// token, if the control plane requires one, comes from TAUT_CONTROLLER_TOKEN.
func (a *App) controllerClient(m *config.Config) *controller.Client {
	return controller.NewClient(m.Controller.URL, a.Getenv("TAUT_CONTROLLER_TOKEN"))
}

// stageLabels name the stages for people, in pipeline order. Present
// participle while running, past participle when done.
var stageLabels = []struct{ stage, doing, done string }{
	{store.StagePlan, "Planning", "Planned"},
	{store.StageExtract, "Reading the export", "Read the export"},
	{store.StageTransform, "Converting messages", "Converted messages"},
	{store.StageTeam, "Creating teams", "Created teams"},
	{store.StageFiles, "Moving files", "Moved files"},
	{store.StageLoad, "Importing into Teams", "Imported into Teams"},
}

// finalizeLabels name the finalizing stages that `taut finalize` adds.
var finalizeLabels = []struct{ stage, doing, done string }{
	{store.StageFinalize, "Opening channels", "Opened channels"},
	{store.StageMembers, "Adding members", "Added members"},
}

// viewLabels returns the stages to show for a run: the import while it is
// importing, the finalizing stages while it is being finalized, and both once
// it is over.
func viewLabels(v controller.RunView) []struct{ stage, doing, done string } {
	labels := runLabels(v)
	if _, releasing := stageOf(v.Progress, store.StageRelease); releasing {
		// Only a run that was abandoned with chats in migration mode has
		// this stage.
		labels = append(append([]struct{ stage, doing, done string }{}, labels...), releaseLabel)
	}
	return labels
}

// releaseLabel names the stage that takes the chats of an abandoned run out
// of migration mode.
var releaseLabel = struct{ stage, doing, done string }{store.StageRelease, "Ending migration mode on chats", "Ended migration mode on chats"}

func runLabels(v controller.RunView) []struct{ stage, doing, done string } {
	if _, finalized := stageOf(v.Progress, store.StageFinalize); !finalized && v.Status != store.RunFinalizing && v.Status != store.RunFinalized {
		return stageLabels
	}
	if v.Status == store.RunFinalizing {
		return finalizeLabels
	}
	return append(append([]struct{ stage, doing, done string }{}, stageLabels...), finalizeLabels...)
}

// verbOf names the command a run's current phase belongs to.
func verbOf(v controller.RunView) string {
	if v.Status == store.RunFinalizing || v.Status == store.RunFinalized {
		return "finalize"
	}
	return "migrate"
}

func stageOf(p *store.Progress, name string) (store.StageProgress, bool) {
	if p == nil {
		return store.StageProgress{}, false
	}
	for _, s := range p.Stages {
		if s.Stage == name {
			return s, true
		}
	}
	return store.StageProgress{}, false
}

// migrateView renders a run's progress. It is a pure function of the run, so
// the live view, `taut status` and the tests all draw the same thing.
func migrateView(t theme.Theme, v controller.RunView, width int, spinner string, rate float64) string {
	return stagesView(t, v, viewLabels(v), width, spinner, rate)
}

// stagesView is migrateView for a chosen set of stages. The live view fixes
// the set when it starts, so finalizing shows the finalizing stages to the
// end rather than the whole history once the run is finalized.
func stagesView(t theme.Theme, v controller.RunView, labels []struct{ stage, doing, done string }, width int, spinner string, rate float64) string {
	var b strings.Builder
	p := v.Progress
	labelW := 0
	for _, l := range labels {
		labelW = max(labelW, len(l.doing))
	}

	if v.Status == store.RunAwaitingExport {
		b.WriteString(t.Style.Brand.Render(spinner) + " " + t.Style.Text.Render("Waiting for the export upload"))
		return b.String()
	}

	for _, l := range labels {
		s, ok := stageOf(p, l.stage)
		var mark, label, detail string
		switch {
		case !ok && l.stage == store.StagePlan && v.Status == store.RunQueued:
			mark, label = t.Style.Brand.Render(spinner), l.doing
		case !ok || s.Lanes == 0:
			mark, label = t.StatusMark(theme.StatusSkip), l.doing
			detail = "waiting"
		case s.Failed > 0:
			mark, label = t.StatusMark(theme.StatusFail), l.doing
			detail = fmt.Sprintf("%s of %s, %s failed", comma(s.BatchesDone), comma(s.Batches), countOf(s.Failed, "lane", "lanes"))
		case s.Done == s.Lanes:
			mark, label = t.StatusMark(theme.StatusOK), l.done
			detail = stageDetail(l.stage, s, p)
		default:
			mark = t.StatusMark(theme.StatusSkip)
			if s.Leased > 0 || s.BatchesDone > 0 {
				mark = t.Style.Brand.Render(spinner)
			}
			label = l.doing
			detail = stageDetail(l.stage, s, p)
			if l.stage == store.StageLoad && rate >= 0.5 {
				detail += fmt.Sprintf(" %s %.0f/s", t.Glyph.Sep, rate)
			}
		}
		if !t.Unicode && len(spinner) == 1 && mark == t.Style.Brand.Render(spinner) {
			mark += " "
		}
		b.WriteString(mark + " " + t.Style.Text.Render(fmt.Sprintf("%-*s", labelW, label)) + "  " + t.Style.Muted.Render(detail) + "\n")
	}

	// The channels being imported right now, busiest first, so the reader
	// can see where the time is going.
	if p != nil && v.Status == store.RunRunning {
		var active []store.ChannelProgress
		for _, c := range p.Channels {
			if c.Posts > 0 && c.Posted+c.Failed+c.Held < c.Posts {
				active = append(active, c)
			}
		}
		sort.SliceStable(active, func(i, j int) bool {
			if active[i].Active != active[j].Active {
				return active[i].Active
			}
			return active[i].Posts > active[j].Posts
		})
		more := 0
		if len(active) > 6 {
			more, active = len(active)-6, active[:6]
		}
		if len(active) > 0 {
			b.WriteString("\n")
			nameW := 0
			for _, c := range active {
				nameW = min(max(nameW, ansi.StringWidth(conversationName(c))), 24)
			}
			barW := min(max(width-nameW-26, 8), 28)
			for _, c := range active {
				// A direct message is shown by its participants' names,
				// which need not be ASCII: measure by width on screen.
				name := conversationName(c)
				if ansi.StringWidth(name) > nameW {
					name = ansi.Truncate(name, nameW, t.Glyph.Ellipsis)
				}
				name += strings.Repeat(" ", max(0, nameW-ansi.StringWidth(name)))
				done := c.Posted + c.Failed + c.Held
				b.WriteString("  " + t.Style.Muted.Render(name) + "  " +
					ui.Bar(t, float64(done)/float64(c.Posts), barW) + "  " +
					t.Style.Muted.Render(fmt.Sprintf("%s of %s", comma(done), comma(c.Posts))) + "\n")
			}
			if more > 0 {
				b.WriteString("  " + t.Style.Faint.Render(fmt.Sprintf("%s %d more", t.Glyph.Ellipsis, more)) + "\n")
			}
		}
	}

	if p != nil && len(p.Problems) > 0 {
		b.WriteString("\n")
		shown := p.Problems
		if len(shown) > 3 {
			shown = shown[:3]
		}
		for _, pr := range shown {
			status, what := theme.StatusWarn, fmt.Sprintf("%s is retrying (attempt %d)", pr.LaneID, pr.Attempts+1)
			if pr.Status == string(store.LaneFailed) {
				status, what = theme.StatusFail, pr.LaneID+" failed"
			}
			b.WriteString(ui.Finding(t, status, what+": "+pr.Error, "", width) + "\n")
		}
		if n := len(p.Problems) - len(shown); n > 0 {
			b.WriteString(t.Style.Faint.Render(fmt.Sprintf("  %s %d more problems; `taut status` lists them", t.Glyph.Ellipsis, n)) + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func stageDetail(stage string, s store.StageProgress, p *store.Progress) string {
	switch stage {
	case store.StagePlan:
		return conversations(progressCounts(p))
	case store.StageTeam:
		return fmt.Sprintf("%s of %s", comma(s.Done), comma(s.Lanes))
	case store.StageFinalize:
		if _, chats := progressCounts(p); chats > 0 {
			return fmt.Sprintf("%s of %s channels and chats", comma(s.Done), comma(s.Lanes))
		}
		return fmt.Sprintf("%s of %s channels", comma(s.Done), comma(s.Lanes))
	case store.StageMembers:
		return fmt.Sprintf("%s of %s teams", comma(s.Done), comma(s.Lanes))
	case store.StageRelease:
		return fmt.Sprintf("%s of %s chats", comma(s.Done), comma(s.Lanes))
	case store.StageFiles:
		done := p.FilesUploaded + p.FilesSkipped + p.FilesFailed
		detail := fmt.Sprintf("%s of %s files", comma(done), comma(p.Files))
		if p.FileBytes > 0 {
			detail += fmt.Sprintf(", %s of %s", bytesize(p.FileBytesDone), bytesize(p.FileBytes))
		}
		return detail
	case store.StageLoad:
		done := p.Posted + p.Failed + p.Held
		return fmt.Sprintf("%s of %s posts", comma(done), comma(p.Posts))
	}
	return fmt.Sprintf("%s of %s days", comma(s.BatchesDone), comma(s.Batches))
}

// migrateSummary renders the closing lines of a run that has stopped.
func migrateSummary(t theme.Theme, v controller.RunView, elapsed time.Duration, logOK *bool, width int) string {
	var b strings.Builder
	p := v.Progress
	facts := []string{}
	if v.Pass > 1 {
		facts = append(facts, fmt.Sprintf("pass %d", v.Pass))
	}
	if p != nil {
		facts = append(facts, countOf(p.Posted, "post", "posts"), conversations(progressCounts(p)))
	}
	if elapsed > 0 {
		facts = append(facts, checklist.Elapsed(elapsed))
	}
	switch v.Status {
	case store.RunCompleted:
		status, statement := theme.StatusOK, "Import complete"
		if p != nil && p.Posts-p.Posted-p.Failed-p.Held > 0 {
			// Every lane finished, yet some posts were never settled. That
			// should be impossible; say so rather than report success.
			status, statement = theme.StatusFail, "Import finished, but "+countOf(p.Posts-p.Posted-p.Failed-p.Held, "post was", "posts were")+" never imported"
		} else if p != nil && p.Failed+p.Held > 0 {
			status, statement = theme.StatusWarn, "Import complete, with "+countOf(p.Failed+p.Held, "post", "posts")+" not imported"
		}
		if n := skippedChats(p); n > 0 && status != theme.StatusFail {
			status, statement = theme.StatusWarn, statement+" and "+countOf(n, "direct-message conversation", "direct-message conversations")+" skipped"
		}
		b.WriteString(ui.Verdict(t, status, statement, facts...))
	case store.RunFailed:
		b.WriteString(ui.Verdict(t, theme.StatusFail, "Import stopped", facts...))
	case store.RunCancelled:
		b.WriteString(ui.Verdict(t, theme.StatusSkip, "Run cancelled", facts...))
	case store.RunFinalizing:
		b.WriteString(ui.Verdict(t, theme.StatusInfo, "Finalizing", facts...))
	case store.RunFinalized:
		b.WriteString(ui.Verdict(t, theme.StatusOK, "Migration finalized", facts...))
	default:
		b.WriteString(ui.Verdict(t, theme.StatusInfo, "Import in progress", facts...))
	}

	if p != nil && (p.Posts > 0 || p.Carried > 0) {
		rows := [][2]string{{"Imported", comma(p.Posted - p.Adopted)}}
		if p.Carried > 0 {
			rows = append(rows, [2]string{"Earlier passes", comma(p.Carried) + "  already in Teams; not imported again"})
		}
		if p.Adopted > 0 {
			rows = append(rows, [2]string{"Recovered", comma(p.Adopted) + "  already in Teams after an interrupted attempt; not duplicated"})
		}
		if p.Failed > 0 {
			rows = append(rows, [2]string{"Rejected", comma(p.Failed) + "  refused by Microsoft Graph"})
		}
		if p.Held > 0 {
			rows = append(rows, [2]string{"Held", comma(p.Held) + "  replies whose thread could not be imported"})
		}
		if n := skippedChats(p); n > 0 {
			// Their posts are among the rejected ones above.
			rows = append(rows, [2]string{"Skipped", countOf(n, "direct-message conversation", "direct-message conversations") +
				"  Microsoft refused a chat for them; `taut audit --op chat.skip` says why"})
		}
		if pending := p.Posts - p.Posted - p.Failed - p.Held; pending > 0 {
			rows = append(rows, [2]string{"Remaining", comma(pending)})
		}
		if p.Files > 0 {
			files := fmt.Sprintf("%s moved (%s)", comma(p.FilesUploaded), bytesize(p.FileBytesDone))
			if p.FilesSkipped > 0 {
				where := "hosted outside Slack or unavailable"
				if _, chats := progressCounts(p); chats > 0 {
					where += ", or in a direct message"
				}
				files += fmt.Sprintf(", %s left where they are (%s)", comma(p.FilesSkipped), where)
			}
			if p.FilesFailed > 0 {
				files += fmt.Sprintf(", %s could not be moved", comma(p.FilesFailed))
			}
			rows = append(rows, [2]string{"Files", files})
		}
		if logOK != nil {
			state := "verified  every write is in the audit trail, hash chain intact"
			if !*logOK {
				state = "FAILED verification; run `taut audit` to investigate"
			}
			rows = append(rows, [2]string{"Audit log", state})
		}
		b.WriteString("\n" + ui.Indent(ui.KV(t, rows), 2))
	}

	if p != nil {
		for _, pr := range p.Problems {
			if pr.Status == string(store.LaneFailed) {
				b.WriteString("\n" + ui.Finding(t, theme.StatusFail, pr.LaneID+": "+pr.Error, "", width))
			}
		}
	}
	switch v.Status {
	case store.RunCompleted:
		next := "open the channels and add their members"
		if _, chats := progressCounts(p); chats > 0 {
			next += "; end migration mode on the chats"
		}
		b.WriteString("\n\n" + ui.Next(t, "taut finalize", next))
	case store.RunFailed:
		b.WriteString("\n" + ui.Hint(t, "Fix the cause above, then run `taut migrate` again. It resumes where it stopped and never imports a message twice.", width))
	}
	return b.String()
}

// passLine says which pass of its migration a run is, or "" for a first
// import that nothing follows.
func passLine(v controller.RunView) string {
	if v.Pass <= 1 {
		return ""
	}
	line := fmt.Sprintf("%d, incremental; follows run %s", v.Pass, v.BaseRunID)
	if p := v.Progress; p != nil && p.Carried > 0 {
		line += fmt.Sprintf(" (%s already in Teams)", countOf(p.Carried, "post", "posts"))
	}
	return line
}

// passesTable lists the passes of a migration, oldest first, or returns ""
// for a migration with one.
func passesTable(t theme.Theme, v controller.RunView, width int) string {
	if len(v.Passes) < 2 {
		return ""
	}
	rows := make([][]string, 0, len(v.Passes))
	for _, p := range v.Passes {
		finished := t.Glyph.Skip
		if p.FinishedAt != nil {
			finished = p.FinishedAt.Local().Format("2 Jan 2006 15:04")
		}
		rows = append(rows, []string{
			t.Style.Text.Render(fmt.Sprint(p.Pass)), t.Style.Muted.Render(p.ID), t.Style.Text.Render(string(p.Status)),
			t.Style.Muted.Render(p.CreatedAt.Local().Format("2 Jan 2006 15:04")), t.Style.Muted.Render(finished),
		})
	}
	return t.Style.Heading.Render("Passes") + "\n" + ui.Table(t, []ui.Column{
		{Title: "Pass", Align: ui.Right}, {Title: "Run", Flex: true}, {Title: "Status"}, {Title: "Started"}, {Title: "Finished"},
	}, rows, width)
}

// skippedChats counts the direct-message conversations of a run that were
// skipped because Microsoft refused a chat for them.
func skippedChats(p *store.Progress) int {
	n := 0
	if p != nil {
		for _, c := range p.Channels {
			if c.State == store.ChannelSkipped {
				n++
			}
		}
	}
	return n
}

// --- live view ---------------------------------------------------------------

type runMsg struct {
	v   controller.RunView
	err error
}
type frameMsg struct{}

type migrateModel struct {
	ctx      context.Context
	client   *controller.Client
	theme    theme.Theme
	locked   bool
	name     string
	width    int
	v        controller.RunView
	err      error
	frame    int
	started  time.Time
	rate     float64
	lastN    int
	lastAt   time.Time
	detached bool
	done     bool
	// labels are the stages this view follows, fixed when it starts.
	labels []struct{ stage, doing, done string }
	// local is true when this process is doing the work itself (local
	// mode): leaving the view stops it.
	local bool
}

func fetchRun(ctx context.Context, c *controller.Client, id string) tea.Cmd {
	return func() tea.Msg {
		v, err := c.Run(ctx, id)
		return runMsg{v, err}
	}
}

func (m migrateModel) Init() tea.Cmd {
	cmds := []tea.Cmd{
		tea.Println("\n" + ui.Pad(ui.Banner(m.theme, verbOf(m.v), m.name))),
		fetchRun(m.ctx, m.client, m.v.ID),
		tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return frameMsg{} }),
	}
	if !m.locked {
		cmds = append(cmds, tea.RequestBackgroundColor)
	}
	return tea.Batch(cmds...)
}

func stopped(s store.RunStatus) bool {
	return s == store.RunCompleted || s == store.RunFailed || s == store.RunCancelled || s == store.RunFinalized
}

func (m migrateModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = min(msg.Width, theme.MaxContentWidth) - ui.Margin
	case tea.BackgroundColorMsg:
		if !m.locked && msg.IsDark() != m.theme.Dark {
			m.theme = theme.New(theme.Options{Dark: msg.IsDark(), Unicode: m.theme.Unicode})
		}
	case tea.KeyPressMsg:
		if k := msg.String(); k == "ctrl+c" || k == "q" {
			m.detached = true
			return m, tea.Quit
		}
	case frameMsg:
		if m.done {
			return m, nil
		}
		m.frame++
		return m, tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return frameMsg{} })
	case runMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, tea.Quit
		}
		m.v = msg.v
		if p := msg.v.Progress; p != nil {
			// A smoothed posts-per-second, so the number is steady enough
			// to read.
			now := time.Now()
			if !m.lastAt.IsZero() {
				if dt := now.Sub(m.lastAt).Seconds(); dt > 0 {
					inst := float64(p.Posted-m.lastN) / dt
					m.rate = 0.7*m.rate + 0.3*inst
				}
			}
			m.lastN, m.lastAt = p.Posted, now
		}
		if stopped(m.v.Status) {
			m.done = true
			// Leave the final state of every stage in the scrollback.
			final := ui.Pad(stagesView(m.theme, m.v, m.labels, m.width, m.theme.Glyph.Spinner[0], 0))
			return m, tea.Sequence(tea.Println("\n"+final), tea.Quit)
		}
		return m, tea.Tick(700*time.Millisecond, func(time.Time) tea.Msg {
			return fetchRun(m.ctx, m.client, m.v.ID)()
		})
	}
	return m, nil
}

func (m migrateModel) View() tea.View {
	if m.done || m.err != nil {
		return tea.NewView("")
	}
	t := m.theme
	spin := t.Glyph.Spinner[m.frame%len(t.Glyph.Spinner)]
	body := stagesView(t, m.v, m.labels, m.width, spin, m.rate)
	hint := "ctrl+c to detach; the run continues"
	if m.local {
		hint = "ctrl+c to stop; taut migrate resumes"
	}
	foot := t.Style.Faint.Render(checklist.Elapsed(time.Since(m.started)) + " " + t.Glyph.Sep + " " + hint)
	return tea.NewView("\n" + ui.Pad(body) + "\n\n" + ui.Pad(foot))
}

// follow watches a run until it stops or the operator detaches. It returns
// the last view seen and whether the operator detached.
func (a *App) follow(ctx context.Context, c *controller.Client, name string, v controller.RunView) (controller.RunView, bool, error) {
	out := a.IO
	started := time.Now()
	if out.Interactive() {
		m := migrateModel{ctx: ctx, client: c, theme: out.Theme, locked: out.ThemeLocked(), name: name,
			width: out.Width - ui.Margin, v: v, started: started, labels: viewLabels(v), local: a.isLocal()}
		final, err := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(out.In), tea.WithEnvironment(out.Env())).Run()
		fm, ok := final.(migrateModel)
		if !ok {
			return v, false, err
		}
		out.SetDark(fm.theme.Dark)
		if fm.err != nil {
			return fm.v, false, fm.err
		}
		if ctx.Err() != nil {
			// Interrupted from outside (a signal): that is a detach, and
			// the program's own error is just the cancellation.
			return fm.v, true, nil //nolint:nilerr
		}
		return fm.v, fm.detached, err
	}

	// Not a terminal: no cursor movement. Print a line when a stage
	// finishes and a heartbeat now and then, so a CI log shows progress
	// without being flooded.
	if !out.IsJSON() {
		out.Println()
		out.Println(ui.Pad(ui.Banner(out.Theme, verbOf(v), name)))
		out.Println()
	}
	reported := map[string]bool{}
	labels := viewLabels(v)
	lastBeat := time.Now()
	last, err := c.Wait(ctx, v.ID, time.Second, func(v controller.RunView) bool {
		if out.IsJSON() || v.Progress == nil {
			return !stopped(v.Status)
		}
		t := out.Theme
		for _, l := range labels {
			s, ok := stageOf(v.Progress, l.stage)
			if ok && s.Lanes > 0 && s.Done == s.Lanes && !reported[l.stage] {
				reported[l.stage] = true
				out.Println(ui.Pad(ui.CheckRow(t, theme.StatusOK, l.done, 20, stageDetail(l.stage, s, v.Progress), out.Width-ui.Margin)))
			}
		}
		if time.Since(lastBeat) >= 15*time.Second && !stopped(v.Status) && v.Status != store.RunFinalizing {
			lastBeat = time.Now()
			p := v.Progress
			out.Println(ui.Pad(t.Style.Muted.Render(fmt.Sprintf("  %s of %s posts imported %s %s elapsed",
				comma(p.Posted+p.Failed+p.Held), comma(p.Posts), t.Glyph.Sep, checklist.Elapsed(time.Since(started))))))
		}
		return !stopped(v.Status)
	})
	if errors.Is(err, context.Canceled) {
		return last, true, nil
	}
	return last, false, err
}

// uploadExport hashes and streams the export ZIP to the controller.
func (a *App) uploadExport(ctx context.Context, c *controller.Client, m *config.Config, id string) error {
	path := m.ResolvePath(m.Source.Slack.Export)
	st, err := os.Stat(path)
	if err != nil {
		return fail.Wrap(err, "export.not_found", "The Slack export could not be opened", "", "Check source.slack.export in the config file.")
	}
	if st.IsDir() {
		return fail.New("export.is_directory", "`taut migrate` needs the export as a ZIP file",
			path+" is a folder.", "Point source.slack.export at the original ZIP, or zip the folder's contents.").WithExit(fail.ExitUsage)
	}
	// Direct messages that are not migrated are not staged either: what is
	// uploaded is a copy of the export without them. Nothing private that
	// the migration does not need leaves this machine.
	if !m.Target.DirectMessages.Enabled {
		has, err := export.HasDirectMessages(path)
		if err != nil {
			return err
		}
		if has {
			a.IO.Status("Leaving the export's direct messages out of the upload…")
			tmp, err := os.CreateTemp("", "taut-export-*.zip") // 0600
			if err != nil {
				return fail.Wrap(err, "export.copy_failed", "A copy of the export without its direct messages could not be made", "",
					"Check the temporary folder ("+os.TempDir()+") and its free space, then retry.")
			}
			defer func() {
				tmp.Close()
				os.Remove(tmp.Name())
			}()
			if _, err := export.StripDirectMessages(path, tmp); err != nil {
				return err
			}
			if err := tmp.Close(); err != nil {
				return fail.Wrap(err, "export.copy_failed", "A copy of the export without its direct messages could not be made", "",
					"Check the free space in "+os.TempDir()+", then retry.")
			}
			path = tmp.Name()
			if st, err = os.Stat(path); err != nil {
				return err
			}
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return fail.Wrap(err, "export.unreadable", "The Slack export could not be read", "", "Check the file's permissions.")
	}
	defer f.Close()

	a.IO.Status("Checking the export (%s)…", bytesize(st.Size()))
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fail.Wrap(err, "export.unreadable", "The Slack export could not be read", "", "Check the file and retry.")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	a.IO.Status("Uploading the export to staging (encrypted)…")
	if _, err := c.UploadExport(ctx, id, f, hex.EncodeToString(h.Sum(nil))); err != nil {
		return err
	}
	if m.Identity.Overrides != "" {
		of, err := os.Open(m.ResolvePath(m.Identity.Overrides))
		if err != nil {
			return fail.Wrap(err, "identity.overrides_unreadable", "The identity override file could not be read", "", "Check identity.overrides in the config file.")
		}
		defer of.Close()
		return c.UploadOverrides(ctx, id, of)
	}
	return nil
}

func newMigrateCmd(app *App) *cobra.Command {
	var fresh, detach, noPreflight, incremental bool
	cmd := &cobra.Command{
		Use:     "migrate",
		Short:   "Run or resume the migration",
		GroupID: groupRun,
		Long: `Import the Slack export into Microsoft Teams.

migrate first repeats the preflight and refuses to start if it finds a blocker.
It then hands the export to the control plane, which plans the run and puts
workers to work, and shows progress until the import is finished.

It is safe to stop at any time. Press ctrl+c to detach: the run continues in
the background. Run migrate again to reattach, or to resume a run that stopped;
anything can be killed mid-run and no message is ever imported twice.

People keep using Slack during a long migration. With --incremental, migrate
starts a later pass from a newer export of the same workspace: it imports the
messages, thread replies and files the earlier passes did not, into the same
teams and channels. It refuses an export of another workspace. Edits and
deletions made in Slack since are not carried over; the import API cannot
change a message.`,
		Example: `  taut migrate
  taut migrate --yes --json        # for CI: accept warnings, print the final state
  taut migrate --detach            # start it and return
  taut migrate --new               # abandon the current run and start over
  taut migrate --incremental       # after a new export: import only what was added`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, ctx := app.IO, cmd.Context()
			m, err := app.loadConfig()
			if err != nil {
				return err
			}
			if detach && m.Local.Enabled {
				return errNoDetach("migrate")
			}
			client, release, err := app.control(ctx, m, true)
			if err != nil {
				return err
			}
			defer release()

			// An existing run is resumed without repeating the preflight:
			// its plan is already fixed.
			existing, findErr := client.LatestRun(ctx, m.Metadata.Name)
			resuming := findErr == nil && existing.Status.Active() && !fresh
			if findErr != nil && fail.From(findErr).Code != "run.not_found" {
				return findErr
			}
			// An incremental pass adds to a migration that has finished
			// importing. Asking for one says nothing about a run that is
			// still going: a pass in progress is resumed, and a first
			// import must finish first.
			switch {
			case !incremental:
			case findErr != nil:
				return fail.New("migrate.nothing_to_follow", "There is no earlier pass to add to",
					"No migration named "+m.Metadata.Name+" has been run on this controller.",
					"Run `taut migrate` for the first import. `--incremental` adds to one that has finished.").WithExit(fail.ExitUsage)
			case existing.Status == store.RunFinalizing:
				return fail.New("migrate.finalizing", "This migration is being finalized",
					"Run "+existing.ID+" is completing migration mode and adding members.",
					"Run `taut finalize` to watch it finish, then `taut migrate --incremental` again.").WithExit(fail.ExitUsage)
			case existing.Status.Active() && existing.Pass <= 1:
				return fail.New("migrate.first_import_running", "The first import of this migration has not finished",
					"Run "+existing.ID+" is "+string(existing.Status)+".",
					"Run `taut migrate` to finish it. An incremental pass can only follow a pass that is complete.").WithExit(fail.ExitUsage)
			}
			// A migration that has been imported is not imported again by
			// accident: a second run would write into teams people may
			// already be using.
			if findErr == nil && !fresh && !incremental {
				switch existing.Status {
				case store.RunCompleted:
					return fail.New("migrate.already_imported", "This migration has already been imported",
						"Run "+existing.ID+" finished importing and is waiting to be finalized.",
						"Run `taut finalize` to finalize it. To import what was added in Slack since, export the workspace again and run `taut migrate --incremental`. `taut migrate --new` imports again from the start.").WithExit(fail.ExitUsage)
				case store.RunFinalizing:
					return fail.New("migrate.finalizing", "This migration is being finalized",
						"Run "+existing.ID+" is completing migration mode and adding members.",
						"Run `taut finalize` to watch it finish.").WithExit(fail.ExitUsage)
				case store.RunFailed:
					if _, finalizing := stageOf(existing.Progress, store.StageFinalize); finalizing {
						return fail.New("migrate.finalizing_failed", "Finalizing this migration stopped part-way",
							"Run "+existing.ID+" was imported; it failed while being finalized.",
							"Run `taut finalize` to continue finalizing.").WithExit(fail.ExitUsage)
					}
				case store.RunFinalized:
					return fail.New("migrate.finalized", "This migration is finished",
						"Run "+existing.ID+" was imported and finalized; its channels are open to their members.",
						"Nothing more to do. To import what was added in Slack since, export the workspace again and run `taut migrate --incremental`. To migrate again into new teams, change the config file and run `taut migrate --new`.").WithExit(fail.ExitUsage)
				}
			}

			// dmNotice is what this command says about direct messages
			// wherever it reports, --json included: that private
			// conversations are imported, and where to.
			var dmNotice *preflight.Finding
			if m.Target.DirectMessages.Enabled {
				f := preflight.PrivateNotice()
				dmNotice = &f
			}
			if !resuming && noPreflight && dmNotice != nil {
				// Skipping the preflight skips its questions, but not this
				// one: it is not about whether the import can work.
				t, w := out.Theme, out.Width-ui.Margin
				switch {
				case out.Yes:
					if !out.IsJSON() {
						out.Println(ui.Pad("\n" + ui.Heading(t, "Direct messages", w) + "\n" +
							ui.Finding(t, theme.StatusWarn, ui.Sanitize(dmNotice.Message), ui.Sanitize(dmNotice.Next), w)))
					}
				case !out.Interactive():
					return fail.New("migrate.direct_messages_unconfirmed", "Importing direct messages needs confirmation",
						"Direct messages are private conversations, and migrate will not decide to import them when it cannot ask.",
						"Run `taut preflight` to see which conversations go where, then `taut migrate --yes` to proceed.").WithExit(fail.ExitBlockers)
				default:
					out.Println(ui.Pad("\n" + ui.Heading(t, "Direct messages", w) + "\n" +
						ui.Finding(t, theme.StatusWarn, ui.Sanitize(dmNotice.Message), ui.Sanitize(dmNotice.Next), w)))
					out.Println()
					proceed := false
					err := huh.NewForm(huh.NewGroup(huh.NewConfirm().
						Title("Import private conversations into people's Teams chats?").
						Description("Direct messages are private. Each goes into a chat its participants can see while the import runs; one-on-one history goes into the chat two people already have. Imported messages cannot be removed by Taut.").
						Affirmative("Import the direct messages").Negative("Not yet").Value(&proceed))).
						WithTheme(forms.Theme(out.Theme)).WithInput(out.In).RunWithContext(ctx)
					if err != nil || !proceed {
						return fail.Exit(fail.ExitOK, "not started") //nolint:nilerr
					}
				}
			}
			if !resuming && !noPreflight {
				// For an incremental pass the preflight compares the export
				// with what the earlier passes imported. That comparison is
				// also what establishes that the export is of the same
				// workspace, before anything is uploaded.
				var base *preflight.Base
				if incremental {
					if base, err = app.passBase(ctx, m, fresh); err != nil {
						return err
					}
				}
				rep, err := app.preflight(ctx, m, false, base)
				if err != nil {
					return err
				}
				t, w := out.Theme, out.Width-ui.Margin
				if inc := rep.Incremental; inc != nil && rep.Verdict != preflight.VerdictBlocked &&
					inc.New.Messages == 0 && inc.New.Channels == 0 && inc.New.Chats == 0 {
					// Nothing to add. Starting a pass would only reopen
					// nothing and record that; say so instead.
					if out.IsJSON() {
						return out.JSON(map[string]any{"started": false, "reason": "nothing_new", "incremental": inc})
					}
					out.Println(ui.Pad("\n" + ui.Verdict(t, theme.StatusOK, "Nothing new to import",
						countOf(inc.Already.Messages, "message", "messages")+" already in Teams") +
						"\n" + ui.Hint(t, "The export holds no message the earlier passes did not import. Export the workspace again when there is more.", w)))
					return nil
				}
				if rep.Verdict == preflight.VerdictBlocked {
					if out.IsJSON() {
						_ = out.JSON(map[string]any{"started": false, "blockers": rep.Blockers})
					} else {
						var b strings.Builder
						b.WriteString("\n" + ui.Verdict(t, theme.StatusFail, "Not started", countOf(len(rep.Blockers), "blocker", "blockers")))
						for _, f := range rep.Blockers {
							b.WriteString("\n" + ui.Finding(t, theme.StatusFail, ui.Sanitize(f.Message), ui.Sanitize(f.Next), w))
						}
						report := "taut preflight"
						if incremental {
							report += " --incremental"
						}
						b.WriteString("\n\n" + ui.Next(t, report, "see the full preflight report"))
						out.Println(ui.Pad(b.String()))
					}
					return fail.Exit(fail.ExitBlockers, "preflight found blockers")
				}
				if len(rep.Warnings) > 0 && !out.Yes {
					if !out.Interactive() {
						return fail.New("migrate.warnings", "The preflight has "+countOf(len(rep.Warnings), "warning", "warnings")+" to review",
							"migrate will not accept warnings on your behalf when it cannot ask.",
							"Run `taut preflight` to read them, then `taut migrate --yes` to proceed.").WithExit(fail.ExitBlockers)
					}
					var b strings.Builder
					b.WriteString("\n" + ui.Heading(t, "Before starting", w))
					for _, f := range rep.Warnings {
						b.WriteString("\n" + ui.Finding(t, theme.StatusWarn, ui.Sanitize(f.Message), "", w))
					}
					out.Println(ui.Pad(b.String()))
					out.Println()
					proceed := false
					title := fmt.Sprintf("Import %s into Microsoft Teams?", countOf(rep.Totals.Messages, "message", "messages"))
					desc := "Imported messages cannot be removed by Taut. `taut preflight` shows the full plan."
					if inc := rep.Incremental; inc != nil {
						title = fmt.Sprintf("Start pass %d and import %s into the existing teams?", inc.Pass, countOf(inc.New.Messages, "new message", "new messages"))
						desc = "Imported messages cannot be removed by Taut. `taut preflight --incremental` shows what the pass adds."
					}
					err := huh.NewForm(huh.NewGroup(huh.NewConfirm().
						Title(title).
						Description(desc).
						Affirmative("Start the import").Negative("Not yet").Value(&proceed))).
						WithTheme(forms.Theme(out.Theme)).WithInput(out.In).RunWithContext(ctx)
					if err != nil || !proceed {
						// Declining, or leaving the prompt, is a decision, not a failure.
						return fail.Exit(fail.ExitOK, "not started") //nolint:nilerr
					}
					// Direct messages are private conversations, and they
					// land in chats people can see at once. That is asked
					// about on its own, not as one warning among others.
					if dm := privateWarning(rep); dm != nil {
						proceed = false
						err := huh.NewForm(huh.NewGroup(huh.NewConfirm().
							Title(fmt.Sprintf("Import %s into people's Teams chats?", countOf(rep.Totals.Chats, "private conversation", "private conversations"))).
							Description("Direct messages are private. Each goes into a chat its participants can see while the import runs; one-on-one history goes into the chat two people already have. Imported messages cannot be removed by Taut.").
							Affirmative("Import the direct messages").Negative("Not yet").Value(&proceed))).
							WithTheme(forms.Theme(out.Theme)).WithInput(out.In).RunWithContext(ctx)
						if err != nil || !proceed {
							return fail.Exit(fail.ExitOK, "not started") //nolint:nilerr
						}
					}
				}
				// With --yes nobody is asked, so the one thing that must
				// not go unsaid is said: private conversations are about
				// to be imported, and where to.
				dmNotice = privateWarning(rep) // with the counts, or nothing if none will be imported
				if dm := dmNotice; dm != nil && out.Yes && !out.IsJSON() {
					out.Println(ui.Pad("\n" + ui.Heading(t, "Direct messages", w) + "\n" +
						ui.Finding(t, theme.StatusWarn, ui.Sanitize(dm.Message), ui.Sanitize(dm.Next), w)))
				}
			}

			configYAML, err := app.runConfig(m)
			if err != nil {
				return err
			}
			// A pass in progress is resumed as what it is; only a run that
			// is being started is started as an incremental pass.
			run, err := client.CreateRunWith(ctx, configYAML, controller.CreateOptions{Replace: fresh, Incremental: incremental && !resuming})
			if err != nil {
				return err
			}
			switch {
			case !run.Resumed && run.Pass > 1:
				out.Status("Starting pass %d, following run %s.", run.Pass, run.BaseRunID)
			case run.Resumed && run.Status == store.RunFailed:
				n, err := client.Retry(ctx, run.ID)
				if err != nil {
					return err
				}
				out.Status("Resuming run %s; retrying %s that had stopped.", run.ID, countOf(n, "lane", "lanes"))
			case run.Resumed:
				out.Status("Reattached to run %s.", run.ID)
			}
			if run.Status == store.RunAwaitingExport {
				if err := app.uploadExport(ctx, client, m, run.ID); err != nil {
					return err
				}
			}
			if detach {
				if out.IsJSON() {
					if dmNotice != nil {
						// The run as before, with the notice beside its fields.
						return out.JSON(struct {
							controller.RunView
							DirectMessages dmNoticeJSON `json:"directMessages"`
						}{run, noticeJSON(dmNotice)})
					}
					return out.JSON(run)
				}
				out.Println(ui.Pad("\n" + ui.Verdict(out.Theme, theme.StatusInfo, "Run started", run.ID) +
					"\n\n" + ui.Next(out.Theme, "taut status", "check on it")))
				return nil
			}

			started := time.Now()
			last, detached, err := app.follow(ctx, client, m.Metadata.Name, run)
			if err != nil {
				return err
			}
			if detached {
				if !out.IsJSON() {
					state, next := "the run continues in the background", "reattach"
					if app.isLocal() {
						// In local mode the work is done by this process
						// and stops with it. What was imported is recorded.
						state, next = "the import is paused; nothing is lost", "resume where it stopped"
					}
					what := "Detached"
					if app.isLocal() {
						what = "Stopped"
					}
					out.Println(ui.Pad("\n" + ui.Verdict(out.Theme, theme.StatusInfo, what, state) +
						"\n\n" + ui.Next(out.Theme, "taut migrate", next)))
				}
				return nil
			}

			var logOK *bool
			if last.Status == store.RunCompleted {
				// The audit trail is part of the result: confirm it is
				// intact before reporting success.
				lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
				ok, _, verr := client.VerifyAuditLog(lctx, last.ID)
				cancel()
				if verr == nil {
					logOK = &ok
				}
			}
			if out.IsJSON() {
				res := map[string]any{"run": last, "auditLogVerified": logOK}
				if dmNotice != nil {
					res["directMessages"] = noticeJSON(dmNotice)
				}
				if err := out.JSON(res); err != nil {
					return err
				}
			} else {
				out.Println(ui.Pad("\n" + migrateSummary(out.Theme, last, time.Since(started), logOK, out.Width-ui.Margin)))
			}
			return migrateExit(last, logOK)
		},
	}
	cmd.Flags().BoolVar(&fresh, "new", false, "abandon the current run for this config file and start a new one")
	cmd.Flags().BoolVar(&incremental, "incremental", false, "start a later pass: import, from a newer export, only what the earlier passes did not")
	cmd.Flags().BoolVar(&detach, "detach", false, "start (or resume) the run and return without watching it")
	cmd.Flags().BoolVar(&noPreflight, "no-preflight", false, "skip the preflight gate")
	_ = cmd.Flags().MarkHidden("no-preflight")
	return cmd
}

// dmNoticeJSON is how `taut migrate --json` says that private conversations
// are imported. With --yes nobody is asked and with --json nothing is
// printed, so the document itself carries it.
type dmNoticeJSON struct {
	Code   string `json:"code"`
	Notice string `json:"notice"`
	Next   string `json:"next"`
}

func noticeJSON(f *preflight.Finding) dmNoticeJSON {
	return dmNoticeJSON{Code: f.Code, Notice: f.Message, Next: f.Next}
}

// privateWarning returns the preflight's warning that private conversations
// are about to be imported, if it has one.
func privateWarning(rep *preflight.Report) *preflight.Finding {
	for i := range rep.Warnings {
		if rep.Warnings[i].Code == "dm.private" {
			return &rep.Warnings[i]
		}
	}
	return nil
}

func migrateExit(v controller.RunView, logOK *bool) error {
	switch {
	case v.Status == store.RunFailed:
		return fail.Exit(fail.ExitFailure, "the import stopped")
	case v.Progress != nil && v.Progress.Posts-v.Progress.Posted-v.Progress.Failed-v.Progress.Held > 0 && v.Status == store.RunCompleted:
		return fail.Exit(fail.ExitFailure, "posts were left unimported")
	case logOK != nil && !*logOK:
		return fail.Exit(fail.ExitFailure, "the audit log failed verification")
	}
	return nil
}

func newStatusCmd(app *App) *cobra.Command {
	var watch bool
	cmd := &cobra.Command{
		Use:     "status",
		Short:   "Show the state of the migration",
		GroupID: groupInspect,
		Long: `Show where the current run for this config file stands: each stage, the
channels in progress, anything that is retrying or has failed, and the totals.

status only reads. Use --watch to follow a run without being able to start one.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, ctx := app.IO, cmd.Context()
			m, err := app.loadConfig()
			if err != nil {
				return err
			}
			client, release, err := app.control(ctx, m, false)
			if err != nil {
				return err
			}
			defer release()
			v, err := client.LatestRun(ctx, m.Metadata.Name)
			if err != nil {
				if fe := fail.From(err); fe.Code == "run.not_found" {
					fe.What = "No migration has been started for " + m.Metadata.Name
					fe.Next = "Run `taut migrate` to start one."
					return fe
				}
				return err
			}
			if watch && !stopped(v.Status) {
				last, detached, err := app.follow(ctx, client, m.Metadata.Name, v)
				if err != nil || detached {
					return err
				}
				v = last
			}
			if out.IsJSON() {
				return out.JSON(v)
			}
			t, w := out.Theme, out.Width-ui.Margin
			var b strings.Builder
			if !watch {
				b.WriteString("\n" + ui.Banner(t, "status", m.Metadata.Name) + "\n\n")
				b.WriteString(migrateView(t, v, w, t.Glyph.Pending, 0) + "\n")
			}
			b.WriteString("\n" + migrateSummary(t, v, 0, nil, w))
			rows := [][2]string{{"Run", v.ID}, {"Started", v.CreatedAt.Local().Format("2 Jan 2006 15:04")}}
			if pass := passLine(v); pass != "" {
				rows = append(rows, [2]string{"Pass", pass})
			}
			b.WriteString("\n\n" + ui.KV(t, rows))
			if table := passesTable(t, v, w); table != "" {
				b.WriteString("\n\n" + table)
			}
			out.Println(ui.Pad(b.String()))
			return nil
		},
	}
	cmd.Flags().BoolVar(&watch, "watch", false, "keep watching until the run stops")
	return cmd
}
