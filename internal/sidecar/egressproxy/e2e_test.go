package egressproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bgfdev/taut/internal/secret"
	"github.com/bgfdev/taut/internal/sidecar/budget"
	"github.com/bgfdev/taut/internal/sidecar/tokenbroker"
	"github.com/bgfdev/taut/internal/sidecar/wire"
)

// TestWithRealTokenBroker wires worker → egress proxy → token broker (over a
// real Unix socket) → upstream, the way a pod does.
func TestWithRealTokenBroker(t *testing.T) {
	const slackToken = "xoxb-123456789012-" + canarySlack
	t.Setenv("TAUT_SLACK_E2E_TOKEN", slackToken)

	dir, err := os.MkdirTemp("", "tg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "run", "token.sock")
	srv, err := tokenbroker.Listen(tokenbroker.Config{Socket: socket})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		_, _ = io.WriteString(w, r.Header.Get("Authorization"))
	})
	r := newHarness(t, func(c *Config) {
		c.Tokens = tokenbroker.NewClient(socket)
		c.Budgets = budget.Static(nil)
	}, "slack=http://"+up.Listener.Addr().String())
	front := httptest.NewServer(r.g.Handler())
	defer front.Close()

	send := func(ref secret.Ref) (int, string, string) {
		req, _ := http.NewRequest("GET", front.URL+"/api/conversations.history?channel=C1", nil)
		req.Header = wire.Headers(up.URL, wire.SlackAuth(ref))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get(wire.HeaderError), string(body)
	}

	if status, _, body := send(secret.Ref{Env: "TAUT_SLACK_E2E_TOKEN"}); status != 200 || body != "Bearer "+slackToken {
		t.Errorf("status %d, upstream saw Authorization %q", status, body)
	}
	if ts := r.g.State().Tenants; len(ts) != 1 || ts[0].Scope != ScopeSlack || ts[0].RateRPS != 1 {
		t.Errorf("state = %+v", ts)
	}

	// The sidecar's refusals reach the worker as structured errors.
	status, code, body := send(secret.Ref{Env: "PATH"})
	if status != http.StatusBadGateway || code != tokenbroker.CodeRefNotAllowed || !strings.Contains(body, `"next"`) {
		t.Errorf("refused reference: %d %s %s", status, code, body)
	}
	if up.count() != 1 {
		t.Errorf("upstream saw %d requests, want 1", up.count())
	}

	// And so does a sidecar that has gone away.
	cancel()
	<-done
	done <- nil
	if status, code, _ := send(secret.Ref{Env: "TAUT_SLACK_E2E_TOKEN"}); status != http.StatusBadGateway || code != tokenbroker.CodeUnreachable {
		t.Errorf("sidecar down: %d %s", status, code)
	}
}
