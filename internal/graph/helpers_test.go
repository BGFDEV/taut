package graph_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/fakegraph"
	"github.com/bgfdev/taut/internal/graph"
)

// clock is a manually advanced clock shared by client and fake server.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)} }

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

// sleeper records requested waits and advances the clock instead of sleeping.
type sleeper struct {
	clock *clock
	mu    sync.Mutex
	waits []time.Duration
}

func (s *sleeper) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.waits = append(s.waits, d)
	s.mu.Unlock()
	s.clock.Advance(d)
	return nil
}

func (s *sleeper) Waits() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.waits...)
}

// env is a fake tenant plus a real client wired to it.
type env struct {
	t      *testing.T
	fake   *fakegraph.Server
	http   *httptest.Server
	clock  *clock
	sleep  *sleeper
	tokens *graph.CachingTokenSource
	client *graph.Client
}

type envOpt func(*envCfg)

type envCfg struct {
	seed       fakegraph.Seed
	serverOpts []fakegraph.Option
	cred       graph.Credential
	clientID   string
	tenant     string
	retry      graph.RetryPolicy
}

func withSeed(s fakegraph.Seed) envOpt { return func(c *envCfg) { c.seed = s } }
func withServer(o ...fakegraph.Option) envOpt {
	return func(c *envCfg) { c.serverOpts = append(c.serverOpts, o...) }
}
func withCredential(cr graph.Credential) envOpt { return func(c *envCfg) { c.cred = cr } }
func withClientID(id string) envOpt             { return func(c *envCfg) { c.clientID = id } }
func withTenant(id string) envOpt               { return func(c *envCfg) { c.tenant = id } }
func withRetry(p graph.RetryPolicy) envOpt      { return func(c *envCfg) { c.retry = p } }
func withRoles(roles ...string) envOpt {
	return func(c *envCfg) { c.seed.Apps[0].Roles = roles }
}

func newEnv(t *testing.T, opts ...envOpt) *env {
	t.Helper()
	cfg := envCfg{seed: fakegraph.DefaultSeed(), clientID: fakegraph.DefaultClientID, tenant: fakegraph.DefaultTenantID}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.cred == nil {
		cfg.cred = mustSecret(t, fakegraph.DefaultSecret)
	}
	e := &env{t: t, clock: newClock()}
	e.sleep = &sleeper{clock: e.clock}
	serverOpts := append([]fakegraph.Option{
		fakegraph.WithClock(e.clock.Now),
		fakegraph.WithSleep(func(ctx context.Context, d time.Duration) { _ = e.sleep.Sleep(ctx, d) }),
	}, cfg.serverOpts...)
	e.fake = fakegraph.New(cfg.seed, serverOpts...)
	e.http = httptest.NewServer(e.fake)
	t.Cleanup(e.http.Close)

	var err error
	e.tokens, err = graph.NewTokenSource(graph.CredentialsOptions{
		TenantID: cfg.tenant, ClientID: cfg.clientID, Credential: cfg.cred,
		Authority: e.http.URL, GraphBase: e.http.URL,
		HTTPClient: e.http.Client(), Now: e.clock.Now,
	})
	if err != nil {
		t.Fatalf("NewTokenSource: %v", err)
	}
	e.client, err = graph.New(graph.Options{
		BaseURL: e.http.URL, HTTPClient: e.http.Client(), TokenSource: e.tokens,
		UserAgent: "taut-test", Retry: cfg.retry,
		Sleep: e.sleep.Sleep, Now: e.clock.Now, Jitter: func() float64 { return 0.5 },
	})
	if err != nil {
		t.Fatalf("graph.New: %v", err)
	}
	return e
}

const tokenPath = "/*/oauth2/v2.0/token"

func mustSecret(t *testing.T, s string) *graph.ClientSecret {
	t.Helper()
	c, err := graph.NewClientSecret(s)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// keyPair is a generated RSA key with a self-signed certificate.
type keyPair struct {
	key     *rsa.PrivateKey
	certDER []byte
	certPEM []byte
}

// RSA generation is slow under -race, so the suite shares two key pairs.
var testKeys = sync.OnceValue(func() [2]keyPair {
	var out [2]keyPair
	for i := range out {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(int64(i + 1)),
			Subject:      pkix.Name{CommonName: fmt.Sprintf("taut-test-%d", i)},
			NotBefore:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			NotAfter:     time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC),
			KeyUsage:     x509.KeyUsageDigitalSignature,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			panic(err)
		}
		out[i] = keyPair{key: key, certDER: der, certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
	}
	return out
})

func (k keyPair) pkcs8PEM() []byte {
	der, err := x509.MarshalPKCS8PrivateKey(k.key)
	if err != nil {
		panic(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func (k keyPair) pkcs1PEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k.key)})
}

// combined is the cert followed by the PKCS#8 key, the layout Taut documents.
func (k keyPair) combined() []byte { return append(append([]byte{}, k.certPEM...), k.pkcs8PEM()...) }

func mustCert(t *testing.T, pemData []byte) *graph.ClientCertificate {
	t.Helper()
	c, err := graph.ParseClientCertificate(pemData)
	if err != nil {
		t.Fatalf("ParseClientCertificate: %v", err)
	}
	return c
}
