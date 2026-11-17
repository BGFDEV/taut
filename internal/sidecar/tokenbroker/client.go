package tokenbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/secret"
	"github.com/bgfdev/taut/internal/sidecar/wire"
)

// clientTimeout bounds one request to the broker. It is longer than the
// broker's own sign-in timeout so that the broker's structured error, not a
// bare timeout, is what the caller sees.
const clientTimeout = 20 * time.Second

// Client talks to a token broker over its Unix socket. It holds no secrets
// and caches nothing; the broker caches. It is safe for concurrent use.
type Client struct {
	socket string
	hc     *http.Client
}

// NewClient returns a client for the broker serving on socket.
func NewClient(socket string) *Client {
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	return &Client{
		socket: socket,
		hc: &http.Client{
			Timeout: clientTimeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dialer.DialContext(ctx, "unix", socket)
				},
				MaxIdleConns:        8,
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     time.Minute,
				DisableCompression:  true,
			},
			// The broker never redirects.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// String describes the client by its socket path; it holds nothing else.
func (c *Client) String() string { return "token.Client{" + c.socket + "}" }

// Entra returns an access token for the application described by req.
func (c *Client) Entra(ctx context.Context, req EntraRequest) (token string, expiresAt time.Time, err error) {
	var resp response
	if err := c.post(ctx, "/v1/token", wire.EntraAuth(req), &resp); err != nil {
		return "", time.Time{}, err
	}
	if resp.ExpiresAt != nil {
		expiresAt = *resp.ExpiresAt
	}
	return resp.Token, expiresAt, nil
}

// Slack returns the Slack token ref points at.
func (c *Client) Slack(ctx context.Context, ref secret.Ref) (string, error) {
	var resp response
	if err := c.post(ctx, "/v1/token", wire.SlackAuth(ref), &resp); err != nil {
		return "", err
	}
	return resp.Token, nil
}

// Invalidate tells the broker that the token it last issued for a was
// rejected upstream, so the next request should sign in again. The broker
// honors it at most once every ten seconds per application.
func (c *Client) Invalidate(ctx context.Context, a wire.Auth) error {
	return c.post(ctx, "/v1/invalidate", a, nil)
}

// Healthy reports whether the broker answers.
func (c *Client) Healthy(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://token.broker/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return c.transportError(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return c.failed("the health check answered HTTP " + http.StatusText(resp.StatusCode))
	}
	return nil
}

func (c *Client) post(ctx context.Context, path string, body any, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return c.failed("the request could not be encoded")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://token.broker"+path, bytes.NewReader(data))
	if err != nil {
		return c.failed("the request could not be built")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return c.transportError(ctx, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return c.transportError(ctx, err)
	}
	if resp.StatusCode != http.StatusOK {
		var env wire.ErrorBody
		if json.Unmarshal(raw, &env) != nil || env.Error.Code == "" {
			return c.failed("it answered HTTP " + http.StatusText(resp.StatusCode) + " without an error description")
		}
		e := env.Error
		return &fail.Error{Code: e.Code, What: e.What, Why: e.Why, Next: e.Next}
	}
	if out == nil {
		return nil
	}
	// Never echo the body in an error: it holds the token.
	if err := json.Unmarshal(raw, out); err != nil {
		return c.failed("its answer was not the expected JSON")
	}
	if r, ok := out.(*response); ok && r.Token == "" {
		return c.failed("its answer held no token")
	}
	return nil
}

func (c *Client) failed(why string) error {
	return fail.New(CodeBrokerFailed, "The token broker did not answer properly", why,
		"Check the token broker's logs; the egress proxy and the token broker must be the same version of Taut.")
}

func (c *Client) transportError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fail.Wrap(ctxErr, "token.canceled", "Stopped while waiting for the token broker", "the request was canceled or its deadline passed",
			"Run the command again.")
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return fail.Wrap(err, CodeUnreachable, "The token broker cannot be reached",
			"nothing is accepting connections on "+c.socket,
			"Check that the token broker is running, that it serves on "+c.socket+
				", and that the egress proxy runs as the same user and mounts the same socket directory. Its own logs say why if it failed to start.")
	}
	return fail.Wrap(err, CodeBrokerFailed, "The token broker did not answer",
		"the connection on "+c.socket+" failed or timed out",
		"Check the token broker's logs and restart it if it is stuck.")
}
