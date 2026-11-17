package telemetry

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/worker"
)

// Observer returns the worker.Observer that records batches and events.
// When telemetry is off it does nothing.
func (p *Providers) Observer() worker.Observer {
	if !p.Enabled() {
		return nopObserver{}
	}
	return p.obs
}

type nopObserver struct{}

func (nopObserver) Batch(ctx context.Context, _ worker.BatchInfo) (context.Context, func(error)) {
	return ctx, func(error) {}
}
func (nopObserver) Count(context.Context, string, worker.Event, int64) {}

// counted says which counter an event adds to and with which outcome.
type counted struct {
	counter metric.Int64Counter
	outcome string
}

type observer struct {
	p        *Providers
	duration metric.Float64Histogram
	events   map[worker.Event]counted
}

func newObserver(p *Providers) *observer {
	o := &observer{p: p, events: map[worker.Event]counted{}}
	// Instrument creation fails only for an invalid name, which a test
	// would catch; a failed instrument is a working no-op.
	o.duration, _ = p.meter.Float64Histogram("taut.batch.duration",
		metric.WithUnit("s"), metric.WithDescription("Time spent on one attempt at one batch."),
		metric.WithExplicitBucketBoundaries(0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600))
	counter := func(name, unit, desc string) metric.Int64Counter {
		c, _ := p.meter.Int64Counter(name, metric.WithUnit(unit), metric.WithDescription(desc))
		return c
	}
	posts := counter("taut.posts", "{post}", "Posts handled by the load stage, by outcome.")
	files := counter("taut.files", "{file}", "Files handled by the files stage, by outcome.")
	o.events = map[worker.Event]counted{
		worker.EventPostImported:     {posts, "imported"},
		worker.EventPostAdopted:      {posts, "adopted"},
		worker.EventPostRejected:     {posts, "rejected"},
		worker.EventPostHeld:         {posts, "held"},
		worker.EventFileUploaded:     {files, "uploaded"},
		worker.EventFileSkipped:      {files, "skipped"},
		worker.EventFileFailed:       {files, "failed"},
		worker.EventFileBytes:        {counter("taut.file.bytes", "By", "Bytes uploaded to SharePoint."), ""},
		worker.EventLeaseLost:        {counter("taut.lease.lost", "{lease}", "Leases lost in the middle of a batch."), ""},
		worker.EventBatchRetried:     {counter("taut.batch.retries", "{attempt}", "Batch attempts that will be retried."), ""},
		worker.EventChannelFinalized: {counter("taut.channels.finalized", "{channel}", "Channels whose migration was completed."), ""},
		worker.EventMemberAdded:      {counter("taut.members.added", "{member}", "Members added to teams and channels."), ""},
	}
	return o
}

// Batch starts the span of one attempt at one batch.
func (o *observer) Batch(ctx context.Context, b worker.BatchInfo) (context.Context, func(error)) {
	start := time.Now()
	stage := stageLabel(b.Stage)
	// Run, lane and holder identify the work in a trace. They never go on
	// a metric: there is no bound on how many there are.
	ctx, span := o.p.tracer.Start(ctx, "worker.batch", trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(
		attribute.String("taut.run", b.RunID),
		attribute.String("taut.lane", b.Lane),
		attribute.String("taut.stage", stage),
		attribute.Int("taut.batch.seq", b.Seq),
		attribute.Int("taut.attempt", b.Attempt),
		attribute.String("taut.holder", b.Holder),
	))
	return ctx, func(err error) {
		outcome, class := "ok", ""
		if err != nil {
			class = errorClass(err)
			switch class {
			case "canceled", "lease_lost":
				outcome = class
			default:
				outcome = "error"
			}
			span.SetAttributes(attribute.String("error.type", class))
			if outcome == "error" {
				// The class only. An error's text can quote a message, a
				// name or a URL.
				span.SetStatus(codes.Error, class)
			}
		}
		span.SetAttributes(attribute.String("taut.outcome", outcome))
		span.End()
		// The batch's context may already be cancelled; the measurement
		// is still wanted.
		o.duration.Record(context.WithoutCancel(ctx), time.Since(start).Seconds(), metric.WithAttributes(
			attribute.String("taut.stage", stage), attribute.String("outcome", outcome)))
	}
}

// Count adds n occurrences of an event. Unknown events are ignored, so the
// set of metrics stays fixed.
func (o *observer) Count(ctx context.Context, stage string, e worker.Event, n int64) {
	c, ok := o.events[e]
	if !ok || n <= 0 || c.counter == nil {
		return
	}
	attrs := []attribute.KeyValue{attribute.String("taut.stage", stageLabel(stage))}
	if c.outcome != "" {
		attrs = append(attrs, attribute.String("outcome", c.outcome))
	}
	c.counter.Add(context.WithoutCancel(ctx), n, metric.WithAttributes(attrs...))
}

// stageLabel returns a stage name fit for a metric attribute. Stage names
// are short lowercase words; anything else becomes "other", so that a
// caller's mistake cannot put an identifier on a metric.
func stageLabel(s string) string {
	if !isLabel(s, 24, false) {
		return "other"
	}
	return s
}

// isLabel reports whether s is a short identifier of lowercase letters,
// digits and underscores, and dots when allowed.
func isLabel(s string, maxLen int, dots bool) bool {
	if s == "" || len(s) > maxLen {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
		case r == '.' && dots:
		default:
			return false
		}
	}
	return true
}

// errorClass names an error's kind without quoting it: "canceled",
// "timeout", "lease_lost", a structured error's code (such as
// "graph.throttled"), or "error".
func errorClass(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, store.ErrFenced):
		return "lease_lost"
	}
	// Codes are identifiers written in the source, never data.
	if fe, ok := errors.AsType[*fail.Error](err); ok && isLabel(fe.Code, 64, true) {
		return fe.Code
	}
	return "error"
}
