// Package dashboard is the full-screen live dashboard behind `taut dashboard`.
//
// The screen is drawn from one controller.DashboardView at a time. A snapshot is
// fetched in the background on a fixed interval; the last good one stays on
// screen when a fetch fails, marked stale, so a network blip never blanks
// the dashboard.
//
// Numbers. Every figure is a pure function of the snapshots, and every
// duration is measured on the controller's clock (DashboardView.At), never the
// operator's, so clock skew between the two cannot bend a rate or an ETA.
// Three choices are worth knowing:
//
//   - Moving denominators. A channel's Posts grows while the transform
//     stage is still allocating them. Until transform is finished the
//     dashboard treats every total as provisional: the overall posts bar
//     is measured against an estimate of the final total (posts so far,
//     scaled by the share of transform batches done, shown as "~48,000"),
//     and no bar or percentage is allowed to reach 100%. The estimate
//     settles as transform proceeds, so the bar creeps instead of running
//     to the end and falling back. Nothing is latched: if the truth moves
//     backwards the bar follows it, because a bar that only ever grows
//     would have to lie.
//   - Rates. Posts per second and file bytes per second are the change in
//     the counters between successive snapshots divided by the change in
//     DashboardView.At. The sparklines plot those samples; the figure beside
//     them, and the ETA, use an exponentially smoothed rate (time constant
//     rateTau) so they are steady enough to read.
//   - Sort. Channels are grouped by team; inside a team the default order
//     is active, failed, waiting, done, each by name. `o` switches to plain
//     name order.
package dashboard

import (
	"context"

	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/store"
)

// Source is where the dashboard gets its data: one run on one control
// plane. The CLI adapts a *controller.Client and a run ID to it; tests script it.
type Source interface {
	// Dashboard returns the current snapshot.
	Dashboard(ctx context.Context) (controller.DashboardView, error)
	// AuditLog returns one page of the run's audit trail.
	AuditLog(ctx context.Context, f store.LogFilter) (controller.LogPage, error)
	// Retry returns the run's failed lanes to the queue and reports how
	// many there were.
	Retry(ctx context.Context) (int, error)
}
