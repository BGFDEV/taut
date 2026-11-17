package preflight_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/fixtures"
	"github.com/bgfdev/taut/internal/preflight"
	"github.com/bgfdev/taut/internal/slack"
	"github.com/bgfdev/taut/internal/slack/export"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/transform"
)

// baseOf builds the ledger a finished first pass over a fixture would
// leave: every importable message of every channel the config selects is
// posted, with the edit marker it had, in a channel of the team "Fixture".
func baseOf(t *testing.T, fixture string, status store.RunStatus, mod func(*config.Config)) *preflight.Base {
	t.Helper()
	o := setup(t, fixture, mod)
	rep := run(t, o)
	users, err := o.Export.Users()
	if err != nil {
		t.Fatal(err)
	}
	channels, err := o.Export.Channels()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]slack.Channel{}
	for _, ch := range channels {
		byID[ch.ID] = ch
	}
	state := store.ChannelMigrating
	if status == store.RunFinalized {
		state = store.ChannelCompleted
	}
	l := store.Ledger{RunID: "base-run", Pass: 1, Status: status, Planned: true, WorkspaceID: slack.WorkspaceID(users)}
	keys := map[string]map[string]*string{}
	for _, u := range users {
		l.Users = append(l.Users, u.ID)
	}
	for _, team := range rep.Teams {
		l.Teams = append(l.Teams, store.LedgerTeam{Name: team.Name, TeamsID: "team-" + team.Name})
		for _, c := range team.Channels {
			lc := store.LedgerChannel{SlackID: c.SlackID, SlackName: c.SlackName, Kind: c.Kind, Team: team.Name,
				TargetName: c.TargetName, State: state, InTeams: true}
			keys[c.SlackID] = map[string]*string{}
			for msg, err := range o.Export.Messages(byID[c.SlackID]) {
				if err != nil || transform.Classify(msg).Disposition == transform.Skip {
					continue
				}
				edited := ""
				if msg.Edited != nil {
					edited = msg.Edited.TS
				}
				keys[c.SlackID][msg.TS] = &edited
				lc.Posted++
			}
			l.Posted += lc.Posted
			l.Channels = append(l.Channels, lc)
		}
	}
	return &preflight.Base{
		Ledger: l, TenantID: o.Config.Target.Entra.TenantID,
		Keys: func(_ context.Context, slackID string, fn func(string, *string) error) error {
			for ts, e := range keys[slackID] {
				if err := fn(ts, e); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// publicNew is what a later fixture adds to the public channels: its facts
// count private channels and direct messages too.
func publicNew(t *testing.T, base string) (added int) {
	t.Helper()
	return channelMessages(fixtures.Facts(fixtures.LaterOf(base))) - channelMessages(fixtures.Facts(base)) + publicDeleted(base)
}

// publicDeleted is how many public-channel messages of the base the later
// export no longer has. The later fixtures delete in public channels only.
func publicDeleted(base string) int { return fixtures.LaterFacts(fixtures.LaterOf(base)).Deleted }

func TestIncrementalCountsOnlyWhatIsNew(t *testing.T) {
	for _, fixture := range []string{"small", "medium"} {
		t.Run(fixture, func(t *testing.T) {
			facts := fixtures.LaterFacts(fixtures.LaterOf(fixture))
			o := setup(t, fixtures.LaterOf(fixture), nil)
			o.Base = baseOf(t, fixture, store.RunCompleted, nil)
			r := run(t, o)
			inc := r.Incremental
			if inc == nil {
				t.Fatal("no incremental section")
			}
			if r.Verdict == preflight.VerdictBlocked {
				t.Fatalf("blocked: %+v", r.Blockers)
			}
			added := publicNew(t, fixture)
			if inc.New.Messages != added || r.Totals.Messages != added || r.Estimate.Imports != added {
				t.Errorf("new messages: incremental %d, totals %d, estimate %d; want %d", inc.New.Messages, r.Totals.Messages, r.Estimate.Imports, added)
			}
			if inc.Pass != 2 || inc.BaseRun != "base-run" || inc.BaseStatus != "completed" || !inc.SameWorkspace {
				t.Errorf("incremental header = %+v", inc)
			}
			if inc.Already.Messages != o.Base.Ledger.Posted || inc.Already.Conversations != len(o.Base.Ledger.Channels) {
				t.Errorf("already = %+v, ledger has %d posts in %d channels", inc.Already, o.Base.Ledger.Posted, len(o.Base.Ledger.Channels))
			}
			if inc.Edited != facts.Edited || inc.Deleted != facts.Deleted {
				t.Errorf("edited %d, deleted %d; the fixture edits %d and deletes %d", inc.Edited, inc.Deleted, facts.Edited, facts.Deleted)
			}
			if inc.New.Channels != facts.NewChannels || inc.New.People != facts.NewUsers || inc.New.RepliesToMigrated == 0 || inc.New.Files == 0 {
				t.Errorf("new = %+v; the fixture adds %d channels and %d people", inc.New, facts.NewChannels, facts.NewUsers)
			}
			// Nothing is finalized, so nothing is reopened; and what cannot
			// be carried over is said.
			if inc.Reopened != 0 || !strings.Contains(codes(r.Warnings), "incremental.edits_not_synced") || strings.Contains(codes(r.Warnings), "incremental.reopens") {
				t.Errorf("reopened %d, warnings %s", inc.Reopened, codes(r.Warnings))
			}
			already, fresh := 0, 0
			for _, c := range inc.Channels {
				already += c.Already
				fresh += c.New
				if c.SlackName == facts.QuietChannel && c.New != 0 {
					t.Errorf("the quiet channel has %d new messages", c.New)
				}
				if c.SlackName == facts.NewChannel && (c.Continued || c.Already != 0) {
					t.Errorf("the new channel: %+v", c)
				}
			}
			if fresh != added || already != o.Base.Ledger.Posted-publicDeleted(fixture) {
				t.Errorf("per channel: %d new and %d already; want %d and %d", fresh, already, added, o.Base.Ledger.Posted-publicDeleted(fixture))
			}
			// The team is the ledger's own: its existence is not a warning.
			o.ExistingTeams = []string{"Fixture"}
			if r := run(t, o); strings.Contains(codes(r.Warnings), "team.exists") {
				t.Errorf("a team the earlier passes created is reported as a collision: %s", codes(r.Warnings))
			}
		})
	}
}

func TestIncrementalNothingNew(t *testing.T) {
	o := setup(t, "small", nil)
	o.Base = baseOf(t, "small", store.RunCompleted, nil)
	r := run(t, o)
	inc := r.Incremental
	if inc.New.Messages != 0 || inc.New.Channels != 0 || inc.Edited != 0 || inc.Deleted != 0 || r.Totals.Messages != 0 {
		t.Fatalf("the same export again: %+v, totals %+v", inc, r.Totals)
	}
	if r.Verdict == preflight.VerdictBlocked || !strings.Contains(codes(r.Notes), "incremental.nothing_new") {
		t.Errorf("verdict %s, notes %s", r.Verdict, codes(r.Notes))
	}
	if n := item(r, "reactions"); n != 0 {
		t.Errorf("content counts %d messages with reactions, all of which are already in Teams", n)
	}
}

func TestIncrementalAfterFinalize(t *testing.T) {
	facts := fixtures.LaterFacts("small-later")
	o := setup(t, "small-later", nil)
	o.Base = baseOf(t, "small", store.RunFinalized, nil)
	r := run(t, o)
	inc := r.Incremental
	// Every base channel but the quiet one has new messages.
	if want := len(o.Base.Ledger.Channels) - 1; inc.Reopened != want || inc.BaseStatus != "finalized" {
		t.Fatalf("reopened %d channels of a %s base, want %d", inc.Reopened, inc.BaseStatus, want)
	}
	if r.Verdict == preflight.VerdictBlocked || !strings.Contains(codes(r.Warnings), "incremental.reopens") {
		t.Errorf("verdict %s, warnings %s", r.Verdict, codes(r.Warnings))
	}
	for _, c := range inc.Channels {
		if c.SlackName == facts.QuietChannel && c.Reopens {
			t.Error("the quiet channel would be reopened")
		}
	}

	// The legacy strategy cannot reopen a channel, and cannot add one to a
	// team that has left migration mode.
	legacy := func(m *config.Config) { m.Load.Strategy = config.StrategyLegacy }
	o = setup(t, "small-later", legacy)
	o.Base = baseOf(t, "small", store.RunFinalized, legacy)
	if r := run(t, o); r.Verdict != preflight.VerdictBlocked || !strings.Contains(codes(r.Blockers), "incremental.legacy_finalized") {
		t.Errorf("legacy after a finalize: verdict %s, blockers %s", r.Verdict, codes(r.Blockers))
	}
	// Before the finalize the legacy strategy has no such problem.
	o = setup(t, "small-later", legacy)
	o.Base = baseOf(t, "small", store.RunCompleted, legacy)
	if r := run(t, o); strings.Contains(codes(r.Blockers), "incremental") {
		t.Errorf("legacy before a finalize: blockers %s", codes(r.Blockers))
	}
	// And a finalized base with nothing new is no problem for it either.
	o = setup(t, "small", legacy)
	o.Base = baseOf(t, "small", store.RunFinalized, legacy)
	if r := run(t, o); strings.Contains(codes(r.Blockers), "incremental") {
		t.Errorf("legacy with nothing new: blockers %s", codes(r.Blockers))
	}
}

func TestIncrementalRefusesWhatWouldSplitHistory(t *testing.T) {
	cases := []struct {
		name string
		// base changes the ledger; mod the new pass's config.
		base func(*preflight.Base)
		mod  func(*config.Config)
		code string
	}{
		{"base still running", func(b *preflight.Base) { b.Ledger.Status = store.RunRunning }, nil, "incremental.base_not_finished"},
		{"base failed", func(b *preflight.Base) { b.Ledger.Status = store.RunFailed }, nil, "incremental.base_not_finished"},
		{"another tenant", func(b *preflight.Base) { b.TenantID = "11111111-2222-4333-8444-555555555555" }, nil, "incremental.tenant_changed"},
		{"channel left out", nil, func(m *config.Config) { m.Target.Teams[0].Channels.Exclude = []string{"random"} }, "incremental.channel_dropped"},
		{"channel moved", nil, func(m *config.Config) {
			m.Target.Teams[0].Channels.Exclude = []string{"random"}
			other := config.DefaultTeam()
			other.Name, other.Owner = "Elsewhere", owner
			other.Channels.Include = []string{"random"}
			m.Target.Teams = append(m.Target.Teams, other)
		}, "incremental.channel_moved"},
		{"team renamed", nil, func(m *config.Config) { m.Target.Teams[0].Name = "Fixture Two" }, "incremental.channel_moved"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := setup(t, "small-later", c.mod)
			o.Base = baseOf(t, "small", store.RunCompleted, nil)
			if c.base != nil {
				c.base(o.Base)
			}
			r := run(t, o)
			if r.Verdict != preflight.VerdictBlocked || !strings.Contains(codes(r.Blockers), c.code) {
				t.Fatalf("verdict %s, blockers %s; want %s", r.Verdict, codes(r.Blockers), c.code)
			}
			for _, b := range r.Blockers {
				if strings.HasPrefix(b.Code, "incremental.") && (b.Message == "" || b.Next == "") {
					t.Errorf("blocker %s has no cause or no next step: %+v", b.Code, b)
				}
			}
		})
	}
}

func TestIncrementalTeamGoneFromTheConfig(t *testing.T) {
	// The base put #random into a second team; the new config has only
	// the first, and selects #random nowhere.
	two := func(m *config.Config) {
		m.Target.Teams[0].Channels.Exclude = []string{"random"}
		other := config.DefaultTeam()
		other.Name, other.Owner = "Elsewhere", owner
		other.Channels.Include = []string{"random"}
		m.Target.Teams = append(m.Target.Teams, other)
	}
	o := setup(t, "small-later", func(m *config.Config) { m.Target.Teams[0].Channels.Exclude = []string{"random"} })
	o.Base = baseOf(t, "small", store.RunCompleted, two)
	r := run(t, o)
	if !strings.Contains(codes(r.Blockers), "incremental.team_dropped") {
		t.Fatalf("blockers %s", codes(r.Blockers))
	}
}

func TestIncrementalAnotherWorkspace(t *testing.T) {
	// The ledger of a different workspace: other conversation IDs, another
	// workspace ID.
	o := setup(t, "small-later", nil)
	o.Base = baseOf(t, "small", store.RunCompleted, nil)
	o.Base.Ledger.WorkspaceID = "T0ELSEWHERE"
	for i := range o.Base.Ledger.Channels {
		o.Base.Ledger.Channels[i].SlackID = "C0QZ" + o.Base.Ledger.Channels[i].SlackID
	}
	r := run(t, o)
	if r.Verdict != preflight.VerdictBlocked || codes(r.Blockers) != "incremental.workspace_mismatch" {
		t.Fatalf("verdict %s, blockers %s", r.Verdict, codes(r.Blockers))
	}
	inc := r.Incremental
	if inc.SameWorkspace || inc.New.Messages != 0 || len(inc.Channels) != 0 || r.Totals.Messages != 0 {
		t.Errorf("another workspace's export was counted as new content: %+v", inc)
	}
	if !strings.Contains(r.Blockers[0].Message, "T0ELSEWHERE") || !strings.Contains(r.Blockers[0].Message, "T0FIXTURE") {
		t.Errorf("the blocker does not name the two workspaces: %s", r.Blockers[0].Message)
	}

	// Without a workspace ID on either side, the conversations decide.
	o = setup(t, "small-later", nil)
	o.Base = baseOf(t, "small", store.RunCompleted, nil)
	o.Base.Ledger.WorkspaceID = ""
	if r := run(t, o); r.Verdict == preflight.VerdictBlocked {
		t.Errorf("a ledger with no workspace ID and shared conversations is refused: %s", codes(r.Blockers))
	}
}

// A channel the earlier passes migrated that the newer export does not list
// was deleted in Slack. It is not a reason to refuse, and it is counted.
func TestIncrementalChannelGoneFromSlack(t *testing.T) {
	path, err := fixtures.Write("small-later", t.TempDir(), fixtures.Without("random"))
	if err != nil {
		t.Fatal(err)
	}
	o := setup(t, "small-later", nil)
	ex, err := export.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ex.Close() })
	o.Export = ex
	o.Base = baseOf(t, "small", store.RunCompleted, nil)
	r := run(t, o)
	if r.Verdict == preflight.VerdictBlocked {
		t.Fatalf("blockers %s", codes(r.Blockers))
	}
	if r.Incremental.Dormant != 1 {
		t.Errorf("dormant = %d, want 1", r.Incremental.Dormant)
	}
}

func TestSameWorkspace(t *testing.T) {
	cases := []struct {
		name               string
		earlierID          string
		earlier            []string
		exportID           string
		exported           []string
		same               bool
		shared, earlierLen int
	}{
		{"shared conversation, same ID", "T1", []string{"C1", "C2"}, "T1", []string{"C2", "C3"}, true, 1, 2},
		{"shared conversation, different ID", "T1", []string{"C1"}, "T2", []string{"C1"}, true, 1, 1},
		{"no shared conversation, same ID", "T1", []string{"C1"}, "T1", []string{"C9"}, true, 0, 1},
		{"no shared conversation, different ID", "T1", []string{"C1"}, "T2", []string{"C9"}, false, 0, 1},
		{"no shared conversation, no IDs", "", []string{"C1"}, "", []string{"C9"}, false, 0, 1},
		{"no shared conversation, one ID missing", "T1", []string{"C1"}, "", []string{"C9"}, false, 0, 1},
		{"nothing earlier", "T1", nil, "T2", []string{"C9"}, true, 0, 0},
	}
	for _, c := range cases {
		got := slack.SameWorkspace(c.earlierID, c.earlier, c.exportID, c.exported)
		if got.Same != c.same || got.Shared != c.shared || got.Earlier != c.earlierLen {
			t.Errorf("%s: %+v", c.name, got)
		}
	}
	users := []slack.User{{ID: "U1", TeamID: "T1"}, {ID: "U2", TeamID: "T1"}, {ID: "U3", TeamID: "T9", IsStranger: true}, {ID: "U4", TeamID: "T9", IsStranger: true}, {ID: "U5", TeamID: "T9", IsStranger: true}, {ID: "U6"}}
	if id := slack.WorkspaceID(users); id != "T1" {
		t.Errorf("WorkspaceID = %q, want T1 (Slack Connect users do not count)", id)
	}
	if id := slack.WorkspaceID([]slack.User{{ID: "U1", TeamID: "TB"}, {ID: "U2", TeamID: "TA"}}); id != "TA" {
		t.Errorf("WorkspaceID on a tie = %q, want the smaller", id)
	}
	if id := slack.WorkspaceID([]slack.User{{ID: "U1"}}); id != "" {
		t.Errorf("WorkspaceID with no team IDs = %q", id)
	}
}
