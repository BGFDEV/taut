// Package fakegraph is an in-memory fake of the Entra ID token endpoint and
// the parts of Microsoft Graph that Taut uses. It runs in-process behind
// httptest for tests and as a standalone server (see Serve) for local
// development.
//
// It aims to fail the way the real services fail: the same status codes,
// error bodies and AADSTS codes, role enforcement per endpoint, paging with
// opaque skip tokens, throttling with Retry-After, and injectable faults.
package fakegraph

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Option customizes a Server.
type Option func(*config)

type config struct {
	now           func() time.Time
	sleep         func(context.Context, time.Duration)
	tokenLifetime time.Duration
	rps           float64
	burst         int
	randSeed      uint64
	pageSize      int
	laxAudience   bool

	channelRPS       float64
	channelBurst     int
	provisionPolls   int
	provisionDelay   time.Duration
	provisionFailFor []string
	lenientFrom      bool
	opaqueMessageIDs bool
	maxMessageBytes  int

	filesFolderLag     int
	privateSiteLag     int
	noStartMigration   bool
	literalStart       bool
	filesFolderBlocked bool
	lenientAttachments bool
	refuseReactions    bool
	maxHostedBytes     int
	uploadLifetime     time.Duration

	// Load-test modes; see load.go.
	latency, jitter  time.Duration
	chatRPS          float64
	chatBurst        int
	tenantChannelRPS float64
	tenantChatRPS    float64

	chatHistoryRefused  bool
	chatAuthorsMembers  bool
	chatBackdateIgnored bool
	chatMentionsMembers bool
	chatExpandLimit     int
}

// WithClock sets the server's clock, used for token expiry and throttling.
func WithClock(now func() time.Time) Option { return func(c *config) { c.now = now } }

// WithSleep replaces how latency faults wait, so tests need not really sleep.
func WithSleep(sleep func(ctx context.Context, d time.Duration)) Option {
	return func(c *config) { c.sleep = sleep }
}

// WithTokenLifetime sets how long issued access tokens are valid. The
// default is one hour.
func WithTokenLifetime(d time.Duration) Option { return func(c *config) { c.tokenLifetime = d } }

// WithRateLimit sets the per-app-per-tenant token bucket for Graph calls.
// The default (10,000 requests per second) is effectively unlimited.
func WithRateLimit(rps float64, burst int) Option {
	return func(c *config) { c.rps, c.burst = rps, burst }
}

// WithRandSeed seeds the generator behind probabilistic faults. The default
// seed is 1, so runs are reproducible unless a caller chooses otherwise.
func WithRandSeed(seed uint64) Option { return func(c *config) { c.randSeed = seed } }

// WithPageSize sets the page size used when a request has no $top. The
// default is 100, as in Graph.
func WithPageSize(n int) Option { return func(c *config) { c.pageSize = n } }

// WithLaxAudience stops Graph endpoints from requiring that the token's
// audience host matches the request's Host header. Use it when a proxy
// rewrites Host between the client and the fake.
func WithLaxAudience() Option { return func(c *config) { c.laxAudience = true } }

// WithChannelRateLimit adds a per-channel token bucket for message and reply
// POSTs, on top of the per-app bucket. A throttled POST answers 429 with
// Retry-After and stores nothing. By default there is no per-channel limit.
func WithChannelRateLimit(rps float64, burst int) Option {
	return func(c *config) { c.channelRPS, c.channelBurst = rps, burst }
}

// WithTeamProvisioningPolls makes a new team's createTeam operation answer
// "inProgress" to its first n GETs. Until provisioning has succeeded the
// team is invisible: GET /teams/{id} is 404. The default is 0: the team is
// ready as soon as POST /teams returns.
func WithTeamProvisioningPolls(n int) Option { return func(c *config) { c.provisionPolls = n } }

// WithTeamProvisioningDelay keeps a new team in "inProgress" until the
// server clock has advanced by d since it was created. It combines with
// WithTeamProvisioningPolls: both conditions must hold.
func WithTeamProvisioningDelay(d time.Duration) Option {
	return func(c *config) { c.provisionDelay = d }
}

// WithTeamProvisioningFailure makes provisioning end in "failed" for teams
// created with one of these display names; such a team never appears.
func WithTeamProvisioningFailure(displayNames ...string) Option {
	return func(c *config) { c.provisionFailFor = append(c.provisionFailFor, displayNames...) }
}

// WithLenientFrom accepts imported messages whose from.user.id is not a
// user of the tenant. Real Graph's behavior here is undocumented; the
// default rejects them with 400, the conservative reading.
func WithLenientFrom() Option { return func(c *config) { c.lenientFrom = true } }

// WithOpaqueMessageIDs gives imported messages unique IDs that are not
// derived from createdDateTime. By default the ID is the timestamp in Unix
// milliseconds, as observed (but not documented) on the real service.
func WithOpaqueMessageIDs() Option { return func(c *config) { c.opaqueMessageIDs = true } }

// WithMaxMessageBytes sets the largest accepted body.content, in UTF-8
// bytes. The default is DefaultMaxMessageBytes.
func WithMaxMessageBytes(n int) Option { return func(c *config) { c.maxMessageBytes = n } }

// WithFilesFolderBlockedInMigration makes GET …/filesFolder answer 404
// while the channel is in migration mode. Whether the real service serves
// it then is not documented; by default the fake does.
func WithFilesFolderBlockedInMigration() Option {
	return func(c *config) { c.filesFolderBlocked = true }
}

// WithFilesFolderLag makes the first n GET …/filesFolder calls of every
// channel answer 404, as the real service can for a brand-new channel.
func WithFilesFolderLag(n int) Option { return func(c *config) { c.filesFolderLag = n } }

// WithStartMigrationUnavailable makes POST …/startMigration answer 404, as
// in the national clouds (GCC High, DoD, 21Vianet), where that API does not
// exist and the only import path is creating teams and channels in
// migration mode.
func WithStartMigrationUnavailable() Option { return func(c *config) { c.noStartMigration = true } }

// WithLiteralMigrationStart makes startMigration read an absent
// conversationCreationDateTime the way the parameter table words it: "If
// not provided, the current date and time is used." The current time is
// not older than the createdDateTime of a channel or chat that exists, so
// the request is refused with 400 like any other value that is not older.
//
// By default the fake takes the other reading the documentation supports
// (the creation time "can only be moved towards the past", and the
// reference's first example sends no body and succeeds): an absent value
// leaves the creation time alone. Microsoft's pages do not say which holds.
// A caller that always sends a value older than the current creation time
// works under both, and this option is how a test proves it does.
func WithLiteralMigrationStart() Option { return func(c *config) { c.literalStart = true } }

// WithPrivateChannelSiteLag makes the first n GET …/filesFolder calls of
// every private or shared channel answer 404, on top of WithFilesFolderLag.
// Such a channel has a SharePoint site of its own, which the real service
// provisions only when the folder is first asked for and which can take
// minutes to appear.
func WithPrivateChannelSiteLag(n int) Option { return func(c *config) { c.privateSiteLag = n } }

// WithLenientAttachments stops the fake from requiring that a message
// attachment's id and contentUrl are the eTag GUID and webDavUrl of an
// existing drive item. That check is the fake's strictness, not documented
// Graph behavior.
func WithLenientAttachments() Option { return func(c *config) { c.lenientAttachments = true } }

// WithReactionsRefused makes an import that carries reactions answer 400.
// Microsoft's import guide lists reactions as in scope and the chatMessage
// reference has a reactions collection, but no page shows an import request
// with one. By default the fake accepts them as the chatMessageReaction
// reference describes; this option is the other possibility.
func WithReactionsRefused() Option { return func(c *config) { c.refuseReactions = true } }

// WithMaxHostedContentBytes sets the largest accepted hosted content, in
// decoded bytes. The default is DefaultMaxHostedContentBytes.
func WithMaxHostedContentBytes(n int) Option { return func(c *config) { c.maxHostedBytes = n } }

// WithUploadSessionLifetime sets how long an upload session stays valid.
// The default is 24 hours.
func WithUploadSessionLifetime(d time.Duration) Option {
	return func(c *config) { c.uploadLifetime = d }
}

// DefaultMaxHostedContentBytes is the default limit on one hosted content.
const DefaultMaxHostedContentBytes = 4 << 20

// DefaultMaxMessageBytes is the default limit on a message's body.content.
const DefaultMaxMessageBytes = 100 * 1024

// Server is the fake. It implements http.Handler and is safe for
// concurrent use.
type Server struct {
	cfg config

	// mu guards st. Reads take the read lock; write endpoints take the
	// write lock.
	mu sync.RWMutex
	st *state

	imports, conflicts, throttles                 atomic.Int64
	uploadsCompleted, uploadBytes, uploadSessions atomic.Int64

	signingKey []byte
	faults     *Faults
	limiter    *limiter
	log        *requestLog
	graph      *http.ServeMux
	admin      *http.ServeMux
}

// New builds a server from seed. It panics if the seed is invalid; call
// Seed.Validate first when the seed comes from outside the program.
func New(seed Seed, opts ...Option) *Server {
	st, err := newState(seed)
	if err != nil {
		panic("fakegraph: invalid seed: " + err.Error())
	}
	cfg := config{
		now: time.Now, sleep: sleepCtx, tokenLifetime: time.Hour,
		rps: 10000, burst: 10000, randSeed: 1, pageSize: 100,
		maxMessageBytes: DefaultMaxMessageBytes,
		maxHostedBytes:  DefaultMaxHostedContentBytes, uploadLifetime: 24 * time.Hour,
		chatExpandLimit: DefaultChatMemberExpandLimit,
	}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.rps <= 0 || cfg.burst <= 0 || cfg.pageSize <= 0 || cfg.tokenLifetime <= 0 {
		panic("fakegraph: rate limit, page size and token lifetime must be positive")
	}
	if cfg.channelRPS < 0 || (cfg.channelRPS > 0 && cfg.channelBurst <= 0) || cfg.provisionPolls < 0 ||
		cfg.provisionDelay < 0 || cfg.maxMessageBytes <= 0 || cfg.filesFolderLag < 0 || cfg.privateSiteLag < 0 ||
		cfg.maxHostedBytes <= 0 || cfg.uploadLifetime <= 0 {
		panic("fakegraph: channel rate limit, provisioning and message size options must not be negative")
	}
	s := &Server{
		cfg: cfg, st: st,
		signingKey: make([]byte, 32),
		faults:     newFaults(cfg.randSeed),
		limiter:    newLimiter(),
		log:        &requestLog{},
	}
	_, _ = rand.Read(s.signingKey)
	s.routes()
	return s
}

// Faults returns the fault-injection rule set.
func (s *Server) Faults() *Faults { return s.faults }

// RequestRecord is one entry of the request log. It never holds a token,
// secret or request body.
type RequestRecord struct {
	Seq    int       `json:"seq"`
	Time   time.Time `json:"time"`
	Method string    `json:"method"`
	Path   string    `json:"path"`
	Query  string    `json:"query,omitempty"`
	// Status is the response status; zero means the connection was dropped.
	Status int `json:"status"`
	// AppID is the calling application, when it could be identified.
	AppID string `json:"appId,omitempty"`
	// Fault is the ID of the terminal fault rule that fired, if any.
	Fault string `json:"fault,omitempty"`
}

// maxLogEntries bounds the request log so a long-running standalone server
// does not grow without limit; the oldest entries are discarded.
const maxLogEntries = 50000

type requestLog struct {
	mu      sync.Mutex
	entries []RequestRecord
	seq     int
}

func (l *requestLog) add(e RequestRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	e.Seq = l.seq
	if len(l.entries) >= maxLogEntries {
		l.entries = append(l.entries[:0], l.entries[maxLogEntries/10:]...)
	}
	l.entries = append(l.entries, e)
}

// Requests returns a copy of the request log, oldest first. Calls to the
// /_fake control endpoints and /healthz are not logged.
func (s *Server) Requests() []RequestRecord {
	s.log.mu.Lock()
	defer s.log.mu.Unlock()
	return append([]RequestRecord(nil), s.log.entries...)
}

// ResetRequests empties the request log.
func (s *Server) ResetRequests() {
	s.log.mu.Lock()
	s.log.entries = nil
	s.log.mu.Unlock()
}

// CountRequests counts logged requests whose method and path match. An
// empty method matches all; pathPattern uses the Fault.Path syntax.
func (s *Server) CountRequests(method, pathPattern string) int {
	n := 0
	for _, r := range s.Requests() {
		if (method == "" || strings.EqualFold(method, r.Method)) && matchPath(pathPattern, r.Path) {
			n++
		}
	}
	return n
}

type recordKey struct{}

// record returns the log entry being built for this request.
func record(r *http.Request) *RequestRecord {
	if e, ok := r.Context().Value(recordKey{}).(*RequestRecord); ok {
		return e
	}
	return &RequestRecord{}
}

// statusWriter remembers the response status.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// discardWriter swallows a response; used by fail_after_commit. It still
// counts as a writer for the handler, so the handler commits as usual.
type discardWriter struct{ h http.Header }

func (d discardWriter) Header() http.Header       { return d.h }
func (discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (discardWriter) WriteHeader(int)             {}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" || strings.HasPrefix(r.URL.Path, "/_fake/") {
		s.admin.ServeHTTP(w, r)
		return
	}

	entry := &RequestRecord{Time: s.cfg.now().UTC(), Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery}
	sw := &statusWriter{ResponseWriter: w}
	r = r.WithContext(context.WithValue(r.Context(), recordKey{}, entry))
	// Deferred so dropped connections (which unwind with a panic) are
	// logged too, with status zero.
	defer func() {
		entry.Status = sw.status
		s.log.add(*entry)
	}()

	latency, fault := s.faults.decide(r)
	latency += s.cfg.delay()
	if latency > 0 {
		s.cfg.sleep(r.Context(), latency)
	}
	if fault != nil {
		entry.Fault = fault.ID
		switch fault.Action {
		case ActionDrop:
			panic(http.ErrAbortHandler)
		case ActionFailAfterCommit:
			s.route(discardWriter{h: http.Header{}}, r)
			if fault.Status == 0 {
				panic(http.ErrAbortHandler)
			}
			s.writeFaultStatus(sw, fault)
			return
		case ActionThrottle:
			s.identify(r)
			s.throttled(sw, time.Duration(fault.RetryAfterSeconds)*time.Second)
			return
		case ActionStatus:
			s.identify(r)
			s.writeFaultStatus(sw, fault)
			return
		}
	}
	s.route(sw, r)
}

// writeFaultStatus answers with an injected error status.
func (s *Server) writeFaultStatus(w http.ResponseWriter, fault *Fault) {
	if fault.RetryAfterSeconds > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(fault.RetryAfterSeconds))
	}
	code := "generalException"
	switch fault.Status {
	case http.StatusServiceUnavailable:
		code = "serviceNotAvailable"
	case http.StatusGatewayTimeout:
		code = "GatewayTimeout"
	case http.StatusTooManyRequests:
		code = "TooManyRequests"
		s.throttles.Add(1)
	}
	writeGraphError(w, s.cfg.now(), fault.Status, code, "Injected fault "+fault.ID+".")
}

// throttled answers 429 with Retry-After (whole seconds, rounded up) and
// counts it.
func (s *Server) throttled(w http.ResponseWriter, wait time.Duration) {
	s.throttles.Add(1)
	writeThrottled(w, s.cfg.now(), int(math.Ceil(wait.Seconds())))
}

// route dispatches to the Entra or Graph side.
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/v1.0/"):
		s.graph.ServeHTTP(w, r)
	case strings.HasPrefix(r.URL.Path, uploadPrefix):
		s.handleUpload(w, r)
	case strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token"):
		s.handleToken(w, r)
	default:
		http.NotFound(w, r)
	}
}

// identify fills the log entry's app ID for faulted Graph requests, which
// never reach the authenticating handler. It ignores invalid tokens.
func (s *Server) identify(r *http.Request) {
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		if c, err := verifyAccessToken(s.signingKey, tok); err == nil {
			record(r).AppID = c.AppID
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeGraphError writes Graph's standard error envelope.
func writeGraphError(w http.ResponseWriter, now time.Time, status int, code, message string) {
	id := newGUID()
	w.Header().Set("request-id", id)
	w.Header().Set("client-request-id", id)
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": message,
			"innerError": map[string]any{
				"date":              now.UTC().Format("2006-01-02T15:04:05"),
				"request-id":        id,
				"client-request-id": id,
			},
		},
	})
}

func writeThrottled(w http.ResponseWriter, now time.Time, retryAfterSeconds int) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
	writeGraphError(w, now, http.StatusTooManyRequests, "TooManyRequests",
		"Too many requests. Please retry after the period indicated in the Retry-After header.")
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}
