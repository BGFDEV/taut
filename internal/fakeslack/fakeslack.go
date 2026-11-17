// Package fakeslack is a tiny in-memory fake of the Slack Web API, enough
// for `taut auth` to be exercised without a real workspace.
package fakeslack

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
)

// Token is one accepted token and what it is allowed to do.
type Token struct {
	Scopes  []string
	Revoked bool
}

// Server is the fake. The zero value is not usable; call New.
type Server struct {
	mu     sync.Mutex
	tokens map[string]Token
	team   string
	// Throttle, when positive, makes the next N requests return 429.
	throttle int
	calls    int
}

// New returns a fake workspace named team that accepts the given tokens.
func New(team string, tokens map[string]Token) *Server {
	return &Server{team: team, tokens: tokens}
}

// ThrottleNext makes the next n requests fail with 429.
func (s *Server) ThrottleNext(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.throttle = n
}

// Calls returns how many requests were served.
func (s *Server) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// ServeHTTP implements http.Handler. Mount it at the API root, e.g.
// http.StripPrefix("/slack/api", server).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.calls++
	throttled := s.throttle > 0
	if throttled {
		s.throttle--
	}
	s.mu.Unlock()

	if throttled {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	if strings.TrimPrefix(r.URL.Path, "/") != "auth.test" {
		reply(w, nil, map[string]any{"ok": false, "error": "unknown_method"})
		return
	}

	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	tok, ok := s.tokens[bearer]
	switch {
	case bearer == "":
		reply(w, nil, map[string]any{"ok": false, "error": "not_authed"})
	case !ok:
		reply(w, nil, map[string]any{"ok": false, "error": "invalid_auth"})
	case tok.Revoked:
		reply(w, nil, map[string]any{"ok": false, "error": "token_revoked"})
	default:
		reply(w, tok.Scopes, map[string]any{
			"ok": true, "url": "https://" + s.team + ".slack.com/", "team": s.team,
			"user": "taut_migration", "team_id": "T0FIXTURE", "user_id": "U0TAUTBOT", "bot_id": "B0TAUTBOT",
		})
	}
}

func reply(w http.ResponseWriter, scopes []string, body map[string]any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if scopes != nil {
		w.Header().Set("X-OAuth-Scopes", strings.Join(scopes, ","))
	}
	_ = json.NewEncoder(w).Encode(body)
}
