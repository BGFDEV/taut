// Package telemetry sets up OpenTelemetry traces and metrics for Taut's
// services and provides the instrumentation they share: the worker
// Observer, the HTTP server middleware, the outbound transports and the controller's
// run gauges.
//
// Telemetry is best effort. Nothing here blocks or fails the work it
// describes: spans and metrics are queued and dropped when the collector is
// away, and export errors are logged at most once per interval.
//
// Nothing secret or personal is recorded. Spans and metrics never carry
// header values, tokens, URL paths or query strings, message text, or
// people's names and addresses; an outbound request is described by its
// scheme, host and a route class from a fixed list. Identifiers of runs and
// lanes appear on spans only, never on metrics, whose attribute values all
// come from small fixed sets.
package telemetry

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// scope names the instrumentation in exported data.
const scope = "github.com/bgfdev/taut/internal/telemetry"

// Options configures Setup.
type Options struct {
	// Service is the process's role: "controller", "worker" or "egress-proxy".
	// It is exported as service.name "taut-<Service>".
	Service string
	// Version is exported as service.version.
	Version string
	// Instance is exported as service.instance.id (a worker's or
	// egress proxy's holder ID).
	Instance string
	// Getenv reads the environment. It decides whether telemetry is on and
	// where it goes; the exporters read the remaining standard OTEL_*
	// variables (headers, timeouts, compression, TLS files, sampler,
	// metric interval) from the process environment themselves.
	Getenv func(string) string
	// Log receives export problems, rate limited. Nil discards them.
	Log *slog.Logger
	// ShutdownTimeout bounds Shutdown. Default 5s.
	ShutdownTimeout time.Duration
	// ErrorInterval is the shortest time between two logged export
	// problems. Default one minute.
	ErrorInterval time.Duration
}

// Providers holds a process's trace and metric providers. The zero-cost
// form, returned when telemetry is off, records nothing, starts no
// goroutine and opens no connection.
type Providers struct {
	enabled bool
	log     *slog.Logger
	limit   *limiter
	tracer  trace.Tracer
	meter   metric.Meter
	prop    propagation.TextMapPropagator
	tp      trace.TracerProvider
	mp      metric.MeterProvider

	shutdownTimeout time.Duration
	shutdowns       []func(context.Context) error

	obs  *observer
	http *httpInstruments
}

// Disabled returns Providers that record nothing.
func Disabled() *Providers {
	return newProviders(tracenoop.NewTracerProvider(), metricnoop.NewMeterProvider(), nil, false)
}

// newProviders builds the instrumentation on top of the given providers.
func newProviders(tp trace.TracerProvider, mp metric.MeterProvider, log *slog.Logger, enabled bool) *Providers {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	p := &Providers{
		enabled: enabled, log: log, tp: tp, mp: mp,
		limit:  newLimiter(time.Minute),
		tracer: tp.Tracer(scope),
		meter:  mp.Meter(scope),
		// Trace context only. Baggage is free-form and would be forwarded
		// to whatever a request reaches, so it is not propagated.
		prop:            propagation.TraceContext{},
		shutdownTimeout: 5 * time.Second,
	}
	p.obs = newObserver(p)
	p.http = newHTTPInstruments(p)
	return p
}

// Enabled reports whether anything is exported.
func (p *Providers) Enabled() bool { return p != nil && p.enabled }

// TracerProvider returns the trace provider (a no-op one when disabled).
func (p *Providers) TracerProvider() trace.TracerProvider { return p.tp }

// MeterProvider returns the metric provider (a no-op one when disabled).
func (p *Providers) MeterProvider() metric.MeterProvider { return p.mp }

// endpoints returns where traces and metrics go, or "" for a signal that is
// off.
func endpoints(getenv func(string) string) (traces, metrics string) {
	if strings.EqualFold(strings.TrimSpace(getenv("OTEL_SDK_DISABLED")), "true") {
		return "", ""
	}
	pick := func(signal, exporter string) string {
		if strings.EqualFold(strings.TrimSpace(getenv(exporter)), "none") {
			return ""
		}
		if v := strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_" + signal + "_ENDPOINT")); v != "" {
			return v
		}
		return strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	}
	return pick("TRACES", "OTEL_TRACES_EXPORTER"), pick("METRICS", "OTEL_METRICS_EXPORTER")
}

// Setup builds the providers for one process. It exports OTLP over gRPC when
// OTEL_EXPORTER_OTLP_ENDPOINT (or the traces- or metrics-specific variable)
// is set and OTEL_SDK_DISABLED is not "true"; otherwise it returns Providers
// that do nothing. An http:// endpoint is used without TLS.
//
// Setup does not wait for the collector: it returns at once whether or not
// the endpoint is reachable. When it exports, it also installs the providers,
// the W3C trace-context propagator and the error handler as OpenTelemetry's
// globals, so call it once, from main.
//
// Setup always returns usable Providers. If the exporters cannot be built it
// returns disabled ones together with the error, which the caller should
// log and carry on.
func Setup(ctx context.Context, o Options) (*Providers, error) {
	getenv := o.Getenv
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	tracesTo, metricsTo := endpoints(getenv)
	if tracesTo == "" && metricsTo == "" {
		return Disabled(), nil
	}
	if proto := getenv("OTEL_EXPORTER_OTLP_PROTOCOL"); proto != "" && proto != "grpc" && o.Log != nil {
		o.Log.Warn("telemetry is exported over OTLP/gRPC only; OTEL_EXPORTER_OTLP_PROTOCOL is ignored", "protocol", proto)
	}

	// Later attributes win: an operator can add resource attributes through
	// OTEL_RESOURCE_ATTRIBUTES, but the service identity is ours.
	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			attribute.String("service.name", "taut-"+o.Service),
			attribute.String("service.version", o.Version),
			attribute.String("service.instance.id", o.Instance),
		),
	)
	if err != nil && res == nil {
		return Disabled(), err
	}

	var (
		tp        trace.TracerProvider = tracenoop.NewTracerProvider()
		mp        metric.MeterProvider = metricnoop.NewMeterProvider()
		shutdowns []func(context.Context) error
	)
	if tracesTo != "" {
		var opts []otlptracegrpc.Option
		if strings.Contains(tracesTo, "://") {
			opts = append(opts, otlptracegrpc.WithEndpointURL(tracesTo))
		} else {
			opts = append(opts, otlptracegrpc.WithEndpoint(tracesTo))
		}
		exp, err := otlptracegrpc.New(ctx, opts...)
		if err != nil {
			return Disabled(), err
		}
		// The batcher queues spans and drops them when the queue is full;
		// ending a span never waits for the collector.
		sdk := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
		tp, shutdowns = sdk, append(shutdowns, sdk.Shutdown)
	}
	if metricsTo != "" {
		var opts []otlpmetricgrpc.Option
		if strings.Contains(metricsTo, "://") {
			opts = append(opts, otlpmetricgrpc.WithEndpointURL(metricsTo))
		} else {
			opts = append(opts, otlpmetricgrpc.WithEndpoint(metricsTo))
		}
		exp, err := otlpmetricgrpc.New(ctx, opts...)
		if err != nil {
			for _, stop := range shutdowns {
				_ = stop(ctx)
			}
			return Disabled(), err
		}
		sdk := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)), sdkmetric.WithResource(res))
		mp, shutdowns = sdk, append(shutdowns, sdk.Shutdown)
	}

	p := newProviders(tp, mp, o.Log, true)
	p.shutdowns = shutdowns
	if o.ShutdownTimeout > 0 {
		p.shutdownTimeout = o.ShutdownTimeout
	}
	if o.ErrorInterval > 0 {
		p.limit = newLimiter(o.ErrorInterval)
	}

	// The SDK reports export failures here, from its own goroutines.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(p.exportError))
	otel.SetLogger(logr.FromSlogHandler(&limitedHandler{next: p.log.Handler(), limit: p.limit}))
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(p.prop)
	return p, nil
}

// exportError logs a problem reported by the SDK, at most once per interval.
func (p *Providers) exportError(err error) {
	if err == nil {
		return
	}
	if ok, suppressed := p.limit.allow(); ok {
		p.log.Warn("telemetry could not be exported; the work itself is not affected",
			"error", err.Error(), "suppressed", suppressed)
	}
}

// Shutdown flushes what is queued and stops the exporters. It returns within
// the shutdown timeout whether or not the collector answers, and it works
// with a context that is already cancelled, as a process's is when it stops.
func (p *Providers) Shutdown(ctx context.Context) {
	if p == nil || len(p.shutdowns) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.shutdownTimeout)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for _, stop := range p.shutdowns {
			wg.Go(func() {
				if err := stop(ctx); err != nil {
					p.log.Debug("telemetry shutdown was incomplete", "error", err.Error())
				}
			})
		}
		wg.Wait()
	}()
	select {
	case <-done:
	case <-ctx.Done():
		p.log.Debug("telemetry shutdown timed out; unsent telemetry was dropped")
	}
}

// limiter allows one event per interval and counts the ones in between.
type limiter struct {
	mu         sync.Mutex
	every      time.Duration
	last       time.Time
	suppressed int
	now        func() time.Time
}

func newLimiter(every time.Duration) *limiter { return &limiter{every: every, now: time.Now} }

// allow reports whether an event may be logged now and, if so, how many
// were suppressed since the last one that was.
func (l *limiter) allow() (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if !l.last.IsZero() && now.Sub(l.last) < l.every {
		l.suppressed++
		return false, 0
	}
	n := l.suppressed
	l.last, l.suppressed = now, 0
	return true, n
}

// limitedHandler carries the SDK's own diagnostics into slog under the same
// rate limit as export errors, and drops anything below a warning.
type limitedHandler struct {
	next  slog.Handler
	limit *limiter
}

func (h *limitedHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelWarn && h.next.Enabled(ctx, l)
}

func (h *limitedHandler) Handle(ctx context.Context, r slog.Record) error {
	if ok, _ := h.limit.allow(); !ok {
		return nil
	}
	return h.next.Handle(ctx, r)
}

func (h *limitedHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return &limitedHandler{next: h.next.WithAttrs(a), limit: h.limit}
}

func (h *limitedHandler) WithGroup(name string) slog.Handler {
	return &limitedHandler{next: h.next.WithGroup(name), limit: h.limit}
}
