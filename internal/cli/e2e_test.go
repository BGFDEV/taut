package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bgfdev/taut/internal/cli"
	"github.com/bgfdev/taut/internal/devstack"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/fakegraph"
	"github.com/bgfdev/taut/internal/fixtures"
	"github.com/bgfdev/taut/internal/testutil"
)

// env is one isolated test environment: a fixture export on disk and the
// fake Entra, Graph and Slack servers.
type env struct {
	t      *testing.T
	dir    string
	config string
	url    string
}

func newEnv(t *testing.T, fixture string) *env {
	t.Helper()
	dir := t.TempDir()
	if _, err := fixtures.Write(fixture, dir); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(devstack.Handler(fixture))
	t.Cleanup(srv.Close)
	t.Setenv("TAUT_SLACK_TOKEN", devstack.SlackToken)
	t.Setenv("TAUT_ENTRA_CLIENT_SECRET", fakegraph.DefaultSecret)
	return &env{t: t, dir: dir, config: filepath.Join(dir, "taut.yaml"), url: srv.URL}
}

// run invokes taut exactly as main does and returns stdout, stderr and the
// exit code.
func (e *env) run(args ...string) (stdout, stderr string, code int) {
	e.t.Helper()
	var out, errb bytes.Buffer
	args = append(args, "--config", e.config)
	code = cli.Main(context.Background(), args, strings.NewReader(""), &out, &errb,
		[]string{"TERM=dumb", "TAUT_GLYPHS=unicode", "LANG=en_US.UTF-8"})
	return out.String(), errb.String(), code
}

func (e *env) init(extra ...string) {
	e.t.Helper()
	args := append([]string{"init", "--yes", "--force",
		"--name", "fixture", "--export", filepath.Join(e.dir, "medium.zip"),
		"--tenant-id", fakegraph.DefaultTenantID, "--client-id", fakegraph.DefaultClientID,
		"--client-secret-env", "TAUT_ENTRA_CLIENT_SECRET",
		"--team", "Fixture Corp", "--owner", devstack.OwnerUPN, "--unmapped-account", devstack.FormerMemberUPN,
		"--authority", e.url, "--graph-url", e.url, "--slack-api-url", e.url + "/slack/api",
	}, extra...)
	if out, errOut, code := e.run(args...); code != 0 {
		e.t.Fatalf("init failed (%d):\n%s\n%s", code, out, errOut)
	}
}

// noSecrets fails the test if any secret reached the output.
func noSecrets(t *testing.T, outputs ...string) {
	t.Helper()
	for _, o := range outputs {
		for _, s := range []string{devstack.SlackToken, fakegraph.DefaultSecret, "eyJ"} {
			if strings.Contains(o, s) {
				t.Fatalf("output contains secret material %q:\n%s", s, o)
			}
		}
	}
}

func TestInitAuthPreflightEndToEnd(t *testing.T) {
	e := newEnv(t, "medium")
	e.init()

	// The config file must reference secrets, never contain them.
	raw, err := os.ReadFile(e.config)
	if err != nil {
		t.Fatal(err)
	}
	noSecrets(t, string(raw))
	if !strings.Contains(string(raw), "{env: TAUT_SLACK_TOKEN}") {
		t.Fatalf("config file does not reference the Slack token by env:\n%s", raw)
	}

	t.Run("auth", func(t *testing.T) {
		out, errOut, code := e.run("auth")
		if code != fail.ExitOK {
			t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
		}
		noSecrets(t, out, errOut)
		testutil.Golden(t, "auth_plain", testutil.Stable(out))
	})

	t.Run("auth json", func(t *testing.T) {
		out, errOut, code := e.run("auth", "--json")
		if code != fail.ExitOK || errOut != "" {
			t.Fatalf("exit %d, stderr %q", code, errOut)
		}
		noSecrets(t, out)
		var doc struct {
			OK      bool `json:"ok"`
			Summary struct{ Failed int }
			Checks  []struct{ Group, Label, Status string }
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("auth --json is not one JSON document: %v\n%s", err, out)
		}
		if !doc.OK || len(doc.Checks) < 8 {
			t.Fatalf("unexpected document: %+v", doc)
		}
	})

	t.Run("preflight", func(t *testing.T) {
		out, errOut, code := e.run("preflight")
		if code != fail.ExitOK {
			t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
		}
		noSecrets(t, out, errOut)
		testutil.Golden(t, "preflight_plain", out)
	})

	t.Run("preflight json", func(t *testing.T) {
		out, errOut, code := e.run("preflight", "--json")
		if code != fail.ExitOK || errOut != "" {
			t.Fatalf("exit %d, stderr %q", code, errOut)
		}
		var rep struct {
			Verdict  string
			Totals   struct{ Channels, Messages int }
			Identity struct {
				Checked       bool
				UnmappedUsers []struct{ Name string }
			}
			Permissions []struct{ Name, State string }
		}
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatalf("preflight --json is not one JSON document: %v", err)
		}
		f := fixtures.Facts("medium")
		if rep.Verdict != "ready_with_warnings" || rep.Totals.Channels != f.PublicChannels || !rep.Identity.Checked || len(rep.Permissions) == 0 {
			t.Fatalf("unexpected report: %+v", rep)
		}
		if want := f.Users - f.DirectoryUsers; len(rep.Identity.UnmappedUsers) != want {
			t.Fatalf("unmapped users = %d, want %d: preflight must list every one", len(rep.Identity.UnmappedUsers), want)
		}
	})

	t.Run("preflight fail-on warnings", func(t *testing.T) {
		if _, _, code := e.run("preflight", "--fail-on", "warnings"); code != fail.ExitBlockers {
			t.Fatalf("exit %d, want %d", code, fail.ExitBlockers)
		}
	})

	t.Run("preflight offline needs no credentials", func(t *testing.T) {
		t.Setenv("TAUT_ENTRA_CLIENT_SECRET", "")
		out, _, code := e.run("preflight", "--offline", "--json")
		if code != fail.ExitOK || !strings.Contains(out, `"identity.unchecked"`) {
			t.Fatalf("exit %d\n%s", code, out)
		}
	})

	t.Run("doctor", func(t *testing.T) {
		out, errOut, code := e.run("doctor", "--json")
		if code != fail.ExitOK {
			t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
		}
		noSecrets(t, out, errOut)
	})
}

// Private channels and the legacy strategy both change who sees what, and
// when. The preflight says so before anything is written.
func TestPreflightPrivateChannelsAndLegacy(t *testing.T) {
	t.Run("private channels", func(t *testing.T) {
		e := newEnv(t, "medium")
		e.init("--include-private")
		out, errOut, code := e.run("preflight")
		if code != fail.ExitOK {
			t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
		}
		noSecrets(t, out, errOut)
		if !strings.Contains(strings.Join(strings.Fields(out), " "), "added to the team as well as to the channel") {
			t.Errorf("the report does not say private-channel members join the team:\n%s", out)
		}
		testutil.Golden(t, "preflight_private_plain", out)
	})

	t.Run("legacy", func(t *testing.T) {
		e := newEnv(t, "medium")
		e.init()
		raw, err := os.ReadFile(e.config)
		if err != nil {
			t.Fatal(err)
		}
		legacy := strings.Replace(string(raw), "strategy: modern", "strategy: legacy", 1)
		if legacy == string(raw) {
			t.Fatalf("the config file does not state the load strategy:\n%s", raw)
		}
		if err := os.WriteFile(e.config, []byte(legacy), 0o600); err != nil {
			t.Fatal(err)
		}
		out, errOut, code := e.run("preflight")
		if code != fail.ExitOK {
			t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
		}
		noSecrets(t, out, errOut)
		testutil.Golden(t, "preflight_legacy_plain", out)

		// Private channels cannot be migrated this way, and the config
		// says so before any of it runs.
		withPrivate := strings.Replace(legacy, "types: [public]", "types: [public, private]", 1)
		if withPrivate == legacy {
			t.Fatalf("the config file does not list channel types as expected:\n%s", legacy)
		}
		if err := os.WriteFile(e.config, []byte(withPrivate), 0o600); err != nil {
			t.Fatal(err)
		}
		out, errOut, code = e.run("preflight")
		if code == fail.ExitOK || !strings.Contains(strings.Join(strings.Fields(out+errOut), " "), "legacy load strategy supports standard (public) channels only") {
			t.Fatalf("legacy with private channels: exit %d\n%s\n%s", code, out, errOut)
		}
	})
}

func TestFailuresAreActionable(t *testing.T) {
	e := newEnv(t, "medium")
	e.init()

	t.Run("wrong client secret", func(t *testing.T) {
		t.Setenv("TAUT_ENTRA_CLIENT_SECRET", "not-the-secret")
		out, _, code := e.run("auth")
		if code == fail.ExitOK {
			t.Fatal("auth passed with a wrong secret")
		}
		if !strings.Contains(out, "✗ Signed in") || !strings.Contains(out, "→") {
			t.Fatalf("failure is not explained with a next step:\n%s", out)
		}
		noSecrets(t, strings.ReplaceAll(out, "not-the-secret", ""))
		if strings.Contains(out, "not-the-secret") {
			t.Fatal("the rejected secret was echoed back")
		}
	})

	t.Run("slack token unset", func(t *testing.T) {
		t.Setenv("TAUT_SLACK_TOKEN", "")
		out, _, code := e.run("auth")
		if code == fail.ExitOK || !strings.Contains(out, "TAUT_SLACK_TOKEN") {
			t.Fatalf("exit %d\n%s", code, out)
		}
	})

	t.Run("blocked preflight exits 3", func(t *testing.T) {
		e.init("--unmapped-policy", "block")
		out, _, code := e.run("preflight")
		if code != fail.ExitBlockers || !strings.Contains(out, "Not ready") {
			t.Fatalf("exit %d\n%s", code, out)
		}
		e.init()
	})

	t.Run("missing config file", func(t *testing.T) {
		var out, errb bytes.Buffer
		code := cli.Main(context.Background(), []string{"preflight", "--json", "--config", filepath.Join(e.dir, "nope.yaml")},
			strings.NewReader(""), &out, &errb, nil)
		var doc struct {
			Error struct{ Code, What, Next string }
		}
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatalf("error output is not JSON: %v\n%s", err, out.String())
		}
		if code == 0 || doc.Error.Code != "config.not_found" || !strings.Contains(doc.Error.Next, "taut init") {
			t.Fatalf("exit %d, %+v", code, doc)
		}
	})

	t.Run("usage error exits 2", func(t *testing.T) {
		if _, errOut, code := e.run("preflight", "--no-such-flag"); code != fail.ExitUsage || !strings.Contains(errOut, "taut --help") {
			t.Fatalf("exit %d\n%s", code, errOut)
		}
	})
}

func TestInitRefusesToOverwrite(t *testing.T) {
	e := newEnv(t, "small")
	if err := os.WriteFile(e.config, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, errOut, code := e.run("init", "--yes", "--name", "x")
	if code != fail.ExitUsage || !strings.Contains(errOut, "--force") {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if b, _ := os.ReadFile(e.config); string(b) != "keep me" {
		t.Fatal("existing config file was modified")
	}
}

func TestInitRejectsPastedSecret(t *testing.T) {
	e := newEnv(t, "small")
	// A token given where an env var NAME belongs must never be written.
	_, errOut, code := e.run("init", "--yes", "--name", "x", "--export", filepath.Join(e.dir, "small.zip"),
		"--tenant-id", fakegraph.DefaultTenantID, "--client-id", fakegraph.DefaultClientID,
		"--team", "X", "--owner", "a@b.co", "--unmapped-account", "f@b.co",
		"--slack-token-env", devstack.SlackToken)
	if code == 0 {
		t.Fatal("init accepted a token as an env var name")
	}
	if strings.Contains(errOut, devstack.SlackToken) {
		t.Fatalf("the token was echoed:\n%s", errOut)
	}
	if _, err := os.Stat(e.config); err == nil {
		b, _ := os.ReadFile(e.config)
		if strings.Contains(string(b), devstack.SlackToken) {
			t.Fatal("the token was written to the config file")
		}
	}
}
