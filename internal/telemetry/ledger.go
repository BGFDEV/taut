package telemetry

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/bgfdev/taut/internal/store"
)

// Ledger returns s measured: every fenced transaction a worker commits to
// the ledger is timed as taut.ledger.transaction.duration, by stage and
// outcome. That is the one number about a worker that neither the batch
// duration (which is mostly waiting for Microsoft) nor the HTTP durations
// give: how long the database takes to record a step.
//
// When telemetry is off it returns s unchanged.
func (p *Providers) Ledger(s store.Store) store.Store {
	if !p.Enabled() {
		return s
	}
	// Sub-millisecond buckets: a healthy commit is a few milliseconds, and
	// the default boundaries start at five.
	h, _ := p.meter.Float64Histogram("taut.ledger.transaction.duration",
		metric.WithUnit("s"), metric.WithDescription("Duration of one fenced ledger transaction, to its commit."),
		metric.WithExplicitBucketBoundaries(0.0005, 0.001, 0.002, 0.003, 0.005, 0.0075, 0.01, 0.015, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10))
	return &timedLedger{Store: s, duration: h}
}

type timedLedger struct {
	store.Store
	duration metric.Float64Histogram
}

func (t *timedLedger) Fenced(ctx context.Context, l *store.Lease, fn func(tx store.Tx) error) error {
	start := time.Now()
	err := t.Store.Fenced(ctx, l, fn)
	outcome, stage := "ok", "other"
	if err != nil {
		outcome = "error"
	}
	if l != nil {
		stage = stageLabel(l.Stage)
	}
	t.duration.Record(context.WithoutCancel(ctx), time.Since(start).Seconds(), metric.WithAttributes(
		attribute.String("taut.stage", stage), attribute.String("outcome", outcome)))
	return err
}
