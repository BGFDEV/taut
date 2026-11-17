package fakegraph

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// This file is SharePoint as Taut sees it: each team has one drive, each
// channel a folder in it (its filesFolder), and files arrive through upload
// sessions. File bytes are never kept: an item records its size and SHA-256.

// Upload limits, as documented for Graph upload sessions.
const (
	// UploadChunkMultiple is the size every chunk but the last must be a
	// multiple of: 320 KiB.
	UploadChunkMultiple = 320 * 1024
	// MaxUploadChunkBytes is the largest single PUT: 60 MiB.
	MaxUploadChunkBytes = 60 << 20
	// MaxSimpleUploadBytes is the largest body PUT …:/content accepts here.
	MaxSimpleUploadBytes = 4 << 20
	uploadPrefix         = "/_upload/"
	sharePointBase       = "https://fakegraph.sharepoint.example/sites/"
)

var (
	rolesFilesRead  = []string{"Files.Read.All", "Files.ReadWrite.All", "Sites.Read.All", "Sites.ReadWrite.All", "Group.Read.All", "Group.ReadWrite.All"}
	rolesFilesWrite = []string{"Files.ReadWrite.All", "Sites.ReadWrite.All"}
	// The reference lists only Sites.ReadWrite.All for createUploadSession
	// with application permissions.
	rolesUploadSession = []string{"Sites.ReadWrite.All"}
)

type driveItemState struct {
	id string
	// guid is the item's unique ID as it appears in the eTag, upper case.
	guid     string
	version  int
	name     string
	team     *teamState
	parent   *driveItemState // nil for a channel's root folder
	folder   bool
	children []*driveItemState
	size     int64
	sha256   string
	created  time.Time
	modified time.Time
	// uploadedBy is the app that created or last replaced the item.
	uploadedBy string
	// site names the SharePoint site the item lives in when that is not
	// the team's own: a private or shared channel has a site, and so a
	// drive, to itself. Empty means the team's site.
	site string
}

// driveID returns the ID of the drive the item is in.
func (it *driveItemState) driveID() string {
	if it.site != "" {
		return "b!" + it.site
	}
	return driveID(it.team)
}

// siteID returns the ID of the site the item is in.
func (it *driveItemState) siteID() string {
	if it.site != "" {
		return it.site
	}
	return it.team.ID
}

// uploadSession is an open upload. mu serializes chunk PUTs of one session
// without holding the server lock while a body is read.
type uploadSession struct {
	mu       sync.Mutex
	token    string
	label    string // what the request log shows instead of the token
	team     *teamState
	parent   *driveItemState
	name     string
	conflict string
	app      string
	expires  time.Time
	total    int64 // -1 until the first chunk states it
	received int64
	hash     hash.Hash
	closed   bool
}

func invalidRequest(format string, a ...any) *gErr {
	return &gErr{http.StatusBadRequest, "invalidRequest", fmt.Sprintf(format, a...)}
}

func itemNotFound() *gErr {
	return &gErr{http.StatusNotFound, "itemNotFound", "The resource could not be found."}
}

func driveID(t *teamState) string { return "b!" + strings.ReplaceAll(t.ID, "-", "") }

// path returns the item's path segments from the drive root.
func (it *driveItemState) path() []string {
	if it.parent == nil {
		return []string{it.name}
	}
	return append(it.parent.path(), it.name)
}

func (it *driveItemState) child(name string) *driveItemState {
	for _, c := range it.children {
		if strings.EqualFold(c.name, name) {
			return c
		}
	}
	return nil
}

func (it *driveItemState) eTag() string { return fmt.Sprintf(`"{%s},%d"`, it.guid, it.version) }

func (it *driveItemState) webDavURL() string {
	segs := it.path()
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return sharePointBase + it.siteID() + "/Shared%20Documents/" + strings.Join(segs, "/")
}

// reservedFileNames may not be used as a file or folder name, in any case.
var reservedFileNames = func() map[string]bool {
	m := map[string]bool{".lock": true, "con": true, "prn": true, "aux": true, "nul": true, "desktop.ini": true}
	for i := 0; i <= 9; i++ {
		m[fmt.Sprintf("com%d", i)] = true
		m[fmt.Sprintf("lpt%d", i)] = true
	}
	return m
}()

// checkFileName applies SharePoint's naming rules and returns the reason a
// name is rejected, or "".
func checkFileName(name string) string {
	lower := strings.ToLower(name)
	switch {
	case name == "":
		return "The provided name cannot be empty."
	case !utf8.ValidString(name):
		return "The provided name is not valid UTF-8."
	case utf8.RuneCountInString(name) > 255:
		return "The provided name is longer than 255 characters."
	case strings.ContainsAny(name, "\"*:<>?/\\|"), strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return "The provided name contains invalid characters."
	case strings.HasPrefix(name, " "), strings.HasSuffix(name, " "):
		return "The provided name cannot start or end with a space."
	case strings.HasSuffix(name, "."):
		return "The provided name cannot end with a period."
	case strings.HasPrefix(name, "~$"):
		return "The provided name cannot start with ~$."
	case reservedFileNames[lower], strings.Contains(lower, "_vti_"):
		return "The provided name is reserved and cannot be used."
	}
	return ""
}

// channelFolder returns the channel's root folder, creating it on first
// use. It must be called with the write lock held.
func (s *Server) channelFolder(t *teamState, ch *channelState) *driveItemState {
	if ch.folder == nil {
		ch.folder = s.newItem(t, nil, ch.DisplayName, true, "")
		if ch.MembershipType != "standard" {
			// Each private or shared channel has its own SharePoint site.
			ch.folder.site = "site" + ch.folder.guid
		}
	}
	return ch.folder
}

// newItem must be called with the write lock held.
func (s *Server) newItem(t *teamState, parent *driveItemState, name string, folder bool, app string) *driveItemState {
	now := s.cfg.now().UTC().Truncate(time.Millisecond)
	it := &driveItemState{
		id: s.nextID("01FAKEGRAPH%021d"), guid: s.nextID("E0000000-0000-4000-8000-%012X"), version: 1,
		name: name, team: t, parent: parent, folder: folder, created: now, modified: now, uploadedBy: app,
	}
	if s.st.items == nil {
		s.st.items = map[string]*driveItemState{}
		s.st.itemsByGUID = map[string]*driveItemState{}
	}
	s.st.items[it.id] = it
	s.st.itemsByGUID[it.guid] = it
	if parent != nil {
		it.site = parent.site
		parent.children = append(parent.children, it)
	}
	return it
}

// itemEntity renders a driveItem. It must be called with s.mu held.
func (s *Server) itemEntity(it *driveItemState) map[string]any {
	ref := map[string]any{"driveId": it.driveID(), "driveType": "documentLibrary", "siteId": it.siteID()}
	if it.parent != nil {
		ref["id"] = it.parent.id
		ref["path"] = "/drives/" + it.driveID() + "/root:/" + strings.Join(it.parent.path(), "/")
	}
	e := map[string]any{
		"id": it.id, "name": it.name, "size": it.size, "eTag": it.eTag(),
		"cTag":   fmt.Sprintf(`"c:{%s},%d"`, it.guid, it.version),
		"webUrl": it.webDavURL() + "?web=1", "webDavUrl": it.webDavURL(),
		"createdDateTime": graphTime(it.created), "lastModifiedDateTime": graphTime(it.modified),
		"parentReference": ref,
	}
	if it.folder {
		e["folder"] = map[string]any{"childCount": len(it.children)}
	} else {
		e["file"] = map[string]any{"mimeType": "application/octet-stream"}
	}
	return e
}

// itemDefaults are the properties returned without $select. As on the real
// service, webDavUrl is only returned when selected.
func itemDefaults(it *driveItemState) []string {
	d := []string{"id", "name", "size", "eTag", "cTag", "webUrl", "createdDateTime", "lastModifiedDateTime", "parentReference"}
	if it.folder {
		return append(d, "folder")
	}
	return append(d, "file")
}

// writeItem projects and writes a driveItem entity built under the lock.
func (s *Server) writeItem(w http.ResponseWriter, r *http.Request, status int, entity map[string]any, defaults []string) {
	out, err := project(entity, r.URL.Query().Get("$select"), defaults, "driveItem")
	if err != nil {
		s.graphError(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	writeJSON(w, status, out)
}

func (s *Server) getFilesFolder(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.checkQuery(w, r, "$select"); !ok {
		return
	}
	var entity map[string]any
	var defaults []string
	e := func() *gErr {
		s.mu.Lock()
		defer s.mu.Unlock()
		t, ch, e := s.locate(r.PathValue("id"), r.PathValue("cid"))
		if e != nil {
			return e
		}
		ch.folderCalls++
		lag := s.cfg.filesFolderLag
		if ch.MembershipType != "standard" {
			// The channel's own site is provisioned on demand: the first
			// request triggers it and it is ready some requests later.
			lag += s.cfg.privateSiteLag
		}
		if ch.folderCalls <= lag {
			return notFound("The files folder of channel %s is not provisioned yet.", ch.ID)
		}
		if s.cfg.filesFolderBlocked && ch.migrationMode == migrationInProgress {
			return notFound("The files folder of channel %s is not available while the channel is in migration mode.", ch.ID)
		}
		it := s.channelFolder(t, ch)
		entity, defaults = s.itemEntity(it), itemDefaults(it)
		return nil
	}()
	if e != nil {
		s.fail(w, e)
		return
	}
	s.writeItem(w, r, http.StatusOK, entity, defaults)
}

// driveAddress is a parsed /drives/{drive}/items/{rest...} path: an item ID,
// optionally followed by ":/relative/path" and an action.
type driveAddress struct {
	drive, itemID string
	rel           []string // path segments below the item
	action        string   // "", "children", "createUploadSession" or "content"
}

func parseDriveAddress(r *http.Request) (driveAddress, bool) {
	a := driveAddress{drive: r.PathValue("drive")}
	rest := r.PathValue("rest")
	id, tail, byPath := strings.Cut(rest, ":/")
	if !byPath {
		id, tail, _ = strings.Cut(rest, "/")
		a.itemID, a.action = id, tail
		return a, id != "" && (tail == "" || tail == "children")
	}
	a.itemID = id
	for _, action := range []string{"createUploadSession", "content"} {
		if p, ok := strings.CutSuffix(tail, ":/"+action); ok {
			tail, a.action = p, action
		}
	}
	tail = strings.TrimSuffix(tail, ":")
	if id == "" || tail == "" {
		return a, false
	}
	a.rel = strings.Split(tail, "/")
	return a, true
}

// resolveItem finds the addressed item's anchor (the item named by ID) in
// the drive. It must be called with s.mu held.
func (s *Server) resolveItem(a driveAddress) (*driveItemState, *gErr) {
	it := s.st.items[a.itemID]
	if it == nil || it.driveID() != a.drive || s.findTeam(it.team.ID) == nil {
		return nil, itemNotFound()
	}
	return it, nil
}

// walk follows path segments below it; the last segment may be absent, in
// which case leaf is nil and parent is where it would live.
func walk(it *driveItemState, rel []string) (parent, leaf *driveItemState, e *gErr) {
	for i, seg := range rel {
		if !it.folder {
			return nil, nil, itemNotFound()
		}
		next := it.child(seg)
		if next == nil {
			if i == len(rel)-1 {
				return it, nil, nil
			}
			return nil, nil, itemNotFound()
		}
		if i == len(rel)-1 {
			return it, next, nil
		}
		it = next
	}
	return nil, it, nil
}

func (s *Server) getDriveItem(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.checkQuery(w, r, "$select"); !ok {
		return
	}
	a, ok := parseDriveAddress(r)
	if !ok || a.action != "" {
		s.fail(w, invalidRequest("Unsupported drive item address."))
		return
	}
	s.mu.RLock()
	it, e := s.resolveItem(a)
	if e == nil {
		if _, it, e = walk(it, a.rel); e == nil && it == nil {
			e = itemNotFound()
		}
	}
	var entity map[string]any
	var defaults []string
	if e == nil {
		entity, defaults = s.itemEntity(it), itemDefaults(it)
	}
	s.mu.RUnlock()
	if e != nil {
		s.fail(w, e)
		return
	}
	s.writeItem(w, r, http.StatusOK, entity, defaults)
}

// postDriveItem serves POST …/children (create a folder) and
// POST …:/createUploadSession.
func (s *Server) postDriveItem(w http.ResponseWriter, r *http.Request) {
	a, ok := parseDriveAddress(r)
	switch {
	case ok && a.action == "children":
		s.createFolder(w, r, a)
	case ok && a.action == "createUploadSession":
		s.authorized(rolesUploadSession, func(w http.ResponseWriter, r *http.Request) { s.createUploadSession(w, r, a) })(w, r)
	default:
		s.fail(w, invalidRequest("Unsupported drive item address."))
	}
}

func (s *Server) createFolder(w http.ResponseWriter, r *http.Request, a driveAddress) {
	var in struct {
		Name     string          `json:"name"`
		Folder   *map[string]any `json:"folder"`
		Conflict string          `json:"@microsoft.graph.conflictBehavior"`
	}
	if e := readJSON(w, r, &in, false); e != nil {
		s.fail(w, e)
		return
	}
	app := record(r).AppID
	var entity map[string]any
	var defaults []string
	e := func() *gErr {
		s.mu.Lock()
		defer s.mu.Unlock()
		parent, e := s.resolveItem(a)
		switch {
		case e != nil:
			return e
		case !parent.folder:
			return invalidRequest("The parent item is not a folder.")
		case in.Folder == nil:
			return invalidRequest("Only folders can be created here: the 'folder' facet is required.")
		case in.Conflict != "" && in.Conflict != "fail":
			return invalidRequest("fakegraph supports only conflictBehavior 'fail' when creating a folder.")
		}
		if why := checkFileName(in.Name); why != "" {
			return invalidRequest("%s", why)
		}
		if parent.child(in.Name) != nil {
			return &gErr{http.StatusConflict, "nameAlreadyExists", "An item with the same name already exists under the parent."}
		}
		it := s.newItem(parent.team, parent, in.Name, true, app)
		entity, defaults = s.itemEntity(it), itemDefaults(it)
		return nil
	}()
	if e != nil {
		s.fail(w, e)
		return
	}
	s.writeItem(w, r, http.StatusCreated, entity, defaults)
}

// uploadTarget validates where an upload goes. It must be called with s.mu
// held.
func (s *Server) uploadTarget(a driveAddress, conflict string) (parent *driveItemState, name string, e *gErr) {
	it, e := s.resolveItem(a)
	if e != nil {
		return nil, "", e
	}
	parent, leaf, e := walk(it, a.rel)
	if e != nil {
		return nil, "", e
	}
	name = a.rel[len(a.rel)-1]
	if why := checkFileName(name); why != "" {
		return nil, "", invalidRequest("%s", why)
	}
	if n := utf8.RuneCountInString(strings.Join(append(parent.path(), name), "/")); n > 400 {
		return nil, "", invalidRequest("The provided path is longer than 400 characters.")
	}
	if leaf != nil && leaf.folder {
		return nil, "", invalidRequest("A folder with the provided name already exists.")
	}
	switch conflict {
	case "replace", "rename", "fail":
	default:
		return nil, "", invalidRequest("Invalid conflictBehavior. Must be 'replace', 'rename' or 'fail'.")
	}
	return parent, name, nil
}

func (s *Server) createUploadSession(w http.ResponseWriter, r *http.Request, a driveAddress) {
	var in struct {
		Item struct {
			Conflict string `json:"@microsoft.graph.conflictBehavior"`
			Name     string `json:"name"`
		} `json:"item"`
	}
	if e := readJSON(w, r, &in, true); e != nil {
		s.fail(w, e)
		return
	}
	conflict := in.Item.Conflict
	if conflict == "" {
		conflict = "replace"
	}
	var tok [24]byte
	_, _ = rand.Read(tok[:])
	sess := &uploadSession{
		token: hex.EncodeToString(tok[:]), conflict: conflict, app: record(r).AppID,
		expires: s.cfg.now().Add(s.cfg.uploadLifetime), total: -1, hash: sha256.New(),
	}
	e := func() *gErr {
		s.mu.Lock()
		defer s.mu.Unlock()
		parent, name, e := s.uploadTarget(a, conflict)
		if e != nil {
			return e
		}
		if in.Item.Name != "" && in.Item.Name != name {
			return invalidRequest("The name in the request body does not match the name in the URL.")
		}
		sess.team, sess.parent, sess.name = parent.team, parent, name
		sess.label = s.nextID("session-%06d")
		if s.st.sessions == nil {
			s.st.sessions = map[string]*uploadSession{}
		}
		s.st.sessions[sess.token] = sess
		return nil
	}()
	if e != nil {
		s.fail(w, e)
		return
	}
	s.uploadSessions.Add(1)
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"@odata.context":     "https://" + r.Host + "/v1.0/$metadata#microsoft.graph.uploadSession",
		"uploadUrl":          scheme + "://" + r.Host + uploadPrefix + sess.token,
		"expirationDateTime": graphTime(sess.expires),
		"nextExpectedRanges": []string{"0-"},
	})
}

// commitFile stores an uploaded file under parent, honoring the conflict
// behavior. It must be called with the write lock held.
func (s *Server) commitFile(parent *driveItemState, name, conflict, app string, size int64, sum string) (it *driveItemState, created bool, e *gErr) {
	existing := parent.child(name)
	switch {
	case existing != nil && existing.folder:
		return nil, false, invalidRequest("A folder with the provided name already exists.")
	case existing != nil && conflict == "fail":
		return nil, false, &gErr{http.StatusConflict, "nameAlreadyExists", "An item with the same name already exists under the parent."}
	case existing != nil && conflict == "rename":
		stem, ext := name, ""
		if i := strings.LastIndex(name, "."); i > 0 {
			stem, ext = name[:i], name[i:]
		}
		for n := 1; parent.child(name) != nil; n++ {
			name = stem + " " + strconv.Itoa(n) + ext
		}
		existing = nil
	}
	if existing != nil {
		it = existing
		it.version++
		it.modified = s.cfg.now().UTC().Truncate(time.Millisecond)
		it.uploadedBy = app
	} else {
		it, created = s.newItem(parent.team, parent, name, false, app), true
	}
	it.size, it.sha256 = size, sum
	s.uploadsCompleted.Add(1)
	s.uploadBytes.Add(size)
	return it, created, nil
}

// putDriveContent serves PUT …:/content, the single-request upload for
// small (and empty) files.
func (s *Server) putDriveContent(w http.ResponseWriter, r *http.Request) {
	a, ok := parseDriveAddress(r)
	if !ok || a.action != "content" {
		s.fail(w, invalidRequest("Unsupported drive item address."))
		return
	}
	q, ok := s.checkQuery(w, r, "@microsoft.graph.conflictBehavior", "$select")
	if !ok {
		return
	}
	conflict := q.Get("@microsoft.graph.conflictBehavior")
	if conflict == "" {
		conflict = "replace"
	}
	s.mu.RLock()
	_, _, e := s.uploadTarget(a, conflict)
	s.mu.RUnlock()
	if e != nil {
		s.fail(w, e)
		return
	}
	h := sha256.New()
	size, err := io.Copy(h, io.LimitReader(r.Body, MaxSimpleUploadBytes+1))
	if err != nil || size > MaxSimpleUploadBytes {
		s.fail(w, invalidRequest("The content is larger than %d bytes (or unreadable); use an upload session.", MaxSimpleUploadBytes))
		return
	}
	app := record(r).AppID
	var entity map[string]any
	var defaults []string
	status := http.StatusOK
	e = func() *gErr {
		s.mu.Lock()
		defer s.mu.Unlock()
		parent, name, e := s.uploadTarget(a, conflict)
		if e != nil {
			return e
		}
		it, created, e := s.commitFile(parent, name, conflict, app, size, hex.EncodeToString(h.Sum(nil)))
		if e != nil {
			return e
		}
		if created {
			status = http.StatusCreated
		}
		entity, defaults = s.itemEntity(it), itemDefaults(it)
		return nil
	}()
	if e != nil {
		s.fail(w, e)
		return
	}
	s.writeItem(w, r, status, entity, defaults)
}

var contentRangeRE = regexp.MustCompile(`^bytes (\d+)-(\d+)/(\d+)$`)

// handleUpload serves the pre-authorized upload URL: PUT a chunk, GET the
// session status, DELETE to cancel.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, uploadPrefix)
	// The URL itself is the credential; keep it out of the request log.
	record(r).Path = uploadPrefix + "unknown"
	now := s.cfg.now()

	s.mu.RLock()
	sess := s.st.sessions[token]
	s.mu.RUnlock()
	if sess != nil {
		record(r).Path = uploadPrefix + sess.label
		record(r).AppID = sess.app
	}
	if r.Header.Get("Authorization") != "" {
		s.graphError(w, http.StatusUnauthorized, "unauthenticated",
			"The upload URL is pre-authorized; requests to it must not carry an Authorization header.")
		return
	}
	gone := func() {
		s.graphError(w, http.StatusNotFound, "itemNotFound", "The upload session was not found or has expired.")
	}
	if sess == nil {
		gone()
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.closed || !now.Before(sess.expires) {
		gone()
		return
	}
	status := func() map[string]any {
		return map[string]any{
			"expirationDateTime": graphTime(sess.expires),
			"nextExpectedRanges": []string{strconv.FormatInt(sess.received, 10) + "-"},
		}
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, status())
	case http.MethodDelete:
		s.closeSession(sess)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodPut:
		m := contentRangeRE.FindStringSubmatch(r.Header.Get("Content-Range"))
		if m == nil {
			s.fail(w, invalidRequest("A Content-Range header of the form 'bytes {start}-{end}/{total}' is required."))
			return
		}
		start, _ := strconv.ParseInt(m[1], 10, 64)
		end, _ := strconv.ParseInt(m[2], 10, 64)
		total, _ := strconv.ParseInt(m[3], 10, 64)
		length := end - start + 1
		switch {
		case end < start || end >= total:
			s.fail(w, invalidRequest("The Content-Range header is not a valid range of the declared size."))
			return
		case sess.total >= 0 && total != sess.total:
			s.fail(w, invalidRequest("The declared total size differs from earlier fragments of this session."))
			return
		case r.ContentLength != length:
			s.fail(w, invalidRequest("Content-Length does not match the Content-Range header."))
			return
		case length > MaxUploadChunkBytes:
			s.fail(w, invalidRequest("The fragment is larger than the maximum of %d bytes per request.", MaxUploadChunkBytes))
			return
		case end+1 < total && length%UploadChunkMultiple != 0:
			s.fail(w, invalidRequest("Every fragment except the last must be a multiple of %d bytes (320 KiB).", UploadChunkMultiple))
			return
		case start != sess.received:
			out := status()
			out["error"] = map[string]any{
				"code":    "invalidRange",
				"message": "The uploaded fragment overlaps with data that has already been received, or leaves a gap.",
			}
			writeJSON(w, http.StatusRequestedRangeNotSatisfiable, out)
			return
		}
		// Hash into a copy, so a truncated body leaves the session intact.
		next, err := sess.hash.(hash.Cloner).Clone()
		if err != nil {
			s.graphError(w, http.StatusInternalServerError, "generalException", "fakegraph could not clone its hash state.")
			return
		}
		if n, err := io.Copy(next, io.LimitReader(r.Body, length+1)); err != nil || n != length {
			s.fail(w, invalidRequest("The request body does not have the length the Content-Range header declares."))
			return
		}
		sess.hash, sess.total, sess.received = next, total, end+1
		if sess.received < total {
			writeJSON(w, http.StatusAccepted, status())
			return
		}
		var entity map[string]any
		var defaults []string
		code := http.StatusOK
		s.mu.Lock()
		it, created, e := s.commitFile(sess.parent, sess.name, sess.conflict, sess.app, total, hex.EncodeToString(sess.hash.Sum(nil)))
		if e == nil {
			entity, defaults = s.itemEntity(it), itemDefaults(it)
		}
		s.mu.Unlock()
		s.closeSession(sess)
		if e != nil {
			s.fail(w, e)
			return
		}
		if created {
			code = http.StatusCreated
		}
		s.writeItem(w, r, code, entity, defaults)
	default:
		s.graphError(w, http.StatusMethodNotAllowed, "invalidRequest", "Upload sessions accept PUT, GET and DELETE.")
	}
}

// closeSession ends a session; the caller holds sess.mu.
func (s *Server) closeSession(sess *uploadSession) {
	sess.closed = true
	s.mu.Lock()
	delete(s.st.sessions, sess.token)
	s.mu.Unlock()
}

// --- inspection -------------------------------------------------------------

// FileView is an uploaded file as tests see it.
type FileView struct {
	ItemID string `json:"itemId"`
	Name   string `json:"name"`
	// Path is relative to the channel's files folder, "/"-separated.
	Path string `json:"path"`
	Size int64  `json:"size"`
	// SHA256 is the hex digest of the uploaded bytes.
	SHA256    string `json:"sha256"`
	WebDavURL string `json:"webDavUrl"`
	// ETag is SharePoint's form, quotes and braces included: "{GUID},N".
	ETag string `json:"eTag"`
	// UploadedBy is the app that created or last replaced the file.
	UploadedBy string `json:"uploadedBy"`
}

// Files returns every file under a channel's files folder, at any depth,
// sorted by path. It returns nil if there is no such team or channel.
func (s *Server) Files(teamID, channelID string) []FileView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ch := findChannel(s.findAnyTeam(teamID), channelID)
	if ch == nil {
		return nil
	}
	return fileViews(ch)
}

func fileViews(ch *channelState) []FileView {
	out := []FileView{}
	var visit func(it *driveItemState, prefix string)
	visit = func(it *driveItemState, prefix string) {
		for _, c := range it.children {
			if c.folder {
				visit(c, prefix+c.name+"/")
				continue
			}
			out = append(out, FileView{
				ItemID: c.id, Name: c.name, Path: prefix + c.name, Size: c.size, SHA256: c.sha256,
				WebDavURL: c.webDavURL(), ETag: c.eTag(), UploadedBy: c.uploadedBy,
			})
		}
	}
	if ch.folder != nil {
		visit(ch.folder, "")
	}
	slices.SortFunc(out, func(a, b FileView) int { return strings.Compare(a.Path, b.Path) })
	return out
}
