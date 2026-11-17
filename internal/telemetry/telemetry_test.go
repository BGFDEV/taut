package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/sidecar/wire"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/worker"
)

// harness is Providers over in-memory exporters.
type harness struct {
	*Providers
	spans  *tracetest.SpanRecorder
	reader *sdkmetric.ManualReader
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{spans: tracetest.NewSpanRecorder(), reader: sdkmetric.NewManualReader()}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(h.spans))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(h.reader))
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		_ = mp.Shutdown(context.Background())
	})
	h.Providers = newProviders(tp, mp, nil, true)
	return h
}

func (h *harness) collect(t *testing.T) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	return rm
}

// sum returns the value of an int64 sum or gauge data point with exactly
// these attributes, and whether it exists.
func sum(rm metricdata.ResourceMetrics, name string, attrs ...attribute.KeyValue) (int64, bool) {
	want := attribute.NewSet(attrs...)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			var points []metricdata.DataPoint[int64]
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				points = d.DataPoints
			case metricdata.Gauge[int64]:
				points = d.DataPoints
			}
			for _, p := range points {
				if p.Attributes.Equals(&want) {
					return p.Value, true
				}
			}
		}
	}
	return 0, false
}

func histogramCount(rm metricdata.ResourceMetrics, name string, attrs ...attribute.KeyValue) uint64 {
	want := attribute.NewSet(attrs...)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if d, ok := m.Data.(metricdata.Histogram[float64]); ok && m.Name == name {
				for _, p := range d.DataPoints {
					if p.Attributes.Equals(&want) {
						return p.Count
					}
				}
			}
		}
	}
	return 0
}

// dumpSpans renders everything a span exports as text.
func dumpSpans(spans []sdktrace.ReadOnlySpan) string {
	var b strings.Builder
	for _, s := range spans {
		fmt.Fprintf(&b, "span %q status=%v %q\n", s.Name(), s.Status().Code, s.Status().Description)
		for _, a := range s.Attributes() {
			fmt.Fprintf(&b, "  %s=%s\n", a.Key, a.Value.Emit())
		}
		for _, e := range s.Events() {
			fmt.Fprintf(&b, "  event %q\n", e.Name)
			for _, a := range e.Attributes {
				fmt.Fprintf(&b, "    %s=%s\n", a.Key, a.Value.Emit())
			}
		}
	}
	return b.String()
}

func dumpMetrics(t *testing.T, rm metricdata.ResourceMetrics) string {
	t.Helper()
	raw, err := json.Marshal(rm)
	if err != nil {
		t.Fatalf("marshal metrics: %v", err)
	}
	return string(raw)
}

func spanNamed(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	t.Fatalf("no span named %q in:\n%s", name, dumpSpans(spans))
	return nil
}

func attr(s sdktrace.ReadOnlySpan, key string) string {
	for _, a := range s.Attributes() {
		if string(a.Key) == key {
			return a.Value.Emit()
		}
	}
	return ""
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type marker struct{ http.Handler }

func TestDisabledDoesNothing(t *testing.T) {
	for name, vars := range map[string]map[string]string{
		"no endpoint":  {},
		"sdk disabled": {"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4317", "OTEL_SDK_DISABLED": "true"},
		"exporters none": {
			"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4317", "OTEL_TRACES_EXPORTER": "none", "OTEL_METRICS_EXPORTER": "none",
		},
	} {
		t.Run(name, func(t *testing.T) {
			before := runtime.NumGoroutine()
			p, err := Setup(context.Background(), Options{Service: "worker", Getenv: env(vars)})
			if err != nil {
				t.Fatal(err)
			}
			if p.Enabled() {
				t.Fatal("telemetry is enabled")
			}
			if len(p.shutdowns) != 0 {
				t.Fatalf("an exporter was created: %d shutdown hooks", len(p.shutdowns))
			}
			if _, ok := p.TracerProvider().(*sdktrace.TracerProvider); ok {
				t.Fatal("a real tracer provider was created")
			}
			if _, ok := p.MeterProvider().(*sdkmetric.MeterProvider); ok {
				t.Fatal("a real meter provider was created")
			}
			if after := runtime.NumGoroutine(); after > before {
				t.Fatalf("goroutines went from %d to %d", before, after)
			}

			// Every hook hands back what it was given.
			base := &http.Transport{}
			if got := p.Transport(base); got != http.RoundTripper(base) {
				t.Fatal("Transport wrapped the base transport")
			}
			if got := p.EgressTransport(base); got != http.RoundTripper(base) {
				t.Fatal("EgressTransport wrapped the base transport")
			}
			h := &marker{http.NotFoundHandler()}
			if got := p.HTTPMiddleware("controller")(h); got != http.Handler(h) {
				t.Fatal("HTTPMiddleware wrapped the handler")
			}
			if _, ok := p.Observer().(nopObserver); !ok {
				t.Fatalf("Observer is %T", p.Observer())
			}
			ctx, done := p.Observer().Batch(context.Background(), worker.BatchInfo{RunID: "r"})
			if trace.SpanContextFromContext(ctx).IsValid() {
				t.Fatal("a span was started")
			}
			done(errors.New("x"))
			p.Observer().Count(ctx, "load", worker.EventPostImported, 1)
			called := false
			if err := p.RegisterRunStats(func(context.Context) ([]RunStat, error) { called = true; return nil, nil }); err != nil {
				t.Fatal(err)
			}
			p.Shutdown(context.Background())
			if called {
				t.Fatal("the run statistics callback ran")
			}
		})
	}
}

// A collector that accepts connections and never answers is the worst case:
// nothing fails fast.
func TestUnreachableCollectorNeverBlocks(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var held []net.Conn
	var mu sync.Mutex
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	}()

	logs := &lockedBuffer{}
	start := time.Now()
	p, err := Setup(context.Background(), Options{
		Service: "worker", Version: "test", Instance: "holder-1",
		Getenv:          env(map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://" + ln.Addr().String()}),
		Log:             slog.New(slog.NewTextHandler(logs, nil)),
		ShutdownTimeout: 400 * time.Millisecond,
		ErrorInterval:   time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Setup took %v with an unresponsive collector", d)
	}
	if !p.Enabled() {
		t.Fatal("telemetry is not enabled")
	}

	// Work is never held up, however much of it there is.
	start = time.Now()
	obs := p.Observer()
	for i := range 5000 {
		ctx, done := obs.Batch(context.Background(), worker.BatchInfo{RunID: "r", Lane: "l", Stage: "load", Seq: i})
		obs.Count(ctx, "load", worker.EventPostImported, 3)
		done(nil)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("5000 batches took %v with an unresponsive collector", d)
	}

	// A burst of export failures is one log line.
	for range 50 {
		p.exportError(errors.New("traces export: context deadline exceeded"))
	}
	if n := strings.Count(logs.String(), "telemetry could not be exported"); n != 1 {
		t.Fatalf("export failures logged %d times, want 1:\n%s", n, logs.String())
	}

	// Shutdown works with a context that is already cancelled, and is bounded.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start = time.Now()
	p.Shutdown(ctx)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Shutdown took %v, want about 400ms", d)
	}
}

func TestLimiter(t *testing.T) {
	now := time.Unix(0, 0)
	l := newLimiter(time.Minute)
	l.now = func() time.Time { return now }
	if ok, _ := l.allow(); !ok {
		t.Fatal("first event suppressed")
	}
	for range 3 {
		if ok, _ := l.allow(); ok {
			t.Fatal("event allowed within the interval")
		}
	}
	now = now.Add(time.Minute)
	if ok, n := l.allow(); !ok || n != 3 {
		t.Fatalf("allow = %v, %d; want true, 3", ok, n)
	}
}

func TestObserverSpansAndMetrics(t *testing.T) {
	h := newHarness(t)
	obs := h.Observer()
	info := worker.BatchInfo{RunID: "run-01J", Lane: "load/C024BE91L", Stage: "load", Seq: 7, Holder: "host-12-ab", Attempt: 2}

	ctx, done := obs.Batch(context.Background(), info)
	obs.Count(ctx, "load", worker.EventPostImported, 40)
	obs.Count(ctx, "load", worker.EventPostAdopted, 2)
	obs.Count(ctx, "load", worker.EventPostRejected, 1)
	obs.Count(ctx, "load", worker.EventPostHeld, 3)
	obs.Count(ctx, "files", worker.EventFileUploaded, 5)
	obs.Count(ctx, "files", worker.EventFileSkipped, 1)
	obs.Count(ctx, "files", worker.EventFileFailed, 1)
	obs.Count(ctx, "files", worker.EventFileBytes, 1<<20)
	obs.Count(ctx, "load", worker.EventLeaseLost, 1)
	obs.Count(ctx, "load", worker.EventBatchRetried, 1)
	obs.Count(ctx, "finalize", worker.EventChannelFinalized, 1)
	obs.Count(ctx, "team", worker.EventMemberAdded, 9)
	obs.Count(ctx, "load", worker.Event("made.up"), 1) // ignored
	obs.Count(ctx, "load", worker.EventPostImported, 0)
	obs.Count(ctx, "load", worker.EventPostImported, -4)
	done(nil)

	_, done = obs.Batch(context.Background(), info)
	done(fail.Wrap(errors.New("boom"), "graph.throttled", "Graph is throttling", "", ""))
	_, done = obs.Batch(context.Background(), info)
	done(fmt.Errorf("renew: %w", store.ErrFenced))
	_, done = obs.Batch(context.Background(), info)
	done(context.Canceled)

	spans := h.spans.Ended()
	if len(spans) != 4 {
		t.Fatalf("got %d spans, want 4", len(spans))
	}
	s := spans[0]
	if s.Name() != "worker.batch" {
		t.Fatalf("span name %q", s.Name())
	}
	for k, want := range map[string]string{
		"taut.run": "run-01J", "taut.lane": "load/C024BE91L", "taut.stage": "load", "taut.batch.seq": "7",
		"taut.attempt": "2", "taut.holder": "host-12-ab", "taut.outcome": "ok",
	} {
		if got := attr(s, k); got != want {
			t.Errorf("span attribute %s = %q, want %q", k, got, want)
		}
	}
	if s.Status().Code != codes.Unset {
		t.Errorf("successful batch has status %v", s.Status().Code)
	}
	if st := spans[1].Status(); st.Code != codes.Error || st.Description != "graph.throttled" {
		t.Errorf("failed batch status = %v %q", st.Code, st.Description)
	}
	if got := attr(spans[2], "taut.outcome"); got != "lease_lost" {
		t.Errorf("fenced batch outcome %q", got)
	}
	if st := spans[3].Status(); st.Code != codes.Unset || attr(spans[3], "taut.outcome") != "canceled" {
		t.Errorf("cancelled batch: status %v outcome %q", st.Code, attr(spans[3], "taut.outcome"))
	}

	rm := h.collect(t)
	stage := func(s string) attribute.KeyValue { return attribute.String("taut.stage", s) }
	outcome := func(s string) attribute.KeyValue { return attribute.String("outcome", s) }
	for _, c := range []struct {
		name  string
		attrs []attribute.KeyValue
		want  int64
	}{
		{"taut.posts", []attribute.KeyValue{stage("load"), outcome("imported")}, 40},
		{"taut.posts", []attribute.KeyValue{stage("load"), outcome("adopted")}, 2},
		{"taut.posts", []attribute.KeyValue{stage("load"), outcome("rejected")}, 1},
		{"taut.posts", []attribute.KeyValue{stage("load"), outcome("held")}, 3},
		{"taut.files", []attribute.KeyValue{stage("files"), outcome("uploaded")}, 5},
		{"taut.files", []attribute.KeyValue{stage("files"), outcome("skipped")}, 1},
		{"taut.files", []attribute.KeyValue{stage("files"), outcome("failed")}, 1},
		{"taut.file.bytes", []attribute.KeyValue{stage("files")}, 1 << 20},
		{"taut.lease.lost", []attribute.KeyValue{stage("load")}, 1},
		{"taut.batch.retries", []attribute.KeyValue{stage("load")}, 1},
		{"taut.channels.finalized", []attribute.KeyValue{stage("finalize")}, 1},
		{"taut.members.added", []attribute.KeyValue{stage("team")}, 9},
	} {
		if got, ok := sum(rm, c.name, c.attrs...); !ok || got != c.want {
			t.Errorf("%s %v = %d (found %v), want %d", c.name, c.attrs, got, ok, c.want)
		}
	}
	for o, want := range map[string]uint64{"ok": 1, "error": 1, "lease_lost": 1, "canceled": 1} {
		if got := histogramCount(rm, "taut.batch.duration", stage("load"), outcome(o)); got != want {
			t.Errorf("taut.batch.duration outcome=%s count = %d, want %d", o, got, want)
		}
	}
	// Identifiers are for spans. None may reach a metric.
	dump := dumpMetrics(t, rm)
	for _, id := range []string{"run-01J", "C024BE91L", "host-12-ab"} {
		if strings.Contains(dump, id) {
			t.Errorf("metrics contain the identifier %q", id)
		}
	}
}

// A worker's request to a server carries the batch's trace: batch →
// client → server, one trace.
func TestTracePropagation(t *testing.T) {
	h := newHarness(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !trace.SpanContextFromContext(r.Context()).IsValid() {
			t.Error("the handler's context has no span")
		}
		_, _ = io.WriteString(w, "ok")
	})
	srv := httptest.NewServer(h.HTTPMiddleware("controller")(mux))
	defer srv.Close()

	ctx, done := h.Observer().Batch(context.Background(), worker.BatchInfo{RunID: "r1", Stage: "load"})
	client := &http.Client{Transport: h.Transport(nil)}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/runs/run-777?name=acme", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	done(nil)
	// A health probe is measured but not traced.
	if resp, err := http.Get(srv.URL + "/healthz"); err == nil {
		_ = resp.Body.Close()
	}

	spans := h.spans.Ended()
	if len(spans) != 3 {
		t.Fatalf("got %d spans, want 3:\n%s", len(spans), dumpSpans(spans))
	}
	batch, cl, sv := spanNamed(t, spans, "worker.batch"), spanNamed(t, spans, "GET"), spanNamed(t, spans, "GET /v1/runs/{id}")
	tid := batch.SpanContext().TraceID()
	if cl.SpanContext().TraceID() != tid || sv.SpanContext().TraceID() != tid {
		t.Fatal("the spans are not one trace")
	}
	if cl.Parent().SpanID() != batch.SpanContext().SpanID() {
		t.Error("the client span is not a child of the batch span")
	}
	if sv.Parent().SpanID() != cl.SpanContext().SpanID() || !sv.Parent().IsRemote() {
		t.Error("the server span is not a remote child of the client span")
	}
	if cl.SpanKind() != trace.SpanKindClient || sv.SpanKind() != trace.SpanKindServer {
		t.Errorf("span kinds: client %v, server %v", cl.SpanKind(), sv.SpanKind())
	}
	if got := attr(sv, "http.route"); got != "/v1/runs/{id}" {
		t.Errorf("http.route = %q", got)
	}

	rm := h.collect(t)
	if n := histogramCount(rm, "http.server.request.duration", attribute.String("taut.server", "controller"),
		attribute.String("http.request.method", "GET"), attribute.String("http.route", "/v1/runs/{id}"),
		attribute.Int("http.response.status_code", 200)); n != 1 {
		t.Errorf("http.server.request.duration count = %d, want 1", n)
	}
	if dump := dumpSpans(spans) + dumpMetrics(t, rm); strings.Contains(dump, "run-777") || strings.Contains(dump, "acme") {
		t.Errorf("a raw path or query was recorded:\n%s", dump)
	}
}

// The egress proxy continues the worker's trace from the headers the proxy
// copies, and sends no trace headers on to the upstream.
func TestEgressContinuesTraceAndStripsHeaders(t *testing.T) {
	h := newHarness(t)
	var upstreamHeader http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHeader = r.Header.Clone()
		w.WriteHeader(http.StatusCreated)
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	proxy := httptest.NewServer(&httputil.ReverseProxy{
		Rewrite:   func(pr *httputil.ProxyRequest) { pr.SetURL(target) },
		Transport: h.EgressTransport(nil),
	})
	defer proxy.Close()

	ctx, done := h.Observer().Batch(context.Background(), worker.BatchInfo{RunID: "r1", Stage: "load"})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, proxy.URL+"/v1.0/teams/T1/channels/C1/messages", strings.NewReader("{}"))
	req.Header.Set(wire.HeaderUpstream, "https://graph.microsoft.com")
	req.Header.Set("Baggage", "user=alice")
	resp, err := (&http.Client{Transport: h.Transport(nil)}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	done(nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if req.Header.Get("Traceparent") != "" {
		t.Error("the transport changed the caller's request")
	}
	for _, name := range []string{"Traceparent", "Tracestate", "Baggage"} {
		if v := upstreamHeader.Get(name); v != "" {
			t.Errorf("the upstream received %s: %s", name, v)
		}
	}

	spans := h.spans.Ended()
	batch, cl, fwd := spanNamed(t, spans, "worker.batch"), spanNamed(t, spans, "POST"), spanNamed(t, spans, "proxy.forward")
	if fwd.SpanContext().TraceID() != batch.SpanContext().TraceID() {
		t.Fatal("the egress proxy's span is in another trace")
	}
	if fwd.Parent().SpanID() != cl.SpanContext().SpanID() || !fwd.Parent().IsRemote() {
		t.Error("the egress proxy's span is not a remote child of the worker's client span")
	}
	if got := attr(cl, "taut.route.class"); got != "graph.messages" {
		t.Errorf("client route class %q", got)
	}
	if got := attr(cl, "taut.upstream"); got != "https://graph.microsoft.com" {
		t.Errorf("taut.upstream = %q", got)
	}
	if got := attr(fwd, "http.response.status_code"); got != "201" {
		t.Errorf("forward status %q", got)
	}
}

func TestRouteClass(t *testing.T) {
	for _, c := range []struct{ host, path, want string }{
		{"graph.microsoft.com", "/v1.0/teams/abc/channels/19:x@thread.tacv2/messages", "graph.messages"},
		{"graph.microsoft.us:443", "/v1.0/teams/abc/channels/def/members", "graph.members"},
		{"graph.microsoft.com", "/v1.0/teams/abc/channels/def/completeMigration", "graph.complete"},
		{"graph.microsoft.com", "/v1.0/drives/b!x/items/01ABC:/a.png:/createUploadSession", "graph.files"},
		{"graph.microsoft.com", "/v1.0/$batch", "graph"},
		{"graph.microsoft.com", "/v1.0/chats", "graph.chats"},
		{"graph.microsoft.com", "/v1.0/chats/19:abc_def@unq.gbl.spaces/messages", "graph.chats"},
		{"graph.microsoft.com", "/v1.0/chats/19:abc@thread.v2/messages/1700000000000", "graph.chats"},
		{"graph.microsoft.com", "/v1.0/chats/19:abc@thread.v2/startMigration", "graph.chats"},
		{"graph.microsoft.com", "/v1.0/chats/19:abc@thread.v2/completeMigration", "graph.chats"},
		{"graph.microsoft.com", "/v1.0/users/5f0a6f5c-0000-4000-8000-000000000001/chats", "graph.chats"},
		{"fakegraph:8080", "/v1.0/chats/19:abc@thread.v2/messages", "graph.chats"},
		{"contoso.sharepoint.com", "/sites/x/_api/v2.0/drive/items/1/uploadSession?guid=abc&tempauth=SECRET", "sharepoint"},
		{"files.slack.com", "/files-pri/T1-F1/a.png", "slack.files"},
		{"slack.com", "/api/users.list", "slack.api"},
		{"login.microsoftonline.com", "/tid/oauth2/v2.0/token", "entra"},
		{"fakegraph:8080", "/v1.0/teams", "graph.teams"},
		{"fakegraph:8080", "/files-pri/T1-F1/a.png", "slack.files"},
		{"127.0.0.1:8900", "/state", "other"},
		{"[::1]:8900", "/upload/abc", "other"},
	} {
		if got := routeClass(c.host, c.path); got != c.want {
			t.Errorf("routeClass(%q, %q) = %q, want %q", c.host, c.path, got, c.want)
		}
	}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, &url.Error{Op: r.Method, URL: r.URL.String(), Err: errors.New("connection refused")}
}

// Nothing secret or personal may reach a span or a metric, whatever is fed
// in.
func TestNoSecretsInTelemetry(t *testing.T) {
	const (
		bearer      = "eyJBEARERSECRET"
		tautAuth    = "TAUTAUTHSECRET"
		slackToken  = "xoxe-1-SLACKSECRET"
		sessionURL  = "UPLOADSESSIONSECRET"
		tempAuth    = "TEMPAUTHSECRET"
		email       = "alice@example.com"
		displayName = "Alice Liddell"
		text        = "the launch is on friday"
		cookie      = "COOKIESECRET"
		queryName   = "acme-migration"
		pathID      = "run-PATHSECRET"
	)
	forbidden := []string{bearer, tautAuth, slackToken, sessionURL, tempAuth, email, displayName, text, cookie, queryName, pathID, "?", "&", "tempauth"}

	h := newHarness(t)
	obs := h.Observer()

	// A server that sees secrets in headers, path and query.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/runs/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("PUT /upload/{rest...}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })
	mux.HandleFunc("GET /files-pri/{rest...}", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, text) })
	mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })
	srv := httptest.NewServer(h.HTTPMiddleware("controller")(mux))
	defer srv.Close()
	client := &http.Client{Transport: h.Transport(nil)}

	// A batch whose error quotes everything it should not.
	ctx, done := obs.Batch(context.Background(), worker.BatchInfo{
		RunID: "run-1", Lane: "load/C1", Stage: "load", Seq: 1, Holder: "h-1", Attempt: 1,
	})
	// Identifiers and text passed where a stage name belongs.
	obs.Count(ctx, email, worker.EventPostImported, 1)
	obs.Count(ctx, "C024BE91L", worker.EventMemberAdded, 1)
	obs.Count(ctx, text, worker.EventFileBytes, 10)
	obs.Count(ctx, "load", worker.Event(slackToken), 1)

	do := func(method, target string, hdr map[string]string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set(wire.HeaderAuth, tautAuth)
		req.Header.Set("Cookie", "d="+cookie)
		req.Header.Set("User-Agent", displayName)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	// A Slack file URL with its token, named to the egress proxy with a full
	// URL where only an origin belongs.
	do(http.MethodGet, srv.URL+"/files-pri/T1-F1/"+url.PathEscape(displayName)+".png?t="+slackToken,
		map[string]string{wire.HeaderUpstream: "https://files.slack.com/files-pri/T1-F1/x.png?t=" + slackToken})
	// A pre-authorized upload session.
	do(http.MethodPut, srv.URL+"/upload/"+sessionURL+"?tempauth="+tempAuth,
		map[string]string{wire.HeaderUpstream: "https://" + "user:" + bearer + "@contoso.sharepoint.com"})
	// The controller's API with a name in the query and an ID in the path.
	do(http.MethodGet, srv.URL+"/v1/runs/"+pathID+"?name="+queryName+"&email="+url.QueryEscape(email), nil)
	// No route, a handler that aborts, a strange method.
	do(http.MethodGet, srv.URL+"/nowhere/"+pathID+"?t="+slackToken, nil)
	do(http.MethodGet, srv.URL+"/boom?t="+slackToken, nil)
	do("X"+strings.ToUpper(strings.ReplaceAll(queryName, "-", "")), srv.URL+"/v1/runs/"+pathID, nil)
	// A transport error, whose text names the full URL.
	failing := &http.Client{Transport: h.Transport(failingTransport{})}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://files.slack.com/files-pri/T1-F1/a.png?t="+slackToken, nil)
	if _, err := failing.Do(req); err == nil {
		t.Fatal("the failing transport succeeded")
	}
	// The egress proxy's side of the same.
	egress := &http.Client{Transport: h.EgressTransport(failingTransport{})}
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, "https://files.slack.com/files-pri/T1-F1/a.png?t="+slackToken, nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	if _, err := egress.Do(req); err == nil {
		t.Fatal("the failing transport succeeded")
	}

	done(fmt.Errorf("posting %q by %s <%s> failed: GET https://files.slack.com/f?t=%s: Authorization: Bearer %s",
		text, displayName, email, slackToken, bearer))
	_, done = obs.Batch(context.Background(), worker.BatchInfo{RunID: "run-1", Stage: text})
	done(fail.Wrap(errors.New(text), "Bearer "+bearer, text, email, displayName)) // a code that is not a code
	_, done = obs.Batch(context.Background(), worker.BatchInfo{RunID: "run-1", Stage: "load"})
	done(fail.New("graph.rejected", text, email, displayName))

	// Run statistics with data where labels belong.
	if err := h.RegisterRunStats(func(context.Context) ([]RunStat, error) {
		return []RunStat{{Posted: 1, Lanes: []LaneStat{{Stage: email, Status: text, Count: 1}}}}, nil
	}); err != nil {
		t.Fatal(err)
	}

	spans := h.spans.Ended()
	if len(spans) < 12 {
		t.Fatalf("only %d spans were recorded; the test did not exercise the instrumentation:\n%s", len(spans), dumpSpans(spans))
	}
	rm := h.collect(t)
	spanDump, metricDump := dumpSpans(spans), dumpMetrics(t, rm)
	for _, s := range forbidden {
		if strings.Contains(spanDump, s) {
			t.Errorf("spans contain %q:\n%s", s, spanDump)
		}
		if len(s) > 2 && strings.Contains(metricDump, s) {
			t.Errorf("metrics contain %q:\n%s", s, metricDump)
		}
	}
	// Metrics carry no identifiers at all.
	for _, id := range []string{"run-1", "load/C1", "h-1", "C024BE91L", "T1-F1", "contoso"} {
		if strings.Contains(metricDump, id) {
			t.Errorf("metrics contain the identifier %q", id)
		}
	}
	// What is recorded is the useful, harmless part.
	if !strings.Contains(spanDump, "taut.upstream=https://files.slack.com\n") {
		t.Errorf("the upstream origin is missing from the spans:\n%s", spanDump)
	}
	if !strings.Contains(spanDump, "taut.upstream=https://contoso.sharepoint.com\n") {
		t.Errorf("the SharePoint origin is missing or carries credentials:\n%s", spanDump)
	}
	if !strings.Contains(spanDump, `"graph.rejected"`) {
		t.Errorf("the error class is missing from the spans:\n%s", spanDump)
	}
}

func TestRunStats(t *testing.T) {
	h := newHarness(t)
	fail := false
	err := h.RegisterRunStats(func(context.Context) ([]RunStat, error) {
		if fail {
			return nil, errors.New("database is away")
		}
		return []RunStat{
			{Posted: 10, Failed: 1, Held: 2, Pending: 7, Lanes: []LaneStat{{"load", "open", 3}, {"load", "done", 1}}},
			{Posted: 5, Pending: 5, Lanes: []LaneStat{{"load", "open", 2}, {"files", "failed", 1}}},
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	rm := h.collect(t)
	state := func(s string) attribute.KeyValue { return attribute.String("state", s) }
	lane := func(stage, status string) []attribute.KeyValue {
		return []attribute.KeyValue{attribute.String("taut.stage", stage), attribute.String("status", status)}
	}
	for _, c := range []struct {
		name  string
		attrs []attribute.KeyValue
		want  int64
	}{
		{"taut.runs.active", nil, 2},
		{"taut.run.posts", []attribute.KeyValue{state("posted")}, 15},
		{"taut.run.posts", []attribute.KeyValue{state("failed")}, 1},
		{"taut.run.posts", []attribute.KeyValue{state("held")}, 2},
		{"taut.run.posts", []attribute.KeyValue{state("pending")}, 12},
		{"taut.run.lanes", lane("load", "open"), 5},
		{"taut.run.lanes", lane("load", "done"), 1},
		{"taut.run.lanes", lane("files", "failed"), 1},
	} {
		if got, ok := sum(rm, c.name, c.attrs...); !ok || got != c.want {
			t.Errorf("%s %v = %d (found %v), want %d", c.name, c.attrs, got, ok, c.want)
		}
	}

	// A failing callback is swallowed: the collection succeeds without the
	// run gauges.
	fail = true
	rm = h.collect(t)
	if _, ok := sum(rm, "taut.runs.active"); ok {
		t.Error("a failed read still reported taut.runs.active")
	}
}
