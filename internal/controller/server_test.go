package controller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testServer(reachable map[string]bool) *Server {
	s := New(Config{
		PostgresURL: "postgres://taut:hunter2@db.internal:5432/taut",
		NATSURL:     "nats://nats.internal",
		S3Endpoint:  "http://staging.internal:9000",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), Services{})
	s.dial = func(_ context.Context, _, addr string) (net.Conn, error) {
		if reachable[addr] {
			c, _ := net.Pipe()
			return c, nil
		}
		return nil, errors.New("connection refused")
	}
	return s
}

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

func TestHealthAndVersion(t *testing.T) {
	h := testServer(nil).Handler()
	if code, _ := get(t, h, "/healthz"); code != 200 {
		t.Fatalf("healthz = %d", code)
	}
	code, body := get(t, h, "/v1/version")
	if code != 200 || !strings.Contains(body, `"service":"controller"`) {
		t.Fatalf("version = %d %s", code, body)
	}
}

func TestReadiness(t *testing.T) {
	all := map[string]bool{"db.internal:5432": true, "nats.internal:4222": true, "staging.internal:9000": true}
	if code, body := get(t, testServer(all).Handler(), "/readyz"); code != 200 {
		t.Fatalf("readyz with all deps up = %d %s", code, body)
	}

	delete(all, "nats.internal:4222")
	code, body := get(t, testServer(all).Handler(), "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with nats down = %d", code)
	}
	var resp struct {
		Dependencies []Dependency `json:"dependencies"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	for _, d := range resp.Dependencies {
		if d.Name == "nats" && d.OK {
			t.Fatal("nats reported ok while unreachable")
		}
	}
	// The Postgres password must never appear in a health response.
	if strings.Contains(body, "hunter2") {
		t.Fatalf("readyz leaked credentials: %s", body)
	}
}
