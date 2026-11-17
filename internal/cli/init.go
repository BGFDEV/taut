package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/secret"
	"github.com/bgfdev/taut/internal/slack/export"
	"github.com/bgfdev/taut/internal/tui/forms"
	"github.com/bgfdev/taut/internal/tui/theme"
	"github.com/bgfdev/taut/internal/ui"
)

// initAnswers is everything `taut init` needs to know. The wizard and the
// flags both fill this struct, so the two paths cannot drift apart.
type initAnswers struct {
	Name   string
	Export string

	SlackAPI      bool
	SlackTokenEnv string
	SlackAPIURL   string // test override

	TenantID        string
	ClientID        string
	CertFile        string
	ClientSecretEnv string
	Authority       string // test override
	GraphURL        string // test override

	Team           string
	Owner          string
	IncludePrivate bool
	// IncludeDMs migrates direct messages and group DMs into Teams chats.
	// It is a flag only: importing private conversations is not something
	// to agree to in passing while answering a wizard.
	IncludeDMs bool

	UnmappedPolicy  string
	UnmappedAccount string

	// Local writes a config file for local mode: the whole migration in
	// one taut process on this machine. DownloadPostgres lets that process
	// download PostgreSQL when none is installed.
	Local            bool
	DownloadPostgres bool

	Force bool
}

func defaultAnswers() initAnswers {
	return initAnswers{ //nolint:gosec // TAUT_SLACK_TOKEN is the name of a variable, not a credential
		SlackAPI:       true,
		SlackTokenEnv:  "TAUT_SLACK_TOKEN",
		CertFile:       "./taut-app.pem",
		UnmappedPolicy: config.PolicyFormerMember,
	}
}

// build turns answers into a config file.
func (a initAnswers) build() *config.Config {
	m := config.Default()
	m.Metadata.Name = a.Name
	m.Source.Slack.Export = a.Export
	m.Source.Slack.API.Enabled = a.SlackAPI
	if a.SlackAPI {
		m.Source.Slack.API.Token = secret.Ref{Env: a.SlackTokenEnv}
	}
	m.Source.Slack.API.BaseURL = a.SlackAPIURL

	m.Target.Entra.TenantID = a.TenantID
	m.Target.Entra.ClientID = a.ClientID
	m.Target.Entra.Authority = a.Authority
	m.Target.Entra.GraphBaseURL = a.GraphURL
	if a.ClientSecretEnv != "" {
		m.Target.Entra.Credential = config.Credential{ClientSecret: secret.Ref{Env: a.ClientSecretEnv}}
	} else {
		m.Target.Entra.Credential = config.Credential{Certificate: secret.Ref{File: a.CertFile}}
	}

	team := config.DefaultTeam()
	team.Name = a.Team
	team.Description = "Migrated from Slack"
	team.Owner = a.Owner
	if a.IncludePrivate {
		team.Channels.Types = []string{config.ChannelPublic, config.ChannelPrivate}
	}
	m.Target.Teams = []config.TeamSpec{team}
	m.Target.DirectMessages.Enabled = a.IncludeDMs

	m.Identity.Unmapped.Policy = a.UnmappedPolicy
	if a.UnmappedPolicy != config.PolicyBlock {
		m.Identity.Unmapped.Account = a.UnmappedAccount
	}
	if a.Local {
		m.Local.Enabled = true
		m.Local.Postgres.Download = a.DownloadPostgres
	}
	return m
}

var (
	reDNSLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	reGUID     = regexp.MustCompile(`^[0-9a-fA-F]{8}-([0-9a-fA-F]{4}-){3}[0-9a-fA-F]{12}$`)
	reEnvName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	reUPN      = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

// Field validators shared by the wizard. Each message says what to enter,
// not just that the input is wrong.

func validName(s string) error {
	if !reDNSLabel.MatchString(s) {
		return errors.New("use lowercase letters, digits and hyphens, e.g. acme-migration")
	}
	return nil
}

func validGUID(what string) func(string) error {
	return func(s string) error {
		if !reGUID.MatchString(strings.TrimSpace(s)) {
			return fmt.Errorf("enter the %s as a GUID, e.g. 1f2e3d4c-0000-4000-8000-123456789abc", what)
		}
		return nil
	}
}

// validEnvName guards the one place a user might paste a secret: it accepts
// only the *name* of a variable, and says so if it is handed a token.
func validEnvName(s string) error {
	if _, isSecret := secret.LooksLikeSecret(s); isSecret {
		return errors.New("that looks like a secret. Enter the NAME of an environment variable that holds it; Taut never stores secrets in the config file")
	}
	if !reEnvName.MatchString(s) {
		return errors.New("enter an environment variable name, e.g. TAUT_SLACK_TOKEN")
	}
	return nil
}

func validUPN(s string) error {
	if !reUPN.MatchString(strings.TrimSpace(s)) {
		return errors.New("enter a user principal name, e.g. admin@acme.com")
	}
	return nil
}

func validExport(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("enter the path to the Slack export ZIP or extracted folder")
	}
	ex, err := export.Open(expandHome(s))
	if err != nil {
		fe := fail.From(err)
		return errors.New(lowerFirst(fe.What))
	}
	return ex.Close()
}

func validCertPath(s string) error {
	if _, isSecret := secret.LooksLikeSecret(s); isSecret {
		return errors.New("that looks like key material. Enter the PATH to the PEM file instead")
	}
	if strings.TrimSpace(s) == "" {
		return errors.New("enter the path to the PEM file holding the certificate and private key")
	}
	return nil
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return p
}

// wizard runs the interactive form, filling a.
func wizard(ctx context.Context, io *ui.IO, a *initAnswers) error {
	credKind := "certificate"
	if a.ClientSecretEnv != "" {
		credKind = "secret"
	}
	if a.ClientSecretEnv == "" {
		a.ClientSecretEnv = "TAUT_ENTRA_CLIENT_SECRET"
	}
	teamTouched := a.Team != ""

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewNote().
				Title("New migration").
				Description("This writes a config file: the plan Taut follows.\nIt holds no secrets, only where to find them, so it is safe to commit."),
			huh.NewInput().Title("Migration name").
				Description("A short identifier for this run.").
				Placeholder("acme-migration").
				Value(&a.Name).Validate(validName),
			huh.NewInput().Title("Slack export").
				Description("Path to the export ZIP, or the folder it was extracted to.").
				Placeholder("./slack-export.zip").
				Value(&a.Export).Validate(validExport),
		).Title("Source"),

		huh.NewGroup(
			huh.NewSelect[bool]().Title("Where will the migration run?").
				Description("Local mode needs nothing deployed and suits a small workspace. A cluster runs many workers and keeps credentials in a separate process.").
				Options(
					huh.NewOption("On this machine, in this one program (local mode)", true),
					huh.NewOption("On a cluster or the Compose stack, behind a controller", false),
				).Value(&a.Local),
		).Title("Where it runs"),

		huh.NewGroup(
			huh.NewConfirm().Title("Download PostgreSQL if none is installed?").
				Description("Local mode keeps its ledger in a PostgreSQL server it starts and stops itself. If none is installed on this machine, taut can download one (15 to 30 MB, from Maven Central) the first time it is needed. Nothing is downloaded otherwise.").
				Affirmative("Allow the download").Negative("No, I will install PostgreSQL").Value(&a.DownloadPostgres),
		).Title("Where it runs").WithHideFunc(func() bool { return !a.Local }),

		huh.NewGroup(
			huh.NewConfirm().Title("Also read from the Slack API?").
				Description("Recommended. The export has no file contents and may lack emails;\nthe API fills those in. Needs a read-only Slack app (`taut auth --slack-manifest`).").
				Affirmative("Yes").Negative("Export only").
				Value(&a.SlackAPI),
			huh.NewInput().Title("Environment variable holding the Slack token").
				Description("The variable's name, not the token.").
				Value(&a.SlackTokenEnv).
				Validate(func(s string) error {
					if !a.SlackAPI {
						return nil
					}
					return validEnvName(s)
				}),
		).Title("Slack API"),

		huh.NewGroup(
			huh.NewInput().Title("Tenant ID").
				Description("Microsoft Entra → Overview → Tenant ID. scripts/entra-app.sh prints it.").
				Value(&a.TenantID).Validate(validGUID("tenant ID")),
			huh.NewInput().Title("Application (client) ID").
				Description("The app registration Taut signs in as.").
				Value(&a.ClientID).Validate(validGUID("client ID")),
			huh.NewSelect[string]().Title("How does the app authenticate?").
				Options(
					huh.NewOption("Certificate (recommended)", "certificate"),
					huh.NewOption("Client secret", "secret"),
				).Value(&credKind),
		).Title("Microsoft Entra"),

		huh.NewGroup(
			huh.NewInput().Title("Certificate file").
				Description("PEM file with the certificate and its private key.").
				Value(&a.CertFile).Validate(validCertPath),
		).WithHideFunc(func() bool { return credKind != "certificate" }),

		huh.NewGroup(
			huh.NewInput().Title("Environment variable holding the client secret").
				Description("The variable's name, not the secret.").
				Value(&a.ClientSecretEnv).Validate(validEnvName),
		).WithHideFunc(func() bool { return credKind != "secret" }),

		huh.NewGroup(
			huh.NewInput().Title("Team name").
				Description("Slack channels become channels of this team. Split into more teams later with `taut map`.").
				Placeholder("Acme").
				Value(&a.Team).Validate(huh.ValidateNotEmpty()),
			huh.NewInput().Title("Team owner").
				Description("User principal name of the person who owns the team during migration.").
				Placeholder("admin@acme.com").
				Value(&a.Owner).Validate(validUPN),
			huh.NewConfirm().Title("Include private channels?").
				Description("Needs an all-conversations export and the ChannelMember.ReadWrite.All permission.").
				Affirmative("Yes").Negative("Public only").
				Value(&a.IncludePrivate),
		).Title("Teams"),

		huh.NewGroup(
			huh.NewSelect[string]().Title("Messages from people with no Entra account").
				Description("Deactivated users, guests and bots have nobody to map to.").
				Options(
					huh.NewOption("Attribute to a \"Former member\" account, keeping the original name in the message", config.PolicyFormerMember),
					huh.NewOption("Attribute to the migration service account", config.PolicyServiceAccount),
					huh.NewOption("Stop: every author must be mapped before migrating", config.PolicyBlock),
				).Value(&a.UnmappedPolicy),
		).Title("People"),

		huh.NewGroup(
			huh.NewInput().Title("Account to attribute those messages to").
				Description("User principal name of an existing Entra user.").
				Placeholder("former-member@acme.com").
				Value(&a.UnmappedAccount).Validate(validUPN),
		).WithHideFunc(func() bool { return a.UnmappedPolicy == config.PolicyBlock }),
	).
		WithTheme(forms.Theme(io.Theme)).
		WithInput(io.In).
		WithShowHelp(true)

	if err := form.RunWithContext(ctx); err != nil {
		if errors.Is(err, huh.ErrUserAborted) || ctx.Err() != nil {
			return fail.New("init.cancelled", "Cancelled; no config file was written", "", "Run `taut init` again when ready.")
		}
		return err
	}

	if credKind == "certificate" {
		a.ClientSecretEnv = ""
	} else {
		a.CertFile = ""
	}
	if !teamTouched && a.Team == "" {
		a.Team = a.Name
	}
	a.Export = strings.TrimSpace(a.Export)
	a.TenantID, a.ClientID = strings.TrimSpace(a.TenantID), strings.TrimSpace(a.ClientID)
	return nil
}

func newInitCmd(app *App) *cobra.Command {
	a := defaultAnswers()
	var noSlackAPI bool

	cmd := &cobra.Command{
		Use:     "init",
		Short:   "Write a config file with a guided wizard",
		GroupID: groupPlan,
		Long: `Write a config file: the declarative plan for a migration.

On a terminal, init asks a few questions. With --yes (or when piped) it builds
the config file from flags alone, which is how CI uses it.

The config file never contains secrets. Where a credential is needed, init asks
for the name of an environment variable or the path of a file instead.`,
		Example: `  taut init
  taut init --yes --name acme --export ./export.zip \
    --tenant-id <guid> --client-id <guid> --cert-file ./taut-app.pem \
    --team Acme --owner admin@acme.com --unmapped-account former@acme.com`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			io := app.IO
			if noSlackAPI {
				a.SlackAPI = false
			}
			path := app.ConfigPath

			if _, err := os.Stat(path); err == nil && !a.Force {
				return fail.New("config.exists",
					fmt.Sprintf("%s already exists", path),
					"init will not overwrite a config file unless told to.",
					"Pass --force to replace it, or --config <path> to write somewhere else.").WithExit(fail.ExitUsage)
			}

			if io.Interactive() {
				if err := wizard(cmd.Context(), io, &a); err != nil {
					return err
				}
			}

			m := a.build()
			m.SetBaseDir(filepath.Dir(path))
			issues := m.Validate()
			if issues.HasErrors() {
				if !io.Interactive() {
					fe := fail.From(issues.Err())
					fe.Next = "Supply the missing values as flags (see `taut init --help`), or run `taut init` on a terminal for the wizard."
					return fe
				}
				return issues.Err()
			}
			if err := m.Write(path, a.Force); err != nil {
				return err
			}

			refs := secretRefs(m)
			if io.IsJSON() {
				return io.JSON(map[string]any{
					"config":   path,
					"name":     m.Metadata.Name,
					"warnings": issues.Warnings(),
					"secrets":  refs,
					"next":     "taut auth",
				})
			}

			t := io.Theme
			w := io.Width - ui.Margin
			var b strings.Builder
			b.WriteString("\n" + ui.Verdict(t, theme.StatusOK, "Wrote "+path))
			for _, warn := range issues.Warnings() {
				b.WriteString("\n" + ui.Finding(t, theme.StatusWarn, warn.Path+": "+warn.Message, "", w))
			}
			if len(refs) > 0 {
				b.WriteString("\n\n" + ui.Heading(t, "Secrets to set before the next step", w) + "\n")
				rows := make([][2]string, len(refs))
				for i, r := range refs {
					rows[i] = [2]string{r.Purpose, r.Where}
				}
				b.WriteString(ui.Indent(ui.KV(t, rows), 2))
			}
			if m.Local.Enabled {
				pg := "an installed PostgreSQL is used; none is downloaded"
				if m.Local.Postgres.Download {
					pg = "an installed PostgreSQL is used, or one is downloaded once from Maven Central"
				}
				b.WriteString("\n\n" + ui.Heading(t, "Local mode", w) + "\n")
				b.WriteString(ui.Indent(ui.KV(t, [][2]string{
					{"State", m.StateDir() + "  the ledger, staged data and their key; keep it private and out of version control"},
					{"Ledger", pg},
					{"Isolation", "one process: credentials are not separated from the worker as they are on a cluster"},
				}), 2))
			}
			b.WriteString("\n\n" + ui.Next(t, "taut auth", "verify credentials and permissions"))
			io.Println(ui.Pad(b.String()))
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&a.Name, "name", "", "migration name (lowercase letters, digits, hyphens)")
	f.StringVar(&a.Export, "export", "", "path to the Slack export ZIP or extracted folder")
	f.BoolVar(&noSlackAPI, "no-slack-api", false, "read from the export only")
	f.StringVar(&a.SlackTokenEnv, "slack-token-env", a.SlackTokenEnv, "environment variable that holds the Slack token")
	f.StringVar(&a.TenantID, "tenant-id", "", "Microsoft Entra tenant ID")
	f.StringVar(&a.ClientID, "client-id", "", "application (client) ID of the app registration")
	f.StringVar(&a.CertFile, "cert-file", a.CertFile, "PEM file with the app's certificate and private key")
	f.StringVar(&a.ClientSecretEnv, "client-secret-env", "", "environment variable that holds a client secret (instead of a certificate)")
	f.StringVar(&a.Team, "team", "", "name of the team to create")
	f.StringVar(&a.Owner, "owner", "", "user principal name of the team owner")
	f.BoolVar(&a.IncludePrivate, "include-private", false, "include private channels")
	f.BoolVar(&a.IncludeDMs, "include-direct-messages", false, "import direct messages and group DMs into Teams chats (global cloud, modern strategy)")
	f.StringVar(&a.UnmappedPolicy, "unmapped-policy", a.UnmappedPolicy, "former-member, service-account or block")
	f.StringVar(&a.UnmappedAccount, "unmapped-account", "", "user principal name that unmapped authors are attributed to")
	f.BoolVar(&a.Local, "local", false, "write a config file for local mode: one taut process on this machine, nothing to deploy")
	f.BoolVar(&a.DownloadPostgres, "download-postgres", false, "with --local: let taut download PostgreSQL if none is installed")
	f.BoolVar(&a.Force, "force", false, "overwrite an existing config file")

	// Endpoint overrides exist for tests and the local fake servers.
	f.StringVar(&a.Authority, "authority", "", "override the Entra authority URL")
	f.StringVar(&a.GraphURL, "graph-url", "", "override the Microsoft Graph base URL")
	f.StringVar(&a.SlackAPIURL, "slack-api-url", "", "override the Slack API base URL")
	for _, name := range []string{"authority", "graph-url", "slack-api-url"} {
		_ = f.MarkHidden(name)
	}
	return cmd
}

// secretRef describes one secret the config file points at, without its value.
type secretRef struct {
	Purpose string `json:"purpose"`
	Where   string `json:"where"`
}

// secretRefs lists the secrets needed for the plan stage (auth and preflight).
func secretRefs(m *config.Config) []secretRef {
	var out []secretRef
	if m.Source.Slack.API.Enabled && !m.Source.Slack.API.Token.IsZero() {
		out = append(out, secretRef{"Slack token", m.Source.Slack.API.Token.Describe()})
	}
	c := m.Target.Entra.Credential
	if !c.Certificate.IsZero() {
		out = append(out, secretRef{"Entra certificate", c.Certificate.Describe()})
	}
	if !c.ClientSecret.IsZero() {
		out = append(out, secretRef{"Entra client secret", c.ClientSecret.Describe()})
	}
	return out
}
