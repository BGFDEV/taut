package worker

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/graph"
	"github.com/bgfdev/taut/internal/pipeline"
	"github.com/bgfdev/taut/internal/store"
)

// The members lane's cursor holds who was there before Taut added anyone.
// It must tell "not read yet" from "read, and nobody was there", and still
// read the shape it had before private channels.
func TestMembersCursor(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      membersCursor
		bad       bool
	}{
		{"never written", ``, membersCursor{}, false},
		{"null", `null`, membersCursor{}, false},
		{"empty team", `{"team":[]}`, membersCursor{Team: []string{}}, false},
		{"team and channels", `{"team":["a","b"],"channels":{"C1":[],"C2":["a"]}}`,
			membersCursor{Team: []string{"a", "b"}, Channels: map[string][]string{"C1": {}, "C2": {"a"}}}, false},
		{"the earlier shape", `["a","b"]`, membersCursor{Team: []string{"a", "b"}}, false},
		{"the earlier shape, empty", ` [] `, membersCursor{Team: []string{}}, false},
		{"damaged", `{"team":`, membersCursor{}, true},
	} {
		got, err := readMembersCursor(json.RawMessage(tc.raw))
		if (err != nil) != tc.bad {
			t.Errorf("%s: err = %v", tc.name, err)
			continue
		}
		if !tc.bad && !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
		// "Read, and nobody was there" must survive a round trip, or the
		// next attempt would take the people it added for the baseline.
		if !tc.bad && got.Team != nil {
			raw, _ := json.Marshal(got)
			again, err := readMembersCursor(raw)
			if err != nil || again.Team == nil || !reflect.DeepEqual(again, got) {
				t.Errorf("%s: round trip gave %+v, %v", tc.name, again, err)
			}
		}
	}
	if got := sortedIDs(map[string]bool{}); got == nil {
		t.Error("an empty membership is recorded as nil, which reads as not recorded")
	}
}

// A direct message's chat is finalized like a channel, by a lane of its
// own, but it belongs to no team: it neither waits for a team nor holds
// one back.
func TestPlanFinalizeWithChats(t *testing.T) {
	channels := []store.ChannelMapping{
		{SlackID: "C1", Kind: "public", TeamIndex: 0, State: store.ChannelMigrating},
		{SlackID: "C2", Kind: "private", TeamIndex: 0, State: store.ChannelMigrating},
		{SlackID: "C3", Kind: "public", TeamIndex: 1, State: store.ChannelCompleted},
		{SlackID: "D1", Kind: store.KindDM, TeamIndex: store.NoTeam, State: store.ChannelMigrating, Members: []string{"U1", "U2"}},
		{SlackID: "G1", Kind: store.KindMPIM, TeamIndex: store.NoTeam, State: store.ChannelMigrating, Members: []string{"U1", "U2", "U3"}},
	}
	teams := []store.TeamMapping{{Index: 0, Name: "A"}, {Index: 1, Name: "B"}}
	lanes := func(sel FinalizeSelection) []string {
		var ids []string
		for _, l := range sel.Plan.Lanes {
			ids = append(ids, l.ID)
		}
		return ids
	}
	deps := func(sel FinalizeSelection, lane string) []string {
		var ids []string
		for _, b := range sel.Plan.Batches {
			if b.LaneID == lane {
				for _, d := range b.DependsOn {
					ids = append(ids, d.LaneID)
				}
			}
		}
		return ids
	}

	// Everything: each conversation gets a lane; the team waits for its
	// channels and for nothing else.
	all, err := PlanFinalize(channels, teams, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if all.Channels != 3 || all.Chats != 2 || all.Already != 1 || all.Teams != 2 {
		t.Errorf("everything: %+v", all)
	}
	if got, want := lanes(all), []string{"finalize:C1", "finalize:C2", "finalize:D1", "finalize:G1", "members:0", "members:1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("lanes = %v, want %v", got, want)
	}
	if got, want := deps(all, "members:0"), []string{"finalize:C1", "finalize:C2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("members:0 waits for %v, want %v", got, want)
	}
	if got := deps(all, "members:1"); len(got) != 0 {
		t.Errorf("members:1 waits for %v", got)
	}

	// Only the chats: no team is held back by them, and the team whose
	// channels are still importing does not get its members.
	chats, err := PlanFinalize(channels, teams, []string{"D1", "G1"}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if chats.Channels != 0 || chats.Chats != 2 || chats.Teams != 1 {
		t.Errorf("only chats: %+v", chats)
	}
	if got, want := lanes(chats), []string{"finalize:D1", "finalize:G1", "members:1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("lanes = %v, want %v", got, want)
	}

	// Only the channels: the teams get their members although the chats
	// stay in migration mode.
	only, err := PlanFinalize(channels, teams, []string{"C1", "C2", "C3"}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if only.Channels != 3 || only.Chats != 0 || only.Teams != 2 {
		t.Errorf("only channels: %+v", only)
	}
	if got, want := lanes(only), []string{"finalize:C1", "finalize:C2", "members:0", "members:1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("lanes = %v, want %v", got, want)
	}

	// A chat an earlier request already queued counts as selected, and
	// still holds no team back.
	again, err := PlanFinalize(channels, teams, []string{"C1"}, map[string]bool{"D1": true}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := lanes(again), []string{"finalize:C1", "finalize:D1", "members:1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("lanes = %v, want %v", got, want)
	}
	if again.Channels != 1 || again.Chats != 0 {
		t.Errorf("selection = %+v", again)
	}
	if _, err := PlanFinalize(channels, teams, []string{"D9"}, nil, false); err == nil {
		t.Error("an unknown direct message was accepted")
	}
}

// Whose message holds a contested timestamp: object IDs are compared
// without regard to case, and in a chat a message with no author Graph
// names is not known to be somebody else's.
func TestWhoseMessage(t *testing.T) {
	at := time.UnixMilli(1700000000123).UTC()
	p := pipeline.Post{TargetMS: 1700000000123, FromID: "A0000000-0000-4000-8000-00000000000a"}
	for _, c := range []struct {
		name string
		m    graph.ChatMessage
		chat bool
		want occupant
	}{
		{"the same author", graph.ChatMessage{CreatedAt: at, FromUserID: p.FromID}, true, occupantOurs},
		{"the same author in another case", graph.ChatMessage{CreatedAt: at, FromUserID: "a0000000-0000-4000-8000-00000000000A"}, true, occupantOurs},
		{"the same author in a channel", graph.ChatMessage{CreatedAt: at, FromUserID: "a0000000-0000-4000-8000-00000000000A"}, false, occupantOurs},
		{"another author", graph.ChatMessage{CreatedAt: at, FromUserID: "b0000000-0000-4000-8000-00000000000b"}, true, occupantForeign},
		{"the same author at another time", graph.ChatMessage{CreatedAt: at.Add(time.Millisecond), FromUserID: p.FromID}, true, occupantForeign},
		{"no author, in a chat", graph.ChatMessage{CreatedAt: at}, true, occupantUnknown},
		{"a blank author, in a chat", graph.ChatMessage{CreatedAt: at, FromUserID: "  "}, true, occupantUnknown},
		{"no author, in a channel", graph.ChatMessage{CreatedAt: at}, false, occupantForeign},
	} {
		if got := whose(c.m, p, c.chat); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	// A post with no author of its own never matches a message without one.
	if got := whose(graph.ChatMessage{CreatedAt: at}, pipeline.Post{TargetMS: p.TargetMS}, false); got == occupantOurs {
		t.Error("two missing authors were taken for the same author")
	}
}

func TestPlainMentions(t *testing.T) {
	in := `<p>ask <at id="0">Ada Abbott</at> or <at id="12">Ben</at>, not &lt;at&gt;</p>`
	if got, want := plainMentions(in), `<p>ask Ada Abbott or Ben, not &lt;at&gt;</p>`; got != want {
		t.Errorf("plainMentions = %q, want %q", got, want)
	}
}
