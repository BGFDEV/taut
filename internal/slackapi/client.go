// Package slackapi is a minimal Slack Web API client.
//
// It covers only what `taut auth` uses: verifying a token and reading the
// scopes it was granted.
package slackapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bgfdev/taut/internal/fail"
)

// DefaultBaseURL is Slack's Web API root.
const DefaultBaseURL = "https://slack.com/api"

// Client calls the Slack Web API.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New builds a Client. An empty baseURL means Slack's production API.
func New(baseURL, token string, hc *http.Client) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: hc}
}

// Identity is what auth.test reports about a token.
type Identity struct {
	Team         string   `json:"team"`
	TeamID       string   `json:"teamId"`
	User         string   `json:"user"`
	UserID       string   `json:"userId"`
	BotID        string   `json:"botId,omitempty"`
	EnterpriseID string   `json:"enterpriseId,omitempty"`
	URL          string   `json:"url"`
	Scopes       []string `json:"scopes"`
	// Kind is "bot" or "user", derived from the token prefix.
	Kind string `json:"kind"`
}

// TokenKind classifies a Slack token by its prefix without revealing it.
func TokenKind(token string) string {
	switch {
	case strings.HasPrefix(token, "xoxb-"):
		return "bot"
	case strings.HasPrefix(token, "xoxp-"):
		return "user"
	case strings.HasPrefix(token, "xoxe.xoxb-"):
		return "rotating bot"
	case strings.HasPrefix(token, "xoxe.xoxp-"):
		return "rotating user"
	case strings.HasPrefix(token, "xoxe"):
		return "rotating"
	case strings.HasPrefix(token, "xapp-"):
		return "app-level"
	}
	return "unknown"
}

// AuthTest verifies the token and returns its identity and granted scopes.
func (c *Client) AuthTest(ctx context.Context) (*Identity, error) {
	if kind := TokenKind(c.token); kind == "app-level" {
		return nil, fail.New("slack.wrong_token_type",
			"That Slack token is an app-level token",
			"App-level tokens (xapp-) cannot read conversations.",
			"Use the Bot User OAuth Token (xoxb-) from your app's OAuth & Permissions page.").WithExit(fail.ExitPermission)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/auth.test", nil)
	if err != nil {
		return nil, fail.Wrap(err, "slack.bad_url", "The Slack API URL is not valid", "", "Check source.slack.api.baseURL in the config file.")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, networkError(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		wait := resp.Header.Get("Retry-After")
		if _, convErr := strconv.Atoi(wait); convErr != nil {
			wait = "60"
		}
		return nil, fail.New("slack.rate_limited", "Slack is rate limiting this app",
			"auth.test returned 429.", "Wait "+wait+" seconds and run `taut auth` again.")
	}
	if resp.StatusCode >= 500 {
		return nil, fail.New("slack.unavailable", "Slack's API is unavailable",
			"auth.test returned HTTP "+strconv.Itoa(resp.StatusCode)+".",
			"Check https://slack-status.com and retry.")
	}

	var body struct {
		OK           bool   `json:"ok"`
		Error        string `json:"error"`
		URL          string `json:"url"`
		Team         string `json:"team"`
		User         string `json:"user"`
		TeamID       string `json:"team_id"`
		UserID       string `json:"user_id"`
		BotID        string `json:"bot_id"`
		EnterpriseID string `json:"enterprise_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fail.New("slack.bad_response", "Slack returned a response Taut could not read",
			"auth.test did not return JSON (HTTP "+strconv.Itoa(resp.StatusCode)+").",
			"Check that source.slack.api.baseURL points at the Slack API, then retry.")
	}
	if !body.OK {
		return nil, authError(body.Error)
	}

	id := &Identity{
		Team: body.Team, TeamID: body.TeamID, User: body.User, UserID: body.UserID,
		BotID: body.BotID, EnterpriseID: body.EnterpriseID, URL: body.URL,
		Kind: TokenKind(c.token),
	}
	for _, s := range strings.Split(resp.Header.Get("X-OAuth-Scopes"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			id.Scopes = append(id.Scopes, s)
		}
	}
	slices.Sort(id.Scopes)
	return id, nil
}

func networkError(err error) error {
	// url.Error embeds the request URL but never headers, so the token
	// cannot leak through it; still, report only the operation and cause.
	var ue *url.Error
	why := err.Error()
	if errors.As(err, &ue) {
		why = ue.Err.Error()
	}
	return fail.Wrap(err, "slack.unreachable", "Could not reach Slack", why,
		"Check your network and proxy settings (`taut doctor`), then retry.")
}

func authError(code string) error {
	const page = "your app's OAuth & Permissions page (https://api.slack.com/apps)"
	switch code {
	case "invalid_auth", "not_authed":
		return fail.New("slack.invalid_token", "Slack rejected the token",
			"The token is malformed or does not belong to any workspace ("+code+").",
			"Copy the Bot User OAuth Token again from "+page+" and update the secret the config file references.").WithExit(fail.ExitPermission)
	case "token_revoked", "token_expired", "account_inactive":
		return fail.New("slack.token_revoked", "The Slack token is no longer valid",
			"Slack reports "+code+".",
			"Reinstall the app to the workspace from "+page+" and use the new token.").WithExit(fail.ExitPermission)
	case "missing_scope":
		return fail.New("slack.missing_scope", "The Slack token is missing a required scope", "",
			"Run `taut auth --slack-manifest` and update the app with the scopes it lists.").WithExit(fail.ExitPermission)
	}
	return fail.New("slack.error", "Slack returned an error", "auth.test failed with \""+code+"\".",
		"Look the code up at https://docs.slack.dev/reference/methods/auth.test and retry.")
}
