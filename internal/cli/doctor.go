package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/colorprofile"
	"github.com/spf13/cobra"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/graph"
	"github.com/bgfdev/taut/internal/secret"
	"github.com/bgfdev/taut/internal/slack/export"
	"github.com/bgfdev/taut/internal/slackapi"
	"github.com/bgfdev/taut/internal/tui/checklist"
	"github.com/bgfdev/taut/internal/version"
)

const (
	groupEnv     = "Environment"
	groupConfig  = "Config"
	groupNetwork = "Connectivity"
	groupStack   = "Control plane"
)

// probeResult is what one HTTP reachability probe learned.
type probeResult struct {
	status  int
	elapsed time.Duration
	// serverTime is the response's Date header, used to detect clock skew.
	serverTime time.Time
}

// probe makes one GET and reports whether anything answered. Any HTTP
// response counts as reachable: the point is DNS, routing, TLS and proxies,
// not the status code.
func probe(ctx context.Context, rawURL string) (probeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return probeResult{}, err
	}
	req.Header.Set("User-Agent", userAgent())
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return probeResult{}, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	r := probeResult{status: resp.StatusCode, elapsed: time.Since(start)}
	if d, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
		r.serverTime = d
	}
	return r, nil
}

func hostOf(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		return u.Host
	}
	return rawURL
}

func reachable(label, rawURL string, onResult func(probeResult)) checklist.Check {
	return checklist.Check{Group: groupNetwork, Label: label, Run: func(ctx context.Context) checklist.Result {
		r, err := probe(ctx, rawURL)
		if err != nil {
			return checklist.Fail("network.unreachable", hostOf(rawURL)+": "+err.Error(),
				"Check DNS, firewall and proxy settings (HTTPS_PROXY / NO_PROXY) for "+hostOf(rawURL)+".")
		}
		if onResult != nil {
			onResult(r)
		}
		return checklist.OK(fmt.Sprintf("%s answered in %d ms", hostOf(rawURL), r.elapsed.Milliseconds()))
	}}
}

func doctorChecks(app *App) []checklist.Check {
	var (
		m         *config.Config
		authority probeResult
	)
	// Local mode needs other things of this machine than a cluster does, so
	// which checks there are depends on the config file. It is read here
	// for that alone; the Config check below reports on it.
	isLocal := false
	if pre, err := config.Load(app.ConfigPath); err == nil {
		isLocal = pre.Local.Enabled
	}
	checks := []checklist.Check{
		{Group: groupEnv, Label: "Taut", Run: func(context.Context) checklist.Result {
			return checklist.OK(fmt.Sprintf("%s (%s), %s/%s", version.Version, version.Commit, runtime.GOOS, runtime.GOARCH))
		}},
		{Group: groupEnv, Label: "Terminal", Run: func(context.Context) checklist.Result {
			io := app.IO
			glyphs, bg := "ASCII", "light"
			if io.Theme.Unicode {
				glyphs = "Unicode"
			}
			if io.Theme.Dark {
				bg = "dark"
			}
			color := "no color"
			switch io.Profile {
			case colorprofile.ANSI:
				color = "16 colors"
			case colorprofile.ANSI256:
				color = "256 colors"
			case colorprofile.TrueColor:
				color = "true color"
			}
			mode := "non-interactive"
			if io.Interactive() {
				mode = "interactive"
			}
			return checklist.OK(fmt.Sprintf("%s, %s, %s, %s, %d columns", mode, color, glyphs, bg, io.Width))
		}},
		{Group: groupEnv, Label: "Proxy", Run: func(context.Context) checklist.Result {
			for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
				if v := app.Getenv(k); v != "" {
					return checklist.Info(k + " is set; requests go through " + hostOf(v))
				}
			}
			return checklist.OK("none configured")
		}},
		{Group: groupEnv, Label: "Docker", Run: func(ctx context.Context) checklist.Result {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if isLocal {
				return checklist.Skip("not needed in local mode")
			}
			out, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}").Output()
			if err != nil {
				return checklist.Warn("not available", "Docker is only needed for the local stack (`make up`). Install or start Docker Desktop if you want it.")
			}
			return checklist.OK("engine " + strings.TrimSpace(string(out)))
		}},

		{Group: groupConfig, Label: "Config", Run: func(context.Context) checklist.Result {
			loaded, err := config.Load(app.ConfigPath)
			if err != nil {
				fe := fail.From(err)
				if fe.Code == "config.not_found" {
					return checklist.Warn(app.ConfigPath+" not found", "Run `taut init` to create one, or pass --config <path>.")
				}
				return failed(err)
			}
			issues := loaded.Validate()
			if issues.HasErrors() {
				return failed(issues.Err())
			}
			m = loaded
			if w := issues.Warnings(); len(w) > 0 {
				return checklist.Warn(fmt.Sprintf("valid, with %s: %s: %s", countOf(len(w), "warning", "warnings"), w[0].Path, w[0].Message), "Review the warning in "+app.ConfigPath+".")
			}
			return checklist.OK(app.ConfigPath + " is valid (" + m.Metadata.Name + ")")
		}},
		{Group: groupConfig, Label: "Slack export", Run: func(context.Context) checklist.Result {
			if m == nil {
				return checklist.Skip("no config file")
			}
			ex, err := export.Open(m.ResolvePath(m.Source.Slack.Export))
			if err != nil {
				return failed(err)
			}
			defer ex.Close()
			chans, err := ex.Channels()
			if err != nil {
				return failed(err)
			}
			users, err := ex.Users()
			if err != nil {
				return failed(err)
			}
			return checklist.OK(fmt.Sprintf("%d conversations, %d users", len(chans), len(users)))
		}},
		{Group: groupConfig, Label: "Slack token", Run: func(context.Context) checklist.Result {
			if m == nil {
				return checklist.Skip("no config file")
			}
			if !m.Source.Slack.API.Enabled {
				return checklist.Skip("Slack API is off")
			}
			tok, warns, err := m.ResolveSecret(m.Source.Slack.API.Token)
			if err != nil {
				return failed(err)
			}
			where := m.Source.Slack.API.Token.Describe()
			if kind := slackapi.TokenKind(tok.Reveal()); kind != "bot" && kind != "user" {
				return checklist.Warn(where+" does not hold a bot or user token ("+kind+")", "Use the Bot User OAuth Token (xoxb-) from the Slack app's OAuth & Permissions page.")
			}
			if len(warns) > 0 {
				return checklist.Warn(where+"; "+warns[0].Message, warns[0].Next)
			}
			return checklist.OK(where + " is set")
		}},
		{Group: groupConfig, Label: "Entra credential", Run: func(context.Context) checklist.Result {
			if m == nil {
				return checklist.Skip("no config file")
			}
			cred, warns, err := entraCredential(m)
			if err != nil {
				return failed(err)
			}
			if len(warns) > 0 {
				return checklist.Warn(warns[0].Message, warns[0].Next)
			}
			if cert, ok := cred.(*graph.ClientCertificate); ok {
				return checklist.OK("certificate " + shortThumb(cert.Thumbprint()) + ", valid until " + cert.NotAfter().Format("2 Jan 2006"))
			}
			return checklist.OK("client secret is set")
		}},
		{Group: groupConfig, Label: "Staging secrets", Run: func(context.Context) checklist.Result {
			if m == nil {
				return checklist.Skip("no config file")
			}
			if m.Local.Enabled {
				return checklist.Skip("not used in local mode; the state directory has its own key")
			}
			var missing []string
			for _, ref := range []struct {
				name string
				err  error
			}{
				{"accessKey", resolveErr(m, m.Staging.AccessKey)},
				{"secretKey", resolveErr(m, m.Staging.SecretKey)},
				{"encryptionKey", resolveErr(m, m.Staging.EncryptionKey)},
			} {
				if ref.err != nil {
					missing = append(missing, ref.name)
				}
			}
			if len(missing) > 0 {
				// Staging is not touched until `taut migrate`, so this is
				// advice rather than a failure.
				return checklist.Skip("not set yet (" + strings.Join(missing, ", ") + "); needed from `taut migrate`")
			}
			return checklist.OK("all three are set")
		}},
	}

	// Connectivity targets come from the config file when there is one, and
	// fall back to the public cloud otherwise.
	endpoints := func() (authorityURL, graphURL, slackURL string) {
		cloud, _ := graph.LookupCloud("")
		slackURL = slackapi.DefaultBaseURL
		if m != nil {
			if c, err := graph.LookupCloud(m.Target.Entra.Cloud); err == nil {
				cloud = c
			}
			if v := m.Target.Entra.Authority; v != "" {
				cloud.Authority = v
			}
			if v := m.Target.Entra.GraphBaseURL; v != "" {
				cloud.GraphBase = v
			}
			if v := m.Source.Slack.API.BaseURL; v != "" {
				slackURL = v
			}
		}
		return cloud.Authority, cloud.GraphBase, slackURL
	}
	lazy := func(label string, pick func() string, onResult func(probeResult)) checklist.Check {
		return checklist.Check{Group: groupNetwork, Label: label, Run: func(ctx context.Context) checklist.Result {
			return reachable(label, pick(), onResult).Run(ctx)
		}}
	}
	checks = append(checks,
		lazy("Microsoft Entra", func() string { a, _, _ := endpoints(); return a }, func(r probeResult) { authority = r }),
		lazy("Microsoft Graph", func() string { _, g, _ := endpoints(); return g + "/v1.0/$metadata" }, nil),
		checklist.Check{Group: groupNetwork, Label: "Slack API", Run: func(ctx context.Context) checklist.Result {
			if m != nil && !m.Source.Slack.API.Enabled {
				return checklist.Skip("Slack API is off")
			}
			_, _, s := endpoints()
			return reachable("Slack API", s+"/api.test", nil).Run(ctx)
		}},
		checklist.Check{Group: groupNetwork, Label: "Clock", Run: func(context.Context) checklist.Result {
			if authority.serverTime.IsZero() {
				return checklist.Skip("could not compare with Microsoft's clock")
			}
			skew := time.Since(authority.serverTime).Abs()
			// Entra rejects certificate assertions outside a five-minute
			// window, so a drifting clock breaks sign-in.
			if skew > 4*time.Minute {
				return checklist.Fail("clock.skew", fmt.Sprintf("this machine is %s off", skew.Round(time.Second)),
					"Sync the system clock (enable automatic date and time); certificate sign-in fails beyond 5 minutes of drift.")
			}
			return checklist.OK(fmt.Sprintf("within %s of Microsoft's", (skew + time.Second).Truncate(time.Second)))
		}},
	)
	if isLocal {
		for _, c := range localChecks(func() *config.Config { return m }) {
			run := c.Run
			c.Run = func(ctx context.Context) checklist.Result {
				if m == nil {
					return checklist.Skip("no config file")
				}
				return run(ctx)
			}
			checks = append(checks, c)
		}
		return checks
	}
	checks = append(checks,
		checklist.Check{Group: groupStack, Label: "Controller", Run: func(ctx context.Context) checklist.Result {
			base := "http://localhost:7070"
			if m != nil && m.Controller.URL != "" {
				base = m.Controller.URL
			}
			r, err := probe(ctx, strings.TrimRight(base, "/")+"/readyz")
			if err != nil {
				return checklist.Skip("not running at " + hostOf(base) + "; needed from `taut migrate`")
			}
			if r.status != http.StatusOK {
				return checklist.Warn(fmt.Sprintf("running at %s but not ready (HTTP %d)", hostOf(base), r.status),
					"One of the controller's dependencies is down. Check `make logs`.")
			}
			return checklist.OK("ready at " + hostOf(base))
		}},
	)
	return checks
}

// resolveErr reports whether a secret reference can be resolved, without
// exposing its value.
func resolveErr(m *config.Config, ref secret.Ref) error {
	_, _, err := m.ResolveSecret(ref)
	return err
}

func newDoctorCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:     "doctor",
		Short:   "Diagnose the environment and connectivity",
		GroupID: groupInspect,
		Long: `Check that this machine can run a migration: the terminal, the config file and
the secrets it references, network paths to Microsoft and Slack, clock drift,
and the controller.

doctor never prints a secret; it only reports whether each one can be found.
It exits 0 unless a check fails.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			io := app.IO
			outcomes := checklist.Run(cmd.Context(), io, "doctor", "", doctorChecks(app), checklist.Options{Verbose: app.Verbose})
			sum := checklist.Summarize(outcomes)
			if io.IsJSON() {
				if err := io.JSON(map[string]any{"ok": sum.Failed == 0, "summary": sum, "checks": outcomes}); err != nil {
					return err
				}
			}
			if sum.Failed > 0 {
				return fail.Exit(fail.ExitFailure, "doctor found problems")
			}
			return nil
		},
	}
}
