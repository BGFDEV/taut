package worker_test

import (
	"math/rand/v2"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/fakegraph"
	"github.com/bgfdev/taut/internal/slack"
	"github.com/bgfdev/taut/internal/store"
)

func withNativeReactions(m *config.Config) { m.Content.Reactions = config.ReactionsNative }

// customEmoji are the reaction names of the fixtures that are no Unicode
// emoji, so they can only be summarized.
var customEmoji = map[string]bool{"partyparrot": true}

// summaryRE matches the line that summarizes every reaction of a message,
// which ends a message whose reactions are not carried as reactions.
var summaryRE = regexp.MustCompile(`<p><em>[^<]+ \d+</em></p>$`)

// wantReactions is what one message must carry: how many reactions each
// person gave that can be a Teams reaction, and whether anything is left
// for the summary line.
type wantReactions struct {
	byUser map[string]int // Entra ID → reactions
	total  int
	rest   bool
}

// expectedReactions works out a message's reactions from the export and
// the fixture's record of who has an account. It shares nothing with the
// transform.
func expectedReactions(entra map[string]string, msg slack.Message) wantReactions {
	w := wantReactions{byUser: map[string]int{}}
	for _, r := range msg.Reactions {
		given := 0
		seen := map[string]bool{}
		for _, u := range r.Users {
			id := entra[u]
			if id == "" || seen[id] || customEmoji[r.Name] {
				continue
			}
			seen[id] = true
			given++
			w.byUser[id]++
		}
		w.total += given
		if max(r.Count, len(r.Users)) > given {
			w.rest = true
		}
	}
	return w
}

// verifyReactions checks every channel message of the fake tenant against
// its Slack source. With native set, a message carries exactly the
// reactions of the people who have an account, each once, and a summary of
// the rest; otherwise it carries none and summarizes them all.
func (r *harness) verifyReactions(fixture string, w want, native bool) (carried, summarized int) {
	t := r.t
	t.Helper()
	entra := entraIDs(fixture)
	team := r.fake.Teams()[0]
	byName := map[string]fakegraph.ChannelView{}
	for _, c := range r.fake.Channels(team.ID) {
		byName[c.DisplayName] = c
	}
	for name, msgs := range w.byChannel {
		target := name
		if name == "general" {
			target = "General"
		}
		stored := r.fake.Messages(team.ID, byName[target].ID)
		if len(stored) != len(msgs) {
			t.Errorf("channel %q holds %d messages, want %d", target, len(stored), len(msgs))
			continue
		}
		for i, m := range stored {
			src := msgs[i]
			want := expectedReactions(entra, src)
			body := m.BodyContent
			if !native {
				if len(m.Reactions) != 0 {
					t.Errorf("channel %q message %s carries %d reactions, want none", target, src.TS, len(m.Reactions))
				}
				if len(src.Reactions) > 0 {
					summarized++
					if !summaryRE.MatchString(body) || strings.Contains(body, "Other reactions:") {
						t.Errorf("channel %q message %s does not end in the summary of its reactions: %q", target, src.TS, tail(body))
					}
				}
				continue
			}
			if len(m.Reactions) != want.total {
				t.Errorf("channel %q message %s carries %d reactions, want %d", target, src.TS, len(m.Reactions), want.total)
				continue
			}
			carried += len(m.Reactions)
			type one struct{ kind, user string }
			seen := map[one]bool{}
			byUser := map[string]int{}
			for _, re := range m.Reactions {
				if seen[one{re.Type, re.UserID}] {
					t.Errorf("channel %q message %s: %s by %s twice", target, src.TS, re.Type, re.UserID)
				}
				seen[one{re.Type, re.UserID}] = true
				byUser[re.UserID]++
				if !re.CreatedAt.Equal(m.CreatedAt) {
					t.Errorf("channel %q message %s: a reaction is dated %s, the message %s", target, src.TS, re.CreatedAt, m.CreatedAt)
				}
			}
			for id, n := range want.byUser {
				if byUser[id] != n {
					t.Errorf("channel %q message %s: %d reactions by %s, want %d", target, src.TS, byUser[id], id, n)
				}
			}
			switch {
			case want.total == 0 && len(src.Reactions) > 0:
				// Nothing could be a reaction: the plain summary.
				if !summaryRE.MatchString(body) || strings.Contains(body, "Other reactions:") {
					t.Errorf("channel %q message %s: %q", target, src.TS, tail(body))
				}
			case want.rest:
				summarized++
				if !summaryRE.MatchString(body) || !strings.Contains(body, "<p><em>Other reactions: ") {
					t.Errorf("channel %q message %s does not summarize the reactions it could not carry: %q", target, src.TS, tail(body))
				}
			case len(src.Reactions) > 0 && summaryRE.MatchString(body):
				t.Errorf("channel %q message %s summarizes reactions it carries: %q", target, src.TS, tail(body))
			}
		}
	}
	return carried, summarized
}

func tail(s string) string {
	if len(s) > 120 {
		return "…" + s[len(s)-120:]
	}
	return s
}

// TestMigrateNativeReactions runs the medium fixture with
// content.reactions: native against a tenant that takes reactions on
// imports.
func TestMigrateNativeReactions(t *testing.T) {
	r := newHarnessWith(t, "medium", withNativeReactions)
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	defer kill()

	run := r.start(ctx)
	v := r.await(ctx, run.ID, 3*time.Minute)
	w := r.expected()
	r.verify(v, w)
	carried, summarized := r.verifyReactions("medium", w, true)
	if carried == 0 || summarized == 0 {
		t.Errorf("%d reactions carried, %d messages with a summary of the rest: the fixture exercises neither", carried, summarized)
	}
	if n := r.logCount(ctx, v.ID, store.LogFilter{Contains: "reactions summarized"}); n != 0 {
		t.Errorf("%d audit entries say reactions were refused", n)
	}
	t.Logf("%d reactions carried as reactions; %d messages summarize some", carried, summarized)
}

// TestNativeReactionsSurviveKillsAndFaults is the kill-and-resume run with
// reactions: a message whose response was lost is adopted, and must hold
// its reactions exactly once like its text.
func TestNativeReactionsSurviveKillsAndFaults(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos run skipped in -short mode")
	}
	r := newHarnessWith(t, "medium", withNativeReactions, fakegraph.WithRandSeed(17))
	ctx := t.Context()
	faults := r.fake.Faults()
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/channels/*/messages", Action: fakegraph.ActionFailAfterCommit, Probability: 0.04})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/channels/*/messages/*/replies", Action: fakegraph.ActionFailAfterCommit, Probability: 0.04})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/channels/*/messages", Action: fakegraph.ActionThrottle, RetryAfterSeconds: 1, Probability: 0.03})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/channels/*/messages/**", Action: fakegraph.ActionStatus, Status: 503, Probability: 0.02})

	run := r.start(ctx)
	var mu sync.Mutex
	kills := []func(){r.worker(ctx, 600*time.Millisecond), r.worker(ctx, 600*time.Millisecond), r.worker(ctx, 600*time.Millisecond)}
	stop := make(chan struct{})
	chaosDone := make(chan struct{})
	killed := 0
	go func() {
		defer close(chaosDone)
		rng := rand.New(rand.NewPCG(5, 23))
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Duration(60+rng.IntN(140)) * pace * time.Millisecond):
			}
			mu.Lock()
			i := rng.IntN(len(kills))
			kills[i]()
			kills[i] = r.worker(ctx, 600*time.Millisecond)
			killed++
			mu.Unlock()
		}
	}()
	v := r.await(ctx, run.ID, 4*time.Minute)
	close(stop)
	<-chaosDone
	mu.Lock()
	for _, k := range kills {
		k()
	}
	mu.Unlock()

	w := r.expected()
	r.verify(v, w)
	carried, _ := r.verifyReactions("medium", w, true)
	stats := r.fake.Stats()
	t.Logf("workers killed: %d; conflicts (409): %d; adopted: %d; reactions carried: %d", killed, stats.Conflicts, v.Progress.Adopted, carried)
	if v.Progress.Adopted == 0 || stats.Conflicts == 0 {
		t.Errorf("the recovery path was never exercised (adopted=%d, conflicts=%d)", v.Progress.Adopted, stats.Conflicts)
	}
	if stats.ImportsAccepted != v.Progress.Posted {
		t.Errorf("Graph accepted %d imports but %d posts exist: a message was written twice", stats.ImportsAccepted, v.Progress.Posted)
	}
	// An adopted message was found with the reactions it was sent with.
	if n := r.logCount(ctx, v.ID, store.LogFilter{Op: "message.adopt", Contains: "reactions"}); n != 0 {
		t.Errorf("%d adopted messages did not hold their reactions", n)
	}
}

// TestNativeReactionsRefused runs against a tenant that refuses reactions
// on imports. Every message still arrives, with its reactions summarized as
// the footer policy would, and the audit log says so.
func TestNativeReactionsRefused(t *testing.T) {
	r := newHarnessWith(t, "medium", withNativeReactions, fakegraph.WithReactionsRefused())
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	defer kill()

	run := r.start(ctx)
	v := r.await(ctx, run.ID, 3*time.Minute)
	w := r.expected()
	r.verify(v, w)
	_, summarized := r.verifyReactions("medium", w, false)
	if summarized == 0 {
		t.Fatal("the fixture has no message with reactions")
	}

	// Each message that would have carried reactions is noted in the audit
	// log, once.
	entra := entraIDs("medium")
	wantNoted := 0
	for _, msgs := range w.byChannel {
		for _, m := range msgs {
			if expectedReactions(entra, m).total > 0 {
				wantNoted++
			}
		}
	}
	if n := r.logCount(ctx, v.ID, store.LogFilter{Op: "message.import", Contains: "reactions summarized"}); n != wantNoted || n == 0 {
		t.Errorf("%d audit entries note summarized reactions, want %d", n, wantNoted)
	}
	// Refused reactions are asked for a few times per channel, not once
	// per message.
	refused := r.fake.CountRequests("POST", "/v1.0/teams/*/channels/*/messages") + r.fake.CountRequests("POST", "/v1.0/teams/*/channels/*/messages/*/replies") - v.Progress.Posted
	if most := 3 * len(w.byChannel); refused <= 0 || refused > most {
		t.Errorf("%d refused requests for %d messages with reactions in %d channels, want at most %d", refused, wantNoted, len(w.byChannel), most)
	}
	t.Logf("%d messages summarize their reactions; %d requests were refused for them", summarized, refused)
}

// TestMigrateNastyNativeReactions checks the edge cases of the nasty
// fixture: one emoji in two skin tones, people who are gone, a custom
// emoji, and a count that disagrees with the users listed.
func TestMigrateNastyNativeReactions(t *testing.T) {
	if testing.Short() {
		t.Skip("large fixture skipped in -short mode")
	}
	r := newHarnessWith(t, "nasty", withNativeReactions)
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	defer kill()

	run := r.start(ctx)
	v := r.await(ctx, run.ID, 3*time.Minute)
	if v.Status != store.RunCompleted || v.Progress.Failed != 0 || v.Progress.Held != 0 {
		t.Fatalf("status %s, failed %d, held %d; problems: %+v", v.Status, v.Progress.Failed, v.Progress.Held, v.Progress.Problems)
	}
	entra := entraIDs("nasty")
	var found *fakegraph.MessageView
	team := r.fake.Teams()[0]
	for _, c := range r.fake.Channels(team.ID) {
		for _, m := range r.fake.Messages(team.ID, c.ID) {
			if len(m.Reactions) > fakegraph.MaxImportReactions {
				t.Errorf("a message carries %d reactions", len(m.Reactions))
			}
			if strings.Contains(m.BodyContent, "Lots of reactions, some from people who are gone.") {
				found = &m
			}
		}
	}
	if found == nil {
		t.Fatal("the message with many reactions is not in the tenant")
	}
	mapped := map[string]bool{}
	for _, id := range entra {
		mapped[id] = true
	}
	kinds := map[string]int{}
	for _, re := range found.Reactions {
		kinds[re.Type]++
		if !mapped[re.UserID] {
			t.Errorf("a reaction is attributed to %s, who is not a mapped person", re.UserID)
		}
	}
	// +1 by the people who have an account; the skin-toned +1 only by
	// people who are gone; eyes by one person of the three counted.
	if kinds["👍"] == 0 || kinds["👍🏽"] != 0 || kinds["👀"] != 1 || len(kinds) != 2 {
		t.Errorf("reactions by emoji: %v", kinds)
	}
	for _, want := range []string{"Other reactions: ", "👍🏽 2", ":partyparrot: 1", "👀 2"} {
		if !strings.Contains(found.BodyContent, want) {
			t.Errorf("the summary lacks %q: %q", want, tail(found.BodyContent))
		}
	}
	if err := r.store.VerifyLog(ctx, v.ID); err != nil {
		t.Errorf("audit log does not verify: %v", err)
	}
}
