package graph

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/bgfdev/taut/internal/fail"
)

// Chats: where direct messages and group DMs go.
//
// What this file relies on is Microsoft's documentation as read on
// 2026-11-15. None of it has been tried against a real tenant:
//
//   - POST /chats creates a chat with every participant listed as a member.
//     For a oneOnOne chat that already exists, Graph returns the existing
//     chat rather than a second one. A group chat is always new.
//   - POST /chats/{id}/startMigration and /completeMigration put a chat into
//     and out of migration mode (global cloud only; group and 1:1 chats;
//     400 when already in, or not in, migration mode).
//   - POST /chats/{id}/messages with createdDateTime and from imports a
//     message while the chat is in migration mode, under the same rules as
//     a channel: later than the chat's createdDateTime, not in the future,
//     unique to the millisecond within the chat (409 otherwise).
//
// Assumed, because the documentation does not say:
//
//   - That visibleHistoryStartDateTime is accepted on the members of a new
//     chat. CreateChat's caller falls back to creating without it.

const (
	permChatCreate = "Chat.Create"
	permChatRead   = "Chat.ReadBasic.All"
	// permChatMessageRead is what reading a chat's messages needs.
	permChatMessageRead = "Chat.Read.All"
)

// MaxChatMembers is the most people Graph allows in one chat.
const MaxChatMembers = 250

// Chat types.
const (
	ChatOneOnOne = "oneOnOne"
	ChatGroup    = "group"
)

// ChatSpec describes a chat to create.
type ChatSpec struct {
	// Group makes a group chat; otherwise the chat is oneOnOne and Members
	// must hold exactly two users.
	Group bool
	// Topic is the group chat's name. Empty leaves it unnamed.
	Topic string
	// Members are the Entra object IDs of every participant. An app cannot
	// be a participant, so all of them are people, each with the owner
	// role, which is what Graph requires of in-tenant members.
	Members []string
	// VisibleHistoryStart, for a group chat, is how far back each member
	// may see the chat's history. Whether Graph honours it at creation is
	// undocumented; see ErrChatHistoryStart.
	VisibleHistoryStart *time.Time
}

// Chat is a Teams chat.
type Chat struct {
	ID       string `json:"id"`
	ChatType string `json:"chatType"`
	Topic    string `json:"topic"`
	// CreatedDateTime is when the chat was created or, after a
	// startMigration with a backdated time, that time. Imported messages
	// must be later than it.
	CreatedDateTime time.Time `json:"createdDateTime"`
	// MigrationMode is "", MigrationInProgress or MigrationCompleted. Empty
	// also covers a service that does not report it.
	MigrationMode string `json:"migrationMode"`
	// Members is set only when the chat was listed with its members.
	Members []ChatMember `json:"members"`
}

// ChatMember is one participant of a chat.
type ChatMember struct {
	// Type is the member's @odata.type. Only
	// "#microsoft.graph.aadUserConversationMember" is a person of a
	// directory; anything else (an anonymous or federated participant) has
	// no UserID to compare.
	Type   string   `json:"@odata.type"`
	UserID string   `json:"userId"`
	Roles  []string `json:"roles"`
}

// IsDirectoryUser reports whether the member is a user of an Entra
// directory with a known object ID.
func (m ChatMember) IsDirectoryUser() bool {
	return strings.EqualFold(strings.TrimPrefix(m.Type, "#"), "microsoft.graph.aadUserConversationMember") && strings.TrimSpace(m.UserID) != ""
}

func chatPath(chatID string) string { return "/v1.0/chats/" + url.PathEscape(chatID) }

// CreateChat creates a chat, or for a oneOnOne chat returns the one the two
// users already have.
//
// Creating a oneOnOne chat is repeatable, so a lost response is retried
// here. Creating a group chat is not: every request makes another chat. It
// is sent once, and a failure that may have committed is returned for the
// caller to resolve by looking for the chat (FindGroupChats).
func (c *Client) CreateChat(ctx context.Context, spec ChatSpec) (Chat, error) {
	members := slices.Clone(spec.Members)
	slices.Sort(members)
	members = slices.Compact(members)
	switch {
	case len(members) < 2, !spec.Group && len(members) != 2:
		return Chat{}, usage("A chat needs at least two different people, and a one-on-one chat exactly two",
			fmt.Sprintf("ChatSpec.Members holds %d distinct users.", len(members)),
			"This is a bug in Taut; please report it.")
	case len(members) > MaxChatMembers:
		return Chat{}, usage("A chat cannot have this many people",
			fmt.Sprintf("ChatSpec.Members holds %d users; Teams allows %d in a chat.", len(members), MaxChatMembers),
			"This is a bug in Taut; please report it.")
	}
	people := make([]map[string]any, 0, len(members))
	for _, id := range members {
		m := map[string]any{
			"@odata.type":     "#microsoft.graph.aadUserConversationMember",
			"roles":           []string{"owner"},
			"user@odata.bind": c.bind("users", id),
		}
		if spec.Group && spec.VisibleHistoryStart != nil {
			m["visibleHistoryStartDateTime"] = creationTime(*spec.VisibleHistoryStart)
		}
		people = append(people, m)
	}
	body := map[string]any{"chatType": ChatOneOnOne, "members": people}
	if spec.Group {
		body["chatType"] = ChatGroup
		if t := strings.TrimSpace(spec.Topic); t != "" {
			body["topic"] = t
		}
	}
	classify := func(e *APIError) *fail.Error {
		msg := strings.ToLower(e.Message)
		switch {
		case chatRefused(e):
			return refusedChat(e)
		case e.Status == http.StatusBadRequest && strings.Contains(msg, "visiblehistorystartdatetime"):
			e.kind = ErrChatHistoryStart
			return fail.Wrap(e, "graph.chat_history_start",
				"Microsoft Teams did not accept a history start time on the members of a new chat",
				e.detail()+e.ref(),
				"Nothing to do: Taut creates the chat without it. Members of the group chat may not see history older than the day the chat was created.")
		case e.Status == http.StatusBadRequest || e.Status == http.StatusNotFound:
			e.kind = ErrChatRefused
			return fail.Wrap(e, "graph.chat_member_invalid",
				"Microsoft Teams did not accept the people of a chat",
				fmt.Sprintf("The chat has %d people. %s", len(members), e.detail()+e.ref()),
				"Every participant must be an enabled user of the tenant with a Teams licence. Check the identity mapping with `taut map`, then run `taut migrate` again.")
		}
		return nil
	}
	var chat Chat
	err := c.do(ctx, request{
		method: "POST", path: "/v1.0/chats", body: body, permission: permChatCreate,
		retryAmbiguous: !spec.Group, classify: classify,
	}, &chat)
	return chat, err
}

// GetChat returns a chat.
func (c *Client) GetChat(ctx context.Context, chatID string) (Chat, error) {
	var chat Chat
	return chat, c.do(ctx, request{method: "GET", path: chatPath(chatID), permission: permChatRead}, &chat)
}

// chatRefused reports whether Graph refused something about one chat for a
// reason that belongs to the chat or its people (a participant without a
// Teams licence, a policy that forbids the chat), as opposed to the app
// lacking a permission, which is the same for every chat.
func chatRefused(e *APIError) bool {
	if e.Status != http.StatusForbidden && e.Status != http.StatusBadRequest {
		return false
	}
	msg := strings.ToLower(e.Message)
	return strings.Contains(msg, "licen") || strings.Contains(msg, "policy") || strings.Contains(msg, "not enabled for")
}

func refusedChat(e *APIError) *fail.Error {
	e.kind = ErrChatRefused
	return fail.Wrap(e, "graph.chat_refused",
		"Microsoft Teams refused this chat",
		e.detail()+e.ref(),
		"A participant has no Teams licence, or a policy of the tenant does not allow the chat. Taut leaves the conversation out and carries on; fix the cause and run a new migration to bring it across.")
}

// ChatMembers returns every member of a chat, following every page. It is
// the only reliable member list: the members expanded into a listing of
// chats are cut off after a number Graph chooses.
func (c *Client) ChatMembers(ctx context.Context, chatID string) ([]ChatMember, error) {
	var out []ChatMember
	for m, err := range list[ChatMember](ctx, c, request{method: "GET", path: chatPath(chatID) + "/members", permission: permChatRead}) {
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// SameChatMembers reports whether a chat's members are exactly the given
// users: nobody missing, nobody more, and every member a user of the
// directory. A member of any other kind makes the answer no.
func SameChatMembers(members []ChatMember, userIDs []string) bool {
	want := make([]string, 0, len(userIDs))
	for _, id := range userIDs {
		want = append(want, strings.ToLower(strings.TrimSpace(id)))
	}
	slices.Sort(want)
	want = slices.Compact(want)
	have := make([]string, 0, len(members))
	for _, m := range members {
		if !m.IsDirectoryUser() {
			return false
		}
		have = append(have, strings.ToLower(strings.TrimSpace(m.UserID)))
	}
	slices.Sort(have)
	return len(have) == len(slices.Compact(slices.Clone(have))) && slices.Equal(have, want)
}

// ChatIsEmpty reports whether a chat holds no message at all.
func (c *Client) ChatIsEmpty(ctx context.Context, chatID string) (bool, error) {
	for _, err := range list[ChatMessage](ctx, c, request{
		method: "GET", path: chatPath(chatID) + "/messages", query: url.Values{"$top": {"1"}}, permission: permChatMessageRead,
	}) {
		if err != nil {
			return false, err
		}
		return false, nil
	}
	return true, nil
}

// FindGroupChats returns the group chats of userID whose members are
// exactly memberIDs and that were created at or after since, oldest first.
// It is how a group chat whose creation was interrupted is found again: an
// app cannot list every chat of the tenant, but it can list one user's
// (Chat.ReadBasic.All).
//
// The listing only supplies candidates. Each one's members are read from
// the chat itself, every page of them: the members a listing can expand
// are cut off, so a chat with more people than the cut would otherwise
// look like one with fewer.
func (c *Client) FindGroupChats(ctx context.Context, userID string, memberIDs []string, since time.Time) ([]Chat, error) {
	var out []Chat
	for chat, err := range list[Chat](ctx, c, request{
		method: "GET", path: "/v1.0/users/" + url.PathEscape(userID) + "/chats", permission: permChatRead,
		query: url.Values{"$filter": {FilterEq("chatType", ChatGroup)}, "$top": {"50"}},
	}) {
		if err != nil {
			return nil, err
		}
		if chat.ChatType != ChatGroup || chat.CreatedDateTime.Before(since) {
			continue
		}
		members, err := c.ChatMembers(ctx, chat.ID)
		if IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if SameChatMembers(members, memberIDs) {
			chat.Members = members
			out = append(out, chat)
		}
	}
	slices.SortFunc(out, func(a, b Chat) int {
		if n := a.CreatedDateTime.Compare(b.CreatedDateTime); n != 0 {
			return n
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

// StartChatMigration puts a chat into migration mode. With
// conversationCreation, the chat's creation time is moved back to it; Graph
// requires that to be older than the chat's current creation time. Without,
// the creation time is left as it is.
func (c *Client) StartChatMigration(ctx context.Context, chatID string, conversationCreation *time.Time) error {
	var body any
	if conversationCreation != nil {
		body = map[string]string{"conversationCreationDateTime": creationTime(*conversationCreation)}
	}
	classify := func(e *APIError) *fail.Error {
		switch {
		case e.Status == http.StatusBadRequest && strings.Contains(strings.ToLower(e.Message), "already in migration"):
			e.kind = ErrAlreadyMigrating
			return fail.Wrap(e, "graph.already_migrating",
				"The chat is already in migration mode",
				e.detail()+e.ref(),
				"Nothing to do if Taut started it: run `taut migrate` to continue importing. Otherwise finish the other migration first.")
		case chatRefused(e):
			return refusedChat(e)
		case e.Status == http.StatusBadRequest:
			// Any other 400 is about this chat too: it is of a kind that
			// cannot be migrated, or the time asked for is not acceptable.
			e.kind = ErrChatRefused
			return fail.Wrap(e, "graph.chat_refused",
				"Microsoft Teams refused to put this chat into migration mode",
				e.detail()+e.ref(),
				"Taut leaves the conversation out and carries on.")
		}
		return nil
	}
	return c.do(ctx, request{
		method: "POST", path: chatPath(chatID) + "/startMigration", body: body,
		permission: permMigrate, retryAmbiguous: true, classify: classify,
	}, nil)
}

// CompleteChatMigration takes a chat out of migration mode.
func (c *Client) CompleteChatMigration(ctx context.Context, chatID string) error {
	classify := func(e *APIError) *fail.Error {
		switch {
		case isNotOwner(e):
			return notOwner(e)
		case e.Status == http.StatusBadRequest && strings.Contains(strings.ToLower(e.Message), "not in migration"):
			e.kind = ErrNotInMigration
			return fail.Wrap(e, "graph.not_in_migration",
				"The chat is not in migration mode",
				e.detail()+e.ref(),
				"Nothing to complete. If the migration was already completed, continue with `taut finalize`.")
		}
		return nil
	}
	return c.do(ctx, request{
		method: "POST", path: chatPath(chatID) + "/completeMigration",
		permission: permMigrate, retryAmbiguous: true, classify: classify,
	}, nil)
}

// GetChatMessage returns one message of a chat.
func (c *Client) GetChatMessage(ctx context.Context, chatID, messageID string) (ChatMessage, error) {
	var m ChatMessage
	return m, c.do(ctx, request{
		method: "GET", path: chatPath(chatID) + "/messages/" + url.PathEscape(messageID), permission: permChatMessageRead,
	}, &m)
}

// ChatMessages lists the messages of a chat, newest first. With around,
// the listing starts at the messages created up to it: Graph filters chat
// messages on createdDateTime with lt only, so the caller stops reading once
// the messages are older than the one it wants.
func (c *Client) ChatMessages(ctx context.Context, chatID string, around *time.Time) iter.Seq2[ChatMessage, error] {
	query := url.Values{"$top": {"50"}}
	if around != nil {
		at := around.UTC().Truncate(time.Millisecond)
		query.Set("$orderby", "createdDateTime desc")
		query.Set("$filter", "createdDateTime lt "+MessageTime(at.Add(time.Millisecond)))
	}
	return list[ChatMessage](ctx, c, request{method: "GET", path: chatPath(chatID) + "/messages", query: query, permission: permChatMessageRead})
}

// findChatMessageAt is FindMessageAt for a chat: by ID, then by a filtered
// listing, then (if the service refuses the filter) by reading the chat.
func (c *Client) findChatMessageAt(ctx context.Context, chatID string, want time.Time) (ChatMessage, bool, error) {
	m, err := c.GetChatMessage(ctx, chatID, MessageIDForTime(want))
	switch {
	case err == nil && m.CreatedAt.Truncate(time.Millisecond).Equal(want):
		return m, true, nil
	case err != nil && !IsNotFound(err):
		return ChatMessage{}, false, err
	}
	for _, around := range []*time.Time{&want, nil} {
		refused := false
		for m, err := range c.ChatMessages(ctx, chatID, around) {
			var api *APIError
			if err != nil && around != nil && errors.As(err, &api) && api.Status == http.StatusBadRequest {
				refused = true
				break
			}
			if err != nil {
				return ChatMessage{}, false, err
			}
			if m.CreatedAt.Truncate(time.Millisecond).Equal(want) {
				return m, true, nil
			}
			if m.CreatedAt.Before(want) {
				break
			}
		}
		if !refused {
			break
		}
	}
	return ChatMessage{}, false, nil
}
