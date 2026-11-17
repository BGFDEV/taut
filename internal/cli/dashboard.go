package cli

import (
	"context"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/tui/dashboard"
	"github.com/bgfdev/taut/internal/tui/shell"
	"github.com/bgfdev/taut/internal/tui/theme"
	"github.com/bgfdev/taut/internal/ui"
)

// dashboardSource adapts the controller client and one run to what the dashboard
// reads from.
type dashboardSource struct {
	client *controller.Client
	run    string
}

func (s dashboardSource) Dashboard(ctx context.Context) (controller.DashboardView, error) {
	return s.client.Dashboard(ctx, s.run)
}

func (s dashboardSource) AuditLog(ctx context.Context, f store.LogFilter) (controller.LogPage, error) {
	return s.client.AuditLog(ctx, s.run, f)
}

func (s dashboardSource) Retry(ctx context.Context) (int, error) {
	return s.client.Retry(ctx, s.run)
}

// minDashboardInterval keeps --interval from turning the dashboard into a load
// test of the control plane.
const minDashboardInterval = 250 * time.Millisecond

// runVerdict is the one-line outcome of a run, worded exactly as migrate,
// finalize and status word it.
func runVerdict(t theme.Theme, v controller.RunView) string {
	return strings.SplitN(migrateSummary(t, v, 0, nil, 0), "\n", 2)[0]
}

// dashboardFarewell is what `taut dashboard` leaves in the scrollback: where the run
// stands and the command to run next.
func dashboardFarewell(t theme.Theme, v controller.RunView) string {
	if !stopped(v.Status) {
		return ui.Verdict(t, theme.StatusInfo, verdictStatement(v), "the run continues in the background") +
			"\n\n" + ui.Next(t, "taut dashboard", "watch it again")
	}
	out := runVerdict(t, v)
	switch v.Status {
	case store.RunCompleted:
		out += "\n\n" + ui.Next(t, "taut finalize", "open the channels and add their members")
	case store.RunFailed:
		out += "\n\n" + ui.Next(t, "taut migrate", "resume where it stopped; no message is ever imported twice")
	case store.RunFinalized:
		out += "\n\n" + ui.Next(t, "taut audit --export audit.ndjson", "keep the audit trail with your records")
	case store.RunCancelled:
		out += "\n\n" + ui.Next(t, "taut migrate", "start a new run")
	}
	return out
}

// verdictStatement names the phase of a run that is still going.
func verdictStatement(v controller.RunView) string {
	switch v.Status {
	case store.RunFinalizing:
		return "Finalizing"
	case store.RunAwaitingExport:
		return "Waiting for the export upload"
	case store.RunQueued:
		return "Planning"
	}
	return "Import in progress"
}

// dashboardPlain is the dashboard for output that is not a terminal: one
// snapshot, top to bottom, drawn the way `taut status` draws a run.
func dashboardPlain(t theme.Theme, name string, v controller.DashboardView, width int) string {
	var b strings.Builder
	b.WriteString("\n" + ui.Banner(t, "dashboard", name) + "\n\n")
	b.WriteString(migrateView(t, v.Run, width, t.Glyph.Pending, 0) + "\n\n")
	rows := append(dashboard.Summary(t, v), [2]string{"Run", v.Run.ID})
	b.WriteString(ui.KV(t, rows) + "\n\n")
	b.WriteString(dashboardFarewell(t, v.Run))
	return b.String()
}

func newDashboardCmd(app *App) *cobra.Command {
	var runID string
	var interval time.Duration
	cmd := &cobra.Command{
		Use:     "dashboard",
		Short:   "Watch a live dashboard of the run",
		GroupID: groupRun,
		Long: `Watch a run from a full-screen dashboard: overall progress and the time
left, every stage, each channel by team, the workers and their leases, the
shared rate budgets and whether Microsoft is throttling, and the lanes that
are retrying or have failed.

dashboard only reads, with one exception: on the problems pane, r returns failed
lanes to the queue after asking. Open a problem with enter to see its full
error and what its lane last wrote to the audit log.

Leaving the dashboard with q does not stop the run. Without a terminal, or
with --json, dashboard prints one snapshot and exits.`,
		Example: `  taut dashboard
  taut dashboard --run 01JCM4Q8ZK7W3N5T2R6V9XYB0D
  taut dashboard --interval 5s       # ask the control plane less often
  taut dashboard --json              # one snapshot, for scripts`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, ctx := app.IO, cmd.Context()
			if interval < minDashboardInterval {
				return fail.Usagef("Use an interval of "+minDashboardInterval.String()+" or longer, for example `--interval 2s`.",
					"--interval %s is too short", interval)
			}
			m, err := app.loadConfig()
			if err != nil {
				return err
			}
			client := app.controllerClient(m)
			if runID == "" {
				v, err := client.LatestRun(ctx, m.Metadata.Name)
				if err != nil {
					if fe := fail.From(err); fe.Code == "run.not_found" {
						fe.What = "No migration has been started for " + m.Metadata.Name
						fe.Next = "Run `taut migrate` to start one."
						return fe
					}
					return err
				}
				runID = v.ID
			}
			src := dashboardSource{client: client, run: runID}
			// The first snapshot is fetched here, not in the dashboard, so a
			// control plane that cannot be reached is an ordinary error
			// rather than an empty screen.
			first, err := src.Dashboard(ctx)
			if err != nil {
				return err
			}
			switch {
			case out.IsJSON():
				return out.JSON(first)
			case !out.Interactive():
				out.Println(ui.Pad(dashboardPlain(out.Theme, m.Metadata.Name, first, out.Width-ui.Margin)))
				return nil
			}
			screen := dashboard.New(src, dashboard.Options{Context: ctx, Interval: interval, Initial: &first, Verdict: runVerdict})
			if err := shell.Run(ctx, out, screen, shell.Options{Command: "dashboard", Context: m.Metadata.Name}); err != nil {
				return err
			}
			// Leave where the run stands in the scrollback after the
			// full-screen view closes.
			last, _ := screen.Last()
			out.Println(ui.Pad("\n" + dashboardFarewell(out.Theme, last.Run)))
			return nil
		},
	}
	cmd.Flags().StringVar(&runID, "run", "", "the run to watch (default: the latest run for this config file)")
	cmd.Flags().DurationVar(&interval, "interval", time.Second, "how often to ask the control plane for a new snapshot")
	return cmd
}
