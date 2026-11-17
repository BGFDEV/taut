package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/bgfdev/taut/internal/devstack"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/fakegraph"
	"github.com/bgfdev/taut/internal/fixtures"
	"github.com/bgfdev/taut/internal/secret"
	"github.com/bgfdev/taut/internal/staging"
)

// newLogger returns the structured JSON logger services use. Every record
// passes through the redacting handler, so a token can never reach a log.
func newLogger(app *App) *slog.Logger {
	level := slog.LevelInfo
	if app.Verbose {
		level = slog.LevelDebug
	}
	return slog.New(secret.NewRedactingHandler(slog.NewJSONHandler(app.err, &slog.HandlerOptions{Level: level})))
}

// newDevCmd groups development helpers: the fake servers and fixtures.
func newDevCmd(app *App) *cobra.Command {
	dev := &cobra.Command{
		Use:    "dev",
		Short:  "Development helpers (fake servers, fixtures)",
		Hidden: true,
	}

	var addr, fixture, limits string
	var latency, jitter time.Duration
	fake := &cobra.Command{
		Use:   "fakegraph",
		Short: "Serve a fake Entra, Microsoft Graph and Slack API on one port",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !fixtures.Known(fixture) {
				return fail.Usagef("Choose one of: small, medium, nasty, or a load fixture such as load:channels=150,messages=200000.", "Unknown fixture %q", fixture)
			}
			var opts []fakegraph.Option
			switch limits {
			case "", "none":
			case "documented":
				opts = append(opts, fakegraph.WithDocumentedLimits())
			default:
				return fail.Usagef("Use --limits none or --limits documented.", "Unknown limits %q", limits)
			}
			if latency < 0 || jitter < 0 {
				return fail.Usagef("Give --latency and --jitter as durations such as 20ms.", "A delay cannot be negative")
			}
			if latency > 0 || jitter > 0 {
				opts = append(opts, fakegraph.WithLatency(latency, jitter))
			}
			ln, err := (&net.ListenConfig{}).Listen(cmd.Context(), "tcp", addr)
			if err != nil {
				return fail.Wrap(err, "dev.listen_failed", "Could not listen on "+addr, "", "Pick a free port with --addr.")
			}
			log := newLogger(app)
			log.Info("fake servers listening", "addr", ln.Addr().String(), "fixture", fixture,
				"graph", "/v1.0", "token", "/{tenant}/oauth2/v2.0/token", "slack", "/slack/api",
				"limits", cmp.Or(limits, "none"), "latency", latency.String(), "jitter", jitter.String())
			srv := &http.Server{Handler: devstack.Handler(fixture, opts...), ReadHeaderTimeout: 5 * time.Second}
			go func() {
				<-cmd.Context().Done()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = srv.Shutdown(ctx)
			}()
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	}
	fake.Flags().StringVar(&addr, "addr", "127.0.0.1:8080", "listen address")
	fake.Flags().StringVar(&fixture, "fixture", "medium", "fixture whose people populate the fake directory")
	fake.Flags().StringVar(&limits, "limits", "none", "rate limits on message imports: none, or documented (5/s per channel, 1/s per chat, 50/s and 20/s per tenant)")
	fake.Flags().DurationVar(&latency, "latency", 0, "delay every Graph request by this long")
	fake.Flags().DurationVar(&jitter, "jitter", 0, "and by a random part of this on top")

	var outDir, load string
	fix := &cobra.Command{
		Use:   "fixtures",
		Short: "Generate the fixture Slack exports",
		Long: `Generate the fixture Slack exports: small, medium and nasty.

With --load, generate a load fixture instead: a workspace of the size and
shape given, written as a stream so that it can be far larger than memory.
The spec is comma-separated key=value pairs over the defaults (150
channels, 200,000 messages, 400 users, 365 days, threads=0.2, files=0.01,
private=0.1, skew=1.05, no direct messages):

  taut dev fixtures --load channels=300,messages=1000000 --out /tmp/load

It writes load.zip and, beside it, load.facts.json: the exact counts of
what the export holds, for checking an import against.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if err := os.MkdirAll(outDir, 0o750); err != nil {
				return fail.Wrap(err, "dev.fixtures_failed", "Could not create "+outDir, "", "Choose a writable directory with --out.")
			}
			if load != "" {
				name := load
				if !fixtures.IsLoad(name) {
					name = fixtures.LoadPrefix + ":" + load
				}
				spec, err := fixtures.ParseLoadSpec(name)
				if err != nil {
					return fail.Usagef("Run `taut dev fixtures --help` for the keys.", "%s", strings.TrimPrefix(err.Error(), "fixtures: "))
				}
				path := filepath.Join(outDir, fixtures.LoadPrefix+".zip")
				f, err := os.Create(path) //nolint:gosec // a path the operator chose
				if err != nil {
					return fail.Wrap(err, "dev.fixtures_failed", "Could not create "+path, "", "Choose a writable directory with --out.")
				}
				facts, err := fixtures.WriteLoad(spec, f)
				if cerr := f.Close(); err == nil {
					err = cerr
				}
				if err != nil {
					return fail.Wrap(err, "dev.fixtures_failed", "Could not write "+path, "", "Check the free space.")
				}
				st, err := os.Stat(path)
				if err != nil {
					return err
				}
				doc := map[string]any{"fixture": spec.String(), "path": path, "bytes": st.Size(), "facts": facts}
				raw, err := json.MarshalIndent(doc, "", "  ")
				if err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(outDir, fixtures.LoadPrefix+".facts.json"), append(raw, '\n'), 0o600); err != nil {
					return fail.Wrap(err, "dev.fixtures_failed", "Could not write the facts beside "+path, "", "Check the directory.")
				}
				if app.IO.IsJSON() {
					return app.IO.JSON(doc)
				}
				app.IO.Println(path)
				return nil
			}
			type row struct {
				Name string `json:"name"`
				Path string `json:"path"`
			}
			var rows []row
			for _, name := range fixtures.Names() {
				p, err := fixtures.Write(name, outDir)
				if err != nil {
					return err
				}
				rows = append(rows, row{name, p})
				app.IO.Println(p)
			}
			if app.IO.IsJSON() {
				return app.IO.JSON(rows)
			}
			return nil
		},
	}
	fix.Flags().StringVar(&outDir, "out", "testdata/fixtures", "directory to write the ZIPs to")
	fix.Flags().StringVar(&load, "load", "", "generate a load fixture of this spec instead, e.g. channels=150,messages=200000")

	// The runtime image has no shell or curl, so container health checks
	// call the binary itself.
	health := &cobra.Command{
		Use:   "healthcheck <url>",
		Short: "Exit 0 if the URL answers 2xx",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Second)
			defer cancel()
			// unix:///path/to.sock:/healthz asks a server on a Unix socket,
			// which is how the token broker listens.
			client, target := http.DefaultClient, args[0]
			if rest, ok := strings.CutPrefix(target, "unix://"); ok {
				sock, path, _ := strings.Cut(rest, ":")
				client = &http.Client{Transport: &http.Transport{
					DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
						return (&net.Dialer{}).DialContext(ctx, "unix", sock)
					},
				}}
				target = "http://unix" + path
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
			if err != nil {
				return fail.Usagef("Pass a full URL, e.g. http://127.0.0.1:7070/healthz.", "Not a valid URL")
			}
			resp, err := client.Do(req)
			if err != nil {
				return fail.New("dev.unhealthy", "No answer from "+args[0], "", "Check that the service is running.")
			}
			defer resp.Body.Close()
			if resp.StatusCode/100 != 2 {
				return fail.New("dev.unhealthy", fmt.Sprintf("%s answered HTTP %d", args[0], resp.StatusCode), "", "Check the service's logs.")
			}
			return nil
		},
	}

	seed := &cobra.Command{
		Use:   "seed",
		Short: "Print the well-known values of the fake tenant",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return app.IO.JSON(map[string]string{
				"tenantId":        fakegraph.DefaultTenantID,
				"clientId":        fakegraph.DefaultClientID,
				"clientSecret":    fakegraph.DefaultSecret,
				"slackToken":      devstack.SlackToken,
				"owner":           devstack.OwnerUPN,
				"unmappedAccount": devstack.FormerMemberUPN,
			})
		},
	}

	keygen := &cobra.Command{
		Use:   "keygen",
		Short: "Generate a staging key-encryption key (TAUT_STAGING_KEY)",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			k, err := staging.GenerateKEK()
			if err != nil {
				return err
			}
			if app.IO.IsJSON() {
				return app.IO.JSON(map[string]string{"TAUT_STAGING_KEY": k})
			}
			// Printed alone on stdout so it can be captured by a script.
			_, err = fmt.Fprintln(app.IO.Out, k)
			return err
		},
	}

	dev.AddCommand(fake, fix, health, seed, keygen)
	return dev
}
