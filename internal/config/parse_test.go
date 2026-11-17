package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/secret"
)

func TestParseExample(t *testing.T) {
	m := example(t)

	want := &Config{
		APIVersion: APIVersion,
		Kind:       Kind,
		Metadata:   Metadata{Name: "acme-migration"},
		Source: Source{Slack: SlackSource{
			Export: "./slack-export.zip",
			API:    SlackAPI{Enabled: true, Token: secret.Ref{Env: "TAUT_SLACK_TOKEN"}},
		}},
		Target: Target{
			Entra: Entra{
				TenantID:   "3f2504e0-4f89-11d3-9a0c-0305e82c3301",
				ClientID:   "9b2e7c1a-52d4-4f0e-8a3b-6c1d2e3f4a5b",
				Cloud:      CloudGlobal,
				Credential: Credential{Certificate: secret.Ref{File: "./taut-app.pem"}},
			},
			Teams: []TeamSpec{{
				Name:        "Acme",
				Description: "Migrated from Slack",
				Owner:       "admin@acme.com",
				Visibility:  VisibilityPrivate,
				Channels: ChannelSelection{
					Include: []string{"*"},
					Exclude: []string{"random"},
					Types:   []string{ChannelPublic},
					Rename:  map[string]string{"old-name": "New Name"},
				},
			}},
		},
		Identity: Identity{
			Overrides: "./identity-overrides.csv",
			Unmapped:  Unmapped{Policy: PolicyFormerMember, Account: "taut-former-member@acme.com"},
		},
		Content: Content{Reactions: ReactionsFooter, CustomEmoji: EmojiText, Since: "2020-01-01"},
		Load:    LoadSpec{Strategy: StrategyModern},
		Staging: Staging{
			Endpoint:      "http://localhost:9000",
			Region:        "us-east-1",
			Bucket:        "taut-staging",
			AccessKey:     secret.Ref{Env: "TAUT_S3_ACCESS_KEY"},
			SecretKey:     secret.Ref{Env: "TAUT_S3_SECRET_KEY"},
			EncryptionKey: secret.Ref{Env: "TAUT_STAGING_KEY"},
		},
		Controller: Controller{URL: "http://localhost:7070"},
	}

	got := *m
	got.src = nil
	if !reflect.DeepEqual(&got, want) {
		t.Errorf("parsed config file differs:\n got %+v\nwant %+v", &got, want)
	}
	if !m.Equal(want) {
		t.Error("Equal() = false for the same plan")
	}
	if issues := m.Validate(); len(issues) != 0 {
		t.Errorf("the documented example should validate cleanly:%s", dump(issues))
	}
}

func TestParseAppliesDefaults(t *testing.T) {
	m := mustParse(t, `
metadata:
  name: tiny
target:
  teams:
    - name: One
      owner: a@b.co
    - name: Two
      owner: a@b.co
      visibility: public
      channels:
        include: [eng-*]
        types: [private]
staging:
  accessKey: { file: ./s3-access }
content:
  reactions:
`)
	def := Default()
	checks := []struct {
		name      string
		got, want any
	}{
		{"apiVersion", m.APIVersion, APIVersion},
		{"kind", m.Kind, Kind},
		{"cloud", m.Target.Entra.Cloud, CloudGlobal},
		{"policy", m.Identity.Unmapped.Policy, PolicyFormerMember},
		{"reactions (null counts as omitted)", m.Content.Reactions, ReactionsFooter},
		{"customEmoji", m.Content.CustomEmoji, EmojiText},
		{"identity.match (off unless asked for)", m.Identity.Match, Match{}},
		{"strategy", m.Load.Strategy, StrategyModern},
		{"api.enabled", m.Source.Slack.API.Enabled, false},
		{"directMessages", m.Target.DirectMessages.Enabled, false},
		{"staging.endpoint", m.Staging.Endpoint, def.Staging.Endpoint},
		{"staging.region", m.Staging.Region, def.Staging.Region},
		{"staging.bucket", m.Staging.Bucket, def.Staging.Bucket},
		{"staging.accessKey is not merged with the default", m.Staging.AccessKey, secret.Ref{File: "./s3-access"}},
		{"staging.secretKey", m.Staging.SecretKey, def.Staging.SecretKey},
		{"staging.encryptionKey", m.Staging.EncryptionKey, def.Staging.EncryptionKey},
		{"controller.url", m.Controller.URL, def.Controller.URL},
		{"teams[0].visibility", m.Target.Teams[0].Visibility, VisibilityPrivate},
		{"teams[0].include", m.Target.Teams[0].Channels.Include, []string{"*"}},
		{"teams[0].types", m.Target.Teams[0].Channels.Types, []string{ChannelPublic}},
		{"teams[1].visibility", m.Target.Teams[1].Visibility, VisibilityPublic},
		{"teams[1].include", m.Target.Teams[1].Channels.Include, []string{"eng-*"}},
		{"teams[1].types", m.Target.Teams[1].Channels.Types, []string{ChannelPrivate}},
		{"export stays empty", m.Source.Slack.Export, ""},
		{"token stays empty", m.Source.Slack.API.Token, secret.Ref{}},
	}
	for _, c := range checks {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s = %#v, want %#v", c.name, c.got, c.want)
		}
	}
}

func TestDefault(t *testing.T) {
	d := Default()
	if d.APIVersion != APIVersion || d.Kind != Kind || d.Load.Strategy != StrategyModern ||
		d.Target.Entra.Cloud != CloudGlobal || d.Controller.URL == "" {
		t.Errorf("unexpected defaults: %+v", d)
	}
	if d.Metadata.Name != "" || d.Target.Entra.TenantID != "" || len(d.Target.Teams) != 0 ||
		!d.Target.Entra.Credential.Certificate.IsZero() {
		t.Errorf("workspace-specific fields must start empty: %+v", d)
	}
	// Whatever Default leaves empty is exactly what Validate asks for.
	issues := d.Validate().Errors()
	for _, p := range []string{
		"metadata.name", "source.slack.export", "target.entra.tenantId", "target.entra.clientId",
		"target.entra.credential", "target.teams", "identity.unmapped.account",
	} {
		if _, ok := find(issues, p); !ok {
			t.Errorf("Default() should fail validation at %s:%s", p, dump(issues))
		}
	}
	if len(issues) != 7 {
		t.Errorf("Default() has %d errors, want the 7 workspace-specific ones:%s", len(issues), dump(issues))
	}
	// Default must hand out independent values.
	d.Target.Teams = append(d.Target.Teams, DefaultTeam())
	d.Staging.Bucket = "changed"
	if fresh := Default(); len(fresh.Target.Teams) != 0 || fresh.Staging.Bucket == "changed" {
		t.Error("Default() shares state between calls")
	}
}

func TestParseStrict(t *testing.T) {
	src := `apiVersion: taut.dev/v1alpha1
kind: Config
metdata:
  name: acme
source:
  slack:
    exprot: ./x.zip
    api:
      enabled: maybe
      token: plain-text
target:
  entra:
    tenant_id: 3f2504e0-4f89-11d3-9a0c-0305e82c3301
    credential:
      certificate: { file: ./a.pem, path: ./b.pem }
  teams: everyone
  zzzzzzzz: 1
content:
  reactions: [footer]
kind: Other
identity:
  match:
    emailLocalPart: yes please
    displayname: true
`
	_, err := Parse([]byte(src))
	issues := issuesOf(t, err)

	want := []struct {
		path string
		line int
		msg  string
	}{
		{"metdata", 3, `did you mean "metadata"?`},
		{"source.slack.exprot", 7, `did you mean "export"?`},
		{"source.slack.api.enabled", 9, "must be true or false, not a string"},
		{"source.slack.api.token", 10, "must be a secret reference"},
		{"target.entra.tenant_id", 13, `did you mean "tenantId"?`},
		{"target.entra.credential.certificate.path", 15, "unknown field"},
		{"target.teams", 16, "must be a list, not a string"},
		{"target.zzzzzzzz", 17, "expected one of: directMessages, entra, teams"},
		{"content.reactions", 19, "must be a single value, not a list"},
		{"kind", 20, "set more than once (first on line 2)"},
		{"identity.match.emailLocalPart", 23, "must be true or false, not a string"},
		{"identity.match.displayname", 24, `did you mean "displayName"?`},
	}
	for _, w := range want {
		got, ok := find(issues, w.path)
		if !ok {
			t.Errorf("no issue at %s:%s", w.path, dump(issues))
			continue
		}
		if got.Line != w.line || !strings.Contains(got.Message, w.msg) || got.Severity != SeverityError {
			t.Errorf("issue at %s = line %d %q, want line %d containing %q", w.path, got.Line, got.Message, w.line, w.msg)
		}
	}
	if len(issues) != len(want) {
		t.Errorf("got %d issues, want %d:%s", len(issues), len(want), dump(issues))
	}
	for i := 1; i < len(issues); i++ {
		if issues[i].Line < issues[i-1].Line {
			t.Errorf("issues are not in line order:%s", dump(issues))
			break
		}
	}

	fe := asFail(t, err)
	if fail.ExitCode(err) != fail.ExitUsage {
		t.Errorf("exit = %d, want ExitUsage", fail.ExitCode(err))
	}
	for _, s := range []string{"line 3", "metdata", "line 16", "taut init"} {
		if !strings.Contains(fe.Why+fe.Next, s) {
			t.Errorf("error text lacks %q: %s | %s", s, fe.Why, fe.Next)
		}
	}
	if strings.Contains(fe.Why, "plain-text") || strings.Contains(fe.Why, "maybe") {
		t.Errorf("error echoes field values: %s", fe.Why)
	}
}

func TestParseSyntaxAndEmpty(t *testing.T) {
	tests := []struct {
		name string
		src  string
		code string
		line int
	}{
		{"bad indentation", "metadata:\n  name: a\n   oops: b\n", "config.syntax", 3},
		{"unclosed flow", "metadata: { name: a\nkind: x\n", "config.syntax", 0},
		{"tab indentation", "metadata:\n\tname: a\n", "config.syntax", 2},
		{"top-level list", "- a\n- b\n", "config.syntax", 1},
		{"top-level scalar", "just text\n", "config.syntax", 1},
		{"empty", "", "config.empty", 0},
		{"only comments", "# nothing here\n", "config.empty", 0},
		{"null document", "~\n", "config.empty", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.src))
			fe := asFail(t, err)
			if fe.Code != tt.code {
				t.Fatalf("code = %q, want %q (%v)", fe.Code, tt.code, err)
			}
			if fail.ExitCode(err) != fail.ExitUsage {
				t.Errorf("exit = %d, want ExitUsage", fail.ExitCode(err))
			}
			if tt.code != "config.syntax" {
				return
			}
			var se *SyntaxError
			if !errors.As(err, &se) {
				t.Fatalf("no *SyntaxError in chain: %v", err)
			}
			if tt.line != 0 && se.Line != tt.line {
				t.Errorf("line = %d, want %d (%v)", se.Line, tt.line, err)
			}
			if se.Line != 0 && !strings.Contains(fe.Why, "line ") {
				t.Errorf("Why does not mention the line: %q", fe.Why)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plans", "taut.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(exampleYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(dir, "plans")
	if m.BaseDir() != base || m.File() != path {
		t.Errorf("BaseDir() = %q, File() = %q", m.BaseDir(), m.File())
	}

	t.Run("resolve paths", func(t *testing.T) {
		abs := filepath.Join(dir, "elsewhere", "export.zip")
		tests := []struct{ in, want string }{
			{m.Source.Slack.Export, filepath.Join(base, "slack-export.zip")},
			{m.Identity.Overrides, filepath.Join(base, "identity-overrides.csv")},
			{"../shared/x.csv", filepath.Join(dir, "shared", "x.csv")},
			{abs, abs},
			{"", ""},
		}
		for _, tt := range tests {
			if got := m.ResolvePath(tt.in); got != tt.want {
				t.Errorf("ResolvePath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		}
		// Parsed from bytes: relative to the working directory.
		if got := example(t).ResolvePath("./a/b.zip"); got != filepath.Join("a", "b.zip") {
			t.Errorf("ResolvePath without a base dir = %q", got)
		}
		p := example(t)
		p.SetBaseDir(dir)
		if got := p.ResolvePath("x"); got != filepath.Join(dir, "x") {
			t.Errorf("ResolvePath after SetBaseDir = %q", got)
		}
	})

	t.Run("secret files resolve against the config file", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(base, "taut-app.pem"), []byte("pem-material\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		v, warns, err := m.ResolveSecret(m.Target.Entra.Credential.Certificate)
		if err != nil || v.Reveal() != "pem-material" || len(warns) != 0 {
			t.Errorf("ResolveSecret = %q, %v, %v", v.Reveal(), warns, err)
		}
	})

	t.Run("not found", func(t *testing.T) {
		_, err := Load(filepath.Join(dir, "missing.yaml"))
		fe := asFail(t, err)
		if fe.Code != "config.not_found" {
			t.Fatalf("code = %q", fe.Code)
		}
		if fe.Next != "Run `taut init` to create one, or pass --config <path>." {
			t.Errorf("Next = %q", fe.Next)
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Error("error does not wrap fs.ErrNotExist")
		}
	})

	t.Run("directory", func(t *testing.T) {
		_, err := Load(dir)
		if fe := asFail(t, err); fe.Code != "config.unreadable" {
			t.Errorf("code = %q", fe.Code)
		}
	})

	t.Run("syntax error names the file and line", func(t *testing.T) {
		bad := filepath.Join(dir, "bad.yaml")
		if err := os.WriteFile(bad, []byte("metadata:\n  name: a\n   oops: b\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(bad)
		fe := asFail(t, err)
		if fe.Code != "config.syntax" || !strings.Contains(fe.What, bad) || !strings.Contains(fe.Why, "line 3") {
			t.Errorf("got %q / %q / %q", fe.Code, fe.What, fe.Why)
		}
	})

	t.Run("invalid names the file", func(t *testing.T) {
		bad := filepath.Join(dir, "unknown.yaml")
		//nolint:misspell // the typo is the test input
		if err := os.WriteFile(bad, []byte("metadata:\n  nmae: a\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(bad)
		fe := asFail(t, err)
		if fe.Code != "config.invalid" || !strings.Contains(fe.What, bad) || !strings.Contains(fe.Next, bad) {
			t.Errorf("got %q / %q / %q", fe.Code, fe.What, fe.Next)
		}
	})
}
