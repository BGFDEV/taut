package fakegraph

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Chats: one-on-one and group chats, their migration mode and message
// import.
//
// Modelled on Microsoft's documentation as read on 2026-11-15:
//
//   - POST /chats creates a chat from its full member list. A oneOnOne chat
//     between two people who already have one is returned as it is, with
//     its original createdDateTime; a group chat is always new.
//   - startMigration takes an optional conversationCreationDateTime that
//     must be older than the chat's createdDateTime; a chat already in
//     migration mode answers 400. completeMigration on a chat that is not
//     answers 400. Only the app that started may import and complete.
//   - A message is imported with createdDateTime and from, under the rules
//     of a channel: later than the chat's createdDateTime, not in the
//     future, unique to the millisecond (409 otherwise). There are no
//     replies.
//
// Where the documentation is silent, the fake picks the behaviour and has
// an option for the opposite: whether members may carry
// visibleHistoryStartDateTime at creation (accepted;
// WithChatHistoryStartRefused), and whether the author of an imported
// message must be a member of the chat (no; WithChatAuthorsMustBeMembers).
// It also reports migrationMode on the chat resource, which the
// documentation does not promise.

// MaxChatMembers is the most people a chat may have.
const MaxChatMembers = 250

// WithChatHistoryStartRefused makes POST /chats refuse members that carry
// visibleHistoryStartDateTime, the behaviour opposite to the default.
func WithChatHistoryStartRefused() Option {
	return func(c *config) { c.chatHistoryRefused = true }
}

// WithChatAuthorsMustBeMembers makes a chat refuse an imported message
// whose author is not one of its members.
func WithChatAuthorsMustBeMembers() Option {
	return func(c *config) { c.chatAuthorsMembers = true }
}

// LoseGroupChatResponses makes the next n creations of a group chat succeed
// without the caller learning of it: the chat exists, the connection is cut
// before the answer. It is the crash a path-based fault cannot aim at,
// since one-on-one and group chats are created by the same request path.
func (s *Server) LoseGroupChatResponses(n int) {
	s.mu.Lock()
	s.st.loseGroupChats = n
	s.mu.Unlock()
}

// WithChatBackdatingIgnored makes a chat's startMigration accept a
// conversationCreationDateTime and leave the chat's creation time as it
// was: a service that does not do what its documentation says. Messages
// older than the chat are then refused.
func WithChatBackdatingIgnored() Option {
	return func(c *config) { c.chatBackdateIgnored = true }
}

// DefaultChatMemberExpandLimit is how many members a listing of chats
// expands per chat. Graph cuts the expanded list off; the full list is
// only at /chats/{id}/members.
const DefaultChatMemberExpandLimit = 25

// WithChatMemberExpandLimit sets how many members $expand=members returns
// per chat when chats are listed.
func WithChatMemberExpandLimit(n int) Option {
	return func(c *config) { c.chatExpandLimit = n }
}

// WithChatMentionsMustBeMembers makes a chat refuse an imported message
// that mentions someone who is not one of its members.
func WithChatMentionsMustBeMembers() Option {
	return func(c *config) { c.chatMentionsMembers = true }
}

// RefuseChatsWith makes creating any chat that includes the user fail with
// 403 and the message: a person without a Teams licence, say.
func (s *Server) RefuseChatsWith(userID, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.chatRefusals == nil {
		s.st.chatRefusals = map[string]string{}
	}
	s.st.chatRefusals[strings.ToLower(userID)] = message
}

// RefuseChatMigrationWith makes startMigration on any chat that includes
// the user fail with 403 and the message.
func (s *Server) RefuseChatMigrationWith(userID, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.chatStartRefusals == nil {
		s.st.chatStartRefusals = map[string]string{}
	}
	s.st.chatStartRefusals[strings.ToLower(userID)] = message
}

// Chat is a chat that exists before the test starts: one people already
// use.
type Chat struct {
	// ID is optional; one is generated when empty.
	ID string `json:"id,omitempty"`
	// ChatType is "oneOnOne" or "group".
	ChatType string `json:"chatType"`
	Topic    string `json:"topic,omitempty"`
	// Members are user IDs or user principal names.
	Members         []string      `json:"members"`
	CreatedDateTime *time.Time    `json:"createdDateTime,omitempty"`
	Messages        []ChatMessage `json:"messages,omitempty"`
}

// ChatMessage is a message people wrote in a seeded chat.
type ChatMessage struct {
	CreatedDateTime time.Time `json:"createdDateTime"`
	// From is a user ID or user principal name.
	From    string `json:"from"`
	Content string `json:"content"`
}

type chatState struct {
	id        string
	chatType  string
	topic     string
	createdBy string
	members   []*chatMember
	// conv holds what a chat shares with a channel: creation time,
	// migration mode and messages.
	conv *channelState
}

type chatMember struct {
	id                  string
	userID              string
	roles               []string
	visibleHistoryStart *time.Time
}

const (
	chatOneOnOne = "oneOnOne"
	chatGroup    = "group"
)

var (
	rolesChatCreate   = []string{"Chat.Create", "Chat.ReadWrite.All"}
	rolesChatRead     = []string{"Chat.ReadBasic.All", "Chat.Read.All", "Chat.ReadWrite.All"}
	rolesChatMessages = []string{"Chat.Read.All", "Chat.ReadWrite.All"}
)

func (s *Server) chatRoutes(g *http.ServeMux) {
	g.HandleFunc("POST /v1.0/chats", s.authorized(rolesChatCreate, s.createChat))
	g.HandleFunc("GET /v1.0/chats/{id}", s.authorized(rolesChatRead, s.getChat))
	g.HandleFunc("GET /v1.0/chats/{id}/members", s.authorized(rolesChatRead, s.listChatMembers))
	g.HandleFunc("GET /v1.0/users/{key}/chats", s.authorized(rolesChatRead, s.listUserChats))
	g.HandleFunc("POST /v1.0/chats/{id}/startMigration", s.authorized(rolesMigrate, s.startChatMigration))
	g.HandleFunc("POST /v1.0/chats/{id}/completeMigration", s.authorized(rolesMigrate, s.completeChatMigration))
	g.HandleFunc("POST /v1.0/chats/{id}/messages", s.authorized(rolesMigrate, s.postChatMessage))
	g.HandleFunc("GET /v1.0/chats/{id}/messages", s.authorized(rolesChatMessages, s.listChatMessages))
	g.HandleFunc("GET /v1.0/chats/{id}/messages/{mid}", s.authorized(rolesChatMessages, s.getChatMessage))
}

func chatKey(chatID string) string { return "chat/" + chatID }

// findChat must be called with s.mu held.
func (s *Server) findChat(id string) *chatState {
	for _, c := range s.st.chats {
		if c.id == id {
			return c
		}
	}
	return nil
}

func (s *Server) locateChat(id string) (*chatState, *gErr) {
	if c := s.findChat(id); c != nil {
		return c, nil
	}
	return nil, notFound("No chat found with id %s.", id)
}

// findUserByKey finds a user by ID or user principal name.
func (s *Server) findUserByKey(key string) *User {
	for _, u := range s.st.users {
		if strings.EqualFold(u.ID, key) || strings.EqualFold(u.UserPrincipalName, key) {
			return u
		}
	}
	return nil
}

func chatMemberIDs(c *chatState) []string {
	ids := make([]string, 0, len(c.members))
	for _, m := range c.members {
		ids = append(ids, strings.ToLower(m.userID))
	}
	slices.Sort(ids)
	return ids
}

// oneOnOne returns the existing one-on-one chat of two users, if any.
func (st *state) oneOnOne(a, b string) *chatState {
	want := []string{strings.ToLower(a), strings.ToLower(b)}
	slices.Sort(want)
	for _, c := range st.chats {
		if c.chatType == chatOneOnOne && slices.Equal(chatMemberIDs(c), want) {
			return c
		}
	}
	return nil
}

// addSeedChat adds a chat from a seed. It reports problems as an error.
func (st *state) addSeedChat(in Chat, nextID func(string) string) (string, error) {
	if in.ChatType != chatOneOnOne && in.ChatType != chatGroup {
		return "", fmt.Errorf("chatType must be %q or %q", chatOneOnOne, chatGroup)
	}
	created := SeedChannelCreated
	if in.CreatedDateTime != nil {
		created = in.CreatedDateTime.UTC().Truncate(time.Millisecond)
	}
	c := &chatState{id: in.ID, chatType: in.ChatType, topic: in.Topic, conv: newChannelState(Channel{}, created)}
	if c.id == "" {
		c.id = nextID(chatIDFormat(in.ChatType))
	}
	find := func(key string) *User {
		for _, u := range st.users {
			if strings.EqualFold(u.ID, key) || strings.EqualFold(u.UserPrincipalName, key) {
				return u
			}
		}
		return nil
	}
	seen := map[string]bool{}
	for _, key := range in.Members {
		u := find(key)
		if u == nil {
			return "", fmt.Errorf("member %q is not a user of the tenant", key)
		}
		if seen[u.ID] {
			continue
		}
		seen[u.ID] = true
		c.members = append(c.members, &chatMember{id: nextID("chatmember-%06d"), userID: u.ID, roles: []string{"owner"}})
	}
	if len(c.members) < 2 || (c.chatType == chatOneOnOne && len(c.members) != 2) {
		return "", fmt.Errorf("a %s chat cannot have %d members", c.chatType, len(c.members))
	}
	for _, other := range st.chats {
		if other.id == c.id {
			return "", fmt.Errorf("duplicate chat id %s", c.id)
		}
	}
	if c.chatType == chatOneOnOne && st.oneOnOne(c.members[0].userID, c.members[1].userID) != nil {
		return "", fmt.Errorf("the two members already have a one-on-one chat")
	}
	for i, m := range in.Messages {
		u := find(m.From)
		at := m.CreatedDateTime.UTC().Truncate(time.Millisecond)
		switch {
		case u == nil:
			return "", fmt.Errorf("messages[%d]: %q is not a user of the tenant", i, m.From)
		case !at.After(created):
			return "", fmt.Errorf("messages[%d] is not later than the chat's createdDateTime", i)
		case c.conv.byMillis[at.UnixMilli()] != nil:
			return "", fmt.Errorf("messages[%d] shares its millisecond with another message", i)
		}
		ms := &messageState{
			id: strconv.FormatInt(at.UnixMilli(), 10), createdAt: at, fromUserID: u.ID, fromName: u.DisplayName,
			contentType: "html", content: m.Content, mentions: []MentionView{}, attachments: []AttachmentView{},
		}
		c.conv.messages[ms.id] = ms
		c.conv.byMillis[at.UnixMilli()] = ms
	}
	st.chats = append(st.chats, c)
	return c.id, nil
}

func chatIDFormat(chatType string) string {
	if chatType == chatOneOnOne {
		return "19:%032x@unq.gbl.spaces"
	}
	return "19:%032x@thread.v2"
}

// AddChat adds a chat that people already use, as if it had always been
// there, and returns its ID. It panics on an invalid chat, like New does
// on an invalid seed.
func (s *Server) AddChat(c Chat) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := s.st.addSeedChat(c, s.nextID)
	if err != nil {
		panic("fakegraph: invalid chat: " + err.Error())
	}
	return id
}

type chatMemberInput struct {
	Type           string   `json:"@odata.type"`
	Roles          []string `json:"roles"`
	Bind           string   `json:"user@odata.bind"`
	VisibleHistory *string  `json:"visibleHistoryStartDateTime"`
}

func (s *Server) createChat(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ChatType string            `json:"chatType"`
		Topic    *string           `json:"topic"`
		Members  []chatMemberInput `json:"members"`
	}
	if e := readJSON(w, r, &in, false); e != nil {
		s.fail(w, e)
		return
	}
	now := s.cfg.now().UTC().Truncate(time.Millisecond)
	app := record(r).AppID
	lost := false
	out, e := func() (map[string]any, *gErr) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case in.ChatType != chatOneOnOne && in.ChatType != chatGroup:
			return nil, badRequest("Invalid chatType specified. Must be 'oneOnOne' or 'group'.")
		case in.ChatType == chatOneOnOne && in.Topic != nil:
			return nil, badRequest("A topic can only be set on a group chat.")
		case in.Topic != nil && utf8.RuneCountInString(*in.Topic) > 250:
			return nil, badRequest("The chat topic must not exceed 250 characters.")
		case len(in.Members) > MaxChatMembers:
			return nil, badRequest("A chat cannot have more than %d members; %d were sent.", MaxChatMembers, len(in.Members))
		}
		var members []*chatMember
		seen := map[string]bool{}
		for i, m := range in.Members {
			if !isMemberType(m.Type) {
				return nil, badRequest("members[%d] must be of type '#microsoft.graph.aadUserConversationMember'.", i)
			}
			u := s.findUser(boundUserID(m.Bind))
			if u == nil || !u.AccountEnabled {
				return nil, badRequest("members[%d]: the user referenced by 'user@odata.bind' was not found in the tenant, or the account is disabled.", i)
			}
			wantRole := "owner"
			if strings.EqualFold(u.UserType, "Guest") {
				wantRole = "guest"
			}
			if len(m.Roles) != 1 || m.Roles[0] != wantRole {
				return nil, badRequest("members[%d]: roles must be ['%s'] for this user.", i, wantRole)
			}
			if seen[u.ID] {
				return nil, badRequest("members[%d]: the user is listed more than once.", i)
			}
			if why, refused := s.st.chatRefusals[strings.ToLower(u.ID)]; refused {
				return nil, forbidden("%s", why)
			}
			seen[u.ID] = true
			cm := &chatMember{userID: u.ID, roles: []string{wantRole}}
			if m.VisibleHistory != nil {
				if s.cfg.chatHistoryRefused || in.ChatType != chatGroup {
					return nil, badRequest("members[%d]: visibleHistoryStartDateTime cannot be set when creating a chat.", i)
				}
				t, err := time.Parse(time.RFC3339Nano, *m.VisibleHistory)
				if err != nil {
					return nil, badRequest("members[%d]: invalid visibleHistoryStartDateTime.", i)
				}
				t = t.UTC()
				cm.visibleHistoryStart = &t
			}
			members = append(members, cm)
		}
		switch {
		case in.ChatType == chatOneOnOne && len(members) != 2:
			return nil, badRequest("A oneOnOne chat must have exactly two members.")
		case len(members) < 2:
			return nil, badRequest("A chat must have at least two members.")
		}
		if in.ChatType == chatOneOnOne {
			if existing := s.st.oneOnOne(members[0].userID, members[1].userID); existing != nil {
				return s.chatEntity(r, existing, false), nil
			}
		}
		c := &chatState{
			id: s.nextID(chatIDFormat(in.ChatType)), chatType: in.ChatType, createdBy: app,
			members: members, conv: newChannelState(Channel{}, now),
		}
		if in.Topic != nil {
			c.topic = *in.Topic
		}
		for _, m := range c.members {
			m.id = s.nextID("chatmember-%06d")
		}
		s.st.chats = append(s.st.chats, c)
		if in.ChatType == chatGroup && s.st.loseGroupChats > 0 {
			s.st.loseGroupChats--
			lost = true
		}
		return s.chatEntity(r, c, false), nil
	}()
	if e != nil {
		s.fail(w, e)
		return
	}
	if lost {
		record(r).Fault = "lost-group-chat-response"
		panic(http.ErrAbortHandler)
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) chatEntity(r *http.Request, c *chatState, withMembers bool) map[string]any {
	var topic, mode any
	if c.topic != "" {
		topic = c.topic
	}
	if c.conv.migrationMode != "" {
		mode = c.conv.migrationMode
	}
	out := map[string]any{
		"@odata.context": "https://" + r.Host + "/v1.0/$metadata#chats/$entity",
		"id":             c.id, "chatType": c.chatType, "topic": topic,
		"createdDateTime": graphTime(c.conv.createdAt), "lastUpdatedDateTime": graphTime(c.conv.createdAt),
		"tenantId": s.st.tenantID, "migrationMode": mode,
	}
	if withMembers {
		out["members"] = s.chatMemberEntities(c)
	}
	return out
}

func (s *Server) chatMemberEntities(c *chatState) []map[string]any {
	members := make([]map[string]any, 0, len(c.members))
	for _, m := range c.members {
		var visible any
		if m.visibleHistoryStart != nil {
			visible = graphTime(*m.visibleHistoryStart)
		}
		members = append(members, map[string]any{
			"@odata.type": "#microsoft.graph.aadUserConversationMember",
			"id":          m.id, "userId": m.userID, "roles": append([]string{}, m.roles...),
			"visibleHistoryStartDateTime": visible, "tenantId": s.st.tenantID,
		})
	}
	return members
}

// listChatMembers is the full member list of a chat, paged.
func (s *Server) listChatMembers(w http.ResponseWriter, r *http.Request) {
	q, ok := s.checkQuery(w, r, "$top", "$skiptoken")
	if !ok {
		return
	}
	chatID := r.PathValue("id")
	s.mu.RLock()
	c, e := s.locateChat(chatID)
	var items []map[string]any
	if e == nil {
		items = s.chatMemberEntities(c)
	}
	s.mu.RUnlock()
	if e != nil {
		s.fail(w, e)
		return
	}
	start, end, next, err := s.window(r, q, len(items))
	if err != nil {
		s.graphError(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	s.writeCollection(w, r, "chats('"+chatID+"')/members", items[start:end], next)
}

func (s *Server) getChat(w http.ResponseWriter, r *http.Request) {
	q, ok := s.checkQuery(w, r, "$expand")
	if !ok {
		return
	}
	if exp := q.Get("$expand"); exp != "" && exp != "members" {
		s.graphError(w, http.StatusBadRequest, "BadRequest", "Only $expand=members is supported on a chat.")
		return
	}
	s.mu.RLock()
	c, e := s.locateChat(r.PathValue("id"))
	var out map[string]any
	if e == nil {
		out = s.chatEntity(r, c, q.Get("$expand") == "members")
	}
	s.mu.RUnlock()
	if e != nil {
		s.fail(w, e)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

var chatTypeFilterRE = regexp.MustCompile(`^\s*chatType\s+eq\s+'(oneOnOne|group|meeting)'\s*$`)

func (s *Server) listUserChats(w http.ResponseWriter, r *http.Request) {
	q, ok := s.checkQuery(w, r, "$expand", "$filter", "$top", "$skiptoken")
	if !ok {
		return
	}
	if exp := q.Get("$expand"); exp != "" && exp != "members" {
		s.graphError(w, http.StatusBadRequest, "BadRequest", "Only $expand=members is supported when listing chats.")
		return
	}
	chatType := ""
	if f := q.Get("$filter"); f != "" {
		m := chatTypeFilterRE.FindStringSubmatch(f)
		if m == nil {
			s.graphError(w, http.StatusBadRequest, "BadRequest", "fakegraph supports only $filter=chatType eq '…' when listing chats.")
			return
		}
		chatType = m[1]
	}
	key := r.PathValue("key")
	s.mu.RLock()
	u := s.findUserByKey(key)
	var items []map[string]any
	if u != nil {
		var matched []*chatState
		for _, c := range s.st.chats {
			if chatType != "" && c.chatType != chatType {
				continue
			}
			if slices.ContainsFunc(c.members, func(m *chatMember) bool { return m.userID == u.ID }) {
				matched = append(matched, c)
			}
		}
		// Newest first, as Graph lists them.
		slices.SortStableFunc(matched, func(a, b *chatState) int { return b.conv.createdAt.Compare(a.conv.createdAt) })
		items = make([]map[string]any, 0, len(matched))
		for _, c := range matched {
			e := s.chatEntity(r, c, q.Get("$expand") == "members")
			// A listing expands only the first few members of each chat.
			if ms, ok := e["members"].([]map[string]any); ok && len(ms) > s.cfg.chatExpandLimit {
				e["members"] = ms[:s.cfg.chatExpandLimit]
			}
			delete(e, "@odata.context")
			items = append(items, e)
		}
	}
	s.mu.RUnlock()
	if u == nil {
		s.graphError(w, http.StatusNotFound, "NotFound", fmt.Sprintf("No user %s found in the tenant.", key))
		return
	}
	start, end, next, err := s.windowN(r, q, len(items), min(s.cfg.pageSize, 20), 50)
	if err != nil {
		s.graphError(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	s.writeCollection(w, r, "users('"+u.ID+"')/chats", items[start:end], next)
}

func (s *Server) startChatMigration(w http.ResponseWriter, r *http.Request) {
	if s.cfg.noStartMigration {
		s.graphError(w, http.StatusNotFound, "ResourceNotFound", "Resource not found for the segment 'startMigration'. This API is not available in this cloud.")
		return
	}
	var in struct {
		Created *string `json:"conversationCreationDateTime"`
	}
	if e := readJSON(w, r, &in, true); e != nil {
		s.fail(w, e)
		return
	}
	app := record(r).AppID
	e := func() *gErr {
		s.mu.Lock()
		defer s.mu.Unlock()
		c, e := s.locateChat(r.PathValue("id"))
		if e != nil {
			return e
		}
		conv := c.conv
		if conv.migrationMode == migrationInProgress {
			return badRequest("The chat is already in migration mode.")
		}
		for _, m := range c.members {
			if why, refused := s.st.chatStartRefusals[strings.ToLower(m.userID)]; refused {
				return forbidden("%s", why)
			}
		}
		created := conv.createdAt
		if in.Created != nil {
			t, err := time.Parse(time.RFC3339Nano, *in.Created)
			if err != nil {
				return badRequest("Invalid conversationCreationDateTime: an ISO 8601 UTC timestamp is expected.")
			}
			t = t.UTC().Truncate(time.Millisecond)
			if !t.Before(conv.createdAt) {
				return badRequest("conversationCreationDateTime must be older than the chat's current createdDateTime (%s).", graphTime(conv.createdAt))
			}
			if !s.cfg.chatBackdateIgnored {
				created = t
			}
		}
		if conv.originalCreatedAt == nil {
			original := conv.createdAt
			conv.originalCreatedAt = &original
		}
		conv.createdAt = created
		conv.migrationMode, conv.migrationOwner = migrationInProgress, app
		return nil
	}()
	if e != nil {
		s.fail(w, e)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) completeChatMigration(w http.ResponseWriter, r *http.Request) {
	app := record(r).AppID
	e := func() *gErr {
		s.mu.Lock()
		defer s.mu.Unlock()
		c, e := s.locateChat(r.PathValue("id"))
		switch {
		case e != nil:
			return e
		case c.conv.migrationMode != migrationInProgress:
			return badRequest("The chat is not in migration mode.")
		case c.conv.migrationOwner != app:
			return forbidden("%s", notOwnerMessage)
		}
		c.conv.migrationMode, c.conv.migrationOwner = migrationCompleted, ""
		return nil
	}()
	if e != nil {
		s.fail(w, e)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) chatMessageEntity(r *http.Request, c *chatState, m *messageState, single bool) map[string]any {
	out := s.messageEntity("", c.conv, m)
	out["chatId"] = c.id
	out["channelIdentity"] = nil
	if single {
		out["@odata.context"] = "https://" + r.Host + "/v1.0/$metadata#chats('" + c.id + "')/messages/$entity"
	}
	return out
}

func (s *Server) postChatMessage(w http.ResponseWriter, r *http.Request) {
	chatID := r.PathValue("id")
	s.mu.RLock()
	_, e := s.locateChat(chatID)
	s.mu.RUnlock()
	if e != nil {
		s.fail(w, e)
		return
	}
	if ok, wait := s.importAllowed(chatKey(chatID), true, s.tenantID(), record(r).AppID); !ok {
		s.throttled(w, wait)
		return
	}
	var in messageInput
	if e := readJSON(w, r, &in, false); e != nil {
		s.fail(w, e)
		return
	}
	if in.CreatedDateTime == nil && in.From == nil {
		s.fail(w, forbidden("Sending a message with application permissions is not supported; "+
			"applications can only import messages (createdDateTime and from set) into a chat in migration mode."))
		return
	}
	now := s.cfg.now()
	app := record(r).AppID
	out, e := func() (map[string]any, *gErr) {
		s.mu.Lock()
		defer s.mu.Unlock()
		c, e := s.locateChat(chatID)
		if e != nil {
			return nil, e
		}
		conv := c.conv
		if conv.migrationMode != migrationInProgress {
			return nil, forbidden("Importing messages is only possible while the chat is in migration mode.")
		}
		if conv.migrationOwner != app {
			return nil, forbidden("%s", notOwnerMessage)
		}
		// Where a chat's files should live is undocumented, so nothing may
		// rely on attaching one: the fake refuses, to catch a caller that tries.
		if len(in.Attachments) > 0 {
			return nil, badRequest("Attachment references are not supported when importing into a chat.")
		}
		m, e := s.validateImport(&in, conv, now)
		if e != nil {
			return nil, e
		}
		if s.cfg.chatAuthorsMembers && !slices.ContainsFunc(c.members, func(cm *chatMember) bool { return cm.userID == m.fromUserID }) {
			return nil, badRequest("The 'from' user (from.user.id) is not a member of the chat.")
		}
		if s.cfg.chatMentionsMembers {
			for _, mn := range m.mentions {
				if !slices.ContainsFunc(c.members, func(cm *chatMember) bool { return strings.EqualFold(cm.userID, mn.UserID) }) {
					return nil, badRequest("Mention id %d names a user who is not a member of the chat.", mn.ID)
				}
			}
		}
		if conv.byMillis[m.createdAt.UnixMilli()] != nil {
			s.conflicts.Add(1)
			return nil, &gErr{http.StatusConflict, "Conflict",
				"A message with the same createdDateTime (" + graphTime(m.createdAt) + ") already exists in this chat."}
		}
		m.id = strconv.FormatInt(m.createdAt.UnixMilli(), 10)
		if s.cfg.opaqueMessageIDs {
			m.id = s.nextID("1%018d")
		}
		m.importedBy = app
		conv.messages[m.id] = m
		conv.byMillis[m.createdAt.UnixMilli()] = m
		s.imports.Add(1)
		return s.chatMessageEntity(r, c, m, true), nil
	}()
	if e != nil {
		s.fail(w, e)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) getChatMessage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.checkQuery(w, r); !ok {
		return
	}
	chatID, id := r.PathValue("id"), r.PathValue("mid")
	s.mu.RLock()
	c, e := s.locateChat(chatID)
	var out map[string]any
	if e == nil {
		if m := c.conv.messages[id]; m != nil {
			out = s.chatMessageEntity(r, c, m, true)
		} else {
			e = notFound("No message %s found in chat %s.", id, chatID)
		}
	}
	s.mu.RUnlock()
	if e != nil {
		s.fail(w, e)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

var chatMessageFilterRE = regexp.MustCompile(`^\s*createdDateTime\s+lt\s+(\S+)\s*$`)

func (s *Server) listChatMessages(w http.ResponseWriter, r *http.Request) {
	q, ok := s.checkQuery(w, r, "$top", "$skiptoken", "$orderby", "$filter")
	if !ok {
		return
	}
	if o := q.Get("$orderby"); o != "" && o != "createdDateTime desc" {
		s.graphError(w, http.StatusBadRequest, "BadRequest", "Only $orderby=createdDateTime desc is supported when listing chat messages.")
		return
	}
	var before time.Time
	if f := q.Get("$filter"); f != "" {
		m := chatMessageFilterRE.FindStringSubmatch(f)
		var err error
		if m != nil {
			before, err = time.Parse(time.RFC3339Nano, m[1])
		}
		if m == nil || err != nil || q.Get("$orderby") == "" {
			s.graphError(w, http.StatusBadRequest, "BadRequest",
				"Listing chat messages supports only $filter=createdDateTime lt …, together with $orderby=createdDateTime desc.")
			return
		}
	}
	chatID := r.PathValue("id")
	s.mu.RLock()
	c, e := s.locateChat(chatID)
	var items []map[string]any
	if e == nil {
		var matched []*messageState
		for _, m := range c.conv.messages {
			if !before.IsZero() && !m.createdAt.Before(before) {
				continue
			}
			matched = append(matched, m)
		}
		slices.SortFunc(matched, func(a, b *messageState) int { return b.createdAt.Compare(a.createdAt) })
		items = make([]map[string]any, 0, len(matched))
		for _, m := range matched {
			items = append(items, s.chatMessageEntity(r, c, m, false))
		}
	}
	s.mu.RUnlock()
	if e != nil {
		s.fail(w, e)
		return
	}
	start, end, next, err := s.windowN(r, q, len(items), min(s.cfg.pageSize, messagePageDefault), messagePageMax)
	if err != nil {
		s.graphError(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	s.writeCollection(w, r, "chats('"+chatID+"')/messages", items[start:end], next)
}

// ChatView is a chat as tests inspect it.
type ChatView struct {
	ID        string    `json:"id"`
	ChatType  string    `json:"chatType"`
	Topic     string    `json:"topic,omitempty"`
	CreatedAt time.Time `json:"createdDateTime"`
	// OriginalCreatedAt is the creation time before migration mode moved
	// it; nil if the chat never entered migration mode.
	OriginalCreatedAt *time.Time `json:"originalCreatedDateTime,omitempty"`
	MigrationMode     string     `json:"migrationMode,omitempty"`
	MigrationOwner    string     `json:"migrationOwner,omitempty"`
	// CreatedBy is the app that created the chat; empty for a seeded one.
	CreatedBy    string           `json:"createdBy,omitempty"`
	Members      []ChatMemberView `json:"members"`
	MessageCount int              `json:"messageCount"`
}

// ChatMemberView is one participant of a chat.
type ChatMemberView struct {
	UserID              string     `json:"userId"`
	Roles               []string   `json:"roles"`
	VisibleHistoryStart *time.Time `json:"visibleHistoryStartDateTime,omitempty"`
}

// ChatContent is a chat with its messages, for the state dump.
type ChatContent struct {
	ChatView
	Messages []MessageView `json:"messages"`
}

func chatView(c *chatState) ChatView {
	v := ChatView{
		ID: c.id, ChatType: c.chatType, Topic: c.topic, CreatedAt: c.conv.createdAt,
		MigrationMode: c.conv.migrationMode, MigrationOwner: c.conv.migrationOwner, CreatedBy: c.createdBy,
		Members: []ChatMemberView{}, MessageCount: len(c.conv.messages),
	}
	if c.conv.originalCreatedAt != nil {
		original := *c.conv.originalCreatedAt
		v.OriginalCreatedAt = &original
	}
	for _, m := range c.members {
		mv := ChatMemberView{UserID: m.userID, Roles: append([]string{}, m.roles...)}
		if m.visibleHistoryStart != nil {
			t := *m.visibleHistoryStart
			mv.VisibleHistoryStart = &t
		}
		v.Members = append(v.Members, mv)
	}
	return v
}

// Chats returns every chat, in creation order (seeded chats first).
func (s *Server) Chats() []ChatView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ChatView, 0, len(s.st.chats))
	for _, c := range s.st.chats {
		out = append(out, chatView(c))
	}
	return out
}

// ChatMessages returns a chat's messages, oldest first. A message people
// wrote (seeded) has an empty ImportedBy.
func (s *Server) ChatMessages(chatID string) []MessageView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.findChat(chatID)
	if c == nil {
		return nil
	}
	return messageViews(c.conv)
}

func (s *Server) chatContent() []ChatContent {
	out := make([]ChatContent, 0, len(s.st.chats))
	for _, c := range s.st.chats {
		out = append(out, ChatContent{ChatView: chatView(c), Messages: messageViews(c.conv)})
	}
	return out
}

// RemoveChannelMember takes a user out of a private channel, as an owner of
// the team could in Teams. It reports whether the user was a member.
func (s *Server) RemoveChannelMember(teamID, channelID, userID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := findChannel(s.findAnyTeam(teamID), channelID)
	if ch == nil {
		return false
	}
	for i, m := range ch.members {
		if strings.EqualFold(m.userID, userID) {
			ch.members = append(ch.members[:i], ch.members[i+1:]...)
			return true
		}
	}
	return false
}
