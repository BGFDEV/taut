package worker_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/fakegraph"
	"github.com/bgfdev/taut/internal/slack"
	"github.com/bgfdev/taut/internal/store"
)

// startNew starts a run that replaces whatever run the config had, as
// `taut migrate --new` does.
func (r *harness) startNew(ctx context.Context) controller.RunView {
	r.t.Helper()
	run, err := r.client.CreateRun(ctx, r.yaml, true)
	if err != nil {
		r.t.Fatal(err)
	}
	f, err := os.Open(r.export)
	if err != nil {
		r.t.Fatal(err)
	}
	defer f.Close()
	if _, err := r.client.UploadExport(ctx, run.ID, f, ""); err != nil {
		r.t.Fatal(err)
	}
	return run
}

// nastyDMs is the nasty fixture with direct messages on and one small
// channel beside them.
func nastyDMs(m *config.Config) {
	withDMs(m)
	m.Target.Teams[0].Channels.Include = []string{"general"}
}

// find returns the conversation that has a message with the given text,
// and that message.
func find(t *testing.T, want []wantChat, text string) (wantChat, slack.Message) {
	t.Helper()
	for _, w := range want {
		for _, m := range w.msgs {
			if strings.HasPrefix(m.Text, text) {
				return w, m
			}
		}
	}
	t.Fatalf("the fixture has no direct message starting %q", text)
	return wantChat{}, slack.Message{}
}

// other returns the participant of a one-on-one conversation who did not
// write msg.
func other(fixture string, w wantChat, msg slack.Message) string {
	author := entraIDs(fixture)[msg.User]
	for _, id := range w.members {
		if id != author {
			return id
		}
	}
	return ""
}

// A run that is cancelled while its chats are in migration mode takes them
// out of it. Nobody finalizes a cancelled run, so otherwise they would stay
// that way, in front of the people in them. Only then is the run purged,
// and the purge removes who was in each conversation.
func TestCancelReleasesChats(t *testing.T) {
	r := newHarnessWith(t, "small", withDMs)
	ctx := t.Context()
	// Slow enough to cancel in the middle.
	r.fake.Faults().MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/chats/*/messages", Action: fakegraph.ActionLatency, LatencyMS: 150})
	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	run := r.start(ctx)

	deadline := time.Now().Add(time.Minute)
	for {
		migrating := 0
		for _, c := range r.fake.Chats() {
			if c.MigrationMode == "inProgress" && c.MessageCount > 0 {
				migrating++
			}
		}
		if migrating == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the chats never got into migration mode")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := r.client.Cancel(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	// Not purged while a chat is still in migration mode.
	r.api.Reconcile(ctx)
	if got, err := r.store.GetRun(ctx, run.ID); err != nil || got.Status != store.RunCancelled {
		t.Fatalf("run = %+v, %v", got, err)
	}

	r.awaitRelease(ctx, run.ID, time.Minute)
	for _, c := range r.fake.Chats() {
		if c.MigrationMode != "completed" {
			t.Errorf("chat %s of the cancelled run: migration mode %q, want completed", c.ID, c.MigrationMode)
		}
	}
	if n := r.logCount(ctx, run.ID, store.LogFilter{Op: "chat.release_migration"}); n != 2 {
		t.Errorf("audit log has %d chat.release_migration entries, want 2", n)
	}
	if err := r.store.VerifyLog(ctx, run.ID); err != nil {
		t.Errorf("audit log: %v", err)
	}
	// Nothing else of the cancelled run was worked after the cancel.
	imported := r.fake.Stats().ImportsAccepted
	time.Sleep(400 * time.Millisecond)
	if now := r.fake.Stats().ImportsAccepted; now > imported+1 {
		t.Errorf("%d messages were imported after the run was cancelled", now-imported)
	}

	// Now it is purged, and the ledger forgets who was in each chat.
	deadline = time.Now().Add(time.Minute)
	for {
		r.api.Reconcile(ctx)
		got, err := r.store.GetRun(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.PurgedAt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the cancelled run was not purged after its chats were released")
		}
		time.Sleep(50 * time.Millisecond)
	}
	channels, err := r.store.Channels(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	chats := 0
	for _, c := range channels {
		if !c.IsChat() {
			continue
		}
		chats++
		if c.TargetName != "" || len(c.Members) != 0 || c.SlackName != "" || c.Topic != "" {
			t.Errorf("after the purge the ledger still says who was in %s: %+v", c.SlackID, c)
		}
		if c.TeamsID == "" || c.State != store.ChannelCompleted {
			t.Errorf("after the purge the record of chat %s is gone: %+v", c.SlackID, c)
		}
	}
	if chats != 2 {
		t.Errorf("%d chats in the ledger", chats)
	}
}

// Even before it is purged, the ledger does not hold Slack's name for a
// group DM, which lists the handle of everyone in it.
func TestChatRowsCarryNoSlackName(t *testing.T) {
	r := newHarnessWith(t, "small", withDMs)
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	run := r.start(ctx)
	v := r.await(ctx, run.ID, time.Minute)
	for _, c := range v.Progress.Channels {
		if store.IsChatKind(c.Kind) && (c.SlackName != "" || strings.Contains(c.TargetName, "mpdm-")) {
			t.Errorf("conversation %s is reported with Slack's name: %+v", c.SlackID, c)
		}
	}
	// The run was created for the layout its config needs.
	if got, err := r.store.GetRun(ctx, run.ID); err != nil || got.Layout != 4 {
		t.Errorf("layout of a run with direct messages = %d, %v; want 4", got.Layout, err)
	}
	// No audit entry of a chat's messages carries a hash of its content.
	for _, c := range v.Progress.Channels {
		if !store.IsChatKind(c.Kind) {
			continue
		}
		entries, err := r.store.LogEntries(ctx, run.ID, "load:"+c.SlackID, 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, e := range entries {
			if strings.HasPrefix(e.Op, "message.") {
				n++
				if e.ContentHash != "" {
					t.Errorf("audit entry %s/%d of a direct message carries a content hash", e.Stream, e.Seq)
				}
			}
		}
		if n == 0 {
			t.Errorf("conversation %s has no message entries", c.SlackID)
		}
	}
	// A channel's entries still do.
	entries, err := r.store.LogEntries(ctx, run.ID, "load:"+v.Progress.Channels[0].SlackID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	hashed := 0
	for _, e := range entries {
		if e.Op == "message.import" && e.ContentHash != "" {
			hashed++
		}
	}
	if hashed == 0 {
		t.Error("a channel's message entries lost their content hash")
	}
}

// `taut migrate --new` after an import: the first run's chats are taken out
// of migration mode, and the second run imports into the same chats, the
// group chat included, finding every message already there. Nothing is
// created twice and nothing is imported twice.
func TestSecondRunUsesTheSameChats(t *testing.T) {
	r := newHarnessWith(t, "small", withDMs)
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	want, skipped := r.expectedChats("small")
	first := r.start(ctx)
	v := r.await(ctx, first.ID, time.Minute)
	r.verifyChats(v, "small", want, skipped, nil, "inProgress")
	accepted := r.fake.Stats().ImportsAccepted
	before := map[string]int{}
	for _, c := range r.fake.Chats() {
		before[c.ID] = c.MessageCount
	}

	second := r.startNew(ctx)
	if second.ID == first.ID {
		t.Fatal("--new did not start a new run")
	}
	v2 := r.await(ctx, second.ID, 2*time.Minute)
	if v2.Status != store.RunCompleted {
		t.Fatalf("second run: status = %s; problems: %+v", v2.Status, v2.Progress.Problems)
	}
	// The first run's chats were released, by its own lanes.
	r.awaitRelease(ctx, first.ID, time.Minute)
	if n := r.logCount(ctx, first.ID, store.LogFilter{Op: "chat.release_migration"}); n != 2 {
		t.Errorf("the first run has %d chat.release_migration entries, want 2", n)
	}

	chats := r.fake.Chats()
	if len(chats) != 2 {
		t.Fatalf("the tenant has %d chats after a second run, want the same 2", len(chats))
	}
	for _, c := range chats {
		if c.MessageCount != before[c.ID] {
			t.Errorf("chat %s (%s) holds %d messages after the second run, had %d: history was imported twice", c.ID, c.ChatType, c.MessageCount, before[c.ID])
		}
	}
	if now := r.fake.Stats().ImportsAccepted; now != accepted {
		t.Errorf("Graph accepted %d more imports in the second run; every message was already there", now-accepted)
	}
	if n := r.logCount(ctx, second.ID, store.LogFilter{Op: "chat.adopt"}); n != 2 {
		t.Errorf("the second run has %d chat.adopt entries, want 2", n)
	}
	if n := r.logCount(ctx, second.ID, store.LogFilter{Op: "chat.create"}); n != 0 {
		t.Errorf("the second run created %d chats", n)
	}
	if n := r.logCount(ctx, second.ID, store.LogFilter{Op: "chat.adopt", Contains: first.ID}); n != 1 {
		t.Errorf("%d chat.adopt entries name the first run, want 1 (the group chat)", n)
	}
	for _, c := range v2.Progress.Channels {
		if store.IsChatKind(c.Kind) && (c.Posted != 6 || c.Failed != 0) {
			t.Errorf("second run, conversation %s: %+v", c.SlackID, c)
		}
	}

	// The second run is finalized like any other, whatever the first run's
	// release did to the chats in between.
	if _, err := r.client.Finalize(ctx, second.ID, controller.FinalizeRequest{}); err != nil {
		t.Fatal(err)
	}
	v2 = r.awaitFinalize(ctx, second.ID, time.Minute)
	if v2.Status != store.RunFinalized {
		t.Fatalf("second run: status = %s, want finalized; problems %+v", v2.Status, v2.Progress.Problems)
	}
	for _, c := range r.fake.Chats() {
		if c.MigrationMode != "completed" {
			t.Errorf("chat %s: migration mode %q after finalize", c.ID, c.MigrationMode)
		}
	}
}

// A chat Microsoft refuses, at creation or when its migration mode is
// started, is one conversation that does not migrate. The run goes on, says
// which and why, and can be finalized.
func TestRefusedChatIsSkipped(t *testing.T) {
	for _, when := range []string{"create", "start"} {
		t.Run(when, func(t *testing.T) {
			r := newHarnessWith(t, "small", withDMs)
			ctx := t.Context()
			want, _ := r.expectedChats("small")
			var dm, group wantChat
			for _, w := range want {
				if w.group {
					group = w
				} else {
					dm = w
				}
			}
			// Someone who is only in the group DM has no Teams licence.
			var third string
			for _, id := range group.members {
				if id != dm.members[0] && id != dm.members[1] {
					third = id
				}
			}
			if when == "create" {
				r.fake.RefuseChatsWith(third, "The user does not have a Teams license.")
			} else {
				r.fake.RefuseChatMigrationWith(third, "Chat migration is blocked by policy for a member of this chat.")
			}
			kill := r.worker(ctx, 5*time.Second)
			defer kill()
			run := r.start(ctx)
			v := r.await(ctx, run.ID, time.Minute)
			if v.Status != store.RunCompleted {
				t.Fatalf("status = %s, want completed: one refused chat is not a failed run; problems %+v", v.Status, v.Progress.Problems)
			}
			for _, c := range v.Progress.Channels {
				switch c.SlackID {
				case group.slackID:
					if c.State != store.ChannelSkipped || c.SkipCode != "graph.chat_refused" || c.Posted != 0 || c.Failed != len(group.msgs) {
						t.Errorf("the refused conversation: %+v", c)
					}
				case dm.slackID:
					if c.State != store.ChannelMigrating || c.Posted != len(dm.msgs) || c.Failed != 0 {
						t.Errorf("the other conversation: %+v", c)
					}
				}
			}
			if v.Progress.Failed != len(group.msgs) {
				t.Errorf("%d posts not imported, want the %d of the refused conversation", v.Progress.Failed, len(group.msgs))
			}
			for _, c := range r.fake.Chats() {
				if c.ChatType == "group" && (c.MessageCount != 0 || c.MigrationMode != "") {
					t.Errorf("the refused group chat: %+v", c)
				}
				if c.ChatType == "oneOnOne" && c.MessageCount != len(dm.msgs) {
					t.Errorf("the one-on-one chat holds %d messages, want %d", c.MessageCount, len(dm.msgs))
				}
			}
			page, err := r.client.AuditLog(ctx, run.ID, store.LogFilter{Op: "chat.skip"})
			if err != nil || len(page.Entries) != 1 || page.Entries[0].Outcome != "error" || !strings.Contains(page.Entries[0].Detail, "graph.chat_refused") {
				t.Errorf("chat.skip entries = %+v, %v", page.Entries, err)
			}
			// The reason is Microsoft's, and names nobody.
			cm, err := r.store.Channel(ctx, run.ID, group.slackID)
			if err != nil || cm.SkipReason == "" || strings.Contains(cm.SkipReason, "Chloe") {
				t.Errorf("skip reason = %q, %v", cm.SkipReason, err)
			}

			fv, err := r.client.Finalize(ctx, run.ID, controller.FinalizeRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if fv.Chats != 2 || fv.Already != 1 {
				t.Errorf("finalize selection = %+v; the skipped conversation has nothing to finalize", fv)
			}
			v = r.awaitFinalize(ctx, run.ID, time.Minute)
			if v.Status != store.RunFinalized {
				t.Fatalf("status = %s, want finalized; problems %+v", v.Status, v.Progress.Problems)
			}
			if err := r.store.VerifyLog(ctx, run.ID); err != nil {
				t.Errorf("audit log: %v", err)
			}
		})
	}
}

// Having the right members is not enough to take a group chat for one an
// interrupted attempt created. A chat the same people made for themselves
// in those minutes, with a message in it, is left alone.
func TestGroupChatAdoptionNeedsMoreThanMembers(t *testing.T) {
	r := newHarnessWith(t, "small", withDMs, fakegraph.WithChatMemberExpandLimit(1))
	ctx := t.Context()
	want, _ := r.expectedChats("small")
	var group wantChat
	for _, w := range want {
		if w.group {
			group = w
		}
	}
	created := time.Now().UTC().Add(-2 * time.Second)
	theirs := r.fake.AddChat(fakegraph.Chat{
		ChatType: "group", Members: group.members, CreatedDateTime: &created,
		Messages: []fakegraph.ChatMessage{{CreatedDateTime: created.Add(time.Second), From: group.members[0], Content: "<p>our own group</p>"}},
	})
	// And one with one person more, which a truncated member list would
	// not tell apart.
	extra := ""
	for _, u := range r.fake.Snapshot().Users {
		if u.AccountEnabled && !strings.Contains(strings.Join(group.members, ","), u.ID) {
			extra = u.ID
		}
	}
	bigger := r.fake.AddChat(fakegraph.Chat{ChatType: "group", Members: append([]string{extra}, group.members...), CreatedDateTime: &created})
	r.fake.LoseGroupChatResponses(1)

	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	run := r.start(ctx)
	v := r.await(ctx, run.ID, 2*time.Minute)
	if v.Status != store.RunCompleted || v.Progress.Failed != 0 {
		t.Fatalf("status = %s, failed %d; problems %+v", v.Status, v.Progress.Failed, v.Progress.Problems)
	}
	groups := 0
	for _, c := range r.fake.Chats() {
		if c.ChatType != "group" {
			continue
		}
		groups++
		switch c.ID {
		case theirs:
			if c.MessageCount != 1 || c.MigrationMode != "" {
				t.Errorf("the group chat people made for themselves was touched: %+v", c)
			}
		case bigger:
			if c.MessageCount != 0 || c.MigrationMode != "" {
				t.Errorf("a chat with one person more was touched: %+v", c)
			}
		default:
			if c.MessageCount != len(group.msgs) || c.MigrationMode != "inProgress" {
				t.Errorf("the migration's group chat: %+v", c)
			}
		}
	}
	if groups != 3 {
		t.Errorf("%d group chats exist; want theirs, the bigger one, and the one the lost request created", groups)
	}
	if n := r.logCount(ctx, run.ID, store.LogFilter{Op: "chat.adopt", Contains: "interrupted"}); n != 1 {
		t.Errorf("%d chat.adopt entries for the interrupted creation, want 1", n)
	}
	if n := r.fake.CountRequests("GET", "/v1.0/chats/*/members"); n == 0 {
		t.Error("no candidate's member list was read from the chat itself")
	}
}

// When Graph definitely refuses to create a chat, no chat came of it, and a
// later attempt does not go looking for one: the window in which a chat
// can be adopted is only as long as an answer is in doubt.
func TestRefusedCreationIsNotLookedFor(t *testing.T) {
	r := newHarnessWith(t, "small", withDMs)
	ctx := t.Context()
	// A refusal that is not about the chat: the lane retries.
	r.fake.Faults().MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/chats", Action: fakegraph.ActionStatus, Status: 403, Count: 2})
	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	want, skipped := r.expectedChats("small")
	run := r.start(ctx)
	v := r.await(ctx, run.ID, 2*time.Minute)
	r.verifyChats(v, "small", want, skipped, nil, "inProgress")
	if n := r.fake.CountRequests("GET", "/v1.0/users/*/chats"); n != 0 {
		t.Errorf("after a definite refusal the retry listed chats %d times, looking for one that cannot exist", n)
	}
	if n := r.logCount(ctx, run.ID, store.LogFilter{Op: "chat.adopt"}); n != 0 {
		t.Errorf("%d chats were adopted", n)
	}
}

// Posts a millisecond apart move together: two messages allocated
// neighbouring milliseconds, and the parts of a split message. Moving only
// the first would put it on top of the second.
func TestShiftMovesNeighbouringPostsTogether(t *testing.T) {
	r := newHarnessWith(t, "nasty", nastyDMs)
	ctx := t.Context()
	want, _ := r.expectedChats("nasty")
	conv, first := find(t, want, "First of two messages in one millisecond")
	_, split := find(t, want, longText(t, want))
	at := func(m slack.Message) time.Time { return m.Time().Truncate(time.Millisecond) }
	chatID := r.fake.AddChat(fakegraph.Chat{
		ChatType: "oneOnOne", Members: conv.members,
		Messages: []fakegraph.ChatMessage{
			{CreatedDateTime: at(first), From: other("nasty", conv, first), Content: "<p>in the way of two neighbours</p>"},
			{CreatedDateTime: at(split), From: other("nasty", conv, split), Content: "<p>in the way of a split message</p>"},
		},
	})
	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	run := r.start(ctx)
	v := r.await(ctx, run.ID, 2*time.Minute)
	if v.Status != store.RunCompleted || v.Progress.Failed != 0 {
		t.Fatalf("status = %s, failed = %d; problems %+v", v.Status, v.Progress.Failed, v.Progress.Problems)
	}
	posts := r.posts(ctx, run.ID, conv)
	imported := 0
	for _, m := range r.fake.ChatMessages(chatID) {
		if m.ImportedBy != "" {
			imported++
		}
	}
	if imported != len(posts) {
		t.Fatalf("%d posts imported, want all %d", imported, len(posts))
	}
	// Each of the two neighbours, and each part of the split message, went
	// in one millisecond later, in order.
	for _, m := range conv.msgs {
		moved := m.TS == split.TS || strings.Contains(m.Text, "messages in one millisecond")
		for part := 0; ; part++ {
			row, err := r.store.GetMessage(ctx, run.ID, store.MessageKey{ChannelID: conv.slackID, SlackTS: m.TS, Part: part})
			if err != nil {
				if part == 0 {
					t.Fatal(err)
				}
				break
			}
			if (row.ShiftMS == 1) != moved || row.State != store.MessagePosted {
				t.Errorf("message %s part %d: shift %d, state %s; moved should be %v", m.TS, part, row.ShiftMS, row.State, moved)
			}
		}
	}
	msgs := r.fake.ChatMessages(chatID)
	for i := 1; i < len(msgs); i++ {
		if !msgs[i].CreatedAt.After(msgs[i-1].CreatedAt) {
			t.Fatalf("messages %d and %d share a millisecond or are out of order", i-1, i)
		}
	}
}

// longText returns the start of the fixture's long direct message.
func longText(t *testing.T, want []wantChat) string {
	t.Helper()
	for _, w := range want {
		for _, m := range w.msgs {
			if len(m.Text) > 90_000 {
				return m.Text[:40]
			}
		}
	}
	t.Fatal("the fixture has no long direct message")
	return ""
}

// A post that cannot be moved past the messages in its way, because it has
// been moved as far as it may be, is not imported; and then none of its
// message is. The tail of a split message never goes into a chat without
// its head.
func TestShiftThatCannotBeDoneFailsTheWholeMessage(t *testing.T) {
	r := newHarnessWith(t, "nasty", nastyDMs)
	ctx := t.Context()
	want, _ := r.expectedChats("nasty")
	conv, split := find(t, want, longText(t, want))
	at := split.Time().Truncate(time.Millisecond)
	blocker := other("nasty", conv, split)
	var theirs []fakegraph.ChatMessage
	// 101 milliseconds in a row, each taken: one more than a post is moved.
	for i := range 101 {
		theirs = append(theirs, fakegraph.ChatMessage{CreatedDateTime: at.Add(time.Duration(i) * time.Millisecond), From: blocker, Content: "<p>busy</p>"})
	}
	chatID := r.fake.AddChat(fakegraph.Chat{ChatType: "oneOnOne", Members: conv.members, Messages: theirs})
	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	run := r.start(ctx)
	v := r.await(ctx, run.ID, 3*time.Minute)
	if v.Status != store.RunCompleted {
		t.Fatalf("status = %s; problems %+v", v.Status, v.Progress.Problems)
	}
	parts := 0
	for part := 0; ; part++ {
		row, err := r.store.GetMessage(ctx, run.ID, store.MessageKey{ChannelID: conv.slackID, SlackTS: split.TS, Part: part})
		if err != nil {
			break
		}
		parts++
		if row.State != store.MessageFailed || row.ErrorCode != "timestamp_conflict" {
			t.Errorf("part %d of the blocked message: state %s, code %q; want every part failed as timestamp_conflict", part, row.State, row.ErrorCode)
		}
		if row.ShiftMS != 100 {
			t.Errorf("part %d was moved %d ms, want the limit of 100", part, row.ShiftMS)
		}
	}
	if parts < 2 {
		t.Fatalf("the long message has %d parts; the test needs a split message", parts)
	}
	if v.Progress.Failed != parts {
		t.Errorf("%d posts failed, want the %d parts of the one message", v.Progress.Failed, parts)
	}
	imported := 0
	for _, m := range r.fake.ChatMessages(chatID) {
		if m.ImportedBy == "" {
			continue
		}
		imported++
		if len(m.BodyContent) > 20_000 {
			t.Errorf("a part of the blocked message is in the chat (%d bytes at %s)", len(m.BodyContent), m.CreatedAt)
		}
	}
	if total := len(r.posts(ctx, run.ID, conv)); imported != total-parts {
		t.Errorf("%d posts imported, want every post but the blocked message's (%d)", imported, total-parts)
	}
	if n := r.logCount(ctx, run.ID, store.LogFilter{Stream: "load:" + conv.slackID, Op: "message.fail", Contains: "timestamp_conflict"}); n != parts {
		t.Errorf("audit log has %d message.fail entries for the blocked message, want one per part (%d)", n, parts)
	}
}

// shiftSpy is a view of the store that reports when a shift has been
// committed.
type shiftSpy struct {
	store.Store
	committed func()
}

type shiftSpyTx struct {
	store.Tx
	shifted *bool
}

func (t shiftSpyTx) ShiftMessages(ctx context.Context, channelID string, fromMS, maxShift int64) (int, error) {
	n, err := t.Tx.ShiftMessages(ctx, channelID, fromMS, maxShift)
	if err == nil {
		*t.shifted = true
	}
	return n, err
}

func (s shiftSpy) Fenced(ctx context.Context, l *store.Lease, fn func(tx store.Tx) error) error {
	shifted := false
	err := s.Store.Fenced(ctx, l, func(tx store.Tx) error { return fn(shiftSpyTx{Tx: tx, shifted: &shifted}) })
	if err == nil && shifted {
		s.committed()
	}
	return err
}

// The worker dies after the shift is committed and before the post is sent
// again. Whoever takes the lane next finds the new time in the ledger and
// sends the post there: once, and at the same time any other attempt would.
func TestKillBetweenShiftAndSend(t *testing.T) {
	r := newHarnessWith(t, "nasty", nastyDMs)
	ctx := t.Context()
	want, _ := r.expectedChats("nasty")
	conv, first := find(t, want, "First of two messages in one millisecond")
	at := first.Time().Truncate(time.Millisecond)
	chatID := r.fake.AddChat(fakegraph.Chat{
		ChatType: "oneOnOne", Members: conv.members,
		Messages: []fakegraph.ChatMessage{{CreatedDateTime: at, From: other("nasty", conv, first), Content: "<p>in the way</p>"}},
	})

	var (
		mu     sync.Mutex
		kill   func()
		killed atomic.Int32
	)
	spy := shiftSpy{Store: r.store, committed: func() {
		// Kill the worker that just committed the shift, before it can
		// send the post again. Killing waits for the worker to stop, which
		// it cannot do from inside its own transaction's caller: do it
		// from another goroutine and hold the worker here until it is
		// cancelled.
		if killed.Add(1) != 1 {
			return
		}
		mu.Lock()
		k := kill
		mu.Unlock()
		go k()
		time.Sleep(300 * time.Millisecond)
	}}
	mu.Lock()
	kill = r.workerWith(ctx, 600*time.Millisecond, spy)
	mu.Unlock()
	run := r.start(ctx)

	deadline := time.Now().Add(time.Minute)
	for killed.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no shift was ever committed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The shift is in the ledger; the post is not in the chat.
	row, err := r.store.GetMessage(ctx, run.ID, store.MessageKey{ChannelID: conv.slackID, SlackTS: first.TS})
	if err != nil || row.ShiftMS != 1 || row.State == store.MessagePosted {
		t.Fatalf("after the kill: row = %+v, %v; want it moved and not posted", row, err)
	}
	for _, m := range r.fake.ChatMessages(chatID) {
		if m.ImportedBy != "" && m.CreatedAt.Equal(at.Add(time.Millisecond)) {
			t.Fatal("the post was sent before the worker died; the test did not hit the window")
		}
	}

	// Another worker takes over.
	next := r.worker(ctx, 600*time.Millisecond)
	defer next()
	v := r.await(ctx, run.ID, 2*time.Minute)
	if v.Status != store.RunCompleted || v.Progress.Failed != 0 {
		t.Fatalf("status = %s, failed %d; problems %+v", v.Status, v.Progress.Failed, v.Progress.Problems)
	}
	row, err = r.store.GetMessage(ctx, run.ID, store.MessageKey{ChannelID: conv.slackID, SlackTS: first.TS})
	if err != nil || row.ShiftMS != 1 || row.State != store.MessagePosted || row.TargetMS != at.UnixMilli()+1 {
		t.Fatalf("after the resume: row = %+v, %v; want it posted one millisecond later, moved once", row, err)
	}
	seen := map[int64]int{}
	imported := 0
	for _, m := range r.fake.ChatMessages(chatID) {
		seen[m.CreatedAt.UnixMilli()]++
		if m.ImportedBy != "" {
			imported++
		}
	}
	if seen[at.UnixMilli()] != 1 || seen[at.UnixMilli()+1] != 1 || seen[at.UnixMilli()+2] != 1 {
		t.Errorf("around the contested millisecond: %d, %d and %d messages; want one each (theirs, then the two neighbours)",
			seen[at.UnixMilli()], seen[at.UnixMilli()+1], seen[at.UnixMilli()+2])
	}
	if total := len(r.posts(ctx, run.ID, conv)); imported != total {
		t.Errorf("%d posts imported, want %d", imported, total)
	}
	if r.fake.Stats().ImportsAccepted != v.Progress.Posted {
		t.Errorf("Graph accepted %d imports for %d posts", r.fake.Stats().ImportsAccepted, v.Progress.Posted)
	}
}

// layoutStore is a view of the store in which a run needs a ledger layout
// this version does not know.
type layoutStore struct{ store.Store }

func (s layoutStore) GetRun(ctx context.Context, id string) (store.Run, error) {
	run, err := s.Store.GetRun(ctx, id)
	run.Layout = 99
	return run, err
}

// A worker does not plan a run that needs a layout it does not know: it
// would plan it without whatever the newer layout is for.
func TestWorkerDoesNotPlanARunItDoesNotUnderstand(t *testing.T) {
	r := newHarnessWith(t, "small", withDMs)
	ctx := t.Context()
	kill := r.workerWith(ctx, 5*time.Second, layoutStore{r.store})
	run := r.start(ctx)
	time.Sleep(1500 * time.Millisecond)
	kill()
	channels, err := r.store.Channels(ctx, run.ID)
	if err != nil || len(channels) != 0 {
		t.Fatalf("the run was planned by a worker that does not know its layout: %d conversations, %v", len(channels), err)
	}
	if got, err := r.store.GetRun(ctx, run.ID); err != nil || got.Status == store.RunFailed {
		t.Fatalf("the run was failed by a worker that should have left it alone: %+v, %v", got, err)
	}
	if n := len(r.fake.Chats()); n != 0 {
		t.Errorf("%d chats were created", n)
	}
	// A worker that does know it carries on from there.
	kill = r.worker(ctx, 5*time.Second)
	defer kill()
	if v := r.await(ctx, run.ID, 2*time.Minute); v.Status != store.RunCompleted {
		t.Fatalf("status = %s; problems %+v", v.Status, v.Progress.Problems)
	}
	if got, _ := r.store.GetRun(ctx, run.ID); got.Layout != 4 {
		t.Errorf("layout = %d", got.Layout)
	}
}

// If a chat will not take a mention of someone who is not in it, the
// message is sent again with the name as plain text, rather than fail the
// lane.
func TestMentionOfSomeoneOutsideTheChat(t *testing.T) {
	r := newHarnessWith(t, "nasty", nastyDMs, fakegraph.WithChatMentionsMustBeMembers())
	ctx := t.Context()
	want, skipped := r.expectedChats("nasty")
	conv, msg := find(t, want, "Ask <@")
	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	run := r.start(ctx)
	v := r.await(ctx, run.ID, 2*time.Minute)
	if v.Status != store.RunCompleted || v.Progress.Failed != 0 {
		t.Fatalf("status = %s, failed %d; problems %+v", v.Status, v.Progress.Failed, v.Progress.Problems)
	}
	r.verifyChats(v, "nasty", want, skipped, nil, "inProgress")
	refused := 0
	for _, rec := range r.fake.Requests() {
		if rec.Method == "POST" && strings.Contains(rec.Path, "/chats/") && strings.HasSuffix(rec.Path, "/messages") && rec.Status == 400 {
			refused++
		}
	}
	if refused != 1 {
		t.Errorf("%d messages were refused, want the 1 with the mention", refused)
	}
	found := false
	for _, c := range r.fake.Chats() {
		for _, m := range r.fake.ChatMessages(c.ID) {
			if len(m.Mentions) != 0 {
				in := false
				for _, cm := range c.Members {
					in = in || cm.UserID == m.Mentions[0].UserID
				}
				if !in {
					t.Errorf("chat %s has a mention of someone who is not in it", c.ID)
				}
			}
			if m.CreatedAt.Equal(msg.Time().Truncate(time.Millisecond)) && strings.Contains(m.BodyContent, "about the report") {
				found = true
				if strings.Contains(m.BodyContent, "<at ") || !strings.Contains(m.BodyContent, "Ada") {
					t.Errorf("the message with the refused mention: %q; want the name as plain text", m.BodyContent)
				}
			}
		}
	}
	if !found {
		t.Errorf("the message with the mention is not in the chat of %s", conv.slackID)
	}
}
