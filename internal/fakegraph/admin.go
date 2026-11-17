package fakegraph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// StateSnapshot is a secret-free summary of the server, served at
// GET /_fake/state.
type StateSnapshot struct {
	TenantID     string        `json:"tenantId"`
	Organization Organization  `json:"organization"`
	Apps         []AppSnapshot `json:"apps"`
	Users        []User        `json:"users"`
	Teams        []Team        `json:"teams"`
	Faults       []FaultStatus `json:"faults"`
	RateLimit    RateLimit     `json:"rateLimit"`
	// TokenLifetimeSeconds is how long issued tokens live.
	TokenLifetimeSeconds int `json:"tokenLifetimeSeconds"`
	// RequestCount is the number of entries in the request log.
	RequestCount int `json:"requestCount"`
	// Stats are the import, conflict and throttle counters.
	Stats Stats `json:"stats"`
	// Content is every team with its provisioning state, members, channels
	// (with migration state), their uploaded files and all their messages. Message bodies are
	// customer data in a real tenant; here they are whatever was imported.
	Content []TeamContent `json:"content"`
	// Chats is every chat with its members and messages.
	Chats []ChatContent `json:"chats"`
}

// AppSnapshot describes a registered app without its secrets.
type AppSnapshot struct {
	ClientID    string   `json:"clientId"`
	DisplayName string   `json:"displayName,omitempty"`
	Roles       []string `json:"roles"`
	// SecretCount and CertificateCount say which credentials are accepted.
	SecretCount      int `json:"secretCount"`
	CertificateCount int `json:"certificateCount"`
}

// RateLimit is the per-app-per-tenant token bucket configuration.
type RateLimit struct {
	RPS   float64 `json:"rps"`
	Burst int     `json:"burst"`
}

// Snapshot returns the current state without any secret material.
func (s *Server) Snapshot() StateSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap := StateSnapshot{
		TenantID:             s.st.tenantID,
		Organization:         s.st.org,
		Apps:                 []AppSnapshot{},
		Users:                []User{},
		Teams:                []Team{},
		Faults:               s.faults.List(),
		RateLimit:            RateLimit{RPS: s.cfg.rps, Burst: s.cfg.burst},
		TokenLifetimeSeconds: int(s.cfg.tokenLifetime.Seconds()),
		RequestCount:         len(s.Requests()),
		Stats:                s.Stats(),
		Content:              s.content(s.cfg.now()),
		Chats:                s.chatContent(),
	}
	for _, key := range s.st.appOrder {
		a := s.st.apps[key]
		snap.Apps = append(snap.Apps, AppSnapshot{
			ClientID: a.ClientID, DisplayName: a.DisplayName, Roles: append([]string{}, a.Roles...),
			SecretCount: len(a.Secrets), CertificateCount: len(a.certs),
		})
	}
	for _, u := range s.st.users {
		snap.Users = append(snap.Users, *u)
	}
	for _, t := range s.st.teams {
		team := t.Team
		team.Channels = make([]Channel, 0, len(t.channels))
		for _, ch := range t.channels {
			team.Channels = append(team.Channels, ch.Channel)
		}
		snap.Teams = append(snap.Teams, team)
	}
	return snap
}

// adminRoutes registers the control endpoints. They are unauthenticated,
// unthrottled, immune to faults and absent from the request log.
func (s *Server) adminRoutes() {
	a := http.NewServeMux()
	a.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	a.HandleFunc("GET /_fake/state", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.Snapshot())
	})
	a.HandleFunc("GET /_fake/summary", s.summaryHandler)
	a.HandleFunc("GET /_fake/requests", func(w http.ResponseWriter, _ *http.Request) {
		reqs := s.Requests()
		if reqs == nil {
			reqs = []RequestRecord{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"requests": reqs})
	})
	a.HandleFunc("DELETE /_fake/requests", func(w http.ResponseWriter, _ *http.Request) {
		s.ResetRequests()
		w.WriteHeader(http.StatusNoContent)
	})
	a.HandleFunc("GET /_fake/faults", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"faults": s.faults.List()})
	})
	a.HandleFunc("POST /_fake/faults", s.postFaults)
	a.HandleFunc("DELETE /_fake/faults", func(w http.ResponseWriter, r *http.Request) {
		if id := r.URL.Query().Get("id"); id != "" {
			if !s.faults.Remove(id) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "no fault with id " + id})
				return
			}
		} else {
			s.faults.Clear()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	s.admin = a
}

// postFaults accepts one Fault object or an array of them. Either every
// rule is installed or none is.
func (s *Server) postFaults(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body too large or unreadable"})
		return
	}
	var rules []Fault
	strict := func(v any) error {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		return dec.Decode(v)
	}
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte("[")) {
		err = strict(&rules)
	} else {
		var one Fault
		err = strict(&one)
		rules = []Fault{one}
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid fault JSON: " + err.Error()})
		return
	}
	var added []string
	for _, rule := range rules {
		id, err := s.faults.Add(rule)
		if err != nil {
			for _, a := range added {
				s.faults.Remove(a)
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		added = append(added, id)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ids": added, "faults": s.faults.List()})
}

// Serve runs a standalone fake on addr (for example "127.0.0.1:8089")
// until ctx ends, then shuts down gracefully. The control endpoints are
// unauthenticated, so bind to loopback or a private network.
func Serve(ctx context.Context, addr string, seed Seed, opts ...Option) error {
	if err := seed.Validate(); err != nil {
		return fmt.Errorf("fakegraph: invalid seed: %w", err)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("fakegraph: listen on %s: %w", addr, err)
	}
	return ServeListener(ctx, ln, seed, opts...)
}

// ServeListener is Serve on an existing listener, which it closes on return.
func ServeListener(ctx context.Context, ln net.Listener, seed Seed, opts ...Option) error {
	if err := seed.Validate(); err != nil {
		_ = ln.Close()
		return fmt.Errorf("fakegraph: invalid seed: %w", err)
	}
	srv := &http.Server{
		Handler:           New(seed, opts...),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		return fmt.Errorf("fakegraph: serve: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
		return fmt.Errorf("fakegraph: shutdown: %w", err)
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("fakegraph: serve: %w", err)
	}
	return nil
}
