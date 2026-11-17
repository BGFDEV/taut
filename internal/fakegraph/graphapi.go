package fakegraph

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Application permissions that authorize each read endpoint, mirroring the
// permission tables in the Graph reference. Any one is sufficient.
var (
	rolesOrganization = []string{"Organization.Read.All", "Organization.ReadWrite.All", "Directory.Read.All", "Directory.ReadWrite.All"}
	rolesUsers        = []string{"User.Read.All", "User.ReadWrite.All", "Directory.Read.All", "Directory.ReadWrite.All"}
	rolesTeams        = []string{"Team.ReadBasic.All", "TeamSettings.Read.All", "TeamSettings.ReadWrite.All", "Group.Read.All", "Group.ReadWrite.All", "Directory.Read.All", "Directory.ReadWrite.All"}
	rolesChannels     = []string{"Channel.ReadBasic.All", "ChannelSettings.Read.All", "ChannelSettings.ReadWrite.All", "Group.Read.All", "Group.ReadWrite.All", "Directory.Read.All", "Directory.ReadWrite.All"}
)

// Roles for the write path and for single-item reads. Teamwork.Migrate.All
// is accepted wherever the Graph reference lists it; it does not authorize
// the list endpoints above.
var (
	rolesTeamGet       = append([]string{"Teamwork.Migrate.All"}, rolesTeams...)
	rolesChannelGet    = append([]string{"Teamwork.Migrate.All"}, rolesChannels...)
	rolesTeamCreate    = []string{"Team.Create", "Teamwork.Migrate.All", "Group.ReadWrite.All", "Directory.ReadWrite.All"}
	rolesTeamOperation = append([]string{"Team.Create"}, rolesTeamGet...)
	rolesChannelCreate = []string{"Channel.Create", "Teamwork.Migrate.All", "Group.ReadWrite.All", "Directory.ReadWrite.All"}
	rolesMigrate       = []string{"Teamwork.Migrate.All"}
	rolesMessageRead   = []string{"ChannelMessage.Read.All", "Group.Read.All", "Group.ReadWrite.All"}
	rolesMemberAdd     = []string{"TeamMember.ReadWrite.All"}
	rolesChannelMember = []string{"ChannelMember.ReadWrite.All"}
	rolesChannelRoster = []string{"ChannelMember.Read.All", "ChannelMember.ReadWrite.All"}
)

func (s *Server) routes() {
	g := http.NewServeMux()
	g.HandleFunc("GET /v1.0/organization", s.authorized(rolesOrganization, s.getOrganization))
	g.HandleFunc("GET /v1.0/users", s.authorized(rolesUsers, s.listUsers))
	g.HandleFunc("GET /v1.0/users/{key}", s.authorized(rolesUsers, s.getUser))
	g.HandleFunc("GET /v1.0/teams", s.authorized(rolesTeams, s.listTeams))
	g.HandleFunc("POST /v1.0/teams", s.authorized(rolesTeamCreate, s.createTeam))
	g.HandleFunc("GET /v1.0/teams/{id}", s.authorized(rolesTeamGet, s.getTeam))
	g.HandleFunc("GET /v1.0/teams/{id}/operations/{op}", s.authorized(rolesTeamOperation, s.getTeamOperation))
	g.HandleFunc("POST /v1.0/teams/{id}/members", s.authorized(rolesMemberAdd, s.addTeamMember))
	g.HandleFunc("GET /v1.0/teams/{id}/members", s.authorized(rolesMemberAdd, s.listTeamMembers))
	g.HandleFunc("POST /v1.0/teams/{id}/members/add", s.authorized(rolesMemberAdd, s.addTeamMembers))
	g.HandleFunc("POST /v1.0/teams/{id}/completeMigration", s.authorized(rolesMigrate, s.completeTeamMigration))
	g.HandleFunc("GET /v1.0/teams/{id}/channels/{cid}/members", s.authorized(rolesChannelRoster, s.listChannelMembers))
	g.HandleFunc("POST /v1.0/teams/{id}/channels/{cid}/members", s.authorized(rolesChannelMember, s.addChannelMember))
	g.HandleFunc("GET /v1.0/teams/{id}/channels", s.authorized(rolesChannels, s.listChannels))
	g.HandleFunc("POST /v1.0/teams/{id}/channels", s.authorized(rolesChannelCreate, s.createChannel))
	g.HandleFunc("GET /v1.0/teams/{id}/channels/{cid}", s.authorized(rolesChannelGet, s.getChannel))
	g.HandleFunc("GET /v1.0/teams/{id}/channels/{cid}/filesFolder", s.authorized(rolesFilesRead, s.getFilesFolder))
	g.HandleFunc("GET /v1.0/drives/{drive}/items/{rest...}", s.authorized(rolesFilesRead, s.getDriveItem))
	g.HandleFunc("POST /v1.0/drives/{drive}/items/{rest...}", s.authorized(rolesFilesWrite, s.postDriveItem))
	g.HandleFunc("PUT /v1.0/drives/{drive}/items/{rest...}", s.authorized(rolesFilesWrite, s.putDriveContent))
	g.HandleFunc("POST /v1.0/teams/{id}/channels/{cid}/startMigration", s.authorized(rolesMigrate, s.startMigration))
	g.HandleFunc("POST /v1.0/teams/{id}/channels/{cid}/completeMigration", s.authorized(rolesMigrate, s.completeMigration))
	g.HandleFunc("POST /v1.0/teams/{id}/channels/{cid}/messages", s.authorized(rolesMigrate, s.postMessage))
	g.HandleFunc("GET /v1.0/teams/{id}/channels/{cid}/messages", s.authorized(rolesMessageRead, s.listMessages))
	g.HandleFunc("GET /v1.0/teams/{id}/channels/{cid}/messages/{mid}", s.authorized(rolesMessageRead, s.getMessage))
	g.HandleFunc("POST /v1.0/teams/{id}/channels/{cid}/messages/{mid}/replies", s.authorized(rolesMigrate, s.postMessage))
	g.HandleFunc("GET /v1.0/teams/{id}/channels/{cid}/messages/{mid}/replies", s.authorized(rolesMessageRead, s.listMessages))
	g.HandleFunc("GET /v1.0/teams/{id}/channels/{cid}/messages/{mid}/replies/{rid}", s.authorized(rolesMessageRead, s.getMessage))
	s.chatRoutes(g)
	g.HandleFunc("/v1.0/", s.authorized(nil, func(w http.ResponseWriter, r *http.Request) {
		s.graphError(w, http.StatusNotImplemented, "fakegraph_NotImplemented",
			fmt.Sprintf("fakegraph does not implement %s %s.", r.Method, r.URL.Path))
	}))
	s.graph = g
	s.adminRoutes()
}

func (s *Server) graphError(w http.ResponseWriter, status int, code, message string) {
	writeGraphError(w, s.cfg.now(), status, code, message)
}

// authorized wraps a Graph handler with the checks Graph makes before any
// work: a valid, unexpired bearer token for this tenant and audience; the
// throttling budget; and one of the listed application roles.
func (s *Server) authorized(anyOf []string, next http.HandlerFunc) http.HandlerFunc {
	unauthorized := func(w http.ResponseWriter, message string) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="", authorization_uri="https://login.microsoftonline.com/common/oauth2/authorize", `+
			`client_id="00000003-0000-0000-c000-000000000000", error="invalid_token", error_description="`+message+`"`)
		s.graphError(w, http.StatusUnauthorized, "InvalidAuthenticationToken", message)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			unauthorized(w, "Access token is empty.")
			return
		}
		claims, err := verifyAccessToken(s.signingKey, token)
		if err != nil {
			unauthorized(w, "Access token validation failure.")
			return
		}
		record(r).AppID = claims.AppID
		now := s.cfg.now()
		if now.Unix() >= claims.Exp {
			unauthorized(w, "Lifetime validation failed, the token is expired.")
			return
		}
		s.mu.RLock()
		tenantID := s.st.tenantID
		s.mu.RUnlock()
		if claims.Tid != tenantID {
			unauthorized(w, "Access token validation failure. Invalid tenant.")
			return
		}
		if aud, err := url.Parse(claims.Aud); !s.cfg.laxAudience && (err != nil || !strings.EqualFold(aud.Host, r.Host)) {
			unauthorized(w, "Access token validation failure. Invalid audience.")
			return
		}
		if ok, wait := s.limiter.allow(tenantAppKey(claims.Tid, claims.AppID), s.cfg.rps, s.cfg.burst, now); !ok {
			s.throttled(w, wait)
			return
		}
		if len(anyOf) > 0 && !slices.ContainsFunc(anyOf, func(role string) bool { return slices.Contains(claims.Roles, role) }) {
			s.graphError(w, http.StatusForbidden, "Authorization_RequestDenied", "Insufficient privileges to complete the operation.")
			return
		}
		next(w, r)
	}
}

// checkQuery rejects query options the endpoint does not support.
func (s *Server) checkQuery(w http.ResponseWriter, r *http.Request, allowed ...string) (url.Values, bool) {
	q := r.URL.Query()
	for k := range q {
		if !slices.Contains(allowed, k) {
			s.graphError(w, http.StatusBadRequest, "BadRequest",
				fmt.Sprintf("Query option '%s' is not allowed (or not implemented by fakegraph) on this resource.", k))
			return nil, false
		}
	}
	return q, true
}

// project applies $select to an entity. Unknown properties are a 400, as in
// Graph.
func project(entity map[string]any, sel string, defaults []string, typeName string) (map[string]any, error) {
	fields := defaults
	if strings.TrimSpace(sel) != "" {
		fields = strings.Split(sel, ",")
	}
	out := make(map[string]any, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		v, ok := entity[f]
		if !ok {
			return nil, fmt.Errorf("Could not find a property named '%s' on type 'microsoft.graph.%s'.", f, typeName) //nolint:staticcheck // mirrors Graph's wording
		}
		out[f] = v
	}
	return out, nil
}

// window resolves $top and $skiptoken against a collection of total items
// and returns the slice bounds plus the next link, if any.
func (s *Server) window(r *http.Request, q url.Values, total int) (start, end int, next string, err error) {
	return s.windowN(r, q, total, s.cfg.pageSize, 999)
}

// windowN is window with an explicit default page size and $top ceiling.
func (s *Server) windowN(r *http.Request, q url.Values, total, size, maxTop int) (start, end int, next string, err error) {
	if raw := q.Get("$top"); raw != "" {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil || n < 1 || n > maxTop {
			return 0, 0, "", fmt.Errorf("Invalid page size specified: '%s'. Must be between 1 and %d inclusive.", raw, maxTop) //nolint:staticcheck // mirrors Graph's wording
		}
		size = n
	}
	if raw := q.Get("$skiptoken"); raw != "" {
		start, err = decodeSkipToken(raw)
		if err != nil || start > total {
			return 0, 0, "", errors.New("Invalid skip token.") //nolint:staticcheck // mirrors Graph's wording
		}
	}
	end = min(start+size, total)
	if end < total {
		nq := url.Values{}
		for k, v := range q {
			nq[k] = v
		}
		nq.Set("$skiptoken", encodeSkipToken(end))
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		next = scheme + "://" + r.Host + r.URL.EscapedPath() + "?" + nq.Encode()
	}
	return start, end, next, nil
}

// Skip tokens are opaque to clients; tests should not construct them.
func encodeSkipToken(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("fg1:" + strconv.Itoa(offset)))
}

func decodeSkipToken(tok string) (int, error) {
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return 0, err
	}
	rest, ok := strings.CutPrefix(string(raw), "fg1:")
	if !ok {
		return 0, errors.New("bad prefix")
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 0 {
		return 0, errors.New("bad offset")
	}
	return n, nil
}

func (s *Server) writeCollection(w http.ResponseWriter, r *http.Request, entity string, items []map[string]any, next string) {
	body := map[string]any{
		"@odata.context": "https://" + r.Host + "/v1.0/$metadata#" + entity,
		"value":          items,
	}
	if next != "" {
		body["@odata.nextLink"] = next
	}
	writeJSON(w, http.StatusOK, body)
}

// --- organization -----------------------------------------------------------

func (s *Server) getOrganization(w http.ResponseWriter, r *http.Request) {
	q, ok := s.checkQuery(w, r, "$select")
	if !ok {
		return
	}
	s.mu.RLock()
	domains := make([]map[string]any, 0, len(s.st.org.Domains))
	for _, d := range s.st.org.Domains {
		domains = append(domains, map[string]any{
			"capabilities": "Email, OfficeCommunicationsOnline", "type": "Managed",
			"name": d.Name, "isDefault": d.IsDefault, "isInitial": d.IsInitial,
		})
	}
	org := map[string]any{"id": s.st.tenantID, "displayName": s.st.org.DisplayName, "verifiedDomains": domains}
	s.mu.RUnlock()

	out, err := project(org, q.Get("$select"), []string{"id", "displayName", "verifiedDomains"}, "organization")
	if err != nil {
		s.graphError(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	s.writeCollection(w, r, "organization", []map[string]any{out}, "")
}

// --- users ------------------------------------------------------------------

// userDefaults are the properties Graph returns without $select, limited
// to those the fake models. accountEnabled, userType and proxyAddresses
// must be selected explicitly, exactly as on the real service.
var userDefaults = []string{"id", "displayName", "mail", "userPrincipalName"}

func userEntity(u *User) map[string]any {
	var mail any
	if u.Mail != "" {
		mail = u.Mail
	}
	proxies := u.ProxyAddresses
	if proxies == nil {
		proxies = []string{}
	}
	return map[string]any{
		"id": u.ID, "displayName": u.DisplayName, "mail": mail, "userPrincipalName": u.UserPrincipalName,
		"accountEnabled": u.AccountEnabled, "userType": u.UserType, "proxyAddresses": proxies,
	}
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	q, ok := s.checkQuery(w, r, "$select", "$top", "$filter", "$skiptoken")
	if !ok {
		return
	}
	clauses, err := parseUserFilter(q.Get("$filter"))
	if err != nil {
		s.graphError(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}

	s.mu.RLock()
	var matched []map[string]any
	for _, u := range s.st.users {
		if matchUser(u, clauses) {
			matched = append(matched, userEntity(u))
		}
	}
	s.mu.RUnlock()

	start, end, next, err := s.window(r, q, len(matched))
	if err != nil {
		s.graphError(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	items := make([]map[string]any, 0, end-start)
	for _, e := range matched[start:end] {
		out, err := project(e, q.Get("$select"), userDefaults, "user")
		if err != nil {
			s.graphError(w, http.StatusBadRequest, "BadRequest", err.Error())
			return
		}
		items = append(items, out)
	}
	s.writeCollection(w, r, "users", items, next)
}

func (s *Server) getUser(w http.ResponseWriter, r *http.Request) {
	q, ok := s.checkQuery(w, r, "$select")
	if !ok {
		return
	}
	key := r.PathValue("key")
	var found map[string]any
	s.mu.RLock()
	for _, u := range s.st.users {
		if strings.EqualFold(u.ID, key) || strings.EqualFold(u.UserPrincipalName, key) {
			found = userEntity(u)
			break
		}
	}
	s.mu.RUnlock()
	if found == nil {
		s.graphError(w, http.StatusNotFound, "Request_ResourceNotFound",
			fmt.Sprintf("Resource '%s' does not exist or one of its queried reference-property objects are not present.", key))
		return
	}
	out, err := project(found, q.Get("$select"), userDefaults, "user")
	if err != nil {
		s.graphError(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	out["@odata.context"] = "https://" + r.Host + "/v1.0/$metadata#users/$entity"
	writeJSON(w, http.StatusOK, out)
}

// filterClause is one `property eq literal` term.
type filterClause struct {
	field string
	str   string
	b     bool
	isStr bool
}

var (
	clauseRE = regexp.MustCompile(`(?i)^\s*([A-Za-z]+)\s+eq\s+(.+?)\s*$`)
	andRE    = regexp.MustCompile(`(?i)\s+and\s+`)
)

// parseUserFilter supports conjunctions of `eq` comparisons on a few user
// properties. Everything else is rejected with Graph-style wording.
func parseUserFilter(expr string) ([]filterClause, error) {
	if strings.TrimSpace(expr) == "" {
		return nil, nil
	}
	var out []filterClause
	for _, term := range splitOutsideQuotes(expr) {
		m := clauseRE.FindStringSubmatch(term)
		if m == nil {
			return nil, fmt.Errorf("Invalid filter clause: '%s' is not supported; fakegraph understands only `property eq value` joined by `and`.", strings.TrimSpace(term)) //nolint:staticcheck // mirrors Graph's wording
		}
		c := filterClause{field: m[1]}
		lit := m[2]
		switch {
		case len(lit) >= 2 && lit[0] == '\'' && lit[len(lit)-1] == '\'':
			inner := lit[1 : len(lit)-1]
			if strings.Contains(strings.ReplaceAll(inner, "''", ""), "'") {
				return nil, fmt.Errorf("Invalid filter clause: unbalanced quote in %s.", lit) //nolint:staticcheck // mirrors Graph's wording
			}
			c.isStr, c.str = true, strings.ReplaceAll(inner, "''", "'")
		case lit == "true" || lit == "false":
			c.b = lit == "true"
		default:
			return nil, fmt.Errorf("Invalid filter clause: unrecognized literal %s.", lit) //nolint:staticcheck // mirrors Graph's wording
		}
		switch c.field {
		case "mail", "userPrincipalName", "id", "displayName", "userType":
			if !c.isStr {
				return nil, fmt.Errorf("Invalid filter clause: a binary operator with incompatible types was detected for property '%s'.", c.field) //nolint:staticcheck // mirrors Graph's wording
			}
		case "accountEnabled":
			if c.isStr {
				return nil, errors.New("Invalid filter clause: a binary operator with incompatible types was detected for property 'accountEnabled'.") //nolint:staticcheck // mirrors Graph's wording
			}
		default:
			return nil, fmt.Errorf("Unsupported or invalid query filter clause specified for property '%s' of resource 'User'.", c.field) //nolint:staticcheck // mirrors Graph's wording
		}
		out = append(out, c)
	}
	return out, nil
}

// splitOutsideQuotes splits on the word `and`, ignoring quoted strings.
func splitOutsideQuotes(expr string) []string {
	// Mask quoted sections so the separator regexp cannot match inside them.
	masked := []byte(expr)
	in := false
	for i, ch := range masked {
		if ch == '\'' {
			in = !in
			continue
		}
		if in {
			masked[i] = 'x'
		}
	}
	var parts []string
	prev := 0
	for _, loc := range andRE.FindAllIndex(masked, -1) {
		parts = append(parts, expr[prev:loc[0]])
		prev = loc[1]
	}
	return append(parts, expr[prev:])
}

func matchUser(u *User, clauses []filterClause) bool {
	for _, c := range clauses {
		var ok bool
		switch c.field {
		case "mail":
			ok = u.Mail != "" && strings.EqualFold(u.Mail, c.str)
		case "userPrincipalName":
			ok = strings.EqualFold(u.UserPrincipalName, c.str)
		case "id":
			ok = strings.EqualFold(u.ID, c.str)
		case "displayName":
			ok = strings.EqualFold(u.DisplayName, c.str)
		case "userType":
			ok = strings.EqualFold(u.UserType, c.str)
		case "accountEnabled":
			ok = u.AccountEnabled == c.b
		}
		if !ok {
			return false
		}
	}
	return true
}

// parseDisplayNameFilter reads the only $filter the team and channel lists
// support: `displayName eq '<literal>'`. has is false when expr is empty.
func parseDisplayNameFilter(expr, resource string) (name string, has bool, err error) {
	if strings.TrimSpace(expr) == "" {
		return "", false, nil
	}
	terms := splitOutsideQuotes(expr)
	m := clauseRE.FindStringSubmatch(terms[0])
	if len(terms) != 1 || m == nil {
		return "", false, fmt.Errorf("Invalid filter clause: fakegraph understands only `displayName eq 'value'` on resource '%s'.", resource) //nolint:staticcheck // mirrors Graph's wording
	}
	if m[1] != "displayName" {
		return "", false, fmt.Errorf("Unsupported or invalid query filter clause specified for property '%s' of resource '%s'.", m[1], resource) //nolint:staticcheck // mirrors Graph's wording
	}
	lit := m[2]
	if len(lit) < 2 || lit[0] != '\'' || lit[len(lit)-1] != '\'' {
		return "", false, fmt.Errorf("Invalid filter clause: unrecognized literal %s.", lit) //nolint:staticcheck // mirrors Graph's wording
	}
	inner := lit[1 : len(lit)-1]
	if strings.Contains(strings.ReplaceAll(inner, "''", ""), "'") {
		return "", false, fmt.Errorf("Invalid filter clause: unbalanced quote in %s.", lit) //nolint:staticcheck // mirrors Graph's wording
	}
	return strings.ReplaceAll(inner, "''", "'"), true, nil
}

// --- teams ------------------------------------------------------------------

var teamDefaults = []string{"id", "displayName", "description"}

// teamGetDefaults are the properties of a single team read.
var teamGetDefaults = []string{"id", "displayName", "description", "visibility"}

func teamEntity(t *teamState) map[string]any {
	var desc any
	if t.Description != "" {
		desc = t.Description
	}
	return map[string]any{"id": t.ID, "displayName": t.DisplayName, "description": desc, "visibility": t.Visibility}
}

func (s *Server) listTeams(w http.ResponseWriter, r *http.Request) {
	q, ok := s.checkQuery(w, r, "$select", "$top", "$skiptoken", "$filter")
	if !ok {
		return
	}
	name, filtered, err := parseDisplayNameFilter(q.Get("$filter"), "Team")
	if err != nil {
		s.graphError(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	now := s.cfg.now()
	s.mu.RLock()
	all := make([]map[string]any, 0, len(s.st.teams))
	for _, t := range s.st.teams {
		if s.provisioning(t, now) != opSucceeded || (filtered && !strings.EqualFold(t.DisplayName, name)) {
			continue
		}
		all = append(all, teamEntity(t))
	}
	s.mu.RUnlock()

	start, end, next, err := s.window(r, q, len(all))
	if err != nil {
		s.graphError(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	items := make([]map[string]any, 0, end-start)
	for _, e := range all[start:end] {
		out, err := project(e, q.Get("$select"), teamDefaults, "team")
		if err != nil {
			s.graphError(w, http.StatusBadRequest, "BadRequest", err.Error())
			return
		}
		items = append(items, out)
	}
	s.writeCollection(w, r, "teams", items, next)
}

// findTeam returns the team with this ID if it is visible, that is, fully
// provisioned. It must be called with s.mu held.
func (s *Server) findTeam(id string) *teamState {
	if t := s.findAnyTeam(id); t != nil && s.provisioning(t, s.cfg.now()) == opSucceeded {
		return t
	}
	return nil
}

// findAnyTeam also returns teams that are still provisioning or failed. It
// must be called with s.mu held.
func (s *Server) findAnyTeam(id string) *teamState {
	for _, t := range s.st.teams {
		if strings.EqualFold(t.ID, id) {
			return t
		}
	}
	return nil
}

func (s *Server) teamNotFound(w http.ResponseWriter, id string) {
	s.graphError(w, http.StatusNotFound, "NotFound", fmt.Sprintf("No team found with Group Id %s.", id))
}

func (s *Server) getTeam(w http.ResponseWriter, r *http.Request) {
	q, ok := s.checkQuery(w, r, "$select")
	if !ok {
		return
	}
	id := r.PathValue("id")
	s.mu.RLock()
	var entity map[string]any
	if t := s.findTeam(id); t != nil {
		entity = teamEntity(t)
	}
	s.mu.RUnlock()
	if entity == nil {
		s.teamNotFound(w, id)
		return
	}
	out, err := project(entity, q.Get("$select"), teamGetDefaults, "team")
	if err != nil {
		s.graphError(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	out["@odata.context"] = "https://" + r.Host + "/v1.0/$metadata#teams/$entity"
	writeJSON(w, http.StatusOK, out)
}

// --- channels ---------------------------------------------------------------

var channelDefaults = []string{"id", "displayName", "description", "membershipType", "createdDateTime", "migrationMode", "originalCreatedDateTime"}

func channelEntity(ch *channelState) map[string]any {
	var desc, mode, original any
	if ch.Description != "" {
		desc = ch.Description
	}
	if ch.migrationMode != "" {
		mode = ch.migrationMode
	}
	if ch.originalCreatedAt != nil {
		original = graphTime(*ch.originalCreatedAt)
	}
	return map[string]any{
		"id": ch.ID, "displayName": ch.DisplayName, "description": desc, "membershipType": ch.MembershipType,
		"createdDateTime": graphTime(ch.createdAt), "migrationMode": mode, "originalCreatedDateTime": original,
	}
}

func (s *Server) listChannels(w http.ResponseWriter, r *http.Request) {
	q, ok := s.checkQuery(w, r, "$select", "$filter")
	if !ok {
		return
	}
	name, filtered, err := parseDisplayNameFilter(q.Get("$filter"), "Channel")
	if err != nil {
		s.graphError(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	id := r.PathValue("id")
	s.mu.RLock()
	t := s.findTeam(id)
	var all []map[string]any
	if t != nil {
		for _, ch := range t.channels {
			if filtered && !strings.EqualFold(ch.DisplayName, name) {
				continue
			}
			all = append(all, channelEntity(ch))
		}
	}
	s.mu.RUnlock()
	if t == nil {
		s.teamNotFound(w, id)
		return
	}
	items := make([]map[string]any, 0, len(all))
	for _, e := range all {
		out, err := project(e, q.Get("$select"), channelDefaults, "channel")
		if err != nil {
			s.graphError(w, http.StatusBadRequest, "BadRequest", err.Error())
			return
		}
		items = append(items, out)
	}
	s.writeCollection(w, r, "teams('"+id+"')/channels", items, "")
}
