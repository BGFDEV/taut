package fakegraph

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// This file is the write path: creating teams and channels, migration mode
// and adding members. Importing and reading messages is in messages.go.

// gErr is a Graph error a handler decided on while holding the lock; it is
// written after the lock is released.
type gErr struct {
	status  int
	code    string
	message string
}

func badRequest(format string, a ...any) *gErr {
	return &gErr{http.StatusBadRequest, "BadRequest", fmt.Sprintf(format, a...)}
}

func notFound(format string, a ...any) *gErr {
	return &gErr{http.StatusNotFound, "NotFound", fmt.Sprintf(format, a...)}
}

func forbidden(format string, a ...any) *gErr {
	return &gErr{http.StatusForbidden, "Forbidden", fmt.Sprintf(format, a...)}
}

func (s *Server) fail(w http.ResponseWriter, e *gErr) {
	s.graphError(w, e.status, e.code, e.message)
}

// graphTime formats a timestamp the way Teams does: UTC, milliseconds.
func graphTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// maxBodyBytes bounds any JSON request body the fake will read.
const maxBodyBytes = 64 << 20

// readJSON decodes the request body into v. An empty body is allowed when
// optional is set, leaving v untouched.
func readJSON(w http.ResponseWriter, r *http.Request, v any, optional bool) *gErr {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		return &gErr{http.StatusRequestEntityTooLarge, "RequestEntityTooLarge",
			fmt.Sprintf("The request body is larger than the %d bytes fakegraph reads.", maxBodyBytes)}
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		if optional {
			return nil
		}
		return badRequest("Empty Payload. JSON content expected.")
	}
	if err := json.Unmarshal(data, v); err != nil {
		// The decoder's message names a position or a type, never content.
		return badRequest("Unable to read JSON request payload. Please ensure Content-Type header is set and payload is of valid JSON format.")
	}
	return nil
}

// nextID returns a deterministic identifier with the given shape. It must be
// called with the write lock held.
func (s *Server) nextID(format string) string {
	s.st.seq++
	return fmt.Sprintf(format, s.st.seq)
}

// findUser must be called with s.mu held.
func (s *Server) findUser(id string) *User {
	for _, u := range s.st.users {
		if strings.EqualFold(u.ID, id) {
			return u
		}
	}
	return nil
}

// bindRE extracts the user ID from a user@odata.bind value, in either
// spelling Graph accepts: users('id') or users/id.
var bindRE = regexp.MustCompile(`(?i)/users(?:\('([^']+)'\)|/([^/?#]+))\s*$`)

func boundUserID(bind string) string {
	m := bindRE.FindStringSubmatch(bind)
	if m == nil {
		return ""
	}
	return m[1] + m[2]
}

// memberInput is an aadUserConversationMember as posted.
type memberInput struct {
	Type  string   `json:"@odata.type"`
	Roles []string `json:"roles"`
	Bind  string   `json:"user@odata.bind"`
}

// --- teams ------------------------------------------------------------------

// Provisioning states of a team, as reported by its createTeam operation.
const (
	opInProgress = "inProgress"
	opSucceeded  = "succeeded"
	opFailed     = "failed"
)

// provisioning reports where a team's creation stands. Seeded teams have
// always succeeded. It must be called with s.mu held.
func (s *Server) provisioning(t *teamState, now time.Time) string {
	switch {
	case t.op == nil:
		return opSucceeded
	case t.op.polls < s.cfg.provisionPolls, now.Before(t.op.createdAt.Add(s.cfg.provisionDelay)):
		return opInProgress
	case t.op.fail:
		return opFailed
	}
	return opSucceeded
}

func (s *Server) createTeam(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Template     string        `json:"template@odata.bind"`
		DisplayName  string        `json:"displayName"`
		Description  string        `json:"description"`
		Visibility   string        `json:"visibility"`
		Members      []memberInput `json:"members"`
		CreationMode string        `json:"@microsoft.graph.teamCreationMode"`
		Created      *string       `json:"createdDateTime"`
	}
	if e := readJSON(w, r, &in, false); e != nil {
		s.fail(w, e)
		return
	}
	now := s.cfg.now().UTC()
	app := record(r).AppID

	teamID, opID, e := func() (string, string, *gErr) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case !strings.Contains(in.Template, "teamsTemplates('standard')"):
			return "", "", badRequest("Invalid template: 'template@odata.bind' must reference teamsTemplates('standard').")
		case strings.TrimSpace(in.DisplayName) == "":
			return "", "", badRequest("Team displayName is required.")
		case utf8.RuneCountInString(in.DisplayName) > 256:
			return "", "", badRequest("Team displayName must not exceed 256 characters.")
		case in.Visibility != "" && in.Visibility != "private" && in.Visibility != "public":
			return "", "", badRequest("Invalid visibility specified. Must be 'private' or 'public'.")
		case in.CreationMode != "" && in.CreationMode != "migration":
			return "", "", badRequest("Invalid '@microsoft.graph.teamCreationMode' specified. Must be 'migration'.")
		}
		// A team created in migration mode has a creation time in the
		// past and nobody in it: members, owners included, can only be
		// added once its migration is completed. Whether the real service
		// rejects members at creation is not documented; its example
		// sends none, and the fake insists on that.
		migration := in.CreationMode == "migration"
		created := now
		var owner *User
		switch {
		case migration && len(in.Members) > 0:
			return "", "", badRequest("Members cannot be specified when creating a team in migration mode; add them after completeMigration.")
		case migration:
			var e *gErr
			if created, e = migrationCreationTime(in.Created, now); e != nil {
				return "", "", e
			}
		case in.Created != nil:
			return "", "", badRequest("'createdDateTime' can only be set when creating a team in migration mode.")
		case len(in.Members) != 1 || !slices.Contains(in.Members[0].Roles, "owner"):
			return "", "", badRequest("A team owner must be provided when creating a team in application context: 'members' must hold exactly one member with the owner role.")
		default:
			if owner = s.findUser(boundUserID(in.Members[0].Bind)); owner == nil {
				return "", "", badRequest("Invalid bind property: the user referenced by 'user@odata.bind' was not found in the tenant.")
			}
		}
		visibility := in.Visibility
		if visibility == "" {
			visibility = "public"
		}
		t := &teamState{
			Team: Team{
				ID: s.nextID("c0000000-0000-4000-8000-%012d"), DisplayName: in.DisplayName,
				Description: in.Description, Visibility: visibility,
			},
			createdBy: app,
			op: &teamOp{
				id: s.nextID("d0000000-0000-4000-8000-%012d"), createdAt: now,
				fail: slices.Contains(s.cfg.provisionFailFor, in.DisplayName),
			},
		}
		if owner != nil {
			t.members = []*memberState{{id: s.nextID("member-%06d"), userID: owner.ID, roles: []string{"owner"}}}
		}
		general := newChannelState(Channel{
			ID: s.nextID("19:%032x@thread.tacv2"), DisplayName: "General", MembershipType: "standard",
		}, created)
		if migration {
			// Whether General starts in migration mode is not documented.
			// Microsoft's sample completed it explicitly, so the fake
			// takes it that it does.
			t.migrationMode, t.migrationOwner = migrationInProgress, app
			general.migrationMode, general.migrationOwner = migrationInProgress, app
		}
		t.channels = []*channelState{general}
		s.st.teams = append(s.st.teams, t)
		return t.ID, t.op.id, nil
	}()
	if e != nil {
		s.fail(w, e)
		return
	}
	w.Header().Set("Content-Location", "/teams('"+teamID+"')")
	w.Header().Set("Location", "/teams('"+teamID+"')/operations('"+opID+"')")
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) getTeamOperation(w http.ResponseWriter, r *http.Request) {
	teamID, opID := r.PathValue("id"), r.PathValue("op")
	now := s.cfg.now()
	s.mu.Lock()
	t := s.findAnyTeam(teamID)
	var out map[string]any
	if t != nil && t.op != nil && strings.EqualFold(t.op.id, opID) {
		status := s.provisioning(t, now)
		t.op.polls++
		var opErr any
		if status == opFailed {
			opErr = map[string]any{"code": "InternalServerError", "message": "Team provisioning failed."}
		}
		out = map[string]any{
			"@odata.context": "https://" + r.Host + "/v1.0/$metadata#teams('" + t.ID + "')/operations/$entity",
			"id":             t.op.id, "operationType": "createTeam", "status": status,
			"createdDateTime": graphTime(t.op.createdAt), "lastActionDateTime": graphTime(now),
			"attemptsCount": 1, "targetResourceId": t.ID, "targetResourceLocation": "/teams('" + t.ID + "')",
			"error": opErr,
		}
	}
	s.mu.Unlock()
	if out == nil {
		s.graphError(w, http.StatusNotFound, "NotFound", fmt.Sprintf("No operation %s found for team %s.", opID, teamID))
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) addTeamMember(w http.ResponseWriter, r *http.Request) {
	var in memberInput
	if e := readJSON(w, r, &in, false); e != nil {
		s.fail(w, e)
		return
	}
	teamID := r.PathValue("id")
	out, e := func() (map[string]any, *gErr) {
		s.mu.Lock()
		defer s.mu.Unlock()
		t := s.findTeam(teamID)
		if t == nil {
			return nil, notFound("No team found with Group Id %s.", teamID)
		}
		if !isMemberType(in.Type) {
			return nil, badRequest("The member must be of type '#microsoft.graph.aadUserConversationMember'.")
		}
		for _, role := range in.Roles {
			if role != "owner" && role != "member" {
				return nil, badRequest("Invalid role specified. Roles must be empty or one of 'owner', 'member'.")
			}
		}
		if t.migrationMode == migrationInProgress {
			return nil, badRequest("%s", teamMigratingMessage)
		}
		u := s.findUser(boundUserID(in.Bind))
		if u == nil {
			return nil, notFound("The user referenced by 'user@odata.bind' was not found in the tenant.")
		}
		m := s.putMember(&t.members, u, in.Roles)
		out := s.memberEntity(m, u)
		out["@odata.context"] = "https://" + r.Host + "/v1.0/$metadata#teams('" + t.ID + "')/members/$entity"
		return out, nil
	}()
	if e != nil {
		s.fail(w, e)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// teamMember returns the team's member with this user ID, or nil.
func teamMember(t *teamState, userID string) *memberState {
	for _, m := range t.members {
		if m.userID == userID {
			return m
		}
	}
	return nil
}

// putMember adds u to a member list, or replaces the roles of the entry
// already there. What the real service does with someone who is already a
// member is not documented; replacing the roles is the reading that can do
// harm (posting an owner again without the owner role demotes them), so it
// is the one modelled. It must be called with the write lock held.
func (s *Server) putMember(list *[]*memberState, u *User, roles []string) *memberState {
	var m *memberState
	for _, existing := range *list {
		if existing.userID == u.ID {
			m = existing
		}
	}
	if m == nil {
		m = &memberState{id: s.nextID("member-%06d"), userID: u.ID}
		*list = append(*list, m)
	}
	m.roles = []string{}
	if slices.Contains(roles, "owner") {
		m.roles = []string{"owner"}
	}
	return m
}

// memberEntity renders an aadUserConversationMember. It must be called with
// s.mu held.
func (s *Server) memberEntity(m *memberState, u *User) map[string]any {
	var mail any
	if u.Mail != "" {
		mail = u.Mail
	}
	return map[string]any{
		"@odata.type": "#microsoft.graph.aadUserConversationMember",
		"id":          m.id, "roles": append([]string{}, m.roles...), "displayName": u.DisplayName, "userId": u.ID, "email": mail,
		"tenantId": s.st.tenantID,
	}
}

// isMemberType accepts the aadUserConversationMember type with or without
// the leading "#"; Microsoft's own examples use both.
func isMemberType(t string) bool {
	return strings.EqualFold(strings.TrimPrefix(t, "#"), "microsoft.graph.aadUserConversationMember")
}

// MaxMembersPerAdd is the most members one members/add request may carry.
const MaxMembersPerAdd = 200

// FailMemberAdd makes the bulk members/add answer for this user with the
// given per-user error instead of adding them, until ClearMemberAddFailures
// is called. The request as a whole then answers 207.
func (s *Server) FailMemberAdd(userID, code, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.memberAddFail == nil {
		s.st.memberAddFail = map[string][2]string{}
	}
	s.st.memberAddFail[strings.ToLower(userID)] = [2]string{code, message}
}

// ClearMemberAddFailures removes every failure set with FailMemberAdd.
func (s *Server) ClearMemberAddFailures() {
	s.mu.Lock()
	s.st.memberAddFail = nil
	s.mu.Unlock()
}

// addTeamMembers serves POST /teams/{id}/members/add: up to 200 members in
// one request, answered 200 when all were added and 207 when some were
// not, with one result per requested member either way.
func (s *Server) addTeamMembers(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Values []memberInput `json:"values"`
	}
	if e := readJSON(w, r, &in, false); e != nil {
		s.fail(w, e)
		return
	}
	teamID := r.PathValue("id")
	status := http.StatusOK
	out, e := func() (map[string]any, *gErr) {
		s.mu.Lock()
		defer s.mu.Unlock()
		t := s.findTeam(teamID)
		switch {
		case t == nil:
			return nil, notFound("No team found with Group Id %s.", teamID)
		case len(in.Values) == 0:
			return nil, badRequest("'values' must hold at least one member.")
		case len(in.Values) > MaxMembersPerAdd:
			return nil, badRequest("A maximum of %d members can be added in one request; %d were sent.", MaxMembersPerAdd, len(in.Values))
		case t.migrationMode == migrationInProgress:
			return nil, badRequest("%s", teamMigratingMessage)
		}
		for _, v := range in.Values {
			if !isMemberType(v.Type) {
				return nil, badRequest("Every member must be of type 'microsoft.graph.aadUserConversationMember'.")
			}
			for _, role := range v.Roles {
				if role != "owner" && role != "member" {
					return nil, badRequest("Invalid role specified. Roles must be empty or one of 'owner', 'member'.")
				}
			}
		}
		results := make([]map[string]any, 0, len(in.Values))
		for _, v := range in.Values {
			id := boundUserID(v.Bind)
			res := map[string]any{"@odata.type": "#microsoft.graph.aadUserConversationMemberResult", "userId": id, "error": nil}
			u := s.findUser(id)
			switch injected, failing := s.st.memberAddFail[strings.ToLower(id)]; {
			case failing:
				res["error"] = map[string]any{"code": injected[0], "message": injected[1]}
			case u == nil:
				// The per-user error codes of the real service are not
				// documented; this mirrors the 404 of the single add.
				res["error"] = map[string]any{"code": "NotFound", "message": "The user referenced by 'user@odata.bind' was not found in the tenant."}
			default:
				res["userId"] = u.ID
				s.putMember(&t.members, u, v.Roles)
			}
			if res["error"] != nil {
				status = http.StatusMultiStatus
			}
			results = append(results, res)
		}
		return map[string]any{
			"@odata.context": "https://" + r.Host + "/v1.0/$metadata#Collection(microsoft.graph.aadUserConversationMemberResult)",
			"value":          results,
		}, nil
	}()
	if e != nil {
		s.fail(w, e)
		return
	}
	writeJSON(w, status, out)
}

const teamMigratingMessage = "Members cannot be added or removed while the team is in migration mode; complete the team's migration first."

const notTeamMemberMessage = "The user is not a member of the team; only members of the team can be added to a private or shared channel."

// addChannelMember serves POST /teams/{id}/channels/{cid}/members, which
// exists for private and shared channels only.
func (s *Server) addChannelMember(w http.ResponseWriter, r *http.Request) {
	var in memberInput
	if e := readJSON(w, r, &in, false); e != nil {
		s.fail(w, e)
		return
	}
	out, e := func() (map[string]any, *gErr) {
		s.mu.Lock()
		defer s.mu.Unlock()
		t, ch, e := s.locate(r.PathValue("id"), r.PathValue("cid"))
		if e != nil {
			return nil, e
		}
		if ch.MembershipType == "standard" {
			return nil, badRequest("Members can only be added to private or shared channels; a standard channel's members are the team's.")
		}
		if !isMemberType(in.Type) {
			return nil, badRequest("The member must be of type '#microsoft.graph.aadUserConversationMember'.")
		}
		for _, role := range in.Roles {
			if role != "owner" && role != "member" {
				return nil, badRequest("Invalid role specified. Roles must be empty or one of 'owner', 'member'.")
			}
		}
		u := s.findUser(boundUserID(in.Bind))
		if u == nil || !u.AccountEnabled {
			return nil, notFound("The user referenced by 'user@odata.bind' was not found in the tenant, or the account is disabled.")
		}
		// The status and wording the real service uses here are not
		// documented; 400 with a message that says so is the fake's choice.
		if teamMember(t, u.ID) == nil {
			return nil, badRequest("%s", notTeamMemberMessage)
		}
		m := s.putMember(&ch.members, u, in.Roles)
		out := s.memberEntity(m, u)
		out["@odata.context"] = "https://" + r.Host + "/v1.0/$metadata#teams('" + t.ID + "')/channels('" + ch.ID + "')/members/$entity"
		return out, nil
	}()
	if e != nil {
		s.fail(w, e)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// listChannelMembers serves GET /teams/{id}/channels/{cid}/members. A
// standard channel has no members of its own and lists the team's.
func (s *Server) listChannelMembers(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	t, ch, e := s.locate(r.PathValue("id"), r.PathValue("cid"))
	items := []map[string]any{}
	if e == nil {
		members := ch.members
		if ch.MembershipType == "standard" {
			members = t.members
		}
		for _, m := range members {
			items = append(items, map[string]any{
				"@odata.type": "#microsoft.graph.aadUserConversationMember",
				"id":          m.id, "userId": m.userID, "roles": append([]string{}, m.roles...),
			})
		}
	}
	s.mu.RUnlock()
	if e != nil {
		s.fail(w, e)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": items})
}

// --- channels ---------------------------------------------------------------

// reservedChannelNames may not be used as a channel name, in any case.
var reservedChannelNames = func() map[string]bool {
	m := map[string]bool{"forms": true, "con": true, "conin$": true, "conout$": true, "prn": true, "aux": true, "nul": true, "desktop.ini": true, "_vti_": true}
	for i := 1; i <= 9; i++ {
		m[fmt.Sprintf("com%d", i)] = true
		m[fmt.Sprintf("lpt%d", i)] = true
	}
	return m
}()

// checkChannelName applies Teams' channel naming rules and returns the
// reason a name is rejected, or "".
func checkChannelName(name string) string {
	n := utf8.RuneCountInString(name)
	switch {
	case n == 0:
		return "Channel name cannot be empty."
	case n > 50:
		return "Channel name cannot be longer than 50 characters."
	case strings.ContainsAny(name, "~#%&*{}+/\\:<>?|'\","):
		return "Channel name cannot contain any of the characters ~ # % & * { } + / \\ : < > ? | ' \" ,"
	case strings.Contains(name, ".."):
		return "Channel name cannot contain consecutive periods."
	case strings.HasPrefix(name, "_"), strings.HasPrefix(name, "."):
		return "Channel name cannot start with an underscore or a period."
	case strings.HasSuffix(name, "."):
		return "Channel name cannot end with a period."
	case reservedChannelNames[strings.ToLower(name)], strings.Contains(strings.ToLower(name), "_vti_"):
		return "Channel name is a reserved word and cannot be used."
	}
	return ""
}

func (s *Server) createChannel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DisplayName    string        `json:"displayName"`
		Description    string        `json:"description"`
		MembershipType string        `json:"membershipType"`
		Members        []memberInput `json:"members"`
		CreationMode   string        `json:"@microsoft.graph.channelCreationMode"`
		Created        *string       `json:"createdDateTime"`
	}
	if e := readJSON(w, r, &in, false); e != nil {
		s.fail(w, e)
		return
	}
	teamID := r.PathValue("id")
	now := s.cfg.now()
	app := record(r).AppID
	out, e := func() (map[string]any, *gErr) {
		s.mu.Lock()
		defer s.mu.Unlock()
		t := s.findTeam(teamID)
		if t == nil {
			return nil, notFound("No team found with Group Id %s.", teamID)
		}
		if why := checkChannelName(in.DisplayName); why != "" {
			return nil, badRequest("%s", why)
		}
		if in.CreationMode != "" && in.CreationMode != "migration" {
			return nil, badRequest("Invalid '@microsoft.graph.channelCreationMode' specified. Must be 'migration'.")
		}
		migration := in.CreationMode == "migration"
		created := now.Truncate(time.Millisecond)
		switch {
		case migration && in.MembershipType != "" && in.MembershipType != "standard":
			return nil, badRequest("Only standard channels can be created in migration mode; use startMigration for private and shared channels.")
		case migration:
			var e *gErr
			if created, e = migrationCreationTime(in.Created, now); e != nil {
				return nil, e
			}
		case in.Created != nil:
			return nil, badRequest("'createdDateTime' can only be set when creating a channel in migration mode.")
		}
		kind := in.MembershipType
		switch kind {
		case "":
			kind = "standard"
		case "standard", "private", "shared":
		default:
			return nil, badRequest("Invalid membershipType specified. Must be 'standard', 'private' or 'shared'.")
		}
		for _, ch := range t.channels {
			if strings.EqualFold(ch.DisplayName, in.DisplayName) {
				return nil, badRequest("Channel name already existed, please use other name.")
			}
		}
		// A private or shared channel created with application permissions
		// has nobody to fall back on as owner, so one must be named, and
		// only members of the team can be in such a channel. A standard
		// channel has no members of its own.
		var owner *User
		switch {
		case kind == "standard" && len(in.Members) > 0:
			return nil, badRequest("Members can only be specified when creating a private or shared channel.")
		case kind == "standard":
		case len(in.Members) != 1 || !slices.Contains(in.Members[0].Roles, "owner"):
			return nil, badRequest("A channel owner must be provided when creating a %s channel in application context: 'members' must hold exactly one member with the owner role.", kind)
		default:
			if owner = s.findUser(boundUserID(in.Members[0].Bind)); owner == nil {
				return nil, badRequest("Invalid bind property: the user referenced by 'user@odata.bind' was not found in the tenant.")
			}
			if teamMember(t, owner.ID) == nil {
				return nil, badRequest("%s", notTeamMemberMessage)
			}
		}
		ch := newChannelState(Channel{
			ID: s.nextID("19:%032x@thread.tacv2"), DisplayName: in.DisplayName,
			Description: in.Description, MembershipType: kind,
		}, created)
		if owner != nil {
			ch.members = []*memberState{{id: s.nextID("member-%06d"), userID: owner.ID, roles: []string{"owner"}}}
		}
		if migration {
			ch.migrationMode, ch.migrationOwner = migrationInProgress, app
		}
		t.channels = append(t.channels, ch)
		out := channelEntity(ch)
		out["@odata.context"] = "https://" + r.Host + "/v1.0/$metadata#teams('" + t.ID + "')/channels/$entity"
		return out, nil
	}()
	if e != nil {
		s.fail(w, e)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// findChannel must be called with s.mu held; t may be nil.
func findChannel(t *teamState, id string) *channelState {
	if t == nil {
		return nil
	}
	for _, ch := range t.channels {
		if ch.ID == id {
			return ch
		}
	}
	return nil
}

// locate resolves a team and channel, or explains which is missing. It must
// be called with s.mu held.
func (s *Server) locate(teamID, channelID string) (*teamState, *channelState, *gErr) {
	t := s.findTeam(teamID)
	if t == nil {
		return nil, nil, notFound("No team found with Group Id %s.", teamID)
	}
	ch := findChannel(t, channelID)
	if ch == nil {
		return nil, nil, notFound("No channel %s found in team %s.", channelID, teamID)
	}
	return t, ch, nil
}

func (s *Server) getChannel(w http.ResponseWriter, r *http.Request) {
	q, ok := s.checkQuery(w, r, "$select")
	if !ok {
		return
	}
	s.mu.RLock()
	t, ch, e := s.locate(r.PathValue("id"), r.PathValue("cid"))
	var entity map[string]any
	if e == nil {
		entity = channelEntity(ch)
	}
	s.mu.RUnlock()
	if e != nil {
		s.fail(w, e)
		return
	}
	out, err := project(entity, q.Get("$select"), channelDefaults, "channel")
	if err != nil {
		s.graphError(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	out["@odata.context"] = "https://" + r.Host + "/v1.0/$metadata#teams('" + t.ID + "')/channels/$entity"
	writeJSON(w, http.StatusOK, out)
}

// --- migration mode ---------------------------------------------------------

// Channel migration modes.
const (
	migrationInProgress = "inProgress"
	migrationCompleted  = "completed"
)

// migrationCreationTime reads the createdDateTime of a team or channel
// being created in migration mode: required, and not in the future.
func migrationCreationTime(raw *string, now time.Time) (time.Time, *gErr) {
	if raw == nil {
		return time.Time{}, badRequest("'createdDateTime' is required when creating in migration mode.")
	}
	t, err := time.Parse(time.RFC3339Nano, *raw)
	if err != nil {
		return time.Time{}, badRequest("Invalid createdDateTime: an ISO 8601 UTC timestamp is expected.")
	}
	t = t.UTC().Truncate(time.Millisecond)
	if t.After(now) {
		return time.Time{}, badRequest("createdDateTime must not be in the future.")
	}
	return t, nil
}

func (s *Server) startMigration(w http.ResponseWriter, r *http.Request) {
	if s.cfg.noStartMigration {
		// The national clouds do not have this API. What exactly they
		// answer is not documented; a 404 naming the segment is how Graph
		// answers for an action a deployment does not know.
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
		_, ch, e := s.locate(r.PathValue("id"), r.PathValue("cid"))
		if e != nil {
			return e
		}
		// Checked first, so repeating a startMigration whose response was
		// lost reports this rather than a creation-time error.
		if ch.migrationMode == migrationInProgress {
			return badRequest("The channel is already in migration mode.")
		}
		created := ch.createdAt
		if in.Created != nil {
			t, err := time.Parse(time.RFC3339Nano, *in.Created)
			if err != nil {
				return badRequest("Invalid conversationCreationDateTime: an ISO 8601 UTC timestamp is expected.")
			}
			t = t.UTC().Truncate(time.Millisecond)
			if !t.Before(ch.createdAt) {
				return badRequest("conversationCreationDateTime must be older than the channel's current createdDateTime (%s).", graphTime(ch.createdAt))
			}
			created = t
		}
		if ch.originalCreatedAt == nil {
			original := ch.createdAt
			ch.originalCreatedAt = &original
		}
		ch.createdAt = created
		ch.migrationMode, ch.migrationOwner = migrationInProgress, app
		return nil
	}()
	if e != nil {
		s.fail(w, e)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) completeMigration(w http.ResponseWriter, r *http.Request) {
	app := record(r).AppID
	e := func() *gErr {
		s.mu.Lock()
		defer s.mu.Unlock()
		_, ch, e := s.locate(r.PathValue("id"), r.PathValue("cid"))
		switch {
		case e != nil:
			return e
		case ch.migrationMode != migrationInProgress:
			return badRequest("The channel is not in migration mode.")
		case ch.migrationOwner != app:
			return forbidden("%s", notOwnerMessage)
		}
		ch.migrationMode, ch.migrationOwner = migrationCompleted, ""
		return nil
	}()
	if e != nil {
		s.fail(w, e)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// completeTeamMigration serves POST /teams/{id}/completeMigration: the end
// of a team created in migration mode. Every channel must have completed
// its own migration first.
func (s *Server) completeTeamMigration(w http.ResponseWriter, r *http.Request) {
	app := record(r).AppID
	teamID := r.PathValue("id")
	e := func() *gErr {
		s.mu.Lock()
		defer s.mu.Unlock()
		t := s.findTeam(teamID)
		switch {
		case t == nil:
			return notFound("No team found with Group Id %s.", teamID)
		case t.migrationMode != migrationInProgress:
			return badRequest("The team is not in migration mode.")
		case t.migrationOwner != app:
			return forbidden("%s", notOwnerMessage)
		}
		for _, ch := range t.channels {
			if ch.migrationMode == migrationInProgress {
				return badRequest("Channel %s of the team is still in migration mode; complete the migration of every channel before the team's.", ch.ID)
			}
		}
		t.migrationMode, t.migrationOwner = migrationCompleted, ""
		return nil
	}()
	if e != nil {
		s.fail(w, e)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

const notOwnerMessage = "The migration of this channel was started by a different application; only that application can import into it or complete it."

// listTeamMembers serves GET /teams/{id}/members.
func (s *Server) listTeamMembers(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.RLock()
	t := s.findTeam(id)
	items := []map[string]any{}
	if t != nil {
		for _, m := range t.members {
			items = append(items, map[string]any{
				"@odata.type": "#microsoft.graph.aadUserConversationMember",
				"id":          m.id, "userId": m.userID, "roles": append([]string{}, m.roles...),
			})
		}
	}
	s.mu.RUnlock()
	if t == nil {
		s.teamNotFound(w, id)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": items})
}
