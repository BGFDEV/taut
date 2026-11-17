package worker_test

import (
	"context"
	"html"
	"math/rand/v2"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/devstack"
	"github.com/bgfdev/taut/internal/fakegraph"
	"github.com/bgfdev/taut/internal/fixtures"
	"github.com/bgfdev/taut/internal/sidecar/budget"
	"github.com/bgfdev/taut/internal/slack"
	"github.com/bgfdev/taut/internal/slack/export"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/transform"
)

// plainText matches message text that converts to HTML unchanged: no
// markup, no mentions, no emoji codes.
var plainText = regexp.MustCompile(`^[A-Za-z0-9 .,()'-]+$`)

// withDMs turns direct-message migration on.
func withDMs(m *config.Config) { m.Target.DirectMessages.Enabled = true }

// wantChat is the ground truth for one direct-message conversation that
// must end up in a Teams chat.
type wantChat struct {
	slackID string
	group   bool
	members []string        // Entra object IDs, sorted
	msgs    []slack.Message // what must be imported, in order
}

func memberKey(group bool, ids []string) string {
	s := append([]string(nil), ids...)
	for i := range s {
		s[i] = strings.ToLower(s[i])
	}
	sort.Strings(s)
	kind := "oneOnOne"
	if group {
		kind = "group"
	}
	return kind + ":" + strings.Join(s, ",")
}

// entraIDs maps the Slack users of a fixture to their Microsoft 365
// accounts in the fake tenant, straight from the fixture's own record of
// who has one.
func entraIDs(fixture string) map[string]string {
	byMail := map[string]string{}
	for _, u := range devstack.Seed(fixture).Users {
		byMail[strings.ToLower(u.Mail)] = u.ID
	}
	out := map[string]string{}
	for _, p := range fixtures.People(fixture) {
		if p.InDirectory && p.Email != "" {
			out[p.SlackID] = byMail[strings.ToLower(p.Email)]
		}
	}
	return out
}

// expectedChats computes, from the export and the fixture's facts alone,
// which direct messages must be migrated and what each must hold. It shares
// nothing with the pipeline but the message classifier.
//
// A conversation migrates only if it has at least two participants and
// every one of them is a current, human, full member of the workspace with
// an account of their own, and it has something to import.
func (r *harness) expectedChats(fixture string) (want []wantChat, skipped []string) {
	r.t.Helper()
	ex, err := export.Open(r.export)
	if err != nil {
		r.t.Fatal(err)
	}
	defer ex.Close()
	users, err := ex.Users()
	if err != nil {
		r.t.Fatal(err)
	}
	byID := map[string]slack.User{}
	for _, u := range users {
		byID[u.ID] = u
	}
	entra := entraIDs(fixture)
	chans, err := ex.Channels()
	if err != nil {
		r.t.Fatal(err)
	}
	for _, ch := range chans {
		if ch.Kind != slack.KindDM && ch.Kind != slack.KindMPIM {
			continue
		}
		w := wantChat{slackID: ch.ID, group: ch.Kind == slack.KindMPIM}
		ok := true
		seen := map[string]bool{}
		for _, id := range ch.Members {
			if seen[id] {
				continue
			}
			seen[id] = true
			u, known := byID[id]
			if !known || u.Deleted || u.IsBot || u.IsAppUser || u.IsStranger || u.IsGuest() || entra[id] == "" {
				ok = false
			}
			w.members = append(w.members, entra[id])
		}
		if len(seen) < 2 {
			ok = false
		}
		for msg, err := range ex.Messages(ch) {
			if err == nil && transform.Classify(msg).Disposition != transform.Skip {
				w.msgs = append(w.msgs, msg)
			}
		}
		if !ok || len(w.msgs) == 0 {
			skipped = append(skipped, ch.ID)
			continue
		}
		sort.Strings(w.members)
		want = append(want, w)
	}
	return want, skipped
}

// posts expands a conversation's messages to one entry per post: a message
// too long for one post is split, and the ledger says into how many.
func (r *harness) posts(ctx context.Context, runID string, w wantChat) []slack.Message {
	r.t.Helper()
	var out []slack.Message
	for _, msg := range w.msgs {
		parts := 0
		for {
			_, err := r.store.GetMessage(ctx, runID, store.MessageKey{ChannelID: w.slackID, SlackTS: msg.TS, Part: parts})
			if err != nil {
				break
			}
			parts++
		}
		switch {
		case parts == 0:
			r.t.Errorf("conversation %s: message %s has no post in the ledger", w.slackID, msg.TS)
			parts = 1
		case len(msg.Text) > 90_000 && parts < 2:
			r.t.Errorf("conversation %s: a message of %d bytes was not split", w.slackID, len(msg.Text))
		case len(msg.Text) < 50_000 && parts != 1:
			r.t.Errorf("conversation %s: a message of %d bytes became %d posts", w.slackID, len(msg.Text), parts)
		}
		for range parts {
			out = append(out, msg)
		}
	}
	return out
}

func chatPosts(want []wantChat) int {
	n := 0
	for _, w := range want {
		n += len(w.msgs)
	}
	return n
}

// seeded describes a chat people already had before the migration.
type seeded struct {
	id       string
	messages []fakegraph.MessageView
}

func (r *harness) snapshotChat(id string) seeded {
	return seeded{id: id, messages: r.fake.ChatMessages(id)}
}

// verifyChats asserts that the fake tenant holds, for every conversation
// that must migrate, exactly one chat with exactly its messages: none
// missing, none twice, in order, each from its own author at its own time,
// with no thread and no attachment. It asserts that nothing else was
// created, and that the messages people had already written are untouched.
// mode is the migration mode every such chat must be in.
func (r *harness) verifyChats(v controller.RunView, fixture string, want []wantChat, skipped []string, before []seeded, mode string) {
	t := r.t
	t.Helper()
	ctx := context.Background()
	entra := entraIDs(fixture)
	// The account that speaks for authors who have none of their own: a
	// bot's message inside a conversation, say. It writes; it is never a
	// participant.
	standIn := ""
	for _, u := range devstack.Seed(fixture).Users {
		if u.UserPrincipalName == devstack.FormerMemberUPN {
			standIn = u.ID
		}
	}

	chats := r.fake.Chats()
	byKey := map[string][]fakegraph.ChatView{}
	for _, c := range chats {
		ids := make([]string, 0, len(c.Members))
		for _, m := range c.Members {
			ids = append(ids, m.UserID)
		}
		key := memberKey(c.ChatType == "group", ids)
		byKey[key] = append(byKey[key], c)
	}
	preexisting := map[string]seeded{}
	for _, s := range before {
		preexisting[s.id] = s
	}
	// Nothing but the chats of migrating conversations (and those that
	// were there before) may exist: a skipped conversation creates nothing.
	extra := 0
	for _, s := range before {
		found := false
		for _, w := range want {
			for _, c := range byKey[memberKey(w.group, w.members)] {
				found = found || c.ID == s.id
			}
		}
		if !found {
			extra++
		}
	}
	if len(chats) != len(want)+extra {
		t.Errorf("the tenant has %d chats, want %d for the migrating conversations and %d that were there before", len(chats), len(want), extra)
	}

	progress := map[string]store.ChannelProgress{}
	for _, c := range v.Progress.Channels {
		progress[c.SlackID] = c
	}
	for _, id := range skipped {
		if _, ok := progress[id]; ok {
			t.Errorf("conversation %s must be skipped but is part of the run", id)
		}
	}

	for _, w := range want {
		found := byKey[memberKey(w.group, w.members)]
		if len(found) != 1 {
			t.Errorf("conversation %s: %d chats with its participants, want exactly 1 (a second one means creation was repeated)", w.slackID, len(found))
			continue
		}
		c := found[0]
		if c.MigrationMode != mode {
			t.Errorf("conversation %s: migration mode = %q, want %q", w.slackID, c.MigrationMode, mode)
		}
		if c.Topic != "" {
			t.Errorf("conversation %s: the chat was given the topic %q; Slack's generated names must not be carried across", w.slackID, c.Topic)
		}
		for _, m := range c.Members {
			if len(m.Roles) != 1 || m.Roles[0] != "owner" {
				t.Errorf("conversation %s: member %s has roles %v", w.slackID, m.UserID, m.Roles)
			}
			if m.UserID == standIn {
				t.Errorf("conversation %s: the account for unmapped authors is a participant of the chat", w.slackID)
			}
		}

		var imported, theirs []fakegraph.MessageView
		for _, m := range r.fake.ChatMessages(c.ID) {
			if m.ImportedBy == "" {
				theirs = append(theirs, m)
			} else {
				imported = append(imported, m)
			}
		}
		// What people wrote in Teams is exactly what it was.
		was := preexisting[c.ID].messages
		if len(theirs) != len(was) {
			t.Errorf("conversation %s: the chat has %d messages of its own, had %d before", w.slackID, len(theirs), len(was))
		}
		for i := range min(len(theirs), len(was)) {
			if theirs[i].ID != was[i].ID || theirs[i].BodyContent != was[i].BodyContent || !theirs[i].CreatedAt.Equal(was[i].CreatedAt) || theirs[i].FromUserID != was[i].FromUserID {
				t.Errorf("conversation %s: a message people wrote in Teams was changed: %+v, was %+v", w.slackID, theirs[i], was[i])
			}
		}

		// A long message is several posts. From here on w.msgs has one
		// entry per post.
		w.msgs = r.posts(ctx, v.ID, w)
		if len(imported) != len(w.msgs) {
			t.Errorf("conversation %s: %d posts imported, want %d", w.slackID, len(imported), len(w.msgs))
			continue
		}
		ids := map[string]bool{}
		for i, m := range imported {
			src := w.msgs[i]
			if ids[m.ID] {
				t.Errorf("conversation %s: message ID %s appears twice", w.slackID, m.ID)
			}
			ids[m.ID] = true
			if d := m.CreatedAt.Sub(src.Time().Truncate(time.Millisecond)); d < 0 || d > time.Second {
				t.Errorf("conversation %s message %d: Teams time is %s away from its Slack time", w.slackID, i, d)
			}
			if i > 0 && !m.CreatedAt.After(imported[i-1].CreatedAt) {
				t.Errorf("conversation %s: timestamps are not strictly increasing at %d", w.slackID, i)
			}
			if !m.CreatedAt.After(c.CreatedAt) {
				t.Errorf("conversation %s message %d is not later than its chat", w.slackID, i)
			}
			author := entra[src.User]
			if author == "" {
				author = standIn
			}
			if m.FromUserID != author {
				t.Errorf("conversation %s message %d: from %s, want %s", w.slackID, i, m.FromUserID, author)
			}
			// A chat has no threads and, in this migration, no files.
			if m.ReplyToID != "" {
				t.Errorf("conversation %s message %d was posted as a reply", w.slackID, i)
			}
			if len(m.Attachments) != 0 || m.HostedContents != 0 {
				t.Errorf("conversation %s message %d carries %d attachments and %d inline images; files of a direct message are not moved",
					w.slackID, i, len(m.Attachments), m.HostedContents)
			}
			if n := strings.Count(m.BodyContent, "(not migrated)"); n != len(src.Files) {
				t.Errorf("conversation %s message %d: %d \"not migrated\" notes for %d files", w.slackID, i, n, len(src.Files))
			}
			if strings.Contains(m.BodyContent, "taut-file:") {
				t.Errorf("conversation %s message %d still has a file marker in its body", w.slackID, i)
			}
			// The body is the message's own: nothing swapped, nothing
			// added to say what a reply answered.
			if len(src.Text) < 1000 && plainText.MatchString(src.Text) && len(src.Blocks) == 0 && !strings.Contains(m.BodyContent, html.EscapeString(src.Text)) {
				t.Errorf("conversation %s message %d does not carry the text of Slack message %s", w.slackID, i, src.TS)
			}
			wroteQuote := strings.Contains(string(src.Blocks), "rich_text_quote") || strings.Contains(src.Text, "&gt;")
			if src.IsReply() && !wroteQuote && strings.Contains(m.BodyContent, "<blockquote") {
				t.Errorf("conversation %s message %d has a quote block nobody wrote", w.slackID, i)
			}
		}

		cp, ok := progress[w.slackID]
		kind := "dm"
		if w.group {
			kind = "mpim"
		}
		if !ok || cp.Kind != kind || cp.Team != "" || cp.Posted != len(w.msgs) || cp.Failed != 0 || cp.Held != 0 {
			t.Errorf("conversation %s: progress = %+v (in run: %v)", w.slackID, cp, ok)
		}

		// The audit log: the chat came to exist once, and every message
		// was imported once.
		ops := map[string]int{}
		var seq int64
		for {
			entries, err := r.store.LogEntries(ctx, v.ID, "load:"+w.slackID, seq, 1000)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				ops[e.Op]++
				seq = e.Seq
				if (e.Op == "chat.create" || e.Op == "chat.adopt" || e.Op == "chat.start_migration") && e.TargetID != c.ID {
					t.Errorf("conversation %s: audit entry %s names chat %s, want %s", w.slackID, e.Op, e.TargetID, c.ID)
				}
			}
			if len(entries) < 1000 {
				break
			}
		}
		if ops["chat.create"]+ops["chat.adopt"] != 1 {
			t.Errorf("conversation %s: audit log has %d chat.create and %d chat.adopt entries, want one in all", w.slackID, ops["chat.create"], ops["chat.adopt"])
		}
		if ops["chat.start_migration"] < 1 {
			t.Errorf("conversation %s: audit log has no chat.start_migration entry", w.slackID)
		}
		if got := ops["message.import"] + ops["message.adopt"]; got != len(w.msgs) {
			t.Errorf("conversation %s: audit log records %d imported messages, want %d", w.slackID, got, len(w.msgs))
		}
	}
	if err := r.store.VerifyLog(ctx, v.ID); err != nil {
		t.Errorf("audit log does not verify: %v", err)
	}
}

func TestMigrateDirectMessagesSmall(t *testing.T) {
	r := newHarnessWith(t, "small", withDMs)
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	defer kill()

	want, skipped := r.expectedChats("small")
	if len(want) != 2 || len(skipped) != 0 {
		t.Fatalf("the small fixture should have a direct message and a group DM to migrate; got %d, %d skipped", len(want), len(skipped))
	}

	run := r.start(ctx)
	v := r.await(ctx, run.ID, 60*time.Second)
	// The channels are exactly what they are without direct messages.
	w := r.expected()
	w.chatPosts = chatPosts(want)
	r.verify(v, w)
	r.verifyChats(v, "small", want, skipped, nil, "inProgress")
	if v.Progress.Adopted != 0 {
		t.Errorf("adopted = %d on an undisturbed run", v.Progress.Adopted)
	}

	// The group chat's members were told to see the history from before
	// its first message; the one-on-one chat's were not asked anything.
	for _, c := range r.fake.Chats() {
		for _, m := range c.Members {
			switch {
			case c.ChatType == "group" && (m.VisibleHistoryStart == nil || !m.VisibleHistoryStart.Equal(c.CreatedAt)):
				t.Errorf("group chat member %s sees history from %v; the chat starts at %s", m.UserID, m.VisibleHistoryStart, c.CreatedAt)
			case c.ChatType == "oneOnOne" && m.VisibleHistoryStart != nil:
				t.Errorf("one-on-one chat member %s was given a history start", m.UserID)
			}
		}
		if c.CreatedBy != fakegraph.DefaultClientID || c.MigrationOwner != fakegraph.DefaultClientID {
			t.Errorf("chat %s: created by %q, migration owned by %q", c.ID, c.CreatedBy, c.MigrationOwner)
		}
	}
	// A new chat is recorded as created, not adopted.
	if n := r.logCount(ctx, run.ID, store.LogFilter{Op: "chat.create"}); n != 2 {
		t.Errorf("audit log has %d chat.create entries, want 2", n)
	}
	if n := r.fake.CountRequests("GET", "/v1.0/users/*/chats"); n != 0 {
		t.Errorf("an undisturbed run looked for an interrupted group chat creation %d times", n)
	}

	// Finalizing completes the chats along with the channels.
	fv, err := r.client.Finalize(ctx, run.ID, controller.FinalizeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if fv.Chats != 2 || fv.Channels != len(v.Progress.Channels)-2 || fv.Already != 0 || fv.Teams != 1 {
		t.Errorf("finalize selection = %+v", fv)
	}
	imports := r.fake.Stats().ImportsAccepted
	v = r.awaitFinalize(ctx, run.ID, 60*time.Second)
	if v.Status != store.RunFinalized {
		t.Fatalf("status = %s, want finalized; problems %+v", v.Status, v.Progress.Problems)
	}
	r.verifyChats(v, "small", want, skipped, nil, "completed")
	for _, c := range v.Progress.Channels {
		if c.State != store.ChannelCompleted {
			t.Errorf("conversation %s state = %s after finalize", c.SlackID, c.State)
		}
	}
	if n := r.logCount(ctx, run.ID, store.LogFilter{Op: "chat.complete_migration"}); n != 2 {
		t.Errorf("audit log has %d chat.complete_migration entries, want 2", n)
	}
	if n := r.logCount(ctx, run.ID, store.LogFilter{Op: "channel.complete_migration"}); n != len(v.Progress.Channels)-2 {
		t.Errorf("audit log has %d channel.complete_migration entries, want %d", n, len(v.Progress.Channels)-2)
	}
	if r.fake.Stats().ImportsAccepted != imports {
		t.Error("finalizing imported messages")
	}
	// A chat takes no part in team membership: nobody was added to the
	// team for having been in a direct message, beyond the channels' own
	// members.
	if n := r.fake.CountRequests("POST", "/v1.0/chats/*/members"); n != 0 {
		t.Errorf("%d requests added members to a chat", n)
	}
}

// With direct messages off, a run touches no chat at all, whatever the
// export holds.
func TestDirectMessagesOffTouchesNoChat(t *testing.T) {
	r := newHarness(t, "small")
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	run := r.start(ctx)
	v := r.await(ctx, run.ID, 60*time.Second)
	r.verify(v, r.expected())
	if n := len(r.fake.Chats()); n != 0 {
		t.Errorf("%d chats were created with direct messages off", n)
	}
	for _, rec := range r.fake.Requests() {
		if strings.Contains(rec.Path, "/chats") {
			t.Fatalf("a request went to %s %s with direct messages off", rec.Method, rec.Path)
		}
	}
	for _, c := range v.Progress.Channels {
		if c.Kind != "public" {
			t.Errorf("conversation %s of kind %q is part of the run", c.SlackID, c.Kind)
		}
	}
}

// The edge cases: conversations that cannot become chats, a chat two people
// already use with a message of theirs on the very millisecond of a Slack
// message, a chat older than its Slack history, thread replies between
// other messages, files and an inline image, and messages within one
// millisecond.
func TestMigrateDirectMessagesNasty(t *testing.T) {
	// Only one small channel besides the direct messages: the channels of
	// this fixture have their own test.
	r := newHarnessWith(t, "nasty", func(m *config.Config) {
		withDMs(m)
		m.Target.Teams[0].Channels.Include = []string{"general"}
	})
	ctx := t.Context()

	want, skipped := r.expectedChats("nasty")
	if len(want) != 3 || len(skipped) != 6 {
		t.Fatalf("the nasty fixture should have 3 direct messages to migrate and 6 that cannot; got %d and %d", len(want), len(skipped))
	}
	var plain, threaded wantChat
	for _, w := range want {
		switch {
		case w.group:
		case len(w.msgs) == 2:
			plain = w
		default:
			threaded = w
		}
	}
	if plain.slackID == "" || threaded.slackID == "" {
		t.Fatal("the nasty fixture has lost its plain or its threaded direct message")
	}
	replies, files := 0, 0
	for _, m := range threaded.msgs {
		if m.IsReply() {
			replies++
		}
		files += len(m.Files)
	}
	if replies < 3 || files != 2 {
		t.Fatalf("the threaded direct message has %d thread replies and %d files; it should have at least 3 and 2", replies, files)
	}

	// Ada and Ben already chat in Teams. Their chat is newer than their
	// Slack history, and Ada wrote a message there in the very millisecond
	// of Ben's Slack reply. (Ben's Slack message would be taken for one of
	// Taut's own if it were his; it is not.)
	first, second := plain.msgs[0], plain.msgs[1]
	created := first.Time().Add(30 * time.Second).Truncate(time.Millisecond)
	clash := second.Time().Truncate(time.Millisecond)
	other := plain.members[0]
	if other == entraIDs("nasty")[second.User] {
		other = plain.members[1]
	}
	existing := r.fake.AddChat(fakegraph.Chat{
		ChatType: "oneOnOne", Members: plain.members, CreatedDateTime: &created,
		Messages: []fakegraph.ChatMessage{
			{CreatedDateTime: clash, From: other, Content: "<p>written in Teams, on the millisecond of a Slack message</p>"},
			{CreatedDateTime: clash.Add(time.Hour), From: plain.members[1], Content: "<p>written in Teams later</p>"},
		},
	})
	// Dev and Elena have a chat that is older than anything in Slack.
	ancient := time.Date(2019, 6, 1, 8, 0, 0, 0, time.UTC)
	older := r.fake.AddChat(fakegraph.Chat{ChatType: "oneOnOne", Members: threaded.members, CreatedDateTime: &ancient})
	before := []seeded{r.snapshotChat(existing), r.snapshotChat(older)}

	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	run := r.start(ctx)
	v := r.await(ctx, run.ID, 2*time.Minute)
	if v.Status != store.RunCompleted {
		t.Fatalf("status = %s; problems: %+v", v.Status, v.Progress.Problems)
	}
	if v.Progress.Failed != 0 || v.Progress.Held != 0 {
		t.Errorf("failed=%d held=%d, want none", v.Progress.Failed, v.Progress.Held)
	}
	r.verifyChats(v, "nasty", want, skipped, before, "inProgress")

	byID := map[string]fakegraph.ChatView{}
	for _, c := range r.fake.Chats() {
		byID[c.ID] = c
	}
	// The existing chat was moved back to before the Slack history, and
	// the history went into it, not into a new chat.
	if c := byID[existing]; !c.CreatedAt.Before(first.Time()) || c.OriginalCreatedAt == nil || !c.OriginalCreatedAt.Equal(created) || c.MessageCount != 4 {
		t.Errorf("the chat Ada and Ben already had: %+v", c)
	}
	// The one older than its history kept its age, but for the second
	// startMigration always moves a creation time back by: the value it is
	// given must be older than the current one, and one is always given.
	if c := byID[older]; !c.CreatedAt.Equal(ancient.Add(-time.Second)) || c.MessageCount != len(r.posts(ctx, v.ID, threaded)) {
		t.Errorf("the chat Dev and Elena already had: %+v", c)
	}
	// Both are recorded as adopted; the group chat as created.
	if n := r.logCount(ctx, run.ID, store.LogFilter{Op: "chat.adopt"}); n != 2 {
		t.Errorf("audit log has %d chat.adopt entries, want 2", n)
	}
	if n := r.logCount(ctx, run.ID, store.LogFilter{Op: "chat.create"}); n != 1 {
		t.Errorf("audit log has %d chat.create entries, want 1", n)
	}

	// The Slack message whose millisecond was taken went in one
	// millisecond later, and the audit log says so.
	var moved []fakegraph.MessageView
	for _, m := range r.fake.ChatMessages(existing) {
		if m.ImportedBy != "" && !m.CreatedAt.Equal(first.Time().Truncate(time.Millisecond)) {
			moved = append(moved, m)
		}
	}
	if len(moved) != 1 || !moved[0].CreatedAt.Equal(clash.Add(time.Millisecond)) {
		t.Errorf("the message whose millisecond was taken: %+v, want it at %s", moved, clash.Add(time.Millisecond))
	}
	page, err := r.client.AuditLog(ctx, run.ID, store.LogFilter{Stream: "load:" + plain.slackID, Op: "message.import"})
	if err != nil {
		t.Fatal(err)
	}
	notes := 0
	for _, e := range page.Entries {
		if strings.Contains(e.Detail, "1 ms later") {
			notes++
		}
	}
	if notes != 1 {
		t.Errorf("%d audit entries say a message was imported later than allocated, want 1", notes)
	}
	row, err := r.store.GetMessage(ctx, run.ID, store.MessageKey{ChannelID: plain.slackID, SlackTS: second.TS})
	if err != nil || row.ShiftMS != 1 || row.TargetMS != clash.UnixMilli()+1 || row.State != store.MessagePosted {
		t.Errorf("ledger row of the moved message = %+v, %v", row, err)
	}
	if r.fake.Stats().Conflicts == 0 {
		t.Error("Graph never refused a timestamp; the collision was not exercised")
	}

	// Files of direct messages are counted, as not moved, and none was
	// fetched from Slack or uploaded.
	p := v.Progress
	if p.FilesSkipped < files {
		t.Errorf("%d files recorded as skipped, want at least the %d of the direct message", p.FilesSkipped, files)
	}
	for _, id := range []string{plain.slackID, threaded.slackID} {
		for _, lane := range []string{"files:" + id} {
			lanes, err := r.store.Lanes(ctx, run.ID, store.StageFiles)
			if err != nil {
				t.Fatal(err)
			}
			for _, l := range lanes {
				if l.ID == lane {
					t.Errorf("direct message %s has a files lane", id)
				}
			}
		}
	}
	for _, m := range threaded.msgs {
		for _, f := range m.Files {
			row, err := r.store.GetFile(ctx, run.ID, threaded.slackID, f.ID)
			if err != nil || row.State != store.FileSkipped || row.ErrorCode != "dm_files_not_moved" {
				t.Errorf("file %s of a direct message = %+v, %v; want skipped as dm_files_not_moved", f.ID, row, err)
			}
		}
	}
	if stats := r.fake.Stats(); stats.UploadsCompleted != 0 {
		// The one channel selected, #general, has no files in this fixture.
		t.Errorf("%d files were uploaded", stats.UploadsCompleted)
	}
}

// Graph accepts a group chat, the worker never learns its ID. The retry
// finds the chat instead of making a second one.
func TestGroupChatCreationSurvivesALostResponse(t *testing.T) {
	r := newHarnessWith(t, "small", withDMs)
	ctx := t.Context()
	r.fake.LoseGroupChatResponses(2)
	kill := r.worker(ctx, 5*time.Second)
	defer kill()

	want, skipped := r.expectedChats("small")
	run := r.start(ctx)
	v := r.await(ctx, run.ID, 2*time.Minute)
	w := r.expected()
	w.chatPosts = chatPosts(want)
	r.verify(v, w)
	// verifyChats asserts one chat per conversation.
	r.verifyChats(v, "small", want, skipped, nil, "inProgress")
	if n := r.logCount(ctx, run.ID, store.LogFilter{Op: "chat.adopt"}); n != 1 {
		t.Errorf("audit log has %d chat.adopt entries, want 1: the group chat found after its creation was interrupted", n)
	}
	if n := r.fake.CountRequests("GET", "/v1.0/users/*/chats"); n == 0 {
		t.Error("the retry did not look for the chat the lost request created")
	}
	groups := 0
	for _, c := range r.fake.Chats() {
		if c.ChatType == "group" {
			groups++
		}
	}
	if groups != 1 {
		t.Errorf("%d group chats exist, want 1", groups)
	}
}

// If Graph does not take a history start time on the members of a new
// chat, the chat is created without it and the audit log says so.
func TestGroupChatWithoutHistoryStart(t *testing.T) {
	r := newHarnessWith(t, "small", withDMs, fakegraph.WithChatHistoryStartRefused())
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	want, skipped := r.expectedChats("small")
	run := r.start(ctx)
	v := r.await(ctx, run.ID, time.Minute)
	r.verifyChats(v, "small", want, skipped, nil, "inProgress")
	page, err := r.client.AuditLog(ctx, run.ID, store.LogFilter{Op: "chat.create"})
	if err != nil {
		t.Fatal(err)
	}
	said := 0
	for _, e := range page.Entries {
		if strings.Contains(e.Detail, "without a history start time") {
			said++
		}
	}
	if len(page.Entries) != 2 || said != 1 {
		t.Errorf("chat.create entries = %+v; want 2, one of them saying the history start was refused", page.Entries)
	}
}

// A chat two people already had that cannot be made old enough: the
// messages older than it are recorded as not imported, with the reason, and
// the rest are imported. A chat Taut itself made that Graph leaves new is
// another matter: nothing could go into it, so its lane fails and says so.
func TestDirectMessagesOlderThanTheirChat(t *testing.T) {
	r := newHarnessWith(t, "small", withDMs, fakegraph.WithChatBackdatingIgnored())
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
	// The two people already have a chat, created between the two days of
	// their Slack history.
	firstDay := dm.msgs[0].Time().Format("2006-01-02")
	old := 0
	for _, m := range dm.msgs {
		if m.Time().Format("2006-01-02") == firstDay {
			old++
		}
	}
	if old == 0 || old == len(dm.msgs) {
		t.Fatalf("the small fixture's direct message should span two days; %d of %d messages are on the first", old, len(dm.msgs))
	}
	created := dm.msgs[old-1].Time().Add(time.Hour).Truncate(time.Millisecond)
	chatID := r.fake.AddChat(fakegraph.Chat{ChatType: "oneOnOne", Members: dm.members, CreatedDateTime: &created})

	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	run := r.start(ctx)
	v := r.await(ctx, run.ID, time.Minute)
	// The group chat is new, and with a service that moves no creation
	// time back it is younger than all of its history. That is not a few
	// old messages; it stops the lane.
	if v.Status != store.RunFailed {
		t.Fatalf("status = %s, want failed: a new chat that cannot be backdated can take nothing", v.Status)
	}
	problem := ""
	for _, p := range v.Progress.Problems {
		if p.LaneID == "load:"+group.slackID {
			problem = p.Error
		}
	}
	if !strings.Contains(problem, "load.chat_not_backdated") || strings.Contains(problem, "Ada") {
		t.Errorf("the group DM's lane failed with %q", problem)
	}
	for _, c := range r.fake.Chats() {
		if c.ChatType == "group" && c.MessageCount != 0 {
			t.Errorf("the group chat holds %d messages older than itself", c.MessageCount)
		}
	}
	if v.Progress.Failed != old {
		t.Errorf("%d posts failed, want the %d older than the existing chat", v.Progress.Failed, old)
	}
	got := r.fake.ChatMessages(chatID)
	if len(got) != len(dm.msgs)-old {
		t.Fatalf("the chat holds %d messages, want %d", len(got), len(dm.msgs)-old)
	}
	for i, m := range got {
		if src := dm.msgs[old+i]; !m.CreatedAt.Equal(src.Time().Truncate(time.Millisecond)) {
			t.Errorf("message %d is at %s, want %s", i, m.CreatedAt, src.Time())
		}
	}
	if n := r.logCount(ctx, run.ID, store.LogFilter{Stream: "load:" + dm.slackID, Op: "message.fail", Contains: "older_than_chat"}); n != old {
		t.Errorf("audit log has %d message.fail entries for messages older than the chat, want %d", n, old)
	}
	// Graph was never asked to take a message it would refuse.
	for _, rec := range r.fake.Requests() {
		if rec.Method == "POST" && strings.HasSuffix(rec.Path, "/messages") && strings.Contains(rec.Path, "/chats/") && rec.Status >= 400 {
			t.Errorf("POST %s was refused with %d", rec.Path, rec.Status)
		}
	}
	// The audit log says what the chat's age means.
	if n := r.logCount(ctx, run.ID, store.LogFilter{Stream: "load:" + dm.slackID, Op: "chat.start_migration", Contains: "cannot be imported"}); n != 1 {
		t.Errorf("the chat.start_migration entry does not say what cannot be imported (%d matching entries)", n)
	}

	// The failed run's chats are not left in migration mode: the operator
	// asks for them back, and gets them, without finishing the run.
	fv, err := r.client.Finalize(ctx, run.ID, controller.FinalizeRequest{Channels: []string{dm.slackID, group.slackID}})
	if err != nil {
		t.Fatal(err)
	}
	if fv.Releasing != 2 || fv.Run.Status != store.RunFailed {
		t.Errorf("releasing the chats of a failed run: %+v", fv)
	}
	r.awaitRelease(ctx, run.ID, time.Minute)
	for _, c := range r.fake.Chats() {
		if c.MigrationMode == "inProgress" {
			t.Errorf("chat %s of a failed run is still in migration mode", c.ID)
		}
	}
	if n := r.logCount(ctx, run.ID, store.LogFilter{Op: "chat.release_migration"}); n != 2 {
		t.Errorf("audit log has %d chat.release_migration entries, want 2", n)
	}
	// Channels cannot be had that way: the import has not finished.
	if _, err := r.client.Finalize(ctx, run.ID, controller.FinalizeRequest{}); err == nil {
		t.Error("a failed run was finalized")
	}
}

// awaitRelease waits until a run has no release work left.
func (r *harness) awaitRelease(ctx context.Context, runID string, limit time.Duration) {
	r.t.Helper()
	deadline := time.Now().Add(patient(limit))
	for {
		lanes, err := r.store.Lanes(ctx, runID, store.StageRelease)
		if err != nil {
			r.t.Fatal(err)
		}
		open := 0
		for _, l := range lanes {
			if l.Status != store.LaneDone {
				open++
			}
		}
		if open == 0 {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("%d of %d release lanes are not done after %s: %+v", open, len(lanes), limit, lanes)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// A chat whose migration something else completed mid-import is put back
// into migration mode and the import carries on, with nothing lost and
// nothing twice.
func TestMigrateReopensChatsThatLeftMigrationMode(t *testing.T) {
	r := newHarnessWith(t, "small", withDMs)
	ctx := t.Context()
	want, skipped := r.expectedChats("small")
	// Slow the imports down enough to act between two of them.
	r.fake.Faults().MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/chats/*/messages", Action: fakegraph.ActionLatency, LatencyMS: 120})
	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	run := r.start(ctx)

	m, err := config.Parse(r.yaml)
	if err != nil {
		t.Fatal(err)
	}
	g, err := r.graphFactory()(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	// As soon as a chat has its first message, complete its migration
	// behind Taut's back.
	closed := map[string]bool{}
	deadline := time.Now().Add(time.Minute)
	for len(closed) < len(want) {
		for _, c := range r.fake.Chats() {
			if !closed[c.ID] && c.MigrationMode == "inProgress" && c.MessageCount >= 1 {
				if c.MessageCount >= 6 {
					t.Fatalf("chat %s was fully imported before it could be closed; the test proves nothing", c.ID)
				}
				if err := g.CompleteChatMigration(ctx, c.ID); err != nil {
					t.Fatal(err)
				}
				closed[c.ID] = true
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the import of the direct messages never got under way")
		}
		time.Sleep(5 * time.Millisecond)
	}

	v := r.await(ctx, run.ID, 2*time.Minute)
	w := r.expected()
	w.chatPosts = chatPosts(want)
	r.verify(v, w)
	r.verifyChats(v, "small", want, skipped, nil, "inProgress")
	if n := r.logCount(ctx, run.ID, store.LogFilter{Op: "chat.start_migration"}); n < 2*len(want) {
		t.Errorf("audit log has %d chat.start_migration entries, want at least %d: each chat was opened and reopened", n, 2*len(want))
	}
}

// The acceptance test for direct messages: the medium fixture's direct
// messages and group DMs are imported and finalized while workers are
// killed and replaced and the fake Graph loses responses after committing,
// on every write a chat needs: its creation, startMigration, each message
// and completeMigration. Every conversation must end up in exactly one
// chat with exactly its messages.
func TestDirectMessagesSurviveKillsAndFaults(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos run skipped in -short mode")
	}
	r := newHarnessWith(t, "medium", func(m *config.Config) {
		withDMs(m)
		// One channel keeps the run about direct messages.
		m.Target.Teams[0].Channels.Include = []string{"general"}
	}, fakegraph.WithRandSeed(23))
	ctx := t.Context()
	want, skipped := r.expectedChats("medium")
	groups := 0
	for _, w := range want {
		if w.group {
			groups++
		}
	}
	if len(want) < 3 || groups == 0 || len(skipped) == 0 {
		t.Fatalf("the medium fixture should have direct messages and group DMs to migrate and some to skip; got %d (%d groups), %d skipped", len(want), groups, len(skipped))
	}

	faults := r.fake.Faults()
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/chats", Action: fakegraph.ActionFailAfterCommit, Probability: 0.3})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/chats", Action: fakegraph.ActionStatus, Status: 503, Probability: 0.1})
	faults.MustAdd(fakegraph.Fault{Method: "GET", Path: "/v1.0/users/*/chats", Action: fakegraph.ActionStatus, Status: 503, Probability: 0.2})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/chats/*/startMigration", Action: fakegraph.ActionFailAfterCommit, Probability: 0.4})
	faults.MustAdd(fakegraph.Fault{Method: "GET", Path: "/v1.0/chats/*", Action: fakegraph.ActionStatus, Status: 503, Probability: 0.1})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/chats/*/messages", Action: fakegraph.ActionFailAfterCommit, Probability: 0.08})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/chats/*/messages", Action: fakegraph.ActionThrottle, RetryAfterSeconds: 1, Probability: 0.03})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/chats/*/messages", Action: fakegraph.ActionStatus, Status: 503, Probability: 0.02})
	faults.MustAdd(fakegraph.Fault{Method: "GET", Path: "/v1.0/chats/*/messages/**", Action: fakegraph.ActionStatus, Status: 503, Probability: 0.05})
	// And the one a path cannot single out: a group chat that was created
	// with its answer lost, certainly at least once.
	r.fake.LoseGroupChatResponses(groups)

	run := r.start(ctx)
	var mu sync.Mutex
	kills := []func(){r.worker(ctx, 600*time.Millisecond), r.worker(ctx, 600*time.Millisecond), r.worker(ctx, 600*time.Millisecond)}
	killed := 0
	chaos := func() (stop func()) {
		quit, done := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(done)
			rng := rand.New(rand.NewPCG(29, 31))
			for {
				select {
				case <-quit:
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
		return func() { close(quit); <-done }
	}

	stop := chaos()
	v := r.await(ctx, run.ID, 4*time.Minute)
	stop()
	if v.Status != store.RunCompleted {
		t.Fatalf("status = %s; problems: %+v", v.Status, v.Progress.Problems)
	}
	if v.Progress.Failed != 0 || v.Progress.Held != 0 {
		t.Errorf("failed=%d held=%d, want none", v.Progress.Failed, v.Progress.Held)
	}
	r.verifyChats(v, "medium", want, skipped, nil, "inProgress")
	stats := r.fake.Stats()
	if stats.ImportsAccepted != v.Progress.Posted {
		t.Errorf("Graph accepted %d imports but %d posts exist: a message was written twice", stats.ImportsAccepted, v.Progress.Posted)
	}
	importKills := killed

	// Finalize under the same treatment.
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/chats/*/completeMigration", Action: fakegraph.ActionFailAfterCommit, Probability: 0.5})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/chats/*/completeMigration", Action: fakegraph.ActionThrottle, RetryAfterSeconds: 1, Probability: 0.1})
	if _, err := r.client.Finalize(ctx, run.ID, controller.FinalizeRequest{}); err != nil {
		t.Fatal(err)
	}
	stop = chaos()
	v = r.awaitFinalize(ctx, run.ID, 3*time.Minute)
	stop()
	mu.Lock()
	for _, k := range kills {
		k()
	}
	mu.Unlock()
	if v.Status != store.RunFinalized {
		t.Fatalf("status = %s, want finalized; problems %+v", v.Status, v.Progress.Problems)
	}
	r.verifyChats(v, "medium", want, skipped, nil, "completed")
	// Each chat's completion is recorded once, however many attempts and
	// lost answers it took.
	if n := r.logCount(ctx, run.ID, store.LogFilter{Op: "chat.complete_migration"}); n != len(want) {
		t.Errorf("audit log has %d chat.complete_migration entries, want %d", n, len(want))
	}
	if r.fake.Stats().ImportsAccepted != stats.ImportsAccepted {
		t.Error("finalizing imported messages")
	}

	adopted := r.logCount(ctx, run.ID, store.LogFilter{Op: "chat.adopt"})
	t.Logf("workers killed: %d during the import, %d during finalize; conversations: %d (%d groups), %d skipped; group chats found after a lost creation: %d; messages adopted after a lost response: %d; conflicts (409): %d",
		importKills, killed-importKills, len(want), groups, len(skipped), adopted, v.Progress.Adopted, stats.Conflicts)
	if importKills < 3 {
		t.Errorf("only %d workers were killed during the import; the run was too short to prove anything", importKills)
	}
	if adopted == 0 {
		t.Error("no group chat was found after a lost creation; the recovery path was never exercised")
	}
	if v.Progress.Adopted == 0 || stats.Conflicts == 0 {
		t.Errorf("the message recovery path was never exercised (adopted=%d, conflicts=%d)", v.Progress.Adopted, stats.Conflicts)
	}
}

// Whether Graph lets someone who is not in a chat be the author of a
// message imported into it is not documented. A bot's message inside a
// direct message is attributed to the account for unmapped authors, which
// is never a participant. If Graph refuses that, the one message is
// recorded as not imported and the conversation carries on.
func TestDirectMessageAuthorWhoIsNotAParticipant(t *testing.T) {
	r := newHarnessWith(t, "nasty", func(m *config.Config) {
		withDMs(m)
		m.Target.Teams[0].Channels.Include = []string{"general"}
	}, fakegraph.WithChatAuthorsMustBeMembers())
	ctx := t.Context()
	want, _ := r.expectedChats("nasty")
	entra := entraIDs("nasty")
	outsiders, total := 0, 0
	for _, w := range want {
		for _, m := range w.msgs {
			total++
			if entra[m.User] == "" {
				outsiders++
			}
		}
	}
	if outsiders == 0 {
		t.Fatal("the nasty fixture has no message in a direct message from someone who is not a participant")
	}
	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	run := r.start(ctx)
	v := r.await(ctx, run.ID, 2*time.Minute)
	if v.Status != store.RunCompleted {
		t.Fatalf("status = %s; problems: %+v", v.Status, v.Progress.Problems)
	}
	imported := 0
	for _, c := range r.fake.Chats() {
		imported += c.MessageCount
	}
	total = 0
	for _, w := range want {
		total += len(r.posts(ctx, run.ID, w))
	}
	// More refusals in a row than a lane tolerates in a channel, and the
	// lane went on: in a chat a refused author says nothing about the chat.
	if outsiders < 6 {
		t.Fatalf("only %d messages from outside the conversation; the fixture should have a run of them", outsiders)
	}
	if v.Progress.Failed != outsiders || imported != total-outsiders {
		t.Errorf("%d posts failed and %d are in chats; want %d refused and %d imported", v.Progress.Failed, imported, outsiders, total-outsiders)
	}
	if n := r.logCount(ctx, run.ID, store.LogFilter{Op: "message.fail", Contains: "graph.invalid_author"}); n != outsiders {
		t.Errorf("audit log has %d message.fail entries for a refused author, want %d", n, outsiders)
	}
}

// Chats and channels can be finalized apart. Completing the chats first
// opens no team to anybody, and the run stays importable for its channels;
// completing the channels then finishes the run.
func TestFinalizeChatsApartFromChannels(t *testing.T) {
	r := newHarnessWith(t, "small", withDMs)
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	want, skipped := r.expectedChats("small")
	run := r.start(ctx)
	v := r.await(ctx, run.ID, time.Minute)

	var chatIDs, channelIDs []string
	for _, c := range v.Progress.Channels {
		if store.IsChatKind(c.Kind) {
			chatIDs = append(chatIDs, c.SlackID)
		} else {
			channelIDs = append(channelIDs, c.SlackID)
		}
	}
	fv, err := r.client.Finalize(ctx, run.ID, controller.FinalizeRequest{Channels: chatIDs})
	if err != nil {
		t.Fatal(err)
	}
	if fv.Chats != 2 || fv.Channels != 0 || fv.Teams != 0 {
		t.Errorf("finalizing the chats alone selected %+v", fv)
	}
	v = r.awaitFinalize(ctx, run.ID, time.Minute)
	if v.Status != store.RunCompleted {
		t.Fatalf("status = %s after finalizing only the chats, want completed again", v.Status)
	}
	r.verifyChats(v, "small", want, skipped, nil, "completed")
	team := r.fake.Teams()[0]
	if len(team.Members) != 1 {
		t.Errorf("the team has %d members after finalizing only chats; its channels are still locked", len(team.Members))
	}
	for _, c := range r.fake.Channels(team.ID) {
		if c.MigrationMode == "completed" {
			t.Errorf("channel %q was completed by a request for the chats", c.DisplayName)
		}
	}
	if n := r.logCount(ctx, run.ID, store.LogFilter{Op: "member.add"}); n != 0 {
		t.Errorf("%d members were added by a request for the chats", n)
	}

	// The rest.
	fv, err = r.client.Finalize(ctx, run.ID, controller.FinalizeRequest{Channels: channelIDs})
	if err != nil {
		t.Fatal(err)
	}
	if fv.Chats != 0 || fv.Channels != len(channelIDs) || fv.Teams != 1 {
		t.Errorf("finalizing the channels selected %+v", fv)
	}
	v = r.awaitFinalize(ctx, run.ID, time.Minute)
	if v.Status != store.RunFinalized {
		t.Fatalf("status = %s, want finalized; problems %+v", v.Status, v.Progress.Problems)
	}
	if n := r.logCount(ctx, run.ID, store.LogFilter{Op: "chat.complete_migration"}); n != 2 {
		t.Errorf("audit log has %d chat.complete_migration entries, want 2", n)
	}
	if n := r.logCount(ctx, run.ID, store.LogFilter{Op: "member.add"}); n == 0 {
		t.Error("the team got no members once its channels were finalized")
	}
	// Everything again is answered as already done.
	fv, err = r.client.Finalize(ctx, run.ID, controller.FinalizeRequest{})
	if err != nil || fv.Chats != 2 || fv.Channels != len(channelIDs) || fv.Already != len(channelIDs)+2 {
		t.Errorf("finalize on a finalized run = %+v, %v", fv, err)
	}
}

// A conversation with nothing to import in the config file's date range
// gets no chat: the plan agrees with the preflight, and nothing people use
// is put into migration mode for nothing.
func TestDirectMessagesOutsideTheDateRangeCreateNoChat(t *testing.T) {
	r := newHarnessWith(t, "small", func(m *config.Config) {
		withDMs(m)
		m.Content.Since = "2024-03-06"
	})
	ctx := t.Context()
	// The two people of the direct message already have a chat.
	want, _ := r.expectedChats("small")
	var dm wantChat
	for _, w := range want {
		if !w.group {
			dm = w
		}
	}
	existing := r.fake.AddChat(fakegraph.Chat{ChatType: "oneOnOne", Members: dm.members})
	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	run := r.start(ctx)
	v := r.await(ctx, run.ID, time.Minute)
	if v.Status != store.RunCompleted {
		t.Fatalf("status = %s; problems: %+v", v.Status, v.Progress.Problems)
	}
	chats := r.fake.Chats()
	if len(chats) != 1 || chats[0].ID != existing || chats[0].MigrationMode != "" || chats[0].MessageCount != 0 {
		t.Errorf("chats = %+v; want only the one that was there, untouched", chats)
	}
	for _, c := range v.Progress.Channels {
		if store.IsChatKind(c.Kind) {
			t.Errorf("conversation %s is part of the run", c.SlackID)
		}
	}
	for _, rec := range r.fake.Requests() {
		if strings.Contains(rec.Path, "/chats") {
			t.Fatalf("a request went to %s %s", rec.Method, rec.Path)
		}
	}
}

// Direct messages through the sidecars: the worker holds no credential, and
// every request for a chat (its creation, its migration mode, its messages)
// goes out through the egress proxy with a token from the token broker.
func TestDirectMessagesThroughSidecars(t *testing.T) {
	r := newHarnessWith(t, "small", withDMs)
	ctx := t.Context()
	totals := budget.DefaultTotals()
	totals["graph"], totals["sharepoint"], totals["slack"] = 3000, 3000, 3000
	alloc := budget.NewAllocator(totals, 2*time.Second, time.Now)
	stopAlloc, err := alloc.Serve(r.bus)
	if err != nil {
		t.Fatal(err)
	}
	defer stopAlloc()
	r.fake.Faults().MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/chats/*/messages", Action: fakegraph.ActionFailAfterCommit, Count: 2})

	p := r.newPod(ctx, "pod-a", time.Second)
	defer p.stop()
	want, skipped := r.expectedChats("small")
	run := r.start(ctx)
	v := r.await(ctx, run.ID, 2*time.Minute)
	w := r.expected()
	w.chatPosts = chatPosts(want)
	r.verify(v, w)
	r.verifyChats(v, "small", want, skipped, nil, "inProgress")
	if v.Progress.Adopted == 0 {
		t.Error("the lost responses were not recovered from")
	}

	if _, err := r.client.Finalize(ctx, run.ID, controller.FinalizeRequest{}); err != nil {
		t.Fatal(err)
	}
	v = r.awaitFinalize(ctx, run.ID, 2*time.Minute)
	if v.Status != store.RunFinalized {
		t.Fatalf("status = %s, want finalized; problems %+v", v.Status, v.Progress.Problems)
	}
	r.verifyChats(v, "small", want, skipped, nil, "completed")
	// Every chat request the fake served carried a valid token of the app.
	chatRequests := 0
	for _, rec := range r.fake.Requests() {
		if !strings.Contains(rec.Path, "/chats") {
			continue
		}
		chatRequests++
		if rec.AppID != fakegraph.DefaultClientID {
			t.Errorf("%s %s arrived without the app's token (status %d)", rec.Method, rec.Path, rec.Status)
		}
	}
	if chatRequests == 0 {
		t.Error("no request for a chat reached the fake")
	}
}
