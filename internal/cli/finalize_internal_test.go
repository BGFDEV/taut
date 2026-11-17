package cli

import (
	"strings"
	"testing"

	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/tui/theme"
)

// The confirmation is the last thing read before the step that cannot be
// undone. It must say who gets access, and under the legacy strategy that
// the team itself is completed.
func TestFinalizeConsequences(t *testing.T) {
	modern, legacy, chats := finalizeConsequences(false, true, false, false), finalizeConsequences(true, true, false, false), finalizeConsequences(false, true, true, false)
	for _, s := range []string{modern, legacy, chats} {
		for _, want := range []string{"cannot be undone", "everyone in its channels", "Members of a private channel are added to that channel, and to its team"} {
			if !strings.Contains(s, want) {
				t.Errorf("the confirmation does not say %q: %s", want, s)
			}
		}
		if strings.Contains(s, "standard channels") {
			t.Errorf("the confirmation still limits members to standard channels: %s", s)
		}
	}
	// With direct messages: their chats were never closed to anybody, and
	// the confirmation says finalizing changes nothing about who sees them.
	for _, want := range []string{"already visible to their participants", "only ends their migration mode", "adds nobody"} {
		if !strings.Contains(chats, want) {
			t.Errorf("the confirmation for chats does not say %q: %s", want, chats)
		}
	}
	if strings.Contains(modern, "direct messages") || strings.Contains(legacy, "direct messages") {
		t.Errorf("a confirmation without direct messages mentions them: %s", modern)
	}
	if strings.Contains(modern, "legacy") {
		t.Errorf("the modern confirmation mentions legacy: %s", modern)
	}
	for _, want := range []string{"the team's own migration is completed as well", "cannot be put back into migration mode"} {
		if !strings.Contains(legacy, want) {
			t.Errorf("the legacy confirmation does not say %q: %s", want, legacy)
		}
	}
}

// What is not being finalized is not promised: with only chats no team gets
// anybody, and with channels left locked their teams get nobody yet.
func TestFinalizeConsequencesOfAPart(t *testing.T) {
	only := finalizeConsequences(false, false, true, false)
	if strings.Contains(only, "gets its members") || strings.Contains(only, "everyone in its channels") {
		t.Errorf("finalizing chats alone promises members: %s", only)
	}
	for _, want := range []string{"cannot be undone", "adds nobody", "No channel is opened and no members are added"} {
		if !strings.Contains(only, want) {
			t.Errorf("the confirmation for chats alone does not say %q: %s", want, only)
		}
	}
	// The same under the legacy strategy: no team is completed by it.
	if s := finalizeConsequences(true, false, true, false); strings.Contains(s, "legacy") {
		t.Errorf("finalizing chats alone speaks of the team: %s", s)
	}
	part := finalizeConsequences(false, true, false, true)
	if strings.Contains(part, "Each team then gets its members") || !strings.Contains(part, "a team with a channel left locked gets none yet") {
		t.Errorf("with channels left locked: %s", part)
	}
}

// A chat is not a channel left locked, and a skipped conversation has
// nothing to finalize.
func TestScopeCountsChatsApart(t *testing.T) {
	p := &store.Progress{Channels: []store.ChannelProgress{
		{SlackID: "C1", Kind: "public", Team: "T", State: store.ChannelMigrating, Posted: 10},
		{SlackID: "C2", Kind: "private", Team: "T", State: store.ChannelMigrating, Posted: 5},
		{SlackID: "C3", Kind: "public", Team: "U", State: store.ChannelCompleted},
		{SlackID: "D1", Kind: store.KindDM, State: store.ChannelMigrating, Posted: 3},
		{SlackID: "D2", Kind: store.KindDM, State: store.ChannelCreated},
		{SlackID: "G1", Kind: store.KindMPIM, State: store.ChannelSkipped, Failed: 7},
		{SlackID: "D3", Kind: store.KindDM, State: store.ChannelCompleted},
	}}
	channels, _ := finalizeOnly(p, "channels")
	if s := scopeOf(p, channels); s.Channels != 3 || s.Open != 2 || s.Chats != 0 || s.Remaining != 0 || s.RemainingChats != 2 || s.Skipped != 0 || len(s.Teams) != 2 {
		t.Errorf("--only channels: %+v", s)
	}
	chats, _ := finalizeOnly(p, "direct-messages")
	if s := scopeOf(p, chats); s.Channels != 4 || s.Open != 2 || s.Chats != 4 || s.OpenChats != 2 || s.Remaining != 2 || s.RemainingChats != 0 || s.Skipped != 1 || len(s.Teams) != 0 {
		t.Errorf("--only direct-messages: %+v", s)
	}
	if s := scopeOf(p, nil); s.Open != 4 || s.OpenChats != 2 || s.Remaining != 0 || s.RemainingChats != 0 || s.Skipped != 1 {
		t.Errorf("everything: %+v", s)
	}
	if s := scopeOf(p, []string{"C1"}); s.Open != 1 || s.Remaining != 1 || s.RemainingChats != 2 {
		t.Errorf("one channel: %+v", s)
	}
	if !chatsOnly(p, chats) || chatsOnly(p, channels) || chatsOnly(p, []string{"D1", "C1"}) || chatsOnly(p, nil) {
		t.Error("chatsOnly does not tell chats from channels")
	}
}

// `finalize --only channels` leaves the chats alone; the confirmation does
// not call them channels left locked, nor say their teams get members later.
func TestFinalizeRowsCountChatsApart(t *testing.T) {
	flat := func(rows [][2]string) string {
		var b strings.Builder
		for _, r := range rows {
			b.WriteString(r[0] + ": " + r[1] + "\n")
		}
		return b.String()
	}
	got := flat(finalizeRows(finalizeScope{Channels: 3, Open: 3, Teams: []string{"T"}, Posted: 10, RemainingChats: 2}, false, false))
	for _, want := range []string{"Channels: 3 to open in 1 team", "Left in migration mode: 2 chats of direct messages"} {
		if !strings.Contains(got, want) {
			t.Errorf("the confirmation does not say %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Left locked") || strings.Contains(got, "members later") {
		t.Errorf("chats are counted as channels left locked:\n%s", got)
	}
	// Under the legacy strategy the team is completed although chats remain.
	got = flat(finalizeRows(finalizeScope{Channels: 3, Open: 3, Teams: []string{"T"}, RemainingChats: 2}, true, false))
	if !strings.Contains(got, "Team: completed too") {
		t.Errorf("chats left in migration mode hold the team back:\n%s", got)
	}
	got = flat(finalizeRows(finalizeScope{Channels: 3, Open: 2, Chats: 2, OpenChats: 1, Skipped: 1, Remaining: 2}, true, true))
	for _, want := range []string{"Channels: 1 to open", "Direct messages: 1 chat to take out of migration mode", "Left locked: 2 channels",
		"Skipped: 1 direct-message conversation", "Team: stays in migration mode", "Staging: kept"} {
		if !strings.Contains(got, want) {
			t.Errorf("the confirmation does not say %q:\n%s", want, got)
		}
	}
}

func TestFinalizeQuestionAndCounts(t *testing.T) {
	for _, c := range []struct {
		scope finalizeScope
		want  string
	}{
		{finalizeScope{Channels: 3, Open: 3}, "Open 3 channels to their members?"},
		{finalizeScope{Channels: 1, Open: 1}, "Open 1 channel to their members?"},
		{finalizeScope{Channels: 5, Open: 5, Chats: 2, OpenChats: 2}, "Open 3 channels to their members, and end migration mode on 2 chats?"},
		{finalizeScope{Channels: 2, Open: 2, Chats: 2, OpenChats: 2}, "End migration mode on 2 chats?"},
		{finalizeScope{Channels: 4, Open: 1, Chats: 1, OpenChats: 1}, "End migration mode on 1 chat?"},
	} {
		if got := finalizeQuestion(c.scope); got != c.want {
			t.Errorf("finalizeQuestion(%+v) = %q, want %q", c.scope, got, c.want)
		}
	}
	for _, c := range []struct {
		channels, chats int
		want            string
	}{
		{3, 0, "3 channels"}, {1, 0, "1 channel"}, {0, 2, "2 direct-message conversations"},
		{3, 1, "3 channels, 1 direct-message conversation"}, {0, 0, "0 channels"},
	} {
		if got := conversations(c.channels, c.chats); got != c.want {
			t.Errorf("conversations(%d, %d) = %q, want %q", c.channels, c.chats, got, c.want)
		}
	}
}

// A conversation that was skipped is said so where the run is summed up,
// and a run that was abandoned shows its chats being released.
func TestSummaryNamesSkippedConversations(t *testing.T) {
	p := &store.Progress{
		Stages: []store.StageProgress{{Stage: store.StagePlan, Lanes: 1, Done: 1}, {Stage: store.StageLoad, Lanes: 2, Done: 2}},
		Channels: []store.ChannelProgress{
			{SlackID: "C1", SlackName: "general", Kind: "public", State: store.ChannelMigrating, Posts: 10, Posted: 10},
			{SlackID: "G1", Kind: store.KindMPIM, TargetName: "A, B, C", State: store.ChannelSkipped, SkipCode: "graph.chat_refused", Posts: 4, Failed: 4},
		},
		Posts: 14, Posted: 10, Failed: 4,
	}
	th := theme.New(theme.Options{})
	out := migrateSummary(th, controller.RunView{ID: "r", Status: store.RunCompleted, Progress: p}, 0, nil, 100)
	for _, want := range []string{"1 direct-message conversation skipped", "Skipped", "taut audit --op chat.skip"} {
		if !strings.Contains(out, want) {
			t.Errorf("the summary does not say %q:\n%s", want, out)
		}
	}
	out = finalizeSummary(th, controller.RunView{ID: "r", Status: store.RunCompleted, Progress: p}, 0, 0, false, nil, 100)
	if !strings.Contains(out, "1 direct-message conversation skipped") || !strings.Contains(out, "1 conversation still in migration mode") {
		t.Errorf("the finalize summary counts a skipped conversation as open:\n%s", out)
	}

	// Only chats finalized so far: that is not "0 channels open".
	only := &store.Progress{Channels: []store.ChannelProgress{
		{SlackID: "C1", SlackName: "general", Kind: "public", State: store.ChannelMigrating},
		{SlackID: "D1", Kind: store.KindDM, State: store.ChannelCompleted},
	}}
	out = finalizeSummary(th, controller.RunView{ID: "r", Status: store.RunCompleted, Progress: only}, 0, 0, false, nil, 100)
	if !strings.Contains(out, "Chats finalized") || strings.Contains(out, "0 channels open") || !strings.Contains(out, "1 chat out of migration mode") {
		t.Errorf("the summary after finalizing only chats:\n%s", out)
	}

	p.Stages = append(p.Stages, store.StageProgress{Stage: store.StageRelease, Lanes: 2, Done: 1, Leased: 1})
	out = migrateView(th, controller.RunView{ID: "r", Status: store.RunCancelled, Progress: p}, 100, "*", 0)
	if !strings.Contains(out, "Ending migration mode on chats") || !strings.Contains(out, "1 of 2 chats") {
		t.Errorf("the view does not show the chats being released:\n%s", out)
	}
}
