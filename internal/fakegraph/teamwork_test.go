package fakegraph

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	testTeam    = "b0000000-0000-4000-8000-000000000001"
	testChannel = "19:general-eng@thread.tacv2"
	testUser    = "a0000000-0000-4000-8000-000000000001"
	chanPath    = "/v1.0/teams/" + testTeam + "/channels/" + testChannel
)

func (h *harness) post(path, token, body string) (*http.Response, map[string]any) {
	h.t.Helper()
	return h.do("POST", path, token, strings.NewReader(body))
}

func importBody(when, extra string) string {
	return fmt.Sprintf(`{"createdDateTime":%q,"from":{"user":{"id":%q,"displayName":"Ada","userIdentityType":"aadUser"}},"body":{"contentType":"html","content":"hello"}%s}`,
		when, testUser, extra)
}

func TestCreateTeamWire(t *testing.T) {
	h := newHarness(t, DefaultSeed(), WithTeamProvisioningPolls(1))
	token := h.token()
	team := func(members string) string {
		return `{"template@odata.bind":"https://graph.microsoft.com/v1.0/teamsTemplates('standard')","displayName":"Platform","visibility":"private","members":[` + members + `]}`
	}
	member := func(role, user string) string {
		return fmt.Sprintf(`{"@odata.type":"#microsoft.graph.aadUserConversationMember","roles":[%s],"user@odata.bind":"https://graph.microsoft.com/v1.0/users('%s')"}`, role, user)
	}
	owner := member(`"owner"`, testUser)
	for name, body := range map[string]string{
		"no owner":      team(""),
		"two owners":    team(owner + "," + owner),
		"member only":   team(member("", testUser)),
		"unknown owner": team(member(`"owner"`, "ghost")),
		"no template":   `{"displayName":"Platform","members":[` + owner + `]}`,
		"no name":       strings.Replace(team(owner), `"Platform"`, `""`, 1),
		"bad json":      `{"displayName":`,
		"empty":         ``,
	} {
		if resp, body := h.post("/v1.0/teams", token, body); resp.StatusCode != 400 || graphCode(body) != "BadRequest" {
			t.Errorf("%s: %d %v", name, resp.StatusCode, body)
		}
	}
	if len(h.s.Teams()) != 2 {
		t.Fatalf("rejected requests created teams: %+v", h.s.Teams())
	}

	resp, body := h.post("/v1.0/teams", token, team(owner))
	loc, cloc := resp.Header.Get("Location"), resp.Header.Get("Content-Location")
	if resp.StatusCode != 202 || body != nil || !strings.HasPrefix(cloc, "/teams('") || !strings.HasPrefix(loc, cloc+"/operations('") {
		t.Fatalf("create team: %d %v Location=%q Content-Location=%q", resp.StatusCode, body, loc, cloc)
	}
	teamID := strings.TrimSuffix(strings.TrimPrefix(cloc, "/teams('"), "')")
	opID := strings.TrimSuffix(loc[strings.Index(loc, "operations('")+len("operations('"):], "')")
	opPath := "/v1.0/teams/" + teamID + "/operations/" + opID

	if resp, _ := h.do("GET", "/v1.0/teams/"+teamID, token, nil); resp.StatusCode != 404 {
		t.Errorf("team visible while provisioning: %d", resp.StatusCode)
	}
	if resp, _ := h.do("GET", "/v1.0/teams/"+teamID+"/channels", token, nil); resp.StatusCode != 404 {
		t.Errorf("channels visible while provisioning: %d", resp.StatusCode)
	}
	if _, body := h.do("GET", opPath, token, nil); body["status"] != "inProgress" || body["operationType"] != "createTeam" || body["targetResourceId"] != teamID || body["id"] != opID {
		t.Errorf("first poll: %v", body)
	}
	if _, body := h.do("GET", opPath, token, nil); body["status"] != "succeeded" {
		t.Errorf("second poll: %v", body)
	}
	if resp, _ := h.do("GET", "/v1.0/teams/"+teamID+"/operations/nope", token, nil); resp.StatusCode != 404 {
		t.Errorf("unknown operation: %d", resp.StatusCode)
	}
	if resp, body := h.do("GET", "/v1.0/teams/"+teamID, token, nil); resp.StatusCode != 200 || body["visibility"] != "private" || body["displayName"] != "Platform" {
		t.Errorf("team: %d %v", resp.StatusCode, body)
	}
	_, body = h.do("GET", "/v1.0/teams/"+teamID+"/channels", token, nil)
	chans := body["value"].([]any)
	if len(chans) != 1 || chans[0].(map[string]any)["displayName"] != "General" || chans[0].(map[string]any)["migrationMode"] != nil {
		t.Errorf("new team channels: %v", chans)
	}

	// Filters.
	q := func(filter string) string { return "?$filter=" + url.QueryEscape(filter) }
	if _, body := h.do("GET", "/v1.0/teams"+q("displayName eq 'platform'"), token, nil); len(body["value"].([]any)) != 1 {
		t.Errorf("team filter: %v", body)
	}
	if _, body := h.do("GET", "/v1.0/teams/"+testTeam+"/channels"+q("displayName eq 'ON-CALL'"), token, nil); len(body["value"].([]any)) != 1 {
		t.Errorf("channel filter: %v", body)
	}
	for _, bad := range []string{"displayName ne 'x'", "description eq 'x'", "displayName eq x", "displayName eq 'a' and displayName eq 'b'", "displayName eq 'it's'"} {
		if resp, _ := h.do("GET", "/v1.0/teams"+q(bad), token, nil); resp.StatusCode != 400 {
			t.Errorf("filter %q: %d, want 400", bad, resp.StatusCode)
		}
	}
}

func TestImportWire(t *testing.T) {
	h := newHarness(t, DefaultSeed(), WithPageSize(100))
	token := h.token()
	msgs := chanPath + "/messages"

	// Not in migration mode: nothing can be imported, and apps cannot send.
	if resp, body := h.post(msgs, token, importBody("2021-01-01T00:00:00Z", "")); resp.StatusCode != 403 || graphCode(body) != "Forbidden" {
		t.Errorf("import outside migration mode: %d %v", resp.StatusCode, body)
	}
	for name, body := range map[string]string{"garbage": `{"conversationCreationDateTime":"yesterday"}`, "bad json": `[`} {
		if resp, _ := h.post(chanPath+"/startMigration", token, body); resp.StatusCode != 400 {
			t.Errorf("startMigration %s: %d", name, resp.StatusCode)
		}
	}
	if resp, _ := h.post(chanPath+"/completeMigration", token, ""); resp.StatusCode != 400 {
		t.Errorf("completeMigration outside migration mode: %d", resp.StatusCode)
	}
	if resp, _ := h.post(chanPath+"/startMigration", token, `{"conversationCreationDateTime":"2015-01-01T00:00:00Z"}`); resp.StatusCode != 204 {
		t.Fatalf("startMigration: %d", resp.StatusCode)
	}
	if resp, body := h.post(chanPath+"/startMigration", token, ""); resp.StatusCode != 400 || !strings.Contains(fmt.Sprint(body), "already in migration mode") {
		t.Errorf("second startMigration: %d %v", resp.StatusCode, body)
	}
	_, body := h.do("GET", chanPath, token, nil)
	if body["migrationMode"] != "inProgress" || body["createdDateTime"] != "2015-01-01T00:00:00.000Z" || body["originalCreatedDateTime"] != "2020-01-01T00:00:00.000Z" {
		t.Errorf("channel: %v", body)
	}

	if resp, body := h.post(msgs, token, `{"body":{"content":"hi"}}`); resp.StatusCode != 403 || graphCode(body) != "Forbidden" {
		t.Errorf("normal send: %d %v", resp.StatusCode, body)
	}
	const when = "2019-02-04T19:58:15.511Z"
	for name, body := range map[string]string{
		"no from":          `{"createdDateTime":"` + when + `","body":{"content":"hi"}}`,
		"no created":       `{"from":{"user":{"id":"` + testUser + `"}},"body":{"content":"hi"}}`,
		"from null user":   `{"createdDateTime":"` + when + `","from":{"user":null},"body":{"content":"hi"}}`,
		"from without id":  `{"createdDateTime":"` + when + `","from":{"user":{"displayName":"x"}},"body":{"content":"hi"}}`,
		"bad time":         importBody("last tuesday", ""),
		"bad content type": strings.Replace(importBody(when, ""), `"html"`, `"markdown"`, 1),
		"no body":          `{"createdDateTime":"` + when + `","from":{"user":{"id":"` + testUser + `"}}}`,
		"mention no id":    strings.Replace(importBody(when, `,"mentions":[{"mentionText":"x","mentioned":{"user":{"id":"u"}}}]`), "hello", `<at id=\"0\">x</at>`, 1),
		"mention no user":  strings.Replace(importBody(when, `,"mentions":[{"id":0,"mentionText":"x"}]`), "hello", `<at id=\"0\">x</at>`, 1),
		"mention twice":    strings.Replace(importBody(when, `,"mentions":[{"id":0,"mentioned":{"user":{"id":"u"}}},{"id":0,"mentioned":{"user":{"id":"u"}}}]`), "hello", `<at id=\"0\">x</at>`, 1),
		"bad json":         `{"createdDateTime":`,
	} {
		if resp, body := h.post(msgs, token, body); resp.StatusCode != 400 || graphCode(body) != "BadRequest" {
			t.Errorf("%s: %d %v", name, resp.StatusCode, body)
		}
	}
	if n := len(h.s.Messages(testTeam, testChannel)); n != 0 {
		t.Fatalf("rejected imports stored %d messages", n)
	}

	// Accepted: the response is the chatMessage, ID = epoch milliseconds.
	resp, body := h.post(msgs, token, importBody(when, ""))
	if resp.StatusCode != 201 || body["id"] != "1549310295511" || body["createdDateTime"] != when || body["replyToId"] != nil ||
		body["from"].(map[string]any)["user"].(map[string]any)["id"] != testUser || body["body"].(map[string]any)["content"] != "hello" {
		t.Fatalf("import: %d %v", resp.StatusCode, body)
	}
	// Sub-millisecond digits are truncated: same millisecond, so 409.
	if resp, body := h.post(msgs, token, importBody("2019-02-04T19:58:15.5119999Z", "")); resp.StatusCode != 409 || graphCode(body) != "Conflict" {
		t.Errorf("duplicate: %d %v", resp.StatusCode, body)
	}
	// A time-zone offset is the same instant.
	if resp, _ := h.post(msgs, token, importBody("2019-02-04T20:58:15.511+01:00", "")); resp.StatusCode != 409 {
		t.Errorf("duplicate with offset: %d", resp.StatusCode)
	}
	// Plain text: <at> is not a tag there, and the default content type is text.
	text := strings.Replace(strings.Replace(importBody("2019-02-05T00:00:00Z", ""), `"html"`, `"text"`, 1), "hello", `<at id=\"0\">x</at>`, 1)
	if resp, body := h.post(msgs, token, text); resp.StatusCode != 201 {
		t.Errorf("text message: %d %v", resp.StatusCode, body)
	}
	resp, body = h.post(msgs+"/1549310295511/replies", token, importBody("2019-02-06T00:00:00Z", ""))
	if resp.StatusCode != 201 || body["replyToId"] != "1549310295511" || body["id"] != "1549411200000" {
		t.Errorf("reply: %d %v", resp.StatusCode, body)
	}

	// Lists: top-level only, newest first, $top capped at 50, paged.
	_, body = h.do("GET", msgs+"?$top=1", token, nil)
	page := body["value"].([]any)
	next, _ := body["@odata.nextLink"].(string)
	if len(page) != 1 || page[0].(map[string]any)["id"] != "1549324800000" || next == "" {
		t.Fatalf("first page: %v", body)
	}
	_, body = h.do("GET", strings.TrimPrefix(next, h.srv.URL), token, nil)
	if page := body["value"].([]any); len(page) != 1 || page[0].(map[string]any)["id"] != "1549310295511" || body["@odata.nextLink"] != nil {
		t.Errorf("second page: %v", body)
	}
	for path, want := range map[string]int{
		msgs + "?$top=51":                                     400,
		msgs + "?$filter=x":                                   400,
		msgs + "/1549310295511":                               200,
		msgs + "/1549411200000":                               404, // a reply is not a top-level message
		msgs + "/1549310295511/replies":                       200,
		msgs + "/1549310295511/replies/1549411200000":         200,
		msgs + "/1549324800000/replies/1549411200000":         404,
		msgs + "/1549411200000/replies":                       404,
		"/v1.0/teams/nope/channels/" + testChannel:            404,
		"/v1.0/teams/" + testTeam + "/channels/nope":          404,
		"/v1.0/teams/" + testTeam + "/channels/nope/messages": 404,
	} {
		if resp, body := h.do("GET", path, token, nil); resp.StatusCode != want {
			t.Errorf("GET %s = %d, want %d (%v)", path, resp.StatusCode, want, body)
		}
	}

	// The state dump carries stats and content.
	_, state := h.do("GET", "/_fake/state", "", nil)
	stats := state["stats"].(map[string]any)
	if stats["importsAccepted"] != float64(3) || stats["conflicts"] != float64(2) || stats["throttles"] != float64(0) {
		t.Errorf("stats: %v", stats)
	}
	ch := state["content"].([]any)[0].(map[string]any)["channels"].([]any)[0].(map[string]any)
	if ch["migrationMode"] != "inProgress" || ch["migrationOwner"] != DefaultClientID || len(ch["messages"].([]any)) != 3 ||
		ch["messages"].([]any)[2].(map[string]any)["replyToId"] != "1549310295511" {
		t.Errorf("content channel: %v", ch)
	}
	if got := h.s.Stats(); got != (Stats{ImportsAccepted: 3, Conflicts: 2}) {
		t.Errorf("Stats() = %+v", got)
	}
	if h.s.Messages("nope", testChannel) != nil || h.s.Messages(testTeam, "nope") != nil || h.s.Channels("nope") != nil {
		t.Error("inspection of unknown teams or channels should be nil")
	}
}

func TestWriteRoleEnforcement(t *testing.T) {
	owner := `{"@odata.type":"#microsoft.graph.aadUserConversationMember","roles":["owner"],"user@odata.bind":"https://graph.microsoft.com/v1.0/users('` + testUser + `')"}`
	for _, tc := range []struct {
		method, path, body string
		roles              []string
		want               int
	}{
		{"POST", "/v1.0/teams", `{"template@odata.bind":"x/teamsTemplates('standard')","displayName":"T","members":[` + owner + `]}`, []string{"Team.Create", "Teamwork.Migrate.All"}, 202},
		{"POST", "/v1.0/teams/" + testTeam + "/channels", `{"displayName":"new"}`, []string{"Channel.Create", "Teamwork.Migrate.All"}, 201},
		{"POST", "/v1.0/teams/" + testTeam + "/members", owner, []string{"TeamMember.ReadWrite.All"}, 201},
		{"POST", "/v1.0/teams/" + testTeam + "/members/add", `{"values":[` + owner + `]}`, []string{"TeamMember.ReadWrite.All"}, 200},
		{"POST", "/v1.0/teams/" + testTeam + "/channels/19:oncall-eng@thread.tacv2/members", owner, []string{"ChannelMember.ReadWrite.All"}, 400},
		{"GET", "/v1.0/teams/" + testTeam + "/channels/19:oncall-eng@thread.tacv2/members", ``, []string{"ChannelMember.ReadWrite.All", "ChannelMember.Read.All"}, 200},
		{"POST", chanPath + "/startMigration", ``, []string{"Teamwork.Migrate.All"}, 204},
		{"POST", chanPath + "/completeMigration", ``, []string{"Teamwork.Migrate.All"}, 400},
		{"POST", chanPath + "/messages", importBody("2021-01-01T00:00:00Z", ""), []string{"Teamwork.Migrate.All"}, 403},
		{"GET", chanPath + "/messages", ``, []string{"ChannelMessage.Read.All"}, 200},
		{"GET", chanPath, ``, []string{"Teamwork.Migrate.All", "Channel.ReadBasic.All"}, 200},
		{"GET", "/v1.0/teams/" + testTeam, ``, []string{"Teamwork.Migrate.All", "Team.ReadBasic.All"}, 200},
	} {
		seed := DefaultSeed()
		seed.Apps[0].Roles = []string{"User.Read.All", "Files.ReadWrite.All"}
		h := newHarness(t, seed)
		resp, body := h.do(tc.method, tc.path, h.token(), strings.NewReader(tc.body))
		if resp.StatusCode != 403 || graphCode(body) != "Authorization_RequestDenied" {
			t.Errorf("%s %s without a role: %d %v", tc.method, tc.path, resp.StatusCode, body)
		}
		for _, role := range tc.roles {
			seed.Apps[0].Roles = []string{role}
			h = newHarness(t, seed)
			resp, body := h.do(tc.method, tc.path, h.token(), strings.NewReader(tc.body))
			if resp.StatusCode != tc.want || graphCode(body) == "Authorization_RequestDenied" {
				t.Errorf("%s %s with %s: %d %v, want %d", tc.method, tc.path, role, resp.StatusCode, body, tc.want)
			}
		}
	}
}

func TestFaultsOnWrites(t *testing.T) {
	h := newHarness(t, DefaultSeed())
	token := h.token()
	if resp, _ := h.post(chanPath+"/startMigration", token, `{"conversationCreationDateTime":"2015-01-01T00:00:00Z"}`); resp.StatusCode != 204 {
		t.Fatal(resp.StatusCode)
	}
	msgs := chanPath + "/messages"
	f := h.s.Faults()
	if _, err := f.Add(Fault{Action: ActionFailAfterCommit, Status: 200}); err == nil {
		t.Error("fail_after_commit with a 2xx status should be rejected")
	}

	// 503 after the commit: stored, but the client is told it failed.
	f.MustAdd(Fault{Method: "POST", Path: "/v1.0/teams/*/channels/*/messages", Action: ActionFailAfterCommit, Status: 503, Count: 1})
	if resp, body := h.post(msgs, token, importBody("2019-01-01T00:00:00Z", "")); resp.StatusCode != 503 || graphCode(body) != "serviceNotAvailable" {
		t.Errorf("fail after commit: %d %v", resp.StatusCode, body)
	}
	if n := len(h.s.Messages(testTeam, testChannel)); n != 1 {
		t.Fatalf("stored %d, want 1", n)
	}
	if resp, _ := h.post(msgs, token, importBody("2019-01-01T00:00:00Z", "")); resp.StatusCode != 409 {
		t.Errorf("retry after a committed failure: %d, want 409", resp.StatusCode)
	}

	// Dropped after the commit, via the control endpoint's JSON form.
	if resp, body := h.post("/_fake/faults", "", `{"method":"POST","path":"/v1.0/teams/*/channels/*/messages","action":"fail_after_commit","count":1}`); resp.StatusCode != 201 {
		t.Fatalf("add fault: %d %v", resp.StatusCode, body)
	}
	req, _ := http.NewRequest("POST", h.srv.URL+msgs, strings.NewReader(importBody("2019-01-02T00:00:00Z", "")))
	req.Header.Set("Authorization", "Bearer "+token)
	if resp, err := h.srv.Client().Do(req); err == nil {
		resp.Body.Close()
		t.Errorf("dropped connection answered %d", resp.StatusCode)
	}
	if n := len(h.s.Messages(testTeam, testChannel)); n != 2 {
		t.Errorf("stored %d, want 2", n)
	}

	// Dropped or failed before the handler: nothing stored.
	for _, fault := range []Fault{
		{Method: "POST", Path: "/v1.0/teams/*/channels/*/messages", Action: ActionDrop, Count: 1},
		{Method: "POST", Path: "/v1.0/teams/*/channels/*/messages", Action: ActionStatus, Status: 503, Count: 1},
		{Method: "POST", Path: "/v1.0/teams/*/channels/*/messages", Action: ActionThrottle, RetryAfterSeconds: 2, Count: 1},
	} {
		f.MustAdd(fault)
		req, _ := http.NewRequest("POST", h.srv.URL+msgs, strings.NewReader(importBody("2019-01-03T00:00:00Z", "")))
		req.Header.Set("Authorization", "Bearer "+token)
		if resp, err := h.srv.Client().Do(req); err == nil {
			resp.Body.Close()
		}
		if n := len(h.s.Messages(testTeam, testChannel)); n != 2 {
			t.Errorf("%s fault stored a message (%d)", fault.Action, n)
		}
	}
	if st := h.s.Stats(); st.ImportsAccepted != 2 || st.Conflicts != 1 || st.Throttles != 1 {
		t.Errorf("stats = %+v", st)
	}
}

func TestChannelRateLimitStoresNothingWhenThrottled(t *testing.T) {
	h := newHarness(t, DefaultSeed(), WithChannelRateLimit(0.5, 1))
	token := h.token()
	if resp, _ := h.post(chanPath+"/startMigration", token, `{"conversationCreationDateTime":"2015-01-01T00:00:00Z"}`); resp.StatusCode != 204 {
		t.Fatal(resp.StatusCode)
	}
	msgs := chanPath + "/messages"
	if resp, _ := h.post(msgs, token, importBody("2019-01-01T00:00:00Z", "")); resp.StatusCode != 201 {
		t.Fatalf("first: %d", resp.StatusCode)
	}
	resp, body := h.post(msgs+"/1546300800000/replies", token, importBody("2019-01-02T00:00:00Z", ""))
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "2" || graphCode(body) != "TooManyRequests" {
		t.Errorf("throttled reply: %d Retry-After=%q %v", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
	if n := len(h.s.Messages(testTeam, testChannel)); n != 1 {
		t.Errorf("throttled request stored a message (%d)", n)
	}
	// Reads are not subject to the channel bucket.
	if resp, _ := h.do("GET", msgs, token, nil); resp.StatusCode != 200 {
		t.Errorf("read while throttled: %d", resp.StatusCode)
	}
	h.advance(2 * time.Second)
	if resp, _ := h.post(msgs+"/1546300800000/replies", token, importBody("2019-01-02T00:00:00Z", "")); resp.StatusCode != 201 {
		t.Errorf("after Retry-After: %d", resp.StatusCode)
	}
	if st := h.s.Stats(); st.Throttles != 1 || st.ImportsAccepted != 2 {
		t.Errorf("stats = %+v", st)
	}
	data, _ := json.Marshal(h.s.Messages(testTeam, testChannel))
	if !strings.Contains(string(data), `"replyToId":"1546300800000"`) {
		t.Errorf("messages JSON = %s", data)
	}
}

// A private channel has a SharePoint site of its own, provisioned when its
// files folder is first asked for.
func TestPrivateChannelFilesFolder(t *testing.T) {
	h := newHarness(t, DefaultSeed(), WithPrivateChannelSiteLag(2))
	token := h.token()
	folder := func(channel string) (*http.Response, map[string]any) {
		return h.do("GET", "/v1.0/teams/"+testTeam+"/channels/"+channel+"/filesFolder", token, nil)
	}
	resp, std := folder(testChannel)
	if resp.StatusCode != 200 {
		t.Fatalf("standard channel folder: %d %v", resp.StatusCode, std)
	}
	const private = "19:oncall-eng@thread.tacv2"
	for i := range 2 {
		if resp, body := folder(private); resp.StatusCode != 404 {
			t.Fatalf("private channel folder, request %d: %d %v, want 404 while its site is provisioned", i+1, resp.StatusCode, body)
		}
	}
	resp, priv := folder(private)
	if resp.StatusCode != 200 {
		t.Fatalf("private channel folder after provisioning: %d %v", resp.StatusCode, priv)
	}
	drive := func(item map[string]any) string {
		ref, _ := item["parentReference"].(map[string]any)
		id, _ := ref["driveId"].(string)
		return id
	}
	if drive(std) == "" || drive(priv) == "" || drive(std) == drive(priv) {
		t.Errorf("the private channel shares the team's drive: %q and %q", drive(std), drive(priv))
	}
	// The folder is addressable through its own drive and no other.
	id, _ := priv["id"].(string)
	if resp, _ := h.do("GET", "/v1.0/drives/"+drive(priv)+"/items/"+id, token, nil); resp.StatusCode != 200 {
		t.Errorf("private folder through its own drive: %d", resp.StatusCode)
	}
	if resp, _ := h.do("GET", "/v1.0/drives/"+drive(std)+"/items/"+id, token, nil); resp.StatusCode != 404 {
		t.Errorf("private folder through the team's drive: %d, want 404", resp.StatusCode)
	}
}
