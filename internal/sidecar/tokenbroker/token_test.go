package tokenbroker

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/secret"
	"github.com/bgfdev/taut/internal/sidecar/wire"
)

// Canaries: if any of these shows up in a log line or an error, a secret
// leaked. None has a shape secret.Redact would catch on its own, except the
// Slack token, which must have one to be served at all.
const (
	canarySecret  = "CANARY-entra-client-secret-7f3a"
	canaryAccess  = "CANARY-access-token"
	canarySlack   = "xoxb-123456789012-CANARYslacktoken"
	canaryNotSlak = "CANARY-not-a-slack-token-value-91bd"
)

// --- fake Entra ---------------------------------------------------------------------

type entraStub struct {
	*httptest.Server
	mu      sync.Mutex
	calls   int
	secrets []string // client_secret values seen
	x5ts    []string // certificate thumbprints seen in assertions
	fail    bool
}

func newEntraStub(t *testing.T) *entraStub {
	t.Helper()
	e := &entraStub{}
	e.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		e.mu.Lock()
		defer e.mu.Unlock()
		e.calls++
		if !strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token") || r.Form.Get("grant_type") != "client_credentials" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if e.fail {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"invalid_client","error_description":"AADSTS7000215: Invalid client secret provided.","error_codes":[7000215]}`)
			return
		}
		if s := r.Form.Get("client_secret"); s != "" {
			e.secrets = append(e.secrets, s)
		}
		if a := r.Form.Get("client_assertion"); a != "" {
			head, _, _ := strings.Cut(a, ".")
			raw, _ := base64.RawURLEncoding.DecodeString(head)
			var h struct {
				X5t string `json:"x5t"`
			}
			_ = json.Unmarshal(raw, &h)
			e.x5ts = append(e.x5ts, h.X5t)
		}
		_, _ = fmt.Fprintf(w, `{"token_type":"Bearer","expires_in":3600,"access_token":"%s-%d"}`, canaryAccess, e.calls)
	}))
	t.Cleanup(e.Close)
	return e
}

func (e *entraStub) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

// --- certificates -------------------------------------------------------------------

// newPEM returns a PEM bundle (certificate + RSA key) and the certificate's
// base64url SHA-1 thumbprint.
func newPEM(t *testing.T) (pemData []byte, x5t string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "taut-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	var buf bytes.Buffer
	_ = pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	_ = pem.Encode(&buf, &pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	sum := sha1.Sum(der)
	return buf.Bytes(), base64.RawURLEncoding.EncodeToString(sum[:])
}

// --- harness ------------------------------------------------------------------------

// shortDir returns a private temporary directory whose path is short enough
// to hold a Unix socket (about 100 bytes on macOS).
func shortDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if len(dir) > 70 {
		var err error
		if dir, err = os.MkdirTemp("", "tt"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
	}
	return dir
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

type harness struct {
	t       *testing.T
	entra   *entraStub
	secrets string // the allowed secrets directory
	socket  string
	client  *Client
	logs    *logBuf
	errs    []string // every error text seen, for the canary check
	now     time.Time
	nowMu   sync.Mutex
	stop    func()
}

func (h *harness) Now() time.Time {
	h.nowMu.Lock()
	defer h.nowMu.Unlock()
	return h.now
}

func (h *harness) Advance(d time.Duration) {
	h.nowMu.Lock()
	h.now = h.now.Add(d)
	h.nowMu.Unlock()
}

func start(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, entra: newEntraStub(t), logs: &logBuf{}, now: time.Now()}
	base := shortDir(t)
	h.secrets = filepath.Join(base, "secrets")
	if err := os.Mkdir(h.secrets, 0o700); err != nil {
		t.Fatal(err)
	}
	h.socket = filepath.Join(base, "run", "token.sock")
	srv, err := Listen(Config{
		Socket:         h.socket,
		SecretDirs:     []string{h.secrets},
		AllowEndpoints: []string{h.entra.URL},
		AllowTenants:   []string{"Contoso"},
		Log:            slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:            h.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	h.stop = sync.OnceFunc(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	t.Cleanup(h.stop)
	t.Cleanup(h.assertNoLeaks)
	h.client = NewClient(h.socket)
	return h
}

func (h *harness) write(name, content string) string {
	h.t.Helper()
	p := filepath.Join(h.secrets, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}
	return p
}

func (h *harness) entraReq(cred wire.Credential) EntraRequest {
	return EntraRequest{TenantID: "contoso", ClientID: "app-1", Authority: h.entra.URL, GraphBase: h.entra.URL, Credential: cred}
}

// wantCode asserts err is a *fail.Error with the given code and records its
// text for the canary check.
func (h *harness) wantCode(err error, code string) *fail.Error {
	h.t.Helper()
	if err == nil {
		h.t.Fatalf("no error, want %s", code)
	}
	h.errs = append(h.errs, fmt.Sprintf("%v | %+v | %#v", err, err, err))
	var fe *fail.Error
	if !errors.As(err, &fe) {
		h.t.Fatalf("error %v is not a *fail.Error", err)
	}
	h.errs = append(h.errs, fe.What, fe.Why, fe.Next)
	if fe.Code != code {
		h.t.Fatalf("code = %s (%v), want %s", fe.Code, fe, code)
	}
	if fe.What == "" || fe.Next == "" {
		h.t.Errorf("%s: missing What or Next: %+v", code, fe)
	}
	return fe
}

func (h *harness) assertNoLeaks() {
	h.t.Helper()
	all := h.logs.String() + "\n" + strings.Join(h.errs, "\n")
	for _, c := range []string{canarySecret, canaryAccess, canarySlack, canaryNotSlak, "PRIVATE KEY", "client_assertion"} {
		if strings.Contains(all, c) {
			h.t.Errorf("canary %q leaked into logs or errors:\n%s", c, all)
		}
	}
}

// --- tests --------------------------------------------------------------------------

func TestEntraClientSecretAndCaching(t *testing.T) {
	h := start(t)
	ctx := context.Background()
	req := h.entraReq(wire.Credential{ClientSecret: secret.Ref{File: h.write("entra-secret", canarySecret+"\n")}})

	tok, exp, err := h.client.Entra(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if tok != canaryAccess+"-1" {
		t.Errorf("token = %q", tok)
	}
	if want := h.Now().Add(time.Hour); !exp.Equal(want) {
		t.Errorf("expiresAt = %v, want %v", exp, want)
	}
	if got := h.entra.secrets; len(got) != 1 || got[0] != canarySecret {
		t.Errorf("stub saw secrets %q", got)
	}

	// Many more requests, some concurrent: still one sign-in.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if tok, _, err := h.client.Entra(ctx, req); err != nil || tok != canaryAccess+"-1" {
				t.Errorf("Entra = %q, %v", tok, err)
			}
		}()
	}
	wg.Wait()
	if n := h.entra.Calls(); n != 1 {
		t.Errorf("sign-ins = %d, want 1", n)
	}

	// A different application is a different source.
	other := req
	other.ClientID = "app-2"
	if tok, _, err := h.client.Entra(ctx, other); err != nil || tok != canaryAccess+"-2" {
		t.Errorf("other app: %q, %v", tok, err)
	}

	// Near expiry the token is replaced.
	h.Advance(58 * time.Minute)
	if tok, _, err := h.client.Entra(ctx, req); err != nil || tok != canaryAccess+"-3" {
		t.Errorf("after expiry margin: %q, %v", tok, err)
	}
}

func TestEntraSecretFromEnv(t *testing.T) {
	t.Setenv("TAUT_ENTRA_TEST_SECRET", canarySecret)
	h := start(t)
	tok, _, err := h.client.Entra(context.Background(), h.entraReq(wire.Credential{ClientSecret: secret.Ref{Env: "TAUT_ENTRA_TEST_SECRET"}}))
	if err != nil || tok == "" {
		t.Fatalf("Entra = %q, %v", tok, err)
	}
	if got := h.entra.secrets; len(got) != 1 || got[0] != canarySecret {
		t.Errorf("stub saw secrets %q", got)
	}
}

func TestEntraCertificateAndRotation(t *testing.T) {
	h := start(t)
	ctx := context.Background()
	pem1, x5t1 := newPEM(t)
	pem2, x5t2 := newPEM(t)
	path := h.write("taut.pem", string(pem1))
	req := h.entraReq(wire.Credential{Certificate: secret.Ref{File: path}})

	for i := 0; i < 3; i++ {
		if tok, _, err := h.client.Entra(ctx, req); err != nil || tok != canaryAccess+"-1" {
			t.Fatalf("Entra = %q, %v", tok, err)
		}
	}
	if got := h.entra.x5ts; len(got) != 1 || got[0] != x5t1 {
		t.Fatalf("assertions signed by %q, want one by %q", got, x5t1)
	}

	// Rotate the file in place. A different mtime guarantees the change is
	// seen even if both PEMs happen to have the same size.
	if err := os.WriteFile(path, pem2, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, time.Now(), time.Now().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if tok, _, err := h.client.Entra(ctx, req); err != nil || tok != canaryAccess+"-2" {
		t.Fatalf("after rotation: %q, %v", tok, err)
	}
	if got := h.entra.x5ts; len(got) != 2 || got[1] != x5t2 {
		t.Errorf("after rotation assertions signed by %q, want the second by %q", got, x5t2)
	}
	if !strings.Contains(h.logs.String(), "reloaded") {
		t.Errorf("no log line about the rotation:\n%s", h.logs.String())
	}
	// And it is cached again.
	if tok, _, _ := h.client.Entra(ctx, req); tok != canaryAccess+"-2" {
		t.Errorf("after rotation, second call: %q", tok)
	}

	// A broken file is reported with the graph package's own code.
	if err := os.WriteFile(path, []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := h.client.Entra(ctx, req)
	h.wantCode(err, "entra.bad_certificate")
}

func TestEntraErrorsPassThrough(t *testing.T) {
	h := start(t)
	h.entra.fail = true
	req := h.entraReq(wire.Credential{ClientSecret: secret.Ref{File: h.write("s", canarySecret)}})
	_, _, err := h.client.Entra(context.Background(), req)
	h.wantCode(err, "entra.invalid_secret")

	req.Cloud = "atlantis"
	_, _, err = h.client.Entra(context.Background(), req)
	h.wantCode(err, "entra.unknown_cloud")
}

func TestInvalidate(t *testing.T) {
	h := start(t)
	ctx := context.Background()
	req := h.entraReq(wire.Credential{ClientSecret: secret.Ref{File: h.write("s", canarySecret)}})
	tok := func() string {
		t.Helper()
		v, _, err := h.client.Entra(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if tok() != canaryAccess+"-1" {
		t.Fatal("first token")
	}
	if err := h.client.Invalidate(ctx, wire.EntraAuth(req)); err != nil {
		t.Fatal(err)
	}
	if got := tok(); got != canaryAccess+"-2" {
		t.Errorf("after Invalidate: %q, want a new token", got)
	}
	// A second invalidation right away is ignored…
	if err := h.client.Invalidate(ctx, wire.EntraAuth(req)); err != nil {
		t.Fatal(err)
	}
	if got := tok(); got != canaryAccess+"-2" {
		t.Errorf("rate-limited Invalidate still discarded the token: %q", got)
	}
	// …and honored again later.
	h.Advance(minInvalidateInterval)
	_ = h.client.Invalidate(ctx, wire.EntraAuth(req))
	if got := tok(); got != canaryAccess+"-3" {
		t.Errorf("later Invalidate: %q", got)
	}
	// Slack and unknown applications are no-ops, not errors.
	if err := h.client.Invalidate(ctx, wire.SlackAuth(secret.Ref{Env: "TAUT_SLACK_X"})); err != nil {
		t.Errorf("slack Invalidate: %v", err)
	}
}

func TestSlack(t *testing.T) {
	t.Setenv("TAUT_SLACK_TEST", canarySlack)
	h := start(t)
	ctx := context.Background()

	got, err := h.client.Slack(ctx, secret.Ref{Env: "TAUT_SLACK_TEST"})
	if err != nil || got != canarySlack {
		t.Fatalf("Slack(env) = %q, %v", got, err)
	}
	path := h.write("slack-token", "  "+canarySlack+"\n")
	got, err = h.client.Slack(ctx, secret.Ref{File: path})
	if err != nil || got != canarySlack {
		t.Fatalf("Slack(file) = %q, %v", got, err)
	}
	// Rotation: the file is read on every request.
	rotated := "xoxp-999999999999-rotatedCANARY"
	h.write("slack-token", rotated)
	if got, _ := h.client.Slack(ctx, secret.Ref{File: path}); got != rotated {
		t.Errorf("after rotation Slack = %q", got)
	}
	h.errs = append(h.errs, "") // rotated token is checked below
	if strings.Contains(h.logs.String(), rotated) {
		t.Error("rotated token in logs")
	}
}

func TestSlackTokenShapes(t *testing.T) {
	for _, ok := range []string{"xoxb-123456789012-abcdef", "xoxp-1-2-3-4567890", "xoxe.xoxp-1-Mi0yLTEyMzQ1", "xoxe-1-abcdefghijkl", "xapp-1-A0123-456-abcdef"} {
		if !slackTokenRE.MatchString(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "xoxb-", "hunter2hunter2hunter2", "root:x:0:0:root:/root:/bin/sh", "xoxb-123456789012-abc\nsecond line", "prefix xoxb-123456789012-abcdef", "-----BEGIN PRIVATE KEY-----"} {
		if slackTokenRE.MatchString(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestRefusals(t *testing.T) {
	t.Setenv("TAUT_SLACK_TEST_NOT", canaryNotSlak)
	t.Setenv("HOME_SECRET", canarySlack) // real token shape, name not allowed
	t.Setenv("TAUT_SLACK_TEST_EMPTY", "  ")
	h := start(t)
	ctx := context.Background()

	outside := filepath.Join(filepath.Dir(h.secrets), "outside")
	if err := os.WriteFile(outside, []byte(canarySlack), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(h.secrets, "link-out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(h.secrets), filepath.Join(h.secrets, "dir-out")); err != nil {
		t.Fatal(err)
	}
	h.write("real", canarySlack)
	if err := os.Symlink("real", filepath.Join(h.secrets, "link-in")); err != nil {
		t.Fatal(err)
	}
	h.write("not-slack", canaryNotSlak)
	h.write("empty", "\n")
	if err := os.Mkdir(filepath.Join(h.secrets, "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}

	slack := func(ref secret.Ref) error { _, err := h.client.Slack(ctx, ref); return err }

	cases := []struct {
		name string
		ref  secret.Ref
		code string
	}{
		{"file outside the secrets dir", secret.Ref{File: outside}, CodeRefNotAllowed},
		{"system file", secret.Ref{File: "/etc/passwd"}, CodeRefNotAllowed},
		{"dot-dot escape", secret.Ref{File: h.secrets + "/../outside"}, CodeRefNotAllowed},
		{"dot-dot through a subdirectory", secret.Ref{File: h.secrets + "/subdir/../../outside"}, CodeRefNotAllowed},
		{"symlink to a file outside", secret.Ref{File: filepath.Join(h.secrets, "link-out")}, CodeRefNotAllowed},
		{"symlinked directory leading outside", secret.Ref{File: filepath.Join(h.secrets, "dir-out", "outside")}, CodeRefNotAllowed},
		{"relative path", secret.Ref{File: "secrets/real"}, CodeRefNotAllowed},
		{"the secrets dir itself", secret.Ref{File: h.secrets}, CodeRefNotAllowed},
		{"a directory", secret.Ref{File: filepath.Join(h.secrets, "subdir")}, CodeRefNotAllowed},
		{"sibling with the dir's name as prefix", secret.Ref{File: h.secrets + "-evil/x"}, CodeRefNotAllowed},
		{"missing file outside (no existence oracle)", secret.Ref{File: "/etc/definitely-not-here"}, CodeRefNotAllowed},
		{"missing file inside", secret.Ref{File: filepath.Join(h.secrets, "nope")}, CodeSecretMissing},
		{"empty file", secret.Ref{File: filepath.Join(h.secrets, "empty")}, CodeSecretMissing},
		{"env not allowed", secret.Ref{Env: "HOME_SECRET"}, CodeRefNotAllowed},
		{"env PATH", secret.Ref{Env: "PATH"}, CodeRefNotAllowed},
		{"env unset", secret.Ref{Env: "TAUT_SLACK_TEST_UNSET"}, CodeSecretMissing},
		{"env empty", secret.Ref{Env: "TAUT_SLACK_TEST_EMPTY"}, CodeSecretMissing},
		{"env value not token-shaped", secret.Ref{Env: "TAUT_SLACK_TEST_NOT"}, CodeRefNotAllowed},
		{"file value not token-shaped", secret.Ref{File: filepath.Join(h.secrets, "not-slack")}, CodeRefNotAllowed},
		{"invalid env name", secret.Ref{Env: "TAUT_SLACK TEST"}, CodeBadRequest},
		{"literal secret as reference", secret.Ref{Env: canarySlack}, CodeBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h.t = t
			h.wantCode(slack(c.ref), c.code)
		})
	}
	h.t = t

	// A symlink that stays inside is fine (Kubernetes mounts secrets so).
	if got, err := h.client.Slack(ctx, secret.Ref{File: filepath.Join(h.secrets, "link-in")}); err != nil || got != canarySlack {
		t.Errorf("symlink inside: %q, %v", got, err)
	}

	// The same rules guard Entra credentials.
	_, _, err := h.client.Entra(ctx, h.entraReq(wire.Credential{ClientSecret: secret.Ref{File: outside}}))
	h.wantCode(err, CodeRefNotAllowed)
	_, _, err = h.client.Entra(ctx, h.entraReq(wire.Credential{Certificate: secret.Ref{Env: "HOME_SECRET"}}))
	h.wantCode(err, CodeRefNotAllowed)

	// Both or neither credential.
	both := wire.Credential{ClientSecret: secret.Ref{Env: "TAUT_ENTRA_A"}, Certificate: secret.Ref{File: filepath.Join(h.secrets, "real")}}
	_, _, err = h.client.Entra(ctx, h.entraReq(both))
	h.wantCode(err, CodeBadRequest)
	_, _, err = h.client.Entra(ctx, h.entraReq(wire.Credential{}))
	h.wantCode(err, CodeBadRequest)
	_, _, err = h.client.Entra(ctx, EntraRequest{Credential: wire.Credential{ClientSecret: secret.Ref{Env: "TAUT_ENTRA_A"}}})
	h.wantCode(err, CodeBadRequest)

	if n := h.entra.Calls(); n != 0 {
		t.Errorf("refused requests reached Entra %d times", n)
	}
}

func TestEndpointOverrideMustBeAllowed(t *testing.T) {
	h := start(t)
	evil := newEntraStub(t)
	req := h.entraReq(wire.Credential{ClientSecret: secret.Ref{File: h.write("s", canarySecret)}})

	req.Authority = evil.URL
	_, _, err := h.client.Entra(context.Background(), req)
	h.wantCode(err, CodeEndpointNotAllowed)

	req.Authority, req.GraphBase = h.entra.URL, "https://management.azure.com"
	_, _, err = h.client.Entra(context.Background(), req)
	h.wantCode(err, CodeEndpointNotAllowed)

	if evil.Calls() != 0 || h.entra.Calls() != 0 {
		t.Errorf("credential sent somewhere: evil=%d, allowed=%d", evil.Calls(), h.entra.Calls())
	}

	// The clouds' own endpoints need no allow-listing.
	s, err := newServer(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if !s.endpointAllowed("https://login.microsoftonline.com/", "https://login.microsoftonline.com") ||
		!s.endpointAllowed("", "https://login.microsoftonline.com") ||
		s.endpointAllowed("https://login.microsoftonline.com.evil.example", "https://login.microsoftonline.com") {
		t.Error("endpointAllowed is wrong for cloud defaults")
	}
}

func TestBadRequests(t *testing.T) {
	h := start(t)
	hc := h.client.hc
	post := func(path, body string) (int, wire.ErrorBody) {
		t.Helper()
		resp, err := hc.Post("http://token.broker"+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var env wire.ErrorBody
		_ = json.NewDecoder(resp.Body).Decode(&env)
		return resp.StatusCode, env
	}
	for _, body := range []string{``, `{`, `[]`, `{"kind":"none"}`, `{"kind":"password"}`, `{"kind":"slack"}`,
		`{"kind":"slack","token":{"env":"A","file":"/b"}}`, strings.Repeat(" ", maxRequestBody+1) + `{}`} {
		status, env := post("/v1/token", body)
		if status != http.StatusBadRequest || env.Error.Code != CodeBadRequest || env.Error.Next == "" {
			t.Errorf("body %.20q: status %d, error %+v", body, status, env.Error)
		}
	}
	if status, env := post("/v1/nope", `{}`); status != http.StatusNotFound || env.Error.Code != CodeBadRequest {
		t.Errorf("unknown path: %d %+v", status, env.Error)
	}
	resp, err := hc.Get("http://token.broker/v1/token")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /v1/token: %d", resp.StatusCode)
	}
}

func TestHealthzAndSocketPermissions(t *testing.T) {
	h := start(t)
	if err := h.client.Healthy(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(h.socket)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %v, want a socket with 0600", info.Mode())
	}
	dir, _ := os.Stat(filepath.Dir(h.socket))
	if dir.Mode().Perm() != 0o700 {
		t.Errorf("socket directory mode = %v, want 0700", dir.Mode().Perm())
	}

	// Shutdown removes the socket.
	h.stop()
	if _, err := os.Lstat(h.socket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket still present after shutdown: %v", err)
	}
}

func TestSocketDirectoryMustBePrivate(t *testing.T) {
	dir := shortDir(t)
	open := filepath.Join(dir, "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Listen(Config{Socket: filepath.Join(open, "t.sock")})
	var fe *fail.Error
	if !errors.As(err, &fe) || fe.Code != "token.socket_dir_insecure" {
		t.Fatalf("err = %v, want token.socket_dir_insecure", err)
	}
	if info, _ := os.Stat(open); info.Mode().Perm() != 0o755 {
		t.Errorf("the broker changed the directory's mode to %v", info.Mode().Perm())
	}
}

func TestStaleSocket(t *testing.T) {
	dir := filepath.Join(shortDir(t), "run")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "t.sock")
	codeOf := func(err error) string {
		var fe *fail.Error
		if errors.As(err, &fe) {
			return fe.Code
		}
		return fmt.Sprint(err)
	}

	// A socket file nobody listens on: left behind by a crash.
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = ln.Close()
	if _, err := os.Lstat(socket); err != nil {
		t.Fatalf("setup: stale socket missing: %v", err)
	}
	srv, err := Listen(Config{Socket: socket})
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}

	// A live one is not stolen.
	if _, err := Listen(Config{Socket: socket}); codeOf(err) != "token.socket_in_use" {
		t.Errorf("second Listen: %v, want token.socket_in_use", err)
	}
	if _, err := os.Lstat(socket); err != nil {
		t.Errorf("the live socket was removed: %v", err)
	}
	_ = srv.ln.Close()

	// A regular file at the path is not removed.
	if err := os.WriteFile(socket, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(Config{Socket: socket}); codeOf(err) != "token.socket_path_taken" {
		t.Errorf("Listen over a regular file: %v, want token.socket_path_taken", err)
	}
	if data, _ := os.ReadFile(socket); string(data) != "precious" {
		t.Error("the regular file was touched")
	}
}

func TestConfigValidation(t *testing.T) {
	for name, cfg := range map[string]Config{
		"relative secrets dir": {SecretDirs: []string{"secrets"}},
		"bad env pattern":      {EnvAllow: []string{"TAUT_["}},
		"bad endpoint":         {AllowEndpoints: []string{"localhost:8080"}},
		"bad cloud":            {AllowClouds: []string{"atlantis"}},
	} {
		if _, err := newServer(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	s, err := newServer(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.cfg.SecretDirs) != 1 || s.cfg.SecretDirs[0] != "/run/secrets/taut" ||
		fmt.Sprint(s.cfg.EnvAllow) != "[TAUT_ENTRA_* TAUT_SLACK_*]" || fmt.Sprint(s.cfg.AllowClouds) != "[global]" {
		t.Errorf("defaults = %v, %v", s.cfg.SecretDirs, s.cfg.EnvAllow)
	}
	if _, ok := within("/x", "/"); ok {
		t.Error(`"/" must not work as a secrets directory`)
	}
}

func TestClientUnreachable(t *testing.T) {
	dir := shortDir(t)
	c := NewClient(filepath.Join(dir, "absent.sock"))
	check := func(err error) {
		t.Helper()
		var fe *fail.Error
		if !errors.As(err, &fe) || fe.Code != CodeUnreachable {
			t.Fatalf("err = %v, want %s", err, CodeUnreachable)
		}
		if !strings.Contains(fe.Next, "absent.sock") || !strings.Contains(fe.Next, "token broker is running") {
			t.Errorf("Next is not concrete: %q", fe.Next)
		}
	}
	_, err := c.Slack(context.Background(), secret.Ref{Env: "TAUT_SLACK_X"})
	check(err)
	_, _, err = c.Entra(context.Background(), EntraRequest{})
	check(err)
	check(c.Healthy(context.Background()))
	check(c.Invalidate(context.Background(), wire.NoAuth()))

	// A stale socket file with no listener is unreachable too.
	ln, err := net.Listen("unix", filepath.Join(dir, "dead.sock"))
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = ln.Close()
	_, err = NewClient(filepath.Join(dir, "dead.sock")).Slack(context.Background(), secret.Ref{Env: "TAUT_SLACK_X"})
	var fe *fail.Error
	if !errors.As(err, &fe) || fe.Code != CodeUnreachable {
		t.Errorf("dead socket: %v", err)
	}
}

func TestClientMapsOddAnswers(t *testing.T) {
	dir := shortDir(t)
	socket := filepath.Join(dir, "odd.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	mode := "garbage"
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "garbage":
			_, _ = io.WriteString(w, canaryAccess+" not json")
		case "empty":
			_, _ = io.WriteString(w, `{}`)
		case "500":
			http.Error(w, canaryAccess, http.StatusInternalServerError)
		case "hang":
			<-r.Context().Done()
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	c := NewClient(socket)
	for _, m := range []string{"garbage", "empty", "500"} {
		mode = m
		_, err := c.Slack(context.Background(), secret.Ref{Env: "TAUT_SLACK_X"})
		var fe *fail.Error
		if !errors.As(err, &fe) || fe.Code != CodeBrokerFailed {
			t.Errorf("%s: err = %v, want %s", m, err, CodeBrokerFailed)
		}
		if err != nil && strings.Contains(fmt.Sprintf("%v %+v", err, fe), canaryAccess) {
			t.Errorf("%s: response body leaked into the error: %v", m, err)
		}
	}

	mode = "hang"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = c.Slack(ctx, secret.Ref{Env: "TAUT_SLACK_X"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("canceled request: %v, want context.DeadlineExceeded", err)
	}
}

func TestResponseNeverFormatsItsToken(t *testing.T) {
	// The wire response type is unexported and only ever JSON-encoded onto
	// the socket; make sure nobody adds it to a log line by accident.
	h := start(t)
	path := h.write("slack", canarySlack)
	if _, err := h.client.Slack(context.Background(), secret.Ref{File: path}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.logs.String(), "token issued") {
		t.Errorf("expected a debug line per issued token:\n%s", h.logs.String())
	}
}

// --- security review ----------------------------------------------------------------

// startWith is start with a different configuration.
func startWith(t *testing.T, mutate func(*Config, *harness)) *harness {
	t.Helper()
	h := &harness{t: t, entra: newEntraStub(t), logs: &logBuf{}, now: time.Now()}
	base := shortDir(t)
	h.secrets = filepath.Join(base, "secrets")
	if err := os.Mkdir(h.secrets, 0o700); err != nil {
		t.Fatal(err)
	}
	h.socket = filepath.Join(base, "run", "token.sock")
	cfg := Config{
		Socket:         h.socket,
		SecretDirs:     []string{h.secrets},
		AllowEndpoints: []string{h.entra.URL},
		Log:            slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:            h.Now,
	}
	mutate(&cfg, h)
	srv, err := Listen(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	h.stop = sync.OnceFunc(func() { cancel(); <-done })
	t.Cleanup(h.stop)
	t.Cleanup(h.assertNoLeaks)
	h.client = NewClient(h.socket)
	return h
}

func TestIdentityMustBeAllowed(t *testing.T) {
	ctx := context.Background()
	const next = "token broker's Allow"

	t.Run("no tenants configured refuses everything", func(t *testing.T) {
		h := startWith(t, func(*Config, *harness) {})
		req := h.entraReq(wire.Credential{ClientSecret: secret.Ref{File: h.write("s", canarySecret)}})
		_, _, err := h.client.Entra(ctx, req)
		if fe := h.wantCode(err, CodeIdentityNotAllowed); !strings.Contains(fe.Next, "AllowTenants") {
			t.Errorf("Next does not name the setting: %q", fe.Next)
		}
		if h.entra.Calls() != 0 {
			t.Error("a sign-in was attempted")
		}
	})

	t.Run("tenant, client and cloud", func(t *testing.T) {
		h := startWith(t, func(c *Config, _ *harness) {
			c.AllowTenants = []string{" 11111111-AAAA-4000-8000-000000000001 "}
			c.AllowClients = []string{"22222222-BBBB-4000-8000-000000000002"}
		})
		good := h.entraReq(wire.Credential{ClientSecret: secret.Ref{File: h.write("s", canarySecret)}})
		good.TenantID, good.ClientID = "11111111-aaaa-4000-8000-000000000001", "22222222-bbbb-4000-8000-000000000002"
		if _, _, err := h.client.Entra(ctx, good); err != nil {
			t.Fatalf("allowed identity (different case) refused: %v", err)
		}
		calls := h.entra.Calls()

		for name, mut := range map[string]func(*EntraRequest){
			"other tenant":       func(r *EntraRequest) { r.TenantID = "attacker-tenant" },
			"tenant as prefix":   func(r *EntraRequest) { r.TenantID += "0" },
			"other client":       func(r *EntraRequest) { r.ClientID = "33333333-cccc-4000-8000-000000000003" },
			"another cloud":      func(r *EntraRequest) { r.Cloud = "china"; r.Authority, r.GraphBase = "", "" },
			"gcc high":           func(r *EntraRequest) { r.Cloud = "usgovhigh" },
			"cloud alias of gcc": func(r *EntraRequest) { r.Cloud = "gcc" },
		} {
			req := good
			mut(&req)
			_, _, err := h.client.Entra(ctx, req)
			if fe := h.wantCode(err, CodeIdentityNotAllowed); !strings.Contains(fe.Next, next) {
				t.Errorf("%s: Next does not name a setting: %q", name, fe.Next)
			}
		}
		if h.entra.Calls() != calls {
			t.Error("a refused identity reached the sign-in endpoint")
		}
	})

	// The reviewer's case: an attacker tenant in another built-in cloud,
	// with some other secret as the "client secret". It must be refused
	// before the secret is even looked at, so the answer is the same
	// whether or not the reference is readable.
	t.Run("checked before any secret is resolved", func(t *testing.T) {
		t.Setenv("TAUT_STAGING_KEY", canarySecret)
		h := startWith(t, func(c *Config, _ *harness) {
			c.AllowTenants = []string{"contoso"}
			c.EnvAllow = []string{"TAUT_*"} // even with a careless allow list
		})
		for _, ref := range []secret.Ref{{Env: "TAUT_STAGING_KEY"}, {Env: "PATH"}, {File: "/etc/passwd"}, {File: filepath.Join(h.secrets, "absent")}} {
			req := EntraRequest{TenantID: "attacker", ClientID: "app", Cloud: "china", Credential: wire.Credential{ClientSecret: ref}}
			_, _, err := h.client.Entra(ctx, req)
			h.wantCode(err, CodeIdentityNotAllowed)
		}
	})

	t.Run("an allowed extra cloud works", func(t *testing.T) {
		h := startWith(t, func(c *Config, _ *harness) {
			c.AllowTenants = []string{"contoso"}
			c.AllowClouds = []string{"GCC-High"}
		})
		req := h.entraReq(wire.Credential{ClientSecret: secret.Ref{File: h.write("s", canarySecret)}})
		req.Cloud = "usgovhigh"
		if _, _, err := h.client.Entra(ctx, req); err != nil {
			t.Errorf("allowed cloud refused: %v", err)
		}
		req.Cloud = "" // global is no longer on the list
		_, _, err := h.client.Entra(ctx, req)
		h.wantCode(err, CodeIdentityNotAllowed)
	})
}

func TestDefaultsExcludeOtherTautSecrets(t *testing.T) {
	t.Setenv("TAUT_STAGING_KEY", canarySlack)
	t.Setenv("TAUT_DATABASE_URL", canarySlack)
	t.Setenv("TAUT_CONTROLLER_TOKEN", canarySlack)
	h := startWith(t, func(c *Config, _ *harness) { c.SecretDirs = nil })
	for _, name := range []string{"TAUT_STAGING_KEY", "TAUT_DATABASE_URL", "TAUT_CONTROLLER_TOKEN"} {
		_, err := h.client.Slack(context.Background(), secret.Ref{Env: name})
		h.wantCode(err, CodeRefNotAllowed)
	}
	// The Kubernetes service-account token lives under /run/secrets.
	_, err := h.client.Slack(context.Background(), secret.Ref{File: "/run/secrets/kubernetes.io/serviceaccount/token"})
	h.wantCode(err, CodeRefNotAllowed)
}

func TestSignInDoesNotFollowRedirects(t *testing.T) {
	evil := newEntraStub(t)
	var hits int
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, evil.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	for name, hc := range map[string]*http.Client{
		"default client":  nil,
		"caller's client": {CheckRedirect: func(*http.Request, []*http.Request) error { return nil }},
	} {
		t.Run(name, func(t *testing.T) {
			h := startWith(t, func(c *Config, _ *harness) {
				c.AllowTenants = []string{"contoso"}
				c.AllowEndpoints = []string{redirector.URL}
				c.HTTPClient = hc
			})
			req := h.entraReq(wire.Credential{ClientSecret: secret.Ref{File: h.write("s", canarySecret)}})
			req.Authority, req.GraphBase = redirector.URL, redirector.URL
			before := hits
			_, _, err := h.client.Entra(context.Background(), req)
			h.wantCode(err, "entra.token_request_failed")
			if hits != before+1 {
				t.Errorf("the authority was asked %d times", hits-before)
			}
			if evil.Calls() != 0 || len(evil.secrets) != 0 {
				t.Errorf("the credential followed the redirect: %d calls", evil.Calls())
			}
			if hc != nil && hc.CheckRedirect(nil, nil) != nil {
				t.Error("the caller's client was modified")
			}
		})
	}
}

func TestSocketDirectoryMustBeOwnedByTheBroker(t *testing.T) {
	dir := filepath.Join(shortDir(t), "run")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(dir)
	if _, ok := ownerUID(info); !ok {
		t.Skip("file ownership is not available on this platform")
	}
	real := geteuid
	geteuid = func() int { return real() + 1 } // as if the directory were someone else's
	defer func() { geteuid = real }()
	_, err := Listen(Config{Socket: filepath.Join(dir, "t.sock")})
	var fe *fail.Error
	if !errors.As(err, &fe) || fe.Code != "token.socket_dir_insecure" || !strings.Contains(fe.Why, "owned by uid") {
		t.Fatalf("err = %v, want token.socket_dir_insecure about ownership", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "t.sock")); err == nil {
		t.Error("a socket was created anyway")
	}
}
