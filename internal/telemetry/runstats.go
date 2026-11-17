package telemetry

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// RunStat is one active run's progress, as the controller's gauges need it. It
// carries no identity: the gauges are sums over the active runs, because a
// run ID on a metric would be an unbounded attribute.
type RunStat struct {
	// Posts by state. Pending are allocated and not yet finished.
	Posted, Failed, Held, Pending int64
	// Lanes counts lanes by stage and status.
	Lanes []LaneStat
}

// LaneStat counts the lanes of one stage in one status ("open", "active",
// "done" or "failed").
type LaneStat struct {
	Stage  string
	Status string
	Count  int64
}

type laneKey struct{ stage, status string }

// RegisterRunStats registers the controller's run gauges, read from fn at each
// collection:
//
//	taut.runs.active               number of runs fn returned
//	taut.run.posts {state}         posted | failed | held | pending
//	taut.run.lanes {stage,status}
//
// fn should return one RunStat per active run and should be cheap; it gets
// five seconds. If it fails, the collection reports no run gauges and the
// error is logged, rate limited. When telemetry is off fn is never called.
func (p *Providers) RegisterRunStats(fn func(ctx context.Context) ([]RunStat, error)) error {
	if !p.Enabled() || fn == nil {
		return nil
	}
	active, err := p.meter.Int64ObservableGauge("taut.runs.active",
		metric.WithUnit("{run}"), metric.WithDescription("Runs that are not finished."))
	if err != nil {
		return err
	}
	posts, err := p.meter.Int64ObservableGauge("taut.run.posts",
		metric.WithUnit("{post}"), metric.WithDescription("Posts of active runs, by state."))
	if err != nil {
		return err
	}
	lanes, err := p.meter.Int64ObservableGauge("taut.run.lanes",
		metric.WithUnit("{lane}"), metric.WithDescription("Lanes of active runs, by stage and status."))
	if err != nil {
		return err
	}
	_, err = p.meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		stats, err := fn(ctx)
		if err != nil {
			if ok, suppressed := p.limit.allow(); ok {
				p.log.Warn("run statistics could not be read for telemetry", "error", err.Error(), "suppressed", suppressed)
			}
			return nil // a failed read must not fail the whole collection
		}
		var posted, failed, held, pending int64
		byLane := map[laneKey]int64{}
		for _, s := range stats {
			posted, failed, held, pending = posted+s.Posted, failed+s.Failed, held+s.Held, pending+s.Pending
			for _, l := range s.Lanes {
				status := l.Status
				if !isLabel(status, 24, false) {
					status = "other"
				}
				byLane[laneKey{stageLabel(l.Stage), status}] += l.Count
			}
		}
		o.ObserveInt64(active, int64(len(stats)))
		for state, n := range map[string]int64{"posted": posted, "failed": failed, "held": held, "pending": pending} {
			o.ObserveInt64(posts, n, metric.WithAttributes(attribute.String("state", state)))
		}
		for k, n := range byLane {
			o.ObserveInt64(lanes, n, metric.WithAttributes(
				attribute.String("taut.stage", k.stage), attribute.String("status", k.status)))
		}
		return nil
	}, active, posts, lanes)
	return err
}
