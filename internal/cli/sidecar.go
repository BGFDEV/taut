package cli

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/bgfdev/taut/internal/bus"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/sidecar/budget"
	"github.com/bgfdev/taut/internal/sidecar/egressproxy"
	"github.com/bgfdev/taut/internal/sidecar/tokenbroker"
)

// Defaults for the sidecars, shared by the sidecar commands and the worker.
const (
	defaultTokenSocket = "/run/taut/token/token.sock" //nolint:gosec // a socket path, not a credential
	defaultProxyAddr   = "127.0.0.1:8900"
)

func (a *App) envList(name string) []string {
	var out []string
	for _, v := range strings.Split(a.Getenv(name), ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func (a *App) envOr(name, def string) string {
	if v := a.Getenv(name); v != "" {
		return v
	}
	return def
}

// newSidecarCmd runs a sidecar: a helper process beside a worker.
func newSidecarCmd(app *App) *cobra.Command {
	sidecar := &cobra.Command{
		Use:    "sidecar",
		Short:  "Run a sidecar",
		Hidden: true,
	}

	tok := &cobra.Command{
		Use:   "token-broker",
		Short: "Run the token broker: the only process that reads credentials",
		Long: `The token broker holds the Entra and Slack credentials and hands out
short-lived tokens to the egress proxy over a Unix socket. Nothing else in a
worker pod can read a secret.

The broker serves only the tenants it is told to. A config file names a tenant,
an application and where its credential lives; without this pin, whoever
writes a config file could have any secret the broker can read sent to a sign-in
host of their choosing.

  TAUT_TOKEN_TENANTS           tenant IDs this broker may sign in to (required)
  TAUT_TOKEN_CLIENTS           application IDs allowed (default: any in those tenants)
  TAUT_TOKEN_CLOUDS            clouds allowed (default: global)
  TAUT_TOKEN_SOCKET            socket path (its directory must be private)
  TAUT_SECRETS_DIR             directories file references may point into (default /run/secrets/taut)
  TAUT_TOKEN_ENV_ALLOW         patterns environment references may match (default TAUT_ENTRA_*, TAUT_SLACK_*)
  TAUT_TOKEN_ALLOW_ENDPOINTS   extra authority or Graph endpoints a config file may name`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := newLogger(app)
			cfg := tokenbroker.Config{
				Socket:         app.envOr("TAUT_TOKEN_SOCKET", defaultTokenSocket),
				SecretDirs:     app.envList("TAUT_SECRETS_DIR"),
				EnvAllow:       app.envList("TAUT_TOKEN_ENV_ALLOW"),
				AllowEndpoints: app.envList("TAUT_TOKEN_ALLOW_ENDPOINTS"),
				AllowTenants:   app.envList("TAUT_TOKEN_TENANTS"),
				AllowClients:   app.envList("TAUT_TOKEN_CLIENTS"),
				AllowClouds:    app.envList("TAUT_TOKEN_CLOUDS"),
				Log:            log,
			}
			if len(cfg.AllowTenants) == 0 {
				log.Warn("TAUT_TOKEN_TENANTS is not set: this broker will refuse every Microsoft sign-in. Set it to the tenant ID being migrated to.")
			}
			log.Info("token broker started", "socket", cfg.Socket)
			return tokenbroker.Serve(cmd.Context(), cfg)
		},
	}

	gov := &cobra.Command{
		Use:   "egress-proxy",
		Short: "Run the egress proxy: the worker's only way out",
		Long: `The egress proxy is a forward proxy on localhost. The worker sends it plain HTTP;
it adds the bearer token (from the token broker), holds each request until the
tenant-wide and per-channel budgets allow it, backs every egress proxy of the
tenant off together when Microsoft or Slack throttles, and makes the TLS
connection. It refuses any destination that is not Microsoft Graph,
SharePoint or Slack.

  TAUT_PROXY_ADDR         listen address; must be loopback (default 127.0.0.1:8900)
  TAUT_TOKEN_SOCKET       the token broker's socket
  TAUT_NATS_URL           where budgets are leased and throttling is shared
  TAUT_PROXY_ALLOW        extra destinations, e.g. graph=http://fakegraph:8080
  TAUT_GRAPH_CHANNEL_RPS  per-channel starting rate (default 1)
  TAUT_GRAPH_CHANNEL_MAX  per-channel ceiling (default 5)
  TAUT_GRAPH_CHAT_RPS     per-chat rate; it is not raised (default 1)
  TAUT_GRAPH_CHAT_SHARE   the part of the Graph budget chats may use: more
                          than 0, at most 1 (default 0.4)`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Stated but not a number in (0, 1] is a mistake to stop at,
			// not one to replace with the default: 0 would stop chat
			// imports and 2 would lift the limit.
			chatShare := 0.0
			if raw := strings.TrimSpace(app.Getenv("TAUT_GRAPH_CHAT_SHARE")); raw != "" {
				v, perr := strconv.ParseFloat(raw, 64)
				if perr != nil || !(v > 0 && v <= 1) {
					return fail.New("proxy.config_invalid", "The chat share of the Graph rate is not valid",
						"TAUT_GRAPH_CHAT_SHARE is "+strconv.Quote(raw)+"; it must be a number more than 0 and at most 1.",
						"Set it to the part of the Graph budget chat imports may use, for example 0.5, or leave it unset.").WithExit(fail.ExitUsage)
				}
				chatShare = v
			}

			log := newLogger(app)
			holder := holderID()

			var (
				b       bus.Bus = bus.NewLocal()
				budgets         = budget.Static(budget.DefaultTotals())
			)
			natsURL, err := app.envSecret("TAUT_NATS_URL")
			if err != nil {
				return err
			}
			if url := natsURL.Reveal(); url != "" {
				n, err := bus.DialNATS(url, "taut-egress-proxy", app.natsOptions())
				if err != nil {
					return err
				}
				defer n.Close()
				b, budgets = n, budget.Remote(n)
			} else {
				// Alone, an egress proxy cannot share a budget with others, so
				// it takes the whole tenant budget for itself. That is only
				// right when it is the only one.
				log.Warn("TAUT_NATS_URL is not set: this egress proxy does not coordinate with others. Run only one, or set it.")
			}

			tel := app.startTelemetry(cmd.Context(), log, "egress-proxy", holder)
			defer tel.Shutdown(cmd.Context())
			// The egress proxy's default transport. Keep these settings the
			// same as egressproxy.New's.
			t := http.DefaultTransport.(*http.Transport).Clone()
			t.DisableCompression = true // pass bodies through exactly as sent
			t.MaxIdleConnsPerHost = 64
			t.ResponseHeaderTimeout = 2 * time.Minute
			// Trace headers never leave for Microsoft or Slack, whether or
			// not this egress proxy's own telemetry is on: a worker's may be.
			var egress http.RoundTripper = stripTrace{t}
			if tel.Enabled() {
				egress = tel.EgressTransport(t)
			}

			g, err := egressproxy.New(egressproxy.Config{
				Holder:          holder,
				Transport:       egress,
				Tokens:          tokenbroker.NewClient(app.envOr("TAUT_TOKEN_SOCKET", defaultTokenSocket)),
				Budgets:         budgets,
				Bus:             b,
				AllowHosts:      app.envList("TAUT_PROXY_ALLOW"),
				ChannelStartRPS: app.envFloat("TAUT_GRAPH_CHANNEL_RPS", 0),
				ChannelMaxRPS:   app.envFloat("TAUT_GRAPH_CHANNEL_MAX", 0),
				ChatShare:       chatShare,
				ChatRPS:         app.envFloat("TAUT_GRAPH_CHAT_RPS", 0),
				// A lease is never honored above these ceilings, whatever
				// arrives over the bus. They must match what the control
				// plane was told to hand out.
				MaxRPS: app.budgetTotals(),
				Log:    log.With("proxy", holder),
			})
			if err != nil {
				return err
			}
			defer g.Close()
			addr := app.envOr("TAUT_PROXY_ADDR", defaultProxyAddr)
			log.Info("egress proxy started", "addr", addr, "proxy", holder)
			if err := g.Serve(cmd.Context(), addr); err != nil {
				return fail.Wrap(err, "proxy.serve_failed", "The egress proxy could not start", "",
					"Check that "+addr+" is free, or set TAUT_PROXY_ADDR.")
			}
			return nil
		},
	}

	sidecar.AddCommand(tok, gov)
	return sidecar
}

// stripTrace removes trace-context headers from outbound requests.
type stripTrace struct{ next http.RoundTripper }

func (s stripTrace) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Header.Get("Traceparent") != "" || r.Header.Get("Tracestate") != "" || r.Header.Get("Baggage") != "" {
		r = r.Clone(r.Context())
		r.Header.Del("Traceparent")
		r.Header.Del("Tracestate")
		r.Header.Del("Baggage")
	}
	return s.next.RoundTrip(r)
}
