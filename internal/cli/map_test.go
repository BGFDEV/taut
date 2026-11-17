package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/devstack"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/fakegraph"
	"github.com/bgfdev/taut/internal/fixtures"
	"github.com/bgfdev/taut/internal/preflight"
	"github.com/bgfdev/taut/internal/slack"
	"github.com/bgfdev/taut/internal/slack/export"
	"github.com/bgfdev/taut/internal/testutil"
	"github.com/bgfdev/taut/internal/tui/mapping"
)

// mapEnv writes a fixture export and a config file for it, as `taut init`
// does.
func mapEnv(t *testing.T, fixture string, extra ...string) *env {
	t.Helper()
	e := newEnv(t, fixture)
	args := append([]string{
		"init", "--yes", "--force",
		"--name", "fixture", "--export", filepath.Join(e.dir, fixture+".zip"),
		"--tenant-id", fakegraph.DefaultTenantID, "--client-id", fakegraph.DefaultClientID,
		"--client-secret-env", "TAUT_ENTRA_CLIENT_SECRET",
		"--team", "Fixture Corp", "--owner", devstack.OwnerUPN, "--unmapped-account", devstack.FormerMemberUPN,
	}, extra...)
	if out, errOut, code := e.run(args...); code != 0 {
		t.Fatalf("init failed (%d):\n%s\n%s", code, out, errOut)
	}
	return e
}

func (e *env) read() string {
	e.t.Helper()
	b, err := os.ReadFile(e.config)
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

func (e *env) load() *config.Config {
	e.t.Helper()
	m, err := config.Load(e.config)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := m.Validate().Err(); err != nil {
		e.t.Fatalf("the config file on disk is invalid: %v", err)
	}
	return m
}

// addTeam appends a second team to the config file on disk.
func (e *env) addTeam(name string, include []string, mutate func(first *config.TeamSpec)) {
	e.t.Helper()
	m := e.load()
	t := config.DefaultTeam()
	t.Name, t.Owner, t.Channels.Include = name, devstack.OwnerUPN, include
	m.Target.Teams = append(m.Target.Teams, t)
	if mutate != nil {
		mutate(&m.Target.Teams[0])
	}
	if err := m.Write(e.config, true); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) channels() []slack.Channel {
	e.t.Helper()
	m := e.load()
	ex, err := export.Open(m.ResolvePath(m.Source.Slack.Export))
	if err != nil {
		e.t.Fatal(err)
	}
	defer ex.Close()
	chs, err := ex.Channels()
	if err != nil {
		e.t.Fatal(err)
	}
	return chs
}

// With direct messages migrated, map lists each one, read-only, with what
// becomes of it as far as the export alone can tell.
func TestMapListsDirectMessages(t *testing.T) {
	e := mapEnv(t, "nasty", "--include-direct-messages")
	out, errOut, code := e.run("map")
	if code != fail.ExitOK || errOut != "" {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	noSecrets(t, out)
	for _, want := range []string{"Direct messages", "migrates", "skipped: a participant is a bot or an app", "skipped: a conversation with oneself", "taut preflight"} {
		if !strings.Contains(out, want) {
			t.Errorf("map does not show %q:\n%s", want, out)
		}
	}
	testutil.Golden(t, "map_dms_plain", out)

	out, errOut, code = e.run("map", "--json")
	if code != fail.ExitOK || errOut != "" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	var doc struct {
		DirectMessages []struct {
			SlackID  string `json:"slackId"`
			Migrates bool   `json:"migrates"`
			Skip     string `json:"skip"`
		} `json:"directMessages"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(doc.DirectMessages) != 9 {
		t.Fatalf("directMessages = %+v", doc.DirectMessages)
	}
	for _, d := range doc.DirectMessages {
		if d.SlackID == "" || d.Migrates != (d.Skip == "") {
			t.Errorf("direct message = %+v", d)
		}
	}
	// A direct message cannot be included, excluded, moved or renamed.
	for _, args := range [][]string{{"--exclude", doc.DirectMessages[0].SlackID}, {"--include", doc.DirectMessages[0].SlackID}} {
		if _, _, code := e.run(append([]string{"map", "--yes"}, args...)...); code == fail.ExitOK {
			t.Errorf("map %v was accepted", args)
		}
	}
}

func TestMapPrintsTheMapping(t *testing.T) {
	e := mapEnv(t, "medium")
	before := e.read()

	t.Run("plain", func(t *testing.T) {
		out, errOut, code := e.run("map")
		if code != fail.ExitOK || errOut != "" {
			t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
		}
		noSecrets(t, out)
		testutil.Golden(t, "map_plain", out)
	})

	t.Run("json", func(t *testing.T) {
		out, errOut, code := e.run("map", "--json")
		if code != fail.ExitOK || errOut != "" {
			t.Fatalf("exit %d, stderr %q", code, errOut)
		}
		testutil.Golden(t, "map_json", out)

		var doc struct {
			Config string
			Teams  []struct {
				Name     string
				Channels []struct {
					SlackID, SlackName, TargetName string
					Renamed                        *bool
					Warnings                       *[]string
				}
			}
			Skipped  []struct{ SlackName, Kind, Reason string }
			Blockers *[]preflight.Finding
			Warnings *[]preflight.Finding
			Changes  *[]mapping.Change
			Saved    *bool
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("map --json is not one JSON document: %v\n%s", err, out)
		}
		f := fixtures.Facts("medium")
		if len(doc.Teams) != 1 || len(doc.Teams[0].Channels) != f.PublicChannels || doc.Blockers == nil || doc.Warnings == nil || doc.Changes == nil || doc.Saved == nil {
			t.Fatalf("unexpected document: %+v", doc)
		}
		for _, c := range doc.Teams[0].Channels {
			if c.SlackID == "" || c.SlackName == "" || c.TargetName == "" || c.Renamed == nil || c.Warnings == nil {
				t.Fatalf("channel is missing a field of the schema: %+v", c)
			}
		}
		if len(doc.Skipped) != f.PrivateChannels+1 {
			t.Fatalf("skipped = %d, want the %d private channels and the DM summary", len(doc.Skipped), f.PrivateChannels)
		}

		// The map shows exactly what `taut preflight` reports.
		report, _, code := e.run("preflight", "--offline", "--json")
		if code != fail.ExitOK {
			t.Fatalf("preflight exit %d", code)
		}
		type plan struct {
			Teams []struct {
				Name     string
				Channels []struct{ SlackID, SlackName, TargetName string }
			}
			Skipped []struct{ SlackName, Kind, Reason string }
		}
		var a, b plan
		if err := json.Unmarshal([]byte(out), &a); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(report), &b); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("map and preflight disagree:\nmap %+v\npreflight %+v", a, b)
		}
	})

	if e.read() != before {
		t.Fatal("printing the mapping modified the config file")
	}
}

func TestMapEditFlags(t *testing.T) {
	e := mapEnv(t, "medium")
	before := e.read()
	edits := []string{"map", "--exclude", "random", "--exclude", "#watercooler", "--rename", "design=Design Studio"}

	t.Run("without --yes nothing is written", func(t *testing.T) {
		out, errOut, code := e.run(edits...)
		if code != fail.ExitOK || errOut != "" {
			t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
		}
		testutil.Golden(t, "map_edits_pending", strings.ReplaceAll(out, e.dir, "<dir>"))
		if e.read() != before {
			t.Fatal("the config file was written without --yes")
		}
		if !strings.Contains(out, "--yes") {
			t.Fatalf("no hint to pass --yes:\n%s", out)
		}
	})

	t.Run("json without --yes", func(t *testing.T) {
		out, _, code := e.run(append(edits, "--json")...)
		var doc struct {
			Changes []mapping.Change
			Saved   bool
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil || code != fail.ExitOK {
			t.Fatalf("exit %d, %v\n%s", code, err, out)
		}
		if doc.Saved || len(doc.Changes) != 3 || e.read() != before {
			t.Fatalf("unexpected: %+v", doc)
		}
	})

	t.Run("with --yes the config file is written", func(t *testing.T) {
		out, errOut, code := e.run(append(edits, "--yes")...)
		if code != fail.ExitOK || errOut != "" {
			t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
		}
		testutil.Golden(t, "map_edits_saved", out)
		noSecrets(t, e.read())

		c := e.load().Target.Teams[0].Channels
		if !reflect.DeepEqual(c.Exclude, []string{"random", "watercooler"}) || !reflect.DeepEqual(c.Rename, map[string]string{"design": "Design Studio"}) {
			t.Fatalf("selection on disk: %+v", c)
		}
		// Still the form `taut init` writes: header, section comments.
		raw := e.read()
		for _, want := range []string{"# Taut config file", "# Where it goes", "exclude: [random, watercooler]", "design: Design Studio"} {
			if !strings.Contains(raw, want) {
				t.Errorf("saved config file lacks %q:\n%s", want, raw)
			}
		}

		// Running the same command again is a no-op, not an error.
		out, _, code = e.run(append(edits, "--yes")...)
		if code != fail.ExitOK || !strings.Contains(out, "Nothing to change") || e.read() != raw {
			t.Fatalf("re-running the edits: exit %d\n%s", code, out)
		}

		// And preflight agrees with what was saved.
		report, _, _ := e.run("preflight", "--offline")
		if strings.Contains(report, "#random ") && !strings.Contains(report, "Not migrated") || !strings.Contains(report, "Design Studio") {
			t.Fatalf("preflight does not reflect the saved edits:\n%s", report)
		}
	})
}

func TestMapRefusalsAreActionable(t *testing.T) {
	e := mapEnv(t, "medium")
	before := e.read()
	cases := []struct {
		name string
		args []string
		code string
		next string
	}{
		{"unknown channel", []string{"--exclude", "nope", "--yes"}, "map.unknown_channel", "taut map"},
		{"unknown team", []string{"--move", "random=Nope", "--yes"}, "map.unknown_team", "target.teams"},
		{"name Teams rejects", []string{"--rename", "random=a/b", "--yes"}, "map.invalid_name", `"a-b"`},
		{"malformed pair", []string{"--rename", "random", "--yes"}, "usage", "--rename <slack-name>=<value>"},
		{"private channel", []string{"--include", "leadership", "--yes"}, "map.kind_not_selected", "types"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, errOut, code := e.run(append(append([]string{"map"}, c.args...), "--json")...)
			var doc struct {
				Error struct{ Code, What, Next string }
			}
			if err := json.Unmarshal([]byte(out), &doc); err != nil {
				t.Fatalf("error output is not JSON: %v\n%s\n%s", err, out, errOut)
			}
			if code != fail.ExitUsage || doc.Error.Code != c.code || !strings.Contains(doc.Error.Next, c.next) {
				t.Fatalf("exit %d, %+v", code, doc.Error)
			}
			if e.read() != before {
				t.Fatal("a refused edit wrote the config file")
			}
		})
	}

	t.Run("plain error has what and next", func(t *testing.T) {
		_, errOut, code := e.run("map", "--exclude", "randon")
		if code != fail.ExitUsage || !strings.Contains(errOut, `There is no channel named "randon"`) || !strings.Contains(errOut, "taut map") {
			t.Fatalf("exit %d\n%s", code, errOut)
		}
	})
}

func TestMapBlockersExitThreeAndWriteNothing(t *testing.T) {
	e := mapEnv(t, "medium")
	// Two teams that both claim #sales and #support.
	e.addTeam("Go To Market", []string{"sales", "support"}, nil)
	before := e.read()

	out, _, code := e.run("map", "--exclude", "random", "--yes")
	if code != fail.ExitBlockers || !strings.Contains(out, "Not saved") || !strings.Contains(out, "selected by 2 teams") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if e.read() != before {
		t.Fatal("the config file was written although blockers remain")
	}
	testutil.Golden(t, "map_blocked", out)

	// Viewing a blocked mapping is not a failure; editing it is.
	if _, _, code := e.run("map"); code != fail.ExitOK {
		t.Fatalf("viewing exit %d", code)
	}

	// Giving each channel one home in the same command resolves it.
	out, errOut, code := e.run("map", "--exclude", "random", "--move", "sales=Go To Market", "--move", "support=go to market", "--yes")
	if code != fail.ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	l := preflight.Layout(e.load(), e.channels(), nil)
	if len(l.Blockers) != 0 || len(l.Teams[1].Channels) != 2 {
		t.Fatalf("after the moves: %+v", l)
	}
}

// The round trip: for every fixture, edits applied in memory and edits saved
// through the command must produce the same mapping when the file is read
// back, and nothing else in the config file may change.
func TestMapRoundTrip(t *testing.T) {
	type edit struct{ flag, value string }
	cases := []struct {
		fixture string
		init    []string
		edits   []edit
	}{
		{"small", nil, []edit{{"exclude", "random"}, {"rename", "project-atlas=Atlas"}}},
		{"medium", []string{"--include-private"}, []edit{
			{"exclude", "random"}, {"exclude", "leadership"}, {"rename", "design=Design Studio"},
			{"move", "sales=Go To Market"}, {"move", "marketing=Go To Market"}, {"rename", "marketing=Brand"},
		}},
		{"nasty", []string{"--include-private"}, []edit{
			{"exclude", "old-project"}, {"exclude", "empty-room"}, {"rename", "_archive=Archive"},
			{"rename", "con=Conference"}, {"move", "secret-plans=Go To Market"}, {"move", "unicode=Go To Market"},
		}},
	}
	for _, c := range cases {
		for _, style := range []string{"standard", "hand-edited"} {
			t.Run(c.fixture+"/"+style, func(t *testing.T) {
				e := mapEnv(t, c.fixture, c.init...)
				e.addTeam("Go To Market", []string{"gtm-*"}, func(first *config.TeamSpec) {
					first.Channels.Exclude = []string{"gtm-*"}
				})
				// Private channels can only move to a team that takes them.
				m := e.load()
				m.Target.Teams[1].Channels.Types = m.Target.Teams[0].Channels.Types
				if err := m.Write(e.config, true); err != nil {
					t.Fatal(err)
				}
				if style == "hand-edited" {
					raw := strings.Replace(e.read(), "target:\n", "target:\n  # KEEP: reviewed by legal on 2026-11-02\n", 1)
					raw = strings.Replace(raw, "identity:\n", "identity: # KEEP: see ticket MIG-42\n", 1)
					raw += "\n# KEEP: trailing note\n"
					if err := os.WriteFile(e.config, []byte(raw), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				original := e.load()
				channels := e.channels()

				// Expected: the same edits on an in-memory editor.
				ed := mapping.NewEditor(original, channels, nil, nil)
				args := []string{"map", "--yes"}
				for _, x := range c.edits {
					name, value, _ := strings.Cut(x.value, "=")
					var err error
					switch x.flag {
					case "exclude":
						err = ed.Exclude(name)
					case "rename":
						err = ed.Rename(name, value)
					case "move":
						err = ed.Move(name, value)
					}
					if err != nil {
						t.Fatalf("%s %s: %v", x.flag, x.value, err)
					}
					args = append(args, "--"+x.flag, x.value)
				}
				want := ed.Layout()
				if len(ed.Unsaved()) < len(c.edits)-1 {
					t.Fatalf("edits had too little effect: %+v", ed.Unsaved())
				}

				out, errOut, code := e.run(args...)
				if code != fail.ExitOK {
					t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
				}
				if strings.Contains(out, "rewritten in the standard form") {
					t.Fatalf("the save fell back to a full rewrite:\n%s", out)
				}

				reloaded := e.load()
				got := preflight.Layout(reloaded, channels, nil)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("layout after save and reload differs:\n got %+v\nwant %+v", got, want)
				}
				if !reloaded.Equal(ed.Config()) {
					t.Fatal("the config file on disk is not the edited config file")
				}

				// Nothing but the teams' channel selections changed.
				strip := func(m *config.Config) *config.Config {
					c := *m
					c.Target.Teams = append([]config.TeamSpec(nil), m.Target.Teams...)
					for i := range c.Target.Teams {
						ch := &c.Target.Teams[i].Channels
						ch.Include, ch.Exclude, ch.Rename = []string{"*"}, nil, nil
					}
					return &c
				}
				if !strip(reloaded).Equal(strip(original)) {
					t.Fatalf("something outside the channel selections changed:\n%s", e.read())
				}
				for i, team := range reloaded.Target.Teams {
					if !reflect.DeepEqual(team.Channels.Types, original.Target.Teams[i].Channels.Types) {
						t.Fatalf("team %d channel types changed", i)
					}
				}

				raw := e.read()
				noSecrets(t, raw)
				if st, err := os.Stat(e.config); err != nil || st.Mode().Perm() != 0o600 {
					t.Fatalf("mode after save: %v %v", st.Mode(), err)
				}
				if style == "hand-edited" {
					if n := strings.Count(raw, "# KEEP:"); n != 3 {
						t.Fatalf("%d of 3 hand-written comments survived:\n%s", n, raw)
					}
				} else if canonical, _ := reloaded.Marshal(); string(canonical) != raw {
					t.Fatalf("a standard-form config file did not stay in standard form:\n%s", raw)
				}
				if left, _ := filepath.Glob(filepath.Join(e.dir, ".taut.yaml.*")); len(left) != 0 {
					t.Fatalf("temporary files left behind: %v", left)
				}

				// A second edit on the saved file, undoing the first exclusion,
				// also round-trips.
				back := c.edits[0].value
				if _, errOut, code := e.run("map", "--yes", "--include", back); code != fail.ExitOK {
					t.Fatalf("second edit: exit %d\n%s", code, errOut)
				}
				ed2 := mapping.NewEditor(reloaded, channels, nil, nil)
				if err := ed2.Include(back, ""); err != nil {
					t.Fatal(err)
				}
				if got := preflight.Layout(e.load(), channels, nil); !reflect.DeepEqual(got, ed2.Layout()) {
					t.Fatal("layout after the second save differs")
				}
			})
		}
	}
}

// From the command line a multi-document config file can be read but not
// edited: the edit exits 2 before anything is written.
func TestMapRefusesToEditAMultiDocumentConfig(t *testing.T) {
	e := mapEnv(t, "medium")
	src := e.read() + "---\nkind: Notes\n"
	if err := os.WriteFile(e.config, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, errOut, code := e.run("map"); code != fail.ExitOK {
		t.Fatalf("showing the mapping failed (%d):\n%s\n%s", code, out, errOut)
	}
	out, errOut, code := e.run("map", "--exclude", "random", "--yes")
	if code != fail.ExitUsage || !strings.Contains(errOut, "more than one YAML document") || !strings.Contains(errOut, "taut map") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if e.read() != src {
		t.Fatal("the config file was written")
	}
}
