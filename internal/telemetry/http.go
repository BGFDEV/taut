package telemetry

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/bgfdev/taut/internal/sidecar/wire"
)

// This file instruments HTTP by hand rather than with otelhttp. The
// standard instrumentation records the full request URL on client spans,
// and here a URL can be a secret: a Slack file link may carry a token in its
// query and an upload-session URL is its own authorization. What is recorded
// below is a method, a status, a scheme and host, and a route from a fixed
// list or a registered pattern.

type httpInstruments struct {
	server metric.Float64Histogram
	client metric.Float64Histogram
}

func newHTTPInstruments(p *Providers) *httpInstruments {
	h := &httpInstruments{}
	// The SDK's default boundaries are meant for milliseconds (0, 5, 10, 25
	// and so on): with seconds as the unit every request lands in the first
	// bucket and no percentile can be read off. These are the ones the HTTP
	// semantic conventions advise, with two finer ones below and two
	// coarser ones above for the waits a rate limit causes.
	buckets := metric.WithExplicitBucketBoundaries(0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10, 30, 60)
	h.server, _ = p.meter.Float64Histogram("http.server.request.duration",
		metric.WithUnit("s"), metric.WithDescription("Duration of HTTP requests served."), buckets)
	h.client, _ = p.meter.Float64Histogram("http.client.request.duration",
		metric.WithUnit("s"), metric.WithDescription("Duration of outbound HTTP requests, up to the response headers."), buckets)
	return h
}

// methodLabel returns the request method if it is a standard one, and
// "_OTHER" otherwise.
func methodLabel(m string) string {
	switch m {
	case "":
		return http.MethodGet
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace:
		return m
	}
	return "_OTHER"
}

// --- server ----------------------------------------------------------------------

// HTTPMiddleware returns middleware that continues the caller's trace,
// records a server span named after the matched route pattern (for example
// "GET /v1/runs/{id}") and measures http.server.request.duration by method,
// route and status. name identifies the server in both. The raw path and
// query are never recorded. Health probes are measured but not traced.
//
// When telemetry is off it returns the handler unchanged.
func (p *Providers) HTTPMiddleware(name string) func(http.Handler) http.Handler {
	if !p.Enabled() {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			method := methodLabel(r.Method)
			ctx := r.Context()
			var span trace.Span
			if r.URL.Path != "/healthz" && r.URL.Path != "/readyz" {
				ctx = p.prop.Extract(ctx, propagation.HeaderCarrier(r.Header))
				ctx, span = p.tracer.Start(ctx, method, trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(
					attribute.String("taut.server", name),
					attribute.String("http.request.method", method),
				))
			}
			// The mux records the pattern it matched on the request it is
			// given, so keep hold of that request.
			r = r.WithContext(ctx)
			rec := &statusRecorder{ResponseWriter: w}
			defer func() {
				route := routeOf(r)
				status := rec.Status()
				if v := recover(); v != nil {
					status = http.StatusInternalServerError
					defer panic(v)
				}
				if span != nil {
					span.SetName(method + " " + route)
					span.SetAttributes(attribute.String("http.route", route), attribute.Int("http.response.status_code", status))
					if status >= 500 {
						span.SetStatus(codes.Error, strconv.Itoa(status))
					}
					span.End()
				}
				p.http.server.Record(context.WithoutCancel(ctx), time.Since(start).Seconds(), metric.WithAttributes(
					attribute.String("taut.server", name),
					attribute.String("http.request.method", method),
					attribute.String("http.route", route),
					attribute.Int("http.response.status_code", status),
				))
			}()
			next.ServeHTTP(rec, r)
		})
	}
}

// routeOf returns the pattern the mux matched, without its method, or
// "unmatched". It is never the request's own path.
func routeOf(r *http.Request) string {
	pat := r.Pattern
	if i := strings.IndexByte(pat, ' '); i >= 0 {
		pat = strings.TrimSpace(pat[i+1:])
	}
	if pat == "" {
		return "unmatched"
	}
	return pat
}

// statusRecorder remembers the status a handler wrote.
type statusRecorder struct {
	http.ResponseWriter
	mu     sync.Mutex
	status int
}

func (s *statusRecorder) note(code int) {
	s.mu.Lock()
	if s.status == 0 {
		s.status = code
	}
	s.mu.Unlock()
}

func (s *statusRecorder) Status() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == 0 {
		return http.StatusOK
	}
	return s.status
}

func (s *statusRecorder) WriteHeader(code int) {
	s.note(code)
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.note(http.StatusOK)
	return s.ResponseWriter.Write(b)
}

// Flush keeps streaming responses working through the wrapper.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// --- client ----------------------------------------------------------------------

// Transport wraps base (nil means http.DefaultTransport) for a client inside
// Taut: each request gets a client span and carries the trace context to the
// server, which is how a worker's request to its egress proxy joins the batch's
// trace. When telemetry is off it returns base unchanged.
func (p *Providers) Transport(base http.RoundTripper) http.RoundTripper {
	return p.transport(base, false)
}

// EgressTransport wraps base for the egress proxy's requests to Microsoft and
// Slack. It continues the trace named by the incoming request's headers
// (the proxy copies them onto the outgoing request), records a
// "proxy.forward" span, and removes the trace headers so that nothing
// about Taut's traces is sent to a third party. When telemetry is off it
// returns base unchanged.
func (p *Providers) EgressTransport(base http.RoundTripper) http.RoundTripper {
	return p.transport(base, true)
}

func (p *Providers) transport(base http.RoundTripper, egress bool) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if !p.Enabled() {
		return base
	}
	return &transport{p: p, base: base, egress: egress}
}

type transport struct {
	p      *Providers
	base   http.RoundTripper
	egress bool
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	ctx := req.Context()
	method := methodLabel(req.Method)
	name := method
	if t.egress {
		ctx = t.p.prop.Extract(ctx, propagation.HeaderCarrier(req.Header))
		name = "proxy.forward"
	}

	// Where the request is really going: a worker names the destination
	// to its egress proxy in a header.
	scheme, host := req.URL.Scheme, req.URL.Host
	attrs := []attribute.KeyValue{
		attribute.String("http.request.method", method),
		attribute.String("url.scheme", scheme),
		attribute.String("server.address", host),
	}
	if up := req.Header.Get(wire.HeaderUpstream); up != "" && !t.egress {
		if u, err := url.Parse(up); err == nil && u.Host != "" {
			host = u.Host
			// Scheme and host only, whatever else the header held.
			attrs = append(attrs, attribute.String("taut.upstream", u.Scheme+"://"+u.Host))
		}
	}
	class := routeClass(host, req.URL.Path)
	attrs = append(attrs, attribute.String("taut.route.class", class))

	ctx, span := t.p.tracer.Start(ctx, name, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	out := req.Clone(ctx) // a RoundTripper must not change the caller's request
	if t.egress {
		for _, h := range []string{"Traceparent", "Tracestate", "Baggage"} {
			out.Header.Del(h)
		}
	} else {
		t.p.prop.Inject(ctx, propagation.HeaderCarrier(out.Header))
	}

	resp, err := t.base.RoundTrip(out)
	mattrs := []attribute.KeyValue{
		attribute.String("http.request.method", method),
		attribute.String("taut.route.class", class),
	}
	if err != nil {
		kind := transportErrorClass(err)
		span.SetAttributes(attribute.String("error.type", kind))
		if kind != "canceled" {
			span.SetStatus(codes.Error, kind) // the kind only: the text names the URL
		}
		span.End()
		t.p.http.client.Record(context.WithoutCancel(ctx), time.Since(start).Seconds(),
			metric.WithAttributes(append(mattrs, attribute.String("error.type", kind))...))
		return nil, err
	}
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	if resp.StatusCode >= 400 {
		span.SetStatus(codes.Error, strconv.Itoa(resp.StatusCode))
	}
	t.p.http.client.Record(context.WithoutCancel(ctx), time.Since(start).Seconds(),
		metric.WithAttributes(append(mattrs, attribute.Int("http.response.status_code", resp.StatusCode))...))
	if resp.Body == nil || resp.Body == http.NoBody || resp.StatusCode == http.StatusSwitchingProtocols {
		span.End()
		return resp, nil
	}
	// The span covers the transfer: it ends when the body is finished with.
	resp.Body = &spanBody{ReadCloser: resp.Body, span: span}
	return resp, nil
}

// spanBody ends a span when a response body is read to its end or closed.
type spanBody struct {
	io.ReadCloser
	span trace.Span
	once sync.Once
}

func (b *spanBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		if !errors.Is(err, io.EOF) {
			kind := transportErrorClass(err)
			b.span.SetAttributes(attribute.String("error.type", kind))
			if kind != "canceled" {
				b.span.SetStatus(codes.Error, kind)
			}
		}
		b.once.Do(func() { b.span.End() })
	}
	return n, err
}

func (b *spanBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() { b.span.End() })
	return err
}

func transportErrorClass(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	if te, ok := errors.AsType[net.Error](err); ok && te.Timeout() {
		return "timeout"
	}
	return "transport"
}

// graphResources maps a Graph path segment to a route class. Only these
// values are ever recorded, so an identifier in a path cannot leak.
var graphResources = map[string]string{
	"messages":            "graph.messages",
	"replies":             "graph.messages",
	"members":             "graph.members",
	"channels":            "graph.channels",
	"teams":               "graph.teams",
	"groups":              "graph.teams",
	"completemigration":   "graph.complete",
	"filesfolder":         "graph.files",
	"createuploadsession": "graph.files",
	"drive":               "graph.files",
	"drives":              "graph.files",
	"items":               "graph.files",
	"users":               "graph.users",
}

// routeClass sorts a request into one of a fixed set of classes by its
// destination host and, for the local fakes, the shape of its path.
func routeClass(host, path string) string {
	h := strings.ToLower(host)
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.HasSuffix(h, "]") {
		h = h[:i]
	}
	class := ""
	switch {
	case h == "files.slack.com" || strings.HasSuffix(h, ".slack-files.com"):
		class = "slack.files"
	case h == "slack.com" || strings.HasSuffix(h, ".slack.com"):
		class = "slack.api"
	case strings.HasPrefix(h, "graph.microsoft.") || strings.HasPrefix(h, "dod-graph.microsoft.") || h == "microsoftgraph.chinacloudapi.cn":
		class = "graph"
	case strings.Contains(h, ".sharepoint.") || strings.Contains(h, ".sharepoint-"):
		class = "sharepoint"
	case strings.HasPrefix(h, "login.microsoftonline.") || h == "login.partner.microsoftonline.cn" || h == "login.chinacloudapi.cn":
		class = "entra"
	case strings.HasPrefix(path, "/v1.0/") || strings.HasPrefix(path, "/beta/"):
		class = "graph"
	case strings.HasPrefix(path, "/api/"):
		class = "slack.api"
	case strings.HasPrefix(path, "/files-pri/"):
		class = "slack.files"
	case strings.Contains(path, "/oauth2/"):
		class = "entra"
	default:
		return "other"
	}
	if class != "graph" {
		return class
	}
	segs := strings.Split(path, "/")
	// Everything about a chat is one class: creating it, its migration
	// mode and the messages imported into it. Chat traffic has its own
	// limits at Microsoft, so it is worth telling from channel traffic,
	// and the class carries no part of the path.
	for _, seg := range segs {
		if strings.EqualFold(seg, "chats") {
			return "graph.chats"
		}
	}
	// The innermost recognised resource names the kind of Graph call.
	for i := len(segs) - 1; i >= 0; i-- {
		if c, ok := graphResources[strings.ToLower(segs[i])]; ok {
			return c
		}
	}
	return class
}
