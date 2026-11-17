package worker

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/graph"
	"github.com/bgfdev/taut/internal/sidecar/wire"
)

// Proxied routes a worker's traffic to Microsoft and Slack through the
// egress proxy sidecar on localhost.
//
// With it, the worker holds no credentials at all. It names where a
// credential lives (the config file's references) in a header; the egress proxy
// asks the token broker, which is the only process that can read the
// secrets, and adds the bearer token itself. The worker never sees one.
type Proxied struct {
	// URL is the egress proxy's address, e.g. http://127.0.0.1:8900.
	URL string
	// Authority and GraphBase override the config file's endpoints (the local
	// stack points them at the fake servers).
	Authority string
	GraphBase string
	// SlackFilesBase, when set, replaces "https://files.slack.com" in the
	// export's file URLs (the local stack points it at the fake file host).
	SlackFilesBase string
	// Client performs requests to the egress proxy. Nil means a client with no
	// overall timeout, as large file transfers need.
	Client *http.Client
	// Retry configures the Graph client's retries. The zero value uses the
	// client's defaults.
	Retry graph.RetryPolicy
	// Sleep, when set, replaces the Graph client's sleeping (tests).
	Sleep func(ctx context.Context, d time.Duration) error
}

// origin returns scheme://host[:port] of a URL.
func origin(raw string) (string, *url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", nil, fail.New("route.bad_url", "A URL the worker was asked to call is not valid", "", "Check the endpoints in the config file.")
	}
	return u.Scheme + "://" + u.Host, u, nil
}

// via returns the egress proxy URL that stands for an upstream URL.
func (g Proxied) via(u *url.URL) string {
	target := strings.TrimRight(g.URL, "/") + u.EscapedPath()
	if u.RawQuery != "" {
		target += "?" + u.RawQuery
	}
	return target
}

// entra describes the run's Entra application for the token broker.
func (g Proxied) entra(m *config.Config) (wire.Entra, string, error) {
	e := m.Target.Entra
	cloud, err := graph.LookupCloud(e.Cloud)
	if err != nil {
		return wire.Entra{}, "", err
	}
	authority, base := firstNonEmpty(g.Authority, e.Authority, cloud.Authority), firstNonEmpty(g.GraphBase, e.GraphBaseURL, cloud.GraphBase)
	return wire.Entra{
		TenantID: e.TenantID, ClientID: e.ClientID, Cloud: e.Cloud, Authority: authority, GraphBase: base,
		Credential: wire.Credential{Certificate: e.Credential.Certificate, ClientSecret: e.Credential.ClientSecret},
	}, base, nil
}

// noToken is the token source of a proxied Graph client: the egress proxy adds
// the Authorization header, so the client sends none.
type noToken struct{}

func (noToken) Token(context.Context) (string, error) { return "", nil }

// GraphFactory returns a factory for Graph clients that go through the
// egress proxy.
func (g Proxied) GraphFactory() GraphFactory {
	return func(_ context.Context, m *config.Config) (Graph, error) {
		desc, base, err := g.entra(m)
		if err != nil {
			return nil, err
		}
		baseOrigin, _, err := origin(base)
		if err != nil {
			return nil, err
		}
		auth := wire.EntraAuth(desc)
		// The egress proxy returns redirects unfollowed. Following one from
		// here would be a direct outbound call, which a worker does not
		// make.
		hc := &http.Client{}
		if g.Client != nil {
			c := *g.Client
			hc = &c
		}
		hc.CheckRedirect = noRedirects
		opts := graph.Options{
			BaseURL: g.URL, TokenSource: noToken{}, HTTPClient: hc, Retry: g.Retry,
			// Every ordinary Graph request: forward to the Graph origin,
			// authenticated as the run's application.
			Header: wire.Headers(baseOrigin, auth),
			// Absolute URLs Graph hands back. An upload session URL is
			// pre-authorized and must not carry a token; a next-page link
			// on the Graph host is an ordinary authenticated request.
			RewriteURL: func(absolute string) (string, http.Header) {
				o, u, err := origin(absolute)
				if err != nil {
					return absolute, nil
				}
				// Only a Graph API URL (a next-page link) gets the token.
				// Anything else Graph hands back is an upload session URL,
				// which is pre-authorized: Graph rejects a bearer token on
				// it, and the token must not be sent to a storage host.
				a := wire.NoAuth()
				if o == baseOrigin && isGraphAPIPath(u.Path) {
					a = auth
				}
				return g.via(u), wire.Headers(o, a)
			},
		}
		if g.Sleep != nil {
			opts.Sleep = g.Sleep
		}
		return graph.New(opts)
	}
}

// SlackFiles returns file access that goes through the egress proxy. An export
// URL that carries its own token is fetched as it is; otherwise the egress proxy
// adds the Slack token the config file references.
func (g Proxied) SlackFiles() SlackFiles {
	return HTTPSlackFiles{
		Client: g.Client,
		Rewrite: func(mf *config.Config, raw string) (string, http.Header, error) {
			if g.SlackFilesBase != "" {
				raw = strings.Replace(raw, "https://files.slack.com", strings.TrimRight(g.SlackFilesBase, "/"), 1)
			}
			o, u, err := origin(raw)
			if err != nil {
				return "", nil, &fileGone{"bad_url", "the export's link for the file is not a valid URL"}
			}
			auth := wire.NoAuth()
			if u.Query().Get("t") == "" {
				if mf != nil && mf.Source.Slack.API.Enabled && !mf.Source.Slack.API.Token.IsZero() {
					auth = wire.SlackAuth(mf.Source.Slack.API.Token)
				}
			}
			return g.via(u), wire.Headers(o, auth), nil
		},
	}
}

// isGraphAPIPath reports whether a path is a versioned Microsoft Graph API
// path.
func isGraphAPIPath(p string) bool {
	return strings.HasPrefix(p, "/v1.0/") || strings.HasPrefix(p, "/beta/")
}
