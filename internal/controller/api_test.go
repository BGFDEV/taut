package controller_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bgfdev/taut/internal/bus"
	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/secret"
	"github.com/bgfdev/taut/internal/staging"
	"github.com/bgfdev/taut/internal/store/storetest"
)

func newAPI(t *testing.T, mod func(*controller.Services)) *controller.Client {
	t.Helper()
	st := storetest.Open(t)
	blobs, err := staging.NewFS(filepath.Join(t.TempDir(), "staging"))
	if err != nil {
		t.Fatal(err)
	}
	k, _ := staging.GenerateKEK()
	kek, err := staging.ParseKEK(secret.New(k))
	if err != nil {
		t.Fatal(err)
	}
	svc := controller.Services{Store: st, Blobs: blobs, KEK: kek, Bus: bus.NewLocal(), Token: "the-api-token"}
	if mod != nil {
		mod(&svc)
	}
	srv := httptest.NewServer(controller.New(controller.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), svc).Handler())
	t.Cleanup(srv.Close)
	t.Setenv("CONTROLLER_TEST_URL", srv.URL)
	return controller.NewClient(srv.URL, "the-api-token")
}

func configYAML(t *testing.T, mod func(*config.Config)) []byte {
	t.Helper()
	m := config.Default()
	m.Metadata.Name = "api-test"
	m.Source.Slack.Export = "./export.zip"
	m.Target.Entra.TenantID = "7f3c1e9a-52d4-4b1e-9a57-0c8f2d6e4b10"
	m.Target.Entra.ClientID = "2b9d4c1f-8e3a-4f6b-b7c2-5a1e0d9f3c84"
	m.Target.Entra.Credential = config.Credential{Certificate: secret.Ref{File: "./taut-app.pem"}}
	team := config.DefaultTeam()
	team.Name, team.Owner = "Acme", "admin@acme.com"
	m.Target.Teams = []config.TeamSpec{team}
	m.Identity.Unmapped.Account = "former@acme.com"
	if mod != nil {
		mod(m)
	}
	y, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return y
}

func code(err error) string {
	var fe *fail.Error
	if errors.As(err, &fe) {
		return fe.Code
	}
	return ""
}

// A config file must not be able to say where sign-ins and requests are sent.
func TestConfigEndpointOverridesAreRefused(t *testing.T) {
	c := newAPI(t, nil)
	ctx := context.Background()
	for name, mod := range map[string]func(*config.Config){
		"authority":  func(m *config.Config) { m.Target.Entra.Authority = "https://evil.example" },
		"graph base": func(m *config.Config) { m.Target.Entra.GraphBaseURL = "https://evil.example" },
	} {
		_, err := c.CreateRun(ctx, configYAML(t, mod), false)
		if code(err) != "config.endpoint_override" {
			t.Errorf("%s override: err = %v, want config.endpoint_override", name, err)
		}
	}
	if _, err := c.CreateRun(ctx, configYAML(t, nil), false); err != nil {
		t.Fatalf("a plain config file was refused: %v", err)
	}

	// Explicitly enabled (a local stack with fake servers), they pass.
	allow := newAPI(t, func(s *controller.Services) { s.AllowTestEndpoints = true })
	if _, err := allow.CreateRun(ctx, configYAML(t, func(m *config.Config) { m.Target.Entra.Authority = "http://fakegraph:8080" }), false); err != nil {
		t.Fatalf("override refused although enabled: %v", err)
	}
}

func TestAPIRequiresItsToken(t *testing.T) {
	newAPI(t, nil)
	url := strings.TrimRight(lookup(t, "CONTROLLER_TEST_URL"), "/")
	for _, tok := range []string{"", "wrong-token"} {
		_, err := controller.NewClient(url, tok).LatestRun(context.Background(), "api-test")
		if code(err) != "controller.unauthorized" || fail.ExitCode(err) != fail.ExitPermission {
			t.Errorf("token %q: err = %v, want controller.unauthorized", tok, err)
		}
	}
	// Health stays open so orchestrators can probe it.
	resp, err := http.Get(url + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz: %v %v", resp, err)
	}
	resp.Body.Close()
}

// An oversized export is refused rather than read without bound.
func TestExportUploadIsBounded(t *testing.T) {
	c := newAPI(t, func(s *controller.Services) { s.MaxExportBytes = 1 << 10 })
	ctx := context.Background()
	run, err := c.CreateRun(ctx, configYAML(t, nil), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.UploadExport(ctx, run.ID, strings.NewReader(strings.Repeat("x", 64<<10)), ""); err == nil {
		t.Fatal("a 64 KiB upload was accepted with a 1 KiB limit")
	}
	if v, _ := c.Run(ctx, run.ID); v.ExportSHA256 != "" {
		t.Fatal("an over-limit upload was recorded as the run's export")
	}
}

func lookup(t *testing.T, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Fatalf("%s is not set", k)
	}
	return v
}
