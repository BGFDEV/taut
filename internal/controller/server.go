// Package controller is Taut's control plane.
//
// The controller owns the API the CLI talks to: it creates runs, receives the Slack
// export and encrypts it into staging, reports progress, and keeps run
// statuses in step with the ledger. It never talks to Slack or Microsoft
// itself; that is the workers' work. Scheduling needs no component of its
// own: which lane may run next is decided by the claim query in the store
// (see internal/store), so the controller can restart at any time without losing or
// repeating work.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/bgfdev/taut/internal/version"
)

// Config is the controller's runtime configuration, read from the environment by the
// `taut controller` command.
type Config struct {
	Addr string // listen address, e.g. ":7070"
	// Dependency URLs. Empty means "not configured" and is skipped by the
	// readiness check.
	PostgresURL string
	NATSURL     string
	S3Endpoint  string
}

// Dependency is the reachability of one backing service.
type Dependency struct {
	Name   string `json:"name"`
	Target string `json:"target"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
}

// Server is the controller HTTP server.
type Server struct {
	cfg  Config
	log  *slog.Logger
	svc  Services
	dial func(ctx context.Context, network, addr string) (net.Conn, error)

	// throttles are the latest throttle events heard from the egress proxies,
	// newest first.
	mu        sync.Mutex
	throttles []ThrottleView
	// finalizing serializes finalize requests: each reads the run's lanes and
	// channels and then adds to them.
	finalizing sync.Mutex
}

// New builds a Server. Pass a zero Services to serve only health and
// version.
func New(cfg Config, log *slog.Logger, svc Services) *Server {
	d := &net.Dialer{Timeout: 2 * time.Second}
	return &Server{cfg: cfg, log: log, svc: svc, dial: d.DialContext}
}

// hostPort extracts host:port from a URL, never returning credentials.
func hostPort(raw, defaultPort string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", errors.New("not a valid URL")
	}
	if u.Port() == "" {
		// A web endpoint without a port means its scheme's port.
		switch u.Scheme {
		case "https":
			defaultPort = "443"
		case "http":
			defaultPort = "80"
		}
		return net.JoinHostPort(u.Hostname(), defaultPort), nil
	}
	return u.Host, nil
}

// Dependencies probes each configured backing service with a TCP connect.
// This is a reachability check only.
func (s *Server) Dependencies(ctx context.Context) []Dependency {
	probes := []struct{ name, raw, port string }{
		{"postgres", s.cfg.PostgresURL, "5432"},
		{"nats", s.cfg.NATSURL, "4222"},
		{"staging", s.cfg.S3Endpoint, "9000"},
	}
	var out []Dependency
	for _, p := range probes {
		if p.raw == "" {
			continue
		}
		d := Dependency{Name: p.name}
		addr, err := hostPort(p.raw, p.port)
		if err != nil {
			d.Error = err.Error()
			out = append(out, d)
			continue
		}
		d.Target = addr
		conn, err := s.dial(ctx, "tcp", addr)
		if err != nil {
			d.Error = "unreachable"
		} else {
			_ = conn.Close()
			d.OK = true
		}
		out = append(out, d)
	}
	return out
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		deps := s.Dependencies(r.Context())
		code, status := http.StatusOK, "ready"
		for _, d := range deps {
			if !d.OK {
				code, status = http.StatusServiceUnavailable, "not_ready"
			}
		}
		writeJSON(w, code, map[string]any{"status": status, "dependencies": deps})
	})
	mux.HandleFunc("GET /v1/version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"service": "controller", "version": version.Version, "commit": version.Commit,
		})
	})
	s.routes(mux)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Serve runs until ctx is cancelled, then shuts down gracefully.
func (s *Server) Serve(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	go s.reconcileLoop(ctx, 2*time.Second)
	go s.listenThrottles(ctx)
	s.log.Info("controller listening", "addr", s.cfg.Addr, "version", version.Version)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		s.log.Info("controller shutting down")
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return nil
	}
}

// handler is Handler with the configured middleware applied.
func (s *Server) handler() http.Handler {
	if s.svc.Middleware != nil {
		return s.svc.Middleware(s.Handler())
	}
	return s.Handler()
}
