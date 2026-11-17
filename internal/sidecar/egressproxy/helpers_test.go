package egressproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/secret"
	"github.com/bgfdev/taut/internal/sidecar/budget"
	"github.com/bgfdev/taut/internal/sidecar/tokenbroker"
	"github.com/bgfdev/taut/internal/sidecar/wire"
)

var _ TokenProvider = (*tokenbroker.Client)(nil)

// Canaries that must never reach a log line, an error body or the state.
const (
	canaryEntra   = "CANARY-entra-access-token"
	canarySlack   = "CANARY-slack-token-value"
	canaryInbound = "CANARY-inbound-authorization"
	canaryURL     = "CANARYurltoken"
)

var canaries = []string{canaryEntra, canarySlack, canaryInbound, canaryURL}

// clock is a virtual clock whose Sleep returns at once, having advanced it.
type clock struct {
	mu     sync.Mutex
	t      time.Time
	sleeps int
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func (c *clock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.sleeps++
	c.mu.Unlock()
	return nil
}

// tokens is a fake TokenProvider. Each Invalidate moves to the next
// generation of token unless frozen.
type tokens struct {
	mu          sync.Mutex
	gen         int
	frozen      bool
	err         error
	entraCalls  int
	slackCalls  int
	invalidates int
}

func (p *tokens) suffix() string {
	if p.gen == 0 {
		return ""
	}
	return "-gen" + string(rune('0'+p.gen))
}

func (p *tokens) Entra(context.Context, wire.Entra) (string, time.Time, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entraCalls++
	if p.err != nil {
		return "", time.Time{}, p.err
	}
	return canaryEntra + p.suffix(), time.Now().Add(time.Hour), nil
}

func (p *tokens) Slack(context.Context, secret.Ref) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.slackCalls++
	if p.err != nil {
		return "", p.err
	}
	return canarySlack + p.suffix(), nil
}

func (p *tokens) Invalidate(context.Context, wire.Auth) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.invalidates++
	if !p.frozen {
		p.gen++
	}
	return nil
}

// leases is a fake budget.Source.
type leases struct {
	mu    sync.Mutex
	rps   float64
	ttl   time.Duration
	err   error
	calls int
}

func (l *leases) Lease(context.Context, budget.Key, string) (budget.Lease, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	return budget.Lease{RPS: l.rps, TTL: l.ttl}, l.err
}

func (l *leases) set(rps float64, err error) {
	l.mu.Lock()
	l.rps, l.err = rps, err
	l.mu.Unlock()
}

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// upstream is a fake Graph/Slack server.
type upstream struct {
	*httptest.Server
	mu       sync.Mutex
	requests []*http.Request // bodies already consumed
	bodies   []string
	handler  func(w http.ResponseWriter, r *http.Request, n int)
}

func newUpstream(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, n int)) *upstream {
	t.Helper()
	u := &upstream{handler: handler}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		_, _ = io.Copy(io.Discard, r.Body)
		u.mu.Lock()
		u.requests = append(u.requests, r)
		u.bodies = append(u.bodies, string(body))
		n := len(u.requests)
		h := u.handler
		u.mu.Unlock()
		if h != nil {
			h(w, r, n)
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.requests)
}

func (u *upstream) last() (*http.Request, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.requests[len(u.requests)-1], u.bodies[len(u.bodies)-1]
}

func (u *upstream) setHandler(h func(w http.ResponseWriter, r *http.Request, n int)) {
	u.mu.Lock()
	u.handler = h
	u.mu.Unlock()
}

// harness is an egress proxy with fakes around it.
type harness struct {
	t      *testing.T
	g      *Proxy
	clock  *clock
	tokens *tokens
	leases *leases
	logs   *logBuf
	seen   []string // response bodies and headers the egress proxy produced
}

// newHarness builds an egress proxy. scoped maps scope → upstream allowed under it.
func newHarness(t *testing.T, mutate func(*Config), allow ...string) *harness {
	t.Helper()
	r := &harness{t: t, clock: newClock(), tokens: &tokens{}, leases: &leases{rps: 1000, ttl: time.Hour}, logs: &logBuf{}}
	cfg := Config{
		Holder:     "gov-a",
		Tokens:     r.tokens,
		Budgets:    r.leases,
		AllowHosts: allow,
		// Most tests want rates far above the real budgets.
		MaxRPS: map[string]float64{ScopeGraph: 1e9, ScopeSharePoint: 1e9, ScopeSlack: 1e9},
		Now:    r.clock.Now,
		Sleep:  r.clock.Sleep,
		Jitter: func() float64 { return 0 },
		Log:    slog.New(slog.NewTextHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	g, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	g.refreshInline = true // deterministic under the virtual clock
	r.g = g
	t.Cleanup(func() {
		g.Close()
		state, _ := json.Marshal(g.State())
		all := r.logs.String() + "\n" + string(state) + "\n" + strings.Join(r.seen, "\n")
		for _, c := range canaries {
			if strings.Contains(all, c) {
				t.Errorf("canary %q leaked into logs, state or egress proxy responses:\n%s", c, all)
			}
		}
	})
	return r
}

var (
	entraAuth = wire.EntraAuth(wire.Entra{TenantID: "contoso", ClientID: "app",
		Credential: wire.Credential{Certificate: secret.Ref{File: "/run/secrets/taut.pem"}}})
	slackAuth = wire.SlackAuth(secret.Ref{Env: "TAUT_SLACK_TOKEN"})
	noAuth    = wire.NoAuth()
)

type call struct {
	method, origin, target string
	auth                   wire.Auth
	body                   io.Reader
	header                 http.Header
	ctx                    context.Context
	tenant                 string // overrides the Entra tenant
	keepAuth               bool   // send auth exactly as given
}

// entraFor is entraAuth for a (fake) Graph at origin: an Entra token is
// only attached to the Graph origin its own descriptor names.
func entraFor(origin string) wire.Auth {
	a := entraAuth
	a.GraphBase = origin
	return a
}

// do sends one request straight into the egress proxy's handler.
func (r *harness) do(c call) *httptest.ResponseRecorder {
	r.t.Helper()
	if c.method == "" {
		c.method = http.MethodGet
	}
	if c.target == "" {
		c.target = "/v1.0/users"
	}
	if c.auth.Kind == wire.KindEntra && c.auth.GraphBase == "" && !c.keepAuth {
		c.auth = entraFor(c.origin)
		if c.tenant != "" {
			c.auth.TenantID = c.tenant
		}
	}
	req := httptest.NewRequest(c.method, c.target, c.body)
	for k, v := range c.header {
		req.Header[k] = v
	}
	for k, v := range wire.Headers(c.origin, c.auth) {
		req.Header[k] = v
	}
	if c.ctx != nil {
		req = req.WithContext(c.ctx)
	}
	rec := httptest.NewRecorder()
	r.g.Handler().ServeHTTP(rec, req)
	if rec.Header().Get(wire.HeaderError) != "" {
		r.seen = append(r.seen, rec.Body.String())
	}
	for k, v := range rec.Header() {
		r.seen = append(r.seen, k+": "+strings.Join(v, ","))
	}
	return rec
}

// get is do for a plain GET with Entra auth, asserting the status.
func (r *harness) get(origin string, want int) *httptest.ResponseRecorder {
	r.t.Helper()
	rec := r.do(call{origin: origin, auth: entraAuth})
	if rec.Code != want {
		r.t.Fatalf("status = %d, want %d (body %s)", rec.Code, want, rec.Body)
	}
	return rec
}

func errorCode(rec *httptest.ResponseRecorder) string {
	var env wire.ErrorBody
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != rec.Header().Get(wire.HeaderError) {
		return "mismatch: body " + env.Error.Code + " header " + rec.Header().Get(wire.HeaderError)
	}
	return env.Error.Code
}

// tenant returns the state of one (tenant, scope).
func (r *harness) tenant(tenantName, scope string) TenantState {
	r.t.Helper()
	for _, ts := range r.g.State().Tenants {
		if ts.Tenant == TenantLabel(tenantName) && ts.Scope == scope {
			return ts
		}
	}
	r.t.Fatalf("no state for %s/%s in %+v", tenantName, scope, r.g.State())
	return TenantState{}
}

// eventually polls cond for up to two seconds of real time. It is used only
// for goroutine handoff (background lease refresh, bus delivery).
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func near(got, want time.Duration) bool {
	d := got - want
	return d > -2*time.Millisecond && d < 2*time.Millisecond
}

var errBoom = errors.New("control plane unreachable")
