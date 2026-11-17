package mapping_test

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/preflight"
	"github.com/bgfdev/taut/internal/slack"
	"github.com/bgfdev/taut/internal/testutil"
	"github.com/bgfdev/taut/internal/tui/mapping"
	"github.com/bgfdev/taut/internal/tui/shell"
	"github.com/bgfdev/taut/internal/tui/theme"
)

// --- fixtures -------------------------------------------------------------------

func team(name string, include ...string) config.TeamSpec {
	t := config.DefaultTeam()
	t.Name, t.Owner = name, "admin@acme.example"
	if len(include) > 0 {
		t.Channels.Include = include
	}
	return t
}

func baseConfig(teams ...config.TeamSpec) *config.Config {
	m := config.Default()
	m.Metadata.Name = "acme-migration"
	m.Source.Slack.Export = "export.zip"
	m.Target.Entra.TenantID = "11111111-2222-3333-4444-555555555555"
	m.Target.Entra.ClientID = "66666666-7777-8888-9999-000000000000"
	m.Target.Entra.Credential.ClientSecret.Env = "TAUT_ENTRA_CLIENT_SECRET"
	m.Identity.Unmapped.Account = "former@acme.example"
	m.Target.Teams = teams
	return m
}

func pub(name string) slack.Channel {
	return slack.Channel{ID: "C" + strings.ToUpper(strings.NewReplacer("-", "", "_", "").Replace(name)), Name: name, Kind: slack.KindPublic}
}

func workspace() []slack.Channel {
	general := pub("general")
	general.IsGeneral = true
	old := pub("old-project")
	old.IsArchived = true
	private := pub("leadership")
	private.Kind = slack.KindPrivate
	return []slack.Channel{
		general, pub("random"), pub("engineering"), pub("design"), pub("_archive"), pub("forms"),
		pub("proj-" + strings.Repeat("very-long-", 7) + "name0"), old, pub("sales-emea"), pub("sales-apac"), private,
		{ID: "D1", Kind: slack.KindDM}, {ID: "D2", Kind: slack.KindDM},
	}
}

func daysFor(channels []slack.Channel) map[string]int {
	d := map[string]int{}
	for i, ch := range channels {
		if ch.Name != "" {
			d[ch.ID] = (i * 37) % 120
		}
	}
	return d
}

// twoTeams is a config file with a catch-all team and a Sales team.
func twoTeams() *config.Config {
	corp := team("Acme Corp")
	corp.Channels.Exclude = []string{"sales-*"}
	return baseConfig(corp, team("Sales", "sales-*"))
}

func newEditor(t *testing.T, m *config.Config, channels []slack.Channel) *mapping.Editor {
	t.Helper()
	if err := m.Validate().Err(); err != nil {
		t.Fatalf("test config file is invalid: %v", err)
	}
	return mapping.NewEditor(m, channels, nil, daysFor(channels))
}

// --- driving the screen -----------------------------------------------------------

type harness struct {
	t     *testing.T
	ed    *mapping.Editor
	scr   *mapping.Screen
	m     tea.Model
	saved []*config.Config
	quit  bool
}

func newHarness(t *testing.T, ed *mapping.Editor, th theme.Theme, w, h int) *harness {
	t.Helper()
	x := &harness{t: t, ed: ed}
	x.scr = mapping.NewScreen(ed, mapping.Options{File: "taut.yaml", Save: func(m *config.Config) error {
		x.saved = append(x.saved, m)
		return nil
	}})
	x.m = shell.New(x.scr, shell.Options{Theme: th, Command: "map", Context: "acme-migration", LockTheme: true})
	x.m, _ = x.m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return x
}

func keyMsg(k string) tea.KeyPressMsg {
	switch k {
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	case "pgdown":
		return tea.KeyPressMsg{Code: tea.KeyPgDown}
	case "ctrl+u":
		return tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl}
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	}
	return tea.KeyPressMsg{Code: []rune(k)[0], Text: k}
}

func (x *harness) press(keys ...string) *harness {
	x.t.Helper()
	for _, k := range keys {
		var cmd tea.Cmd
		x.m, cmd = x.m.Update(keyMsg(k))
		for cmd != nil {
			msg := cmd()
			cmd = nil
			switch msg := msg.(type) {
			case tea.QuitMsg:
				x.quit = true
			case nil:
			default:
				x.m, cmd = x.m.Update(msg)
			}
		}
	}
	return x
}

// typeText types each rune of s.
func (x *harness) typeText(s string) *harness {
	for _, r := range s {
		k := string(r)
		if r == ' ' {
			k = "space"
		}
		x.press(k)
	}
	return x
}

func (x *harness) frame() string { return testutil.Plain(x.m.View().Content) }

// --- snapshots ------------------------------------------------------------------

func TestSnapshots(t *testing.T) {
	dark := theme.Default()
	ascii := theme.New(theme.Options{Dark: true, Unicode: false})
	fresh := func(th theme.Theme, w, h int) *harness {
		return newHarness(t, newEditor(t, twoTeams(), workspace()), th, w, h)
	}

	cases := []struct {
		name string
		x    *harness
	}{
		{"initial_80x24", fresh(dark, 80, 24)},
		{"initial_120x40", fresh(dark, 120, 40)},
		{"collapsed_team_80x24", fresh(dark, 80, 24).press("left")},
		{"filter_active_80x24", fresh(dark, 80, 24).press("/").typeText("sales")},
		{"filter_applied_80x24", fresh(dark, 80, 24).press("/").typeText("pro").press("enter")},
		{"rename_in_progress_80x24", fresh(dark, 80, 24).press("down", "down", "r", "ctrl+u").typeText("Water Cooler")},
		{"rename_rejected_80x24", fresh(dark, 80, 24).press("down", "down", "r", "ctrl+u").typeText("chat/random?").press("enter")},
		{"rename_collision_80x24", fresh(dark, 80, 24).press("down", "down", "r", "ctrl+u").typeText("design").press("enter")},
		{"edited_80x24", fresh(dark, 80, 24).press("down", "down", "space", "down", "r", "ctrl+u").typeText("Studio").press("enter")},
		{"move_picker_80x24", fresh(dark, 80, 24).press("down", "down", "m")},
		{"moved_80x24", fresh(dark, 80, 24).press("down", "down", "m", "enter")},
		{"unsaved_quit_confirm_80x24", fresh(dark, 80, 24).press("down", "down", "space", "q")},
		{"saved_80x24", fresh(dark, 80, 24).press("down", "down", "space", "s")},
		{"issues_80x24", fresh(dark, 80, 24).press("i")},
		{"help_while_unsaved_80x24", fresh(dark, 80, 24).press("down", "down", "space", "?")},
		{"narrow_60x14", fresh(dark, 60, 14)},
		{"short_60x12", fresh(dark, 60, 12)},
		{"ascii_80x24", fresh(ascii, 80, 24).press("down", "down", "space")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			testutil.Golden(t, c.name, c.x.frame())
		})
	}

	t.Run("ambiguous_80x24", func(t *testing.T) {
		m := baseConfig(team("Acme Corp"), team("Sales", "sales-*"))
		testutil.Golden(t, "ambiguous_80x24", newHarness(t, newEditor(t, m, workspace()), dark, 80, 24).frame())
	})

	// With direct messages migrated, each is listed under its own heading,
	// read-only, with what becomes of it.
	dms := func(w, h int) *harness {
		m := twoTeams()
		m.Target.DirectMessages.Enabled = true
		ed := newEditor(t, m, dmWorkspace())
		ed.SetUsers(dmUsers())
		return newHarness(t, ed, dark, w, h)
	}
	t.Run("direct_messages_100x30", func(t *testing.T) {
		testutil.Golden(t, "direct_messages_100x30", dms(100, 30).frame())
	})
	t.Run("direct_messages_selected_100x30", func(t *testing.T) {
		x := dms(100, 30).press("G", "up", "up", "up", "up", "space")
		testutil.Golden(t, "direct_messages_selected_100x30", x.frame())
	})

	t.Run("many_channels_80x24", func(t *testing.T) {
		x := newHarness(t, newEditor(t, baseConfig(team("Acme Corp")), manyChannels(600)), dark, 80, 24)
		x.press("pgdown", "pgdown", "pgdown")
		testutil.Golden(t, "many_channels_80x24", x.frame())
	})
}

// dmUsers and dmWorkspace are a workspace whose direct messages cover what
// the mapping view can tell from an export alone.
func dmUsers() []slack.User {
	u := func(id, name string) slack.User {
		return slack.User{ID: id, Name: strings.ToLower(name), Profile: slack.Profile{DisplayName: name}}
	}
	bot, guest, gone := u("UBOT", "deploybot"), u("UGUEST", "Farid Fofana"), u("UGONE", "Grace Garcia")
	bot.IsBot, guest.IsRestricted, gone.Deleted = true, true, true
	return []slack.User{u("UA", "Ada Abbott"), u("UB", "Ben Banerjee"), u("UC", "Chloe Castro"), bot, guest, gone}
}

func dmWorkspace() []slack.Channel {
	general := pub("general")
	general.IsGeneral = true
	return []slack.Channel{
		general, pub("random"), pub("sales-emea"),
		{ID: "D1", Kind: slack.KindDM, Members: []string{"UA", "UB"}},
		{ID: "D2", Kind: slack.KindDM, Members: []string{"UA", "UBOT"}},
		{ID: "D3", Kind: slack.KindDM, Members: []string{"UA"}},
		{ID: "D4", Kind: slack.KindDM, Members: []string{"UB", "UGUEST"}},
		{ID: "D5", Kind: slack.KindDM, Members: []string{"UC", "UGONE"}},
		{ID: "G1", Name: "mpdm-ada--ben--chloe-1", Kind: slack.KindMPIM, Members: []string{"UA", "UB", "UC"}},
	}
}

// Direct messages are shown, never edited, and only when the config file
// migrates them.
func TestDirectMessagesInTheView(t *testing.T) {
	m := twoTeams()
	m.Target.DirectMessages.Enabled = true
	ed := newEditor(t, m, dmWorkspace())
	ed.SetUsers(dmUsers())
	v := ed.View()
	if len(v.DirectMessages) != 6 {
		t.Fatalf("direct messages = %+v", v.DirectMessages)
	}
	got := map[string]string{}
	for i, d := range v.DirectMessages {
		got[d.SlackID] = d.Status()
		if d.Migrates != (i < 2) {
			t.Errorf("row %d (%s): migrating conversations come first", i, d.SlackID)
		}
	}
	want := map[string]string{
		"D1": "migrates", "G1": "migrates",
		"D2": "skipped: a participant is a bot or an app",
		"D3": "skipped: a conversation with oneself",
		"D4": "skipped: a participant is a guest in Slack",
		"D5": "skipped: a participant's Slack account is deactivated",
	}
	for id, status := range want {
		if got[id] != status {
			t.Errorf("%s: %q, want %q", id, got[id], status)
		}
	}
	if v.DirectMessages[0].Label != "Ada Abbott, Ben Banerjee" {
		t.Errorf("label = %q", v.DirectMessages[0].Label)
	}
	for _, s := range v.Skipped {
		if s.Kind == "dm" {
			t.Errorf("direct messages are listed as not migrating too: %+v", s)
		}
	}
	if !strings.Contains(v.Summary(), "2 of 6 direct messages into chats") {
		t.Errorf("summary = %q", v.Summary())
	}

	// No edit applies to one, by any of its names.
	for _, name := range []string{"D1", "Ada Abbott, Ben Banerjee", "mpdm-ada--ben--chloe-1"} {
		if err := ed.Exclude(name); err == nil {
			t.Errorf("Exclude(%q) was accepted", name)
		}
		if err := ed.Include(name, ""); err == nil {
			t.Errorf("Include(%q) was accepted", name)
		}
	}
	if ed.Dirty() {
		t.Error("refused edits left the session dirty")
	}
	// On the screen, pressing space on one says why it cannot be edited.
	x := newHarness(t, ed, theme.Default(), 100, 30).press("G", "space")
	if !strings.Contains(x.frame(), "Direct messages cannot be edited here") || ed.Dirty() {
		t.Errorf("space on a direct message:\n%s", x.frame())
	}

	// Off: one line under "Not migrating", as before, whatever the users.
	off := newEditor(t, twoTeams(), dmWorkspace())
	off.SetUsers(dmUsers())
	if v := off.View(); len(v.DirectMessages) != 0 || len(v.Skipped) != 1 || v.Skipped[0].Kind != "dm" {
		t.Errorf("direct messages off: %+v / %+v", v.DirectMessages, v.Skipped)
	}
}

func manyChannels(n int) []slack.Channel {
	out := make([]slack.Channel, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, pub(fmt.Sprintf("proj-%04d", i)))
	}
	return out
}

// Every frame must be exactly the size of the window in every state, with
// the chrome on the first and last rows: no flicker, no layout jumps.
func TestFrameIsStable(t *testing.T) {
	for _, size := range [][2]int{{60, 14}, {80, 24}, {97, 31}, {200, 60}} {
		w, h := size[0], size[1]
		fresh := func() *harness { return newHarness(t, newEditor(t, twoTeams(), workspace()), theme.Default(), w, h) }
		states := map[string]*harness{
			"initial":  fresh(),
			"edited":   fresh().press("down", "space", "down", "space"),
			"filter":   fresh().press("/").typeText("s"),
			"rename":   fresh().press("down", "down", "r").typeText(strings.Repeat("x", 90)),
			"rejected": fresh().press("down", "down", "r").typeText("/").press("enter"),
			"picker":   fresh().press("down", "down", "m"),
			"confirm":  fresh().press("down", "down", "space", "q"),
			"issues":   fresh().press("i"),
			"bottom":   fresh().press("G"),
			"many":     newHarness(t, newEditor(t, baseConfig(team("Acme Corp")), manyChannels(600)), theme.Default(), w, h).press("G"),
		}
		for name, x := range states {
			lines := strings.Split(x.frame(), "\n")
			if len(lines) != h {
				t.Errorf("%dx%d %s: frame has %d rows, want %d", w, h, name, len(lines), h)
				continue
			}
			for i, l := range lines {
				if got := ansi.StringWidth(l); got > w {
					t.Errorf("%dx%d %s: row %d is %d cells wide: %q", w, h, name, i, got, l)
				}
			}
			if !strings.Contains(lines[0], "taut") || !strings.Contains(lines[h-1], "quit") {
				t.Errorf("%dx%d %s: chrome moved:\n%s", w, h, name, x.frame())
			}
		}
	}
}

func TestASCIIThemeEmitsOnlyASCII(t *testing.T) {
	ascii := theme.New(theme.Options{Dark: true, Unicode: false})
	x := newHarness(t, newEditor(t, twoTeams(), workspace()), ascii, 80, 24).press("down", "down", "space", "down", "r")
	for _, r := range x.frame() {
		if r > 127 {
			t.Fatalf("non-ASCII rune %q in ASCII theme output:\n%s", r, x.frame())
		}
	}
}

func TestResizeKeepsCursorAndEdits(t *testing.T) {
	x := newHarness(t, newEditor(t, twoTeams(), workspace()), theme.Default(), 80, 24).press("down", "down", "space", "down")
	x.m, _ = x.m.Update(tea.WindowSizeMsg{Width: 132, Height: 43})
	out := x.frame()
	if !strings.Contains(out, "1 unsaved change") || !strings.Contains(out, "❯  ├─ #design") {
		t.Fatalf("state was lost on resize:\n%s", out)
	}
}

// --- behaviour ------------------------------------------------------------------

func TestQuitAsksOnlyWhenThereAreUnsavedChanges(t *testing.T) {
	x := newHarness(t, newEditor(t, twoTeams(), workspace()), theme.Default(), 80, 24)
	if x.press("q"); !x.quit {
		t.Fatal("q should quit at once when nothing changed")
	}

	x = newHarness(t, newEditor(t, twoTeams(), workspace()), theme.Default(), 80, 24).press("down", "down", "space", "q")
	if x.quit {
		t.Fatal("q quit with unsaved changes without asking")
	}
	if x.press("esc"); x.quit || !x.ed.Dirty() {
		t.Fatal("esc should return to editing with the changes intact")
	}
	if x.press("q", "d"); !x.quit || len(x.saved) != 0 {
		t.Fatalf("d should quit without saving (quit=%v saves=%d)", x.quit, len(x.saved))
	}

	x = newHarness(t, newEditor(t, twoTeams(), workspace()), theme.Default(), 80, 24).press("down", "down", "space", "q", "s")
	if !x.quit || len(x.saved) != 1 || x.ed.Dirty() || x.scr.Saves() != 1 {
		t.Fatalf("s should save then quit (quit=%v saves=%d dirty=%v)", x.quit, len(x.saved), x.ed.Dirty())
	}
	if got := x.saved[0].Target.Teams[0].Channels.Exclude; !reflect.DeepEqual(got, []string{"sales-*", "random"}) {
		t.Fatalf("saved exclude = %v", got)
	}
}

func TestSaveFailureIsShownAndKeepsChanges(t *testing.T) {
	ed := newEditor(t, twoTeams(), workspace())
	scr := mapping.NewScreen(ed, mapping.Options{File: "taut.yaml", Save: func(*config.Config) error {
		return fail.New("config.write_failed", "The config file could not be written to taut.yaml", "the disk is full", "Free some space, then save again.")
	}})
	x := &harness{t: t, ed: ed, scr: scr}
	x.m = shell.New(scr, shell.Options{Theme: theme.Default(), Command: "map", LockTheme: true})
	x.m, _ = x.m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	x.press("down", "down", "space", "s")
	if out := x.frame(); !strings.Contains(out, "could not be written") || !strings.Contains(out, "Free some space") {
		t.Fatalf("save failure not shown:\n%s", out)
	}
	if !ed.Dirty() || scr.Saves() != 0 {
		t.Fatal("a failed save must leave the changes unsaved")
	}
}

func TestUndoRestoresEachStep(t *testing.T) {
	x := newHarness(t, newEditor(t, twoTeams(), workspace()), theme.Default(), 80, 24)
	start := x.ed.Layout()
	x.press("down", "down", "space") // exclude #random
	x.press("r", "ctrl+u").typeText("Eng").press("enter")
	if n := len(x.ed.Unsaved()); n != 2 {
		t.Fatalf("unsaved = %d, want 2:\n%s", n, x.frame())
	}
	x.press("u")
	if n := len(x.ed.Unsaved()); n != 1 {
		t.Fatalf("after one undo unsaved = %d, want 1", n)
	}
	x.press("u", "u")
	if x.ed.Dirty() || !reflect.DeepEqual(start, x.ed.Layout()) {
		t.Fatalf("undo did not restore the original mapping:\n%s", x.frame())
	}
	if !strings.Contains(x.frame(), "Nothing to undo") {
		t.Fatalf("an extra undo should say so:\n%s", x.frame())
	}
}

func TestFilterNarrowsAndClears(t *testing.T) {
	x := newHarness(t, newEditor(t, twoTeams(), workspace()), theme.Default(), 80, 24).press("/").typeText("SALES").press("enter")
	out := x.frame()
	if strings.Contains(out, "#random") || !strings.Contains(out, "#sales-emea") || !strings.Contains(out, "filter SALES") {
		t.Fatalf("filter not applied:\n%s", out)
	}
	// Edits act on the filtered row.
	x.press("space")
	if c := x.ed.Unsaved(); len(c) != 1 || c[0].SlackName != "sales-emea" || c[0].Action != mapping.ActionExcluded {
		t.Fatalf("toggle under a filter changed %+v", c)
	}
	if out := x.press("esc").frame(); !strings.Contains(out, "#random") {
		t.Fatalf("esc did not clear the filter:\n%s", out)
	}
}

func TestIncludeAsksForATeamOnlyWhenItMustAsk(t *testing.T) {
	// #leadership is private; neither team takes private channels, so it
	// cannot be included and the screen must say why rather than pretend.
	x := newHarness(t, newEditor(t, twoTeams(), workspace()), theme.Default(), 100, 30).press("G", "up", "space")
	if out := x.frame(); !strings.Contains(out, "takes no private channels") || x.ed.Dirty() {
		t.Fatalf("expected an explanation and no change:\n%s", out)
	}

	// A channel excluded by name goes back to the team that excluded it.
	x = newHarness(t, newEditor(t, twoTeams(), workspace()), theme.Default(), 100, 30).press("down", "down", "space")
	x.press("G", "up", "up", "space")
	if x.ed.Dirty() {
		t.Fatalf("exclude then include should be a round trip:\n%s", x.frame())
	}
}

// --- editor ---------------------------------------------------------------------

func place(l *preflight.PlanLayout, name string) (team, target string) {
	for _, t := range l.Teams {
		for _, c := range t.Channels {
			if c.SlackName == name {
				return t.Name, c.TargetName
			}
		}
	}
	return "", ""
}

func code(err error) string {
	if err == nil {
		return ""
	}
	return fail.From(err).Code
}

func TestEditorEdits(t *testing.T) {
	ed := newEditor(t, twoTeams(), workspace())
	orig := ed.Config()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(ed.Exclude("#random"))
	must(ed.Rename("engineering", "Engineering Guild"))
	must(ed.Move("design", "sales"))
	// The catch-all team excludes sales-* by pattern; lifting that for one
	// channel is not expressible in the config file, so the move is refused.
	if err := ed.Move("sales-apac", "Acme Corp"); code(err) != "map.excluded_by_pattern" {
		t.Fatalf("move against an exclude pattern: %v", err)
	}

	l := ed.Layout()
	if team, _ := place(l, "random"); team != "" {
		t.Errorf("random still migrates to %q", team)
	}
	if team, name := place(l, "engineering"); team != "Acme Corp" || name != "Engineering Guild" {
		t.Errorf("engineering → %q / %q", team, name)
	}
	if team, _ := place(l, "design"); team != "Sales" {
		t.Errorf("design → %q, want Sales", team)
	}
	if team, _ := place(l, "sales-apac"); team != "Sales" {
		t.Errorf("sales-apac → %q, want it left in Sales", team)
	}
	if len(l.Blockers) != 0 {
		t.Errorf("edits introduced blockers: %+v", l.Blockers)
	}

	want := []mapping.Change{
		{SlackName: "random", Action: mapping.ActionExcluded, FromTeam: "Acme Corp", FromName: "random"},
		{SlackName: "engineering", Action: mapping.ActionRenamed, FromTeam: "Acme Corp", ToTeam: "Acme Corp", FromName: "engineering", ToName: "Engineering Guild"},
		{SlackName: "design", Action: mapping.ActionMoved, FromTeam: "Acme Corp", ToTeam: "Sales", FromName: "design", ToName: "design"},
	}
	if got := ed.Unsaved(); !reflect.DeepEqual(got, want) {
		t.Errorf("changes:\n got %+v\nwant %+v", got, want)
	}

	// The config file handed in is never modified.
	if !orig.Equal(twoTeams()) {
		t.Error("the original config file was modified")
	}
	// Only channel selections differ.
	a, b := *ed.Config(), *twoTeams()
	a.Target.Teams, b.Target.Teams = nil, nil
	if !a.Equal(&b) {
		t.Error("an edit touched something other than target.teams")
	}
	for i, team := range ed.Config().Target.Teams {
		was := twoTeams().Target.Teams[i]
		team.Channels, was.Channels = config.ChannelSelection{}, config.ChannelSelection{}
		if !reflect.DeepEqual(team, was) {
			t.Errorf("team %d changed outside its channel selection", i)
		}
	}
	if err := ed.Config().Validate().Err(); err != nil {
		t.Errorf("edited config file is invalid: %v", err)
	}
}

func TestEditorRefusals(t *testing.T) {
	cases := []struct {
		name string
		do   func(*mapping.Editor) error
		code string
	}{
		{"unknown channel", func(e *mapping.Editor) error { return e.Exclude("nope") }, "map.unknown_channel"},
		{"unknown team", func(e *mapping.Editor) error { return e.Move("random", "Nope") }, "map.unknown_team"},
		{"forbidden characters", func(e *mapping.Editor) error { return e.Rename("random", "a/b") }, "map.invalid_name"},
		{"reserved name", func(e *mapping.Editor) error { return e.Rename("random", "forms") }, "map.invalid_name"},
		{"too long", func(e *mapping.Editor) error { return e.Rename("random", strings.Repeat("x", 51)) }, "map.invalid_name"},
		{"taken by another channel", func(e *mapping.Editor) error { return e.Rename("random", "design") }, "map.name_taken"},
		{"taken by General", func(e *mapping.Editor) error { return e.Rename("random", "general") }, "map.name_taken"},
		{"general itself", func(e *mapping.Editor) error { return e.Rename("general", "Lobby") }, "map.general"},
		{"not migrating", func(e *mapping.Editor) error { return e.Rename("leadership", "Leads") }, "map.not_migrating"},
		{"kind not selected", func(e *mapping.Editor) error { return e.Include("leadership", "Sales") }, "map.kind_not_selected"},
		{"excluded by a pattern", func(e *mapping.Editor) error { return e.Move("sales-emea", "Acme Corp") }, "map.excluded_by_pattern"},
		{"dm", func(e *mapping.Editor) error { return e.Exclude("D1") }, "map.unknown_channel"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ed := newEditor(t, twoTeams(), workspace())
			err := c.do(ed)
			fe := fail.From(err)
			if code(err) != c.code {
				t.Fatalf("code = %q (%v), want %q", code(err), err, c.code)
			}
			if fe.Exit != fail.ExitUsage || fe.Next == "" {
				t.Errorf("refusal needs exit 2 and a next step: %+v", fe)
			}
			if ed.Dirty() || ed.CanUndo() {
				t.Error("a refused edit changed the config file")
			}
		})
	}
}

func TestEditorNoOpsAndIdempotence(t *testing.T) {
	ed := newEditor(t, twoTeams(), workspace())
	for name, err := range map[string]error{
		"exclude skipped":  ed.Exclude("leadership"),
		"include included": ed.Include("random", ""),
		"move to own team": ed.Move("random", "Acme Corp"),
		"rename to itself": ed.Rename("random", "random"),
	} {
		if !errors.Is(err, mapping.ErrNoChange) {
			t.Errorf("%s: %v, want ErrNoChange", name, err)
		}
	}
	if ed.Dirty() {
		t.Fatal("no-ops made the editor dirty")
	}

	// Toggling a channel out and back in leaves the config file as it was,
	// including its rename.
	if err := ed.Rename("random", "Water Cooler"); err != nil {
		t.Fatal(err)
	}
	renamed := ed.Config()
	if err := ed.Exclude("random"); err != nil {
		t.Fatal(err)
	}
	if err := ed.Include("random", ""); err != nil {
		t.Fatal(err)
	}
	if !ed.Config().Equal(renamed) {
		t.Fatal("exclude then include did not round-trip")
	}
	if err := ed.Rename("random", ""); err != nil || ed.Dirty() {
		t.Fatalf("an empty name should remove the rename (err=%v dirty=%v)", err, ed.Dirty())
	}
}

func TestEditorResolvesAmbiguity(t *testing.T) {
	// Both teams claim sales-*: a blocker until each channel has one home.
	m := baseConfig(team("Acme Corp"), team("Sales", "sales-*"))
	ed := newEditor(t, m, workspace())
	if n := len(ed.Layout().Blockers); n != 2 {
		t.Fatalf("blockers = %d, want 2", n)
	}
	if err := ed.Move("sales-emea", "Sales"); err != nil {
		t.Fatal(err)
	}
	if err := ed.Exclude("sales-apac"); err != nil {
		t.Fatal(err)
	}
	l := ed.Layout()
	if len(l.Blockers) != 0 {
		t.Fatalf("blockers remain: %+v", l.Blockers)
	}
	if team, _ := place(l, "sales-emea"); team != "Sales" {
		t.Errorf("sales-emea → %q", team)
	}
	if team, _ := place(l, "sales-apac"); team != "" {
		t.Errorf("sales-apac → %q, want excluded everywhere", team)
	}
}

func TestMoveCarriesTheRename(t *testing.T) {
	ed := newEditor(t, twoTeams(), workspace())
	if err := ed.Rename("design", "Studio"); err != nil {
		t.Fatal(err)
	}
	if err := ed.Move("design", "Sales"); err != nil {
		t.Fatal(err)
	}
	if team, name := place(ed.Layout(), "design"); team != "Sales" || name != "Studio" {
		t.Fatalf("design → %q / %q, want Sales / Studio", team, name)
	}
	if r := ed.Config().Target.Teams[0].Channels.Rename; len(r) != 0 {
		t.Fatalf("the rename was left behind in the old team: %v", r)
	}
}

func TestGlobCharactersInNamesAreMatchedLiterally(t *testing.T) {
	odd := pub("what[is]this*")
	channels := append(workspace(), odd, pub("whatiithis"))
	ed := newEditor(t, baseConfig(team("Acme Corp")), channels)
	if err := ed.Exclude(odd.Name); err != nil {
		t.Fatal(err)
	}
	l := ed.Layout()
	if team, _ := place(l, odd.Name); team != "" {
		t.Errorf("%s still migrates", odd.Name)
	}
	if team, _ := place(l, "whatiithis"); team == "" {
		t.Error("excluding one channel took a different one with it")
	}
	if err := ed.Include(odd.Name, ""); err != nil || ed.Dirty() {
		t.Fatalf("include after exclude: err=%v dirty=%v", err, ed.Dirty())
	}
}

// The view must show exactly what Layout computed, with nothing invented.
func TestViewMirrorsLayout(t *testing.T) {
	ed := newEditor(t, twoTeams(), workspace())
	_ = ed.Exclude("random")
	_ = ed.Rename("design", "Studio")
	l, v := ed.Layout(), ed.View()
	if len(v.Teams) != len(l.Teams) || len(v.Skipped) != len(l.Skipped) {
		t.Fatalf("view has %d teams/%d skipped, layout %d/%d", len(v.Teams), len(v.Skipped), len(l.Teams), len(l.Skipped))
	}
	for i, team := range l.Teams {
		for j, c := range team.Channels {
			got := v.Teams[i].Channels[j]
			if got.SlackID != c.SlackID || got.SlackName != c.SlackName || got.TargetName != c.TargetName || got.Renamed != c.Renamed || got.Archived != c.Archived {
				t.Errorf("team %d channel %d: view %+v, layout %+v", i, j, got, c)
			}
		}
	}
	for i, s := range l.Skipped {
		if got := v.Skipped[i]; got.SlackName != s.SlackName || got.Reason != s.Reason || got.Kind != s.Kind {
			t.Errorf("skipped %d: view %+v, layout %+v", i, got, s)
		}
	}
	forced := map[string]bool{}
	for _, c := range v.Teams[0].Channels {
		forced[c.SlackName] = c.Forced
	}
	if !forced["_archive"] || !forced["forms"] || forced["design"] || forced["general"] {
		t.Errorf("forced-name markers are wrong: %v", forced)
	}
}
