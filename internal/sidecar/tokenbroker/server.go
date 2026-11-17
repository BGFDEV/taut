// Package tokenbroker implements the token broker: the one process in a pod that
// can read the migration's credentials.
//
// It serves HTTP/1.1 over a Unix domain socket that only its own user can
// open (socket mode 0600 in a 0700 directory), and answers two questions for
// the egress proxy: "give me an access token for this Entra application"
// and "give me this Slack token". Workers never talk to it.
//
// # Threat model
//
// The requests name credentials by reference (secret.Ref), and those
// references originate in a migration config file, which is untrusted input to
// this privileged process. The broker therefore:
//
//   - reads files only inside Config.SecretDirs, checked lexically and again
//     after symlink evaluation, and opened through os.Root so that a symlink
//     swapped in afterwards cannot escape either;
//   - reads only environment variables whose names match Config.EnvAllow;
//   - returns a Slack reference's value only if it has the shape of a Slack
//     token, so the endpoint cannot be used to read arbitrary files or
//     variables back out;
//   - never returns an Entra credential, only the access token obtained
//     with it;
//   - signs in only as an identity its operator configured: the tenant must
//     be in Config.AllowTenants (empty means no Entra sign-in at all), the
//     client in Config.AllowClients when that is set, and the cloud in
//     Config.AllowClouds (empty means the global cloud only). This is
//     checked before any secret is read or any connection opened; without
//     it a caller could have any readable secret posted as a client secret
//     under a tenant of its choosing;
//   - sends an Entra credential only to the sign-in endpoint of that cloud,
//     or to an endpoint the operator listed in Config.AllowEndpoints
//     (otherwise a config file could name its own "authority" and have the
//     client secret posted to it), and never follows a redirect from it.
//
// Tokens and credentials never appear in logs or error text.
package tokenbroker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/graph"
	"github.com/bgfdev/taut/internal/secret"
	"github.com/bgfdev/taut/internal/sidecar/wire"
)

// Stable error codes.
const (
	CodeRefNotAllowed      = "token.ref_not_allowed"
	CodeSecretMissing      = "token.secret_missing"
	CodeBadRequest         = "token.bad_request"
	CodeEndpointNotAllowed = "token.endpoint_not_allowed"
	CodeIdentityNotAllowed = "token.identity_not_allowed"
	CodeUnreachable        = "token.broker_unreachable"
	CodeBrokerFailed       = "token.broker_failed"
)

// DefaultSecretDir is where file references may point when
// Config.SecretDirs is empty. It is deliberately not /run/secrets, which on
// Kubernetes also holds the pod's service-account token.
const DefaultSecretDir = "/run/secrets/taut" //nolint:gosec // a default path or pattern, not a credential

// DefaultEnvAllow returns the patterns referenced environment variables must
// match when Config.EnvAllow is empty. It is deliberately not TAUT_*, which
// would also match the staging key, the database URL and the control-plane
// token.
func DefaultEnvAllow() []string { return []string{"TAUT_ENTRA_*", "TAUT_SLACK_*"} }

// geteuid is replaced in tests.
var geteuid = os.Geteuid

const (
	maxRequestBody = 64 << 10
	maxSecretFile  = 1 << 20
	// maxSources bounds the number of cached Entra token sources. A
	// migration uses one or two; the bound only stops a misbehaving caller
	// from growing the map.
	maxSources = 64
	// minInvalidateInterval is how often one token source may be told to
	// discard its token. An egress proxy asks after a 401; without the limit an
	// upstream that answers 401 to everything would turn each request into
	// a sign-in.
	minInvalidateInterval = 10 * time.Second
)

// Request is the body of POST /v1/token and POST /v1/invalidate.
type Request = wire.Auth

// EntraRequest identifies an Entra application and its credential.
type EntraRequest = wire.Entra

// response is the body of a successful POST /v1/token.
type response struct {
	Token     string     `json:"token"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// Config configures the token broker.
type Config struct {
	// Socket is the path of the Unix socket to serve on. Its directory is
	// created with mode 0700 if missing and must have exactly that mode.
	Socket string
	// SecretDirs are the directories file references may point into.
	// Defaults to /run/secrets/taut.
	SecretDirs []string
	// EnvAllow are glob patterns (path.Match syntax) that the names of
	// referenced environment variables must match. Defaults to
	// TAUT_ENTRA_* and TAUT_SLACK_*.
	EnvAllow []string
	// AllowTenants are the Entra tenants (directory IDs, or domain names
	// if the config file uses those) the broker may sign in to, matched
	// exactly and case-insensitively. There is no "any tenant" setting:
	// when it is empty every Entra request is refused.
	AllowTenants []string
	// AllowClients are the application (client) IDs the broker may sign
	// in as, matched exactly and case-insensitively. Empty allows any
	// client of an allowed tenant.
	AllowClients []string
	// AllowClouds are the Microsoft clouds (names accepted by
	// graph.LookupCloud) the broker may sign in to. Empty allows only the
	// global cloud.
	AllowClouds []string
	// AllowEndpoints are origins (scheme://host[:port]) that a request's
	// authority or graphBase override may name, in addition to the
	// endpoints of the Microsoft clouds. It exists for local fakes.
	AllowEndpoints []string
	// Log receives one line per credential load, rotation and refusal.
	Log *slog.Logger
	// Now defaults to time.Now.
	Now func() time.Time
	// HTTPClient is used for sign-in requests. Defaults to a client with a
	// 15-second timeout. The broker uses a copy that never follows
	// redirects, whatever the original's CheckRedirect says: a redirect
	// would re-post the credential to a host nobody allowed.
	HTTPClient *http.Client
}

// Server is a listening token broker.
type Server struct {
	cfg    Config
	log    *slog.Logger
	now    func() time.Time
	ln     net.Listener
	socket string

	mu      sync.Mutex
	sources map[string]*source
}

// source is one cached Entra token source.
type source struct {
	fingerprint    string
	tokens         *graph.CachingTokenSource
	last           *recorder
	lastUsed       time.Time
	lastInvalidate time.Time
}

// recorder remembers the expiry of the token most recently fetched.
type recorder struct {
	fetcher graph.TokenFetcher
	mu      sync.Mutex
	value   string
	expires time.Time
}

func (r *recorder) Fetch(ctx context.Context) (graph.AccessToken, error) {
	t, err := r.fetcher.Fetch(ctx)
	if err == nil {
		r.mu.Lock()
		r.value, r.expires = t.Value, t.ExpiresAt
		r.mu.Unlock()
	}
	return t, err
}

func (r *recorder) expiryOf(token string) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.expires, r.value == token && !r.expires.IsZero()
}

// Serve listens on cfg.Socket and serves until ctx is done, then removes the
// socket.
func Serve(ctx context.Context, cfg Config) error {
	s, err := Listen(cfg)
	if err != nil {
		return err
	}
	return s.Serve(ctx)
}

// Listen validates cfg and creates the socket. Call Serve on the result.
func Listen(cfg Config) (*Server, error) {
	s, err := newServer(cfg)
	if err != nil {
		return nil, err
	}
	if cfg.Socket == "" {
		return nil, fail.New("token.config_invalid", "The token broker has no socket path", "",
			"Pass the path of the Unix socket to serve on, for example /run/taut/token/token.sock.").WithExit(fail.ExitUsage)
	}
	socket, err := filepath.Abs(cfg.Socket)
	if err != nil {
		return nil, fail.Wrap(err, "token.config_invalid", "The token broker's socket path is not usable", "", "Pass an absolute path.")
	}
	if err := prepareSocketDir(filepath.Dir(socket)); err != nil {
		return nil, err
	}
	if err := removeStaleSocket(socket); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return nil, fail.Wrap(err, "token.listen_failed", "The token broker could not create its socket", "",
			"Check that "+filepath.Dir(socket)+" is writable by this user and that the path is shorter than about 100 characters.")
	}
	// The directory is already private; the mode is the second lock.
	if err := os.Chmod(socket, 0o600); err != nil {
		_ = ln.Close()
		return nil, fail.Wrap(err, "token.listen_failed", "The token broker could not restrict its socket to mode 0600", "",
			"Check that "+filepath.Dir(socket)+" is on a filesystem that supports permissions.")
	}
	s.ln, s.socket = ln, socket
	return s, nil
}

func newServer(cfg Config) (*Server, error) {
	if len(cfg.SecretDirs) == 0 {
		cfg.SecretDirs = []string{DefaultSecretDir}
	}
	if len(cfg.EnvAllow) == 0 {
		cfg.EnvAllow = DefaultEnvAllow()
	}
	bad := func(what, why string) error {
		return fail.New("token.config_invalid", what, why, "Fix the token broker's configuration and start it again.").WithExit(fail.ExitUsage)
	}
	for _, d := range cfg.SecretDirs {
		if !filepath.IsAbs(d) {
			return nil, bad("A secrets directory is not an absolute path", fmt.Sprintf("%q is relative", d))
		}
	}
	for _, p := range cfg.EnvAllow {
		if _, err := path.Match(p, ""); err != nil || p == "" {
			return nil, bad("An environment allow pattern is not valid", fmt.Sprintf("%q is not a glob pattern", p))
		}
	}
	for i, e := range cfg.AllowEndpoints {
		n := normalizeEndpoint(e)
		if !strings.HasPrefix(n, "http://") && !strings.HasPrefix(n, "https://") {
			return nil, bad("An allowed endpoint is not an origin", fmt.Sprintf("%q must look like https://host[:port]", e))
		}
		cfg.AllowEndpoints[i] = n
	}
	cfg.AllowTenants = normalizeIDs(cfg.AllowTenants)
	cfg.AllowClients = normalizeIDs(cfg.AllowClients)
	clouds := make([]string, 0, len(cfg.AllowClouds))
	for _, name := range cfg.AllowClouds {
		c, err := graph.LookupCloud(name)
		if err != nil || strings.TrimSpace(name) == "" {
			return nil, bad("An allowed cloud is not a known Microsoft cloud", fmt.Sprintf("%q is not a cloud name", name))
		}
		clouds = append(clouds, c.Name)
	}
	if len(clouds) == 0 {
		clouds = []string{graph.CloudGlobal}
	}
	cfg.AllowClouds = clouds
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	hc := http.Client{Timeout: 15 * time.Second}
	if cfg.HTTPClient != nil {
		hc = *cfg.HTTPClient
	}
	// Never follow a redirect: it would carry the credential to a host
	// that is not on any allow list. The 3xx then reads as a failed sign-in.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cfg.HTTPClient = &hc
	var h slog.Handler = slog.DiscardHandler
	if cfg.Log != nil {
		h = cfg.Log.Handler()
	}
	return &Server{
		cfg:     cfg,
		log:     slog.New(secret.NewRedactingHandler(h)).With("sidecar", "token-broker"),
		now:     cfg.Now,
		sources: map[string]*source{},
	}, nil
}

// normalizeIDs lower-cases and trims identifiers, dropping empty ones.
func normalizeIDs(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func prepareSocketDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail.Wrap(err, "token.socket_dir_unusable", "The token broker could not create its socket directory", "",
			"Create "+dir+" with mode 0700, owned by the user the sidecars run as.")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fail.Wrap(err, "token.socket_dir_unusable", "The token broker could not inspect its socket directory", "",
			"Check that "+dir+" exists and is readable.")
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		// Refuse rather than chmod: the path may be a shared directory
		// such as /tmp or a volume root, which is not ours to lock.
		return fail.New("token.socket_dir_insecure",
			"The token broker's socket directory is open to other users",
			fmt.Sprintf("%s has mode %04o; it must be 0700 so that only the sidecars can reach the socket", dir, perm),
			"Put the socket in a dedicated subdirectory (the broker creates it with the right mode), or run `chmod 700 "+dir+"`.")
	}
	// Mode 0700 only helps if the directory is ours: in somebody else's,
	// they can replace the socket with their own listener.
	if uid, ok := ownerUID(info); ok && uid != geteuid() {
		return fail.New("token.socket_dir_insecure",
			"The token broker's socket directory belongs to another user",
			fmt.Sprintf("%s is owned by uid %d, but the broker runs as uid %d; its owner could replace the socket", dir, uid, geteuid()),
			"Put the socket in a directory owned by the user the sidecars run as, or let the broker create a dedicated subdirectory.")
	}
	return nil
}

// removeStaleSocket removes a socket file left by a previous run, but only
// if it is a socket and nothing is listening on it.
func removeStaleSocket(socket string) error {
	info, err := os.Lstat(socket)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fail.Wrap(err, "token.listen_failed", "The token broker could not inspect its socket path", "",
			"Check the permissions of "+filepath.Dir(socket)+".")
	}
	if info.Mode()&fs.ModeSocket == 0 {
		return fail.New("token.socket_path_taken", "The token broker's socket path is occupied by something that is not a socket",
			socket+" exists and is a "+describeMode(info.Mode()),
			"Remove it, or configure a different socket path.")
	}
	conn, err := net.DialTimeout("unix", socket, 500*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		return fail.New("token.socket_in_use", "Another token broker is already serving on this socket",
			"a process accepted a connection on "+socket,
			"Stop the other broker, or configure a different socket path.")
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fail.Wrap(err, "token.listen_failed", "The token broker could not remove a stale socket", "",
			"Remove "+socket+" by hand and start the broker again.")
	}
	return nil
}

func describeMode(m fs.FileMode) string {
	switch {
	case m.IsDir():
		return "directory"
	case m&fs.ModeSymlink != 0:
		return "symbolic link"
	case m.IsRegular():
		return "regular file"
	default:
		return "special file"
	}
}

// Serve serves until ctx is done, then shuts down and removes the socket.
func (s *Server) Serve(ctx context.Context) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	s.log.Info("token broker listening", "socket", s.socket)
	err := srv.Serve(s.ln)
	<-done
	_ = os.Remove(s.socket) // the listener unlinks it too; this covers odd filesystems
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Handler returns the broker's HTTP handler. It is exported for tests; in
// production it is reachable only through the socket.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
	})
	mux.HandleFunc("POST /v1/token", s.handleToken)
	mux.HandleFunc("POST /v1/invalidate", s.handleInvalidate)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		wire.WriteError(w, http.StatusNotFound, fail.New(CodeBadRequest, "The token broker has no such endpoint", "",
			"Use POST /v1/token, POST /v1/invalidate or GET /healthz."))
	})
	return mux
}

func (s *Server) decode(w http.ResponseWriter, r *http.Request) (Request, bool) {
	var req Request
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	switch {
	case err != nil || len(body) > maxRequestBody:
		err = fail.New(CodeBadRequest, "The token request could not be read", "the body is missing, truncated or too large",
			"Send a JSON object of at most 64 KiB.")
	case json.Unmarshal(body, &req) != nil:
		err = fail.New(CodeBadRequest, "The token request is not valid JSON", "",
			"Send a JSON object with a \"kind\" of \"entra\" or \"slack\".")
	default:
		err = req.Validate(CodeBadRequest)
		if err == nil && req.Kind == wire.KindNone {
			err = fail.New(CodeBadRequest, "The token request asks for no token", "kind \"none\" needs no token",
				"Send kind \"entra\" or \"slack\".")
		}
	}
	if err != nil {
		s.refuse(w, req, err)
		return req, false
	}
	return req, true
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	req, ok := s.decode(w, r)
	if !ok {
		return
	}
	var (
		resp response
		err  error
	)
	switch req.Kind {
	case wire.KindEntra:
		resp, err = s.entra(r.Context(), req.Entra)
	case wire.KindSlack:
		resp, err = s.slack(req.Token)
	}
	if err != nil {
		s.refuse(w, req, err)
		return
	}
	s.log.Debug("token issued", "kind", req.Kind, "tenant", req.TenantID)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleInvalidate(w http.ResponseWriter, r *http.Request) {
	req, ok := s.decode(w, r)
	if !ok {
		return
	}
	invalidated := false
	if req.Kind == wire.KindEntra {
		now := s.now()
		s.mu.Lock()
		if src := s.sources[sourceKey(req.Entra)]; src != nil && now.Sub(src.lastInvalidate) >= minInvalidateInterval {
			src.lastInvalidate = now
			src.tokens.Invalidate()
			invalidated = true
		}
		s.mu.Unlock()
	}
	if invalidated {
		s.log.Info("cached token discarded at the egress proxy's request", "tenant", req.TenantID, "client", req.ClientID)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"invalidated": invalidated})
}

// refuse logs and writes an error. Nothing derived from a secret's value is
// ever part of err; see the constructors below.
func (s *Server) refuse(w http.ResponseWriter, req Request, err error) {
	fe := fail.From(err)
	s.log.Warn("token request refused", "code", fe.Code, "kind", req.Kind, "tenant", req.TenantID, "why", fe.Why)
	wire.WriteError(w, statusFor(fe.Code), fe)
}

func statusFor(code string) int {
	switch {
	case code == CodeBadRequest, code == "entra.unknown_cloud", code == "entra.config_incomplete",
		code == "entra.config_invalid", code == "graph.config_invalid":
		return http.StatusBadRequest
	case code == CodeRefNotAllowed, code == CodeEndpointNotAllowed, code == CodeIdentityNotAllowed:
		return http.StatusForbidden
	case code == CodeSecretMissing:
		return http.StatusNotFound
	case code == "graph.canceled":
		return http.StatusRequestTimeout
	case strings.HasPrefix(code, "entra."):
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

// --- Entra ------------------------------------------------------------------------

func sourceKey(e EntraRequest) string {
	b, _ := json.Marshal(e)
	return string(b)
}

func normalizeEndpoint(s string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(s), "/"))
}

func (s *Server) endpointAllowed(override, cloudDefault string) bool {
	if strings.TrimSpace(override) == "" {
		return true
	}
	n := normalizeEndpoint(override)
	if n == normalizeEndpoint(cloudDefault) {
		return true
	}
	for _, e := range s.cfg.AllowEndpoints {
		if n == e {
			return true
		}
	}
	return false
}

func (s *Server) entra(ctx context.Context, req EntraRequest) (response, error) {
	cloud, err := graph.LookupCloud(req.Cloud)
	if err != nil {
		return response{}, err
	}
	// Who may be signed in as is settled before anything is read or sent.
	identity := func(why, setting string) error {
		return fail.New(CodeIdentityNotAllowed,
			"The token broker is not configured to sign in as this identity", why,
			"If the config file is right, add the value to the token broker's "+setting+" setting and restart it; otherwise correct the config file.")
	}
	switch tenant, client := strings.ToLower(strings.TrimSpace(req.TenantID)), strings.ToLower(strings.TrimSpace(req.ClientID)); {
	case len(s.cfg.AllowTenants) == 0:
		return response{}, identity("no Entra tenant is allowed: the broker's list of allowed tenants is empty", "AllowTenants (allowed tenants)")
	case !contains(s.cfg.AllowTenants, tenant):
		return response{}, identity(fmt.Sprintf("tenant %q is not in the broker's list of allowed tenants", secret.Redact(req.TenantID)), "AllowTenants (allowed tenants)")
	case len(s.cfg.AllowClients) > 0 && !contains(s.cfg.AllowClients, client):
		return response{}, identity(fmt.Sprintf("client %q is not in the broker's list of allowed clients", secret.Redact(req.ClientID)), "AllowClients (allowed clients)")
	case !contains(s.cfg.AllowClouds, cloud.Name):
		return response{}, identity(fmt.Sprintf("cloud %q is not in the broker's list of allowed clouds (%s)", cloud.Name, strings.Join(s.cfg.AllowClouds, ", ")), "AllowClouds (allowed clouds)")
	}
	for _, o := range []struct{ name, override, def string }{
		{"authority", req.Authority, cloud.Authority},
		{"graphBase", req.GraphBase, cloud.GraphBase},
	} {
		if !s.endpointAllowed(o.override, o.def) {
			return response{}, fail.New(CodeEndpointNotAllowed,
				"The token request names an endpoint the token broker may not use",
				fmt.Sprintf("%s %q is neither the %s cloud's endpoint nor in the broker's list of allowed endpoints; "+
					"credentials are only ever sent to those", o.name, secret.Redact(o.override), cloud.Name),
				"Remove the override from the config file and set the cloud name instead. For a local fake, add its origin to the token broker's allowed endpoints.")
		}
	}

	src, err := s.source(req)
	if err != nil {
		return response{}, err
	}
	tok, err := src.tokens.Token(ctx)
	if err != nil {
		return response{}, err
	}
	resp := response{Token: tok}
	if exp, ok := src.last.expiryOf(tok); ok {
		resp.ExpiresAt = &exp
	}
	return resp, nil
}

// source returns the cached token source for req, (re)building it when the
// credential behind the reference has changed.
func (s *Server) source(req EntraRequest) (*source, error) {
	ref, isCert := req.Credential.ClientSecret, false
	if !req.Credential.Certificate.IsZero() {
		ref, isCert = req.Credential.Certificate, true
	}
	h, err := s.open(ref)
	if err != nil {
		return nil, err
	}
	defer h.Close()

	key := sourceKey(req)
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if src := s.sources[key]; src != nil && src.fingerprint == h.fingerprint {
		src.lastUsed = now
		return src, nil
	}

	// New or rotated: read the credential. Holding the lock keeps
	// concurrent first requests from each signing in separately; the read
	// is a small local file.
	value, err := h.read()
	if err != nil {
		return nil, err
	}
	var cred graph.Credential
	if isCert {
		cred, err = graph.ParseClientCertificate([]byte(value))
	} else {
		cred, err = graph.NewClientSecret(value)
	}
	if err != nil {
		return nil, err
	}
	cc, err := graph.NewClientCredentials(graph.CredentialsOptions{
		TenantID:   req.TenantID,
		ClientID:   req.ClientID,
		Credential: cred,
		Cloud:      req.Cloud,
		Authority:  req.Authority,
		GraphBase:  req.GraphBase,
		HTTPClient: s.cfg.HTTPClient,
		UserAgent:  "taut-token-broker",
		Now:        s.now,
	})
	if err != nil {
		return nil, err
	}
	rec := &recorder{fetcher: cc}
	_, rotated := s.sources[key]
	src := &source{
		fingerprint: h.fingerprint,
		tokens:      graph.NewCachingTokenSource(rec, graph.CacheOptions{Now: s.now}),
		last:        rec,
		lastUsed:    now,
	}
	s.sources[key] = src
	s.evictLocked()
	msg := "entra credential loaded"
	if rotated {
		msg = "entra credential changed; reloaded"
	}
	s.log.Info(msg, "tenant", req.TenantID, "client", req.ClientID, "credential", cred.Kind(), "from", ref.Describe())
	return src, nil
}

func (s *Server) evictLocked() {
	for len(s.sources) > maxSources {
		var oldest string
		var at time.Time
		for k, src := range s.sources {
			if oldest == "" || src.lastUsed.Before(at) {
				oldest, at = k, src.lastUsed
			}
		}
		delete(s.sources, oldest)
	}
}

// --- Slack ------------------------------------------------------------------------

// slackTokenRE is the whole-value shape of a Slack token: bot, user, app,
// legacy and rotating forms.
var slackTokenRE = regexp.MustCompile(`^(?:xoxe\.)?(?:xox[abeprs]|xapp|xoxe)-[A-Za-z0-9._-]{10,}$`)

func (s *Server) slack(ref secret.Ref) (response, error) {
	h, err := s.open(ref)
	if err != nil {
		return response{}, err
	}
	defer h.Close()
	value, err := h.read()
	if err != nil {
		return response{}, err
	}
	if !slackTokenRE.MatchString(value) {
		// Say nothing about the value: this branch is what stops the
		// endpoint from being a file and environment reader.
		return response{}, fail.New(CodeRefNotAllowed,
			"The referenced value is not a Slack token",
			"the value at "+ref.Describe()+" does not have the shape of a Slack token (xoxb-…, xoxp-…, xoxe.…), so it was not returned",
			"Point the config file's Slack token reference at the bot or user token itself.")
	}
	return response{Token: value}, nil
}

// --- references -------------------------------------------------------------------

// handle is an allowed, opened secret reference.
type handle struct {
	// fingerprint changes when the secret behind the reference changes.
	fingerprint string
	read        func() (string, error)
	close       func()
}

func (h *handle) Close() {
	if h.close != nil {
		h.close()
	}
}

func notAllowed(ref secret.Ref, why, next string) error {
	return fail.New(CodeRefNotAllowed, "The token broker may not read "+ref.Describe(), why, next)
}

func missing(ref secret.Ref, why, next string) error {
	return fail.New(CodeSecretMissing, "The secret at "+ref.Describe()+" is not available to the token broker", why, next)
}

// open checks that ref is one the broker may read and opens it.
func (s *Server) open(ref secret.Ref) (*handle, error) {
	if err := ref.Validate(); err != nil {
		fe := fail.From(err)
		return nil, fail.New(CodeBadRequest, fe.What, fe.Why, fe.Next)
	}
	if ref.Env != "" {
		return s.openEnv(ref)
	}
	return s.openFile(ref)
}

func (s *Server) openEnv(ref secret.Ref) (*handle, error) {
	allowed := false
	for _, p := range s.cfg.EnvAllow {
		if ok, _ := path.Match(p, ref.Env); ok {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, notAllowed(ref,
			"the variable's name matches none of the allowed patterns ("+strings.Join(s.cfg.EnvAllow, ", ")+")",
			"Rename the variable to match (for example TAUT_SLACK_TOKEN) and update the config file, or widen the token broker's allow list.")
	}
	value := strings.TrimSpace(os.Getenv(ref.Env))
	if value == "" {
		return nil, missing(ref, "the variable is unset or empty in the token broker's environment",
			"Set "+ref.Env+" on the token broker container (not on the worker) and restart it.")
	}
	sum := sha256.Sum256([]byte(value))
	return &handle{
		fingerprint: "env:" + ref.Env + ":" + hex.EncodeToString(sum[:]),
		read:        func() (string, error) { return value, nil },
	}, nil
}

// within reports whether p is strictly inside dir, and its path relative to
// dir. Both must be clean absolute paths.
func within(p, dir string) (rel string, ok bool) {
	if dir == string(filepath.Separator) {
		return "", false // "/" as a secrets directory would allow everything
	}
	rel, found := strings.CutPrefix(p, dir+string(filepath.Separator))
	return rel, found && rel != ""
}

func (s *Server) openFile(ref secret.Ref) (*handle, error) {
	dirs := strings.Join(s.cfg.SecretDirs, ", ")
	nextMove := "Mount the secret under " + dirs + " in the token broker container and reference that path in the config file."
	if !filepath.IsAbs(ref.File) {
		return nil, notAllowed(ref, "the path is relative; the token broker only reads absolute paths inside "+dirs, nextMove)
	}
	clean := filepath.Clean(ref.File)

	// Lexical containment first, so that nothing outside the secrets
	// directories is ever touched, not even to ask whether it exists.
	var root, rel string
	for _, d := range s.cfg.SecretDirs {
		d = filepath.Clean(d)
		candidates := []string{d}
		if real, err := filepath.EvalSymlinks(d); err == nil && real != d {
			candidates = append(candidates, real)
		}
		for _, c := range candidates {
			if r, ok := within(clean, c); ok {
				root, rel = c, r
				break
			}
		}
		if root != "" {
			break
		}
	}
	if root == "" {
		return nil, notAllowed(ref, "the path is outside the token broker's secrets directories ("+dirs+")", nextMove)
	}

	// Then the real location, for a clear answer about symlinks.
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, missing(ref, "the secrets directory "+root+" does not exist in the token broker container", nextMove)
	}
	real, err := filepath.EvalSymlinks(clean)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, missing(ref, "the file does not exist in the token broker container",
				"Mount the secret at that path in the token broker container, or correct the path in the config file.")
		}
		return nil, missing(ref, "the file cannot be reached by the token broker", "Check the mount and its permissions in the token broker container.")
	}
	if _, ok := within(real, realRoot); !ok {
		return nil, notAllowed(ref, "the path is a symbolic link that leads outside the token broker's secrets directories ("+dirs+")", nextMove)
	}

	// Finally open it through os.Root, which refuses to leave the
	// directory even if a link is swapped between the check and the open.
	rootDir, err := os.OpenRoot(root)
	if err != nil {
		return nil, missing(ref, "the secrets directory "+root+" cannot be opened", "Check the mount and its permissions in the token broker container.")
	}
	f, err := rootDir.Open(rel)
	_ = rootDir.Close()
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil, missing(ref, "the file does not exist in the token broker container", nextMove)
		case errors.Is(err, fs.ErrPermission):
			return nil, missing(ref, "the token broker's user may not read the file", "Fix the file's ownership or mode in the token broker container.")
		default:
			return nil, notAllowed(ref, "the path could not be opened without leaving the secrets directory "+root, nextMove)
		}
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, notAllowed(ref, "the path does not name a regular file", "Point the reference at the file that holds the secret.")
	}
	return &handle{
		fingerprint: fmt.Sprintf("file:%s:%d:%d", real, info.ModTime().UnixNano(), info.Size()),
		close:       func() { _ = f.Close() },
		read: func() (string, error) {
			data, err := io.ReadAll(io.LimitReader(f, maxSecretFile+1))
			if err != nil {
				return "", missing(ref, "the file could not be read", "Check the mount in the token broker container.")
			}
			if len(data) > maxSecretFile {
				return "", notAllowed(ref, "the file is larger than 1 MiB, which no token or key is", "Point the reference at the secret itself.")
			}
			value := strings.TrimSpace(string(data))
			if value == "" {
				return "", missing(ref, "the file is empty", "Write the secret into the file.")
			}
			return value, nil
		},
	}, nil
}
