package egressproxy

import (
	"bytes"
	"container/list"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bgfdev/taut/internal/bus"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/secret"
	"github.com/bgfdev/taut/internal/sidecar/budget"
	"github.com/bgfdev/taut/internal/sidecar/wire"
)

// Stable error codes of responses the egress proxy itself produces. Each such
// response carries the code in the X-Taut-Error header and a JSON body
// {"error":{code,what,why,next}}.
const (
	CodeDestinationNotAllowed = "proxy.destination_not_allowed"
	CodeBadRequest            = "proxy.bad_request"
	CodeUpstreamUnreachable   = "proxy.upstream_unreachable"
	CodeCanceled              = "proxy.canceled"
	CodeNoTokens              = "proxy.no_token_provider" //nolint:gosec // an error code, not a credential
)

// Paths served by the egress proxy itself, when X-Taut-Upstream is absent.
const (
	PathHealthz = "/_proxy/healthz"
	PathState   = "/_proxy/state"
)

// maxReplayBody is the largest request body kept for the one retry after 401.
const maxReplayBody = 1 << 20

var (
	errBadLease  = errors.New("the lease has no usable rate")
	errNoBudgets = errors.New("no budget source is configured")
)

// TokenProvider supplies bearer tokens. *token.Client satisfies it.
type TokenProvider interface {
	// Entra returns an access token for the application.
	Entra(ctx context.Context, req wire.Entra) (token string, expiresAt time.Time, err error)
	// Slack returns the Slack token the reference points at.
	Slack(ctx context.Context, ref secret.Ref) (string, error)
	// Invalidate reports that the last token for a was rejected upstream,
	// so the provider should not serve it from its cache again.
	Invalidate(ctx context.Context, a wire.Auth) error
}

// Config configures a Proxy.
type Config struct {
	// Holder identifies this egress proxy to the control plane and on the
	// bus. Defaults to a random name.
	Holder string
	// Tokens supplies bearer tokens. Without it only kind "none" works.
	Tokens TokenProvider
	// Budgets supplies tenant-wide rate leases. Without it every tenant
	// scope runs at the fallback rate.
	Budgets budget.Source
	// Bus carries throttle events between egress proxies. Optional.
	Bus bus.Bus
	// AllowHosts adds destinations to DefaultAllowHosts. An entry is
	// "[scope=][scheme://]host[:port]": host is a DNS name, "*.suffix" or
	// an IP literal; scheme defaults to https, and http must be spelled
	// out; without a port only the scheme's default port matches; scope
	// (graph, sharepoint, slack or other) selects the rate budget and
	// (graph, sharepoint, slack, slackfiles or other) selects the rate
	// budget and which credentials the host may receive, and defaults to
	// other: not rate limited, and kind "none" only. A fake Graph needs
	// "graph=http://127.0.0.1:8081" to be sent Entra tokens; a fake Slack
	// needs "slack=…" for its API (paths under /api/) and "slackfiles=…"
	// for file downloads (GET and HEAD), both under the "slack" budget.
	AllowHosts []string
	// FallbackRPS is the tenant rate without a valid lease. Default 2.
	FallbackRPS float64
	// ChannelStartRPS and ChannelMaxRPS bound the per-channel rate.
	// Defaults 1 and 5.
	ChannelStartRPS, ChannelMaxRPS float64
	// ChatRPS is the rate of message imports into one chat. Microsoft
	// documents one a second per chat and nothing higher, so unlike a
	// channel's rate it is a ceiling, not a start: it is lowered by a
	// throttle and recovers to this, never beyond. Default 1.
	ChatRPS float64
	// ChatShare is the part of a tenant's Graph rate that message imports
	// into chats may use: more than 0 and at most 1. Microsoft allows an
	// app 20 chat messages a second per tenant, against 50 for channels;
	// the default, 0.4, keeps chat imports to 16 a second of the default
	// 40, under that limit with headroom, across all egress proxies, since each
	// applies the share to its own lease. Zero means the default; any
	// other value outside (0, 1] is refused.
	ChatShare float64
	// MaxChannels bounds the number of channel buckets kept. Default 4096.
	MaxChannels int
	// MaxTenants bounds the number of (tenant, scope) buckets. Beyond it,
	// further keys share one overflow bucket at FallbackRPS. Default 64.
	MaxTenants int
	// MaxRPS is the most a lease may grant per budget scope ("graph",
	// "sharepoint", "slack"); larger leases are clamped to it. It defaults
	// to budget.DefaultTotals(), so an operator who raises a total in the
	// control plane must raise it here too.
	MaxRPS map[string]float64
	// Transport reaches the upstreams. The default never follows
	// redirects (no RoundTripper does) and leaves bodies untouched.
	Transport http.RoundTripper
	// Now, Sleep and Jitter exist for tests. Jitter returns a number in
	// [0, 1).
	Now    func() time.Time
	Sleep  func(ctx context.Context, d time.Duration) error
	Jitter func() float64
	// Log receives one line per throttle and lease change, and one debug
	// line per request.
	Log *slog.Logger
}

// Proxy is the egress proxy. Create it with New and release it with Close.
type Proxy struct {
	cfg       Config
	log       *slog.Logger
	now       func() time.Time
	allow     *allowlist
	transport http.RoundTripper
	proxy     *httputil.ReverseProxy

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu              sync.Mutex
	tenants         map[budget.Key]*tenantState
	channels        map[string]*channelState
	channelLRU      *list.List
	overflow        *tenantState
	refreshInline   bool // tests: renew leases on the request path
	throttles       int64
	remoteThrottles int64
}

// route is what the egress proxy decided about one request.
type route struct {
	upstream  *url.URL
	scope     string // the budget scope
	key       budget.Key
	chanKey   string
	auth      wire.Auth
	canReplay bool
	replay    []byte
}

type routeKey struct{}

// tokenError marks an error from the token provider.
type tokenError struct{ err error }

func (e *tokenError) Error() string { return e.err.Error() }
func (e *tokenError) Unwrap() error { return e.err }

// New validates cfg and returns a Proxy.
func New(cfg Config) (*Proxy, error) {
	allow, err := newAllowlist(cfg.AllowHosts)
	if err != nil {
		return nil, fail.Wrap(err, "proxy.config_invalid", "The egress proxy's list of allowed hosts is not valid", "",
			"Write each entry as [scope=][scheme://]host[:port], for example graph=http://127.0.0.1:8081.").WithExit(fail.ExitUsage)
	}
	if cfg.Holder == "" {
		var b [6]byte
		_, _ = rand.Read(b[:])
		cfg.Holder = "egress-proxy-" + hex.EncodeToString(b[:])
	}
	if cfg.FallbackRPS <= 0 {
		cfg.FallbackRPS = 2
	}
	if cfg.ChannelStartRPS <= 0 {
		cfg.ChannelStartRPS = 1
	}
	if cfg.ChannelMaxRPS <= 0 {
		cfg.ChannelMaxRPS = 5
	}
	if cfg.ChannelMaxRPS < cfg.ChannelStartRPS {
		cfg.ChannelMaxRPS = cfg.ChannelStartRPS
	}
	switch {
	case cfg.ChatShare == 0:
		cfg.ChatShare = 0.4
	case !(cfg.ChatShare > 0 && cfg.ChatShare <= 1):
		// A share of 0 would stop chat imports for good and one above 1
		// would lift the limit it exists to keep. Neither is a setting
		// to guess a meaning for.
		return nil, fail.New("proxy.config_invalid", "The chat share of the Graph rate is not valid",
			fmt.Sprintf("TAUT_GRAPH_CHAT_SHARE is %v; it must be more than 0 and at most 1.", cfg.ChatShare),
			"Set it to the part of the Graph budget chat imports may use, for example 0.5, or leave it unset.").WithExit(fail.ExitUsage)
	}
	if cfg.ChatRPS <= 0 {
		cfg.ChatRPS = 1
	}
	if cfg.MaxChannels <= 0 {
		cfg.MaxChannels = 4096
	}
	if cfg.MaxTenants <= 0 {
		cfg.MaxTenants = 64
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleep
	}
	if cfg.Jitter == nil {
		cfg.Jitter = mrand.Float64
	}
	if cfg.Transport == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.DisableCompression = true // pass bodies through exactly as sent
		t.MaxIdleConnsPerHost = 64
		t.ResponseHeaderTimeout = 2 * time.Minute
		cfg.Transport = t
	}
	var h slog.Handler = slog.DiscardHandler
	if cfg.Log != nil {
		h = cfg.Log.Handler()
	}
	g := &Proxy{
		cfg:        cfg,
		log:        slog.New(secret.NewRedactingHandler(h)).With("sidecar", "egress-proxy", "holder", cfg.Holder),
		now:        cfg.Now,
		allow:      allow,
		transport:  cfg.Transport,
		tenants:    map[budget.Key]*tenantState{},
		channels:   map[string]*channelState{},
		channelLRU: list.New(),
	}
	g.ctx, g.cancel = context.WithCancel(context.Background())
	g.proxy = &httputil.ReverseProxy{
		Rewrite:      g.rewrite,
		Transport:    roundTripper(g.roundTrip),
		ErrorHandler: g.proxyError,
		ErrorLog:     slog.NewLogLogger(g.log.Handler(), slog.LevelDebug),
	}
	if cfg.Budgets == nil {
		g.log.Warn("no budget source configured; every tenant scope runs at the fallback rate", "fallbackRps", cfg.FallbackRPS)
	}
	if cfg.Bus != nil {
		events, stop := cfg.Bus.Listen(wire.SubjectThrottle)
		g.wg.Add(1)
		go g.listen(events, stop)
	}
	return g, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Close stops the egress proxy's background work. Requests in flight finish.
func (g *Proxy) Close() {
	g.cancel()
	g.wg.Wait()
}

// Serve listens on addr, which must be a loopback address (see the package
// documentation), until ctx is done.
func (g *Proxy) Serve(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fail.Wrap(err, "proxy.listen_failed", "The egress proxy could not listen on "+addr, "",
			"Check that the port is free and the address is a loopback address such as 127.0.0.1:8480.")
	}
	return g.ServeListener(ctx, ln)
}

// ServeListener is Serve on an existing listener.
func (g *Proxy) ServeListener(ctx context.Context, ln net.Listener) error {
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok && !tcp.IP.IsLoopback() {
		g.log.Warn("the egress proxy is listening on a non-loopback address; anything that can reach it can act as the migration",
			"addr", ln.Addr().String())
	}
	srv := &http.Server{
		Handler:           g.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(g.log.Handler(), slog.LevelDebug),
		// No read or write timeout: uploads and downloads may be long.
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if srv.Shutdown(shutdownCtx) != nil {
			_ = srv.Close()
		}
	}()
	g.log.Info("egress proxy listening", "addr", ln.Addr().String())
	err := srv.Serve(ln)
	<-done
	g.Close()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Handler returns the egress proxy's HTTP handler.
func (g *Proxy) Handler() http.Handler { return http.HandlerFunc(g.serveHTTP) }

func (g *Proxy) writeError(w http.ResponseWriter, status int, fe *fail.Error) {
	w.Header().Set(wire.HeaderError, fe.Code)
	wire.WriteError(w, status, fe)
}

func badRequest(why string) *fail.Error {
	return fail.New(CodeBadRequest, "The egress proxy could not route the request", why,
		"Send requests through Taut's HTTP client, which sets "+wire.HeaderUpstream+" and "+wire.HeaderAuth+"; this is a bug in the caller.")
}

func (g *Proxy) serveHTTP(w http.ResponseWriter, r *http.Request) {
	ups := r.Header.Values(wire.HeaderUpstream)
	if len(ups) == 0 {
		g.serveManagement(w, r)
		return
	}
	if len(ups) > 1 || len(r.Header.Values(wire.HeaderAuth)) > 1 {
		g.writeError(w, http.StatusBadRequest, badRequest("a routing header is present more than once"))
		return
	}
	upstream, err := parseUpstream(ups[0])
	if err != nil {
		g.writeError(w, http.StatusBadRequest, badRequest("the "+wire.HeaderUpstream+" header is not usable: "+err.Error()))
		return
	}
	scope, err := g.allow.check(upstream)
	if err != nil {
		g.log.Warn("destination refused", "host", upstream.Host, "why", err.Error())
		g.writeError(w, http.StatusForbidden, fail.New(CodeDestinationNotAllowed,
			"The egress proxy does not forward requests to "+secret.Redact(upstream.Host), err.Error(),
			"Taut only talks to Microsoft Graph, SharePoint and Slack. For a local fake or another endpoint, add it to the egress proxy's allowed hosts."))
		return
	}
	auth, err := wire.DecodeAuth(r.Header.Get(wire.HeaderAuth))
	if err != nil {
		var fe *fail.Error
		why := err.Error()
		if errors.As(err, &fe) {
			why = fe.Why
		}
		g.writeError(w, http.StatusBadRequest, badRequest(why))
		return
	}
	if err := checkPairing(auth, scope, r.Method, r.URL.Path, upstream); err != nil {
		g.log.Warn("credential refused for destination", "host", upstream.Host, "kind", auth.Kind, "why", err.Error())
		g.writeError(w, http.StatusForbidden, fail.New(CodeDestinationNotAllowed,
			"The egress proxy does not send "+auth.Kind+" credentials to "+secret.Redact(upstream.Host), err.Error(),
			"Use kind \"none\" for pre-authorized URLs. For a local fake, scope its allowed-hosts entry (graph=, slack= or slackfiles=); otherwise this is a bug in the caller."))
		return
	}

	// Only now, with destination and credential accepted, does the
	// request get to name a budget.
	scope = budgetScope(scope)
	rt := &route{upstream: upstream, scope: scope, auth: auth}
	rt.key = budget.Key{Tenant: strings.ToLower(upstream.Hostname()), Scope: scope}
	if auth.Kind == wire.KindEntra {
		rt.key.Tenant = strings.ToLower(strings.TrimSpace(auth.TenantID))
	}
	rt.chanKey = channelKey(r.Method, scope, r.URL.EscapedPath())

	// Keep a small body so the request can be replayed once after a 401.
	switch {
	case r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0:
		rt.canReplay = true
	case auth.Kind != wire.KindNone && r.ContentLength > 0 && r.ContentLength <= maxReplayBody:
		buf := make([]byte, r.ContentLength)
		if _, err := io.ReadFull(r.Body, buf); err != nil {
			g.writeError(w, http.StatusBadRequest, badRequest("the request body ended before its Content-Length"))
			return
		}
		rt.replay, rt.canReplay = buf, true
		r.Body = io.NopCloser(bytes.NewReader(buf))
	}
	g.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), routeKey{}, rt)))
}

// rewrite points the outgoing request at the upstream and removes
// everything that was addressed to the egress proxy.
func (g *Proxy) rewrite(pr *httputil.ProxyRequest) {
	rt := pr.In.Context().Value(routeKey{}).(*route)
	pr.Out.URL.Scheme = rt.upstream.Scheme
	pr.Out.URL.Host = rt.upstream.Host
	pr.Out.URL.RawQuery = pr.In.URL.RawQuery // verbatim; the egress proxy does not interpret it
	pr.Out.Host = rt.upstream.Host
	for name := range pr.Out.Header {
		if len(name) >= len(wire.HeaderPrefix) && strings.EqualFold(name[:len(wire.HeaderPrefix)], wire.HeaderPrefix) {
			delete(pr.Out.Header, name)
		}
	}
	pr.Out.Header.Del("Authorization")
	pr.Out.Header.Del("Proxy-Authorization")
}

// bearer returns the token for a, or "" for kind none.
func (g *Proxy) bearer(ctx context.Context, a wire.Auth) (string, error) {
	if a.Kind == wire.KindNone {
		return "", nil
	}
	if g.cfg.Tokens == nil {
		return "", &tokenError{fail.New(CodeNoTokens, "The egress proxy has no token broker to ask", "it was started without one",
			"Start the egress proxy with the token broker's socket path.")}
	}
	var tok string
	var err error
	if a.Kind == wire.KindEntra {
		tok, _, err = g.cfg.Tokens.Entra(ctx, a.Entra)
	} else {
		tok, err = g.cfg.Tokens.Slack(ctx, a.Token)
	}
	if err == nil && tok == "" {
		err = fail.New("token.broker_failed", "The token broker returned an empty token", "", "Check the token broker's logs.")
	}
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", &tokenError{err}
	}
	return tok, nil
}

// send admits and forwards one attempt.
func (g *Proxy) send(req *http.Request, rt *route, tok string, body io.ReadCloser) (*http.Response, error) {
	ctx := req.Context()
	queuedAt := g.now()
	admittedAt, err := g.admit(ctx, rt)
	if err != nil {
		return nil, err
	}
	out := req.Clone(ctx)
	if body != nil {
		out.Body = body
	}
	if tok != "" {
		out.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := g.transport.RoundTrip(out)
	if err != nil {
		return nil, err
	}
	g.observe(rt, admittedAt, resp)
	if g.log.Enabled(ctx, slog.LevelDebug) {
		g.log.Debug("forwarded", "method", req.Method, "url", secret.Redact(req.URL.String()), "status", resp.StatusCode,
			"waited", admittedAt.Sub(queuedAt).String(), "scope", rt.scope)
	}
	return resp, nil
}

// roundTrip is the ReverseProxy's transport: token, admission, forwarding,
// feedback, and the single retry after a 401.
func (g *Proxy) roundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	rt := ctx.Value(routeKey{}).(*route)
	tok, err := g.bearer(ctx, rt.auth)
	if err != nil {
		return nil, err
	}
	resp, err := g.send(req, rt, tok, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized && tok != "" && rt.canReplay {
		// The token may have been revoked or rotated under us. Ask once
		// for a different one; if there is none, the 401 stands.
		_ = g.cfg.Tokens.Invalidate(ctx, rt.auth)
		if tok2, err := g.bearer(ctx, rt.auth); err == nil && tok2 != tok {
			var body io.ReadCloser = http.NoBody
			if rt.replay != nil {
				body = io.NopCloser(bytes.NewReader(rt.replay))
			}
			if resp2, err := g.send(req, rt, tok2, body); err == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
				_ = resp.Body.Close()
				resp, tok = resp2, tok2
				g.log.Info("retried once with a fresh token after 401", "scope", rt.scope, "tenant", rt.key.Tenant, "status", resp.StatusCode)
			}
		}
	}
	if tok != "" {
		// An upstream that echoes the credential must not hand it to
		// the worker. Headers are checked now; trailers arrive with the
		// end of the body, so they are checked there. The body itself is
		// streamed and not inspected.
		scrubHeader(resp.Header, tok)
		if resp.StatusCode != http.StatusSwitchingProtocols && resp.Body != nil {
			resp.Body = &trailerScrubber{ReadCloser: resp.Body, resp: resp, tok: tok}
		}
	}
	return resp, nil
}

// scrubHeader removes every header whose value contains tok.
func scrubHeader(h http.Header, tok string) {
	for name, values := range h {
		for _, v := range values {
			if strings.Contains(v, tok) {
				delete(h, name)
				break
			}
		}
	}
}

// trailerScrubber removes trailers that contain the bearer token once the
// body has been read to its end, which is when trailers become available.
type trailerScrubber struct {
	io.ReadCloser
	resp *http.Response
	tok  string
}

func (t *trailerScrubber) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	if err != nil {
		scrubHeader(t.resp.Trailer, t.tok)
	}
	return n, err
}

// retryAfter parses a Retry-After header: seconds or an HTTP date.
func retryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	var d time.Duration
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		if secs < 0 || secs != secs {
			return 0, false
		}
		d = time.Duration(min(secs, maxRetryAfter.Seconds()) * float64(time.Second))
	} else if at, err := http.ParseTime(v); err == nil {
		d = max(at.Sub(now), 0)
	} else {
		return 0, false
	}
	return min(d, maxRetryAfter), true
}

// observe feeds one response into the AIMD state.
func (g *Proxy) observe(rt *route, admittedAt time.Time, resp *http.Response) {
	if rt.scope == ScopeOther {
		return
	}
	status := resp.StatusCode
	ra, has := retryAfter(resp.Header, g.now())
	switch {
	case status == http.StatusTooManyRequests && !has:
		ra = defaultRetryAfter
	case status == http.StatusTooManyRequests, status == http.StatusServiceUnavailable && has:
	case status < 400:
		g.succeeded(rt)
		return
	default:
		return
	}
	pause, mult := g.throttled(rt, admittedAt, ra)
	g.log.Warn("upstream throttled", "tenant", rt.key.Tenant, "scope", rt.scope, "status", status,
		"retryAfter", ra.String(), "tenantPause", pause.String(), "multiplier", mult, "channel", rt.chanKey)
	if g.cfg.Bus != nil {
		data, _ := json.Marshal(wire.Throttle{
			Tenant: rt.key.Tenant, Scope: rt.scope, RetryAfterMs: pause.Milliseconds(),
			At: g.now().UTC().Format(time.RFC3339Nano), Holder: g.cfg.Holder,
		})
		ctx, cancel := context.WithTimeout(g.ctx, 2*time.Second)
		_ = g.cfg.Bus.Publish(ctx, wire.SubjectThrottle, data) // best effort
		cancel()
	}
}

// listen applies throttle events published by other egress proxies.
func (g *Proxy) listen(events <-chan []byte, stop func()) {
	defer g.wg.Done()
	defer stop()
	for {
		select {
		case <-g.ctx.Done():
			return
		case data, ok := <-events:
			if !ok {
				return
			}
			var ev wire.Throttle
			if json.Unmarshal(data, &ev) != nil || ev.Holder == g.cfg.Holder || ev.Tenant == "" || ev.RetryAfterMs < 0 {
				continue
			}
			// Nothing authenticates the sender, so an event is believed
			// only if it is current and only for a short pause.
			at, err := time.Parse(time.RFC3339Nano, ev.At)
			if age := g.now().Sub(at); err != nil || age > maxEventAge || age < -maxEventAge {
				continue
			}
			pause := time.Duration(min(ev.RetryAfterMs, maxRemotePause.Milliseconds())) * time.Millisecond
			if g.remoteThrottle(budget.Key{Tenant: ev.Tenant, Scope: ev.Scope}, pause) {
				g.log.Info("pausing with the tenant's other egress proxies", "tenant", ev.Tenant, "scope", ev.Scope,
					"pause", pause.String(), "from", ev.Holder)
			}
		}
	}
}

func (g *Proxy) proxyError(w http.ResponseWriter, r *http.Request, err error) {
	var te *tokenError
	switch {
	case r.Context().Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		g.writeError(w, 499, fail.New(CodeCanceled, "The request was canceled before it was sent or answered", "",
			"Retry the request."))
	case errors.As(err, &te):
		fe := fail.From(te.err)
		g.log.Warn("no token for request", "code", fe.Code, "why", fe.Why)
		g.writeError(w, http.StatusBadGateway, fe)
	default:
		host := ""
		if rt, ok := r.Context().Value(routeKey{}).(*route); ok {
			host = rt.upstream.Host
		}
		g.log.Warn("upstream unreachable", "host", host, "error", err)
		g.writeError(w, http.StatusBadGateway, fail.New(CodeUpstreamUnreachable,
			"The egress proxy could not reach "+secret.Redact(host), secret.Redact(err.Error()),
			"Check the pod's outbound network access and DNS, then retry."))
	}
}

// --- management -----------------------------------------------------------------

// State is the egress proxy's admission state, as served on /_proxy/state.
// It holds no secrets.
type State struct {
	Holder          string        `json:"holder"`
	Tenants         []TenantState `json:"tenants"`
	Channels        int           `json:"channels"`
	Throttles       int64         `json:"throttles"`
	RemoteThrottles int64         `json:"remoteThrottles"`
}

// TenantLabel returns the short, stable label under which a tenant key
// appears in State: a prefix of its SHA-256. The state endpoint is
// unauthenticated on loopback, so it does not reveal tenant IDs; an operator
// who knows the ID can compute the label.
func TenantLabel(tenant string) string {
	sum := sha256.Sum256([]byte(tenant))
	return "t-" + hex.EncodeToString(sum[:6])
}

// TenantState describes one (tenant, scope) bucket.
type TenantState struct {
	// Tenant is TenantLabel of the bucket's tenant key, or "overflow" for
	// the bucket shared by keys beyond Config.MaxTenants.
	Tenant string `json:"tenant"`
	Scope  string `json:"scope"`
	// LeaseRPS is the last lease; Leased says whether it is in force
	// (false means the fallback rate applies).
	LeaseRPS    float64    `json:"leaseRps"`
	Leased      bool       `json:"leased"`
	Multiplier  float64    `json:"multiplier"`
	RateRPS     float64    `json:"rateRps"`
	PausedUntil *time.Time `json:"pausedUntil,omitempty"`
	QueueDepth  int        `json:"queueDepth"`
}

// State returns a snapshot of the admission state.
func (g *Proxy) State() State {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	s := State{Holder: g.cfg.Holder, Tenants: []TenantState{}, Channels: len(g.channels),
		Throttles: g.throttles, RemoteThrottles: g.remoteThrottles}
	all := make([]*tenantState, 0, len(g.tenants)+1)
	for _, ts := range g.tenants {
		all = append(all, ts)
	}
	if g.overflow != nil {
		all = append(all, g.overflow)
	}
	for _, ts := range all {
		base := g.baseRate(ts, now)
		label := TenantLabel(ts.key.Tenant)
		if ts.overflow {
			label = "overflow"
		}
		t := TenantState{
			Tenant: label, Scope: ts.key.Scope,
			LeaseRPS: ts.leaseRPS, Leased: ts.leased && base == ts.leaseRPS,
			Multiplier: ts.mult, RateRPS: base * ts.mult, QueueDepth: ts.waiting,
		}
		if ts.pauseUntil.After(now) {
			until := ts.pauseUntil
			t.PausedUntil = &until
		}
		s.Tenants = append(s.Tenants, t)
	}
	sort.Slice(s.Tenants, func(i, j int) bool {
		if s.Tenants[i].Tenant != s.Tenants[j].Tenant {
			return s.Tenants[i].Tenant < s.Tenants[j].Tenant
		}
		return s.Tenants[i].Scope < s.Tenants[j].Scope
	})
	return s
}

func (g *Proxy) serveManagement(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method != http.MethodGet && r.Method != http.MethodHead:
	case r.URL.Path == PathHealthz:
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
		return
	case r.URL.Path == PathState:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(g.State())
		return
	}
	g.writeError(w, http.StatusBadRequest, badRequest("the "+wire.HeaderUpstream+" header is missing"))
}
