package fakegraph

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// put sends a raw PUT, with optional headers, and returns the response and
// its decoded JSON body.
func (h *harness) put(target string, body []byte, headers map[string]string) (*http.Response, map[string]any) {
	h.t.Helper()
	if !strings.HasPrefix(target, "http") {
		target = h.srv.URL + target
	}
	req, err := http.NewRequest("PUT", target, bytes.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatalf("PUT: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp, out
}

func contentRange(start, end, total int) map[string]string {
	return map[string]string{"Content-Range": fmt.Sprintf("bytes %d-%d/%d", start, end, total)}
}

// folder returns the drive ID and item ID of the test channel's files folder.
func (h *harness) folder(token string) (drive, item string) {
	h.t.Helper()
	resp, body := h.do("GET", chanPath+"/filesFolder", token, nil)
	if resp.StatusCode != 200 {
		h.t.Fatalf("filesFolder: %d %v", resp.StatusCode, body)
	}
	return body["parentReference"].(map[string]any)["driveId"].(string), body["id"].(string)
}

func (h *harness) session(token, drive, parent, name string) string {
	h.t.Helper()
	resp, body := h.post("/v1.0/drives/"+drive+"/items/"+parent+":/"+url.PathEscape(name)+":/createUploadSession", token,
		`{"item":{"@microsoft.graph.conflictBehavior":"rename"}}`)
	if resp.StatusCode != 200 {
		h.t.Fatalf("createUploadSession: %d %v", resp.StatusCode, body)
	}
	if r := body["nextExpectedRanges"].([]any); len(r) != 1 || r[0] != "0-" || body["expirationDateTime"] == "" {
		h.t.Fatalf("session = %v", body)
	}
	return body["uploadUrl"].(string)
}

func TestDriveItemWire(t *testing.T) {
	h := newHarness(t, DefaultSeed())
	token := h.token()
	drive, root := h.folder(token)
	items := "/v1.0/drives/" + drive + "/items/"

	resp, body := h.do("GET", chanPath+"/filesFolder", token, nil)
	if resp.StatusCode != 200 || body["name"] != "General" || body["folder"] == nil || body["webUrl"] == "" || body["webDavUrl"] != nil {
		t.Errorf("filesFolder: %d %v", resp.StatusCode, body)
	}

	// Folders.
	mk := func(name, conflict string) (*http.Response, map[string]any) {
		return h.post(items+root+"/children", token, fmt.Sprintf(`{"name":%q,"folder":{},"@microsoft.graph.conflictBehavior":%q}`, name, conflict))
	}
	resp, body = mk("Slack", "fail")
	if resp.StatusCode != 201 || body["name"] != "Slack" || body["folder"] == nil {
		t.Fatalf("create folder: %d %v", resp.StatusCode, body)
	}
	slack := body["id"].(string)
	if resp, body := mk("slack", "fail"); resp.StatusCode != 409 || graphCode(body) != "nameAlreadyExists" {
		t.Errorf("duplicate folder: %d %v", resp.StatusCode, body)
	}
	if resp, body := h.post(items+root+"/children", token, `{"name":"x"}`); resp.StatusCode != 400 || graphCode(body) != "invalidRequest" {
		t.Errorf("child without a folder facet: %d %v", resp.StatusCode, body)
	}

	// A small file by simple PUT, then path addressing and $select.
	content := []byte("hello sharepoint")
	resp, body = h.put(items+slack+":/"+url.PathEscape("a b#1.txt")+":/content", content, map[string]string{"Authorization": "Bearer " + token})
	if resp.StatusCode != 201 || body["size"] != float64(len(content)) || body["file"] == nil {
		t.Fatalf("simple upload: %d %v", resp.StatusCode, body)
	}
	fileID := body["id"].(string)
	if resp, _ := h.put(items+slack+":/"+url.PathEscape("a b#1.txt")+":/content", content, map[string]string{"Authorization": "Bearer " + token}); resp.StatusCode != 200 {
		t.Errorf("replacing by simple upload: %d, want 200", resp.StatusCode)
	}
	if resp, body := h.put(items+slack+":/big.bin:/content", make([]byte, MaxSimpleUploadBytes+1), map[string]string{"Authorization": "Bearer " + token}); resp.StatusCode != 400 {
		t.Errorf("oversized simple upload: %d %v", resp.StatusCode, body)
	}

	_, body = h.do("GET", items+fileID, token, nil)
	if _, has := body["webDavUrl"]; has || body["eTag"] == nil || body["parentReference"].(map[string]any)["id"] != slack {
		t.Errorf("default projection: %v", body)
	}
	_, body = h.do("GET", items+fileID+"?$select=id,name,size,eTag,webUrl,webDavUrl", token, nil)
	etag, _ := body["eTag"].(string)
	if len(body) != 6 || !regexp.MustCompile(`^"\{[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}\},2"$`).MatchString(etag) ||
		!strings.HasSuffix(body["webDavUrl"].(string), "/Shared%20Documents/General/Slack/a%20b%231.txt") || body["webUrl"] == body["webDavUrl"] {
		t.Errorf("selected projection: %v", body)
	}
	for path, want := range map[string]int{
		items + root + ":/Slack":                              200,
		items + root + ":/slack/A%20B%231.txt":                200,
		items + root + ":/Slack/a%20b%231.txt:":               200,
		items + slack + ":/a%20b%231.txt":                     200,
		items + root + ":/Slack/missing.txt":                  404,
		items + root + ":/Missing/a.txt":                      404,
		items + fileID + ":/below-a-file":                     404,
		items + "nope":                                        404,
		"/v1.0/drives/b!other/items/" + root:                  404,
		items + fileID + "?$select=shoeSize":                  400,
		items + fileID + "?$expand=children":                  400,
		chanPath + "/filesFolder?$select=id,webDavUrl":        200,
		"/v1.0/teams/" + testTeam + "/channels/x/filesFolder": 404,
	} {
		if resp, body := h.do("GET", path, token, nil); resp.StatusCode != want {
			t.Errorf("GET %s = %d, want %d (%v)", path, resp.StatusCode, want, body)
		}
	}

	// Names SharePoint refuses, for folders, sessions and simple uploads.
	for _, name := range []string{
		`a"b`, "a*b", "a:b", "a<b", "a>b", "a?b", `a\b`, "a|b", " lead", "trail ", "trail.", ".lock", "CON", "prn", "Aux", "NUL",
		"COM0", "com9", "LPT0", "lpt9", "_vti_", "x_VTI_y", "desktop.ini", "~$doc.docx", "tab\tname", strings.Repeat("n", 256),
	} {
		if resp, body := mk(name, "fail"); resp.StatusCode != 400 || graphCode(body) != "invalidRequest" {
			t.Errorf("folder %q: %d %v", name, resp.StatusCode, body)
		}
		esc := url.PathEscape(name)
		if resp, body := h.post(items+root+":/"+esc+":/createUploadSession", token, `{}`); resp.StatusCode != 400 || graphCode(body) != "invalidRequest" {
			t.Errorf("session for %q: %d %v", name, resp.StatusCode, body)
		}
		if resp, _ := h.put(items+root+":/"+esc+":/content", nil, map[string]string{"Authorization": "Bearer " + token}); resp.StatusCode != 400 {
			t.Errorf("simple upload of %q: %d", name, resp.StatusCode)
		}
	}
	for _, name := range []string{"CON.txt", "a.b", ".hidden", "ok name (1) [2] #3 & 4%.txt", strings.Repeat("n", 255)} {
		if resp, body := mk(name, "fail"); resp.StatusCode != 201 {
			t.Errorf("folder %q: %d %v", name, resp.StatusCode, body)
		}
	}
	// The whole path is limited to 400 characters.
	deep := root
	for i := range 2 {
		_, body := h.post(items+deep+"/children", token, fmt.Sprintf(`{"name":%q,"folder":{}}`, strings.Repeat("d", 190)+fmt.Sprint(i)))
		deep = body["id"].(string)
	}
	if resp, body := h.post(items+deep+":/"+strings.Repeat("f", 30)+":/createUploadSession", token, `{}`); resp.StatusCode != 400 {
		t.Errorf("over-long path: %d %v", resp.StatusCode, body)
	}
	for body, want := range map[string]int{
		`{"item":{"@microsoft.graph.conflictBehavior":"shrug"}}`: 400,
		`{"item":{"name":"other.txt"}}`:                          400,
		`{"item":{"name":"ok.txt"}}`:                             200,
		``:                                                       200,
	} {
		if resp, out := h.post(items+slack+":/ok.txt:/createUploadSession", token, body); resp.StatusCode != want {
			t.Errorf("createUploadSession %s: %d %v, want %d", body, resp.StatusCode, out, want)
		}
	}
	if resp, _ := h.post(items+slack+":/Missing/ok.txt:/createUploadSession", token, ``); resp.StatusCode != 404 {
		t.Errorf("session under a missing folder: %d", resp.StatusCode)
	}
}

func TestUploadSessionWire(t *testing.T) {
	h := newHarness(t, DefaultSeed(), WithUploadSessionLifetime(time.Hour))
	token := h.token()
	drive, root := h.folder(token)
	const unit = UploadChunkMultiple
	data := make([]byte, 2*unit+500)
	for i := range data {
		data[i] = byte(i * 31)
	}
	total := len(data)
	u := h.session(token, drive, root, "movie.bin")
	if !strings.HasPrefix(u, h.srv.URL+"/_upload/") {
		t.Fatalf("uploadUrl = %q", u)
	}

	// The URL is pre-authorized: a bearer token is refused.
	hdr := contentRange(0, unit-1, total)
	hdr["Authorization"] = "Bearer " + token
	if resp, body := h.put(u, data[:unit], hdr); resp.StatusCode != 401 {
		t.Errorf("chunk with a bearer token: %d %v", resp.StatusCode, body)
	}
	// Chunk rules.
	for name, tc := range map[string]struct {
		body []byte
		hdr  map[string]string
		want int
	}{
		"no Content-Range":        {data[:unit], nil, 400},
		"malformed Content-Range": {data[:unit], map[string]string{"Content-Range": "bytes 0-*/" + fmt.Sprint(total)}, 400},
		"not a 320 KiB multiple":  {data[:1000], contentRange(0, 999, total), 400},
		"length mismatch":         {data[:unit-1], contentRange(0, unit-1, total), 400},
		"end beyond total":        {data[:unit], contentRange(0, unit-1, unit-5), 400},
		"end before start":        {data[:unit], contentRange(unit, 0, total), 400},
		"gap":                     {data[unit : 2*unit], contentRange(unit, 2*unit-1, total), 416},
		"larger than 60 MiB":      {make([]byte, 193*unit), contentRange(0, 193*unit-1, 400*unit), 400},
	} {
		if resp, body := h.put(u, tc.body, tc.hdr); resp.StatusCode != tc.want {
			t.Errorf("%s: %d %v, want %d", name, resp.StatusCode, body, tc.want)
		}
	}
	status := func() string {
		_, body := h.do("GET", strings.TrimPrefix(u, h.srv.URL), "", nil)
		r, _ := body["nextExpectedRanges"].([]any)
		if len(r) != 1 {
			return fmt.Sprint(body)
		}
		return r[0].(string)
	}
	if got := status(); got != "0-" {
		t.Fatalf("rejected chunks moved the session: %s", got)
	}

	resp, body := h.put(u, data[:unit], contentRange(0, unit-1, total))
	if r := body["nextExpectedRanges"].([]any); resp.StatusCode != 202 || len(r) != 1 || r[0] != fmt.Sprintf("%d-", unit) || body["expirationDateTime"] == nil {
		t.Fatalf("first chunk: %d %v", resp.StatusCode, body)
	}
	// Re-sending the same range, or overlapping it, is refused with the
	// expected range; so is a different total.
	resp, body = h.put(u, data[:unit], contentRange(0, unit-1, total))
	if r, _ := body["nextExpectedRanges"].([]any); resp.StatusCode != 416 || len(r) != 1 || r[0] != fmt.Sprintf("%d-", unit) {
		t.Errorf("repeated chunk: %d %v", resp.StatusCode, body)
	}
	if resp, _ := h.put(u, data[unit:2*unit], contentRange(unit, 2*unit-1, total+1)); resp.StatusCode != 400 {
		t.Errorf("changed total: %d", resp.StatusCode)
	}
	if resp, _ := h.put(u, data[unit:2*unit], contentRange(unit, 2*unit-1, total)); resp.StatusCode != 202 {
		t.Fatalf("second chunk: %d", resp.StatusCode)
	}
	if got := status(); got != fmt.Sprintf("%d-", 2*unit) {
		t.Errorf("status = %s", got)
	}
	resp, body = h.put(u, data[2*unit:], contentRange(2*unit, total-1, total))
	if resp.StatusCode != 201 || body["name"] != "movie.bin" || body["size"] != float64(total) {
		t.Fatalf("final chunk: %d %v", resp.StatusCode, body)
	}
	sum := sha256.Sum256(data)
	files := h.s.Files(testTeam, testChannel)
	if len(files) != 1 || files[0].SHA256 != hex.EncodeToString(sum[:]) || files[0].Size != int64(total) || files[0].Path != "movie.bin" {
		t.Errorf("files = %+v", files)
	}
	// The session is gone once the file is complete.
	if resp, _ := h.do("GET", strings.TrimPrefix(u, h.srv.URL), "", nil); resp.StatusCode != 404 {
		t.Errorf("status of a completed session: %d", resp.StatusCode)
	}
	if resp, _ := h.put(u, data[:1], contentRange(0, 0, 1)); resp.StatusCode != 404 {
		t.Errorf("chunk to a completed session: %d", resp.StatusCode)
	}

	// Conflict behaviors at completion: rename, fail, replace.
	one := func(conflict string) (*http.Response, map[string]any) {
		_, body := h.post("/v1.0/drives/"+drive+"/items/"+root+":/movie.bin:/createUploadSession", token,
			`{"item":{"@microsoft.graph.conflictBehavior":"`+conflict+`"}}`)
		return h.put(body["uploadUrl"].(string), []byte("x"), contentRange(0, 0, 1))
	}
	if resp, body := one("rename"); resp.StatusCode != 201 || body["name"] != "movie 1.bin" {
		t.Errorf("rename: %d %v", resp.StatusCode, body)
	}
	if resp, body := one("fail"); resp.StatusCode != 409 || graphCode(body) != "nameAlreadyExists" {
		t.Errorf("fail: %d %v", resp.StatusCode, body)
	}
	if resp, body := one("replace"); resp.StatusCode != 200 || body["name"] != "movie.bin" || body["size"] != float64(1) {
		t.Errorf("replace: %d %v", resp.StatusCode, body)
	}

	// Cancel, expiry, unknown sessions.
	u = h.session(token, drive, root, "gone.bin")
	if resp, _ := h.do("DELETE", strings.TrimPrefix(u, h.srv.URL), "", nil); resp.StatusCode != 204 {
		t.Errorf("cancel: %d", resp.StatusCode)
	}
	if resp, _ := h.put(u, []byte("x"), contentRange(0, 0, 1)); resp.StatusCode != 404 {
		t.Errorf("chunk to a cancelled session: %d", resp.StatusCode)
	}
	u = h.session(token, drive, root, "late.bin")
	h.advance(time.Hour)
	if resp, _ := h.put(u, []byte("x"), contentRange(0, 0, 1)); resp.StatusCode != 404 {
		t.Errorf("chunk to an expired session: %d", resp.StatusCode)
	}
	if resp, _ := h.do("GET", "/_upload/0000", "", nil); resp.StatusCode != 404 {
		t.Errorf("unknown session: %d", resp.StatusCode)
	}

	// The upload URL is a credential: the request log shows a label.
	for _, r := range h.s.Requests() {
		if strings.HasPrefix(r.Path, "/_upload/") && !strings.HasPrefix(r.Path, "/_upload/session-") && r.Path != "/_upload/unknown" {
			t.Fatalf("request log leaks an upload URL: %s", r.Path)
		}
	}
	if n := h.s.CountRequests("PUT", "/_upload/session-*"); n < 10 {
		t.Errorf("logged chunk PUTs = %d", n)
	}
	st := h.s.Stats()
	if st.UploadsCompleted != 3 || st.UploadSessions != 6 || st.UploadBytes != int64(total)+2 {
		t.Errorf("stats = %+v", st)
	}
	_, state := h.do("GET", "/_fake/state", "", nil)
	ch := state["content"].([]any)[0].(map[string]any)["channels"].([]any)[0].(map[string]any)
	if len(ch["files"].([]any)) != 2 || state["stats"].(map[string]any)["uploadsCompleted"] != float64(3) {
		t.Errorf("state files = %v", ch["files"])
	}
}

func TestFileRoleEnforcement(t *testing.T) {
	seed := DefaultSeed()
	h := newHarness(t, seed)
	drive, root := h.folder(h.token())
	items := "/v1.0/drives/" + drive + "/items/" + root
	for _, tc := range []struct {
		method, path, body string
		roles              []string
		want               int
	}{
		{"GET", chanPath + "/filesFolder", ``, rolesFilesRead, 200},
		{"GET", items, ``, rolesFilesRead, 200},
		{"POST", items + "/children", `{"name":"f","folder":{}}`, rolesFilesWrite, 201},
		{"POST", items + ":/a.txt:/createUploadSession", `{}`, rolesUploadSession, 200},
		{"PUT", items + ":/a.txt:/content", ``, rolesFilesWrite, 201},
	} {
		for _, role := range append([]string{"Teamwork.Migrate.All", "Files.Read.All"}, tc.roles...) {
			seed.Apps[0].Roles = []string{role}
			h := newHarness(t, seed)
			h.folder(seedToken(t, h)) // the folder exists in every harness, with the same deterministic IDs
			resp, body := h.do(tc.method, tc.path, h.token(), strings.NewReader(tc.body))
			want := tc.want
			if !contains(tc.roles, role) {
				want = 403
			}
			if resp.StatusCode != want {
				t.Errorf("%s %s with %s: %d %v, want %d", tc.method, tc.path, role, resp.StatusCode, body, want)
			}
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// seedToken returns a token for a second app that holds every role, so a
// test can prepare state whatever roles the app under test has.
func seedToken(t *testing.T, h *harness) string {
	t.Helper()
	h.s.mu.Lock()
	h.s.st.apps["setup-app"] = &appState{App: App{ClientID: "setup-app", Secrets: []string{"setup"}, Roles: DefaultRoles()}}
	h.s.mu.Unlock()
	form := h.secretForm()
	form.Set("client_id", "setup-app")
	form.Set("client_secret", "setup")
	resp, body := h.tokenRequest(form)
	if resp.StatusCode != 200 {
		t.Fatalf("setup token: %d %v", resp.StatusCode, body)
	}
	return body["access_token"].(string)
}

func TestHostedContentAndAttachmentWire(t *testing.T) {
	h := newHarness(t, DefaultSeed())
	token := h.token()
	if resp, _ := h.post(chanPath+"/startMigration", token, `{"conversationCreationDateTime":"2015-01-01T00:00:00Z"}`); resp.StatusCode != 204 {
		t.Fatal(resp.StatusCode)
	}
	msg := func(when, content, extra string) string {
		c, _ := json.Marshal(content)
		return fmt.Sprintf(`{"createdDateTime":%q,"from":{"user":{"id":%q}},"body":{"contentType":"html","content":%s}%s}`, when, testUser, c, extra)
	}
	img := `<img src="../hostedContents/1/$value">`
	for name, extra := range map[string]string{
		"bad base64":      `,"hostedContents":[{"@microsoft.graph.temporaryId":"1","contentBytes":"***","contentType":"image/png"}]`,
		"no temporary id": `,"hostedContents":[{"contentBytes":"aGk=","contentType":"image/png"}]`,
		"duplicate id":    `,"hostedContents":[{"@microsoft.graph.temporaryId":"1","contentBytes":"aGk=","contentType":"image/png"},{"@microsoft.graph.temporaryId":"1","contentBytes":"aGk=","contentType":"image/png"}]`,
		"not an image":    `,"hostedContents":[{"@microsoft.graph.temporaryId":"1","contentBytes":"aGk=","contentType":"text/html"}]`,
		"file attachment": `,"hostedContents":[{"@microsoft.graph.temporaryId":"1","contentBytes":"aGk=","contentType":"image/png"}],"attachments":[{"id":"x","contentType":"file","contentUrl":"https://x"}]`,
	} {
		if resp, body := h.post(chanPath+"/messages", token, msg("2019-01-01T00:00:00Z", img, extra)); resp.StatusCode != 400 || graphCode(body) != "BadRequest" {
			t.Errorf("%s: %d %v", name, resp.StatusCode, body)
		}
	}
	resp, body := h.post(chanPath+"/messages", token, msg("2019-01-01T00:00:00Z", img,
		`,"hostedContents":[{"@microsoft.graph.temporaryId":"1","contentBytes":"aGk=","contentType":"image/png"}]`))
	if resp.StatusCode != 201 {
		t.Fatalf("hosted content: %d %v", resp.StatusCode, body)
	}
	sum := sha256.Sum256([]byte("hi"))
	if m := h.s.Messages(testTeam, testChannel); len(m) != 1 || m[0].HostedContents != 1 || m[0].HostedBytes != 2 || m[0].HostedSHA256 != hex.EncodeToString(sum[:]) || len(m[0].Attachments) != 0 {
		t.Errorf("stored = %+v", m)
	}

	// An attachment comes back on the stored message.
	drive, root := h.folder(token)
	_, file := h.put("/v1.0/drives/"+drive+"/items/"+root+":/a.txt:/content?$select=id,eTag,webDavUrl", []byte("x"), map[string]string{"Authorization": "Bearer " + token})
	guid := strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(strings.Split(file["eTag"].(string), ",")[0], `"{`), "}"))
	att := fmt.Sprintf(`,"attachments":[{"id":%q,"contentType":"reference","contentUrl":%q,"name":"a.txt"}]`, guid, file["webDavUrl"])
	resp, body = h.post(chanPath+"/messages", token, msg("2019-01-02T00:00:00Z", `see <attachment id="`+guid+`"></attachment>`, att))
	if resp.StatusCode != 201 {
		t.Fatalf("attachment: %d %v", resp.StatusCode, body)
	}
	if a := body["attachments"].([]any); len(a) != 1 || a[0].(map[string]any)["id"] != guid || a[0].(map[string]any)["contentUrl"] != file["webDavUrl"] {
		t.Errorf("attachments in the response: %v", a)
	}
	// In a plain-text body the marker is not a tag.
	text := strings.Replace(msg("2019-01-03T00:00:00Z", `see <attachment id="`+guid+`"></attachment>`, att), `"html"`, `"text"`, 1)
	if resp, _ := h.post(chanPath+"/messages", token, text); resp.StatusCode != 400 {
		t.Errorf("attachment in a text body: %d", resp.StatusCode)
	}
}
