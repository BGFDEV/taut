package graph_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/fakegraph"
	"github.com/bgfdev/taut/internal/graph"
)

func reacted(when string, reactions ...graph.Reaction) graph.ImportMessage {
	m := msgAt(when)
	m.Reactions = reactions
	return m
}

// Reactions travel in the message's reactions collection and come back on
// every read, so a message found again after a lost response can be checked
// for them.
func TestImportReactions(t *testing.T) {
	e, target := migrating(t)
	ctx := context.Background()
	c := e.client

	want := []graph.Reaction{{Type: "👍", UserID: adaID}, {Type: "👍", UserID: graceID}, {Type: "🎉", UserID: adaID}, {Type: "1️⃣", UserID: graceID}}
	created, err := c.ImportMessage(ctx, target, reacted("2019-01-01T00:00:00Z", want...))
	if err != nil {
		t.Fatal(err)
	}
	same := func(got []graph.Reaction) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	if !same(created.Reactions) {
		t.Errorf("created: %+v", created.Reactions)
	}
	read, err := c.GetMessage(ctx, engTeam, engGen, created.ID)
	if err != nil || !same(read.Reactions) {
		t.Errorf("GetMessage: %+v, %v", read.Reactions, err)
	}
	found, ok, err := c.FindMessageAt(ctx, target, ts("2019-01-01T00:00:00Z"))
	if err != nil || !ok || !same(found.Reactions) {
		t.Errorf("FindMessageAt: %+v, %t, %v", found.Reactions, ok, err)
	}
	// Each reaction is dated like the message: an export has no other time.
	stored := e.fake.Messages(engTeam, engGen)
	if len(stored) != 1 || len(stored[0].Reactions) != len(want) {
		t.Fatalf("stored: %+v", stored)
	}
	for _, r := range stored[0].Reactions {
		if !r.CreatedAt.Equal(ts("2019-01-01T00:00:00Z")) {
			t.Errorf("reaction %s dated %s", r.Type, r.CreatedAt)
		}
	}

	// A reply carries reactions the same way.
	reply, err := c.ImportMessage(ctx, graph.ImportTarget{TeamID: engTeam, ChannelID: engGen, ReplyToID: created.ID},
		reacted("2019-01-01T00:00:01Z", graph.Reaction{Type: "👀", UserID: graceID}))
	if err != nil || len(reply.Reactions) != 1 {
		t.Errorf("reply: %+v, %v", reply.Reactions, err)
	}
	if r, err := c.GetReply(ctx, engTeam, engGen, created.ID, reply.ID); err != nil || len(r.Reactions) != 1 || r.Reactions[0] != (graph.Reaction{Type: "👀", UserID: graceID}) {
		t.Errorf("GetReply: %+v, %v", r.Reactions, err)
	}

	// Refusals are about the reactions, are not retried, and store nothing.
	over := make([]graph.Reaction, graph.MaxImportReactions+1)
	for i := range over {
		over[i] = graph.Reaction{Type: "👍", UserID: adaID}
	}
	for name, msg := range map[string]graph.ImportMessage{
		"unknown user":   reacted("2019-02-01T00:00:00Z", graph.Reaction{Type: "👍", UserID: "not-a-user"}),
		"a shortcode":    reacted("2019-02-01T00:00:00Z", graph.Reaction{Type: ":+1:", UserID: adaID}),
		"over the limit": reacted("2019-02-01T00:00:00Z", over...),
	} {
		_, err := c.ImportMessage(ctx, target, msg)
		if !graph.IsReactionInvalid(err) {
			t.Errorf("%s: got %v, want a refusal of the reactions", name, err)
			continue
		}
		fe := wantFail(t, err, "graph.reaction_invalid")
		if !strings.Contains(fe.Next, "content.reactions") {
			t.Errorf("%s: next = %q", name, fe.Next)
		}
		noBody(t, err)
	}
	for name, r := range map[string]graph.Reaction{"no type": {UserID: adaID}, "no user": {Type: "👍"}} {
		if _, err := c.ImportMessage(ctx, target, reacted("2019-02-01T00:00:00Z", r)); err == nil || fail.From(err).Code != "graph.bad_request" || graph.IsReactionInvalid(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if got := len(e.fake.Messages(engTeam, engGen)); got != 2 {
		t.Errorf("stored = %d, want 2", got)
	}
	if len(e.sleep.Waits()) != 0 {
		t.Errorf("a refusal was retried: %v", e.sleep.Waits())
	}
	// Exactly the limit is accepted.
	if m, err := c.ImportMessage(ctx, target, reacted("2019-02-01T00:00:00Z", over[1:]...)); err != nil || len(m.Reactions) != graph.MaxImportReactions {
		t.Errorf("%d reactions: %d stored, %v", graph.MaxImportReactions, len(m.Reactions), err)
	}

	// A refusal that mentions reactions on a message that carries none is
	// not this error: there is nothing to send again without.
	refusing, target := migrating(t, withServer(fakegraph.WithReactionsRefused()))
	if _, err := refusing.client.ImportMessage(ctx, target, reacted("2019-02-01T00:00:00Z", want...)); !graph.IsReactionInvalid(err) {
		t.Errorf("a tenant that refuses reactions: %v", err)
	}
	if _, err := refusing.client.ImportMessage(ctx, target, msgAt("2019-02-01T00:00:00Z")); err != nil {
		t.Errorf("the same message without reactions: %v", err)
	}
}

// A chat takes reactions like a channel does.
func TestImportReactionsIntoChat(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	chat, err := e.client.CreateChat(ctx, graph.ChatSpec{Members: []string{adaID, graceID}})
	if err != nil {
		t.Fatal(err)
	}
	back := ts("2015-01-01T00:00:00Z")
	if err := e.client.StartChatMigration(ctx, chat.ID, &back); err != nil {
		t.Fatal(err)
	}
	m, err := e.client.ImportMessage(ctx, graph.ImportTarget{ChatID: chat.ID}, reacted("2019-01-01T00:00:00Z", graph.Reaction{Type: "❤️", UserID: graceID}))
	if err != nil {
		t.Fatal(err)
	}
	read, err := e.client.GetChatMessage(ctx, chat.ID, m.ID)
	if err != nil || len(read.Reactions) != 1 || read.Reactions[0] != (graph.Reaction{Type: "❤️", UserID: graceID}) {
		t.Errorf("chat message: %+v, %v", read.Reactions, err)
	}
}
