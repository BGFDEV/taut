package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/graph"
	"github.com/bgfdev/taut/internal/slack"
	"github.com/bgfdev/taut/internal/staging"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/transform"
)

// Audit log operations for files.
const (
	opFileUpload = "file.upload"
	opFileFail   = "file.fail"
)

// filesFolderName is the subfolder of a channel's Files tab that Taut
// uploads into, so migrated files are easy to tell from new ones.
const filesFolderName = "Slack"

// fileIDPattern is what a Slack file ID must look like to be used in a
// staging key and a file marker. The transform applies the same rule.
var fileIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// SlackFiles fetches file content from Slack. The worker does not hold
// Slack credentials; the implementation decides how the request is
// authorized (an export URL carries its own token; otherwise the
// egress proxy adds one).
type SlackFiles interface {
	// Open starts a download. The caller closes the body. size is the
	// Content-Length if the server stated one, else -1.
	Open(ctx context.Context, m *config.Config, rawURL string) (body io.ReadCloser, size int64, contentType string, err error)
}

// ValidSlackFileURL reports whether a URL from an export is a file on
// Slack's own file hosts. An export is data from outside: a file object's
// URL could name anything, including an internal host or Slack's Web API,
// and fetching it would hand the response (or a token) to whoever wrote the
// URL. Only Slack's file hosts, over https, on their file paths, are
// fetched.
func ValidSlackFileURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host != "files.slack.com" && !strings.HasSuffix(host, ".slack-files.com") {
		return false
	}
	return strings.HasPrefix(u.Path, "/files-pri/") || strings.HasPrefix(u.Path, "/files-tmb/")
}

// fileKey is where a file's content is staged.
func fileKey(runID, channelID, fileID string) string {
	return staging.RunPrefix(runID) + "files/" + channelID + "/" + fileID
}

// skipReason says why a file is not copied, or "" if it should be.
func skipReason(f slack.File) (code, reason string) {
	switch {
	case !fileIDPattern.MatchString(f.ID):
		return "no_id", "the export gives the file no usable ID"
	case f.IsHidden():
		return "hidden_by_limit", "beyond the Slack plan's history limit"
	case f.IsTombstone():
		return "deleted", "deleted in Slack"
	case f.IsAccessStub():
		return "file_access", "shared from another workspace; the export has only a reference to it"
	case f.IsExternal || f.Mode == slack.FileModeExternal:
		return "external", "hosted outside Slack; its link is kept in the message"
	case !f.Downloadable():
		return "not_downloadable", "the export has no download link for it"
	}
	return "", ""
}

// files moves one day's files of a channel: Slack → encrypted staging → the
// channel's SharePoint folder.
//
// A file is recorded before anything is fetched, and each step (staged,
// uploaded) is recorded as it completes, so a crash resumes at the step it
// interrupted. Both steps are safe to repeat: staging a file again
// overwrites the same object, and an upload replaces the item of the same
// name.
//
// Failures are sorted by what they are about. A file Slack no longer has, or
// that SharePoint refuses outright, is settled as failed with its reason and
// the lane carries on; its messages will say the file could not be migrated.
// Anything that is not about the one file (Slack refusing the token, an
// outage, a stalled connection) fails the attempt instead, so it is retried
// and, if it persists, stops the lane before the channel is opened, while
// nothing irreversible has been written.
func (d *Worker) files(ctx context.Context, rs *runState, lease *store.Lease, b *store.Batch) (err error) {
	channelID := lease.Key
	settled := 0
	defer func() { err = withProgress(err, settled) }()

	in, err := d.inputKey(ctx, rs.run.ID, store.StageExtract, channelID, b.Seq)
	if err != nil {
		return err
	}
	msgs, err := readBatch[slack.Message](ctx, rs.blobs, in)
	if err != nil {
		return err
	}

	// 1. Record every file this day refers to. A file already recorded by
	// an earlier day of this channel keeps its row, and is that batch's
	// responsibility. Within the day, a downloadable occurrence of a file
	// wins over one that is not (the same file can appear once as a stub
	// and once in full).
	urls := map[string]string{}
	index := map[string]int{}
	var rows []store.FileMapping
	for _, msg := range msgs {
		// Only messages that are converted reference their files; a
		// placeholder (a deleted thread starter, a huddle) does not.
		if transform.Classify(msg).Disposition != transform.Import {
			continue
		}
		for _, f := range msg.Files {
			id := f.ID
			if !fileIDPattern.MatchString(id) {
				continue
			}
			row := store.FileMapping{
				ChannelID: channelID, SlackID: id, Name: firstNonEmpty(f.Name, f.Title, id),
				Mimetype: f.Mimetype, Size: f.Size, State: store.FilePending, BatchSeq: b.Seq,
			}
			if code, reason := skipReason(f); code != "" {
				row.State, row.ErrorCode, row.Error = store.FileSkipped, code, reason
			}
			if i, seen := index[id]; seen {
				if rows[i].State == store.FileSkipped && row.State == store.FilePending {
					rows[i] = row
					urls[id] = firstNonEmpty(f.URLPrivateDownload, f.URLPrivate)
				}
				continue
			}
			index[id] = len(rows)
			rows = append(rows, row)
			urls[id] = firstNonEmpty(f.URLPrivateDownload, f.URLPrivate)
		}
	}
	if len(rows) > 0 {
		if err := d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error { return tx.PutFiles(ctx, rows) }); err != nil {
			return err
		}
	}

	// 2. Work through the files this batch owns.
	owned, err := d.cfg.Store.FilesInBatch(ctx, rs.run.ID, channelID, b.Seq)
	if err != nil {
		return err
	}
	stats := map[string]int{}
	for _, f := range owned {
		if err := ctx.Err(); err != nil {
			return err
		}
		source := "file/" + channelID + "/" + f.SlackID
		switch f.State {
		case store.FileUploaded:
			stats["already_uploaded"]++
			continue
		case store.FileSkipped:
			stats["skipped"]++
			continue
		}

		moved, item, err := d.moveFile(ctx, rs, lease, f, urls[f.SlackID])
		var gone *fileGone
		if errors.As(err, &gone) {
			if err := d.settleFile(ctx, lease, f, store.FileFailed, gone.code, gone.reason, opFileFail, source); err != nil {
				return err
			}
			stats["failed"]++
			settled++
			continue
		}
		if err != nil {
			return err
		}
		err = d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error {
			if err := tx.MarkFileUploaded(ctx, channelID, f.SlackID, item.DriveID, item.ID, item.WebDavURL, item.ETagGUID()); err != nil {
				return err
			}
			return tx.Log(ctx, d.entry(lease, opFileUpload, source, item.ID, moved.SHA256))
		})
		if err != nil {
			return err
		}
		d.cfg.Observer.Count(ctx, store.StageFiles, EventFileUploaded, 1)
		d.cfg.Observer.Count(ctx, store.StageFiles, EventFileBytes, moved.StagedBytes)
		stats["uploaded"]++
		settled++
	}

	statsJSON, _ := json.Marshal(stats)
	return d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error {
		return tx.CompleteBatch(ctx, b.Seq, "", len(owned), statsJSON)
	})
}

// fileGone means this one file can never be moved; retrying is pointless.
type fileGone struct{ code, reason string }

func (e *fileGone) Error() string { return e.reason }

// moveFile stages a file if it is not staged yet and uploads it. It returns
// *fileGone for failures that are about the file itself.
func (d *Worker) moveFile(ctx context.Context, rs *runState, lease *store.Lease, f store.FileMapping, rawURL string) (store.FileMapping, graph.DriveItem, error) {
	var err error
	if f.State == store.FilePending || f.State == store.FileFailed {
		if f, err = d.stageWithRetry(ctx, rs, lease, f, rawURL); err != nil {
			return f, graph.DriveItem{}, err
		}
	}
	item, err := d.upload(ctx, rs, lease, f)
	if err != nil && needsRestage(err) {
		// The staged copy is missing, damaged, or not the length that was
		// recorded. Fetch it again rather than failing on it forever.
		if f, err = d.stageWithRetry(ctx, rs, lease, f, rawURL); err != nil {
			return f, graph.DriveItem{}, err
		}
		item, err = d.upload(ctx, rs, lease, f)
	}
	if err != nil {
		if refused(err) {
			return f, graph.DriveItem{}, &fileGone{firstNonEmpty(code(err), "upload_refused"), "SharePoint refused the file: " + describe(err)}
		}
		return f, graph.DriveItem{}, err
	}
	// A message can only attach an item by these two values. Recording an
	// upload without them would count a file as moved that no message can
	// show.
	if item.ETagGUID() == "" || item.WebDavURL == "" {
		return f, graph.DriveItem{}, fmt.Errorf("SharePoint returned file %s without the identifiers needed to attach it", f.SlackID)
	}
	return f, item, nil
}

// needsRestage reports whether an upload failed because of the staged copy
// rather than the destination.
func needsRestage(err error) bool {
	if errors.Is(err, staging.ErrNotExist) {
		return true
	}
	switch code(err) {
	case "staging.corrupt", "graph.upload_size_mismatch", "graph.upload_read_failed":
		return true
	}
	return false
}

// refused reports whether SharePoint rejected this file in a way that
// retrying will not change: a client error that is not about credentials,
// throttling, or something not existing yet.
func refused(err error) bool {
	var api *graph.APIError
	if !errors.As(err, &api) {
		return false
	}
	switch api.Status {
	case http.StatusBadRequest, http.StatusConflict, http.StatusRequestEntityTooLarge,
		http.StatusUnprocessableEntity, http.StatusLocked, http.StatusInsufficientStorage:
		return true
	}
	return false
}

// stageWithRetry stages a file, retrying ordinary transient failures in
// place so that one flaky download does not cost the whole batch an attempt.
func (d *Worker) stageWithRetry(ctx context.Context, rs *runState, lease *store.Lease, f store.FileMapping, rawURL string) (store.FileMapping, error) {
	var err error
	for try := 0; ; try++ {
		var staged store.FileMapping
		staged, err = d.stage(ctx, rs, lease, f, rawURL)
		if err == nil {
			return staged, nil
		}
		var gone *fileGone
		if errors.As(err, &gone) || isNotMine(err) || errors.Is(err, store.ErrFenced) || code(err) == "slack.files_unauthorized" || try >= 2 || ctx.Err() != nil {
			return f, err
		}
		wait := time.Duration(1+2*try) * time.Second
		if d.cfg.FileRetryWait > 0 {
			wait = d.cfg.FileRetryWait
		}
		select {
		case <-ctx.Done():
			return f, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// errSlackUnauthorized is returned when Slack will not serve files with the
// credentials at hand. It is a problem with the run's setup, not with any
// one file, so it fails the attempt: settling every file as "could not be
// migrated" and carrying on would import the whole channel without its
// files, and that cannot be undone.
func errSlackUnauthorized(why string) error {
	return fail.New("slack.files_unauthorized", "Slack would not serve a file", why,
		"If the export is more than a few days old its file links may have expired: export again, or enable source.slack.api with a token that has files:read. Then run `taut migrate`.")
}

// stage downloads a file from Slack into encrypted staging and records it.
func (d *Worker) stage(ctx context.Context, rs *runState, lease *store.Lease, f store.FileMapping, rawURL string) (store.FileMapping, error) {
	if rawURL == "" {
		return f, &fileGone{"no_url", "the export has no download link for it"}
	}
	if !ValidSlackFileURL(rawURL) {
		return f, &fileGone{"bad_url", "the export's link for the file does not point at Slack's file host"}
	}
	if d.cfg.Slack == nil {
		return f, notMine(errors.New("this worker has no way to reach Slack's file host configured"))
	}

	// A connection that stops delivering without closing would otherwise
	// hold the lane forever: the lease heartbeat keeps running. Cancel the
	// download if nothing arrives for a while.
	dctx, cancel := context.WithCancel(ctx)
	defer cancel()
	idle := d.cfg.FileIdleTimeout
	watchdog := time.AfterFunc(idle, cancel)
	defer watchdog.Stop()

	body, size, contentType, err := d.cfg.Slack.Open(dctx, rs.m, rawURL)
	if err != nil {
		var gone *fileGone
		if errors.As(err, &gone) && gone.code == "not_authorized" {
			return f, errSlackUnauthorized(gone.reason + ".")
		}
		return f, stalled(ctx, dctx, err)
	}
	defer body.Close()

	// Slack answers a request it does not authorize with a sign-in page
	// and status 200. Storing that as the file would be silent corruption.
	// For a file that is itself HTML the content type proves nothing, so
	// the length must match what Slack said the file is.
	if strings.HasPrefix(strings.ToLower(contentType), "text/html") {
		htmlFile := isHTMLFile(f)
		if !htmlFile || (size >= 0 && f.Size > 0 && size != f.Size) {
			return f, errSlackUnauthorized("It answered with a sign-in page instead of the file.")
		}
	}

	key := fileKey(rs.run.ID, f.ChannelID, f.SlackID)
	hashed, digest := staging.SHA256Reader(&idleReader{r: body, timer: watchdog, idle: idle})
	n, err := rs.blobs.Put(dctx, key, hashed)
	if err != nil {
		// Includes a download cut off part-way: nothing is stored.
		return f, stalled(ctx, dctx, fmt.Errorf("staging file %s: %w", f.SlackID, err))
	}
	if n == 0 && f.Size > 0 {
		return f, fmt.Errorf("file %s arrived empty from Slack, which lists it as %d bytes", f.SlackID, f.Size)
	}
	sum := digest()
	err = d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error {
		return tx.MarkFileStaged(ctx, f.ChannelID, f.SlackID, key, sum, n)
	})
	if err != nil {
		return f, err
	}
	f.State, f.StagingKey, f.SHA256, f.StagedBytes = store.FileStaged, key, sum, n
	return f, nil
}

// stalled rewrites an error caused by the idle watchdog (the download's own
// context was cancelled while the batch's was not) into one that says so.
func stalled(parent, download context.Context, err error) error {
	if parent.Err() == nil && download.Err() != nil {
		return errors.New("the download from Slack stalled and was abandoned")
	}
	return err
}

// idleReader resets a watchdog timer each time data arrives.
type idleReader struct {
	r     io.Reader
	timer *time.Timer
	idle  time.Duration
}

func (i *idleReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	if n > 0 {
		i.timer.Reset(i.idle)
	}
	return n, err
}

// isHTMLFile reports whether an HTML body is what this file is expected to
// be: canvases and posts are downloaded as HTML, and so are saved web pages.
func isHTMLFile(f store.FileMapping) bool {
	n := strings.ToLower(f.Name)
	return strings.HasSuffix(n, ".html") || strings.HasSuffix(n, ".htm") ||
		strings.HasPrefix(strings.ToLower(f.Mimetype), "text/html") || f.Mimetype == "application/vnd.slack-docs"
}

// upload sends a staged file to the channel's SharePoint folder.
func (d *Worker) upload(ctx context.Context, rs *runState, lease *store.Lease, f store.FileMapping) (graph.DriveItem, error) {
	g, err := d.client(ctx, rs)
	if err != nil {
		return graph.DriveItem{}, err
	}
	driveID, folderID, err := d.channelFolder(ctx, rs, lease, g, f.ChannelID)
	if err != nil {
		return graph.DriveItem{}, err
	}
	src, err := rs.blobs.Get(ctx, f.StagingKey)
	if err != nil {
		return graph.DriveItem{}, err
	}
	defer src.Close()
	// The Slack file ID leads the name: Slack allows many files with the
	// same name in a channel, a SharePoint folder does not.
	name := graph.SanitizeFileName(f.SlackID + "-" + f.Name)
	return g.Upload(ctx, graph.UploadSpec{
		DriveID: driveID, ParentID: folderID, Name: name, Size: f.StagedBytes, Content: src, Conflict: "replace",
	})
}

// errFolderUnavailable settles a file whose channel has no usable files
// folder: a legacy channel, in migration mode, whose folder Graph would not
// return for as long as the worker waits for one.
var errFolderUnavailable = &fileGone{"folder_unavailable", "the channel's files folder is not available while the channel is in migration mode"}

func (rs *runState) folderIsUnavailable(channelID string) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.noFolder[channelID]
}

func (rs *runState) markFolderUnavailable(channelID string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.noFolder == nil {
		rs.noFolder = map[string]bool{}
	}
	rs.noFolder[channelID] = true
}

// channelFolder returns the drive and folder files of a channel go into,
// creating Taut's subfolder on first use and remembering it.
func (d *Worker) channelFolder(ctx context.Context, rs *runState, lease *store.Lease, g Graph, channelID string) (driveID, folderID string, err error) {
	cm, err := d.cfg.Store.Channel(ctx, rs.run.ID, channelID)
	if err != nil {
		return "", "", err
	}
	if cm.FilesDriveID != "" && cm.FilesFolderID != "" {
		return cm.FilesDriveID, cm.FilesFolderID, nil
	}
	if cm.TeamsID == "" {
		return "", "", fmt.Errorf("channel %s has not been created yet", cm.SlackName)
	}
	teamID, err := d.teamID(ctx, rs, cm.TeamIndex)
	if err != nil {
		return "", "", err
	}
	// Under the legacy strategy the channel is in migration mode from its
	// creation, so its files are uploaded during migration mode, and
	// whether the folder can be used then is not documented. If it never
	// appeared, the channel's files cannot be moved; do not wait again for
	// each one.
	legacy := isLegacy(rs)
	if legacy && rs.folderIsUnavailable(channelID) {
		return "", "", errFolderUnavailable
	}
	// A new channel's folder is provisioned some time after the channel,
	// and how long that takes is not documented. Waiting here, under the
	// lease, keeps "not ready yet" from being counted as failed attempts.
	var root graph.DriveItem
	deadline := time.Now().Add(d.cfg.FolderWait)
	for {
		root, err = g.FilesFolder(ctx, teamID, cm.TeamsID)
		if err == nil || !graph.IsNotFound(err) || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(d.cfg.FolderPoll):
		}
	}
	if err != nil && legacy && graph.IsNotFound(err) {
		// Waiting longer would not help and failing the lane would block
		// the channel for good: a legacy channel cannot leave migration
		// mode and come back. Its files are settled as not moved instead,
		// and its messages say so.
		d.cfg.Log.Warn("the channel's files folder did not appear while the channel is in migration mode; its files will not be moved",
			"channel", cm.SlackName, "waited", d.cfg.FolderWait.String())
		rs.markFolderUnavailable(channelID)
		return "", "", errFolderUnavailable
	}
	if err != nil {
		return "", "", err
	}
	sub, err := g.EnsureFolder(ctx, root.DriveID, root.ID, filesFolderName)
	if err != nil {
		return "", "", err
	}
	err = d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error {
		return tx.SetChannelFolder(ctx, channelID, sub.DriveID, sub.ID)
	})
	if err != nil {
		return "", "", err
	}
	return sub.DriveID, sub.ID, nil
}

func (d *Worker) settleFile(ctx context.Context, lease *store.Lease, f store.FileMapping, state store.FileState, errCode, reason, op, source string) error {
	err := d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error {
		if err := tx.MarkFileSettled(ctx, f.ChannelID, f.SlackID, state, errCode, clip(reason)); err != nil {
			return err
		}
		e := d.entry(lease, op, source, "", f.SHA256)
		e.Outcome, e.Detail = "error", errCode
		return tx.Log(ctx, e)
	})
	if err == nil {
		event := EventFileFailed
		if state == store.FileSkipped {
			event = EventFileSkipped
		}
		d.cfg.Observer.Count(ctx, store.StageFiles, event, 1)
	}
	return err
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// --- Slack file access -----------------------------------------------------------

// HTTPSlackFiles fetches Slack files over HTTP.
type HTTPSlackFiles struct {
	// Client performs the request. When the worker runs behind the
	// egress proxy, this client routes through it.
	Client *http.Client
	// Rewrite, when set, maps the URL found in the export to the URL to
	// request and any headers to add (the egress proxy's routing headers, or a
	// test's fake host).
	Rewrite func(m *config.Config, rawURL string) (target string, header http.Header, err error)
}

// noRedirects makes an HTTP client hand back a redirect instead of
// following it, so the caller decides where a request may go.
func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// Open implements SlackFiles. Redirects are followed by hand, at most three
// times, and only to another URL that is itself a Slack file URL: a redirect
// must not be a way around ValidSlackFileURL, or around the egress proxy.
func (s HTTPSlackFiles) Open(ctx context.Context, m *config.Config, rawURL string) (io.ReadCloser, int64, string, error) {
	base := s.Client
	if base == nil {
		base = &http.Client{}
	}
	client := *base
	client.CheckRedirect = noRedirects

	for hop := 0; ; hop++ {
		target, header := rawURL, http.Header(nil)
		if s.Rewrite != nil {
			var err error
			if target, header, err = s.Rewrite(m, rawURL); err != nil {
				return nil, 0, "", err
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, 0, "", &fileGone{"bad_url", "the export's link for the file is not a valid URL"}
		}
		for k, vs := range header {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			// A url.Error includes the URL, and an export URL embeds a token.
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err
			}
			return nil, 0, "", fail.Wrap(err, "slack.files_unreachable", "Could not reach Slack's file host", "", "Check the worker's network path to files.slack.com.")
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			return resp.Body, resp.ContentLength, resp.Header.Get("Content-Type"), nil
		case resp.StatusCode >= 300 && resp.StatusCode < 400:
			loc := resp.Header.Get("Location")
			resp.Body.Close()
			next, err := url.Parse(loc)
			if err == nil {
				if prev, perr := url.Parse(rawURL); perr == nil {
					next = prev.ResolveReference(next)
				}
			}
			if err != nil || hop >= 3 || !ValidSlackFileURL(next.String()) {
				return nil, 0, "", &fileGone{"bad_redirect", "Slack redirected the download somewhere that is not its file host"}
			}
			rawURL = next.String()
			continue
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
			resp.Body.Close()
			return nil, 0, "", &fileGone{"not_found", "Slack no longer has the file"}
		case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
			resp.Body.Close()
			return nil, 0, "", &fileGone{"not_authorized", "Slack refused access to the file"}
		}
		resp.Body.Close()
		return nil, 0, "", fail.New("slack.files_error", "Slack's file host returned an error",
			fmt.Sprintf("HTTP %d.", resp.StatusCode), "It is usually temporary; the file will be retried.")
	}
}
