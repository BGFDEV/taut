package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bgfdev/taut/internal/fail"
)

// TokenSource supplies bearer tokens for Microsoft Graph.
type TokenSource interface {
	// Token returns a token that is valid now.
	Token(ctx context.Context) (string, error)
}

// AccessToken is a bearer token and its expiry. Its formatted output never
// contains the token.
type AccessToken struct {
	// Value is the bearer token. Treat it as a secret.
	Value string
	// ExpiresAt is when Entra ID said the token stops being valid.
	ExpiresAt time.Time
}

// Format implements fmt.Formatter so the token cannot be printed by any verb.
func (t AccessToken) Format(f fmt.State, _ rune) {
	fmt.Fprintf(f, "graph.AccessToken{[redacted], expires: %s}", t.ExpiresAt.UTC().Format(time.RFC3339))
}

// TokenFetcher requests a brand-new token on every call.
type TokenFetcher interface {
	Fetch(ctx context.Context) (AccessToken, error)
}

// CredentialsOptions configure the client-credentials flow.
type CredentialsOptions struct {
	// TenantID is the directory (tenant) ID or a verified domain name.
	TenantID string
	// ClientID is the application (client) ID.
	ClientID string
	// Credential is a *ClientCertificate (preferred) or *ClientSecret.
	Credential Credential
	// Cloud is a name accepted by LookupCloud; empty means global.
	Cloud string
	// Authority overrides the cloud's sign-in host, e.g. for a fake server.
	Authority string
	// GraphBase overrides the cloud's Graph root; the requested scope is
	// GraphBase + "/.default".
	GraphBase string
	// HTTPClient defaults to a client with a 30-second timeout.
	HTTPClient *http.Client
	// UserAgent is sent on token requests when set.
	UserAgent string
	// Now defaults to time.Now.
	Now func() time.Time
}

// ClientCredentials performs the OAuth 2.0 client-credentials flow against
// Entra ID. It does not cache; wrap it with NewCachingTokenSource.
type ClientCredentials struct {
	tenantID  string
	clientID  string
	cred      Credential
	endpoint  string
	graphBase string
	hc        *http.Client
	userAgent string
	now       func() time.Time
}

// NewClientCredentials validates opts and returns a token fetcher.
func NewClientCredentials(opts CredentialsOptions) (*ClientCredentials, error) {
	cloud, err := LookupCloud(opts.Cloud)
	if err != nil {
		return nil, err
	}
	missing := func(what string) error {
		return fail.New("entra.config_incomplete",
			"The Entra ID configuration is incomplete",
			"No "+what+" was provided.",
			"Run `taut init` to set it, or add it to the config file, then run `taut auth`.",
		).WithExit(fail.ExitUsage)
	}
	tenant := strings.TrimSpace(opts.TenantID)
	switch {
	case tenant == "":
		return nil, missing("tenant ID")
	case strings.TrimSpace(opts.ClientID) == "":
		return nil, missing("client ID")
	case opts.Credential == nil:
		return nil, missing("certificate or client secret")
	}
	authority, err := cleanBase(firstNonEmpty(opts.Authority, cloud.Authority), "authority")
	if err != nil {
		return nil, err
	}
	graphBase, err := cleanBase(firstNonEmpty(opts.GraphBase, cloud.GraphBase), "Graph base URL")
	if err != nil {
		return nil, err
	}
	c := &ClientCredentials{
		tenantID:  tenant,
		clientID:  strings.TrimSpace(opts.ClientID),
		cred:      opts.Credential,
		endpoint:  authority + "/" + url.PathEscape(tenant) + "/oauth2/v2.0/token",
		graphBase: graphBase,
		hc:        opts.HTTPClient,
		userAgent: opts.UserAgent,
		now:       opts.Now,
	}
	if c.hc == nil {
		c.hc = &http.Client{Timeout: 30 * time.Second}
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c, nil
}

// TokenEndpoint returns the URL tokens are requested from.
func (c *ClientCredentials) TokenEndpoint() string { return c.endpoint }

// GraphBase returns the Graph root the tokens are scoped to. Pass it as
// Options.BaseURL so the client and its tokens always agree.
func (c *ClientCredentials) GraphBase() string { return c.graphBase }

// CredentialKind reports KindCertificate or KindSecret.
func (c *ClientCredentials) CredentialKind() string { return c.cred.Kind() }

// Format implements fmt.Formatter; credentials are never printed.
func (c ClientCredentials) Format(f fmt.State, _ rune) {
	fmt.Fprintf(f, "graph.ClientCredentials{tenant: %s, client: %s, credential: %s}", c.tenantID, c.clientID, c.cred.Kind())
}

// Token implements TokenSource by fetching a new token every time.
func (c *ClientCredentials) Token(ctx context.Context) (string, error) {
	t, err := c.Fetch(ctx)
	return t.Value, err
}

// Fetch requests a new access token.
func (c *ClientCredentials) Fetch(ctx context.Context) (AccessToken, error) {
	now := c.now()
	form := url.Values{
		"grant_type": {"client_credentials"},
		"client_id":  {c.clientID},
		"scope":      {c.graphBase + "/.default"},
	}
	sensitive, err := c.cred.authenticate(form, c.clientID, c.endpoint, now)
	if err != nil {
		return AccessToken{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return AccessToken{}, fail.New("entra.config_invalid",
			"The Entra ID token endpoint is not a valid URL",
			"Built from the configured authority and tenant ID.",
			"Check the tenant ID and cloud in the config file.").WithExit(fail.ExitUsage)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("client-request-id", newGUID())
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return AccessToken{}, canceled(ctx, "requesting a token from Entra ID")
		}
		return AccessToken{}, fail.Wrap(err, "entra.unreachable",
			"Entra ID could not be reached",
			scrub(err.Error(), sensitive)+".",
			"Check network access and proxy settings for "+hostOf(c.endpoint)+
				", confirm the cloud setting matches the tenant, then run `taut doctor`.")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != http.StatusOK {
		return AccessToken{}, c.tokenFailure(resp.StatusCode, body, sensitive)
	}
	var ok struct {
		AccessToken string          `json:"access_token"`
		ExpiresIn   json.RawMessage `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &ok); err != nil || ok.AccessToken == "" {
		return AccessToken{}, fail.New("entra.bad_response",
			"Entra ID returned a response Taut could not read",
			"The token endpoint answered 200 without a usable access_token.",
			"Check that nothing between Taut and "+hostOf(c.endpoint)+" (proxy, captive portal) rewrites responses, then retry.")
	}
	secs, err := strconv.ParseFloat(strings.Trim(string(ok.ExpiresIn), `"`), 64)
	if err != nil || secs <= 0 {
		secs = 300 // be conservative when the lifetime is unreadable
	}
	return AccessToken{Value: ok.AccessToken, ExpiresAt: now.Add(time.Duration(secs * float64(time.Second)))}, nil
}

// TokenError is the error body Entra ID returns from the token endpoint. It
// is carried inside the *fail.Error for callers that need the details.
type TokenError struct {
	Status int
	// OAuthError is the `error` field, e.g. "invalid_client".
	OAuthError string
	// AADSTS is the Entra error code, e.g. "AADSTS7000215"; empty if absent.
	AADSTS string
	// Description is the first line of `error_description`, scrubbed.
	Description string
	// TraceID and CorrelationID identify the request to Microsoft support.
	TraceID       string
	CorrelationID string
}

func (e *TokenError) Error() string {
	s := fmt.Sprintf("entra token endpoint: %d %s", e.Status, e.OAuthError)
	if e.Description != "" {
		s += ": " + e.Description
	}
	return s
}

var aadstsRE = regexp.MustCompile(`AADSTS\d+`)

func (c *ClientCredentials) tokenFailure(status int, body []byte, sensitive []string) error {
	var raw struct {
		Error         string `json:"error"`
		Description   string `json:"error_description"`
		Codes         []int  `json:"error_codes"`
		TraceID       string `json:"trace_id"`
		CorrelationID string `json:"correlation_id"`
	}
	_ = json.Unmarshal(body, &raw)
	desc, _, _ := strings.Cut(raw.Description, "\n")
	desc, _, _ = strings.Cut(desc, " Trace ID:")
	te := &TokenError{
		Status:        status,
		OAuthError:    raw.Error,
		Description:   scrub(strings.TrimSpace(desc), sensitive),
		TraceID:       raw.TraceID,
		CorrelationID: raw.CorrelationID,
	}
	if len(raw.Codes) > 0 {
		te.AADSTS = "AADSTS" + strconv.Itoa(raw.Codes[0])
	} else {
		te.AADSTS = aadstsRE.FindString(raw.Description)
	}

	trace := ""
	if te.TraceID != "" {
		trace = " (trace ID " + te.TraceID + ")"
	}
	const portal = "Entra admin center → App registrations → your app → Certificates & secrets"
	mk := func(code, what, why, next string) error {
		return fail.Wrap(te, code, what, why+trace, next)
	}

	switch te.AADSTS {
	case "AADSTS700016":
		return mk("entra.app_not_found",
			"Entra ID does not know this application",
			fmt.Sprintf("No application with client ID %s exists in tenant %s (AADSTS700016).", c.clientID, c.tenantID),
			"Check the client ID and tenant ID in the config file. If the app was registered in a different tenant, "+
				"run scripts/entra-app.sh against this one, then `taut auth`.")
	case "AADSTS7000215":
		return mk("entra.invalid_secret",
			"Entra ID rejected the client secret",
			"The secret does not match any secret on the app registration (AADSTS7000215). "+
				"A common cause is copying the Secret ID instead of the secret Value.",
			"In "+portal+", create a new secret, copy its Value into the env var or file the config file references, "+
				"and run `taut auth`. Better: switch to a certificate.")
	case "AADSTS7000222":
		return mk("entra.expired_secret",
			"The Entra client secret has expired",
			"The secret was valid once but is past its expiry date (AADSTS7000222).",
			"In "+portal+", create a new secret (or upload a certificate), update the config file's reference, and run `taut auth`.")
	case "AADSTS700027":
		why := "Entra ID could not match the signed assertion to a certificate registered on the app (AADSTS700027)."
		next := "Upload the certificate (public part only) in " + portal + ", or point the config file at the key pair that is already registered, then run `taut auth`."
		if cc, ok := c.cred.(*ClientCertificate); ok {
			why = fmt.Sprintf("The certificate with thumbprint %s is not registered on the app, or its signature did not verify (AADSTS700027).", cc.Thumbprint())
		}
		return mk("entra.bad_certificate", "Entra ID rejected the certificate", why, next)
	case "AADSTS700024":
		return mk("entra.clock_skew",
			"Entra ID rejected the signed assertion's time window",
			"The assertion's validity period does not include Entra's current time (AADSTS700024), which means this machine's clock is wrong.",
			"Synchronize the system clock (NTP), then run `taut auth` again.")
	case "AADSTS90002":
		return mk("entra.tenant_not_found",
			"Entra ID could not find the tenant",
			fmt.Sprintf("Tenant %q does not exist at %s (AADSTS90002).", c.tenantID, hostOf(c.endpoint)),
			"Check the tenant ID in the config file (Entra admin center → Overview → Tenant ID). "+
				"For GCC High, DoD or China tenants set the cloud to usgovhigh, dod or china.")
	}
	if status == http.StatusTooManyRequests || status >= 500 {
		return mk("entra.unavailable",
			"Entra ID is temporarily unable to issue tokens",
			fmt.Sprintf("The token endpoint answered HTTP %d. %s", status, te.Description),
			"Wait a minute and run the command again. If it keeps failing, check https://status.cloud.microsoft.")
	}
	why := te.Description
	if why == "" {
		why = fmt.Sprintf("The token endpoint answered HTTP %d with error %q.", status, te.OAuthError)
	}
	return mk("entra.token_request_failed",
		"Entra ID refused to issue a token",
		why,
		"Check the tenant ID, client ID and credential in the config file, then run `taut auth`. "+
			"Search the AADSTS code at https://login.microsoftonline.com/error for details.")
}

// CacheOptions configure a CachingTokenSource.
type CacheOptions struct {
	// RefreshBefore is how long before expiry a token is replaced.
	// Defaults to five minutes. For tokens that live less than twice this
	// long, half the lifetime is used instead.
	RefreshBefore time.Duration
	// Now defaults to time.Now.
	Now func() time.Time
}

// CachingTokenSource caches a token until shortly before it expires. It is
// safe for concurrent use; concurrent callers share one fetch.
type CachingTokenSource struct {
	fetcher TokenFetcher
	margin  time.Duration
	now     func() time.Time

	mu sync.Mutex
	// tok is a pointer so a stray copy of this struct formats as an address.
	tok        *AccessToken
	validUntil time.Time
	inflight   *fetchCall
}

type fetchCall struct {
	done chan struct{}
	tok  AccessToken
	err  error
}

// NewCachingTokenSource wraps fetcher with a cache.
func NewCachingTokenSource(fetcher TokenFetcher, opts CacheOptions) *CachingTokenSource {
	c := &CachingTokenSource{fetcher: fetcher, margin: opts.RefreshBefore, now: opts.Now}
	if c.margin <= 0 {
		c.margin = 5 * time.Minute
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c
}

// NewTokenSource is the usual way to authenticate: client credentials with
// a cache in front. opts.Now is used by both layers.
func NewTokenSource(opts CredentialsOptions) (*CachingTokenSource, error) {
	cc, err := NewClientCredentials(opts)
	if err != nil {
		return nil, err
	}
	return NewCachingTokenSource(cc, CacheOptions{Now: opts.Now}), nil
}

// Format implements fmt.Formatter; the cached token is never printed.
func (c *CachingTokenSource) Format(f fmt.State, _ rune) {
	fmt.Fprint(f, "graph.CachingTokenSource{[redacted]}")
}

// Invalidate drops the cached token so the next call fetches a new one. The
// client calls it when Graph answers 401.
func (c *CachingTokenSource) Invalidate() {
	c.mu.Lock()
	c.validUntil = time.Time{}
	c.mu.Unlock()
}

// Token implements TokenSource.
func (c *CachingTokenSource) Token(ctx context.Context) (string, error) {
	for {
		c.mu.Lock()
		if c.now().Before(c.validUntil) {
			v := c.tok.Value
			c.mu.Unlock()
			return v, nil
		}
		call := c.inflight
		leader := call == nil
		if leader {
			call = &fetchCall{done: make(chan struct{})}
			c.inflight = call
		}
		c.mu.Unlock()

		if leader {
			started := c.now()
			call.tok, call.err = c.fetcher.Fetch(ctx)
			c.mu.Lock()
			if call.err == nil {
				margin := c.margin
				if life := call.tok.ExpiresAt.Sub(started); life < 2*margin {
					margin = life / 2
				}
				c.tok, c.validUntil = &call.tok, call.tok.ExpiresAt.Add(-margin)
			}
			c.inflight = nil
			c.mu.Unlock()
			close(call.done)
			return call.tok.Value, call.err
		}

		select {
		case <-call.done:
		case <-ctx.Done():
			return "", canceled(ctx, "waiting for a token from Entra ID")
		}
		if call.err == nil {
			return call.tok.Value, nil
		}
		// The leader's own context ending is not this caller's failure.
		if errors.Is(call.err, context.Canceled) || errors.Is(call.err, context.DeadlineExceeded) {
			continue
		}
		return "", call.err
	}
}

// canceled wraps a context error so it stays matchable with errors.Is.
func canceled(ctx context.Context, doing string) error {
	err := ctx.Err()
	why := "The operation was canceled."
	if errors.Is(err, context.DeadlineExceeded) {
		why = "The operation's deadline passed."
	}
	return fail.Wrap(err, "graph.canceled",
		"Stopped while "+doing, why,
		"Run the command again; nothing was changed by the interrupted request.")
}

// scrub removes sensitive values from text that came from outside.
func scrub(s string, sensitive []string) string {
	for _, v := range sensitive {
		if v != "" {
			s = strings.ReplaceAll(s, v, "[redacted]")
		}
	}
	return s
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// cleanBase validates an absolute http(s) base URL and trims trailing slashes.
func cleanBase(raw, what string) (string, error) {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fail.New("graph.config_invalid",
			"The "+what+" is not a valid URL",
			"It must be an absolute http(s) URL without a query, such as https://graph.microsoft.com.",
			"Fix the endpoint override, or remove it and set the cloud name instead.").WithExit(fail.ExitUsage)
	}
	return s, nil
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return "the endpoint"
}
