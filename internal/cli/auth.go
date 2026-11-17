package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/graph"
	"github.com/bgfdev/taut/internal/slackapi"
	"github.com/bgfdev/taut/internal/tui/checklist"
	"github.com/bgfdev/taut/internal/tui/theme"
	"github.com/bgfdev/taut/internal/ui"
)

const (
	groupSlack = "Slack"
	groupEntra = "Microsoft Entra"
)

// failed converts an error into a failing check result, keeping its code
// and next step.
func failed(err error) checklist.Result {
	fe := fail.From(err)
	detail := fe.What
	if fe.Why != "" {
		detail += ": " + fe.Why
	}
	return checklist.Fail(fe.Code, detail, fe.Next)
}

// authState carries what one check learns to the checks after it.
type authState struct {
	m *config.Config

	slack       *slackapi.Client
	slackID     *slackapi.Identity
	slackScopes []slackapi.ScopeStatus

	cred   graph.Credential
	graph  *graphSession
	claims graph.Claims
	perms  []graph.PermissionStatus

	permissionGap bool
}

func authChecks(m *config.Config) ([]checklist.Check, *authState) {
	st := &authState{m: m}
	private, dms := features(m)
	var checks []checklist.Check

	// --- Slack ---------------------------------------------------------
	if !m.Source.Slack.API.Enabled {
		checks = append(checks, checklist.Check{Group: groupSlack, Label: "Slack API", Run: func(context.Context) checklist.Result {
			return checklist.Skip("off in the config file; reading from the export only")
		}})
	} else {
		checks = append(checks,
			checklist.Check{Group: groupSlack, Label: "Token found", Run: func(context.Context) checklist.Result {
				c, warns, err := slackClient(m)
				if err != nil {
					return failed(err)
				}
				st.slack = c
				where := m.Source.Slack.API.Token.Describe()
				if len(warns) > 0 {
					return checklist.Warn(where+"; "+warns[0].Message, warns[0].Next)
				}
				return checklist.OK(where)
			}},
			checklist.Check{Group: groupSlack, Label: "Token accepted", Run: func(ctx context.Context) checklist.Result {
				if st.slack == nil {
					return checklist.Skip("no token")
				}
				id, err := st.slack.AuthTest(ctx)
				if err != nil {
					if fail.ExitCode(err) == fail.ExitPermission {
						st.permissionGap = true
					}
					return failed(err)
				}
				st.slackID = id
				return checklist.OK(fmt.Sprintf("%s workspace, %s token", id.Team, id.Kind))
			}},
			checklist.Check{
				Group: groupSlack, Label: "Scopes",
				Run: func(context.Context) checklist.Result {
					if st.slackID == nil {
						return checklist.Skip("not signed in")
					}
					st.slackScopes = slackapi.Compare(st.slackID.Scopes, slackapi.Features{PrivateChannels: private, DirectMessages: dms})
					need, have := 0, 0
					for _, s := range st.slackScopes {
						if s.Required {
							need++
							if s.Granted {
								have++
							}
						}
					}
					if have < need {
						st.permissionGap = true
						return checklist.Fail("slack.missing_scope",
							fmt.Sprintf("%d of %d required granted", have, need),
							"Run `taut auth --slack-manifest`, update the Slack app with the scopes it lists, reinstall it, then run `taut auth` again.")
					}
					return checklist.OK(fmt.Sprintf("%d of %d required granted", have, need))
				},
				Expand: func() []checklist.Outcome {
					var out []checklist.Outcome
					for _, s := range st.slackScopes {
						o := checklist.Outcome{Label: s.Name}
						switch {
						case s.Granted && s.Required:
							o.Result = checklist.OK(s.Why)
						case s.Required:
							o.Result = checklist.Fail("slack.missing_scope", s.Why, "")
						case s.Granted:
							o.Result = checklist.Info("granted, not needed (" + s.Feature + " off)")
						default:
							o.Result = checklist.Skip("not needed (" + s.Feature + " off)")
						}
						out = append(out, o)
					}
					return out
				},
			},
		)
	}

	// --- Microsoft Entra -----------------------------------------------
	checks = append(checks,
		checklist.Check{Group: groupEntra, Label: "Credential", Run: func(context.Context) checklist.Result {
			cred, warns, err := entraCredential(m)
			if err != nil {
				return failed(err)
			}
			st.cred = cred
			if cert, ok := cred.(*graph.ClientCertificate); ok {
				days := int(time.Until(cert.NotAfter()).Hours() / 24)
				detail := fmt.Sprintf("certificate %s, expires in %d days", shortThumb(cert.Thumbprint()), days)
				if days < 30 {
					return checklist.Warn(detail, "Create a new certificate and upload it to the app registration before this one expires (scripts/entra-app.sh).")
				}
				return checklist.OK(detail)
			}
			next := "Prefer a certificate: run scripts/entra-app.sh, then set target.entra.credential.certificate in the config file."
			if len(warns) > 0 {
				return checklist.Warn("client secret; "+warns[0].Message, warns[0].Next)
			}
			return checklist.Warn("client secret (a certificate is safer)", next)
		}},
		checklist.Check{Group: groupEntra, Label: "Signed in", Run: func(ctx context.Context) checklist.Result {
			if st.cred == nil {
				return checklist.Skip("no credential")
			}
			sess, err := connectGraph(m, st.cred)
			if err != nil {
				return failed(err)
			}
			tok, err := sess.Tokens.Token(ctx)
			if err != nil {
				if fail.ExitCode(err) == fail.ExitPermission {
					st.permissionGap = true
				}
				return failed(err)
			}
			claims, err := graph.ParseClaims(tok)
			if err != nil {
				return failed(err)
			}
			st.graph, st.claims = sess, claims
			return checklist.OK(fmt.Sprintf("tenant %s, %s cloud", claims.TenantID, sess.Cloud.Name))
		}},
		checklist.Check{
			Group: groupEntra, Label: "Permissions",
			Run: func(context.Context) checklist.Result {
				if st.graph == nil {
					return checklist.Skip("not signed in")
				}
				st.perms = graph.Compare(st.claims.Roles, graph.FeatureSet{PrivateChannels: private, DirectMessages: dms})
				missing := graph.MissingRequired(st.perms)
				need := 0
				for _, p := range st.perms {
					if p.State == graph.PermissionGranted && p.Requirement != graph.Recommended || p.State == graph.PermissionMissingRequired {
						need++
					}
				}
				if len(missing) > 0 {
					st.permissionGap = true
					return checklist.Fail("graph.permission_missing",
						fmt.Sprintf("%d of %d required granted", need-len(missing), need),
						"Add the missing application permissions to the app registration and grant admin consent (scripts/entra-app.sh does both), then run `taut auth` again.")
				}
				return checklist.OK(fmt.Sprintf("%d of %d required granted", need, need))
			},
			Expand: func() []checklist.Outcome {
				var out []checklist.Outcome
				for _, p := range st.perms {
					o := checklist.Outcome{Label: p.Name}
					switch p.State {
					case graph.PermissionGranted:
						o.Result = checklist.OK(p.Purpose)
					case graph.PermissionMissingRequired:
						o.Result = checklist.Fail("graph.permission_missing", p.Purpose, "")
					case graph.PermissionMissingOptional:
						o.Result = checklist.Result{Status: theme.StatusWarn, Detail: "recommended: " + p.Purpose}
					default:
						if p.Granted {
							o.Result = checklist.Info("granted, not needed for this config file")
						} else {
							o.Result = checklist.Skip("not needed for this config file")
						}
					}
					out = append(out, o)
				}
				return out
			},
		},
		checklist.Check{Group: groupEntra, Label: "Directory", Run: func(ctx context.Context) checklist.Result {
			if st.graph == nil {
				return checklist.Skip("not signed in")
			}
			for _, err := range st.graph.Client.Users(ctx, graph.UserQuery{PageSize: 1}) {
				if err != nil {
					if fail.ExitCode(err) == fail.ExitPermission {
						st.permissionGap = true
					}
					return failed(err)
				}
				break
			}
			org, err := st.graph.Client.Organization(ctx)
			if err != nil {
				// Reading the organization name needs a permission Taut
				// only recommends; the directory itself is readable.
				return checklist.OK("users can be listed")
			}
			return checklist.OK(org.DisplayName)
		}},
	)
	return checks, st
}

func shortThumb(t string) string {
	if len(t) > 8 {
		return t[:8]
	}
	return t
}

func newAuthCmd(app *App) *cobra.Command {
	var slackManifest bool
	cmd := &cobra.Command{
		Use:     "auth",
		Short:   "Verify Slack and Microsoft credentials and permissions",
		GroupID: groupPlan,
		Long: `Connect to Slack and Microsoft Entra with the credentials the config file
references, and compare what each granted against what Taut needs.

auth is read-only. It exits 0 when everything required is in place, 4 when a
permission or scope is missing, and 1 for any other failure.`,
		Example: `  taut auth
  taut auth --json | jq '.checks[] | select(.status == "failed")'
  taut auth --slack-manifest > slack-app.yaml`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			io := app.IO
			if slackManifest {
				f := slackapi.Features{}
				if m, err := config.Load(app.ConfigPath); err == nil {
					f.PrivateChannels, f.DirectMessages = features(m)
				}
				if io.IsJSON() {
					return io.JSON(map[string]any{"slackAppManifest": slackapi.AppManifest(f), "scopes": slackapi.Required(f)})
				}
				_, err := fmt.Fprint(io.Out, slackapi.AppManifest(f))
				return err
			}

			m, err := app.loadConfig()
			if err != nil {
				return err
			}
			checks, st := authChecks(m)
			outcomes := checklist.Run(cmd.Context(), io, "auth", m.Metadata.Name, checks, checklist.Options{Verbose: app.Verbose})
			sum := checklist.Summarize(outcomes)

			if io.IsJSON() {
				if err := io.JSON(map[string]any{
					"config":  m.Metadata.Name,
					"ok":      sum.Failed == 0,
					"summary": sum,
					"checks":  outcomes,
				}); err != nil {
					return err
				}
			} else if sum.Failed == 0 {
				t := io.Theme
				io.Println()
				io.Println(ui.Pad(ui.Next(t, "taut preflight", "check what will and won't migrate")))
			}

			switch {
			case sum.Failed == 0:
				return nil
			case st.permissionGap:
				return fail.Exit(fail.ExitPermission, "auth found missing permissions")
			default:
				return fail.Exit(fail.ExitFailure, "auth found problems")
			}
		},
	}
	cmd.Flags().BoolVar(&slackManifest, "slack-manifest", false, "print the Slack app manifest that grants the scopes Taut needs")
	return cmd
}
