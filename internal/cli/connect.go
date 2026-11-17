package cli

import (
	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/graph"
	"github.com/bgfdev/taut/internal/secret"
	"github.com/bgfdev/taut/internal/slackapi"
	"github.com/bgfdev/taut/internal/version"
)

// loadConfig reads and validates the config file named by --config.
func (a *App) loadConfig() (*config.Config, error) {
	m, err := config.Load(a.ConfigPath)
	if err != nil {
		return nil, err
	}
	if issues := m.Validate(); issues.HasErrors() {
		return nil, issues.Err()
	}
	return m, nil
}

// entraCredential loads the credential the config file references.
func entraCredential(m *config.Config) (graph.Credential, []secret.Warning, error) {
	c := m.Target.Entra.Credential
	if !c.Certificate.IsZero() {
		if c.Certificate.File == "" {
			// A certificate delivered through an environment variable.
			v, warns, err := m.ResolveSecret(c.Certificate)
			if err != nil {
				return nil, nil, err
			}
			cert, err := graph.ParseClientCertificate([]byte(v.Reveal()))
			return cert, warns, err
		}
		cert, err := graph.LoadClientCertificate(m.ResolvePath(c.Certificate.File))
		if err != nil {
			return nil, nil, err
		}
		return cert, nil, nil
	}
	v, warns, err := m.ResolveSecret(c.ClientSecret)
	if err != nil {
		return nil, nil, err
	}
	cred, err := graph.NewClientSecret(v.Reveal())
	if err != nil {
		return nil, nil, err
	}
	return cred, warns, nil
}

// graphSession is an authenticated connection to Microsoft Graph.
type graphSession struct {
	Client *graph.Client
	Tokens *graph.CachingTokenSource
	Cloud  graph.Cloud
}

// connectGraph builds a Graph client for the config file's tenant. It does not
// make a network call; the first request (or Tokens.Token) signs in.
func connectGraph(m *config.Config, cred graph.Credential) (*graphSession, error) {
	e := m.Target.Entra
	cloud, err := graph.LookupCloud(e.Cloud)
	if err != nil {
		return nil, err
	}
	if e.Authority != "" {
		cloud.Authority = e.Authority
	}
	if e.GraphBaseURL != "" {
		cloud.GraphBase = e.GraphBaseURL
	}
	tokens, err := graph.NewTokenSource(graph.CredentialsOptions{
		TenantID: e.TenantID, ClientID: e.ClientID, Credential: cred,
		Cloud: e.Cloud, Authority: cloud.Authority, GraphBase: cloud.GraphBase,
		UserAgent: userAgent(),
	})
	if err != nil {
		return nil, err
	}
	client, err := graph.New(graph.Options{BaseURL: cloud.GraphBase, TokenSource: tokens, UserAgent: userAgent()})
	if err != nil {
		return nil, err
	}
	return &graphSession{Client: client, Tokens: tokens, Cloud: cloud}, nil
}

// slackClient builds a Slack API client from the config file's token reference.
func slackClient(m *config.Config) (*slackapi.Client, []secret.Warning, error) {
	api := m.Source.Slack.API
	if !api.Enabled {
		return nil, nil, fail.New("slack.api_disabled", "The Slack API is turned off in the config file", "",
			"Set source.slack.api.enabled to true and reference a token.")
	}
	tok, warns, err := m.ResolveSecret(api.Token)
	if err != nil {
		return nil, nil, err
	}
	return slackapi.New(api.BaseURL, tok.Reveal(), nil), warns, nil
}

// features reports which optional capabilities the config file asks for; they
// decide which permissions and scopes are required.
func features(m *config.Config) (private, dms bool) {
	for _, t := range m.Target.Teams {
		for _, k := range t.Channels.Types {
			if k == config.ChannelPrivate {
				private = true
			}
		}
	}
	return private, m.Target.DirectMessages.Enabled
}

func userAgent() string { return "taut/" + version.Version }
