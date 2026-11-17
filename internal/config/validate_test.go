package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/secret"
)

func TestValidateRules(t *testing.T) {
	secondTeam := func(m *Config) *TeamSpec {
		t2 := DefaultTeam()
		t2.Name, t2.Owner = "Second", "owner@acme.com"
		t2.Channels.Include = []string{"eng-*"}
		m.Target.Teams = append(m.Target.Teams, t2)
		return &m.Target.Teams[1]
	}
	const team0 = "target.teams[0]"

	tests := []struct {
		name   string
		mutate func(m *Config)
		path   string
		sev    Severity
		msg    string
	}{
		// Header.
		{"apiVersion required", func(m *Config) { m.APIVersion = "" }, "apiVersion", SeverityError, "is required"},
		{"apiVersion unsupported", func(m *Config) { m.APIVersion = "taut.dev/v9" }, "apiVersion", SeverityError, "not supported"},
		{"kind wrong", func(m *Config) { m.Kind = "Plan" }, "kind", SeverityError, "use Config"},
		{"name required", func(m *Config) { m.Metadata.Name = "" }, "metadata.name", SeverityError, "is required"},
		{"name uppercase", func(m *Config) { m.Metadata.Name = "Acme" }, "metadata.name", SeverityError, "lowercase"},
		{"name leading hyphen", func(m *Config) { m.Metadata.Name = "-acme" }, "metadata.name", SeverityError, "not a valid name"},
		{"name underscore", func(m *Config) { m.Metadata.Name = "acme_migration" }, "metadata.name", SeverityError, "not a valid name"},
		{"name too long", func(m *Config) { m.Metadata.Name = strings.Repeat("a", 64) }, "metadata.name", SeverityError, "1–63"},

		// Source.
		{"export required", func(m *Config) { m.Source.Slack.Export = "" }, "source.slack.export", SeverityError, "is required"},
		{"api enabled without token", func(m *Config) { m.Source.Slack.API.Token = secret.Ref{} }, "source.slack.api.token", SeverityError, "required when api.enabled is true"},
		{"api token both env and file", func(m *Config) { m.Source.Slack.API.Token.File = "./tok" }, "source.slack.api.token", SeverityError, "exactly one"},
		{"api token bad env name", func(m *Config) { m.Source.Slack.API.Token.Env = "my token" }, "source.slack.api.token", SeverityError, "not a valid variable name"},
		{"api baseURL shape", func(m *Config) { m.Source.Slack.API.BaseURL = "slack.test" }, "source.slack.api.baseURL", SeverityError, "not a valid URL"},

		// Entra.
		{"tenantId required", func(m *Config) { m.Target.Entra.TenantID = "" }, "target.entra.tenantId", SeverityError, "is required"},
		{"tenantId not a GUID", func(m *Config) { m.Target.Entra.TenantID = "acme.onmicrosoft.com" }, "target.entra.tenantId", SeverityError, "is not a GUID"},
		{"tenantId placeholder", func(m *Config) { m.Target.Entra.TenantID = zeroGUID }, "target.entra.tenantId", SeverityWarning, "placeholder"},
		{"clientId not a GUID", func(m *Config) { m.Target.Entra.ClientID = "3f2504e0-4f89-11d3-9a0c" }, "target.entra.clientId", SeverityError, "is not a GUID"},
		{"cloud enum", func(m *Config) { m.Target.Entra.Cloud = "usgv" }, "target.entra.cloud", SeverityError, `did you mean "usgov"?`},
		{"cloud required", func(m *Config) { m.Target.Entra.Cloud = "" }, "target.entra.cloud", SeverityError, "is required"},
		{"credential missing", func(m *Config) { m.Target.Entra.Credential = Credential{} }, "target.entra.credential", SeverityError, "exactly one"},
		{"credential both", func(m *Config) {
			m.Target.Entra.Credential.ClientSecret = secret.Ref{Env: "TAUT_ENTRA_CLIENT_SECRET"}
		}, "target.entra.credential", SeverityError, "sets both"},
		{"client secret warns", func(m *Config) {
			m.Target.Entra.Credential = Credential{ClientSecret: secret.Ref{Env: "TAUT_ENTRA_CLIENT_SECRET"}}
		}, "target.entra.credential.clientSecret", SeverityWarning, "certificate is preferred"},
		{"certificate ref invalid", func(m *Config) { m.Target.Entra.Credential.Certificate = secret.Ref{Env: "A", File: "b"} }, "target.entra.credential.certificate", SeverityError, "exactly one"},
		{"authority shape", func(m *Config) { m.Target.Entra.Authority = "ftp://login.test" }, "target.entra.authority", SeverityError, "not a valid URL"},
		{"graphBaseURL credentials", func(m *Config) { m.Target.Entra.GraphBaseURL = "https://user:pw@graph.test" }, "target.entra.graphBaseURL", SeverityError, "must not embed credentials"},

		// Teams.
		{"no teams", func(m *Config) { m.Target.Teams = nil }, "target.teams", SeverityError, "at least one team"},
		{"team name required", func(m *Config) { m.Target.Teams[0].Name = " " }, team0 + ".name", SeverityError, "is required"},
		{"duplicate team names", func(m *Config) { secondTeam(m).Name = "ACME" }, "target.teams[1].name", SeverityError, "also the name of target.teams[0]"},
		{"owner required", func(m *Config) { m.Target.Teams[0].Owner = "" }, team0 + ".owner", SeverityError, "is required"},
		{"owner not a UPN", func(m *Config) { m.Target.Teams[0].Owner = "admin" }, team0 + ".owner", SeverityError, "user principal name"},
		{"owner without domain suffix", func(m *Config) { m.Target.Teams[0].Owner = "admin@acme" }, team0 + ".owner", SeverityError, "user principal name"},
		{"visibility enum", func(m *Config) { m.Target.Teams[0].Visibility = "secret" }, team0 + ".visibility", SeverityError, "private, public"},
		{"include empty", func(m *Config) { m.Target.Teams[0].Channels.Include = nil }, team0 + ".channels.include", SeverityError, "no channels"},
		{"include bad glob", func(m *Config) { m.Target.Teams[0].Channels.Include = []string{"ok", "[a-"} }, team0 + ".channels.include[1]", SeverityError, "not a valid glob"},
		{"exclude bad glob", func(m *Config) { m.Target.Teams[0].Channels.Exclude = []string{`bad\`} }, team0 + ".channels.exclude[0]", SeverityError, "not a valid glob"},
		{"exclude empty pattern", func(m *Config) { m.Target.Teams[0].Channels.Exclude = []string{""} }, team0 + ".channels.exclude[0]", SeverityError, "is empty"},
		{"types empty", func(m *Config) { m.Target.Teams[0].Channels.Types = nil }, team0 + ".channels.types", SeverityError, "is empty"},
		{"types enum", func(m *Config) { m.Target.Teams[0].Channels.Types = []string{"public", "shared"} }, team0 + ".channels.types[1]", SeverityError, "public, private"},
		{"types duplicate", func(m *Config) { m.Target.Teams[0].Channels.Types = []string{"public", "public"} }, team0 + ".channels.types[1]", SeverityWarning, "more than once"},
		{"rename empty target", func(m *Config) { m.Target.Teams[0].Channels.Rename["x"] = "" }, team0 + ".channels.rename.x", SeverityError, "no new name"},
		{"rename collision", func(m *Config) { m.Target.Teams[0].Channels.Rename["other"] = "new name" }, team0 + ".channels.rename.other", SeverityError, "must be unique"},
		{"same include in two teams", func(m *Config) { secondTeam(m).Channels.Include = []string{"eng-*", "*"} }, "target.teams[1].channels.include[1]", SeverityWarning, "also included by target.teams[0]"},

		// Strategy interplay.
		{"legacy with private channels", func(m *Config) {
			m.Load.Strategy = StrategyLegacy
			m.Target.Teams[0].Channels.Types = []string{ChannelPublic, ChannelPrivate}
		}, team0 + ".channels.types", SeverityError, "standard (public) channels only"},
		{"modern outside the global cloud", func(m *Config) { m.Target.Entra.Cloud = CloudUSGovHigh }, "load.strategy", SeverityError, "startMigration"},
		{"strategy enum", func(m *Config) { m.Load.Strategy = "modren" }, "load.strategy", SeverityError, `did you mean "modern"?`},
		{"direct messages with the legacy strategy", func(m *Config) {
			m.Load.Strategy, m.Target.DirectMessages.Enabled = StrategyLegacy, true
		}, "target.directMessages.enabled", SeverityError, "set load.strategy to modern"},
		{"direct messages in a national cloud", func(m *Config) {
			m.Load.Strategy, m.Target.Entra.Cloud, m.Target.DirectMessages.Enabled = StrategyLegacy, CloudUSGovHigh, true
		}, "target.directMessages.enabled", SeverityError, "exists only in the global cloud"},
		{"direct messages in the China cloud", func(m *Config) {
			m.Load.Strategy, m.Target.Entra.Cloud, m.Target.DirectMessages.Enabled = StrategyLegacy, CloudChina, true
		}, "target.directMessages.enabled", SeverityError, "Set target.directMessages.enabled to false"},

		// Identity.
		{"policy enum", func(m *Config) { m.Identity.Unmapped.Policy = "skip" }, "identity.unmapped.policy", SeverityError, "former-member, service-account, block"},
		{"account required", func(m *Config) { m.Identity.Unmapped.Account = "" }, "identity.unmapped.account", SeverityError, "required when the policy is former-member"},
		{"account required for service-account", func(m *Config) {
			m.Identity.Unmapped = Unmapped{Policy: PolicyServiceAccount}
		}, "identity.unmapped.account", SeverityError, "required when the policy is service-account"},
		{"account not a UPN", func(m *Config) { m.Identity.Unmapped.Account = "former member" }, "identity.unmapped.account", SeverityError, "user principal name"},
		{"account ignored under block", func(m *Config) { m.Identity.Unmapped.Policy = PolicyBlock }, "identity.unmapped.account", SeverityWarning, "ignored"},

		// Content.
		{"reactions enum", func(m *Config) { m.Content.Reactions = "emoji" }, "content.reactions", SeverityError, "footer, native, drop"},
		{"customEmoji enum", func(m *Config) { m.Content.CustomEmoji = "img" }, "content.customEmoji", SeverityError, "use one of: text"},
		{"customEmoji image", func(m *Config) { m.Content.CustomEmoji = "image" }, "content.customEmoji", SeverityError, "cannot be image: a Slack export holds no emoji images"},
		{"customEmoji required", func(m *Config) { m.Content.CustomEmoji = "" }, "content.customEmoji", SeverityError, "is required"},
		{"since format", func(m *Config) { m.Content.Since = "01/02/2020" }, "content.since", SeverityError, "YYYY-MM-DD"},
		{"until impossible date", func(m *Config) { m.Content.Until = "2021-02-30" }, "content.until", SeverityError, "not a valid date"},
		{"since after until", func(m *Config) { m.Content.Until = "2019-12-31" }, "content.until", SeverityError, "before content.since"},

		// Staging and the controller.
		{"staging endpoint required", func(m *Config) { m.Staging.Endpoint = "" }, "staging.endpoint", SeverityError, "is required"},
		{"staging endpoint shape", func(m *Config) { m.Staging.Endpoint = "localhost:9000" }, "staging.endpoint", SeverityError, "not a valid URL"},
		{"staging region required", func(m *Config) { m.Staging.Region = "" }, "staging.region", SeverityError, "is required"},
		{"staging bucket required", func(m *Config) { m.Staging.Bucket = "" }, "staging.bucket", SeverityError, "is required"},
		{"staging bucket shape", func(m *Config) { m.Staging.Bucket = "Taut_Staging" }, "staging.bucket", SeverityError, "not a valid bucket name"},
		{"staging accessKey required", func(m *Config) { m.Staging.AccessKey = secret.Ref{} }, "staging.accessKey", SeverityError, "is required"},
		{"staging secretKey invalid", func(m *Config) { m.Staging.SecretKey = secret.Ref{Env: "9X"} }, "staging.secretKey", SeverityError, "not a valid variable name"},
		{"staging encryptionKey required", func(m *Config) { m.Staging.EncryptionKey = secret.Ref{} }, "staging.encryptionKey", SeverityError, "is required"},
		{"controller url required", func(m *Config) { m.Controller.URL = "" }, "controller.url", SeverityError, "is required"},
		{"controller url shape", func(m *Config) { m.Controller.URL = "controller" }, "controller.url", SeverityError, "not a valid URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := example(t)
			tt.mutate(m)
			issues := m.Validate()
			got, ok := find(issues, tt.path)
			if !ok {
				t.Fatalf("no issue at %s:%s", tt.path, dump(issues))
			}
			if got.Severity != tt.sev {
				t.Errorf("severity = %s, want %s (%s)", got.Severity, tt.sev, got.Message)
			}
			if !strings.Contains(got.Message, tt.msg) {
				t.Errorf("message %q does not contain %q", got.Message, tt.msg)
			}
			if len(issues) != 1 {
				t.Errorf("want exactly the one issue, got:%s", dump(issues))
			}
			if issues.HasErrors() != (tt.sev == SeverityError) {
				t.Errorf("HasErrors() = %v with:%s", issues.HasErrors(), dump(issues))
			}
		})
	}
}

func TestValidateAcceptedVariants(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(m *Config)
	}{
		{"legacy with public channels", func(m *Config) { m.Load.Strategy = StrategyLegacy }},
		{"direct messages with the modern strategy", func(m *Config) { m.Target.DirectMessages.Enabled = true }},
		{"direct messages off in a national cloud", func(m *Config) {
			m.Load.Strategy, m.Target.Entra.Cloud = StrategyLegacy, CloudDoD
		}},
		{"legacy in a national cloud", func(m *Config) {
			m.Load.Strategy, m.Target.Entra.Cloud = StrategyLegacy, CloudDoD
		}},
		{"modern with private channels", func(m *Config) {
			m.Target.Teams[0].Channels.Types = []string{ChannelPublic, ChannelPrivate}
		}},
		{"block without account", func(m *Config) { m.Identity.Unmapped = Unmapped{Policy: PolicyBlock} }},
		{"matchers on", func(m *Config) { m.Identity.Match = Match{EmailLocalPart: true, DisplayName: true} }},
		{"native reactions", func(m *Config) { m.Content.Reactions = ReactionsNative }},
		{"api disabled without token", func(m *Config) { m.Source.Slack.API = SlackAPI{} }},
		{"same day range", func(m *Config) { m.Content.Until = m.Content.Since }},
		{"until only", func(m *Config) { m.Content.Since, m.Content.Until = "", "2024-12-31" }},
		{"test overrides", func(m *Config) {
			m.Source.Slack.API.BaseURL = "http://127.0.0.1:8081/api"
			m.Target.Entra.Authority = "http://127.0.0.1:8082"
			m.Target.Entra.GraphBaseURL = "https://graph.test/v1.0"
		}},
		{"absolute secret file", func(m *Config) { m.Source.Slack.API.Token = secret.Ref{File: "/run/secrets/slack-token"} }},
		{"two disjoint teams", func(m *Config) {
			t2 := DefaultTeam()
			t2.Name, t2.Owner, t2.Channels.Include = "Eng", "eng@acme.com", []string{"eng-*"}
			m.Target.Teams = append(m.Target.Teams, t2)
		}},
		{"same pattern but different kinds", func(m *Config) {
			t2 := DefaultTeam()
			t2.Name, t2.Owner, t2.Channels.Types = "Private", "eng@acme.com", []string{ChannelPrivate}
			m.Target.Teams = append(m.Target.Teams, t2)
		}},
		{"character class glob", func(m *Config) { m.Target.Teams[0].Channels.Include = []string{"proj-[a-c]?", `lit\*`} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := example(t)
			tt.mutate(m)
			if issues := m.Validate(); len(issues) != 0 {
				t.Errorf("expected no issues:%s", dump(issues))
			}
		})
	}
}

func TestValidateReportsEverythingWithLines(t *testing.T) {
	m := mustParse(t, `apiVersion: taut.dev/v1alpha1
kind: Config
metadata:
  name: Bad Name
source:
  slack:
    export: ./x.zip
target:
  entra:
    tenantId: nope
    clientId: 9b2e7c1a-52d4-4f0e-8a3b-6c1d2e3f4a5b
    credential:
      clientSecret: { env: TAUT_ENTRA_CLIENT_SECRET }
  teams:
    - name: A
      owner: someone
    - name: A
      owner: b@acme.com
      channels:
        include:
          - "ok-*"
          - "[bad"
identity:
  unmapped:
    policy: block
content:
  since: 2024-06-01
  until: 2024-01-01
`)
	issues := m.Validate()
	want := map[string]int{
		"metadata.name":                        4,
		"target.entra.tenantId":                10,
		"target.entra.credential.clientSecret": 13,
		"target.teams[0].owner":                16,
		"target.teams[1].name":                 17,
		"target.teams[1].channels.include[1]":  22,
		"content.until":                        28,
	}
	for path, line := range want {
		got, ok := find(issues, path)
		if !ok {
			t.Errorf("no issue at %s:%s", path, dump(issues))
		} else if got.Line != line {
			t.Errorf("%s: line = %d, want %d", path, got.Line, line)
		}
	}
	if len(issues) != len(want) {
		t.Errorf("got %d issues, want %d:%s", len(issues), len(want), dump(issues))
	}
	if got := len(issues.Errors()); got != 6 {
		t.Errorf("Errors() has %d entries, want 6", got)
	}
	if got := len(issues.Warnings()); got != 1 {
		t.Errorf("Warnings() has %d entries, want 1", got)
	}
}

func TestIssuesErr(t *testing.T) {
	if err := (Issues{}).Err(); err != nil {
		t.Errorf("no issues: Err() = %v", err)
	}
	onlyWarnings := Issues{{Path: "a", Message: "careful", Severity: SeverityWarning}}
	if onlyWarnings.HasErrors() || onlyWarnings.Err() != nil {
		t.Error("warnings alone must not produce an error")
	}

	issues := Issues{
		{Path: "target.teams[0].owner", Message: "is required", Severity: SeverityError, Line: 12},
		{Path: "a", Message: "careful", Severity: SeverityWarning, Line: 3},
		{Path: "load.strategy", Message: "is not valid", Severity: SeverityError},
	}
	err := issues.Err()
	fe := asFail(t, err)
	if fe.Code != "config.invalid" || fail.ExitCode(err) != fail.ExitUsage {
		t.Errorf("code = %q, exit = %d", fe.Code, fail.ExitCode(err))
	}
	for _, s := range []string{"2 problems", "line 12: target.teams[0].owner: is required", "load.strategy: is not valid"} {
		if !strings.Contains(fe.Why, s) {
			t.Errorf("Why lacks %q: %s", s, fe.Why)
		}
	}
	if strings.Contains(fe.Why, "careful") {
		t.Errorf("Why includes a warning: %s", fe.Why)
	}
	if !strings.Contains(fe.Next, "taut init") || !strings.Contains(fe.Next, "Edit") {
		t.Errorf("Next = %q", fe.Next)
	}
	var ie *InvalidError
	if !errors.As(err, &ie) || len(ie.Issues) != 2 {
		t.Errorf("InvalidError = %+v", ie)
	}

	single := Issues{issues[0]}.Err()
	if fe := asFail(t, single); fe.Why != "line 12: target.teams[0].owner: is required" {
		t.Errorf("single-issue Why = %q", fe.Why)
	}

	var many Issues
	for i := 0; i < 30; i++ {
		many = append(many, Issue{Path: fmt.Sprintf("p%d", i), Message: "bad", Severity: SeverityError})
	}
	if fe := asFail(t, many.Err()); !strings.Contains(fe.Why, "… and 18 more") || strings.Contains(fe.Why, "p20:") {
		t.Errorf("long lists should be truncated: %s", fe.Why)
	}
}

// findSecretIssue returns the literal-secret finding at path.
func findSecretIssue(is Issues, path string) (Issue, bool) {
	for _, i := range is {
		if i.Path == path && strings.Contains(i.Message, "looks like a literal") {
			return i, true
		}
	}
	return Issue{}, false
}

var hygieneSecrets = []struct {
	name, kind, value string
	leaks             []string
}{
	{"slack token", secret.KindSlackToken, fakeSlackToken, []string{fakeSlackToken, fakeSlackToken[:20]}},
	{"jwt", secret.KindJWT, fakeJWT, []string{fakeJWT, fakeJWT[:24]}},
	{"entra secret", secret.KindEntraSecret, fakeEntraSecret, []string{fakeEntraSecret, fakeEntraSecret[:16]}},
	{"pem", secret.KindPrivateKey, fakePEM, []string{fakePEMBody, fakePEMBody[:16], "BEGIN PRIVATE"}},
}

func assertNoLeak(t *testing.T, leaks []string, err error, issues Issues) {
	t.Helper()
	fe := asFail(t, err)
	rendered := err.Error() + fe.What + fe.Why + fe.Next
	for _, i := range issues {
		rendered += i.String() + i.Path
	}
	for _, leak := range leaks {
		if strings.Contains(rendered, leak) {
			t.Errorf("output echoes the secret (%q): %s", leak, rendered)
		}
	}
}

// A literal secret anywhere in the file is rejected when the file is parsed,
// so it never reaches a config file value.
func TestSecretHygieneOnParse(t *testing.T) {
	placements := []struct {
		name string
		old  string
		new  string // %s is the secret as a YAML scalar
		path string
		line int
	}{
		{"as a literal where a reference belongs", "token: { env: TAUT_SLACK_TOKEN }", "token: %s", "source.slack.api.token", 10},
		{"inside a reference", "token: { env: TAUT_SLACK_TOKEN }", "token:\n        env: %s", "source.slack.api.token.env", 11},
		{"in a free-text field", "description: Migrated from Slack", "description: %s", "target.teams[0].description", 20},
		{"in a list", `exclude: ["random"]`, `exclude: ["random", %s]`, "target.teams[0].channels.exclude[1]", 25},
		{"in an unknown field", "  directMessages:\n", "  password: %s\n  directMessages:\n", "target.password", 28},
		{"as a mapping key", "rename: { old-name: New Name }", "rename: { %s: general }", "target.teams[0].channels.rename." + secret.Marker, 27},
	}
	for _, s := range hygieneSecrets {
		for _, p := range placements {
			t.Run(s.name+" "+p.name, func(t *testing.T) {
				if !strings.Contains(exampleYAML, p.old) {
					t.Fatalf("fixture lacks %q", p.old)
				}
				// A double-quoted scalar carries the PEM's newlines as \n.
				src := strings.Replace(exampleYAML, p.old, fmt.Sprintf(p.new, strconv.Quote(s.value)), 1)

				m, err := Parse([]byte(src))
				if err == nil || m != nil {
					t.Fatal("Parse accepted a config file containing a literal secret")
				}
				issues := issuesOf(t, err)
				got, ok := findSecretIssue(issues, p.path)
				if !ok {
					t.Fatalf("no secret finding at %s:%s", p.path, dump(issues))
				}
				if got.Severity != SeverityError || got.Line != p.line {
					t.Errorf("issue = %s line %d, want error on line %d", got.Severity, got.Line, p.line)
				}
				for _, want := range []string{"literal " + s.kind, "environment variable", "{ env: NAME }"} {
					if !strings.Contains(got.Message, want) {
						t.Errorf("message lacks %q: %s", want, got.Message)
					}
				}
				if n := len(issues); n != 1 && p.name != "in an unknown field" {
					t.Errorf("want only the secret finding, got:%s", dump(issues))
				}
				assertNoLeak(t, s.leaks, err, issues)
			})
		}
	}

	t.Run("pem block scalar", func(t *testing.T) {
		block := "description: |\n        " + strings.ReplaceAll(fakePEM, "\n", "\n        ")
		src := strings.Replace(exampleYAML, "description: Migrated from Slack", block, 1)
		_, err := Parse([]byte(src))
		issues := issuesOf(t, err)
		if _, ok := findSecretIssue(issues, "target.teams[0].description"); !ok {
			t.Errorf("no secret finding:%s", dump(issues))
		}
		assertNoLeak(t, []string{fakePEMBody[:16]}, err, issues)
	})

	t.Run("several at once", func(t *testing.T) {
		src := strings.Replace(exampleYAML, "{ env: TAUT_SLACK_TOKEN }", fakeSlackToken, 1)
		src = strings.Replace(src, "{ env: TAUT_S3_SECRET_KEY }", `"`+fakeEntraSecret+`"`, 1)
		_, err := Parse([]byte(src))
		issues := issuesOf(t, err)
		if len(issues) != 2 {
			t.Errorf("want 2 findings, got:%s", dump(issues))
		}
		assertNoLeak(t, []string{fakeSlackToken[:20], fakeEntraSecret[:16]}, err, issues)
	})
}

// A config file built or edited in memory gets the same check from Validate.
func TestSecretHygieneOnValidate(t *testing.T) {
	set := []struct {
		name string
		put  func(m *Config, v string)
		path string
		line int
	}{
		{"free-text field", func(m *Config, v string) { m.Target.Teams[0].Description = v }, "target.teams[0].description", 20},
		{"reference env", func(m *Config, v string) { m.Staging.SecretKey = secret.Ref{Env: v} }, "staging.secretKey.env", 47},
		{"reference file", func(m *Config, v string) { m.Target.Entra.Credential.Certificate = secret.Ref{File: v} }, "target.entra.credential.certificate.file", 17},
		{"url", func(m *Config, v string) { m.Controller.URL = "http://localhost:7070/" + v }, "controller.url", 50},
		{"list item", func(m *Config, v string) { m.Target.Teams[0].Channels.Include = []string{"*", v} }, "target.teams[0].channels.include[1]", 24},
		{"map key", func(m *Config, v string) { m.Target.Teams[0].Channels.Rename[v] = "general" }, "target.teams[0].channels.rename." + secret.Marker, 27},
		{"map value", func(m *Config, v string) { m.Target.Teams[0].Channels.Rename["old-name"] = v }, "target.teams[0].channels.rename.old-name", 27},
	}
	for _, s := range hygieneSecrets {
		for _, p := range set {
			t.Run(s.name+" in "+p.name, func(t *testing.T) {
				m := example(t)
				p.put(m, s.value)
				issues := m.Validate()
				got, ok := findSecretIssue(issues, p.path)
				if !ok {
					t.Fatalf("no secret finding at %s:%s", p.path, dump(issues))
				}
				if got.Severity != SeverityError || got.Line != p.line {
					t.Errorf("issue = %s line %d, want error on line %d", got.Severity, got.Line, p.line)
				}
				if !strings.Contains(got.Message, "literal "+s.kind) {
					t.Errorf("message = %q", got.Message)
				}
				assertNoLeak(t, s.leaks, issues.Err(), issues)

				// Without source positions the finding still appears.
				m.src = nil
				if got, ok := findSecretIssue(m.Validate(), p.path); !ok || got.Line != 0 {
					t.Errorf("in-memory config file: finding = %+v, %v", got, ok)
				}
			})
		}
	}
}

func TestValidateDoesNotFlagOrdinaryValues(t *testing.T) {
	m := example(t)
	m.Target.Teams[0].Description = "Tokens, JWTs and xoxb-style prefixes are discussed in #security ~ weekly"
	m.Target.Teams[0].Channels.Include = []string{"proj-*-2024", "~archive-*"}
	m.Source.Slack.Export = "~/Exports/Acme-Corp-Slack-Export-Jan-1-2020.zip"
	m.Target.Entra.Credential.Certificate = secret.Ref{File: "/etc/taut/keys/Acme_Migration~Backup.2024-01.pem"}
	if issues := m.Validate(); len(issues) != 0 {
		t.Errorf("false positives:%s", dump(issues))
	}
}
