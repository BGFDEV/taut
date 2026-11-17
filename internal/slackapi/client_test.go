package slackapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/fakeslack"
	"github.com/bgfdev/taut/internal/slackapi"
)

const (
	goodToken    = "xoxb-FAKE-0000-good"
	revokedToken = "xoxb-FAKE-0000-revoked"
)

func server(t *testing.T) (*fakeslack.Server, string) {
	t.Helper()
	fs := fakeslack.New("acme", map[string]fakeslack.Token{
		goodToken:    {Scopes: []string{"users:read", "channels:read", "channels:history"}},
		revokedToken: {Revoked: true},
	})
	ts := httptest.NewServer(http.StripPrefix("/api", fs))
	t.Cleanup(ts.Close)
	return fs, ts.URL + "/api"
}

func TestAuthTest(t *testing.T) {
	_, url := server(t)
	id, err := slackapi.New(url, goodToken, nil).AuthTest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id.Team != "acme" || id.Kind != "bot" || len(id.Scopes) != 3 || id.Scopes[0] != "channels:history" {
		t.Fatalf("unexpected identity: %+v", id)
	}
}

func TestAuthErrorsAreStructuredAndNeverLeakTheToken(t *testing.T) {
	fs, url := server(t)
	cases := []struct {
		name, token, code string
		exit              int
		setup             func()
	}{
		{"unknown token", "xoxb-FAKE-0000-nope", "slack.invalid_token", fail.ExitPermission, nil},
		{"revoked", revokedToken, "slack.token_revoked", fail.ExitPermission, nil},
		{"app-level token", "xapp-FAKE-1-A0", "slack.wrong_token_type", fail.ExitPermission, nil},
		{"throttled", goodToken, "slack.rate_limited", fail.ExitFailure, func() { fs.ThrottleNext(1) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.setup != nil {
				c.setup()
			}
			_, err := slackapi.New(url, c.token, nil).AuthTest(context.Background())
			var fe *fail.Error
			if !errors.As(err, &fe) {
				t.Fatalf("want *fail.Error, got %v", err)
			}
			if fe.Code != c.code || fail.ExitCode(err) != c.exit {
				t.Fatalf("got code %q exit %d, want %q exit %d", fe.Code, fail.ExitCode(err), c.code, c.exit)
			}
			if fe.Next == "" {
				t.Fatal("error has no next step")
			}
			if all := fe.What + fe.Why + fe.Next + fe.Error(); strings.Contains(all, c.token) {
				t.Fatalf("error text leaks the token: %s", all)
			}
		})
	}
}

func TestUnreachableDoesNotLeakToken(t *testing.T) {
	_, err := slackapi.New("http://127.0.0.1:1/api", goodToken, nil).AuthTest(context.Background())
	var fe *fail.Error
	if !errors.As(err, &fe) || fe.Code != "slack.unreachable" {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(fe.Error()+fe.Next, goodToken) {
		t.Fatal("token leaked")
	}
}

func TestCompareAndManifest(t *testing.T) {
	st := slackapi.Compare([]string{"users:read", "groups:read"}, slackapi.Features{})
	var missing, optionalGranted int
	for _, s := range st {
		if s.Required && !s.Granted {
			missing++
		}
		if !s.Required && s.Granted {
			optionalGranted++
		}
	}
	if missing != len(slackapi.Required(slackapi.Features{}))-1 || optionalGranted != 1 {
		t.Fatalf("missing=%d optionalGranted=%d", missing, optionalGranted)
	}

	m := slackapi.AppManifest(slackapi.Features{PrivateChannels: true})
	for _, want := range []string{"users:read.email", "groups:history", "channels:join"} {
		if !strings.Contains(m, "- "+want+"\n") {
			t.Errorf("app manifest is missing scope %s", want)
		}
	}
	if strings.Contains(m, "im:history") {
		t.Error("app manifest requests DM scopes that were not asked for")
	}
	// Direct messages are read from the export, never through the Slack
	// API, so migrating them asks Slack for nothing more.
	withDMs := slackapi.Features{PrivateChannels: true, DirectMessages: true}
	if dm := slackapi.AppManifest(withDMs); dm != m || strings.Contains(dm, "im:") || strings.Contains(dm, "mpim:") {
		t.Errorf("migrating direct messages changes the app manifest:\n%s", dm)
	}
	if len(slackapi.Required(withDMs)) != len(slackapi.Required(slackapi.Features{PrivateChannels: true})) {
		t.Error("migrating direct messages requires more Slack scopes")
	}
	if strings.Contains(m, ":write") {
		t.Error("app manifest must stay read-only")
	}
}
