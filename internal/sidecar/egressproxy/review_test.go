package egressproxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/bus"
	"github.com/bgfdev/taut/internal/sidecar/wire"
)

// --- item 4: token-to-destination pairing --------------------------------------------

func TestPairingRules(t *testing.T) {
	a, err := newAllowlist([]string{"graph=http://fakegraph:9000", "slack=http://fakeslack:9100", "slackfiles=http://fakefiles:9200", "http://plain:9300"})
	if err != nil {
		t.Fatal(err)
	}
	entra := func(cloud, graphBase string) wire.Auth {
		e := entraAuth
		e.Cloud, e.GraphBase = cloud, graphBase
		return e
	}
	cases := []struct {
		name   string
		auth   wire.Auth
		method string
		origin string
		path   string
		ok     bool
	}{
		// Entra: only the Graph origin of its own descriptor.
		{"entra global → global graph", entra("", ""), "GET", "https://graph.microsoft.com", "/v1.0/users", true},
		{"entra global → graph with explicit port", entra("global", ""), "POST", "https://GRAPH.microsoft.com:443", "/v1.0/x", true},
		{"entra gcc high → its graph", entra("usgovhigh", ""), "GET", "https://graph.microsoft.us", "/v1.0/users", true},
		{"entra china → its graph", entra("china", ""), "GET", "https://microsoftgraph.chinacloudapi.cn", "/v1.0/users", true},
		{"entra with graphBase → that fake", entra("", "http://fakegraph:9000/"), "GET", "http://fakegraph:9000", "/v1.0/users", true},
		{"entra global → another cloud's graph", entra("", ""), "GET", "https://graph.microsoft.us", "/v1.0/users", false},
		{"entra global → china graph", entra("global", ""), "GET", "https://microsoftgraph.chinacloudapi.cn", "/v1.0/users", false},
		{"entra dod → gcc high graph", entra("dod", ""), "GET", "https://graph.microsoft.us", "/v1.0/users", false},
		{"entra → sharepoint", entra("", ""), "PUT", "https://contoso.sharepoint.com", "/upload", false},
		{"entra with graphBase sharepoint → sharepoint", entra("", "https://contoso.sharepoint.com"), "PUT", "https://contoso.sharepoint.com", "/upload", false},
		{"entra → slack", entra("", "https://slack.com"), "GET", "https://slack.com", "/api/x", false},
		{"entra → graph-scoped fake it does not name", entra("", ""), "GET", "http://fakegraph:9000", "/v1.0/users", false},
		{"entra naming the fake → real graph", entra("", "http://fakegraph:9000"), "GET", "https://graph.microsoft.com", "/v1.0/users", false},
		{"entra → unscoped host, even if named", entra("", "http://plain:9300"), "GET", "http://plain:9300", "/v1.0/users", false},
		{"entra unknown cloud", entra("atlantis", ""), "GET", "https://graph.microsoft.com", "/v1.0/users", false},
		{"entra graphBase with a path", entra("", "https://graph.microsoft.com/v1.0"), "GET", "https://graph.microsoft.com", "/v1.0/users", false},

		// Slack: the API under /api/, file hosts for reads.
		{"slack → api", slackAuth, "POST", "https://slack.com", "/api/conversations.history", true},
		{"slack → api GET", slackAuth, "GET", "https://slack.com", "/api/auth.test", true},
		{"slack → slack.com outside /api/", slackAuth, "GET", "https://slack.com", "/oauth/authorize", false},
		{"slack → slack.com root", slackAuth, "GET", "https://slack.com", "/", false},
		{"slack → /api without slash", slackAuth, "GET", "https://slack.com", "/api", false},
		{"slack → /apix/", slackAuth, "GET", "https://slack.com", "/apix/y", false},
		{"slack → dot-dot out of /api/", slackAuth, "GET", "https://slack.com", "/api/../oauth", false},
		{"slack → files GET", slackAuth, "GET", "https://files.slack.com", "/files-pri/T1-F1/x.png", true},
		{"slack → files HEAD", slackAuth, "HEAD", "https://files.slack.com", "/files-pri/T1-F1/x.png", true},
		{"slack → slack-files GET", slackAuth, "GET", "https://a.slack-files.com", "/x", true},
		{"slack → files POST", slackAuth, "POST", "https://files.slack.com", "/upload", false},
		{"slack → files DELETE", slackAuth, "DELETE", "https://a.slack-files.com", "/x", false},
		{"slack → graph", slackAuth, "GET", "https://graph.microsoft.com", "/api/x", false},
		{"slack → sharepoint", slackAuth, "GET", "https://contoso.sharepoint.com", "/api/x", false},
		{"slack → fake api", slackAuth, "POST", "http://fakeslack:9100", "/api/chat.postMessage", true},
		{"slack → fake api outside /api/", slackAuth, "GET", "http://fakeslack:9100", "/files/x", false},
		{"slack → fake files GET", slackAuth, "GET", "http://fakefiles:9200", "/files/x", true},
		{"slack → fake files POST", slackAuth, "POST", "http://fakefiles:9200", "/files/x", false},
		{"slack → unscoped host", slackAuth, "GET", "http://plain:9300", "/api/x", false},

		// None goes anywhere on the allowlist.
		{"none → sharepoint", noAuth, "PUT", "https://contoso.sharepoint.com", "/upload", true},
		{"none → files", noAuth, "GET", "https://files.slack.com", "/x", true},
		{"none → unscoped host", noAuth, "POST", "http://plain:9300", "/x", true},
		{"none → graph", noAuth, "GET", "https://graph.microsoft.com", "/x", true},
	}
	for _, c := range cases {
		u, err := url.Parse(c.origin)
		if err != nil {
			t.Fatal(err)
		}
		scope, err := a.check(u)
		if err != nil {
			t.Errorf("%s: destination refused: %v", c.name, err)
			continue
		}
		if err := checkPairing(c.auth, scope, c.method, c.path, u); (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want allowed=%v", c.name, err, c.ok)
		}
	}
	if budgetScope(ScopeSlackFiles) != ScopeSlack || budgetScope(ScopeGraph) != ScopeGraph {
		t.Error("slackfiles must draw on the slack budget")
	}
}

func TestPairingIsEnforcedBeforeTheTokenBroker(t *testing.T) {
	up := newUpstream(t, echo)
	addr := up.Listener.Addr().String()
	refused := func(t *testing.T, r *harness, c call) {
		t.Helper()
		rec := r.do(c)
		if rec.Code != http.StatusForbidden || errorCode(rec) != CodeDestinationNotAllowed {
			t.Errorf("%d %s, want 403 %s", rec.Code, rec.Body, CodeDestinationNotAllowed)
		}
		if n := r.tokens.entraCalls + r.tokens.slackCalls; n != 0 {
			t.Errorf("the token broker was asked %d times", n)
		}
		if up.count() != 0 || len(r.g.State().Tenants) != 0 {
			t.Errorf("the refused request had effects: upstream %d, buckets %d", up.count(), len(r.g.State().Tenants))
		}
	}

	t.Run("unscoped host takes no credential", func(t *testing.T) {
		r := newHarness(t, nil, "http://"+addr)
		refused(t, r, call{origin: up.URL, auth: entraAuth})
		refused(t, r, call{origin: up.URL, auth: slackAuth, target: "/api/x"})
	})
	t.Run("sharepoint takes no entra token", func(t *testing.T) {
		r := newHarness(t, nil, "sharepoint=http://"+addr)
		refused(t, r, call{method: "PUT", origin: up.URL, auth: entraAuth})
	})
	t.Run("graph host the descriptor does not name", func(t *testing.T) {
		r := newHarness(t, nil, "graph=http://"+addr)
		refused(t, r, call{origin: up.URL, auth: entraAuth, keepAuth: true}) // descriptor resolves to graph.microsoft.com
	})
	t.Run("slack api outside /api/", func(t *testing.T) {
		r := newHarness(t, nil, "slack=http://"+addr)
		refused(t, r, call{origin: up.URL, auth: slackAuth, target: "/files-pri/x"})
	})
	t.Run("slack file host is read-only", func(t *testing.T) {
		r := newHarness(t, nil, "slackfiles=http://"+addr)
		refused(t, r, call{method: "POST", origin: up.URL, auth: slackAuth, target: "/files/x"})
		// …and reads work, on the slack budget.
		rec := r.do(call{origin: up.URL, auth: slackAuth, target: "/files/x?t=" + canaryURL})
		if rec.Code != http.StatusCreated {
			t.Fatalf("GET on a file host: %d %s", rec.Code, rec.Body)
		}
		if req, _ := up.last(); req.Header.Get("Authorization") != "Bearer "+canarySlack {
			t.Error("the Slack token was not attached")
		}
		if ts := r.g.State().Tenants; len(ts) != 1 || ts[0].Scope != ScopeSlack {
			t.Errorf("file host budget = %+v, want the slack scope", ts)
		}
	})
}

// --- item 5: budget keys --------------------------------------------------------------

func TestTenantBucketsAreCapped(t *testing.T) {
	r, up := graphHarness(t, 10, func(c *Config) { c.MaxTenants = 4; c.FallbackRPS = 2 })
	for i := 0; i < 4; i++ {
		r.do(call{origin: up.URL, auth: entraAuth, tenant: fmt.Sprintf("tenant-%d", i)})
	}
	leaseCalls := r.leases.calls

	// A hundred invented tenants: no new buckets, no lease requests, and
	// together they get one fallback-rate bucket.
	d := r.elapsed(func() {
		for i := 0; i < 100; i++ {
			if rec := r.do(call{origin: up.URL, auth: entraAuth, tenant: fmt.Sprintf("invented-%d", i)}); rec.Code != 200 {
				t.Fatalf("status %d", rec.Code)
			}
		}
	})
	if !near(d, 49*time.Second) {
		t.Errorf("100 requests for invented tenants took %v, want 49s (2 rps shared, burst 2)", d)
	}
	st := r.g.State()
	if len(st.Tenants) != 5 || len(r.g.tenants) != 4 {
		t.Fatalf("%d buckets in state, %d real; want 4 real plus the overflow", len(st.Tenants), len(r.g.tenants))
	}
	if r.leases.calls != leaseCalls {
		t.Errorf("invented tenants caused %d lease requests", r.leases.calls-leaseCalls)
	}
	var overflow *TenantState
	for i := range st.Tenants {
		if st.Tenants[i].Tenant == "overflow" {
			overflow = &st.Tenants[i]
		}
	}
	if overflow == nil || overflow.RateRPS != 2 || overflow.Leased {
		t.Errorf("overflow bucket = %+v", overflow)
	}
	// Tenants that already had a bucket keep their own rate.
	r.clock.Advance(time.Second)
	if d := r.elapsed(func() {
		for i := 0; i < 10; i++ {
			r.do(call{origin: up.URL, auth: entraAuth, tenant: "tenant-0"})
		}
	}); d != 0 {
		t.Errorf("an established tenant waited %v", d)
	}
	// A throttle on an overflow key pauses the overflow bucket, not a new one.
	up.setHandler(throttleOnce(429, "3"))
	r.do(call{origin: up.URL, auth: entraAuth, tenant: "invented-x"})
	if len(r.g.tenants) != 4 {
		t.Errorf("a 429 created a bucket: %d", len(r.g.tenants))
	}
}

// --- item 6: unauthenticated bus messages -----------------------------------------------

func TestLeaseIsClampedToTheScopeBudget(t *testing.T) {
	up := newUpstream(t, ok)
	addr := up.Listener.Addr().String()

	// Default ceiling: budget.DefaultTotals. A forged "1000000 rps for a
	// day" is believed as 40 rps for 60 s.
	r := newHarness(t, func(c *Config) { c.MaxRPS = nil }, "graph=http://"+addr)
	r.leases.set(1e6, nil)
	r.leases.ttl = 24 * time.Hour
	r.get(up.URL, 200)
	if ts := r.tenant("contoso", ScopeGraph); ts.LeaseRPS != 40 || ts.RateRPS != 40 || !ts.Leased {
		t.Errorf("clamped lease = %+v, want 40 rps", ts)
	}
	if d := r.elapsed(func() {
		for i := 0; i < 79; i++ {
			r.get(up.URL, 200)
		}
	}); !near(d, time.Second) {
		t.Errorf("80 requests took %v, want 1s at 40 rps", d)
	}
	// The TTL is capped at 60 s, so it is renewed at 30 s…
	calls := r.leases.calls
	r.clock.Advance(28 * time.Second) // 1 s has passed already
	r.get(up.URL, 200)
	if r.leases.calls != calls {
		t.Errorf("renewed early")
	}
	r.clock.Advance(time.Second)
	r.get(up.URL, 200)
	if r.leases.calls != calls+1 {
		t.Errorf("lease calls = %d, want a renewal 30 s into a capped 60 s lease", r.leases.calls-calls)
	}
	// …and has run out after 60 s.
	r.clock.Advance(61 * time.Second)
	if ts := r.tenant("contoso", ScopeGraph); ts.Leased || ts.RateRPS != 2 {
		t.Errorf("a 24 h lease is still believed after 61 s: %+v", ts)
	}

	// An operator-raised total is passed in explicitly.
	r = newHarness(t, func(c *Config) { c.MaxRPS = map[string]float64{ScopeGraph: 100} }, "graph=http://"+addr)
	r.leases.set(1e6, nil)
	r.get(up.URL, 200)
	if ts := r.tenant("contoso", ScopeGraph); ts.LeaseRPS != 100 {
		t.Errorf("with MaxRPS 100: %+v", ts)
	}

	// Slack's ceiling is its own.
	r = newHarness(t, func(c *Config) { c.MaxRPS = nil }, "slack=http://"+addr)
	r.leases.set(500, nil)
	r.do(call{origin: up.URL, auth: slackAuth, target: "/api/x"})
	if ts := r.g.State().Tenants[0]; ts.LeaseRPS != 1 {
		t.Errorf("slack lease = %+v, want 1 rps", ts)
	}

	// Non-positive and non-finite leases are failures.
	for _, bad := range []float64{0, -3} {
		r = newHarness(t, func(c *Config) { c.MaxRPS = nil }, "graph=http://"+addr)
		r.leases.set(bad, nil)
		r.get(up.URL, 200)
		if ts := r.tenant("contoso", ScopeGraph); ts.Leased || ts.RateRPS != 2 {
			t.Errorf("lease of %v rps: %+v", bad, ts)
		}
	}
}

func TestLocal429KeepsItsLongPause(t *testing.T) {
	b := bus.NewLocal()
	up := newUpstream(t, throttleOnce(429, "300"))
	allow := "graph=http://" + up.Listener.Addr().String()
	a := newHarness(t, func(c *Config) { c.Bus = b; c.Holder = "gov-a" }, allow)
	other := newHarness(t, func(c *Config) { c.Bus = b; c.Holder = "gov-b" }, allow)
	up.setHandler(ok)
	other.get(up.URL, 200)
	up.setHandler(throttleOnce(429, "300"))
	a.get(up.URL, 429)
	if ts := a.tenant("contoso", ScopeGraph); !ts.PausedUntil.Equal(a.clock.Now().Add(300 * time.Second)) {
		t.Errorf("first-hand pause = %v, want the full 300 s", ts.PausedUntil)
	}
	eventually(t, "the remote pause", func() bool { return other.g.State().RemoteThrottles == 1 })
	if ts := other.tenant("contoso", ScopeGraph); !ts.PausedUntil.Equal(other.clock.Now().Add(30 * time.Second)) {
		t.Errorf("second-hand pause = %v, want it capped at 30 s", ts.PausedUntil)
	}
}

// --- item 7: trailers -------------------------------------------------------------------

func TestTokenIsScrubbedFromTrailers(t *testing.T) {
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Trailer", "X-Echo-Trailer, X-Checksum")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "body")
		w.Header().Set("X-Echo-Trailer", r.Header.Get("Authorization"))
		w.Header().Set("X-Checksum", "abc123")
	}))
	defer upSrv.Close()
	r := newHarness(t, nil, "graph=http://"+upSrv.Listener.Addr().String())
	front := httptest.NewServer(r.g.Handler())
	defer front.Close()

	for _, auth := range []wire.Auth{entraFor(upSrv.URL), noAuth} {
		req, _ := http.NewRequest("GET", front.URL+"/v1.0/users", nil)
		req.Header = wire.Headers(upSrv.URL, auth)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != "body" || resp.Trailer.Get("X-Checksum") != "abc123" {
			t.Fatalf("kind %s: body %q, trailers %v; ordinary trailers must pass", auth.Kind, body, resp.Trailer)
		}
		got := resp.Trailer.Get("X-Echo-Trailer")
		if auth.Kind == wire.KindEntra && got != "" {
			t.Errorf("the bearer token came back in a trailer: %d bytes", len(got))
		}
		for _, v := range resp.Trailer {
			if strings.Contains(strings.Join(v, " "), canaryEntra) {
				t.Error("token in trailers")
			}
		}
	}
}

// --- item 8: state ----------------------------------------------------------------------

func TestStateHidesTenantIDs(t *testing.T) {
	const tenantID = "5d7f3a10-9b2c-4e6f-8a1d-0c2b3e4f5a6b"
	up := newUpstream(t, ok)
	r := newHarness(t, nil, "graph=http://"+up.Listener.Addr().String())
	r.do(call{origin: up.URL, auth: entraAuth, tenant: tenantID})
	r.do(call{origin: up.URL, auth: noAuth})

	rec := httptest.NewRecorder()
	r.g.Handler().ServeHTTP(rec, httptest.NewRequest("GET", PathState, nil))
	body := rec.Body.String()
	if strings.Contains(body, tenantID) || strings.Contains(body, tenantID[:8]) || strings.Contains(body, "127.0.0.1") {
		t.Errorf("the state reveals a tenant key: %s", body)
	}
	var st State
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	labels := map[string]bool{}
	for _, ts := range st.Tenants {
		labels[ts.Tenant] = true
	}
	if len(st.Tenants) != 2 || !labels[TenantLabel(tenantID)] || !labels[TenantLabel("127.0.0.1")] {
		t.Errorf("labels = %v", labels)
	}
	// Stable, short, and distinct per key.
	if TenantLabel(tenantID) != TenantLabel(tenantID) || TenantLabel(tenantID) == TenantLabel("other") || len(TenantLabel(tenantID)) != 14 {
		t.Errorf("label = %q", TenantLabel(tenantID))
	}
}
