package fakegraph

import (
	"cmp"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// messageInput is a chatMessage as posted for import.
type messageInput struct {
	CreatedDateTime *string `json:"createdDateTime"`
	From            *struct {
		User *identityInput `json:"user"`
	} `json:"from"`
	Body *struct {
		ContentType *string `json:"contentType"`
		Content     string  `json:"content"`
	} `json:"body"`
	Mentions []struct {
		ID        *int   `json:"id"`
		Text      string `json:"mentionText"`
		Mentioned *struct {
			User *identityInput `json:"user"`
		} `json:"mentioned"`
	} `json:"mentions"`
	Attachments []struct {
		ID          string `json:"id"`
		ContentType string `json:"contentType"`
		ContentURL  string `json:"contentUrl"`
		Name        string `json:"name"`
	} `json:"attachments"`
	HostedContents []struct {
		TemporaryID  string `json:"@microsoft.graph.temporaryId"`
		ContentBytes string `json:"contentBytes"`
		ContentType  string `json:"contentType"`
	} `json:"hostedContents"`
}

var (
	// attachmentRE finds the <attachment id="…"></attachment> markers of an
	// HTML body; hostedRE the hosted contents its images point at.
	attachmentRE = regexp.MustCompile(`(?i)<attachment\s+id\s*=\s*"([^"]*)"\s*>\s*</attachment>`)
	hostedRE     = regexp.MustCompile(`\.\./hostedContents/([^/"'\s]+)/\$value`)
	guidRE       = regexp.MustCompile(`^[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}$`)
)

type identityInput struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

// atRE finds the <at id="N"> tags of an HTML message body.
var atRE = regexp.MustCompile(`(?i)<at\s+id\s*=\s*"(\d+)"\s*>`)

// Page sizes of the message lists: Graph returns 20 by default and at most
// 50 per page.
const (
	messagePageDefault = 20
	messagePageMax     = 50
)

// postMessage serves both POST …/messages and POST …/messages/{mid}/replies.
// Only imports are implemented: a message carrying createdDateTime and from,
// posted to a channel in migration mode.
func (s *Server) postMessage(w http.ResponseWriter, r *http.Request) {
	teamID, channelID, parentID := r.PathValue("id"), r.PathValue("cid"), r.PathValue("mid")
	isReply := parentID != ""

	s.mu.RLock()
	_, _, e := s.locate(teamID, channelID)
	s.mu.RUnlock()
	if e != nil {
		s.fail(w, e)
		return
	}
	// Throttling comes before any work on the message, so a throttled
	// request never stores anything.
	if ok, wait := s.importAllowed(channelKey(teamID, channelID), false, s.tenantID(), record(r).AppID); !ok {
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
			"applications can only import messages (createdDateTime and from set) into a channel in migration mode."))
		return
	}

	now := s.cfg.now()
	app := record(r).AppID
	status := http.StatusCreated
	out, e := func() (map[string]any, *gErr) {
		s.mu.Lock()
		defer s.mu.Unlock()
		t, ch, e := s.locate(teamID, channelID)
		if e != nil {
			return nil, e
		}
		if ch.migrationMode != migrationInProgress {
			return nil, forbidden("Importing messages is only possible while the channel is in migration mode.")
		}
		if ch.migrationOwner != app {
			return nil, forbidden("%s", notOwnerMessage)
		}
		if isReply {
			parent := ch.messages[parentID]
			if parent == nil {
				return nil, notFound("No message %s found in channel %s.", parentID, channelID)
			}
			if parent.replyToID != "" {
				return nil, badRequest("Message %s is itself a reply; replies can only be added to a top-level message.", parentID)
			}
		}
		m, e := s.validateImport(&in, ch, now)
		if e != nil {
			return nil, e
		}
		if ch.byMillis[m.createdAt.UnixMilli()] != nil {
			s.conflicts.Add(1)
			return nil, &gErr{http.StatusConflict, "Conflict",
				"A message with the same createdDateTime (" + graphTime(m.createdAt) + ") already exists in this channel."}
		}
		m.id = strconv.FormatInt(m.createdAt.UnixMilli(), 10)
		if s.cfg.opaqueMessageIDs {
			// Numeric like the real thing, but far outside any timestamp.
			m.id = s.nextID("1%018d")
		}
		m.replyToID, m.importedBy = parentID, app
		ch.messages[m.id] = m
		ch.byMillis[m.createdAt.UnixMilli()] = m
		s.imports.Add(1)
		out := s.messageEntity(t.ID, ch, m)
		out["@odata.context"] = "https://" + r.Host + "/v1.0/$metadata#teams('" + t.ID + "')/channels('" + ch.ID + "')/messages/$entity"
		return out, nil
	}()
	if e != nil {
		s.fail(w, e)
		return
	}
	writeJSON(w, status, out)
}

// validateImport checks an import payload against the channel and returns
// the message to store. It must be called with s.mu held.
func (s *Server) validateImport(in *messageInput, ch *channelState, now time.Time) (*messageState, *gErr) {
	if in.CreatedDateTime == nil || in.From == nil {
		return nil, badRequest("Both createdDateTime and from must be set to import a message.")
	}
	created, err := time.Parse(time.RFC3339Nano, *in.CreatedDateTime)
	if err != nil {
		return nil, badRequest("Invalid createdDateTime: an ISO 8601 UTC timestamp is expected.")
	}
	created = created.UTC().Truncate(time.Millisecond)
	switch {
	case created.After(now):
		return nil, badRequest("createdDateTime must not be in the future.")
	case !created.After(ch.createdAt):
		return nil, badRequest("createdDateTime must be later than the channel's createdDateTime (%s).", graphTime(ch.createdAt))
	}

	if in.From.User == nil || in.From.User.ID == "" {
		return nil, badRequest("The 'from' identity must be a user with an id (from.user.id).")
	}
	fromID, fromName := in.From.User.ID, in.From.User.DisplayName
	if u := s.findUser(fromID); u != nil {
		fromID = u.ID
	} else if !s.cfg.lenientFrom {
		return nil, badRequest("The 'from' user (from.user.id) was not found in the tenant.")
	}

	if in.Body == nil || in.Body.Content == "" {
		return nil, badRequest("Missing body content.")
	}
	contentType := "text"
	if in.Body.ContentType != nil {
		contentType = *in.Body.ContentType
	}
	if contentType != "html" && contentType != "text" {
		return nil, badRequest("Invalid body.contentType specified. Must be 'html' or 'text'.")
	}
	if size := len(in.Body.Content); size > s.cfg.maxMessageBytes {
		return nil, badRequest("The message body is too large: %d bytes exceeds the limit of %d bytes.", size, s.cfg.maxMessageBytes)
	}

	// Every <at id="N"> needs a mention with that id, and the reverse.
	tagged := map[int]bool{}
	if contentType == "html" {
		for _, m := range atRE.FindAllStringSubmatch(in.Body.Content, -1) {
			if id, err := strconv.Atoi(m[1]); err == nil {
				tagged[id] = true
			}
		}
	}
	mentions := make([]MentionView, 0, len(in.Mentions))
	seen := map[int]bool{}
	for _, mn := range in.Mentions {
		switch {
		case mn.ID == nil:
			return nil, badRequest("Every mention needs an id.")
		case seen[*mn.ID]:
			return nil, badRequest("Mention id %d is used more than once.", *mn.ID)
		case !tagged[*mn.ID]:
			return nil, badRequest("Mention id %d has no matching <at id=\"%d\"> tag in the message body.", *mn.ID, *mn.ID)
		case mn.Mentioned == nil || mn.Mentioned.User == nil || mn.Mentioned.User.ID == "":
			return nil, badRequest("Mention id %d must name a user (mentioned.user.id).", *mn.ID)
		}
		seen[*mn.ID] = true
		mentions = append(mentions, MentionView{
			ID: *mn.ID, Text: mn.Text, UserID: mn.Mentioned.User.ID, DisplayName: mn.Mentioned.User.DisplayName,
		})
	}
	for id := range tagged {
		if !seen[id] {
			return nil, badRequest("The <at id=\"%d\"> tag in the message body has no matching entry in mentions.", id)
		}
	}
	msg := &messageState{
		createdAt: created, fromUserID: fromID, fromName: fromName,
		contentType: contentType, content: in.Body.Content, mentions: mentions,
		attachments: []AttachmentView{},
	}

	// Every attachment needs an <attachment id="…"> marker in the body, and
	// the reverse.
	marked := map[string]bool{}
	if contentType == "html" {
		for _, m := range attachmentRE.FindAllStringSubmatch(in.Body.Content, -1) {
			marked[m[1]] = true
		}
	}
	attached := map[string]bool{}
	for i, a := range in.Attachments {
		switch {
		case a.ID == "":
			return nil, badRequest("Attachment %d needs an id.", i)
		case attached[a.ID]:
			return nil, badRequest("Attachment id %s is used more than once.", a.ID)
		case a.ContentType != "reference":
			return nil, badRequest("Attachment %s: only attachments of contentType 'reference' can be imported.", a.ID)
		case a.ContentURL == "":
			return nil, badRequest("Attachment %s needs a contentUrl.", a.ID)
		case !marked[a.ID]:
			return nil, badRequest("Attachment %s has no matching <attachment id> marker in the message body.", a.ID)
		}
		if !s.cfg.lenientAttachments {
			if !guidRE.MatchString(a.ID) {
				return nil, badRequest("Attachment %s: the id must be the GUID from the file's eTag, without braces.", a.ID)
			}
			item := s.st.itemsByGUID[strings.ToUpper(a.ID)]
			if item == nil || item.folder {
				return nil, badRequest("Attachment %s: the id is not the eTag GUID of a file in the tenant.", a.ID)
			}
			if a.ContentURL != item.webDavURL() {
				return nil, badRequest("Attachment %s: the contentUrl must be the file's webDavUrl; sharing links and other URLs are not supported in migration.", a.ID)
			}
		}
		attached[a.ID] = true
		msg.attachments = append(msg.attachments, AttachmentView{ID: a.ID, Name: a.Name, ContentURL: a.ContentURL})
	}
	for id := range marked {
		if !attached[id] {
			return nil, badRequest("The <attachment id> marker %s in the message body has no matching entry in attachments.", id)
		}
	}

	// Every hosted content must be referenced by the body, and the reverse.
	referenced := map[string]bool{}
	for _, m := range hostedRE.FindAllStringSubmatch(in.Body.Content, -1) {
		referenced[m[1]] = true
	}
	hosted := map[string]bool{}
	sum := sha256.New()
	for i, hc := range in.HostedContents {
		id := hc.TemporaryID
		switch {
		case id == "":
			return nil, badRequest("Hosted content %d needs an @microsoft.graph.temporaryId.", i)
		case hosted[id]:
			return nil, badRequest("Hosted content temporaryId %s is used more than once.", id)
		case !strings.HasPrefix(hc.ContentType, "image/"):
			return nil, badRequest("Hosted content %s: contentType must be an image type.", id)
		case !referenced[id]:
			return nil, badRequest("Hosted content %s is not referenced by the message body (../hostedContents/%s/$value).", id, id)
		}
		data, err := base64.StdEncoding.DecodeString(hc.ContentBytes)
		if err != nil {
			return nil, badRequest("Hosted content %s: contentBytes is not valid base64.", id)
		}
		if len(data) > s.cfg.maxHostedBytes {
			return nil, badRequest("Hosted content %s is too large: %d bytes exceeds the limit of %d bytes.", id, len(data), s.cfg.maxHostedBytes)
		}
		hosted[id] = true
		sum.Write(data)
		msg.hostedCount++
		msg.hostedBytes += int64(len(data))
	}
	for id := range referenced {
		if !hosted[id] {
			return nil, badRequest("The message body references hosted content %s, which is not in hostedContents.", id)
		}
	}
	if msg.hostedCount > 0 {
		msg.hostedHash = hex.EncodeToString(sum.Sum(nil))
	}
	return msg, nil
}

// messageEntity renders a chatMessage. It must be called with s.mu held.
func (s *Server) messageEntity(teamID string, ch *channelState, m *messageState) map[string]any {
	var replyTo any
	if m.replyToID != "" {
		replyTo = m.replyToID
	}
	mentions := make([]map[string]any, 0, len(m.mentions))
	for _, mn := range m.mentions {
		mentions = append(mentions, map[string]any{
			"id": mn.ID, "mentionText": mn.Text,
			"mentioned": map[string]any{
				"application": nil, "device": nil, "conversation": nil,
				"user": map[string]any{"id": mn.UserID, "displayName": mn.DisplayName, "userIdentityType": "aadUser"},
			},
		})
	}
	attachments := make([]map[string]any, 0, len(m.attachments))
	for _, a := range m.attachments {
		attachments = append(attachments, map[string]any{
			"id": a.ID, "contentType": "reference", "contentUrl": a.ContentURL, "name": a.Name,
			"content": nil, "thumbnailUrl": nil, "teamsAppId": nil,
		})
	}
	created := graphTime(m.createdAt)
	return map[string]any{
		"id": m.id, "replyToId": replyTo, "etag": m.id, "messageType": "message",
		"createdDateTime": created, "lastModifiedDateTime": created,
		"lastEditedDateTime": nil, "deletedDateTime": nil,
		"subject": nil, "summary": nil, "chatId": nil, "importance": "normal", "locale": "en-us",
		"webUrl": nil, "policyViolation": nil, "eventDetail": nil,
		"from": map[string]any{
			"application": nil, "device": nil,
			"user": map[string]any{
				"@odata.type": "#microsoft.graph.teamworkUserIdentity",
				"id":          m.fromUserID, "displayName": m.fromName, "userIdentityType": "aadUser", "tenantId": s.st.tenantID,
			},
		},
		"body":            map[string]any{"contentType": m.contentType, "content": m.content},
		"channelIdentity": map[string]any{"teamId": teamID, "channelId": ch.ID},
		"attachments":     attachments, "mentions": mentions, "reactions": []any{},
	}
}

// getMessage serves GET …/messages/{mid} (top-level messages only) and
// GET …/messages/{mid}/replies/{rid}.
func (s *Server) getMessage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.checkQuery(w, r); !ok {
		return
	}
	teamID, channelID, parentID, replyID := r.PathValue("id"), r.PathValue("cid"), r.PathValue("mid"), r.PathValue("rid")
	s.mu.RLock()
	t, ch, e := s.locate(teamID, channelID)
	var out map[string]any
	if e == nil {
		want, wantParent := parentID, ""
		if replyID != "" {
			want, wantParent = replyID, parentID
		}
		if m := ch.messages[want]; m != nil && m.replyToID == wantParent {
			out = s.messageEntity(t.ID, ch, m)
			out["@odata.context"] = "https://" + r.Host + "/v1.0/$metadata#teams('" + t.ID + "')/channels('" + ch.ID + "')/messages/$entity"
		} else {
			e = notFound("No message %s found in channel %s.", want, channelID)
		}
	}
	s.mu.RUnlock()
	if e != nil {
		s.fail(w, e)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// listMessages serves GET …/messages (top-level messages) and
// GET …/messages/{mid}/replies, newest first.
func (s *Server) listMessages(w http.ResponseWriter, r *http.Request) {
	q, ok := s.checkQuery(w, r, "$top", "$skiptoken")
	if !ok {
		return
	}
	teamID, channelID, parentID := r.PathValue("id"), r.PathValue("cid"), r.PathValue("mid")
	s.mu.RLock()
	t, ch, e := s.locate(teamID, channelID)
	var items []map[string]any
	if e == nil && parentID != "" {
		if p := ch.messages[parentID]; p == nil || p.replyToID != "" {
			e = notFound("No message %s found in channel %s.", parentID, channelID)
		}
	}
	if e == nil {
		var matched []*messageState
		for _, m := range ch.messages {
			if m.replyToID == parentID {
				matched = append(matched, m)
			}
		}
		slices.SortFunc(matched, func(a, b *messageState) int { return b.createdAt.Compare(a.createdAt) })
		items = make([]map[string]any, 0, len(matched))
		for _, m := range matched {
			items = append(items, s.messageEntity(t.ID, ch, m))
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
	entity := "teams('" + teamID + "')/channels('" + channelID + "')/messages"
	if parentID != "" {
		entity += "('" + parentID + "')/replies"
	}
	s.writeCollection(w, r, entity, items[start:end], next)
}

// --- inspection -------------------------------------------------------------

// TeamView is a team as tests see it.
type TeamView struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Description string `json:"description,omitempty"`
	Visibility  string `json:"visibility"`
	// Provisioning is "succeeded", "inProgress" or "failed". Only a
	// succeeded team is visible through the Graph endpoints.
	Provisioning string `json:"provisioning"`
	// CreatedBy is the app that created the team; empty for seeded teams.
	CreatedBy string       `json:"createdBy,omitempty"`
	Members   []MemberView `json:"members"`
	// MigrationMode is "inProgress" for a team created in migration mode
	// until its migration is completed, then "completed"; otherwise "".
	MigrationMode string `json:"migrationMode,omitempty"`
}

// MemberView is one member of a team.
type MemberView struct {
	UserID string `json:"userId"`
	// Owner is false for plain members.
	Owner bool `json:"owner"`
}

// ChannelView is a channel as tests see it.
type ChannelView struct {
	ID             string    `json:"id"`
	TeamID         string    `json:"teamId"`
	DisplayName    string    `json:"displayName"`
	Description    string    `json:"description,omitempty"`
	MembershipType string    `json:"membershipType"`
	CreatedAt      time.Time `json:"createdDateTime"`
	// OriginalCreatedAt is the creation time before the first
	// startMigration; nil if the channel never entered migration mode.
	OriginalCreatedAt *time.Time `json:"originalCreatedDateTime,omitempty"`
	// MigrationMode is "", "inProgress" or "completed".
	MigrationMode string `json:"migrationMode,omitempty"`
	// MigrationOwner is the app holding the open migration session.
	MigrationOwner string `json:"migrationOwner,omitempty"`
	// MessageCount counts top-level messages and replies.
	MessageCount int `json:"messageCount"`
	// Members are the channel's own members (private and shared channels
	// only; a standard channel's members are the team's).
	Members []MemberView `json:"members,omitempty"`
}

// MessageView is a stored message, top-level or reply.
type MessageView struct {
	ID string `json:"id"`
	// ReplyToID is the parent message's ID; empty for top-level messages.
	ReplyToID       string        `json:"replyToId,omitempty"`
	CreatedAt       time.Time     `json:"createdDateTime"`
	FromUserID      string        `json:"fromUserId"`
	FromDisplayName string        `json:"fromDisplayName"`
	BodyContentType string        `json:"bodyContentType"`
	BodyContent     string        `json:"bodyContent"`
	Mentions        []MentionView `json:"mentions"`
	// ImportedBy is the app that imported the message.
	ImportedBy  string           `json:"importedBy"`
	Attachments []AttachmentView `json:"attachments"`
	// HostedContents is the number of hosted contents (inline images),
	// HostedBytes their total decoded size and HostedSHA256 the hex digest
	// of their decoded bytes concatenated in request order ("" for none).
	HostedContents int    `json:"hostedContents"`
	HostedBytes    int64  `json:"hostedBytes"`
	HostedSHA256   string `json:"hostedSha256,omitempty"`
}

// AttachmentView is one file attachment of a message.
type AttachmentView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	ContentURL string `json:"contentUrl"`
}

// MentionView is one @mention of a message.
type MentionView struct {
	ID          int    `json:"id"`
	Text        string `json:"mentionText"`
	UserID      string `json:"userId"`
	DisplayName string `json:"displayName,omitempty"`
}

// Stats are counters since the server started.
type Stats struct {
	// ImportsAccepted counts messages and replies stored (201 Created,
	// whether or not the response reached the client).
	ImportsAccepted int `json:"importsAccepted"`
	// Conflicts counts 409 responses to imports.
	Conflicts int `json:"conflicts"`
	// Throttles counts 429 responses, from rate limits and injected faults.
	Throttles int `json:"throttles"`
	// UploadsCompleted counts files stored (by upload session or simple
	// PUT), UploadBytes their total size, UploadSessions sessions created.
	UploadsCompleted int   `json:"uploadsCompleted"`
	UploadBytes      int64 `json:"uploadBytes"`
	UploadSessions   int   `json:"uploadSessions"`
}

// Stats returns the counters.
func (s *Server) Stats() Stats {
	return Stats{
		ImportsAccepted: int(s.imports.Load()),
		Conflicts:       int(s.conflicts.Load()),
		Throttles:       int(s.throttles.Load()),

		UploadsCompleted: int(s.uploadsCompleted.Load()),
		UploadBytes:      s.uploadBytes.Load(),
		UploadSessions:   int(s.uploadSessions.Load()),
	}
}

// Teams returns every team, seeded ones first and then in creation order,
// including teams whose provisioning is unfinished or failed.
func (s *Server) Teams() []TeamView {
	now := s.cfg.now()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]TeamView, 0, len(s.st.teams))
	for _, t := range s.st.teams {
		out = append(out, s.teamView(t, now))
	}
	return out
}

func (s *Server) teamView(t *teamState, now time.Time) TeamView {
	v := TeamView{
		ID: t.ID, DisplayName: t.DisplayName, Description: t.Description, Visibility: t.Visibility,
		Provisioning: s.provisioning(t, now), CreatedBy: t.createdBy, Members: []MemberView{},
		MigrationMode: t.migrationMode,
	}
	for _, m := range t.members {
		v.Members = append(v.Members, MemberView{UserID: m.userID, Owner: slices.Contains(m.roles, "owner")})
	}
	return v
}

// Channels returns the channels of a team in creation order, or nil if
// there is no such team.
func (s *Server) Channels(teamID string) []ChannelView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t := s.findAnyTeam(teamID)
	if t == nil {
		return nil
	}
	out := make([]ChannelView, 0, len(t.channels))
	for _, ch := range t.channels {
		out = append(out, channelView(t, ch))
	}
	return out
}

func channelView(t *teamState, ch *channelState) ChannelView {
	v := ChannelView{
		ID: ch.ID, TeamID: t.ID, DisplayName: ch.DisplayName, Description: ch.Description,
		MembershipType: ch.MembershipType, CreatedAt: ch.createdAt,
		MigrationMode: ch.migrationMode, MigrationOwner: ch.migrationOwner, MessageCount: len(ch.messages),
	}
	if ch.originalCreatedAt != nil {
		original := *ch.originalCreatedAt
		v.OriginalCreatedAt = &original
	}
	for _, m := range ch.members {
		v.Members = append(v.Members, MemberView{UserID: m.userID, Owner: slices.Contains(m.roles, "owner")})
	}
	return v
}

// Messages returns every message of a channel, top-level messages and
// replies alike, sorted by createdDateTime (which is unique within a
// channel). It returns nil if there is no such team or channel.
func (s *Server) Messages(teamID, channelID string) []MessageView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ch := findChannel(s.findAnyTeam(teamID), channelID)
	if ch == nil {
		return nil
	}
	return messageViews(ch)
}

func messageViews(ch *channelState) []MessageView {
	out := make([]MessageView, 0, len(ch.messages))
	for _, m := range ch.messages {
		out = append(out, MessageView{
			ID: m.id, ReplyToID: m.replyToID, CreatedAt: m.createdAt,
			FromUserID: m.fromUserID, FromDisplayName: m.fromName,
			BodyContentType: m.contentType, BodyContent: m.content,
			Mentions: append([]MentionView{}, m.mentions...), ImportedBy: m.importedBy,
			Attachments:    append([]AttachmentView{}, m.attachments...),
			HostedContents: m.hostedCount, HostedBytes: m.hostedBytes, HostedSHA256: m.hostedHash,
		})
	}
	slices.SortFunc(out, func(a, b MessageView) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return out
}

// TeamContent is a team with its channels and their messages, as served
// under "content" by GET /_fake/state.
type TeamContent struct {
	TeamView
	Channels []ChannelContent `json:"channels"`
}

// ChannelContent is a channel with its messages.
type ChannelContent struct {
	ChannelView
	Files    []FileView    `json:"files"`
	Messages []MessageView `json:"messages"`
}

// content must be called with s.mu held.
func (s *Server) content(now time.Time) []TeamContent {
	out := make([]TeamContent, 0, len(s.st.teams))
	for _, t := range s.st.teams {
		tc := TeamContent{TeamView: s.teamView(t, now), Channels: []ChannelContent{}}
		for _, ch := range t.channels {
			tc.Channels = append(tc.Channels, ChannelContent{ChannelView: channelView(t, ch), Files: fileViews(ch), Messages: messageViews(ch)})
		}
		out = append(out, tc)
	}
	return out
}
