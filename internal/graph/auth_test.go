package graph_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/fakegraph"
	"github.com/bgfdev/taut/internal/graph"
)

// wantFail asserts err is a structured error with the given code and a
// complete what/why/next, and returns it.
func wantFail(t *testing.T, err error, code string) *fail.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("want error %s, got nil", code)
	}
	var fe *fail.Error
	if !errors.As(err, &fe) {
		t.Fatalf("want *fail.Error %s, got %T: %v", code, err, err)
	}
	if fe.Code != code {
		t.Fatalf("code = %q, want %q (error: %v)", fe.Code, code, err)
	}
	if fe.What == "" || fe.Why == "" || fe.Next == "" {
		t.Fatalf("%s must have what, why and next: %+v", code, *fe)
	}
	return fe
}

// assertNoSecrets renders err every way an error can reach an operator or a
// log and fails if any secret appears.
func assertNoSecrets(t *testing.T, err error, secrets ...string) {
	t.Helper()
	var rendered []string
	for e := err; e != nil; e = errors.Unwrap(e) {
		rendered = append(rendered, e.Error(), fmt.Sprintf("%v|%+v|%#v|%s|%q", e, e, e, e, e))
		var fe *fail.Error
		if errors.As(e, &fe) {
			js, _ := json.Marshal(fe)
			rendered = append(rendered, fe.What, fe.Why, fe.Next, string(js))
		}
	}
	all := strings.Join(rendered, "\n")
	for _, s := range secrets {
		if s == "" {
			t.Fatal("empty secret passed to assertNoSecrets")
		}
		if strings.Contains(all, s) {
			t.Errorf("error output leaks secret material %q...:\n%s", s[:min(8, len(s))], all)
		}
	}
	if strings.Contains(strings.ToLower(all), "bearer ey") {
		t.Errorf("error output contains a bearer token:\n%s", all)
	}
}

func TestSecretAuthAndClaims(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	org, err := e.client.Organization(ctx)
	if err != nil {
		t.Fatalf("Organization: %v", err)
	}
	if org.ID != fakegraph.DefaultTenantID || org.DisplayName != "Contoso Ltd" {
		t.Errorf("org = %+v", org)
	}
	if len(org.VerifiedDomains) != 2 || !org.VerifiedDomains[0].IsDefault || org.VerifiedDomains[0].Name != "contoso.example" {
		t.Errorf("domains = %+v", org.VerifiedDomains)
	}

	token, err := e.tokens.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	roles, err := graph.Roles(token)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(roles, fakegraph.DefaultRoles()) {
		t.Errorf("roles = %v", roles)
	}
	claims, err := graph.ParseClaims(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.TenantID != fakegraph.DefaultTenantID || claims.AppID != fakegraph.DefaultClientID {
		t.Errorf("claims = %+v", claims)
	}
	if want := e.clock.Now().Add(time.Hour); !claims.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", claims.ExpiresAt, want)
	}
	if claims.Audience != e.http.URL {
		t.Errorf("Audience = %q, want %q", claims.Audience, e.http.URL)
	}

	// Everything in the catalog is granted by the default seed.
	for _, st := range graph.Compare(roles, graph.FeatureSet{PrivateChannels: true, DirectMessages: true}) {
		if st.State != graph.PermissionGranted {
			t.Errorf("%s: state %s", st.Name, st.State)
		}
	}
}

func TestTenantAddressedByDomain(t *testing.T) {
	e := newEnv(t, withTenant("contoso.example"))
	if _, err := e.client.Organization(context.Background()); err != nil {
		t.Fatalf("Organization: %v", err)
	}
}

func certSeed(k keyPair) fakegraph.Seed {
	seed := fakegraph.DefaultSeed()
	seed.Apps[0].Secrets = nil
	seed.Apps[0].Certificates = []fakegraph.Certificate{{PEM: string(k.certPEM)}}
	return seed
}

func TestCertificateAuth(t *testing.T) {
	keys := testKeys()
	for name, pemData := range map[string][]byte{
		"pkcs8":     keys[0].combined(),
		"pkcs1":     append(append([]byte{}, keys[0].pkcs1PEM()...), keys[0].certPEM...),
		"two certs": append(append(append([]byte{}, keys[1].certPEM...), keys[0].certPEM...), keys[0].pkcs8PEM()...),
	} {
		t.Run(name, func(t *testing.T) {
			cred := mustCert(t, pemData)
			e := newEnv(t, withSeed(certSeed(keys[0])), withCredential(cred))
			if _, err := e.client.Organization(context.Background()); err != nil {
				t.Fatalf("Organization with certificate auth: %v", err)
			}
			if got := e.fake.CountRequests("POST", tokenPath); got != 1 {
				t.Errorf("token requests = %d, want 1", got)
			}
		})
	}
}

func TestCertificateFromFileAndThumbprintOnlyRegistration(t *testing.T) {
	keys := testKeys()
	path := filepath.Join(t.TempDir(), "taut.pem")
	if err := os.WriteFile(path, keys[0].combined(), 0o600); err != nil {
		t.Fatal(err)
	}
	cred, err := graph.LoadClientCertificate(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cred.Thumbprint()) != 40 || cred.Thumbprint() != strings.ToUpper(cred.Thumbprint()) {
		t.Errorf("Thumbprint = %q", cred.Thumbprint())
	}
	seed := fakegraph.DefaultSeed()
	seed.Apps[0].Certificates = []fakegraph.Certificate{{Thumbprint: cred.Thumbprint()}}
	e := newEnv(t, withSeed(seed), withCredential(cred))
	if _, err := e.client.Organization(context.Background()); err != nil {
		t.Fatalf("Organization: %v", err)
	}

	_, err = graph.LoadClientCertificate(filepath.Join(t.TempDir(), "missing.pem"))
	wantFail(t, err, "entra.certificate_unreadable")
}

// formCapture records token request bodies so tests can prove the secret
// material in them never reaches an error.
type formCapture struct {
	next  http.RoundTripper
	mu    sync.Mutex
	forms []url.Values
}

func (f *formCapture) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil && strings.HasSuffix(r.URL.Path, "/token") {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		if v, err := url.ParseQuery(string(body)); err == nil {
			f.mu.Lock()
			f.forms = append(f.forms, v)
			f.mu.Unlock()
		}
	}
	return f.next.RoundTrip(r)
}

func TestUnregisteredCertificate(t *testing.T) {
	keys := testKeys()
	cred := mustCert(t, keys[1].combined()) // server only knows keys[0]
	e := newEnv(t, withSeed(certSeed(keys[0])))

	capture := &formCapture{next: e.http.Client().Transport}
	cc, err := graph.NewClientCredentials(graph.CredentialsOptions{
		TenantID: fakegraph.DefaultTenantID, ClientID: fakegraph.DefaultClientID, Credential: cred,
		Authority: e.http.URL, GraphBase: e.http.URL, HTTPClient: &http.Client{Transport: capture}, Now: e.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = cc.Fetch(context.Background())
	fe := wantFail(t, err, "entra.bad_certificate")
	if !strings.Contains(fe.Why, cred.Thumbprint()) {
		t.Errorf("Why should name the thumbprint to upload: %q", fe.Why)
	}
	var te *graph.TokenError
	if !errors.As(err, &te) || te.AADSTS != "AADSTS700027" || te.Status != 401 || te.TraceID == "" {
		t.Errorf("TokenError = %+v", te)
	}
	if len(capture.forms) != 1 {
		t.Fatalf("captured %d token requests", len(capture.forms))
	}
	form := capture.forms[0]
	if form.Get("client_assertion_type") != "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" || form.Has("client_secret") {
		t.Errorf("unexpected form fields: %v", slices.Sorted(func(yield func(string) bool) {
			for k := range form {
				if !yield(k) {
					return
				}
			}
		}))
	}
	assertion := form.Get("client_assertion")
	assertNoSecrets(t, err, assertion, strings.Split(assertion, ".")[2])
}

func TestEntraErrorMapping(t *testing.T) {
	const wrong = "wrong-secret-Zq8~XXXXXXXX"
	const expired = "expired-secret-Kp3~YYYYYYYY"
	seed := fakegraph.DefaultSeed()
	seed.Apps[0].ExpiredSecrets = []string{expired}

	tests := []struct {
		name   string
		opts   []envOpt
		code   string
		aadsts string
		next   string
		secret string
	}{
		{"wrong secret", []envOpt{withCredential(mustSecret(t, wrong))}, "entra.invalid_secret", "AADSTS7000215", "Value", wrong},
		{"expired secret", []envOpt{withSeed(seed), withCredential(mustSecret(t, expired))}, "entra.expired_secret", "AADSTS7000222", "new secret", expired},
		{"unknown app", []envOpt{withClientID("00000000-1111-2222-3333-444444444444")}, "entra.app_not_found", "AADSTS700016", "client ID", fakegraph.DefaultSecret},
		{"unknown tenant", []envOpt{withTenant("nope.example")}, "entra.tenant_not_found", "AADSTS90002", "tenant ID", fakegraph.DefaultSecret},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.opts...)
			_, err := e.client.Organization(context.Background())
			fe := wantFail(t, err, tc.code)
			if !strings.Contains(fe.Next, tc.next) {
				t.Errorf("Next = %q, want it to mention %q", fe.Next, tc.next)
			}
			if strings.Contains(fe.Why, "Correlation ID") || strings.Contains(fe.Why, "\n") {
				t.Errorf("Why should be one clean line: %q", fe.Why)
			}
			var te *graph.TokenError
			if !errors.As(err, &te) || te.AADSTS != tc.aadsts {
				t.Errorf("TokenError = %+v, want %s", te, tc.aadsts)
			}
			if fail.ExitCode(err) != fail.ExitFailure {
				t.Errorf("exit = %d", fail.ExitCode(err))
			}
			assertNoSecrets(t, err, tc.secret)
			if got := e.fake.CountRequests("", "/v1.0/**"); got != 0 {
				t.Errorf("Graph was called %d times without a token", got)
			}
		})
	}
}

func TestEntraUnreachable(t *testing.T) {
	e := newEnv(t)
	e.fake.Faults().MustAdd(fakegraph.Fault{Path: tokenPath, Action: fakegraph.ActionDrop})
	_, err := e.tokens.Token(context.Background())
	wantFail(t, err, "entra.unreachable")
	assertNoSecrets(t, err, fakegraph.DefaultSecret)

	e.fake.Faults().Clear()
	e.fake.Faults().MustAdd(fakegraph.Fault{Path: tokenPath, Action: fakegraph.ActionStatus, Status: 503})
	_, err = e.tokens.Token(context.Background())
	wantFail(t, err, "entra.unavailable")
}

func TestTokenCacheRefreshesBeforeExpiry(t *testing.T) {
	e := newEnv(t) // one-hour tokens, refreshed five minutes early
	ctx := context.Background()
	first, err := e.tokens.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(54 * time.Minute)
	if again, _ := e.tokens.Token(ctx); again != first {
		t.Error("token changed before the refresh window")
	}
	if got := e.fake.CountRequests("POST", tokenPath); got != 1 {
		t.Fatalf("token requests = %d, want 1", got)
	}
	e.clock.Advance(2 * time.Minute) // 56 minutes: inside the window
	fresh, err := e.tokens.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fresh == first {
		t.Error("token was not refreshed inside the refresh window")
	}
	if got := e.fake.CountRequests("POST", tokenPath); got != 2 {
		t.Fatalf("token requests = %d, want 2", got)
	}
}

func TestShortLivedTokensAreStillCached(t *testing.T) {
	e := newEnv(t, withServer(fakegraph.WithTokenLifetime(2*time.Minute)))
	ctx := context.Background()
	for range 5 {
		if _, err := e.tokens.Token(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.fake.CountRequests("POST", tokenPath); got != 1 {
		t.Fatalf("token requests = %d, want 1 (lifetime shorter than the refresh margin)", got)
	}
}

// countingFetcher blocks until released so concurrent callers pile up.
type countingFetcher struct {
	calls   atomic.Int32
	release chan struct{}
	err     error
}

func (f *countingFetcher) Fetch(ctx context.Context) (graph.AccessToken, error) {
	n := f.calls.Add(1)
	select {
	case <-f.release:
	case <-ctx.Done():
		return graph.AccessToken{}, ctx.Err()
	}
	if f.err != nil {
		return graph.AccessToken{}, f.err
	}
	return graph.AccessToken{Value: fmt.Sprintf("tok-%d", n), ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func TestTokenCacheSingleFlight(t *testing.T) {
	f := &countingFetcher{release: make(chan struct{})}
	ts := graph.NewCachingTokenSource(f, graph.CacheOptions{})
	const n = 32
	var wg sync.WaitGroup
	results := make([]string, n)
	for i := range n {
		wg.Go(func() {
			tok, err := ts.Token(context.Background())
			if err != nil {
				t.Errorf("Token: %v", err)
			}
			results[i] = tok
		})
	}
	close(f.release)
	wg.Wait()
	if got := f.calls.Load(); got != 1 {
		t.Errorf("fetches = %d, want 1", got)
	}
	for _, r := range results {
		if r != "tok-1" {
			t.Fatalf("results = %v", results)
		}
	}
}

func TestTokenCacheWaiterSurvivesLeaderCancellation(t *testing.T) {
	f := &countingFetcher{release: make(chan struct{})}
	ts := graph.NewCachingTokenSource(f, graph.CacheOptions{})

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, err := ts.Token(leaderCtx)
		leaderDone <- err
	}()
	for f.calls.Load() == 0 { // leader is now inside Fetch
		time.Sleep(time.Millisecond)
	}
	waiterDone := make(chan string, 1)
	go func() {
		tok, err := ts.Token(context.Background())
		if err != nil {
			t.Errorf("waiter: %v", err)
		}
		waiterDone <- tok
	}()
	cancelLeader()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader err = %v", err)
	}
	close(f.release)
	if tok := <-waiterDone; !strings.HasPrefix(tok, "tok-") {
		t.Fatalf("waiter token = %q", tok)
	}
}

func TestTokenCacheDoesNotCacheFailures(t *testing.T) {
	f := &countingFetcher{release: make(chan struct{}), err: errors.New("boom")}
	close(f.release)
	ts := graph.NewCachingTokenSource(f, graph.CacheOptions{})
	for range 3 {
		if _, err := ts.Token(context.Background()); err == nil {
			t.Fatal("want error")
		}
	}
	if got := f.calls.Load(); got != 3 {
		t.Errorf("fetches = %d, want 3", got)
	}
}

func TestCredentialParsingErrors(t *testing.T) {
	keys := testKeys()
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sec1, _ := x509.MarshalECPrivateKey(ec)
	ecKey := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1}))
	ecPKCS8DER, _ := x509.MarshalPKCS8PrivateKey(ec)
	ecPKCS8 := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecPKCS8DER}))
	legacyEncrypted := "-----BEGIN RSA PRIVATE KEY-----\nProc-Type: 4,ENCRYPTED\nDEK-Info: AES-256-CBC,0123456789ABCDEF0123456789ABCDEF\n\nAAAA\n-----END RSA PRIVATE KEY-----\n"
	pkcs8Encrypted := "-----BEGIN ENCRYPTED PRIVATE KEY-----\nAAAA\n-----END ENCRYPTED PRIVATE KEY-----\n"

	tests := []struct {
		name string
		pem  string
		code string
	}{
		{"empty", "", "entra.bad_certificate"},
		{"not pem", "MIIC binary pfx bytes", "entra.bad_certificate"},
		{"cert only", string(keys[0].certPEM), "entra.key_missing"},
		{"key only", string(keys[0].pkcs8PEM()), "entra.certificate_missing"},
		{"legacy encrypted key", string(keys[0].certPEM) + legacyEncrypted, "entra.key_encrypted"},
		{"pkcs8 encrypted key", string(keys[0].certPEM) + pkcs8Encrypted, "entra.key_encrypted"},
		{"ec key", string(keys[0].certPEM) + ecKey, "entra.key_unsupported"},
		{"ec key in pkcs8", string(keys[0].certPEM) + ecPKCS8, "entra.key_unsupported"},
		{"mismatched pair", string(keys[1].certPEM) + string(keys[0].pkcs8PEM()), "entra.key_mismatch"},
		{"garbage key", string(keys[0].certPEM) + "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n", "entra.bad_certificate"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := graph.ParseClientCertificate([]byte(tc.pem))
			wantFail(t, err, tc.code)
		})
	}

	_, err = graph.NewClientSecret("  ")
	wantFail(t, err, "entra.secret_missing")
}

func TestExpiredCertificateIsCaughtLocally(t *testing.T) {
	keys := testKeys()
	e := newEnv(t, withSeed(certSeed(keys[0])), withCredential(mustCert(t, keys[0].combined())))
	e.clock.Advance(3 * 365 * 24 * time.Hour) // past NotAfter (2028-01-01)
	_, err := e.tokens.Token(context.Background())
	wantFail(t, err, "entra.certificate_expired")
	if got := e.fake.CountRequests("POST", tokenPath); got != 0 {
		t.Errorf("token endpoint called %d times with an expired certificate", got)
	}
}

func TestCredentialsNeverFormat(t *testing.T) {
	const secret = "s3cr3t-value-Qw9~ZZZZZZZZZZ"
	keys := testKeys()
	e := newEnv(t, withCredential(mustSecret(t, secret)), withSeed(func() fakegraph.Seed {
		s := fakegraph.DefaultSeed()
		s.Apps[0].Secrets = []string{secret}
		return s
	}()))
	token, err := e.tokens.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cc, err := graph.NewClientCredentials(graph.CredentialsOptions{
		TenantID: "t", ClientID: "c", Credential: mustSecret(t, secret),
	})
	if err != nil {
		t.Fatal(err)
	}
	cert := mustCert(t, keys[0].combined())
	keyB64 := strings.Join(strings.Split(string(keys[0].pkcs8PEM()), "\n")[1:3], "")
	holder := struct {
		Cred   graph.Credential
		Source graph.TokenSource
		Token  graph.AccessToken
		client *graph.Client
	}{mustSecret(t, secret), e.tokens, graph.AccessToken{Value: token}, e.client}

	values := []any{
		mustSecret(t, secret), *mustSecret(t, secret), cert, *cert, cc, *cc, e.tokens, e.client, *e.client,
		graph.AccessToken{Value: token}, &graph.AccessToken{Value: token}, holder, &holder,
		[]graph.Credential{mustSecret(t, secret), cert},
	}
	for _, v := range values {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
			out := fmt.Sprintf(verb, v)
			for _, s := range []string{secret, token, keyB64, fmt.Sprintf("%x", secret), fmt.Sprintf("%x", token)} {
				if strings.Contains(out, s) {
					t.Errorf("fmt.Sprintf(%q, %T) leaks secret material: %.120s", verb, v, out)
				}
			}
		}
	}
}

func TestParseClaimsRejectsGarbageWithoutEchoingIt(t *testing.T) {
	for _, tok := range []string{"", "not-a-jwt", "a.b", "aaaa.!!!!.cccc", "aaaa.bm90IGpzb24.cccc"} {
		_, err := graph.ParseClaims(tok)
		wantFail(t, err, "graph.token_unreadable")
		if tok != "" && strings.Contains(err.Error(), tok) {
			t.Errorf("error echoes the token %q", tok)
		}
	}
	// aud may be a list, appid may arrive as azp (v2 tokens).
	payload := `{"aud":["https://graph.microsoft.com","x"],"azp":"app-1","tid":"t-1","roles":["A","B"],"exp":1800000000}`
	c, err := graph.ParseClaims("e30." + b64url(payload) + ".sig")
	if err != nil {
		t.Fatal(err)
	}
	if c.AppID != "app-1" || c.Audience != "https://graph.microsoft.com" || len(c.Roles) != 2 || c.ExpiresAt.Unix() != 1800000000 {
		t.Errorf("claims = %+v", c)
	}
}

func TestClouds(t *testing.T) {
	want := map[string][2]string{
		"global":    {"https://login.microsoftonline.com", "https://graph.microsoft.com"},
		"usgov":     {"https://login.microsoftonline.com", "https://graph.microsoft.com"},
		"usgovhigh": {"https://login.microsoftonline.us", "https://graph.microsoft.us"},
		"dod":       {"https://login.microsoftonline.us", "https://dod-graph.microsoft.us"},
		"china":     {"https://login.partner.microsoftonline.cn", "https://microsoftgraph.chinacloudapi.cn"},
	}
	if len(graph.Clouds()) != len(want) {
		t.Fatalf("Clouds() = %v", graph.Clouds())
	}
	for name, urls := range want {
		c, err := graph.LookupCloud(strings.ToUpper(name))
		if err != nil || c.Authority != urls[0] || c.GraphBase != urls[1] {
			t.Errorf("LookupCloud(%s) = %+v, %v", name, c, err)
		}
		cc, err := graph.NewClientCredentials(graph.CredentialsOptions{
			TenantID: "contoso.example", ClientID: "c", Credential: mustSecret(t, "x"), Cloud: name,
		})
		if err != nil {
			t.Fatal(err)
		}
		if cc.TokenEndpoint() != urls[0]+"/contoso.example/oauth2/v2.0/token" || cc.GraphBase() != urls[1] {
			t.Errorf("%s: endpoint %s, graph %s", name, cc.TokenEndpoint(), cc.GraphBase())
		}
	}
	if c, _ := graph.LookupCloud(""); c.Name != graph.CloudGlobal {
		t.Errorf("empty cloud = %+v", c)
	}
	_, err := graph.LookupCloud("mars")
	if fe := wantFail(t, err, "entra.unknown_cloud"); fe.Exit != fail.ExitUsage {
		t.Errorf("exit = %d", fe.Exit)
	}
	_, err = graph.NewClientCredentials(graph.CredentialsOptions{ClientID: "c", Credential: mustSecret(t, "x")})
	wantFail(t, err, "entra.config_incomplete")
}

func b64url(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
