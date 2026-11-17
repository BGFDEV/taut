package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/bgfdev/taut/internal/bus"
	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/secret"
	"github.com/bgfdev/taut/internal/sidecar/budget"
	"github.com/bgfdev/taut/internal/staging"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/telemetry"
	"github.com/bgfdev/taut/internal/version"
	"github.com/bgfdev/taut/internal/worker"
)

// Service configuration comes from the environment, the way a container or
// a Kubernetes pod supplies it. Secrets may be given directly (NAME) or as
// the path of a file holding them (NAME_FILE), which is how mounted secrets
// arrive.

// envSecret reads a secret from NAME or from the file named by NAME_FILE.
func (a *App) envSecret(name string) (secret.Value, error) {
	if v := a.Getenv(name); v != "" {
		return secret.New(v), nil
	}
	if path := a.Getenv(name + "_FILE"); path != "" {
		v, err := secret.Ref{File: path}.Resolve("")
		if err != nil {
			return secret.Value{}, err
		}
		return v, nil
	}
	return secret.Value{}, nil
}

func missingEnv(name, what, next string) error {
	return fail.New("service.config_missing", name+" is not set", what+" needs it.", next).WithExit(fail.ExitUsage)
}

// backing holds the services the controller and the workers share.
type backing struct {
	Store *store.PG
	Blobs staging.Blobs
	KEK   staging.KEK
	Bus   bus.Bus
}

func (b *backing) Close() {
	if b.Bus != nil {
		b.Bus.Close()
	}
	if b.Store != nil {
		b.Store.Close()
	}
	b.KEK.Destroy()
}

// openBacking connects to Postgres, staging and the bus.
func (a *App) openBacking(ctx context.Context, service, clientName string) (*backing, error) {
	b := &backing{}
	ok := false
	defer func() {
		if !ok {
			b.Close()
		}
	}()

	dbURL, err := a.envSecret("TAUT_POSTGRES_URL")
	if err != nil {
		return nil, err
	}
	if dbURL.IsZero() {
		return nil, missingEnv("TAUT_POSTGRES_URL", service, "Set it to the Postgres connection URL, e.g. postgres://taut:…@postgres:5432/taut.")
	}
	b.Store, err = store.Open(ctx, dbURL.Reveal())
	if err != nil {
		return nil, err
	}
	if err := b.Store.Migrate(ctx); err != nil {
		return nil, err
	}

	kek, err := a.envSecret("TAUT_STAGING_KEY")
	if err != nil {
		return nil, err
	}
	if kek.IsZero() {
		return nil, missingEnv("TAUT_STAGING_KEY", service,
			"Generate a key once with `taut dev keygen` and give the same value to the controller and every worker. Keep it secret: it protects everything in staging.")
	}
	if b.KEK, err = staging.ParseKEK(kek); err != nil {
		return nil, err
	}

	if b.Blobs, err = a.openStaging(ctx, service); err != nil {
		return nil, err
	}

	natsURL, err := a.envSecret("TAUT_NATS_URL")
	if err != nil {
		return nil, err
	}
	if !natsURL.IsZero() {
		if b.Bus, err = bus.DialNATS(natsURL.Reveal(), clientName, a.natsOptions()); err != nil {
			return nil, err
		}
	} else {
		// Without NATS, workers find work on their poll timer.
		b.Bus = bus.NewLocal()
	}
	ok = true
	return b, nil
}

func (a *App) openStaging(ctx context.Context, service string) (staging.Blobs, error) {
	backend := a.Getenv("TAUT_STAGING_BACKEND")
	if backend == "" {
		if a.Getenv("TAUT_S3_ENDPOINT") != "" {
			backend = "s3"
		} else {
			backend = "fs"
		}
	}
	switch backend {
	case "fs":
		dir := a.Getenv("TAUT_STAGING_DIR")
		if dir == "" {
			return nil, missingEnv("TAUT_STAGING_DIR", service, "Set it to a directory shared by the controller and the workers, or configure S3 with TAUT_S3_ENDPOINT.")
		}
		return staging.NewFS(dir)
	case "s3":
		access, err := a.envSecret("TAUT_S3_ACCESS_KEY")
		if err != nil {
			return nil, err
		}
		secretKey, err := a.envSecret("TAUT_S3_SECRET_KEY")
		if err != nil {
			return nil, err
		}
		cfg := staging.S3Config{
			Endpoint: a.Getenv("TAUT_S3_ENDPOINT"), Region: a.Getenv("TAUT_S3_REGION"), Bucket: a.Getenv("TAUT_S3_BUCKET"),
			AccessKey: access, SecretKey: secretKey,
		}
		if cfg.Region == "" {
			cfg.Region = "us-east-1"
		}
		if cfg.Bucket == "" {
			cfg.Bucket = "taut-staging"
		}
		s3, err := staging.NewS3(cfg)
		if err != nil {
			return nil, err
		}
		if err := s3.EnsureBucket(ctx); err != nil {
			return nil, err
		}
		return s3, nil
	}
	return nil, fail.Usagef("Set TAUT_STAGING_BACKEND to fs or s3.", "Unknown staging backend %q", backend)
}

func (a *App) envFloat(name string, def float64) float64 {
	if v, err := strconv.ParseFloat(a.Getenv(name), 64); err == nil && v > 0 {
		return v
	}
	return def
}

func (a *App) envInt(name string, def int) int {
	if v, err := strconv.Atoi(a.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

// startTelemetry sets up traces and metrics for a service, exported to the
// collector named by OTEL_EXPORTER_OTLP_ENDPOINT, or does nothing if none is
// named. Telemetry is never a reason not to start: if it cannot be set up
// the service says so and runs without it. Defer Shutdown on the result.
func (a *App) startTelemetry(ctx context.Context, log *slog.Logger, service, instance string) *telemetry.Providers {
	tel, err := telemetry.Setup(ctx, telemetry.Options{
		Service: service, Version: version.Version, Instance: instance, Getenv: a.Getenv, Log: log,
	})
	if err != nil {
		log.Warn("telemetry could not be set up; continuing without it", "error", err.Error())
	}
	return tel
}

// newControllerCmd runs the control plane. It is a service, not an operator
// command, so it is hidden from the main help.
func newControllerCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:    "controller",
		Short:  "Run the controller",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := newLogger(app)
			// Readiness probes these addresses. The URLs may arrive as
			// files; only host and port are kept.
			pgURL, _ := app.envSecret("TAUT_POSTGRES_URL")
			natsURL, _ := app.envSecret("TAUT_NATS_URL")
			cfg := controller.Config{
				Addr:        app.Getenv("TAUT_CONTROLLER_ADDR"),
				PostgresURL: redactURL(pgURL.Reveal()),
				NATSURL:     redactURL(natsURL.Reveal()),
				S3Endpoint:  app.Getenv("TAUT_S3_ENDPOINT"),
			}
			if cfg.Addr == "" {
				cfg.Addr = ":7070"
			}
			b, err := app.openBacking(cmd.Context(), "The controller", "taut-controller")
			if err != nil {
				return err
			}
			defer b.Close()
			token, err := app.envSecret("TAUT_CONTROLLER_TOKEN")
			if err != nil {
				return err
			}
			if token.IsZero() {
				// Whoever can reach this API is the migration operator:
				// they decide what gets imported as whom. Open is never the
				// default.
				if app.Getenv("TAUT_CONTROLLER_INSECURE") != "1" {
					return fail.New("controller.no_token", "TAUT_CONTROLLER_TOKEN is not set",
						"The controller's API starts and steers migrations, so it requires a token.",
						"Set TAUT_CONTROLLER_TOKEN (or TAUT_CONTROLLER_TOKEN_FILE) for the controller and the same value where `taut migrate` runs. For a throwaway local stack, set TAUT_CONTROLLER_INSECURE=1.").WithExit(fail.ExitUsage)
				}
				log.Warn("TAUT_CONTROLLER_INSECURE=1: the API accepts unauthenticated requests. Local development only.")
			}
			// The control plane owns the tenant-wide rate budgets; each
			// egress proxy leases its share over the bus.
			alloc := budget.NewAllocator(app.budgetTotals(), 0, nil)
			stopAlloc, err := alloc.Serve(b.Bus)
			if err != nil {
				return fail.Wrap(err, "controller.budget_failed", "The controller could not start serving rate budgets", "", "Check the NATS connection (TAUT_NATS_URL).")
			}
			defer stopAlloc()
			tel := app.startTelemetry(cmd.Context(), log, "controller", holderID())
			if err := tel.RegisterRunStats(func(ctx context.Context) ([]telemetry.RunStat, error) { return runStats(ctx, b.Store) }); err != nil {
				log.Warn("run gauges are not available", "error", err)
			}
			defer tel.Shutdown(cmd.Context())
			svc := controller.Services{
				Store: b.Store, Blobs: b.Blobs, KEK: b.KEK, Bus: b.Bus, Token: token.Reveal(),
				AllowTestEndpoints: app.Getenv("TAUT_ALLOW_TEST_ENDPOINTS") == "1",
				Budgets:            alloc.Snapshot,
				Middleware:         untraced(tel.HTTPMiddleware("controller")),
			}
			if err := controller.New(cfg, log, svc).Serve(cmd.Context()); err != nil {
				return fail.Wrap(err, "controller.serve_failed", "The controller could not start", "",
					"Check that "+cfg.Addr+" is free, or set TAUT_CONTROLLER_ADDR.")
			}
			return nil
		},
	}
}

// redactURL drops credentials from a connection URL kept for health probes.
func redactURL(raw string) string {
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			return raw[:i+3] + rest[at+1:]
		}
	}
	return raw
}

// graphFactory builds Graph clients for runs. The run's config names the
// tenant and references the credential; the reference is resolved here, in
// the worker's environment. TAUT_ENTRA_AUTHORITY and TAUT_GRAPH_URL
// override the endpoints, which is how the local stack points workers at
// the fake servers by their in-network address.
func (a *App) graphFactory() worker.GraphFactory {
	return func(_ context.Context, m *config.Config) (worker.Graph, error) {
		mc := *m
		// Endpoints come from this process's own configuration only. A
		// config file must not be able to say where a credential is sent.
		mc.Target.Entra.Authority = a.Getenv("TAUT_ENTRA_AUTHORITY")
		mc.Target.Entra.GraphBaseURL = a.Getenv("TAUT_GRAPH_URL")
		cred, _, err := entraCredential(&mc)
		if err != nil {
			return nil, err
		}
		sess, err := connectGraph(&mc, cred)
		if err != nil {
			return nil, err
		}
		return sess.Client, nil
	}
}

func holderID() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "worker"
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), hex.EncodeToString(b[:]))
}

// newWorkerCmd runs a worker.
func newWorkerCmd(app *App) *cobra.Command {
	var stages []string
	cmd := &cobra.Command{
		Use:    "worker",
		Short:  "Run a worker",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := newLogger(app)
			if len(stages) == 0 {
				if v := app.Getenv("TAUT_WORKER_STAGES"); v != "" {
					stages = strings.Split(v, ",")
				}
			}
			all := []string{store.StagePlan, store.StageExtract, store.StageTransform, store.StageTeam, store.StageFiles, store.StageLoad, store.StageFinalize, store.StageMembers, store.StageRelease}
			if len(stages) == 0 || (len(stages) == 1 && stages[0] == "all") {
				stages = all
			}
			for i, s := range stages {
				stages[i] = strings.TrimSpace(s)
				valid := false
				for _, k := range all {
					valid = valid || k == stages[i]
				}
				if !valid {
					return fail.Usagef("Choose from: "+strings.Join(all, ", ")+", or all.", "Unknown stage %q", stages[i])
				}
			}

			// A worker that can put a chat into migration mode, or take one
			// out at finalize, can also take one out when its run is
			// abandoned. That work must never be without a worker.
			if (slices.Contains(stages, store.StageLoad) || slices.Contains(stages, store.StageFinalize)) && !slices.Contains(stages, store.StageRelease) {
				stages = append(stages, store.StageRelease)
			}

			b, err := app.openBacking(cmd.Context(), "A worker", "taut-worker")
			if err != nil {
				return err
			}
			defer b.Close()
			holder := holderID()
			tel := app.startTelemetry(cmd.Context(), log, "worker", holder)
			defer tel.Shutdown(cmd.Context())
			// Outbound requests carry the batch's trace to the egress proxy.
			outbound := &http.Client{Transport: tel.Transport(nil)}
			cfg := worker.Config{
				Holder: holder, Stages: stages, Store: tel.Ledger(b.Store), Blobs: b.Blobs, KEK: b.KEK, Bus: b.Bus,
				Log:         log.With("worker", holder),
				Concurrency: app.envInt("TAUT_WORKER_CONCURRENCY", 4),
				ScratchDir:  app.Getenv("TAUT_SCRATCH_DIR"),
				Observer:    tel.Observer(),
			}
			if gov := app.Getenv("TAUT_PROXY_URL"); gov != "" {
				// The normal arrangement: this process holds no credential
				// and reaches Microsoft and Slack only through its egress proxy,
				// which also sets the pace.
				route := worker.Proxied{
					URL: gov, Authority: app.Getenv("TAUT_ENTRA_AUTHORITY"), GraphBase: app.Getenv("TAUT_GRAPH_URL"),
					SlackFilesBase: app.Getenv("TAUT_SLACK_FILES_URL"),
					Client:         outbound,
				}
				cfg.Graph, cfg.Slack = route.GraphFactory(), route.SlackFiles()
				cfg.ChannelRPS, cfg.TenantRPS = 1e6, 1e6
				log.Info("routing through the egress proxy", "proxy", gov)
			} else {
				// Without an egress proxy a worker would have to hold the
				// credential and resolve the config file's references itself,
				// which lets whoever writes a config file aim it at any secret
				// this process can read. That is never the default.
				if app.Getenv("TAUT_INSECURE_NO_SIDECARS") != "1" {
					return fail.New("worker.no_proxy", "TAUT_PROXY_URL is not set",
						"A worker reaches Microsoft and Slack only through its egress proxy, and holds no credentials itself.",
						"Run `taut sidecar token-broker` and `taut sidecar egress-proxy` beside the worker and set TAUT_PROXY_URL (the Compose file shows the arrangement). For a throwaway test without sidecars, set TAUT_INSECURE_NO_SIDECARS=1.").WithExit(fail.ExitUsage)
				}
				log.Warn("TAUT_INSECURE_NO_SIDECARS=1: this worker holds the credential itself and limits only its own rate. Do not use this with a real tenant.")
				files := app.Getenv("TAUT_SLACK_FILES_URL")
				cfg.Graph = app.graphFactory()
				cfg.Slack = worker.HTTPSlackFiles{Client: outbound, Rewrite: func(_ *config.Config, raw string) (string, http.Header, error) {
					if files != "" {
						raw = strings.Replace(raw, "https://files.slack.com", strings.TrimRight(files, "/"), 1)
					}
					return raw, nil, nil
				}}
				cfg.ChannelRPS = app.envFloat("TAUT_GRAPH_CHANNEL_RPS", 1)
				cfg.TenantRPS = app.envFloat("TAUT_GRAPH_TENANT_RPS", 40)
			}
			d, err := worker.New(cfg)
			if err != nil {
				return err
			}
			log.Info("worker started", "worker", holder, "stages", strings.Join(stages, ","))
			// An optional liveness endpoint, for orchestrators. It says the
			// process is running its loops; it exposes nothing else.
			if addr := app.Getenv("TAUT_WORKER_HEALTH_ADDR"); addr != "" {
				mux := http.NewServeMux()
				mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
				hs := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
				ln, err := (&net.ListenConfig{}).Listen(cmd.Context(), "tcp", addr)
				if err != nil {
					return fail.Wrap(err, "worker.health_listen", "The worker could not open its health endpoint", "",
						"Check that "+addr+" is free, or change TAUT_WORKER_HEALTH_ADDR.").WithExit(fail.ExitUsage)
				}
				go func() { _ = hs.Serve(ln) }()
				defer hs.Close()
			}
			return d.Run(cmd.Context())
		},
	}
	cmd.Flags().StringSliceVar(&stages, "stages", nil, "stages to work on: plan, extract, transform, team, files, load, or all")
	return cmd
}

// budgetTotals returns the tenant-wide rate budgets per scope: the defaults
// (40 for Graph, 20 for SharePoint, 1 for Slack), overridden by
// TAUT_BUDGET_GRAPH_RPS, TAUT_BUDGET_SHAREPOINT_RPS and TAUT_BUDGET_SLACK_RPS.
// The controller hands these out; each egress proxy uses the same values as a ceiling.
func (a *App) budgetTotals() map[string]float64 {
	totals := budget.DefaultTotals()
	for scope, env := range map[string]string{
		"graph": "TAUT_BUDGET_GRAPH_RPS", "sharepoint": "TAUT_BUDGET_SHAREPOINT_RPS", "slack": "TAUT_BUDGET_SLACK_RPS",
	} {
		totals[scope] = a.envFloat(env, totals[scope])
	}
	return totals
}

// untraced drops inbound trace context before a middleware sees it. The controller's
// callers are operators' CLIs, not services in a trace, and the headers
// arrive before the API token is checked: an unauthenticated caller must
// not be able to attach the controller's spans to a trace of their choosing.
func untraced(mw func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		inner := mw(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Del("Traceparent")
			r.Header.Del("Tracestate")
			r.Header.Del("Baggage")
			inner.ServeHTTP(w, r)
		})
	}
}

// runStats summarizes the active runs for the controller's gauges.
func runStats(ctx context.Context, st store.Store) ([]telemetry.RunStat, error) {
	runs, err := st.ActiveRuns(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]telemetry.RunStat, 0, len(runs))
	for _, r := range runs {
		if r.Status == store.RunAwaitingExport {
			out = append(out, telemetry.RunStat{})
			continue
		}
		p, err := st.Progress(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		rs := telemetry.RunStat{
			Posted: int64(p.Posted), Failed: int64(p.Failed), Held: int64(p.Held),
			Pending: int64(max(p.Posts-p.Posted-p.Failed-p.Held, 0)),
		}
		for _, s := range p.Stages {
			for status, n := range map[string]int{
				"open": s.Lanes - s.Done - s.Failed - s.Leased, "active": s.Leased, "done": s.Done, "failed": s.Failed,
			} {
				if n > 0 {
					rs.Lanes = append(rs.Lanes, telemetry.LaneStat{Stage: s.Stage, Status: status, Count: int64(n)})
				}
			}
		}
		out = append(out, rs)
	}
	return out, nil
}

// natsOptions reads how this process authenticates to NATS and what it
// trusts: TAUT_NATS_CREDS_FILE, TAUT_NATS_CA_FILE, TAUT_NATS_CERT_FILE and
// TAUT_NATS_KEY_FILE. The URL itself may be given as TAUT_NATS_URL_FILE.
func (a *App) natsOptions() bus.NATSOptions {
	return bus.NATSOptions{
		CredsFile: a.Getenv("TAUT_NATS_CREDS_FILE"), CAFile: a.Getenv("TAUT_NATS_CA_FILE"),
		CertFile: a.Getenv("TAUT_NATS_CERT_FILE"), KeyFile: a.Getenv("TAUT_NATS_KEY_FILE"),
	}
}
