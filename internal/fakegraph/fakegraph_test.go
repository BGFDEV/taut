package fakegraph

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec // thumbprints
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSeedRoundTripsThroughJSON(t *testing.T) {
	seed := DefaultSeed()
	seed.Apps[0].Certificates = []Certificate{{Thumbprint: strings.Repeat("AB", 20)}}
	data, err := json.MarshalIndent(seed, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "seed.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSeed(path)
	if err != nil {
		t.Fatalf("LoadSeed: %v", err)
	}
	if !reflect.DeepEqual(seed, loaded) {
		t.Errorf("seed changed in a JSON round trip:\n%s", data)
	}
	for _, key := range []string{`"tenantId"`, `"clientId"`, `"secrets"`, `"roles"`, `"userPrincipalName"`, `"accountEnabled"`, `"channels"`, `"membershipType"`} {
		if !bytes.Contains(data, []byte(key)) {
			t.Errorf("seed JSON lacks %s", key)
		}
	}
}

func TestLoadSeedRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for name, body := range map[string]string{
		"typo.json":      `{"tenantId":"t","usres":[]}`,
		"notenant.json":  `{"users":[]}`,
		"dupuser.json":   `{"tenantId":"t","users":[{"id":"1","userPrincipalName":"a@x"},{"id":"1","userPrincipalName":"b@x"}]}`,
		"badthumb.json":  `{"tenantId":"t","apps":[{"clientId":"c","certificates":[{"thumbprint":"zz"}],"roles":[]}]}`,
		"badpem.json":    `{"tenantId":"t","apps":[{"clientId":"c","certificates":[{"pem":"nope"}],"roles":[]}]}`,
		"malformed.json": `{`,
	} {
		if _, err := LoadSeed(write(name, body)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if _, err := LoadSeed(filepath.Join(dir, "absent.json")); err == nil {
		t.Error("missing file: want error")
	}
	if err := DefaultSeed().Validate(); err != nil {
		t.Errorf("DefaultSeed is invalid: %v", err)
	}
	defer func() {
		if recover() == nil {
			t.Error("New should panic on an invalid seed")
		}
	}()
	New(Seed{})
}

func TestMatchPath(t *testing.T) {
	for _, tc := range []struct {
		pattern, path string
		want          bool
	}{
		{"", "/anything/at/all", true},
		{"/v1.0/users", "/v1.0/users", true},
		{"/v1.0/users", "/v1.0/users/abc", false},
		{"/v1.0/users/*", "/v1.0/users/abc", true},
		{"/v1.0/users/*", "/v1.0/users", false},
		{"/v1.0/teams/*/channels", "/v1.0/teams/t1/channels", true},
		{"/v1.0/teams/*/channels", "/v1.0/teams/t1/members", false},
		{"/v1.0/**", "/v1.0/teams/t1/channels", true},
		{"/v1.0/**", "/tenant/oauth2/v2.0/token", false},
		{"/*/oauth2/v2.0/token", "/tenant/oauth2/v2.0/token", true},
		{"/v1.0/u*", "/v1.0/users", true},
	} {
		if got := matchPath(tc.pattern, tc.path); got != tc.want {
			t.Errorf("matchPath(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestFaultValidation(t *testing.T) {
	f := newFaults(1)
	for _, bad := range []Fault{
		{},
		{Action: "explode"},
		{Action: ActionStatus, Status: 200},
		{Action: ActionLatency},
		{Action: ActionDrop, Count: -1},
		{Action: ActionDrop, Probability: 1.5},
		{Action: ActionDrop, Path: "/v1.0/[bad"},
	} {
		if _, err := f.Add(bad); err == nil {
			t.Errorf("Add(%+v): want error", bad)
		}
	}
	id, err := f.Add(Fault{Method: "get", Action: ActionDrop})
	if err != nil || id != "fault-1" || f.List()[0].Method != "GET" {
		t.Errorf("Add = %q, %v, %+v", id, err, f.List())
	}
	if !f.Remove(id) || f.Remove(id) || len(f.List()) != 0 {
		t.Error("Remove misbehaved")
	}
}

func TestProbabilisticFaultsAreDeterministic(t *testing.T) {
	run := func(seed uint64) string {
		f := newFaults(seed)
		f.MustAdd(Fault{Action: ActionDrop, Probability: 0.3})
		var sb strings.Builder
		for range 200 {
			r := httptest.NewRequest("GET", "/v1.0/users", nil)
			if _, hit := f.decide(r); hit != nil {
				sb.WriteByte('x')
			} else {
				sb.WriteByte('.')
			}
		}
		return sb.String()
	}
	a, b, c := run(7), run(7), run(8)
	if a != b {
		t.Error("same seed produced different fault sequences")
	}
	if a == c {
		t.Error("different seeds produced identical sequences")
	}
	if hits := strings.Count(a, "x"); hits < 35 || hits > 85 {
		t.Errorf("probability 0.3 fired %d/200 times", hits)
	}
}

func TestLimiter(t *testing.T) {
	l := newLimiter()
	now := time.Unix(1_800_000_000, 0)
	for i := range 3 {
		if ok, _ := l.allow("a", 2, 3, now); !ok {
			t.Fatalf("burst call %d denied", i)
		}
	}
	ok, wait := l.allow("a", 2, 3, now)
	if ok || wait != 500*time.Millisecond {
		t.Errorf("empty bucket: ok=%v wait=%v, want false 500ms", ok, wait)
	}
	if ok, _ := l.allow("b", 2, 3, now); !ok {
		t.Error("buckets must be independent per key")
	}
	if ok, _ := l.allow("a", 2, 3, now.Add(500*time.Millisecond)); !ok {
		t.Error("bucket did not refill")
	}
	if ok, _ := l.allow("a", 2, 3, now.Add(time.Hour)); !ok {
		t.Error("bucket did not refill after a long idle")
	}
	for range 2 { // refill is capped at burst (3), one was just used
		l.allow("a", 2, 3, now.Add(time.Hour))
	}
	if ok, _ := l.allow("a", 2, 3, now.Add(time.Hour)); ok {
		t.Error("bucket exceeded its burst after idling")
	}
}

// harness drives the fake over real HTTP without the graph client, to pin
// down the wire behavior itself.
type harness struct {
	t   *testing.T
	s   *Server
	srv *httptest.Server
	now time.Time
	mu  sync.Mutex
}

func newHarness(t *testing.T, seed Seed, opts ...Option) *harness {
	h := &harness{t: t, now: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)}
	opts = append([]Option{WithClock(func() time.Time {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.now
	})}, opts...)
	h.s = New(seed, opts...)
	h.srv = httptest.NewServer(h.s)
	t.Cleanup(h.srv.Close)
	return h
}

func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	h.mu.Unlock()
}

func (h *harness) do(method, path, bearer string, body io.Reader) (*http.Response, map[string]any) {
	h.t.Helper()
	req, err := http.NewRequest(method, h.srv.URL+path, body)
	if err != nil {
		h.t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if method == http.MethodPost && strings.HasSuffix(path, "/token") {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp, out
}

func (h *harness) tokenRequest(form url.Values) (*http.Response, map[string]any) {
	h.t.Helper()
	return h.do("POST", "/"+DefaultTenantID+"/oauth2/v2.0/token", "", strings.NewReader(form.Encode()))
}

func (h *harness) secretForm() url.Values {
	return url.Values{
		"grant_type": {"client_credentials"}, "client_id": {DefaultClientID},
		"client_secret": {DefaultSecret}, "scope": {h.srv.URL + "/.default"},
	}
}

func (h *harness) token() string {
	h.t.Helper()
	resp, body := h.tokenRequest(h.secretForm())
	if resp.StatusCode != 200 {
		h.t.Fatalf("token request: %d %v", resp.StatusCode, body)
	}
	return body["access_token"].(string)
}

func graphCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

func TestTokenEndpointValidation(t *testing.T) {
	h := newHarness(t, DefaultSeed())
	mod := func(k, v string) url.Values {
		f := h.secretForm()
		if v == "" {
			f.Del(k)
		} else {
			f.Set(k, v)
		}
		return f
	}
	for _, tc := range []struct {
		name   string
		form   url.Values
		status int
		oauth  string
		aadsts float64
	}{
		{"bad grant", mod("grant_type", "password"), 400, "unsupported_grant_type", 70003},
		{"no client id", mod("client_id", ""), 400, "invalid_request", 900144},
		{"unknown client", mod("client_id", "nope"), 400, "unauthorized_client", 700016},
		{"scope without .default", mod("scope", h.srv.URL+"/User.Read"), 400, "invalid_scope", 1002012},
		{"no scope", mod("scope", ""), 400, "invalid_scope", 1002012},
		{"wrong secret", mod("client_secret", "nope"), 401, "invalid_client", 7000215},
		{"no credential", mod("client_secret", ""), 401, "invalid_client", 7000216},
		{"garbage assertion", func() url.Values {
			f := mod("client_secret", "")
			f.Set("client_assertion_type", assertionType)
			f.Set("client_assertion", "a.b.c")
			return f
		}(), 401, "invalid_client", 50027},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := h.tokenRequest(tc.form)
			codes, _ := body["error_codes"].([]any)
			if resp.StatusCode != tc.status || body["error"] != tc.oauth || len(codes) != 1 || codes[0] != tc.aadsts {
				t.Errorf("got %d %v", resp.StatusCode, body)
			}
			desc, _ := body["error_description"].(string)
			if !strings.HasPrefix(desc, fmt.Sprintf("AADSTS%d: ", int(tc.aadsts))) || !strings.Contains(desc, "Trace ID: ") ||
				body["trace_id"] == "" || body["correlation_id"] == "" {
				t.Errorf("error body is not Entra-shaped: %v", body)
			}
		})
	}

	resp, body := h.do("POST", "/other-tenant/oauth2/v2.0/token", "", strings.NewReader(h.secretForm().Encode()))
	if resp.StatusCode != 400 || !strings.HasPrefix(body["error_description"].(string), "AADSTS90002") {
		t.Errorf("unknown tenant: %d %v", resp.StatusCode, body)
	}
	if resp, _ := h.do("GET", "/"+DefaultTenantID+"/oauth2/v2.0/token", "", nil); resp.StatusCode != 405 {
		t.Errorf("GET token endpoint: %d", resp.StatusCode)
	}

	resp, body = h.tokenRequest(h.secretForm())
	if resp.StatusCode != 200 || body["token_type"] != "Bearer" || body["expires_in"] != float64(3600) {
		t.Errorf("success body: %d %v", resp.StatusCode, body)
	}
	// The request log names the app but never the secret.
	logJSON, _ := json.Marshal(h.s.Requests())
	if bytes.Contains(logJSON, []byte(DefaultSecret)) || !bytes.Contains(logJSON, []byte(DefaultClientID)) {
		t.Errorf("request log: %s", logJSON)
	}
}

// assertionFor builds a client assertion with full control over its parts.
func assertionFor(key *rsa.PrivateKey, x5t []byte, alg string, claims map[string]any) string {
	header, _ := json.Marshal(map[string]any{"alg": alg, "typ": "JWT", "x5t": b64(x5t)})
	payload, _ := json.Marshal(claims)
	input := b64(header) + "." + b64(payload)
	digest := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		panic(err)
	}
	return input + "." + b64(sig)
}

func selfSigned(t *testing.T, cn string) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return key, der
}

func TestClientAssertionValidation(t *testing.T) {
	key, der := selfSigned(t, "registered")
	otherKey, _ := selfSigned(t, "attacker")
	thumb := sha1.Sum(der) //nolint:gosec // thumbprint
	seed := DefaultSeed()
	seed.Apps[0].Secrets = nil
	seed.Apps[0].Certificates = []Certificate{{
		Thumbprint: strings.ToUpper(hex.EncodeToString(thumb[:])),
		PEM:        string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
	}}
	h := newHarness(t, seed)
	endpoint := h.srv.URL + "/" + DefaultTenantID + "/oauth2/v2.0/token"
	now := h.now.Unix()
	good := func() map[string]any {
		return map[string]any{"aud": endpoint, "iss": DefaultClientID, "sub": DefaultClientID, "jti": "j", "nbf": now, "iat": now, "exp": now + 600}
	}
	with := func(k string, v any) map[string]any { c := good(); c[k] = v; return c }
	otherThumb := sha1.Sum([]byte("unregistered")) //nolint:gosec // thumbprint

	for _, tc := range []struct {
		name      string
		assertion string
		aadsts    float64 // zero means success
	}{
		{"valid", assertionFor(key, thumb[:], "RS256", good()), 0},
		{"valid with v2.0 issuer audience", assertionFor(key, thumb[:], "RS256", with("aud", h.srv.URL+"/"+DefaultTenantID+"/v2.0")), 0},
		{"unregistered thumbprint", assertionFor(key, otherThumb[:], "RS256", good()), 700027},
		{"registered thumbprint, wrong key", assertionFor(otherKey, thumb[:], "RS256", good()), 700027},
		{"wrong alg", assertionFor(key, thumb[:], "PS256", good()), 700027},
		{"issuer is another app", assertionFor(key, thumb[:], "RS256", with("iss", "someone-else")), 700021},
		{"audience is another host", assertionFor(key, thumb[:], "RS256", with("aud", "https://login.microsoftonline.com/"+DefaultTenantID+"/oauth2/v2.0/token")), 700023},
		{"expired", assertionFor(key, thumb[:], "RS256", with("exp", now-3600)), 700024},
		{"not yet valid", assertionFor(key, thumb[:], "RS256", with("nbf", now+3600)), 700024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			form := url.Values{
				"grant_type": {"client_credentials"}, "client_id": {DefaultClientID}, "scope": {h.srv.URL + "/.default"},
				"client_assertion_type": {assertionType}, "client_assertion": {tc.assertion},
			}
			resp, body := h.tokenRequest(form)
			if tc.aadsts == 0 {
				if resp.StatusCode != 200 || body["access_token"] == nil {
					t.Fatalf("want token, got %d %v", resp.StatusCode, body)
				}
				return
			}
			codes, _ := body["error_codes"].([]any)
			if resp.StatusCode != 401 || len(codes) != 1 || codes[0] != tc.aadsts {
				t.Errorf("got %d %v, want AADSTS%d", resp.StatusCode, body, int(tc.aadsts))
			}
		})
	}
}

func TestBearerValidation(t *testing.T) {
	h := newHarness(t, DefaultSeed(), WithTokenLifetime(10*time.Minute))
	token := h.token()

	check := func(name, bearer string, wantStatus int, wantMsg string) {
		t.Helper()
		resp, body := h.do("GET", "/v1.0/organization", bearer, nil)
		if resp.StatusCode != wantStatus {
			t.Errorf("%s: status %d, want %d (%v)", name, resp.StatusCode, wantStatus, body)
			return
		}
		if wantStatus != 401 {
			return
		}
		e, _ := body["error"].(map[string]any)
		inner, _ := e["innerError"].(map[string]any)
		if graphCode(body) != "InvalidAuthenticationToken" || !strings.Contains(e["message"].(string), wantMsg) ||
			inner["request-id"] == nil || inner["date"] == nil {
			t.Errorf("%s: body %v", name, body)
		}
		if wa := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(wa, "Bearer ") || !strings.Contains(wa, `error="invalid_token"`) {
			t.Errorf("%s: WWW-Authenticate = %q", name, wa)
		}
	}
	check("valid", token, 200, "")
	check("missing", "", 401, "empty")
	check("garbage", "abc.def.ghi", 401, "validation failure")
	check("tampered", token[:len(token)-4]+"AAAA", 401, "validation failure")

	// A token signed by another fake instance (another "tenant") is refused.
	other := newHarness(t, DefaultSeed())
	check("foreign signature", other.token(), 401, "validation failure")

	// A token whose audience is a different Graph host is refused...
	form := h.secretForm()
	form.Set("scope", "https://graph.microsoft.us/.default")
	_, body := h.tokenRequest(form)
	wrongAud := body["access_token"].(string)
	check("wrong audience", wrongAud, 401, "Invalid audience")
	// ...unless the server was told a proxy rewrites Host.
	lax := newHarness(t, DefaultSeed(), WithLaxAudience())
	form = lax.secretForm()
	form.Set("scope", "https://graph.microsoft.us/.default")
	_, body = lax.tokenRequest(form)
	if resp, _ := lax.do("GET", "/v1.0/organization", body["access_token"].(string), nil); resp.StatusCode != 200 {
		t.Errorf("lax audience: %d", resp.StatusCode)
	}

	h.advance(10 * time.Minute)
	check("expired", token, 401, "expired")
}

func TestGraphWireDetails(t *testing.T) {
	h := newHarness(t, DefaultSeed(), WithPageSize(3))
	token := h.token()
	get := func(path string) (*http.Response, map[string]any) { return h.do("GET", path, token, nil) }

	// Without $select Graph omits accountEnabled, userType, proxyAddresses.
	_, body := get("/v1.0/users")
	first := body["value"].([]any)[0].(map[string]any)
	if _, has := first["accountEnabled"]; has || first["displayName"] != "Ada Lovelace" || len(first) != 4 {
		t.Errorf("default user projection = %v", first)
	}
	next, _ := body["@odata.nextLink"].(string)
	if !strings.HasPrefix(next, h.srv.URL+"/v1.0/users?") || !strings.Contains(next, "skiptoken") {
		t.Fatalf("nextLink = %q", next)
	}
	// The next link is usable verbatim and carries the query forward.
	_, body = get("/v1.0/users?$select=id,userType&$top=2")
	next = body["@odata.nextLink"].(string)
	resp, body := h.do("GET", strings.TrimPrefix(next, h.srv.URL), token, nil)
	page := body["value"].([]any)
	if resp.StatusCode != 200 || len(page) != 2 || page[0].(map[string]any)["id"] != "a0000000-0000-4000-8000-000000000003" || len(page[0].(map[string]any)) != 2 {
		t.Errorf("second page: %d %v", resp.StatusCode, body)
	}
	// The last page has no next link.
	_, body = get("/v1.0/users?$top=999")
	if _, has := body["@odata.nextLink"]; has || len(body["value"].([]any)) != 7 {
		t.Errorf("full page: %v", body)
	}

	for path, wantStatus := range map[string]int{
		"/v1.0/users?$top=0":                               400,
		"/v1.0/users?$top=1000":                            400,
		"/v1.0/users?$skiptoken=garbage":                   400,
		"/v1.0/users?$select=shoeSize":                     400,
		"/v1.0/users?$orderby=displayName":                 400,
		"/v1.0/users?$filter=mail%20ne%20%27x%27":          400,
		"/v1.0/users/nobody":                               404,
		"/v1.0/teams/nope":                                 404,
		"/v1.0/teams/nope/channels":                        404,
		"/v1.0/teams/b0000000-0000-4000-8000-000000000001": 200,
		"/v1.0/me": 501,
	} {
		if resp, body := get(path); resp.StatusCode != wantStatus {
			t.Errorf("GET %s = %d, want %d (%v)", path, resp.StatusCode, wantStatus, body)
		}
	}
	if resp, body := h.do("POST", "/v1.0/subscriptions", token, strings.NewReader("{}")); resp.StatusCode != 501 || graphCode(body) != "fakegraph_NotImplemented" {
		t.Errorf("unimplemented write: %d %v", resp.StatusCode, body)
	}
	if resp, _ := h.do("GET", "/nothing/here", "", nil); resp.StatusCode != 404 {
		t.Errorf("unknown path: %d", resp.StatusCode)
	}
}

func TestRoleEnforcementPerEndpoint(t *testing.T) {
	for path, role := range map[string]string{
		"/v1.0/organization": "Organization.Read.All",
		"/v1.0/users":        "User.Read.All",
		"/v1.0/users/a0000000-0000-4000-8000-000000000001": "User.Read.All",
		"/v1.0/teams": "Team.ReadBasic.All",
		"/v1.0/teams/b0000000-0000-4000-8000-000000000001/channels": "Channel.ReadBasic.All",
	} {
		seed := DefaultSeed()
		seed.Apps[0].Roles = []string{"Teamwork.Migrate.All"}
		h := newHarness(t, seed)
		resp, body := h.do("GET", path, h.token(), nil)
		if resp.StatusCode != 403 || graphCode(body) != "Authorization_RequestDenied" {
			t.Errorf("%s without %s: %d %v", path, role, resp.StatusCode, body)
		}
		seed.Apps[0].Roles = []string{role}
		h = newHarness(t, seed)
		if resp, body := h.do("GET", path, h.token(), nil); resp.StatusCode != 200 {
			t.Errorf("%s with %s: %d %v", path, role, resp.StatusCode, body)
		}
	}
}

func TestThrottlingIsPerApp(t *testing.T) {
	seed := DefaultSeed()
	seed.Apps = append(seed.Apps, App{ClientID: "second-app", Secrets: []string{"s2"}, Roles: DefaultRoles()})
	h := newHarness(t, seed, WithRateLimit(0.5, 1))
	first := h.token()
	form := h.secretForm()
	form.Set("client_id", "second-app")
	form.Set("client_secret", "s2")
	_, body := h.tokenRequest(form)
	second := body["access_token"].(string)

	if resp, _ := h.do("GET", "/v1.0/organization", first, nil); resp.StatusCode != 200 {
		t.Fatalf("first call: %d", resp.StatusCode)
	}
	resp, body := h.do("GET", "/v1.0/organization", first, nil)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "2" || graphCode(body) != "TooManyRequests" {
		t.Errorf("throttled call: %d Retry-After=%q %v", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
	if resp, _ := h.do("GET", "/v1.0/organization", second, nil); resp.StatusCode != 200 {
		t.Errorf("another app shares the first app's bucket: %d", resp.StatusCode)
	}
	h.advance(2 * time.Second)
	if resp, _ := h.do("GET", "/v1.0/organization", first, nil); resp.StatusCode != 200 {
		t.Errorf("after Retry-After: %d", resp.StatusCode)
	}
}

func TestControlEndpoints(t *testing.T) {
	h := newHarness(t, DefaultSeed())
	token := h.token()
	post := func(body string) (*http.Response, map[string]any) {
		return h.do("POST", "/_fake/faults", "", strings.NewReader(body))
	}

	if resp, body := h.do("GET", "/healthz", "", nil); resp.StatusCode != 200 || body["status"] != "ok" {
		t.Errorf("healthz: %d %v", resp.StatusCode, body)
	}

	resp, body := post(`{"method":"GET","path":"/v1.0/users","action":"status","status":503,"count":1}`)
	if resp.StatusCode != 201 || len(body["ids"].([]any)) != 1 {
		t.Fatalf("POST fault: %d %v", resp.StatusCode, body)
	}
	resp, body = post(`[{"path":"/v1.0/teams","action":"throttle","retryAfterSeconds":9},{"path":"/v1.0/teams","action":"drop"}]`)
	if resp.StatusCode != 201 || len(body["ids"].([]any)) != 2 {
		t.Fatalf("POST faults: %d %v", resp.StatusCode, body)
	}
	// Invalid input installs nothing, even when part of a batch is valid.
	for _, bad := range []string{`{"action":"status"}`, `{"action":"drop","oops":1}`, `not json`, `[{"action":"drop"},{"action":"nope"}]`} {
		if resp, _ := post(bad); resp.StatusCode != 400 {
			t.Errorf("POST %s: %d, want 400", bad, resp.StatusCode)
		}
	}
	if n := len(h.s.Faults().List()); n != 3 {
		t.Fatalf("faults installed = %d, want 3", n)
	}

	if resp, _ := h.do("GET", "/v1.0/users", token, nil); resp.StatusCode != 503 {
		t.Errorf("faulted call: %d", resp.StatusCode)
	}
	if resp, _ := h.do("GET", "/v1.0/users", token, nil); resp.StatusCode != 200 {
		t.Errorf("count-limited fault should be spent: %d", resp.StatusCode)
	}
	resp, _ = h.do("GET", "/v1.0/teams", token, nil)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "9" {
		t.Errorf("throttle fault: %d %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}

	_, body = h.do("GET", "/_fake/faults", "", nil)
	faults := body["faults"].([]any)
	if f0 := faults[0].(map[string]any); f0["fired"] != float64(1) || f0["exhausted"] != true {
		t.Errorf("fault status = %v", f0)
	}

	_, body = h.do("GET", "/_fake/requests", "", nil)
	reqs := body["requests"].([]any)
	if len(reqs) != 4 { // token + three graph calls; control calls are not logged
		t.Fatalf("request log has %d entries: %v", len(reqs), reqs)
	}
	if r := reqs[1].(map[string]any); r["status"] != float64(503) || r["appId"] != DefaultClientID || r["fault"] != "fault-1" || r["path"] != "/v1.0/users" {
		t.Errorf("logged request = %v", r)
	}

	resp, raw := h.do("GET", "/_fake/state", "", nil)
	stateJSON, _ := json.Marshal(raw)
	if resp.StatusCode != 200 || raw["tenantId"] != DefaultTenantID || len(raw["users"].([]any)) != 7 ||
		bytes.Contains(stateJSON, []byte(DefaultSecret)) || raw["requestCount"] != float64(4) {
		t.Errorf("state: %s", stateJSON)
	}
	if app := raw["apps"].([]any)[0].(map[string]any); app["secretCount"] != float64(1) || app["clientId"] != DefaultClientID {
		t.Errorf("state app = %v", app)
	}

	if resp, _ := h.do("DELETE", "/_fake/faults?id=fault-2", "", nil); resp.StatusCode != 204 {
		t.Errorf("DELETE one: %d", resp.StatusCode)
	}
	if resp, _ := h.do("DELETE", "/_fake/faults?id=fault-2", "", nil); resp.StatusCode != 404 {
		t.Errorf("DELETE missing: %d", resp.StatusCode)
	}
	if resp, _ := h.do("DELETE", "/_fake/faults", "", nil); resp.StatusCode != 204 || len(h.s.Faults().List()) != 0 {
		t.Errorf("DELETE all: %d, %d left", resp.StatusCode, len(h.s.Faults().List()))
	}
	if resp, _ := h.do("DELETE", "/_fake/requests", "", nil); resp.StatusCode != 204 || len(h.s.Requests()) != 0 {
		t.Errorf("DELETE requests: %d", resp.StatusCode)
	}
	// Control endpoints are immune to faults.
	h.s.Faults().MustAdd(Fault{Action: ActionDrop})
	if resp, _ := h.do("GET", "/healthz", "", nil); resp.StatusCode != 200 {
		t.Errorf("healthz under a catch-all fault: %d", resp.StatusCode)
	}
}

func TestFaultsCanBreakTheTokenEndpoint(t *testing.T) {
	h := newHarness(t, DefaultSeed())
	h.s.Faults().MustAdd(Fault{Method: "POST", Path: "/*/oauth2/v2.0/token", Action: ActionStatus, Status: 500, Count: 1})
	if resp, _ := h.tokenRequest(h.secretForm()); resp.StatusCode != 500 {
		t.Errorf("faulted token request: %d", resp.StatusCode)
	}
	if resp, _ := h.tokenRequest(h.secretForm()); resp.StatusCode != 200 {
		t.Errorf("token request after fault: %d", resp.StatusCode)
	}
}

func TestConcurrentUse(t *testing.T) {
	h := newHarness(t, DefaultSeed())
	token := h.token()
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			for j := range 10 {
				switch (i + j) % 4 {
				case 0:
					h.do("GET", "/v1.0/users?$top=2", token, nil)
				case 1:
					id := h.s.Faults().MustAdd(Fault{Path: "/v1.0/none", Action: ActionLatency, LatencyMS: 1})
					h.s.Faults().Remove(id)
				case 2:
					h.do("GET", "/_fake/state", "", nil)
				case 3:
					h.tokenRequest(h.secretForm())
				}
			}
		})
	}
	wg.Wait()
	if h.s.CountRequests("GET", "/v1.0/users") != 40 {
		t.Errorf("users calls = %d, want 40", h.s.CountRequests("GET", "/v1.0/users"))
	}
}

func TestServeShutsDownGracefully(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ServeListener(ctx, ln, DefaultSeed()) }()

	url := "http://" + ln.Addr().String() + "/healthz"
	var resp *http.Response
	for range 200 {
		if resp, err = http.Get(url); err == nil { //nolint:noctx // test
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("server never came up: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("healthz = %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v, want nil after graceful shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancellation")
	}
	if _, err := http.Get(url); err == nil { //nolint:noctx // test
		t.Error("server still answering after shutdown")
	}

	if err := Serve(context.Background(), "127.0.0.1:0", Seed{}); err == nil {
		t.Error("Serve with an invalid seed: want error")
	}
	if err := Serve(context.Background(), "256.0.0.1:99999", DefaultSeed()); err == nil {
		t.Error("Serve on a bad address: want error")
	}
}
