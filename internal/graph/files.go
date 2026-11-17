package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bgfdev/taut/internal/fail"
)

// This file moves files into SharePoint: a channel's files folder, folders
// below it, and resumable uploads.

const (
	permFiles = "Files.ReadWrite.All"
	// permSites is what the reference lists for createUploadSession with
	// application permissions.
	permSites = "Sites.ReadWrite.All"
	// UploadChunkMultiple is the unit Graph requires every upload chunk but
	// the last to be a multiple of: 320 KiB.
	UploadChunkMultiple = 320 * 1024
	// DefaultUploadChunk is the chunk size Upload uses when none is given:
	// 10 MiB.
	DefaultUploadChunk = 32 * UploadChunkMultiple
	// MaxUploadChunk is the largest chunk sent in one request. Graph
	// requires each request to be less than 60 MiB, so this is the largest
	// multiple of 320 KiB below that.
	MaxUploadChunk = 191 * UploadChunkMultiple

	itemSelect  = "id,name,size,eTag,webUrl,webDavUrl"
	uploadLabel = "(upload session URL)"
)

// ErrUploadSessionExpired matches (with errors.Is) an upload that cannot be
// continued because its session is gone; the whole upload must be restarted.
var ErrUploadSessionExpired = errors.New("graph: upload session expired")

// IsUploadSessionExpired reports whether err means an upload session
// expired or was lost, so the caller should start the upload again.
func IsUploadSessionExpired(err error) bool { return errors.Is(err, ErrUploadSessionExpired) }

// DriveItem is a file or folder in SharePoint.
type DriveItem struct {
	ID      string
	DriveID string
	Name    string
	WebURL  string
	// WebDavURL is what a message attachment's contentUrl must be. Graph
	// returns it only when selected; Upload and EnsureFolder select it,
	// FilesFolder does not.
	WebDavURL string
	// ETag is SharePoint's form, quotes and braces included: "{GUID},N".
	ETag string
	Size int64
}

// UnmarshalJSON reads Graph's driveItem shape.
func (d *DriveItem) UnmarshalJSON(data []byte) error {
	var w struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		WebURL    string `json:"webUrl"`
		WebDavURL string `json:"webDavUrl"`
		ETag      string `json:"eTag"`
		Size      int64  `json:"size"`
		Parent    struct {
			DriveID string `json:"driveId"`
		} `json:"parentReference"`
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	*d = DriveItem{ID: w.ID, DriveID: w.Parent.DriveID, Name: w.Name, WebURL: w.WebURL, WebDavURL: w.WebDavURL, ETag: w.ETag, Size: w.Size}
	return nil
}

// ETagGUID returns the item's unique ID from its eTag: the bare GUID in
// lower case, which is what a message attachment's id must be. It is empty
// if the eTag is not in SharePoint's "{GUID},version" form.
func (d DriveItem) ETagGUID() string {
	s := strings.Trim(strings.TrimPrefix(d.ETag, "W/"), `"`)
	s, _, _ = strings.Cut(s, ",")
	s = strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(s, "{"), "}"))
	if len(s) != 36 || strings.Count(s, "-") != 4 || strings.Trim(s, "0123456789abcdef-") != "" {
		return ""
	}
	return s
}

func itemPath(driveID, itemID string) string {
	return "/v1.0/drives/" + url.PathEscape(driveID) + "/items/" + url.PathEscape(itemID)
}

// childPath addresses a path below an item: …/items/{id}:/a/b.
func childPath(driveID, parentID, rel string) string {
	segs := strings.Split(rel, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return itemPath(driveID, parentID) + ":/" + strings.Join(segs, "/")
}

// childLabel is childPath for error text, without the name: file names are
// customer data.
func childLabel(driveID, parentID, suffix string) string {
	return itemPath(driveID, parentID) + ":/(name)" + suffix
}

// FilesFolder returns the SharePoint folder behind a channel's Files tab.
// For a channel created moments ago the folder may not exist yet: that is
// graph.files_folder_not_ready, which matches ErrNotFound (IsNotFound), and
// the caller should wait and try again.
func (c *Client) FilesFolder(ctx context.Context, teamID, channelID string) (DriveItem, error) {
	classify := func(e *APIError) *fail.Error {
		if e.Status != http.StatusNotFound {
			return nil
		}
		return fail.Wrap(e, "graph.files_folder_not_ready",
			"The channel's files folder is not available",
			fmt.Sprintf("Team %s, channel %s: %s%s", teamID, channelID, e.detail(), e.ref()),
			"SharePoint can take a few minutes to provision the folder of a new channel. Wait and run `taut migrate` again; "+
				"if it persists, check that the team and channel still exist.")
	}
	var d DriveItem
	return d, c.do(ctx, request{method: "GET", path: channelPath(teamID, channelID) + "/filesFolder", permission: permFiles, classify: classify}, &d)
}

func (c *Client) fileNameErrors(e *APIError) *fail.Error {
	if e.Status == http.StatusBadRequest && strings.Contains(strings.ToLower(e.Message), "name") {
		return fail.Wrap(e, "graph.file_name_invalid",
			"SharePoint does not allow this file or folder name",
			e.detail()+e.ref(),
			"This is a bug in Taut (names are sanitized before upload); please report it with --verbose output.")
	}
	return nil
}

// itemAt fetches the item at a path below parentID; found is false on 404.
func (c *Client) itemAt(ctx context.Context, driveID, parentID, rel string) (item DriveItem, found bool, err error) {
	err = c.do(ctx, request{
		method: "GET", path: childPath(driveID, parentID, rel), label: childLabel(driveID, parentID, ""),
		query: url.Values{"$select": {itemSelect}}, permission: permFiles,
	}, &item)
	item.DriveID = driveID
	switch {
	case err == nil:
		return item, true, nil
	case IsNotFound(err):
		return DriveItem{}, false, nil
	}
	return DriveItem{}, false, err
}

// item fetches an item by ID with the fields Taut needs.
func (c *Client) item(ctx context.Context, driveID, itemID string) (DriveItem, error) {
	var d DriveItem
	err := c.do(ctx, request{method: "GET", path: itemPath(driveID, itemID), query: url.Values{"$select": {itemSelect}}, permission: permFiles}, &d)
	d.DriveID = driveID
	return d, err
}

// EnsureFolder returns the folder called name under parentID, creating it
// if it is missing. It is safe to call concurrently and to repeat: losing a
// creation race (409) is resolved by reading the folder again.
func (c *Client) EnsureFolder(ctx context.Context, driveID, parentID, name string) (DriveItem, error) {
	if it, found, err := c.itemAt(ctx, driveID, parentID, name); err != nil || found {
		return it, err
	}
	body := map[string]any{"name": name, "folder": map[string]any{}, "@microsoft.graph.conflictBehavior": "fail"}
	var created DriveItem
	err := c.do(ctx, request{
		method: "POST", path: itemPath(driveID, parentID) + "/children", body: body, permission: permFiles,
		// A repeat after a lost response answers 409, handled below.
		retryAmbiguous: true, classify: c.fileNameErrors,
	}, &created)
	if err != nil && !IsConflict(err) {
		return DriveItem{}, err
	}
	it, found, err := c.itemAt(ctx, driveID, parentID, name)
	if err == nil && !found {
		err = fail.New("graph.bad_response",
			"SharePoint reported a folder as existing but it cannot be read",
			"Creating the folder under item "+parentID+" answered 409, yet reading it back answered 404.",
			"Run the command again.")
	}
	return it, err
}

// UploadSpec describes a file to upload.
type UploadSpec struct {
	DriveID string
	// ParentID is the folder to upload into.
	ParentID string
	// Name must be a valid SharePoint file name; see SanitizeFileName.
	Name string
	// Size is the exact number of bytes Content delivers. It is required:
	// Graph needs the total up front.
	Size    int64
	Content io.Reader
	// ChunkSize is the bytes per request: a multiple of UploadChunkMultiple,
	// at most MaxUploadChunk. Zero means DefaultUploadChunk.
	ChunkSize int
	// Conflict is what happens when the name is taken: "replace" (the
	// default, which makes repeating an upload harmless) or "fail".
	// "rename" is not offered: a retried upload would create duplicates.
	Conflict string
}

// Upload stores a file in SharePoint and returns it with its WebDavURL and
// ETag, ready to be attached to a message.
//
// Content is streamed through one buffer of ChunkSize bytes, so memory use
// does not depend on Size. Files go through an upload session; chunks are
// sent to the session's pre-authorized URL without an Authorization header.
// A chunk that is throttled is retried. After a failure that may have
// committed (transport error, 5xx, 416) Upload asks the session which bytes
// it holds and continues from there rather than re-sending blindly; if the
// session is gone because the upload in fact completed, the stored file is
// found and returned. Empty files use a single PUT, since a session cannot
// carry zero bytes.
//
// Errors: graph.upload_session_expired (IsUploadSessionExpired) when the
// session expired or lost track, in which case the caller restarts the
// upload with a fresh reader; graph.upload_size_mismatch when Content
// delivers fewer or more than Size bytes (the session is cancelled and
// nothing is stored); graph.file_name_invalid; graph.conflict when Conflict
// is "fail" and the name is taken. File content never appears in errors.
func (c *Client) Upload(ctx context.Context, spec UploadSpec) (DriveItem, error) {
	chunk := spec.ChunkSize
	if chunk == 0 {
		chunk = DefaultUploadChunk
	}
	conflict := spec.Conflict
	if conflict == "" {
		conflict = "replace"
	}
	switch {
	case spec.DriveID == "" || spec.ParentID == "" || spec.Name == "":
		return DriveItem{}, usage("An upload is incomplete", "The drive ID, parent folder ID or file name is missing.",
			"This is a bug in Taut; please report it.")
	case spec.Size < 0 || (spec.Size > 0 && spec.Content == nil):
		return DriveItem{}, usage("An upload has no usable size or content", "UploadSpec.Size is negative, or Content is nil.",
			"This is a bug in Taut; please report it.")
	case chunk < 0 || chunk%UploadChunkMultiple != 0 || chunk > MaxUploadChunk:
		return DriveItem{}, usage("The upload chunk size is not valid",
			fmt.Sprintf("The chunk size is %d bytes; Graph requires a multiple of %d bytes (320 KiB) of at most %d bytes (under 60 MiB).", chunk, UploadChunkMultiple, MaxUploadChunk),
			"Set the upload chunk size in the config file to a multiple of 320 KiB, for example 10485760.")
	case conflict != "replace" && conflict != "fail":
		return DriveItem{}, usage("The upload conflict behavior is not supported", "UploadSpec.Conflict must be \"replace\" or \"fail\".",
			"This is a bug in Taut; please report it.")
	}
	if spec.Size == 0 {
		return c.uploadEmpty(ctx, spec, conflict)
	}

	// With "replace", remember what is there now: after a lost final
	// response this tells our upload apart from the file it replaced.
	var before DriveItem
	var existed bool
	if conflict == "replace" {
		var err error
		if before, existed, err = c.itemAt(ctx, spec.DriveID, spec.ParentID, spec.Name); err != nil {
			return DriveItem{}, err
		}
	}

	var sess struct {
		UploadURL string `json:"uploadUrl"`
	}
	err := c.do(ctx, request{
		method: "POST", path: childPath(spec.DriveID, spec.ParentID, spec.Name) + ":/createUploadSession",
		label: childLabel(spec.DriveID, spec.ParentID, ":/createUploadSession"),
		body:  map[string]any{"item": map[string]any{"@microsoft.graph.conflictBehavior": conflict, "name": spec.Name}},
		// A session that is created twice is merely abandoned once.
		permission: permSites, retryAmbiguous: true, classify: c.fileNameErrors,
	}, &sess)
	if err != nil {
		return DriveItem{}, err
	}
	up := &upload{c: c, spec: spec, url: sess.UploadURL, before: before, existed: existed}

	buf := make([]byte, min(int64(chunk), spec.Size))
	for off := int64(0); off < spec.Size; {
		n := int(min(int64(len(buf)), spec.Size-off))
		last := off+int64(n) == spec.Size
		if err := up.fill(buf[:n], last); err != nil {
			up.cancel(ctx)
			return DriveItem{}, err
		}
		itemID, err := up.sendChunk(ctx, buf[:n], off)
		if err != nil {
			if !IsUploadSessionExpired(err) {
				up.cancel(ctx)
			}
			return DriveItem{}, err
		}
		if itemID != "" {
			return c.item(ctx, spec.DriveID, itemID)
		}
		off += int64(n)
	}
	// Every byte was acknowledged but no chunk response carried the item
	// (its response was lost and the session has since closed).
	return up.stored(ctx)
}

// uploadEmpty stores a zero-byte file with a single PUT.
func (c *Client) uploadEmpty(ctx context.Context, spec UploadSpec, conflict string) (DriveItem, error) {
	if spec.Content != nil {
		var one [1]byte
		if n, _ := io.ReadFull(spec.Content, one[:]); n > 0 {
			return DriveItem{}, sizeMismatch(spec, "more than")
		}
	}
	var d DriveItem
	err := c.do(ctx, request{
		method: "PUT", path: childPath(spec.DriveID, spec.ParentID, spec.Name) + ":/content",
		label:  childLabel(spec.DriveID, spec.ParentID, ":/content"),
		query:  url.Values{"@microsoft.graph.conflictBehavior": {conflict}},
		hasRaw: true, permission: permFiles, classify: c.fileNameErrors,
	}, &d)
	if err != nil {
		return DriveItem{}, err
	}
	return c.item(ctx, spec.DriveID, d.ID)
}

func sizeMismatch(spec UploadSpec, how string) *fail.Error {
	return fail.New("graph.upload_size_mismatch",
		"A file did not have the size that was announced",
		fmt.Sprintf("The content delivered %s the announced %d bytes; the upload was abandoned and nothing was stored.", how, spec.Size),
		"The staged copy of the file is probably incomplete or changed. Run `taut migrate` again to fetch it anew.")
}

// upload is one upload session in progress.
type upload struct {
	c       *Client
	spec    UploadSpec
	url     string
	before  DriveItem
	existed bool
}

// fill reads exactly len(buf) bytes of content and, before the last chunk,
// checks that the content ends there.
func (u *upload) fill(buf []byte, last bool) error {
	if _, err := io.ReadFull(u.spec.Content, buf); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return sizeMismatch(u.spec, "fewer than")
		}
		return readFailed(err)
	}
	if !last {
		return nil
	}
	var one [1]byte
	switch n, err := io.ReadFull(u.spec.Content, one[:]); {
	case n > 0:
		return sizeMismatch(u.spec, "more than")
	case err != nil && !errors.Is(err, io.EOF):
		return readFailed(err)
	}
	return nil
}

func readFailed(err error) *fail.Error {
	return fail.Wrap(err, "graph.upload_read_failed",
		"A file could not be read while uploading it", "",
		"Check the staging storage is reachable, then run `taut migrate` again.")
}

func (u *upload) expired(e *APIError, why string) *fail.Error {
	var cause error = ErrUploadSessionExpired
	if e != nil {
		e.kind = ErrUploadSessionExpired
		cause = e
	}
	return fail.Wrap(cause, "graph.upload_session_expired",
		"An upload could not be continued",
		why+fmt.Sprintf(" (drive %s, folder %s, %d bytes).", u.spec.DriveID, u.spec.ParentID, u.spec.Size),
		"Run `taut migrate` again; the file is uploaded again from the start.")
}

// cancel abandons the session, best effort, even if ctx has ended.
func (u *upload) cancel(ctx context.Context) {
	cctx, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer stop()
	_ = u.c.do(cctx, request{method: "DELETE", absolute: u.url, external: true, label: uploadLabel, noBlindRetry: true}, nil)
}

// sendChunk uploads buf as bytes [off, off+len(buf)) and returns the stored
// item's ID if this chunk completed the file.
func (u *upload) sendChunk(ctx context.Context, buf []byte, off int64) (itemID string, err error) {
	c := u.c
	end := off + int64(len(buf))
	start := off // first byte still to be sent
	var waited time.Duration
	for attempt := 1; ; attempt++ {
		var status int
		var item DriveItem
		err := c.do(ctx, request{
			method: "PUT", absolute: u.url, external: true, label: uploadLabel, noBlindRetry: true,
			raw: buf[start-off:], hasRaw: true, status: &status,
			send: http.Header{"Content-Range": {fmt.Sprintf("bytes %d-%d/%d", start, end-1, u.spec.Size)}},
		}, &item)
		if err == nil {
			if status == http.StatusOK || status == http.StatusCreated {
				return item.ID, nil
			}
			return "", nil
		}
		var api *APIError
		if !errors.As(err, &api) {
			return "", err // canceled
		}
		if api.Status == http.StatusNotFound && attempt == 1 {
			return "", u.expired(api, "Graph no longer knows the upload session: it expired or was cancelled")
		}
		ambiguous := api.Status == 0 || api.Status >= 500 || api.Status == http.StatusRequestedRangeNotSatisfiable || api.Status == http.StatusNotFound
		if !ambiguous || attempt >= c.retry.MaxAttempts {
			return "", err
		}
		delay := c.backoff(attempt)
		if waited+delay > c.retry.MaxTotalWait {
			return "", err
		}
		if serr := c.sleep(ctx, delay); serr != nil {
			return "", canceled(ctx, "waiting to resume an upload")
		}
		waited += delay

		// The chunk may or may not have been stored. Ask before re-sending.
		next, gone, serr := u.status(ctx)
		switch {
		case serr != nil:
			return "", serr
		case gone:
			// Either the file is complete and the session closed, or the
			// session expired.
			it, serr := u.stored(ctx)
			return it.ID, serr
		case next == end:
			return "", nil // accepted; the response was lost
		case next >= start && next < end:
			start = next
		default:
			return "", u.expired(nil, fmt.Sprintf("The upload session expects byte %d next, which is outside the chunk being sent (%d-%d)", next, off, end-1))
		}
	}
}

// status asks the session which byte it expects next. gone reports that
// the session no longer exists.
func (u *upload) status(ctx context.Context) (next int64, gone bool, err error) {
	var st struct {
		Next []string `json:"nextExpectedRanges"`
	}
	err = u.c.do(ctx, request{method: "GET", absolute: u.url, external: true, label: uploadLabel}, &st)
	switch {
	case IsNotFound(err):
		return 0, true, nil
	case err != nil:
		return 0, false, err
	case len(st.Next) == 0:
		return u.spec.Size, false, nil
	}
	first, _, _ := strings.Cut(st.Next[0], "-")
	if next, err = strconv.ParseInt(first, 10, 64); err != nil {
		return 0, false, fail.New("graph.bad_response",
			"Microsoft Graph returned an upload status Taut could not read",
			"nextExpectedRanges of the upload session is not a byte range.",
			"Run the command again. If it repeats, report it with --verbose output.")
	}
	return next, false, nil
}

// stored finds the file after the session closed without telling us the
// outcome. It only accepts an item of the right size that is not the one
// that was there before the upload began.
func (u *upload) stored(ctx context.Context) (DriveItem, error) {
	it, found, err := u.c.itemAt(ctx, u.spec.DriveID, u.spec.ParentID, u.spec.Name)
	switch {
	case err != nil:
		return DriveItem{}, err
	case found && it.Size == u.spec.Size && !(u.existed && it.ETag == u.before.ETag):
		return it, nil
	}
	return DriveItem{}, u.expired(nil, "The upload session is gone and the file is not in place")
}

// reservedFileNames may not be used as a SharePoint file name, in any case.
var reservedFileNames = func() map[string]bool {
	m := map[string]bool{".lock": true, "con": true, "prn": true, "aux": true, "nul": true, "desktop.ini": true}
	for i := 0; i <= 9; i++ {
		m["com"+strconv.Itoa(i)] = true
		m["lpt"+strconv.Itoa(i)] = true
	}
	return m
}()

// maxFileNameBytes caps sanitized names, leaving room for the folder path
// within SharePoint's 400-character limit.
const maxFileNameBytes = 200

// SanitizeFileName turns any string into a valid SharePoint file name. The
// result is deterministic: characters SharePoint forbids (" * : < > ? / \ |
// and control characters) become "_", surrounding spaces and trailing
// periods are removed, a leading "~$" and the reserved names (.lock, CON,
// PRN, AUX, NUL, COM0–9, LPT0–9, desktop.ini, anything containing _vti_)
// are defused, and the name is cut to 200 bytes on a rune boundary, keeping
// its extension. An empty result becomes "file". Distinct inputs can map to
// the same output; callers that need uniqueness add their own suffix.
func SanitizeFileName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || strings.ContainsRune("\"*:<>?/\\|", r) {
			return '_'
		}
		return r
	}, strings.ToValidUTF8(name, "_"))
	trim := func(s string) string { return strings.TrimRight(strings.TrimSpace(s), ". ") }
	name = trim(name)

	// Find "_vti_" case-insensitively on the name's own bytes. Lower-casing
	// the whole name first and slicing by that index is wrong: case folding
	// changes the byte length of some characters.
	for {
		i := indexFoldASCII(name, "_vti_")
		if i < 0 {
			break
		}
		name = name[:i] + "-vti-" + name[i+len("_vti_"):]
	}
	if strings.HasPrefix(name, "~$") {
		name = "_" + name[1:]
	}
	if reservedFileNames[strings.ToLower(name)] {
		name = "_" + name
	}

	if len(name) > maxFileNameBytes {
		ext := ""
		if i := strings.LastIndex(name, "."); i > 0 && len(name)-i <= 20 {
			name, ext = name[:i], name[i:]
		}
		cut := maxFileNameBytes - len(ext)
		for cut > 0 && !utf8.RuneStart(name[cut]) {
			cut--
		}
		// Cutting can expose a trailing space or period.
		name = trim(name[:cut]) + ext
		name = trim(name)
	}
	if name == "" {
		return "file"
	}
	return name
}

// indexFoldASCII returns the byte index of the first occurrence of pat in s,
// comparing ASCII letters case-insensitively and everything else exactly.
// pat must be ASCII.
func indexFoldASCII(s, pat string) int {
	lower := func(b byte) byte {
		if b >= 'A' && b <= 'Z' {
			return b + 'a' - 'A'
		}
		return b
	}
	for i := 0; i+len(pat) <= len(s); i++ {
		j := 0
		for j < len(pat) && lower(s[i+j]) == lower(pat[j]) {
			j++
		}
		if j == len(pat) {
			return i
		}
	}
	return -1
}
