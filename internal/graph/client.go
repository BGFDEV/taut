// Package graph is a small hand-written Microsoft Graph client: Entra ID
// app-only authentication, a retrying JSON transport, paging, and the
// handful of endpoints Taut needs.
//
// Every error returned to callers is a *fail.Error with a stable code, so it
// can be shown to an operator as-is. Tokens, secrets and signed assertions
// never appear in errors or in the formatted output of any type here.
package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bgfdev/taut/internal/fail"
)

// RetryPolicy bounds how the client retries throttled and failed requests.
// Zero fields take the defaults noted on each.
type RetryPolicy struct {
	// MaxAttempts is the total number of sends per request. Default 5.
	MaxAttempts int
	// MaxTotalWait caps the summed sleep per request. A Retry-After that
	// would exceed it fails the request instead. Default 2 minutes.
	MaxTotalWait time.Duration
	// BaseDelay is the first backoff ceiling when there is no Retry-After;
	// it doubles per attempt. Default 500 ms.
	BaseDelay time.Duration
	// MaxDelay caps a single backoff. Default 30 seconds.
	MaxDelay time.Duration
}

// Options configure a Client.
type Options struct {
	// BaseURL is the Graph root without a version segment, for example
	// https://graph.microsoft.com. Empty means the global cloud.
	BaseURL string
	// HTTPClient defaults to a client with a 100-second timeout.
	HTTPClient *http.Client
	// TokenSource is required. If it also has an Invalidate() method (as
	// CachingTokenSource does), it is called once when Graph answers 401.
	TokenSource TokenSource
	// UserAgent is sent on every request when set.
	UserAgent string
	Retry     RetryPolicy
	// Sleep waits between attempts; it must return early with the context's
	// error when ctx ends. Defaults to a timer. Tests inject a fake.
	Sleep func(ctx context.Context, d time.Duration) error
	// Now is used to interpret HTTP-date Retry-After values. Defaults to
	// time.Now.
	Now func() time.Time
	// Jitter returns a number in [0,1). Defaults to math/rand.
	Jitter func() float64
	// Provisioning bounds how long CreateTeam waits for a new team.
	Provisioning ProvisioningPolicy
	// Header holds static headers added to every request the client sends,
	// upload chunks included (for a proxy's own authentication, say).
	Header http.Header
	// RewriteURL, when set, is consulted for every absolute URL the client
	// is about to call that it did not build from BaseURL: upload session
	// URLs always, and @odata.nextLink values that point at another host.
	// It returns the URL to call instead and headers to add to that
	// request, which lets a caller route such URLs through a proxy. When it
	// is nil, upload URLs are called directly and a paging link to another
	// host is refused.
	RewriteURL func(absolute string) (target string, header http.Header)
}

// ProvisioningPolicy bounds the wait for a long-running Graph operation
// (team creation). Zero fields take the defaults noted on each.
type ProvisioningPolicy struct {
	// PollInterval is the wait before the second status check; it doubles
	// per check up to 10 seconds. Default 1 second.
	PollInterval time.Duration
	// MaxWait caps the summed wait between checks. Default 5 minutes.
	MaxWait time.Duration
}

// Client talks to Microsoft Graph. It is safe for concurrent use.
type Client struct {
	base      *url.URL
	hc        *http.Client
	ts        TokenSource
	userAgent string
	retry     RetryPolicy
	sleep     func(context.Context, time.Duration) error
	now       func() time.Time
	jitter    func() float64
	provision ProvisioningPolicy
	header    http.Header
	rewrite   func(string) (string, http.Header)
}

// New builds a Client.
func New(opts Options) (*Client, error) {
	if opts.TokenSource == nil {
		return nil, fail.New("graph.config_invalid",
			"The Graph client has no token source",
			"Options.TokenSource is nil.",
			"This is a bug in Taut; please report it.")
	}
	raw, err := cleanBase(firstNonEmpty(opts.BaseURL, "https://graph.microsoft.com"), "Graph base URL")
	if err != nil {
		return nil, err
	}
	base, _ := url.Parse(raw)
	c := &Client{
		base: base, hc: opts.HTTPClient, ts: opts.TokenSource, userAgent: opts.UserAgent,
		retry: opts.Retry, sleep: opts.Sleep, now: opts.Now, jitter: opts.Jitter,
		provision: opts.Provisioning, header: opts.Header.Clone(), rewrite: opts.RewriteURL,
	}
	if c.provision.PollInterval <= 0 {
		c.provision.PollInterval = time.Second
	}
	if c.provision.MaxWait <= 0 {
		c.provision.MaxWait = 5 * time.Minute
	}
	if c.hc == nil {
		c.hc = &http.Client{Timeout: 100 * time.Second}
	}
	if c.retry.MaxAttempts <= 0 {
		c.retry.MaxAttempts = 5
	}
	if c.retry.MaxTotalWait <= 0 {
		c.retry.MaxTotalWait = 2 * time.Minute
	}
	if c.retry.BaseDelay <= 0 {
		c.retry.BaseDelay = 500 * time.Millisecond
	}
	if c.retry.MaxDelay <= 0 {
		c.retry.MaxDelay = 30 * time.Second
	}
	if c.sleep == nil {
		c.sleep = sleepCtx
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.jitter == nil {
		c.jitter = rand.Float64
	}
	return c, nil
}

// request describes one Graph call.
type request struct {
	method string
	// path is relative to the base URL ("/v1.0/users"). Ignored when
	// absolute is set.
	path  string
	query url.Values
	// absolute is a full URL from @odata.nextLink.
	absolute string
	// body is JSON-encoded when non-nil.
	body any
	// permission is the application permission this endpoint needs; it
	// makes 403 errors actionable.
	permission string
	// retryAmbiguous marks a non-idempotent request that is nevertheless
	// safe to send again after a failure that may have committed (transport
	// error, 5xx), because the server rejects the duplicate.
	retryAmbiguous bool
	// classify may turn an error response into a more specific error than
	// the generic mapping; it returns nil to fall through.
	classify func(*APIError) *fail.Error
	// header, when non-nil, receives the response headers of a 2xx answer.
	header *http.Header
	// status, when non-nil, receives the status code of a 2xx answer.
	status *int
	// raw is sent as the body as-is (when body is nil); hasRaw says so even
	// for an empty body. send adds extra request headers.
	raw    []byte
	hasRaw bool
	send   http.Header
	// external marks an absolute URL that is pre-authorized and may be on
	// another host (an upload session): no Authorization header is sent.
	external bool
	// label replaces the URL path in errors, for URLs that are secrets.
	label string
	// noBlindRetry stops retries after failures that may have committed
	// even for an idempotent method; the caller reconciles first.
	noBlindRetry bool
}

// failed converts an error response for rq into the operator-facing error.
func (rq request) failed(e *APIError, giveUp string) *fail.Error {
	if rq.classify != nil && e.Status != 0 {
		if fe := rq.classify(e); fe != nil {
			return fe
		}
	}
	return failure(e, rq.permission, giveUp)
}

// invalidator is implemented by token sources that cache.
type invalidator interface{ Invalidate() }

// do sends rq, retrying as allowed, and decodes a 2xx JSON body into out
// (when out is non-nil).
//
// Retries: 429, 503, 504 and transport errors are retried for idempotent
// methods. For other methods only 429 is retried, because it means the
// request was rejected before processing; a 5xx or a dropped connection may
// have committed. Requests marked retryAmbiguous are the exception: they
// are retried on 429, 500, 502, 503, 504 and transport errors. Bodies are
// always replayable because they are marshalled up front.
func (c *Client) do(ctx context.Context, rq request, out any) error {
	target, extra, err := c.resolve(rq)
	if err != nil {
		return err
	}
	shown := target.Path
	if rq.label != "" {
		shown = rq.label
	}
	body := rq.raw
	if rq.hasRaw && body == nil {
		body = []byte{}
	}
	if rq.body != nil {
		if body, err = json.Marshal(rq.body); err != nil {
			return fail.Wrap(err, "graph.bad_request", "A Graph request could not be encoded", "",
				"This is a bug in Taut; please report it.")
		}
	}
	idempotent := isIdempotent(rq.method) && !rq.noBlindRetry
	repeatable := idempotent || rq.retryAmbiguous

	var (
		waited    time.Duration
		refreshed bool
	)
	for attempt := 1; ; attempt++ {
		var token string
		if !rq.external {
			// An empty token means "send no Authorization header": a proxy
			// in front of Graph adds it.
			if token, err = c.ts.Token(ctx); err != nil {
				return err // already structured by the token source
			}
		}
		resp, respBody, sendErr := c.send(ctx, rq, target, body, token, extra) //nolint:bodyclose // send reads and closes the body
		if sendErr != nil && ctx.Err() != nil {
			return canceled(ctx, "calling Microsoft Graph")
		}

		var (
			apiErr *APIError
			delay  time.Duration
		)
		switch {
		case sendErr != nil:
			var ue *url.Error
			if rq.label != "" && errors.As(sendErr, &ue) {
				sendErr = ue.Err // the URL is a secret; keep only the cause
			}
			apiErr = &APIError{Method: rq.method, Path: shown, Attempts: attempt, Err: sendErr}
			if !repeatable {
				return rq.failed(apiErr, "The request is not safe to repeat automatically.")
			}
			delay = c.backoff(attempt)
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			if rq.header != nil {
				*rq.header = resp.Header
			}
			if rq.status != nil {
				*rq.status = resp.StatusCode
			}
			if out == nil || resp.StatusCode == http.StatusNoContent || len(respBody) == 0 {
				return nil
			}
			if err := json.Unmarshal(respBody, out); err != nil {
				return fail.Wrap(err, "graph.bad_response",
					"Microsoft Graph returned a response Taut could not read",
					rq.method+" "+shown+" answered with a body that is not the expected JSON: "+err.Error()+".",
					"Check that nothing between Taut and Graph (proxy, captive portal) rewrites responses, then retry.")
			}
			return nil
		default:
			apiErr = parseAPIError(rq.method, shown, resp, respBody)
			apiErr.Attempts = attempt
			retryAfter, hasRetryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), c.now())
			apiErr.RetryAfter = retryAfter

			if resp.StatusCode == http.StatusUnauthorized && !refreshed && !rq.external {
				if inv, ok := c.ts.(invalidator); ok {
					// The cached token may have expired early or been
					// revoked; get a fresh one once, without a delay.
					refreshed = true
					inv.Invalidate()
					attempt--
					continue
				}
			}
			if !retryable(resp.StatusCode, idempotent, rq.retryAmbiguous) {
				return rq.failed(apiErr, "")
			}
			if hasRetryAfter {
				// Honor the server's wait in full, plus a little jitter so
				// parallel callers do not return in lockstep.
				delay = retryAfter + time.Duration(c.jitter()*float64(min(retryAfter/10, time.Second)))
			} else {
				delay = c.backoff(attempt)
			}
		}

		if attempt >= c.retry.MaxAttempts {
			return rq.failed(apiErr, "The retry limit was reached.")
		}
		if waited+delay > c.retry.MaxTotalWait {
			return rq.failed(apiErr, "The next retry would wait "+delay.Round(time.Millisecond).String()+
				", which exceeds the "+c.retry.MaxTotalWait.String()+" wait budget.")
		}
		if err := c.sleep(ctx, delay); err != nil {
			if ctx.Err() != nil {
				return canceled(ctx, "waiting to retry a Microsoft Graph request")
			}
			return fail.Wrap(err, "graph.canceled", "Stopped while waiting to retry a Microsoft Graph request", "",
				"Run the command again.")
		}
		waited += delay
	}
}

// send performs one HTTP exchange and reads the whole response body.
func (c *Client) send(ctx context.Context, rq request, target *url.URL, body []byte, token string, extra http.Header) (*http.Response, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, rq.method, target.String(), rd)
	if err != nil {
		return nil, nil, err
	}
	if rq.hasRaw {
		// The caller reuses this buffer for the next chunk. The transport
		// may still be writing it after it has returned a response (a
		// server can answer before reading the body), so wait until every
		// body handed to the transport has been closed.
		var inFlight sync.WaitGroup
		newBody := func() (io.ReadCloser, error) {
			inFlight.Add(1)
			return &trackedBody{Reader: bytes.NewReader(body), done: inFlight.Done}, nil
		}
		req.Body, _ = newBody()
		req.GetBody = newBody
		req.ContentLength = int64(len(body))
		defer func() {
			released := make(chan struct{})
			go func() { inFlight.Wait(); close(released) }()
			select {
			case <-released:
			case <-ctx.Done():
			}
		}()
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("client-request-id", newGUID())
	switch {
	case rq.hasRaw:
		req.Header.Set("Content-Type", "application/octet-stream")
	case body != nil:
		req.Header.Set("Content-Type", "application/json")
	}
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	for _, h := range []http.Header{c.header, rq.send, extra} {
		for k, vs := range h {
			req.Header[http.CanonicalHeaderKey(k)] = append([]string(nil), vs...)
		}
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	return resp, data, nil
}

// trackedBody reports when the transport has finished with a request body.
type trackedBody struct {
	*bytes.Reader
	once sync.Once
	done func()
}

func (b *trackedBody) Close() error {
	b.once.Do(b.done)
	return nil
}

// resolve builds the request URL. Absolute URLs (next links) must stay on
// the configured Graph host, so a bearer token is never sent elsewhere.
//
// Upload session URLs (rq.external) are pre-authorized and may live on any
// host; they are sent without a token. Options.RewriteURL may redirect those,
// and paging links to another host, through a proxy.
func (c *Client) resolve(rq request) (*url.URL, http.Header, error) {
	if rq.absolute != "" {
		u, err := url.Parse(rq.absolute)
		sameHost := err == nil && strings.EqualFold(u.Host, c.base.Host) && u.Scheme == c.base.Scheme
		switch {
		case err == nil && c.rewrite != nil && (rq.external || !sameHost):
			target, hdr := c.rewrite(rq.absolute)
			if u, err = url.Parse(target); err != nil || u.Host == "" {
				return nil, nil, fail.New("graph.config_invalid",
					"A rewritten Graph URL is not usable",
					"Options.RewriteURL returned a value that is not an absolute URL.",
					"Check the proxy settings; this may be a bug in Taut.")
			}
			return u, hdr, nil
		case err == nil && u.Host != "" && rq.external:
			return u, nil, nil
		case rq.external:
			return nil, nil, fail.New("graph.bad_response",
				"Microsoft Graph returned an upload URL Taut cannot use",
				"The uploadUrl of the upload session is not an absolute URL.",
				"Run the command again. If it repeats, report it with --verbose output.")
		case !sameHost:
			return nil, nil, fail.New("graph.bad_response",
				"Microsoft Graph returned a paging link Taut will not follow",
				"The @odata.nextLink does not point at "+c.base.Host+", so following it could leak the access token.",
				"Check that the configured Graph endpoint and cloud are correct, and that no proxy rewrites responses.")
		}
		return u, nil, nil
	}
	// rq.path is already escaped segment by segment, so it is parsed
	// rather than assigned to URL.Path (which would escape it again).
	u, err := url.Parse(strings.TrimRight(c.base.String(), "/") + rq.path)
	if err != nil {
		return nil, nil, fail.Wrap(err, "graph.bad_request", "A Graph request URL could not be built", "",
			"This is a bug in Taut; please report it.")
	}
	if len(rq.query) > 0 {
		// Graph wants %20, not "+", for spaces in OData expressions.
		u.RawQuery = strings.ReplaceAll(rq.query.Encode(), "+", "%20")
	}
	return u, nil, nil
}

// backoff returns a capped exponential delay with full jitter.
func (c *Client) backoff(attempt int) time.Duration {
	ceiling := c.retry.BaseDelay
	for i := 1; i < attempt && ceiling < c.retry.MaxDelay; i++ {
		ceiling *= 2
	}
	ceiling = min(ceiling, c.retry.MaxDelay)
	return time.Duration(c.jitter() * float64(ceiling))
}

func isIdempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete, http.MethodOptions:
		return true
	}
	return false
}

func retryable(status int, idempotent, ambiguous bool) bool {
	switch status {
	case http.StatusTooManyRequests:
		return true
	case http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return idempotent || ambiguous
	case http.StatusInternalServerError, http.StatusBadGateway:
		return ambiguous
	}
	return false
}

// parseRetryAfter reads a Retry-After header: delay seconds or an HTTP-date.
func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		if secs < 0 {
			secs = 0
		}
		return time.Duration(secs * float64(time.Second)), true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(t.Sub(now), 0), true
	}
	return 0, false
}

// sleepCtx waits for d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
