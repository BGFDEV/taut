package cli

import (
	"bufio"
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"

	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/tui/auditlog"
	"github.com/bgfdev/taut/internal/tui/shell"
	"github.com/bgfdev/taut/internal/tui/theme"
	"github.com/bgfdev/taut/internal/ui"
)

// auditLogFlags are the command's flags as typed.
type auditLogFlags struct {
	run, channel, stream, op, outcome, grep string
	since, until, export, format            string
	errors, verify                          bool
	limit                                   int
	limitSet                                bool
}

// auditLogPlan is a validated request: everything the command decided
// before talking to the control plane.
type auditLogPlan struct {
	filter store.LogFilter
	// limit bounds a listing; zero means every match.
	limit int
	// exportPath, when set, writes matches to a file instead of listing.
	exportPath string
	format     auditlog.Format
	verify     bool
	overwrite  bool
	// scope describes the fixed filters for the browser's filter line.
	scope []string
}

// auditLogDefaultLimit is how many entries a listing shows unless told
// otherwise.
const auditLogDefaultLimit = 200

// plan validates the flags. Nothing here needs the network, so a mistyped
// flag is reported before anything else happens.
func (f auditLogFlags) plan(now time.Time, yes bool) (auditLogPlan, error) {
	p := auditLogPlan{verify: f.verify, overwrite: yes, exportPath: f.export}
	p.filter = store.LogFilter{Stream: f.stream, Op: f.op, Contains: f.grep}

	switch f.outcome {
	case "", "ok", "error":
		p.filter.Outcome = f.outcome
	default:
		return p, fail.Usagef("Use `--outcome ok` or `--outcome error`.", "--outcome %q is not an outcome", f.outcome)
	}
	if f.errors {
		if f.outcome == "ok" {
			return p, fail.Usagef("Use one or the other.", "--errors and --outcome ok contradict each other")
		}
		p.filter.Outcome = "error"
	}
	if f.channel != "" && f.stream != "" {
		return p, fail.Usagef("Use `--channel` for all of a channel's entries, or `--stream` for one lane.",
			"--channel and --stream cannot be combined")
	}

	var err error
	if f.since != "" {
		if p.filter.Since, err = parseWhen(f.since, now); err != nil {
			return p, fail.Usagef("Use a time such as `2026-11-14T09:00:00Z`, a date, or an age such as `2h` or `3d`.",
				"--since %q is not a time: %s", f.since, err)
		}
		p.scope = append(p.scope, "since "+p.filter.Since.UTC().Format("2006-01-02 15:04Z"))
	}
	if f.until != "" {
		if p.filter.Until, err = parseWhen(f.until, now); err != nil {
			return p, fail.Usagef("Use a time such as `2026-11-14T09:00:00Z`, a date, or an age such as `2h` or `3d`.",
				"--until %q is not a time: %s", f.until, err)
		}
		p.scope = append(p.scope, "until "+p.filter.Until.UTC().Format("2006-01-02 15:04Z"))
	}
	if !p.filter.Since.IsZero() && !p.filter.Until.IsZero() && !p.filter.Since.Before(p.filter.Until) {
		return p, fail.Usagef("Swap them, or widen the range.", "--since is not before --until, so nothing can match")
	}

	if f.limit < 0 {
		return p, fail.Usagef("Use a positive number, or 0 for every match.", "--limit %d is not a valid limit", f.limit)
	}
	p.limit = f.limit
	if f.export != "" && !f.limitSet {
		p.limit = 0 // an export is complete unless a limit was asked for
	}

	if f.format != "" {
		if f.export == "" {
			return p, fail.Usagef("Add `--export <file>`, or use `--json` to print entries as NDJSON.", "--format only applies to --export")
		}
		var ok bool
		if p.format, ok = auditlog.ParseFormat(f.format); !ok {
			return p, fail.Usagef("Use `--format ndjson` or `--format csv`.", "--format %q is not an export format", f.format)
		}
	} else if f.export != "" {
		p.format = auditlog.FormatFor(f.export)
	}
	return p, nil
}

var ageToken = regexp.MustCompile(`^(\d+(?:\.\d+)?)(ms|w|d|h|m|s)`)

// parseAge parses an age such as "2h", "3d", "1w2d" or "90m". Days and
// weeks are accepted because that is how people think about an audit
// window; a day is 24 hours.
func parseAge(s string) (time.Duration, bool) {
	var total time.Duration
	rest := s
	for rest != "" {
		m := ageToken.FindStringSubmatch(rest)
		if m == nil {
			return 0, false
		}
		n, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			return 0, false
		}
		unit := map[string]time.Duration{
			"ms": time.Millisecond, "s": time.Second, "m": time.Minute,
			"h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour,
		}[m[2]]
		step := time.Duration(n * float64(unit))
		if n > float64(1<<62)/float64(unit) || total+step < total {
			return 0, false
		}
		total += step
		rest = rest[len(m[0]):]
	}
	return total, s != ""
}

// parseWhen parses a --since or --until value: an age relative to now
// ("2h", "3d"), an RFC 3339 time, or a date or date-time without a zone,
// which is read in now's location.
func parseWhen(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if d, ok := parseAge(s); ok {
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, now.Location()); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("expected an RFC 3339 time, a date, or an age")
}

// controllerLog adapts the controller client to auditlog.Source for one run.
type controllerLog struct {
	client *controller.Client
	run    string
}

func (b controllerLog) AuditLog(ctx context.Context, f store.LogFilter) (controller.LogPage, error) {
	return b.client.AuditLog(ctx, b.run, f)
}

func (b controllerLog) Verify(ctx context.Context) (bool, string, error) {
	return b.client.VerifyAuditLog(ctx, b.run)
}

var slackChannelID = regexp.MustCompile(`^[CGD][A-Z0-9]{4,}$`)

// channelStages are the stages whose lanes write to Microsoft 365 on behalf
// of one channel, and so have an audit log stream named "<stage>:<channel>".
var channelStages = []string{store.StageFiles, store.StageLoad, store.StageFinalize}

// channelStreams resolves --channel to the streams holding that channel's
// entries. A name is looked up in the run; a Slack channel ID is accepted
// as it stands, so the flag still works for a run whose plan has not been
// made yet. A direct message is found by its Slack ID only: it has no name,
// and it is labelled by that ID, not by who is in it.
func channelStreams(v controller.RunView, channel string) (label string, streams []string, err error) {
	want := strings.TrimPrefix(strings.TrimSpace(channel), "#")
	id := ""
	if v.Progress != nil {
		for _, c := range v.Progress.Channels {
			chat := store.IsChatKind(c.Kind)
			if strings.EqualFold(c.SlackID, want) || (!chat && c.SlackName == want) {
				id, label = c.SlackID, "#"+c.SlackName
				if chat {
					label = "direct message " + c.SlackID
				}
				break
			}
		}
	}
	if id == "" {
		if !slackChannelID.MatchString(want) {
			return "", nil, fail.Usagef(
				"Check the name with `taut status`, or pass the Slack channel ID (like C024BE91L) or an exact `--stream` such as load:C024BE91L.",
				"This run has no channel named %q", channel)
		}
		id, label = want, want
	}
	for _, stage := range channelStages {
		// A direct message has no files lane: its files are not moved.
		if stage == store.StageFiles && strings.HasPrefix(label, "direct message ") {
			continue
		}
		streams = append(streams, stage+":"+id)
	}
	return label, streams, nil
}

func newAuditLogCmd(app *App) *cobra.Command {
	var f auditLogFlags
	cmd := &cobra.Command{
		Use:     "audit",
		Short:   "Search and export the audit trail",
		GroupID: groupInspect,
		Long: `Search and export the audit log: the append-only, hash-chained record of
everything Taut wrote, or tried to write, to Microsoft 365. Each entry has
the Slack source ID, the Teams target ID, a hash of the content, the time,
the worker that did it and the outcome.

On a terminal, audit opens a browser: scroll, filter, open an entry to
see every field, export, and verify. Piped, or with --yes, it prints a
table; with --json it prints one JSON object per line (NDJSON).

Filters combine. --channel takes a Slack channel name or ID and covers that
channel's own lanes (files, load and finalize). A direct message is named by
its Slack ID (like D024BE91L). Entries about the team itself,
such as creating the team or adding members, live in streams like team:0
and members:0: reach those with --stream, which takes one exact stream.

--since and --until take an RFC 3339 time (2026-11-14T09:00:00Z), a date
(2026-11-14, local time), or an age counted back from now (90m, 2h, 3d, 1w).

--export writes every match to a file, ignoring the display limit unless
--limit is given. The format follows the extension (.csv, otherwise
NDJSON) or --format. The file is written in one step, is readable only by
you, and its SHA-256 is printed so the export can be shown to be unaltered
later. An existing file is replaced only with --yes.

--verify asks the controller to recompute every hash chain of the run. It exits 1
if any entry was altered, removed or is missing. With --export, the
audit log is verified first and nothing is exported if it fails.

audit only reads.`,
		Example: `  taut audit --errors
  taut audit --channel general --since 2h
  taut audit --op message.fail --grep 403 --json
  taut audit --verify --export audit.csv`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, ctx := app.IO, cmd.Context()
			f.limitSet = cmd.Flags().Changed("limit")
			p, err := f.plan(time.Now(), app.Yes)
			if err != nil {
				return err
			}
			m, err := app.loadConfig()
			if err != nil {
				return err
			}
			client := app.controllerClient(m)
			var v controller.RunView
			if f.run != "" {
				v, err = client.Run(ctx, f.run)
			} else {
				v, err = client.LatestRun(ctx, m.Metadata.Name)
			}
			if err != nil {
				if fe := fail.From(err); fe.Code == "run.not_found" && f.run == "" {
					fe.What = "No migration has been started for " + m.Metadata.Name
					fe.Next = "Run `taut migrate` to start one; the audit log begins with the first write."
					return fe
				}
				return err
			}

			var src auditlog.Source = controllerLog{client: client, run: v.ID}
			switch {
			case f.channel != "":
				label, streams, err := channelStreams(v, f.channel)
				if err != nil {
					return err
				}
				src = auditlog.Streams(src, streams)
				p.scope = append([]string{label}, p.scope...)
			case f.stream != "":
				p.scope = append([]string{f.stream}, p.scope...)
			}
			return runAuditLog(ctx, out, src, p, auditLogRun{name: m.Metadata.Name, id: v.ID})
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.run, "run", "", "run ID (default: the latest run for the config file)")
	fl.StringVar(&f.channel, "channel", "", "only this channel's lanes (Slack name or ID; a direct message by ID)")
	fl.StringVar(&f.stream, "stream", "", "only this exact stream, e.g. load:C024BE91L or team:0")
	fl.StringVar(&f.op, "op", "", "only this operation, e.g. message.import")
	fl.StringVar(&f.outcome, "outcome", "", "only this outcome: ok or error")
	fl.BoolVar(&f.errors, "errors", false, "only failures (same as --outcome error)")
	fl.StringVar(&f.grep, "grep", "", "only entries whose source ID, target ID or detail contains this text")
	fl.StringVar(&f.since, "since", "", "only entries at or after this time, or this recent (2h, 3d)")
	fl.StringVar(&f.until, "until", "", "only entries before this time, or older than this (2h, 3d)")
	fl.IntVar(&f.limit, "limit", auditLogDefaultLimit, "stop after this many entries; 0 for all")
	fl.StringVar(&f.export, "export", "", "write every match to this file")
	fl.StringVar(&f.format, "format", "", "export format: ndjson or csv (default: from the file extension)")
	fl.BoolVar(&f.verify, "verify", false, "recompute the hash chains and report whether they are intact")
	return cmd
}

// auditLogRun identifies the run being read, for titles and summaries.
type auditLogRun struct{ name, id string }

// auditLogVerdict is the --json document for --verify and --export.
type auditLogVerdict struct {
	Run      string                 `json:"run"`
	Verified *bool                  `json:"verified,omitempty"`
	Export   *auditlog.ExportResult `json:"export,omitempty"`
}

// runAuditLog carries out a validated request against a source. It is the
// whole command apart from finding the run, so tests drive it with a fake.
func runAuditLog(ctx context.Context, out *ui.IO, src auditlog.Source, p auditLogPlan, run auditLogRun) error {
	t, w := out.Theme, out.Width-ui.Margin
	doc := auditLogVerdict{Run: run.id}

	if p.verify {
		ok, problem, err := src.Verify(ctx)
		if err != nil {
			return err
		}
		if !ok {
			fe := auditlog.Corrupt(problem)
			if p.exportPath != "" {
				fe.Next += " Nothing was exported; to export the entries as they are for investigation, run the command again without `--verify`."
			}
			return fe
		}
		doc.Verified = &ok
	}

	switch {
	case p.exportPath != "":
		res, err := auditlog.Export(ctx, src, p.filter, auditlog.ExportOptions{
			Path: p.exportPath, Format: p.format, Limit: p.limit, Overwrite: p.overwrite,
		})
		if err != nil {
			return err
		}
		doc.Export = &res
		if out.IsJSON() {
			return out.JSON(doc)
		}
		rows := [][2]string{{"File", auditlog.Sanitize(res.Path)}, {"SHA-256", res.SHA256}, {"Run", run.id}}
		if p.verify {
			rows = append(rows, [2]string{"Verified", "every hash chain was intact before the export"})
		}
		out.Println(ui.Pad("\n" + ui.Banner(t, "audit", run.name) + "\n\n" +
			auditlog.ExportSummary(t, res) + "\n\n" + ui.KV(t, rows)))
		return nil

	case p.verify:
		if out.IsJSON() {
			return out.JSON(doc)
		}
		out.Println(ui.Pad("\n" + ui.Banner(t, "audit", run.name) + "\n\n" +
			ui.Verdict(t, theme.StatusOK, "Audit log verified", "every hash chain is intact", "run "+run.id)))
		return nil

	case out.IsJSON():
		// NDJSON: one entry per line, the same bytes an export would hold.
		buf := bufio.NewWriter(out.Out)
		enc := auditlog.NewEncoder(buf, auditlog.NDJSON)
		if _, err := auditlog.Walk(ctx, src, p.filter, p.limit, enc.Entry); err != nil {
			_ = buf.Flush()
			return err
		}
		return buf.Flush()

	case out.Interactive():
		scr := auditlog.NewScreen(src, auditlog.Options{
			Context: ctx, Filter: p.filter,
			Scope:      strings.Join(p.scope, " "+t.Glyph.Sep+" "),
			ExportPath: "audit-" + run.id + ".ndjson",
		})
		return shell.Run(ctx, out, scr, shell.Options{Command: "audit", Context: run.name})
	}

	var entries []store.LogEntry
	more, err := auditlog.Walk(ctx, src, p.filter, p.limit, func(e store.LogEntry) error {
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return err
	}
	out.Println(ui.Pad("\n" + ui.Banner(t, "audit", run.name) + "\n\n" + auditLogTable(t, entries, more, w)))
	return nil
}

// auditLogTable renders a listing and its summary line. An entry's detail
// hangs beneath its row, cut to the width: it is the explanation of a
// failure, and a column for it would squeeze the IDs out of a narrow
// terminal. The summary only states what is known: a count is a total only
// when nothing was left out.
func auditLogTable(t theme.Theme, entries []store.LogEntry, more bool, width int) string {
	if len(entries) == 0 {
		return ui.Verdict(t, theme.StatusInfo, "No entries match")
	}
	const stamp = "2006-01-02T15:04:05Z"
	// The operation follows its status mark, so its title and any detail
	// beneath it are indented past the mark.
	markW := 2
	if !t.Unicode {
		markW = 3
	}
	cols := []ui.Column{
		{Title: "TIME (UTC)"},
		{Title: strings.Repeat(" ", markW) + "OPERATION"},
		{Title: "SOURCE " + t.Glyph.Arrow + " TARGET", Flex: true},
	}
	rows := make([][]string, len(entries))
	for i, e := range entries {
		rows[i] = []string{
			t.Style.Muted.Render(e.At.UTC().Format(stamp)),
			t.StatusMark(auditlog.OutcomeStatus(e.Outcome)) + " " + t.Style.Text.Render(auditlog.Sanitize(e.Op)),
			auditlog.Endpoints(t, e),
		}
	}
	lines := strings.Split(ui.Table(t, cols, rows, width), "\n")
	hang := len(stamp) + t.Space.MD + markW
	elbow := t.Glyph.Elbow + " "
	var b strings.Builder
	b.WriteString(lines[0])
	for i, e := range entries {
		b.WriteString("\n" + lines[i+1])
		if d := auditlog.Sanitize(e.Detail); d != "" {
			room := max(width-hang-ansi.StringWidth(elbow), 16)
			b.WriteString("\n" + strings.Repeat(" ", hang) + t.Style.Faint.Render(elbow) +
				t.Style.Muted.Render(ansi.Truncate(d, room, t.Glyph.Ellipsis)))
		}
	}

	count := countOf(len(entries), "entry", "entries")
	fact := "all that match"
	if more {
		fact = "more available: add --limit 0 or --export <file>"
	}
	return b.String() + "\n\n" + ui.Verdict(t, theme.StatusInfo, count, fact)
}
