package graph_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/fakegraph"
	"github.com/bgfdev/taut/internal/graph"
)

const (
	alanID      = "a0000000-0000-4000-8000-000000000003"
	katherineID = "a0000000-0000-4000-8000-000000000004"
	edsgerID    = "a0000000-0000-4000-8000-000000000005" // disabled
)

func TestCreateOneOnOneChatReturnsTheExistingOne(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	first, err := e.client.CreateChat(ctx, graph.ChatSpec{Members: []string{adaID, graceID}})
	if err != nil || first.ID == "" || first.ChatType != graph.ChatOneOnOne {
		t.Fatalf("CreateChat = %+v, %v", first, err)
	}
	e.clock.Advance(time.Hour)
	// The order of the members does not matter, and nothing new is made.
	again, err := e.client.CreateChat(ctx, graph.ChatSpec{Members: []string{graceID, adaID}})
	if err != nil || again.ID != first.ID || !again.CreatedDateTime.Equal(first.CreatedDateTime) {
		t.Fatalf("second CreateChat = %+v, %v; want the chat %s created at %s", again, err, first.ID, first.CreatedDateTime)
	}
	if n := len(e.fake.Chats()); n != 1 {
		t.Fatalf("the tenant has %d chats, want 1", n)
	}

	// A chat people already had is returned too, with its own age.
	old := ts("2021-03-04T05:06:07Z")
	seeded := e.fake.AddChat(fakegraph.Chat{ChatType: "oneOnOne", Members: []string{adaID, alanID}, CreatedDateTime: &old})
	got, err := e.client.CreateChat(ctx, graph.ChatSpec{Members: []string{adaID, alanID}})
	if err != nil || got.ID != seeded || !got.CreatedDateTime.Equal(old) {
		t.Fatalf("CreateChat for an existing pair = %+v, %v", got, err)
	}

	// A lost response is retried, since the request is repeatable.
	e.fake.Faults().MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/chats", Action: fakegraph.ActionFailAfterCommit, Count: 1})
	lost, err := e.client.CreateChat(ctx, graph.ChatSpec{Members: []string{graceID, alanID}})
	if err != nil || lost.ID == "" {
		t.Fatalf("CreateChat after a lost response = %+v, %v", lost, err)
	}
	if n := len(e.fake.Chats()); n != 3 {
		t.Fatalf("the tenant has %d chats, want 3", n)
	}
}

func TestCreateGroupChat(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	start := ts("2019-05-01T00:00:00Z")

	chat, err := e.client.CreateChat(ctx, graph.ChatSpec{
		Group: true, Topic: "Launch crew", Members: []string{adaID, graceID, alanID, adaID}, VisibleHistoryStart: &start,
	})
	if err != nil || chat.ChatType != graph.ChatGroup || chat.Topic != "Launch crew" {
		t.Fatalf("CreateChat = %+v, %v", chat, err)
	}
	views := e.fake.Chats()
	if len(views) != 1 || len(views[0].Members) != 3 {
		t.Fatalf("chats = %+v", views)
	}
	for _, m := range views[0].Members {
		if len(m.Roles) != 1 || m.Roles[0] != "owner" || m.VisibleHistoryStart == nil || !m.VisibleHistoryStart.Equal(start) {
			t.Errorf("member %+v: want an owner who sees history from %s", m, start)
		}
	}

	// A group chat is new every time, so a lost response is not retried:
	// the caller has to look for the chat.
	e.fake.Faults().MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/chats", Action: fakegraph.ActionFailAfterCommit, Count: 1})
	if _, err := e.client.CreateChat(ctx, graph.ChatSpec{Group: true, Members: []string{adaID, graceID, katherineID}}); err == nil {
		t.Fatal("a group chat creation whose response was lost must fail, not be sent again")
	}
	if got := e.fake.CountRequests("POST", "/v1.0/chats"); got != 2 {
		t.Fatalf("POST /chats was sent %d times, want 2", got)
	}
	found, err := e.client.FindGroupChats(ctx, adaID, []string{katherineID, graceID, adaID}, e.clock.Now().Add(-time.Minute))
	if err != nil || len(found) != 1 {
		t.Fatalf("FindGroupChats = %+v, %v; want the chat the lost request created", found, err)
	}
	// Other member sets, and chats older than asked for, are not matches.
	if found, err := e.client.FindGroupChats(ctx, adaID, []string{adaID, graceID}, time.Time{}); err != nil || len(found) != 0 {
		t.Fatalf("FindGroupChats for a subset = %+v, %v", found, err)
	}
	if found, err := e.client.FindGroupChats(ctx, adaID, []string{adaID, graceID, katherineID}, e.clock.Now().Add(time.Minute)); err != nil || len(found) != 0 {
		t.Fatalf("FindGroupChats since the future = %+v, %v", found, err)
	}
}

func TestCreateChatRefusals(t *testing.T) {
	e := newEnv(t, withServer(fakegraph.WithChatHistoryStartRefused()))
	ctx := context.Background()
	start := ts("2019-05-01T00:00:00Z")

	for name, spec := range map[string]graph.ChatSpec{
		"one person":          {Members: []string{adaID}},
		"same person twice":   {Members: []string{adaID, adaID}},
		"three in one-on-one": {Members: []string{adaID, graceID, alanID}},
	} {
		if _, err := e.client.CreateChat(ctx, spec); code(err) != "graph.bad_request" {
			t.Errorf("%s: err = %v, want graph.bad_request", name, err)
		}
	}
	if got := e.fake.CountRequests("POST", "/v1.0/chats"); got != 0 {
		t.Fatalf("%d requests were sent for chats that can never exist", got)
	}

	_, err := e.client.CreateChat(ctx, graph.ChatSpec{Group: true, Members: []string{adaID, graceID, alanID}, VisibleHistoryStart: &start})
	if !graph.IsChatHistoryStart(err) {
		t.Fatalf("err = %v, want the history start refused", err)
	}
	if _, err := e.client.CreateChat(ctx, graph.ChatSpec{Group: true, Members: []string{adaID, graceID, alanID}}); err != nil {
		t.Fatalf("CreateChat without a history start: %v", err)
	}
	// A disabled account cannot be put in a chat.
	if _, err := e.client.CreateChat(ctx, graph.ChatSpec{Members: []string{adaID, edsgerID}}); code(err) != "graph.chat_member_invalid" {
		t.Fatalf("err = %v, want graph.chat_member_invalid", err)
	}
}

func code(err error) string {
	if fe := fail.From(err); fe != nil {
		return fe.Code
	}
	return ""
}

func TestChatMigrationAndImport(t *testing.T) {
	e := newEnv(t, withServer(fakegraph.WithPageSize(2)))
	ctx := context.Background()
	c := e.client

	// Ada and Grace already chat in Teams.
	created := ts("2024-02-01T00:00:00Z")
	chatID := e.fake.AddChat(fakegraph.Chat{
		ChatType: "oneOnOne", Members: []string{adaID, graceID}, CreatedDateTime: &created,
		Messages: []fakegraph.ChatMessage{
			{CreatedDateTime: ts("2024-02-02T10:00:00.123Z"), From: graceID, Content: "<p>written in Teams</p>"},
			{CreatedDateTime: ts("2024-02-03T10:00:00Z"), From: adaID, Content: "<p>also written in Teams</p>"},
		},
	})
	target := graph.ImportTarget{ChatID: chatID}

	// Not in migration mode yet: nothing can be imported.
	if _, err := c.ImportMessage(ctx, target, msgAt("2023-06-01T09:00:00Z")); !graph.IsNotInMigration(err) {
		t.Fatalf("import outside migration mode: %v", err)
	}
	if err := c.CompleteChatMigration(ctx, chatID); !graph.IsNotInMigration(err) {
		t.Fatalf("completeMigration outside migration mode: %v", err)
	}

	// The creation time only moves backwards.
	later := ts("2024-06-01T00:00:00Z")
	if err := c.StartChatMigration(ctx, chatID, &later); err == nil {
		t.Fatal("startMigration with a later creation time must be refused")
	}
	back := ts("2023-01-01T00:00:00Z")
	if err := c.StartChatMigration(ctx, chatID, &back); err != nil {
		t.Fatalf("StartChatMigration: %v", err)
	}
	if err := c.StartChatMigration(ctx, chatID, nil); !graph.IsAlreadyMigrating(err) {
		t.Fatalf("second startMigration: %v", err)
	}
	chat, err := c.GetChat(ctx, chatID)
	if err != nil || chat.MigrationMode != graph.MigrationInProgress || !chat.CreatedDateTime.Equal(back) {
		t.Fatalf("GetChat = %+v, %v", chat, err)
	}

	// Import, with a response lost after the commit: the client retries,
	// Graph answers 409, and the caller finds the message it wrote.
	e.fake.Faults().MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/chats/*/messages", Action: fakegraph.ActionFailAfterCommit, Count: 1})
	first := msgAt("2023-06-01T09:00:00.500Z")
	_, err = c.ImportMessage(ctx, target, first)
	if !graph.IsConflict(err) {
		t.Fatalf("import after a lost response: %v, want a conflict", err)
	}
	noBody(t, err)
	if !strings.Contains(fail.From(err).What, "chat") {
		t.Errorf("the conflict does not say it is about a chat: %q", fail.From(err).What)
	}
	found, ok, err := c.FindMessageAt(ctx, target, first.CreatedAt)
	if err != nil || !ok || found.FromUserID != adaID || !found.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("FindMessageAt = %+v, %v, %v", found, ok, err)
	}
	byID, err := c.GetChatMessage(ctx, chatID, graph.MessageIDForTime(first.CreatedAt))
	if err != nil || byID.ID != found.ID {
		t.Fatalf("GetChatMessage = %+v, %v", byID, err)
	}

	// A millisecond a person's own message holds is a conflict as well,
	// and reading it back shows it is not ours.
	clash := msgAt("2024-02-02T10:00:00.123Z")
	if _, err := c.ImportMessage(ctx, target, clash); !graph.IsConflict(err) {
		t.Fatalf("import onto an existing message: %v", err)
	}
	found, ok, err = c.FindMessageAt(ctx, target, clash.CreatedAt)
	if err != nil || !ok || found.FromUserID != graceID {
		t.Fatalf("FindMessageAt = %+v, %v, %v; want Grace's own message", found, ok, err)
	}
	// Nothing at a free millisecond.
	if _, ok, err := c.FindMessageAt(ctx, target, ts("2023-06-01T09:00:00.501Z")); err != nil || ok {
		t.Fatalf("FindMessageAt a free millisecond = %v, %v", ok, err)
	}

	// Older than the chat: refused.
	if _, err := c.ImportMessage(ctx, target, msgAt("2022-12-31T23:59:59Z")); err == nil {
		t.Fatal("a message older than the chat must be refused")
	}

	if err := c.CompleteChatMigration(ctx, chatID); err != nil {
		t.Fatalf("CompleteChatMigration: %v", err)
	}
	if err := c.CompleteChatMigration(ctx, chatID); !graph.IsNotInMigration(err) {
		t.Fatalf("second completeMigration: %v", err)
	}
	msgs := e.fake.ChatMessages(chatID)
	if len(msgs) != 3 || msgs[0].ImportedBy == "" || msgs[1].ImportedBy != "" || msgs[2].ImportedBy != "" {
		t.Fatalf("chat messages = %+v; want one imported and the two people wrote", msgs)
	}
}

func TestFindChatMessageWithOpaqueIDs(t *testing.T) {
	e := newEnv(t, withServer(fakegraph.WithOpaqueMessageIDs(), fakegraph.WithPageSize(2)))
	ctx := context.Background()
	chat, err := e.client.CreateChat(ctx, graph.ChatSpec{Members: []string{adaID, graceID}})
	if err != nil {
		t.Fatal(err)
	}
	back := ts("2020-01-01T00:00:00Z")
	if err := e.client.StartChatMigration(ctx, chat.ID, &back); err != nil {
		t.Fatal(err)
	}
	target := graph.ImportTarget{ChatID: chat.ID}
	var want graph.ChatMessage
	for i := range 7 {
		m, err := e.client.ImportMessage(ctx, target, msgAt(ts("2021-01-01T00:00:00Z").Add(time.Duration(i)*time.Second).Format(time.RFC3339Nano)))
		if err != nil {
			t.Fatal(err)
		}
		if i == 3 {
			want = m
		}
	}
	got, ok, err := e.client.FindMessageAt(ctx, target, want.CreatedAt)
	if err != nil || !ok || got.ID != want.ID {
		t.Fatalf("FindMessageAt = %+v, %v, %v; want %s", got, ok, err, want.ID)
	}
}

func TestChatMigrationBelongsToTheAppThatStartedIt(t *testing.T) {
	seed := fakegraph.DefaultSeed()
	seed.Apps = append(seed.Apps, fakegraph.App{ClientID: "9c0f7a55-1111-4222-8333-444455556666", Secrets: []string{"other-secret"}, Roles: fakegraph.DefaultRoles()})
	e := newEnv(t, withSeed(seed))
	ctx := context.Background()
	chat, err := e.client.CreateChat(ctx, graph.ChatSpec{Members: []string{adaID, graceID}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.client.StartChatMigration(ctx, chat.ID, nil); err != nil {
		t.Fatal(err)
	}
	other := e.clientFor("9c0f7a55-1111-4222-8333-444455556666", "other-secret", nil)
	if _, err := other.ImportMessage(ctx, graph.ImportTarget{ChatID: chat.ID}, msgAt("2099-01-01T00:00:00Z")); code(err) != "graph.migration_not_owner" && code(err) != "graph.bad_request" {
		// The fake checks ownership before the timestamp.
		t.Fatalf("import by another app: %v", err)
	}
	if err := other.CompleteChatMigration(ctx, chat.ID); code(err) != "graph.migration_not_owner" {
		t.Fatalf("completeMigration by another app: %v", err)
	}
}

func TestChatStartMigrationUnavailable(t *testing.T) {
	e := newEnv(t, withServer(fakegraph.WithStartMigrationUnavailable()))
	ctx := context.Background()
	chat, err := e.client.CreateChat(ctx, graph.ChatSpec{Members: []string{adaID, graceID}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.client.StartChatMigration(ctx, chat.ID, nil); !graph.IsNotFound(err) {
		t.Fatalf("startMigration in a cloud without it: %v", err)
	}
}

// A candidate group chat is matched on its full member list, read from the
// chat itself page by page. What a listing expands is cut off, and a chat
// with more people must never pass for one with fewer.
func TestFindGroupChatsReadsEveryMember(t *testing.T) {
	e := newEnv(t, withServer(fakegraph.WithChatMemberExpandLimit(2), fakegraph.WithPageSize(2)))
	ctx := context.Background()
	three, err := e.client.CreateChat(ctx, graph.ChatSpec{Group: true, Members: []string{adaID, graceID, alanID}})
	if err != nil {
		t.Fatal(err)
	}
	four, err := e.client.CreateChat(ctx, graph.ChatSpec{Group: true, Members: []string{adaID, graceID, alanID, katherineID}})
	if err != nil {
		t.Fatal(err)
	}
	members, err := e.client.ChatMembers(ctx, four.ID)
	if err != nil || len(members) != 4 {
		t.Fatalf("ChatMembers = %+v, %v; want all four across pages", members, err)
	}
	for _, m := range members {
		if !m.IsDirectoryUser() {
			t.Errorf("member %+v is not recognised as a directory user", m)
		}
	}
	// The first two members of both chats are the same two people, which
	// is all a listing shows.
	for _, c := range []struct {
		ids  []string
		want string
	}{
		{[]string{adaID, graceID, alanID}, three.ID},
		{[]string{katherineID, alanID, graceID, adaID}, four.ID},
		{[]string{adaID, graceID}, ""},
		{[]string{adaID, graceID, katherineID}, ""},
	} {
		found, err := e.client.FindGroupChats(ctx, adaID, c.ids, time.Time{})
		switch {
		case err != nil:
			t.Fatal(err)
		case c.want == "" && len(found) != 0:
			t.Errorf("members %v matched %+v", c.ids, found)
		case c.want != "" && (len(found) != 1 || found[0].ID != c.want):
			t.Errorf("members %v matched %+v, want %s", c.ids, found, c.want)
		}
	}

	if graph.SameChatMembers([]graph.ChatMember{{Type: "#microsoft.graph.aadUserConversationMember", UserID: adaID}, {Type: "#microsoft.graph.anonymousGuestConversationMember"}}, []string{adaID}) {
		t.Error("a chat with a member who is not a directory user matched")
	}
	if graph.SameChatMembers([]graph.ChatMember{{Type: "#microsoft.graph.aadUserConversationMember", UserID: adaID}, {Type: "#microsoft.graph.aadUserConversationMember"}}, []string{adaID, ""}) {
		t.Error("a member without a user ID matched")
	}
	if !graph.SameChatMembers([]graph.ChatMember{{Type: "microsoft.graph.aadUserConversationMember", UserID: strings.ToUpper(adaID)}}, []string{adaID}) {
		t.Error("object IDs are compared without regard to case")
	}

	empty, err := e.client.ChatIsEmpty(ctx, three.ID)
	if err != nil || !empty {
		t.Fatalf("ChatIsEmpty = %v, %v", empty, err)
	}
	back := ts("2020-01-01T00:00:00Z")
	if err := e.client.StartChatMigration(ctx, three.ID, &back); err != nil {
		t.Fatal(err)
	}
	if _, err := e.client.ImportMessage(ctx, graph.ImportTarget{ChatID: three.ID}, msgAt("2021-01-01T00:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if empty, err := e.client.ChatIsEmpty(ctx, three.ID); err != nil || empty {
		t.Fatalf("ChatIsEmpty after an import = %v, %v", empty, err)
	}
}

// A refusal that belongs to one chat (a licence, a policy, its people) is
// told apart from the app lacking a permission.
func TestChatRefusals(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.fake.RefuseChatsWith(alanID, "The user does not have a Teams license.")
	_, err := e.client.CreateChat(ctx, graph.ChatSpec{Members: []string{adaID, alanID}})
	if !graph.IsChatRefused(err) || code(err) != "graph.chat_refused" {
		t.Fatalf("a chat with an unlicensed person: %v", err)
	}
	if _, err := e.client.CreateChat(ctx, graph.ChatSpec{Members: []string{adaID, edsgerID}}); !graph.IsChatRefused(err) {
		t.Fatalf("a chat with a disabled account: %v", err)
	}
	chat, err := e.client.CreateChat(ctx, graph.ChatSpec{Members: []string{adaID, graceID}})
	if err != nil {
		t.Fatal(err)
	}
	e.fake.RefuseChatMigrationWith(graceID, "Chat migration is blocked by policy for this user.")
	if err := e.client.StartChatMigration(ctx, chat.ID, nil); !graph.IsChatRefused(err) {
		t.Fatalf("startMigration refused by policy: %v", err)
	}

	// The app lacking the permission is not about the chat.
	noPerm := newEnv(t, withRoles("Teamwork.Migrate.All", "User.Read.All"))
	if _, err := noPerm.client.CreateChat(ctx, graph.ChatSpec{Members: []string{adaID, graceID}}); err == nil || graph.IsChatRefused(err) {
		t.Fatalf("a missing permission was taken for a refusal of the chat: %v", err)
	}
}

func TestChatMentionOfANonMember(t *testing.T) {
	e := newEnv(t, withServer(fakegraph.WithChatMentionsMustBeMembers()))
	ctx := context.Background()
	chat, err := e.client.CreateChat(ctx, graph.ChatSpec{Members: []string{adaID, graceID}})
	if err != nil {
		t.Fatal(err)
	}
	back := ts("2020-01-01T00:00:00Z")
	if err := e.client.StartChatMigration(ctx, chat.ID, &back); err != nil {
		t.Fatal(err)
	}
	msg := graph.ImportMessage{CreatedAt: ts("2021-01-01T00:00:00Z"), From: graph.Identity{UserID: adaID},
		HTML: `<p><at id="0">Alan</at> knows</p>`, Mentions: []graph.Mention{{ID: 0, Text: "Alan", UserID: alanID}}}
	if _, err := e.client.ImportMessage(ctx, graph.ImportTarget{ChatID: chat.ID}, msg); !graph.IsMentionInvalid(err) {
		t.Fatalf("a mention of someone outside the chat: %v", err)
	}
	msg.Mentions[0].UserID, msg.HTML = graceID, `<p><at id="0">Grace</at> knows</p>`
	if _, err := e.client.ImportMessage(ctx, graph.ImportTarget{ChatID: chat.ID}, msg); err != nil {
		t.Fatalf("a mention of a member: %v", err)
	}
}
