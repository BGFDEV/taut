package egressproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/sidecar/wire"
)

func echo(w http.ResponseWriter, r *http.Request, _ int) {
	w.Header().Set("X-Echo-Auth", r.Header.Get("Authorization"))
	w.Header().Set("X-Upstream-Header", "kept")
	w.Header().Set("Content-Type", "application/vnd.test")
	w.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(w, "upstream-body")
}

func TestForwardingAndAuthInjection(t *testing.T) {
	const query = "?$filter=a%20b&t=" + canaryURL + "&odd=1;2"

	for _, c := range []struct {
		name       string
		scope      string
		path       string
		auth       wire.Auth
		wantBearer string
	}{
		{"entra", "graph=", "/v1.0/teams/19%3Aabc%40thread.tacv2/x%2Fy", entraAuth, canaryEntra},
		{"slack", "slack=", "/api/files.info/x%2Fy", slackAuth, canarySlack},
		{"none", "", "/v1.0/teams/19%3Aabc%40thread.tacv2/x%2Fy", noAuth, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			up := newUpstream(t, echo)
			r := newHarness(t, nil, c.scope+"http://"+up.Listener.Addr().String())
			target := c.path + query
			rec := r.do(call{method: "POST", origin: up.URL, target: target, auth: c.auth, body: strings.NewReader("hello"),
				header: http.Header{
					"Authorization":       {"Bearer " + canaryInbound},
					"Proxy-Authorization": {"Basic " + canaryInbound},
					"X-Taut-Extra":        {"internal"},
					"Connection":          {"X-Hop"},
					"X-Hop":               {"1"},
					"X-Custom":            {"kept"},
					"Content-Type":        {"application/json"},
				}})
			if rec.Code != http.StatusCreated || rec.Body.String() != "upstream-body" ||
				rec.Header().Get("X-Upstream-Header") != "kept" || rec.Header().Get("Content-Type") != "application/vnd.test" {
				t.Fatalf("response not passed through: %d %q %v", rec.Code, rec.Body, rec.Header())
			}
			if rec.Header().Get(wire.HeaderError) != "" {
				t.Errorf("relayed response is marked as an egress proxy error")
			}
			req, body := up.last()
			if body != "hello" || req.Method != "POST" {
				t.Errorf("upstream got %s %q", req.Method, body)
			}
			if got := req.RequestURI; got != target {
				t.Errorf("request target\n got  %s\n want %s", got, target)
			}
			if req.Host != up.Listener.Addr().String() {
				t.Errorf("Host = %q", req.Host)
			}
			want := ""
			if c.wantBearer != "" {
				want = "Bearer " + c.wantBearer
			}
			if got := req.Header.Get("Authorization"); got != want {
				t.Errorf("Authorization = %q, want %q", got, want)
			}
			for name, values := range req.Header {
				if strings.HasPrefix(strings.ToLower(name), "x-taut-") || name == "Proxy-Authorization" || name == "X-Hop" || name == "X-Forwarded-For" {
					t.Errorf("header %s forwarded upstream", name)
				}
				if strings.Contains(strings.Join(values, " "), canaryInbound) {
					t.Errorf("inbound credential forwarded in %s", name)
				}
			}
			if req.Header.Get("X-Custom") != "kept" || req.Header.Get("Content-Type") != "application/json" {
				t.Errorf("ordinary headers lost: %v", req.Header)
			}
			// The upstream echoed the Authorization header; the worker
			// must not get the token that way.
			for name, values := range rec.Header() {
				if c.wantBearer != "" && strings.Contains(strings.Join(values, " "), c.wantBearer) {
					t.Errorf("token came back in response header %s", name)
				}
			}
		})
	}
}

func TestBadRouting(t *testing.T) {
	up := newUpstream(t, echo)
	r := newHarness(t, nil, "http://"+up.Listener.Addr().String())
	h := r.g.Handler()
	send := func(mut func(*http.Request)) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/x", nil)
		req.Header = wire.Headers(up.URL, noAuth)
		mut(req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	cases := map[string]func(*http.Request){
		"no auth header":       func(q *http.Request) { q.Header.Del(wire.HeaderAuth) },
		"garbage auth":         func(q *http.Request) { q.Header.Set(wire.HeaderAuth, "%%%") },
		"invalid auth":         func(q *http.Request) { q.Header.Set(wire.HeaderAuth, "e30") },
		"two upstreams":        func(q *http.Request) { q.Header.Add(wire.HeaderUpstream, "https://graph.microsoft.com") },
		"upstream with path":   func(q *http.Request) { q.Header.Set(wire.HeaderUpstream, up.URL+"/v1.0") },
		"upstream with query":  func(q *http.Request) { q.Header.Set(wire.HeaderUpstream, up.URL+"?a=1") },
		"upstream with user":   func(q *http.Request) { q.Header.Set(wire.HeaderUpstream, "https://user:pw@graph.microsoft.com") },
		"upstream no scheme":   func(q *http.Request) { q.Header.Set(wire.HeaderUpstream, "graph.microsoft.com") },
		"upstream ftp":         func(q *http.Request) { q.Header.Set(wire.HeaderUpstream, "ftp://graph.microsoft.com") },
		"upstream unparseable": func(q *http.Request) { q.Header.Set(wire.HeaderUpstream, "https://[::1") },
	}
	for name, mut := range cases {
		rec := send(mut)
		if rec.Code != http.StatusBadRequest || errorCode(rec) != CodeBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if up.count() != 0 {
		t.Errorf("bad requests reached the upstream %d times", up.count())
	}
}

func TestAllowlistMatcher(t *testing.T) {
	a, err := newAllowlist([]string{"http://127.0.0.1:8081", "graph=http://fakegraph:9000", "slack=https://slack.test:8443", "https://[::1]:7000", "extra.example"})
	if err != nil {
		t.Fatal(err)
	}
	accept := map[string]string{
		"https://graph.microsoft.com":             ScopeGraph,
		"https://GRAPH.Microsoft.com":             ScopeGraph,
		"https://graph.microsoft.com:443":         ScopeGraph,
		"https://graph.microsoft.us":              ScopeGraph,
		"https://dod-graph.microsoft.us":          ScopeGraph,
		"https://microsoftgraph.chinacloudapi.cn": ScopeGraph,
		"https://contoso.sharepoint.com":          ScopeSharePoint,
		"https://contoso-my.sharepoint.com":       ScopeSharePoint,
		"https://a.b.sharepoint.us":               ScopeSharePoint,
		"https://contoso.sharepoint.cn":           ScopeSharePoint,
		"https://slack.com":                       ScopeSlack,
		"https://files.slack.com":                 ScopeSlackFiles,
		"https://files-origin.slack-files.com":    ScopeSlackFiles,
		"http://127.0.0.1:8081":                   ScopeOther,
		"http://fakegraph:9000":                   ScopeGraph,
		"https://slack.test:8443":                 ScopeSlack,
		"https://[::1]:7000":                      ScopeOther,
		"https://extra.example":                   ScopeOther,
	}
	for raw, want := range accept {
		u, err := parseUpstream(raw)
		if err != nil {
			t.Errorf("%s: %v", raw, err)
			continue
		}
		if got, err := a.check(u); err != nil || got != want {
			t.Errorf("%s: scope %q, err %v; want %q", raw, got, err, want)
		}
	}
	reject := []string{
		"http://graph.microsoft.com",       // plain http
		"https://graph.microsoft.com:8443", // other port
		"https://graph.microsoft.com.evil.example",
		"https://evilgraph.microsoft.com",
		"https://login.microsoftonline.com",
		"https://sharepoint.com",     // the wildcard needs a label
		"https://evilsharepoint.com", // suffix without a dot boundary
		"https://contoso.sharepoint.com.evil.example",
		"https://api.slack.com",
		"https://slack-files.com",
		"https://example.com",
		"https://127.0.0.1",            // IP literal
		"https://127.0.0.1:8081",       // allowed only as http
		"http://127.0.0.1:8082",        // other port
		"http://127.0.0.2:8081",        // other address
		"https://169.254.169.254",      // metadata service
		"http://169.254.169.254",       // metadata service
		"https://[::1]",                // IPv6 literal on another port
		"https://[fd00::1]:7000",       // another IPv6 literal
		"https://fakegraph:9000",       // allowed only as http
		"http://extra.example",         // allowed only as https
		"https://slack.test",           // allowed only on 8443
		"https://exa_mple.com",         // not a DNS name
		"https://graph.microsoft.com.", // trailing dot
	}
	for _, raw := range reject {
		u, err := parseUpstream(raw)
		if err != nil {
			continue // refused even earlier
		}
		if scope, err := a.check(u); err == nil {
			t.Errorf("%s accepted with scope %q", raw, scope)
		}
	}

	for _, bad := range []string{"", "nope=graph.microsoft.com", "ftp://x.example", "*.com", "*", "host:", "a b", "https://x.example/path"} {
		if _, err := newAllowlist([]string{bad}); err == nil {
			t.Errorf("allow entry %q accepted", bad)
		}
	}
	if _, err := New(Config{AllowHosts: []string{"*.com"}}); fail.From(err).Code != "proxy.config_invalid" {
		t.Errorf("New with a bad entry: %v", err)
	}
}

func TestDestinationRefusedOverHTTP(t *testing.T) {
	up := newUpstream(t, echo)
	other := newUpstream(t, echo) // a real server, but not on the allowlist
	r := newHarness(t, nil, "graph=http://"+up.Listener.Addr().String())

	for _, origin := range []string{other.URL, "https://example.com", "http://graph.microsoft.com", "https://10.0.0.1", "http://169.254.169.254"} {
		rec := r.do(call{origin: origin, auth: entraAuth})
		if rec.Code != http.StatusForbidden || errorCode(rec) != CodeDestinationNotAllowed {
			t.Errorf("%s: %d %s", origin, rec.Code, rec.Body)
		}
	}
	// A Slack token is not sent to a Graph host, nor an Entra token to Slack.
	if rec := r.do(call{origin: up.URL, auth: slackAuth}); rec.Code != http.StatusForbidden || errorCode(rec) != CodeDestinationNotAllowed {
		t.Errorf("slack token to graph: %d %s", rec.Code, rec.Body)
	}
	if rec := r.do(call{origin: "https://slack.com", auth: entraAuth, target: "/api/x"}); rec.Code != http.StatusForbidden {
		t.Errorf("entra token to slack: %d %s", rec.Code, rec.Body)
	}
	if up.count()+other.count() != 0 || r.tokens.entraCalls+r.tokens.slackCalls != 0 {
		t.Errorf("refused requests had effects: upstream %d, other %d, tokens %d", up.count(), other.count(), r.tokens.entraCalls+r.tokens.slackCalls)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	evil := newUpstream(t, echo)
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Location", evil.URL+"/steal")
		w.WriteHeader(http.StatusFound)
	})
	r := newHarness(t, nil, "graph=http://"+up.Listener.Addr().String())
	for _, status := range []int{301, 302, 303, 307, 308} {
		up.setHandler(func(w http.ResponseWriter, _ *http.Request, _ int) {
			w.Header().Set("Location", evil.URL+"/steal")
			w.WriteHeader(status)
		})
		rec := r.do(call{origin: up.URL, auth: entraAuth})
		if rec.Code != status || rec.Header().Get("Location") != evil.URL+"/steal" {
			t.Errorf("%d: got %d, Location %q", status, rec.Code, rec.Header().Get("Location"))
		}
	}
	if evil.count() != 0 {
		t.Errorf("the redirect target was contacted %d times", evil.count())
	}
}

func TestUnauthorizedRetry(t *testing.T) {
	accept := func(want string) func(http.ResponseWriter, *http.Request, int) {
		return func(w http.ResponseWriter, r *http.Request, _ int) {
			if r.Header.Get("Authorization") != "Bearer "+want {
				w.Header().Set("WWW-Authenticate", "Bearer")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, "original-401-body")
				return
			}
			_, _ = io.WriteString(w, "ok")
		}
	}
	setup := func(t *testing.T) (*harness, *upstream) {
		up := newUpstream(t, accept(canaryEntra+"-gen1"))
		return newHarness(t, nil, "graph=http://"+up.Listener.Addr().String()), up
	}

	t.Run("no body", func(t *testing.T) {
		r, up := setup(t)
		rec := r.do(call{origin: up.URL, auth: entraAuth})
		if rec.Code != 200 || rec.Body.String() != "ok" || up.count() != 2 || r.tokens.invalidates != 1 {
			t.Errorf("status %d body %q, upstream calls %d, invalidates %d", rec.Code, rec.Body, up.count(), r.tokens.invalidates)
		}
	})
	t.Run("small body is replayed", func(t *testing.T) {
		r, up := setup(t)
		body := strings.Repeat("m", 100<<10)
		rec := r.do(call{method: "POST", origin: up.URL, auth: entraAuth, body: strings.NewReader(body)})
		if rec.Code != 200 || up.count() != 2 {
			t.Fatalf("status %d, upstream calls %d", rec.Code, up.count())
		}
		if up.bodies[0] != body || up.bodies[1] != body {
			t.Errorf("replayed body differs: %d and %d bytes", len(up.bodies[0]), len(up.bodies[1]))
		}
	})
	t.Run("slack", func(t *testing.T) {
		up := newUpstream(t, accept(canarySlack+"-gen1"))
		r := newHarness(t, nil, "slack=http://"+up.Listener.Addr().String())
		if rec := r.do(call{origin: up.URL, auth: slackAuth, target: "/api/conversations.list"}); rec.Code != 200 || up.count() != 2 {
			t.Errorf("status %d, upstream calls %d", rec.Code, up.count())
		}
	})
	t.Run("only once", func(t *testing.T) {
		r, up := setup(t)
		up.setHandler(accept("never"))
		rec := r.do(call{origin: up.URL, auth: entraAuth})
		if rec.Code != 401 || up.count() != 2 || rec.Body.String() != "original-401-body" {
			t.Errorf("status %d body %q, upstream calls %d", rec.Code, rec.Body, up.count())
		}
	})
	t.Run("large body is not retried", func(t *testing.T) {
		r, up := setup(t)
		rec := r.do(call{method: "PUT", origin: up.URL, auth: entraAuth, body: strings.NewReader(strings.Repeat("x", maxReplayBody+1))})
		if rec.Code != 401 || up.count() != 1 || rec.Body.String() != "original-401-body" || rec.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("status %d body %q, upstream calls %d", rec.Code, rec.Body, up.count())
		}
	})
	t.Run("body of unknown length is not retried", func(t *testing.T) {
		r, up := setup(t)
		rec := r.do(call{method: "PUT", origin: up.URL, auth: entraAuth, body: io.MultiReader(strings.NewReader("chunked"))})
		if rec.Code != 401 || up.count() != 1 {
			t.Errorf("status %d, upstream calls %d", rec.Code, up.count())
		}
	})
	t.Run("same token again means no retry", func(t *testing.T) {
		r, up := setup(t)
		r.tokens.frozen = true
		rec := r.do(call{origin: up.URL, auth: entraAuth})
		if rec.Code != 401 || up.count() != 1 || rec.Body.String() != "original-401-body" {
			t.Errorf("status %d body %q, upstream calls %d", rec.Code, rec.Body, up.count())
		}
	})
	t.Run("kind none is never retried", func(t *testing.T) {
		r, up := setup(t)
		if rec := r.do(call{origin: up.URL, auth: noAuth}); rec.Code != 401 || up.count() != 1 || r.tokens.invalidates != 0 {
			t.Errorf("status %d, upstream calls %d", rec.Code, up.count())
		}
	})
}

func TestTokenAndUpstreamFailures(t *testing.T) {
	up := newUpstream(t, echo)
	r := newHarness(t, nil, "graph=http://"+up.Listener.Addr().String())

	r.tokens.err = fail.New("token.broker_unreachable", "The token broker cannot be reached", "nothing listens", "Start it.")
	rec := r.do(call{origin: up.URL, auth: entraAuth})
	if rec.Code != http.StatusBadGateway || errorCode(rec) != "token.broker_unreachable" || up.count() != 0 {
		t.Errorf("token failure: %d %s", rec.Code, rec.Body)
	}
	// It consumed no budget either: the tenant has no bucket yet.
	if n := len(r.g.State().Tenants); n != 0 {
		t.Errorf("a request without a token created %d buckets", n)
	}
	r.tokens.err = nil

	noTokens := newHarness(t, func(c *Config) { c.Tokens = nil }, "graph=http://"+up.Listener.Addr().String())
	if rec := noTokens.do(call{origin: up.URL, auth: entraAuth}); rec.Code != http.StatusBadGateway || errorCode(rec) != CodeNoTokens {
		t.Errorf("no provider: %d %s", rec.Code, rec.Body)
	}
	if rec := noTokens.do(call{origin: up.URL, auth: noAuth}); rec.Code != http.StatusCreated {
		t.Errorf("kind none without a provider: %d", rec.Code)
	}

	up.Close()
	rec = r.do(call{origin: up.URL, auth: entraAuth, target: "/files?t=" + canaryURL})
	if rec.Code != http.StatusBadGateway || errorCode(rec) != CodeUpstreamUnreachable {
		t.Errorf("dead upstream: %d %s", rec.Code, rec.Body)
	}
}

func TestManagementEndpoints(t *testing.T) {
	up := newUpstream(t, echo)
	r := newHarness(t, nil, "graph=http://"+up.Listener.Addr().String())
	r.get(up.URL, http.StatusCreated)
	h := r.g.Handler()
	get := func(method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec
	}
	if rec := get("GET", PathHealthz); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Errorf("healthz: %d %s", rec.Code, rec.Body)
	}
	rec := get("GET", PathState)
	var st State
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil || rec.Code != 200 {
		t.Fatalf("state: %d %s (%v)", rec.Code, rec.Body, err)
	}
	if strings.Contains(rec.Body.String(), "contoso") {
		t.Errorf("the state shows a tenant ID: %s", rec.Body)
	}
	if st.Holder != "gov-a" || len(st.Tenants) != 1 || st.Tenants[0].Tenant != TenantLabel("contoso") || len(st.Tenants[0].Tenant) != 14 || st.Tenants[0].Scope != ScopeGraph ||
		st.Tenants[0].LeaseRPS != 1000 || !st.Tenants[0].Leased || st.Tenants[0].Multiplier != 1 || st.Tenants[0].QueueDepth != 0 {
		t.Errorf("state = %+v", st)
	}
	for _, c := range [][2]string{{"GET", "/v1.0/users"}, {"POST", PathState}, {"GET", "/_proxy/other"}} {
		if rec := get(c[0], c[1]); rec.Code != http.StatusBadRequest || errorCode(rec) != CodeBadRequest {
			t.Errorf("%s %s without upstream: %d %s", c[0], c[1], rec.Code, rec.Body)
		}
	}
	// With the routing header, the same path is ordinary proxied traffic.
	before := up.count()
	if rec := r.do(call{origin: up.URL, auth: noAuth, target: PathState}); rec.Code != http.StatusCreated || up.count() != before+1 {
		t.Errorf("proxied %s: %d", PathState, rec.Code)
	}
}

func TestChannelKey(t *testing.T) {
	cases := []struct{ method, scope, path, want string }{
		{"POST", ScopeGraph, "/v1.0/teams/T1/channels/C1/messages", "t1/c1"},
		{"POST", ScopeGraph, "/beta/teams/T1/channels/C1/messages", "t1/c1"},
		{"POST", ScopeGraph, "/v1.0/teams/T1/channels/C1/messages/M1/replies", "t1/c1"},
		{"POST", ScopeGraph, "/beta/teams/T1/channels/C1/messages/M1/replies", "t1/c1"},
		{"POST", ScopeGraph, "/v1.0/teams/T1/channels/19%3Aabc%40thread.tacv2/messages", "t1/19:abc@thread.tacv2"},
		{"POST", ScopeGraph, "/v1.0/teams/T1/channels/19:abc@thread.tacv2/messages", "t1/19:abc@thread.tacv2"},
		{"POST", ScopeGraph, "/v1.0/teams/a%2Fb/channels/c%2Fd/messages/1%2F2/replies", "a/b/c/d"},
		{"GET", ScopeGraph, "/v1.0/teams/T1/channels/C1/messages", ""},
		{"PATCH", ScopeGraph, "/v1.0/teams/T1/channels/C1/messages/M1", ""},
		{"POST", ScopeSharePoint, "/v1.0/teams/T1/channels/C1/messages", ""},
		{"POST", ScopeOther, "/v1.0/teams/T1/channels/C1/messages", ""},
		{"POST", ScopeGraph, "/v2.0/teams/T1/channels/C1/messages", ""},
		{"POST", ScopeGraph, "/v1.0/teams/T1/channels/C1", ""},
		{"POST", ScopeGraph, "/v1.0/teams/T1/channels", ""},
		{"POST", ScopeGraph, "/v1.0/teams/T1/channels/C1/messages/M1", ""},
		{"POST", ScopeGraph, "/v1.0/teams/T1/channels/C1/messages/M1/replies/R1", ""},
		{"POST", ScopeGraph, "/v1.0/teams/T1/channels/C1/members", ""},
		{"POST", ScopeGraph, "/v1.0/teams/T1/channels/%zz/messages", ""},
		{"POST", ScopeGraph, "/prefix/v1.0/teams/T1/channels/C1/messages", ""},
		// A path Graph serves the same way gets the same bucket: its
		// segments are not case sensitive, and a trailing or doubled
		// slash changes nothing. Otherwise a second spelling of a
		// conversation would be a second allowance for it.
		{"POST", ScopeGraph, "/v1.0/teams/T1/channels/C1/messages/", "t1/c1"},
		{"POST", ScopeGraph, "/v1.0/teams/T1/channels/C1/messages/M1/replies/", "t1/c1"},
		{"POST", ScopeGraph, "/V1.0/Teams/t1/Channels/c1/Messages", "t1/c1"},
		{"POST", ScopeGraph, "/v1.0//teams/T1//channels/C1/messages", "t1/c1"},
		{"POST", ScopeGraph, "/v1.0/teams/T1/channels/C1/MESSAGES/M1/Replies", "t1/c1"},
		{"POST", ScopeGraph, "/v1.0/teams/T1/channels/C1/messages//replies", ""},
		{"POST", ScopeGraph, "/v1.0/teams//channels/C1/messages", ""},
		// A chat has its own bucket, apart from every channel's.
		{"POST", ScopeGraph, "/v1.0/chats/X/messages", "chat:x"},
		{"POST", ScopeGraph, "/beta/chats/X/messages", "chat:x"},
		{"POST", ScopeGraph, "/v1.0/chats/19%3Aabc%40unq.gbl.spaces/messages", "chat:19:abc@unq.gbl.spaces"},
		{"POST", ScopeGraph, "/v1.0/chats/19:abc@thread.v2/messages", "chat:19:abc@thread.v2"},
		{"POST", ScopeGraph, "/v1.0/chats/X/messages/", "chat:x"},
		{"POST", ScopeGraph, "/v1.0/Chats/x/Messages", "chat:x"},
		{"POST", ScopeGraph, "/v1.0/chats//X///messages", "chat:x"},
		{"GET", ScopeGraph, "/v1.0/chats/X/messages", ""},
		{"POST", ScopeSharePoint, "/v1.0/chats/X/messages", ""},
		{"POST", ScopeGraph, "/v1.0/chats", ""},
		{"POST", ScopeGraph, "/v1.0/chats/X", ""},
		{"POST", ScopeGraph, "/v1.0/chats//messages", ""},
		{"POST", ScopeGraph, "/v1.0/chats/%zz/messages", ""},
		{"POST", ScopeGraph, "/v1.0/chats/X/messages/M1", ""},
		{"POST", ScopeGraph, "/v1.0/chats/X/startMigration", ""},
		{"POST", ScopeGraph, "/v1.0/chats/X/completeMigration", ""},
		{"POST", ScopeGraph, "/v1.0/chats/X/members", ""},
		{"POST", ScopeGraph, "/v2.0/chats/X/messages", ""},
		{"POST", ScopeGraph, "/v1.0/users/U/chats/X/messages", ""},
	}
	for _, c := range cases {
		if got := channelKey(c.method, c.scope, c.path); got != c.want {
			t.Errorf("channelKey(%s, %s, %s) = %q, want %q", c.method, c.scope, c.path, got, c.want)
		}
	}
}

// --- streaming ----------------------------------------------------------------------

// pattern is an endless deterministic byte stream.
type pattern struct{ n uint64 }

func (p *pattern) Read(b []byte) (int, error) {
	for i := range b {
		p.n = p.n*6364136223846793005 + 1442695040888963407
		b[i] = byte(p.n >> 56)
	}
	return len(b), nil
}

func heapNow() (heap, total uint64) {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc, m.TotalAlloc
}

func TestStreamingLargeBodies(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 3 × 256 MiB through the proxy")
	}
	const size = 256 << 20
	want := crc32.NewIEEE()
	_, _ = io.CopyN(want, &pattern{}, size)

	var gotUpload atomic.Uint32
	var gotLen atomic.Int64
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Header().Set("Content-Length", fmt.Sprint(size))
			_, _ = io.CopyN(w, &pattern{}, size)
			return
		}
		h := crc32.NewIEEE()
		n, _ := io.Copy(h, r.Body)
		gotLen.Store(n)
		gotUpload.Store(h.Sum32())
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upSrv.Close()

	r := newHarness(t, func(c *Config) { c.Now, c.Sleep = nil, nil }, "graph=http://"+upSrv.Listener.Addr().String())
	front := httptest.NewServer(r.g.Handler())
	defer front.Close()

	// Peak heap, sampled while the transfers run.
	var peak atomic.Uint64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var m runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > peak.Load() {
					peak.Store(m.HeapAlloc)
				}
			}
		}
	}()
	heap0, total0 := heapNow()

	send := func(method string, auth wire.Auth, body io.Reader, length int64) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, front.URL+"/upload/session?t="+canaryURL, body)
		req.ContentLength = length
		for k, v := range wire.Headers(upSrv.URL, auth) {
			req.Header[k] = v
		}
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Upload with a declared length, pre-authorized (a SharePoint upload session).
	resp := send("PUT", noAuth, io.LimitReader(&pattern{}, size), size)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || gotLen.Load() != size || gotUpload.Load() != want.Sum32() {
		t.Fatalf("upload: status %d, %d bytes, crc match %v", resp.StatusCode, gotLen.Load(), gotUpload.Load() == want.Sum32())
	}
	// Upload of unknown length (chunked) on an authenticated request.
	gotLen.Store(0)
	resp = send("PUT", entraFor(upSrv.URL), io.LimitReader(&pattern{}, size), -1)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || gotLen.Load() != size || gotUpload.Load() != want.Sum32() {
		t.Fatalf("chunked upload: status %d, %d bytes", resp.StatusCode, gotLen.Load())
	}
	// Download.
	resp = send("GET", noAuth, nil, 0)
	h := crc32.NewIEEE()
	n, err := io.Copy(h, resp.Body)
	resp.Body.Close()
	if err != nil || n != size || h.Sum32() != want.Sum32() || resp.ContentLength != size {
		t.Fatalf("download: %d bytes, err %v, crc match %v, Content-Length %d", n, err, h.Sum32() == want.Sum32(), resp.ContentLength)
	}

	close(stop)
	wg.Wait()
	heap1, total1 := heapNow()
	const limit = 48 << 20
	t.Logf("768 MiB moved: peak heap +%d KiB, live heap %+d KiB, total allocated %d KiB",
		(int64(peak.Load())-int64(heap0))>>10, (int64(heap1)-int64(heap0))>>10, (total1-total0)>>10)
	if p := peak.Load(); p > heap0+limit {
		t.Errorf("peak heap grew by %d MiB while streaming; bodies are being buffered", (p-heap0)>>20)
	}
	if total1-total0 > limit {
		t.Errorf("%d MiB allocated in total while streaming 768 MiB; bodies are being copied into memory", (total1-total0)>>20)
	}
}

// --- concurrency --------------------------------------------------------------------

func TestConcurrentRequests(t *testing.T) {
	var hits atomic.Int64
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+canaryEntra {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		_, _ = io.WriteString(w, r.URL.Path)
	}))
	defer upSrv.Close()
	r := newHarness(t, func(c *Config) { c.MaxChannels = 16 }, "graph=http://"+upSrv.Listener.Addr().String())
	r.leases.set(50, nil) // low enough that requests queue
	front := httptest.NewServer(r.g.Handler())
	defer front.Close()

	const workers, each = 64, 20
	var wg sync.WaitGroup
	var bad atomic.Int64
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				method, path := "GET", fmt.Sprintf("/v1.0/users/%d-%d", w, i)
				if i%2 == 1 {
					method, path = "POST", fmt.Sprintf("/v1.0/teams/T/channels/C%d/messages", w)
				}
				req, _ := http.NewRequest(method, front.URL+path, strings.NewReader("{}"))
				for k, v := range wire.Headers(upSrv.URL, entraFor(upSrv.URL)) {
					req.Header[k] = v
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					bad.Add(1)
					continue
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 || string(body) != path {
					bad.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if bad.Load() != 0 || hits.Load() != workers*each {
		t.Errorf("%d bad responses; upstream saw %d requests, want exactly %d", bad.Load(), hits.Load(), workers*each)
	}
	st := r.g.State()
	if st.Channels > 16 || st.Tenants[0].QueueDepth != 0 {
		t.Errorf("after the run: %d channels, queue depth %d", st.Channels, st.Tenants[0].QueueDepth)
	}
}

func TestServeShutsDown(t *testing.T) {
	r := newHarness(t, func(c *Config) { c.Now, c.Sleep = nil, nil })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.g.Serve(ctx, "127.0.0.1:0") }()
	eventually(t, "the listening log line", func() bool { return strings.Contains(r.logs.String(), "egress proxy listening") })
	line := r.logs.String()
	i := strings.Index(line, "addr=")
	addr := strings.Fields(line[i+len("addr="):])[0]
	resp, err := http.Get((&url.URL{Scheme: "http", Host: addr, Path: PathHealthz}).String())
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz over TCP: %v", err)
	}
	resp.Body.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
	if strings.Contains(r.logs.String(), "non-loopback") {
		t.Error("loopback address was reported as non-loopback")
	}
}
