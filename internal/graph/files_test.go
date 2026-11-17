package graph_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/fakegraph"
	"github.com/bgfdev/taut/internal/graph"
)

const (
	kib320      = graph.UploadChunkMultiple
	fileCanary  = "FILE-CONTENT-CANARY-9042"
	uploadsPath = "/_upload/*"
)

// pattern is a deterministic stream of size bytes, cheap to generate: 64 KiB
// blocks that start with the file canary and their block number.
type pattern struct {
	size, off int64
	block     [64 << 10]byte
	onRead    func(off int64)
}

func newPattern(size int64) *pattern {
	p := &pattern{size: size}
	for i := range p.block {
		p.block[i] = byte(i*7 + i>>8)
	}
	copy(p.block[8:], fileCanary)
	return p
}

func (p *pattern) Read(b []byte) (int, error) {
	if p.onRead != nil {
		p.onRead(p.off)
	}
	if p.off >= p.size {
		return 0, io.EOF
	}
	n := 0
	for n < len(b) && p.off < p.size {
		in := p.off % int64(len(p.block))
		if in == 0 {
			binary.BigEndian.PutUint64(p.block[:8], uint64(p.off))
		}
		c := copy(b[n:], p.block[in:min(int64(len(p.block)), in+p.size-p.off)])
		n += c
		p.off += int64(c)
	}
	return n, nil
}

func patternSum(size int64) string {
	h := sha256.New()
	if _, err := io.Copy(h, newPattern(size)); err != nil {
		panic(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// slack returns an env with a "Slack" folder under Engineering/General.
func slack(t *testing.T, opts ...envOpt) (*env, graph.DriveItem) {
	t.Helper()
	e := newEnv(t, opts...)
	ctx := context.Background()
	root, err := e.client.FilesFolder(ctx, engTeam, engGen)
	if err != nil {
		t.Fatalf("FilesFolder: %v", err)
	}
	folder, err := e.client.EnsureFolder(ctx, root.DriveID, root.ID, "Slack")
	if err != nil {
		t.Fatalf("EnsureFolder: %v", err)
	}
	return e, folder
}

func spec(folder graph.DriveItem, name string, size int64) graph.UploadSpec {
	return graph.UploadSpec{DriveID: folder.DriveID, ParentID: folder.ID, Name: name, Size: size, Content: newPattern(size), ChunkSize: kib320}
}

func noFileContent(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		assertNoSecrets(t, err, fileCanary, "/_upload/", bodyCanary)
	}
}

func TestFilesFolder(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	root, err := e.client.FilesFolder(ctx, engTeam, engGen)
	if err != nil || root.ID == "" || !strings.HasPrefix(root.DriveID, "b!") || root.Name != "General" || root.WebURL == "" {
		t.Fatalf("FilesFolder = %+v, %v", root, err)
	}
	again, err := e.client.FilesFolder(ctx, engTeam, engGen)
	if err != nil || again.ID != root.ID {
		t.Errorf("FilesFolder is not stable: %+v, %v", again, err)
	}
	other, err := e.client.FilesFolder(ctx, engTeam, engRel)
	if err != nil || other.ID == root.ID || other.DriveID != root.DriveID {
		t.Errorf("another channel's folder = %+v, %v", other, err)
	}
	// By default the folder is served in migration mode too.
	if err := e.client.StartMigration(ctx, engTeam, engGen, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.client.FilesFolder(ctx, engTeam, engGen); err != nil {
		t.Errorf("FilesFolder in migration mode: %v", err)
	}
	_, err = e.client.FilesFolder(ctx, engTeam, "19:nope@thread.tacv2")
	wantFail(t, err, "graph.files_folder_not_ready")

	// A new channel's folder can lag.
	lag := newEnv(t, withServer(fakegraph.WithFilesFolderLag(2)))
	for i := range 2 {
		_, err := lag.client.FilesFolder(ctx, engTeam, engGen)
		wantFail(t, err, "graph.files_folder_not_ready")
		var api *graph.APIError
		if !graph.IsNotFound(err) || !errors.As(err, &api) || api.Status != 404 {
			t.Errorf("call %d: want a not-found error, got %v", i, err)
		}
	}
	if _, err := lag.client.FilesFolder(ctx, engTeam, engGen); err != nil {
		t.Errorf("after the lag: %v", err)
	}

	// The conservative reading: not available during migration mode.
	blocked := newEnv(t, withServer(fakegraph.WithFilesFolderBlockedInMigration()))
	if _, err := blocked.client.FilesFolder(ctx, engTeam, engGen); err != nil {
		t.Fatalf("before migration: %v", err)
	}
	if err := blocked.client.StartMigration(ctx, engTeam, engGen, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := blocked.client.FilesFolder(ctx, engTeam, engGen); !graph.IsNotFound(err) {
		t.Errorf("blocked in migration: %v", err)
	}
	if err := blocked.client.CompleteChannelMigration(ctx, engTeam, engGen); err != nil {
		t.Fatal(err)
	}
	if _, err := blocked.client.FilesFolder(ctx, engTeam, engGen); err != nil {
		t.Errorf("after completion: %v", err)
	}

	// Roles: reading needs a Files/Sites role, writing a ReadWrite one.
	_, err = newEnv(t, withRoles("Teamwork.Migrate.All")).client.FilesFolder(ctx, engTeam, engGen)
	if fe := wantFail(t, err, "graph.forbidden"); !strings.Contains(fe.Next, "Files.ReadWrite.All") {
		t.Errorf("next = %q", fe.Next)
	}
	reader := newEnv(t, withRoles("Files.Read.All"))
	root, err = reader.client.FilesFolder(ctx, engTeam, engGen)
	if err != nil {
		t.Fatalf("Files.Read.All: %v", err)
	}
	_, err = reader.client.EnsureFolder(ctx, root.DriveID, root.ID, "Slack")
	wantFail(t, err, "graph.forbidden")
	_, err = reader.client.Upload(ctx, spec(root, "a.txt", 10))
	wantFail(t, err, "graph.forbidden")
	_, err = reader.client.Upload(ctx, spec(root, "a.txt", 0))
	wantFail(t, err, "graph.forbidden")
}

func TestEnsureFolder(t *testing.T) {
	ctx := context.Background()
	e, folder := slack(t)
	root, _ := e.client.FilesFolder(ctx, engTeam, engGen)
	if folder.Name != "Slack" || folder.ID == "" || folder.DriveID != root.DriveID || !strings.HasSuffix(folder.WebDavURL, "/General/Slack") {
		t.Fatalf("folder = %+v", folder)
	}
	again, err := e.client.EnsureFolder(ctx, root.DriveID, root.ID, "slack") // SharePoint ignores case
	if err != nil || again.ID != folder.ID {
		t.Fatalf("second EnsureFolder = %+v, %v", again, err)
	}
	if got := e.fake.CountRequests("POST", "/v1.0/drives/*/items/*/children"); got != 1 {
		t.Errorf("folder creations = %d, want 1", got)
	}
	nested, err := e.client.EnsureFolder(ctx, root.DriveID, folder.ID, "2019 & more #1")
	if err != nil || !strings.HasSuffix(nested.WebDavURL, "/General/Slack/2019%20&%20more%20%231") {
		t.Fatalf("nested = %+v, %v", nested, err)
	}

	// Racing creators all get the same folder.
	var wg sync.WaitGroup
	ids := make([]string, 8)
	for i := range ids {
		wg.Go(func() {
			it, err := e.client.EnsureFolder(ctx, root.DriveID, folder.ID, "raced")
			if err != nil {
				t.Errorf("racer %d: %v", i, err)
			}
			ids[i] = it.ID
		})
	}
	wg.Wait()
	for _, id := range ids {
		if id == "" || id != ids[0] {
			t.Fatalf("racers disagree: %v", ids)
		}
	}
	// A creation whose response is lost, and one that is refused with 409.
	e.fake.Faults().MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/drives/*/items/*/children", Action: fakegraph.ActionFailAfterCommit, Count: 1})
	lost, err := e.client.EnsureFolder(ctx, root.DriveID, folder.ID, "lost")
	if err != nil || lost.ID == "" {
		t.Fatalf("EnsureFolder with a lost response = %+v, %v", lost, err)
	}

	_, err = e.client.EnsureFolder(ctx, root.DriveID, root.ID, "bad:name")
	wantFail(t, err, "graph.file_name_invalid")
	_, err = e.client.EnsureFolder(ctx, root.DriveID, "no-such-item", "x")
	wantFail(t, err, "graph.not_found")
	_, err = e.client.EnsureFolder(ctx, "b!other", root.ID, "x")
	wantFail(t, err, "graph.not_found")
}

func TestUploadSizes(t *testing.T) {
	ctx := context.Background()
	e, folder := slack(t)
	for _, tc := range []struct {
		name string
		size int64
		puts int
	}{
		{"empty.bin", 0, 0},
		{"one.bin", 1, 1},
		{"exact.bin", kib320, 1},
		{"plus one.bin", kib320 + 1, 2},
		{"several #1 (é).bin", 3*kib320 + 12345, 4},
	} {
		e.fake.ResetRequests()
		item, err := e.client.Upload(ctx, spec(folder, tc.name, tc.size))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if item.Name != tc.name || item.Size != tc.size || item.DriveID != folder.DriveID || item.ID == "" ||
			!strings.HasPrefix(item.WebDavURL, "https://") || len(item.ETagGUID()) != 36 {
			t.Errorf("%s: item = %+v", tc.name, item)
		}
		if got := e.fake.CountRequests("PUT", uploadsPath); got != tc.puts {
			t.Errorf("%s: %d chunk PUTs, want %d", tc.name, got, tc.puts)
		}
		var stored *fakegraph.FileView
		for _, f := range e.fake.Files(engTeam, engGen) {
			if f.Name == tc.name {
				stored = &f
			}
		}
		if stored == nil || stored.Size != tc.size || stored.SHA256 != patternSum(tc.size) || stored.Path != "Slack/"+tc.name ||
			stored.WebDavURL != item.WebDavURL || stored.ETag != item.ETag || stored.ItemID != item.ID || stored.UploadedBy != fakegraph.DefaultClientID {
			t.Errorf("%s: stored = %+v, item = %+v", tc.name, stored, item)
		}
		if !strings.EqualFold("{"+item.ETagGUID()+"}", strings.Split(strings.Trim(item.ETag, `"`), ",")[0]) {
			t.Errorf("%s: ETagGUID %q does not match eTag %q", tc.name, item.ETagGUID(), item.ETag)
		}
	}
	st := e.fake.Stats()
	if st.UploadsCompleted != 5 || st.UploadSessions != 4 || st.UploadBytes != 1+kib320+kib320+1+3*kib320+12345 {
		t.Errorf("stats = %+v", st)
	}
	// The default chunk size takes a small file in one request.
	s := spec(folder, "default chunk.bin", 1000)
	s.ChunkSize = 0
	if _, err := e.client.Upload(ctx, s); err != nil {
		t.Errorf("default chunk size: %v", err)
	}

	// Replacing keeps the item and its GUID and bumps the version.
	first, _ := e.client.Upload(ctx, spec(folder, "doc.bin", 100))
	second, err := e.client.Upload(ctx, spec(folder, "doc.bin", 200))
	if err != nil || second.ID != first.ID || second.ETagGUID() != first.ETagGUID() || second.ETag == first.ETag || second.Size != 200 {
		t.Errorf("replace: first %+v, second %+v, %v", first, second, err)
	}
	// "fail" refuses, for sessions and for empty files alike.
	for _, size := range []int64{50, 0} {
		s := spec(folder, "doc.bin", size)
		s.Conflict = "fail"
		_, err := e.client.Upload(ctx, s)
		wantFail(t, err, "graph.conflict")
		noFileContent(t, err)
	}
	if got := e.fake.Files(engTeam, engGen); len(got) != 7 {
		t.Errorf("files = %d, want 7", len(got))
	}

	// Invalid specs never reach Graph.
	e.fake.ResetRequests()
	for name, s := range map[string]graph.UploadSpec{
		"no name":         spec(folder, "", 1),
		"negative size":   spec(folder, "x", -1),
		"odd chunk":       {DriveID: folder.DriveID, ParentID: folder.ID, Name: "x", Size: 1, Content: newPattern(1), ChunkSize: 1000},
		"huge chunk":      {DriveID: folder.DriveID, ParentID: folder.ID, Name: "x", Size: 1, Content: newPattern(1), ChunkSize: 193 * kib320},
		"nil content":     {DriveID: folder.DriveID, ParentID: folder.ID, Name: "x", Size: 1},
		"rename conflict": {DriveID: folder.DriveID, ParentID: folder.ID, Name: "x", Size: 1, Content: newPattern(1), Conflict: "rename"},
	} {
		if _, err := e.client.Upload(ctx, s); err == nil || fail.From(err).Code != "graph.bad_request" {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := len(e.fake.Requests()); n != 0 {
		t.Errorf("invalid specs sent %d requests", n)
	}
	// A name SharePoint refuses, and a folder that does not exist.
	for _, size := range []int64{0, 10} {
		_, err := e.client.Upload(ctx, spec(folder, "a|b.txt", size))
		wantFail(t, err, "graph.file_name_invalid")
		_, err = e.client.Upload(ctx, graph.UploadSpec{DriveID: folder.DriveID, ParentID: "nope", Name: "a.txt", Size: size, Content: newPattern(size)})
		wantFail(t, err, "graph.not_found")
	}
}

func TestUploadLargeFileIsStreamed(t *testing.T) {
	if testing.Short() {
		t.Skip("256 MiB upload")
	}
	const size = 256 << 20
	e, folder := slack(t)
	want := patternSum(size)

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak uint64
	content := newPattern(size)
	content.onRead = func(int64) {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		peak = max(peak, m.HeapAlloc)
	}
	started := time.Now()
	item, err := e.client.Upload(context.Background(), graph.UploadSpec{
		DriveID: folder.DriveID, ParentID: folder.ID, Name: "big.bin", Size: size, Content: content,
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if f := e.fake.Files(engTeam, engGen); len(f) != 1 || f[0].Size != size || f[0].SHA256 != want || item.Size != size {
		t.Fatalf("stored = %+v", f)
	}
	if got := e.fake.CountRequests("PUT", uploadsPath); got != 26 {
		t.Errorf("chunk PUTs = %d, want 26 (10 MiB chunks)", got)
	}
	growth := int64(peak) - int64(base.HeapAlloc)
	t.Logf("256 MiB uploaded in %s; peak heap growth %d MiB", time.Since(started).Round(time.Millisecond), growth>>20)
	if growth > 64<<20 {
		t.Errorf("heap grew by %d MiB while uploading %d MiB; the upload must stream", growth>>20, size>>20)
	}
}

func TestUploadWrongSize(t *testing.T) {
	ctx := context.Background()
	e, folder := slack(t)
	for name, tc := range map[string]struct {
		announced, actual int64
	}{
		"short in the first chunk": {1000, 999},
		"short in a later chunk":   {3 * kib320, 2*kib320 + 5},
		"long by one":              {1000, 1001},
		"long, several chunks":     {2 * kib320, 5 * kib320},
		"empty but not":            {0, 1},
	} {
		s := spec(folder, "wrong.bin", tc.announced)
		s.Content = newPattern(tc.actual)
		_, err := e.client.Upload(ctx, s)
		if err == nil || fail.From(err).Code != "graph.upload_size_mismatch" {
			t.Errorf("%s: %v", name, err)
			continue
		}
		wantFail(t, err, "graph.upload_size_mismatch")
		noFileContent(t, err)
	}
	if f := e.fake.Files(engTeam, engGen); len(f) != 0 {
		t.Errorf("a wrong-sized upload was stored: %+v", f)
	}
	// Each session was cancelled, so none is left open.
	if got, want := e.fake.CountRequests("DELETE", uploadsPath), e.fake.Stats().UploadSessions; got != want || want != 4 {
		t.Errorf("cancelled %d of %d sessions", got, want)
	}

	// A reader that fails.
	s := spec(folder, "broken.bin", 1000)
	s.Content = io.MultiReader(newPattern(10), errReader{})
	_, err := e.client.Upload(ctx, s)
	wantFail(t, err, "graph.upload_read_failed")
	if !errors.Is(err, errBoom) {
		t.Errorf("the reader's error should be wrapped: %v", err)
	}
}

var errBoom = errors.New("staging read failed")

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errBoom }

func TestUploadResumesAfterLostResponses(t *testing.T) {
	ctx := context.Background()
	const size = 3*kib320 + 77
	want := patternSum(size)
	for name, tc := range map[string]struct {
		fault fakegraph.Fault
		size  int64
		puts  int // chunk PUTs in total
	}{
		// Bytes accepted, response lost: the client asks, then moves on.
		"intermediate chunk dropped after commit": {fakegraph.Fault{Method: "PUT", Path: uploadsPath, Action: fakegraph.ActionFailAfterCommit, Count: 1}, size, 4},
		"intermediate chunk 503 after commit":     {fakegraph.Fault{Method: "PUT", Path: uploadsPath, Action: fakegraph.ActionFailAfterCommit, Status: 503, Count: 1}, size, 4},
		// Bytes not accepted: the same chunk is sent again.
		"intermediate chunk dropped": {fakegraph.Fault{Method: "PUT", Path: uploadsPath, Action: fakegraph.ActionDrop, Count: 1}, size, 5},
		"intermediate chunk 500":     {fakegraph.Fault{Method: "PUT", Path: uploadsPath, Action: fakegraph.ActionStatus, Status: 500, Count: 1}, size, 5},
		// The final chunk: the item exists, the session is gone.
		"final chunk dropped after commit": {fakegraph.Fault{Method: "PUT", Path: uploadsPath, Action: fakegraph.ActionFailAfterCommit, Count: 1}, 1000, 1},
		"final chunk 503 after commit":     {fakegraph.Fault{Method: "PUT", Path: uploadsPath, Action: fakegraph.ActionFailAfterCommit, Status: 503, Count: 1}, 1000, 1},
		"final chunk dropped":              {fakegraph.Fault{Method: "PUT", Path: uploadsPath, Action: fakegraph.ActionDrop, Count: 1}, 1000, 2},
	} {
		t.Run(name, func(t *testing.T) {
			e, folder := slack(t)
			// An older file of the same size is in place, so "the file is
			// there" alone must not count as success.
			old := spec(folder, "report.bin", tc.size)
			old.Content = bytes.NewReader(make([]byte, tc.size))
			before, err := e.client.Upload(ctx, old)
			if err != nil {
				t.Fatal(err)
			}
			e.fake.ResetRequests()
			e.fake.Faults().MustAdd(tc.fault)
			item, err := e.client.Upload(ctx, spec(folder, "report.bin", tc.size))
			if err != nil {
				t.Fatalf("Upload: %v", err)
			}
			files := e.fake.Files(engTeam, engGen)
			if len(files) != 1 || files[0].Name != "report.bin" || files[0].Size != tc.size || files[0].SHA256 != patternSum(tc.size) {
				t.Fatalf("files = %+v (want one report.bin with hash %s)", files, want)
			}
			if item.ID != before.ID || item.ETag == before.ETag || item.ETag != files[0].ETag {
				t.Errorf("item = %+v, before = %+v", item, before)
			}
			if got := e.fake.CountRequests("PUT", uploadsPath); got != tc.puts {
				t.Errorf("chunk PUTs = %d, want %d", got, tc.puts)
			}
			if got := e.fake.CountRequests("GET", uploadsPath); got != 1 {
				t.Errorf("session status checks = %d, want 1", got)
			}
			if st := e.fake.Stats(); st.UploadsCompleted != 2 || st.UploadSessions != 2 {
				t.Errorf("stats = %+v", st)
			}
		})
	}

	// When the session is gone and the upload did not complete, the old
	// file must not be mistaken for the new one.
	e, folder := slack(t, withServer(fakegraph.WithUploadSessionLifetime(time.Hour)), withRetry(graph.RetryPolicy{BaseDelay: 2 * time.Hour, MaxDelay: 2 * time.Hour, MaxTotalWait: 24 * time.Hour}))
	old := spec(folder, "report.bin", 1000)
	old.Content = bytes.NewReader(make([]byte, 1000))
	if _, err := e.client.Upload(ctx, old); err != nil {
		t.Fatal(err)
	}
	e.fake.Faults().MustAdd(fakegraph.Fault{Method: "PUT", Path: uploadsPath, Action: fakegraph.ActionDrop, Count: 1})
	_, err := e.client.Upload(ctx, spec(folder, "report.bin", 1000)) // the backoff outlasts the session
	wantFail(t, err, "graph.upload_session_expired")
	if !graph.IsUploadSessionExpired(err) {
		t.Errorf("IsUploadSessionExpired false for %v", err)
	}
	noFileContent(t, err)
	if f := e.fake.Files(engTeam, engGen); len(f) != 1 || f[0].SHA256 == patternSum(1000) {
		t.Errorf("files = %+v", f)
	}
}

func TestUploadThrottlingAndExpiry(t *testing.T) {
	ctx := context.Background()
	e, folder := slack(t, withServer(fakegraph.WithUploadSessionLifetime(time.Hour)))
	e.fake.Faults().MustAdd(fakegraph.Fault{Method: "PUT", Path: uploadsPath, Action: fakegraph.ActionThrottle, RetryAfterSeconds: 7, Count: 3})
	const size = 2*kib320 + 1
	if _, err := e.client.Upload(ctx, spec(folder, "throttled.bin", size)); err != nil {
		t.Fatalf("Upload under throttling: %v", err)
	}
	waits := e.sleep.Waits()
	if len(waits) != 3 {
		t.Fatalf("waits = %v", waits)
	}
	for _, w := range waits {
		if w < 7*time.Second || w > 8*time.Second {
			t.Errorf("wait %v does not honor Retry-After: 7", w)
		}
	}
	if f := e.fake.Files(engTeam, engGen); len(f) != 1 || f[0].SHA256 != patternSum(size) {
		t.Errorf("files = %+v", f)
	}
	if got := e.fake.CountRequests("GET", uploadsPath); got != 0 {
		t.Errorf("a throttled chunk is not ambiguous, yet the session was queried %d times", got)
	}

	// The session expires between two chunks.
	s := spec(folder, "expired.bin", size)
	content := newPattern(size)
	content.onRead = func(off int64) {
		if off == kib320 {
			e.clock.Advance(2 * time.Hour)
		}
	}
	s.Content = content
	_, err := e.client.Upload(ctx, s)
	fe := wantFail(t, err, "graph.upload_session_expired")
	var api *graph.APIError
	if !graph.IsUploadSessionExpired(err) || !errors.As(err, &api) || api.Status != 404 || strings.Contains(fe.Error(), "_upload") {
		t.Errorf("expiry error = %v", err)
	}
	noFileContent(t, err)
	if f := e.fake.Files(engTeam, engGen); len(f) != 1 {
		t.Errorf("an expired upload was stored: %+v", f)
	}
	if graph.IsUploadSessionExpired(nil) || graph.IsUploadSessionExpired(errBoom) {
		t.Error("IsUploadSessionExpired false positives")
	}

	// A persistent failure gives up with a retryable error and no file.
	e.fake.Faults().MustAdd(fakegraph.Fault{Method: "PUT", Path: uploadsPath, Action: fakegraph.ActionStatus, Status: 503})
	_, err = e.client.Upload(ctx, spec(folder, "down.bin", size))
	wantFail(t, err, "graph.unavailable")
	noFileContent(t, err)
	if f := e.fake.Files(engTeam, engGen); len(f) != 1 {
		t.Errorf("files = %+v", f)
	}
}

func TestSanitizeFileName(t *testing.T) {
	for in, want := range map[string]string{
		"report.docx":               "report.docx",
		"":                          "file",
		"   ":                       "file",
		"...":                       "file",
		` a"b*c:d<e>f?g/h\i|j .`:    "a_b_c_d_e_f_g_h_i_j",
		"tab\there\nnew\x00nul\x7f": "tab_here_new_nul_",
		"trailing dots...":          "trailing dots",
		"  spaced  .txt":            "spaced  .txt",
		"~$lockfile.docx":           "_$lockfile.docx",
		"CON":                       "_CON",
		"con":                       "_con",
		"Com0":                      "_Com0",
		"LPT9":                      "_LPT9",
		"nul":                       "_nul",
		".lock":                     "_.lock",
		"Desktop.ini":               "_Desktop.ini",
		"CON.txt":                   "CON.txt",
		"my_vti_file":               "my-vti-file",
		"_VTI_":                     "-vti-",
		"_vti_vti_":                 "-vti-vti_",
		"über größe ☃.png":          "über größe ☃.png",
		"bad\xffutf8.txt":           "bad_utf8.txt",
		"../../etc/passwd":          ".._.._etc_passwd",
		"a%20b#c&d.txt":             "a%20b#c&d.txt",
		"CON ":                      "_CON",
		"con.":                      "_con",
	} {
		if got := graph.SanitizeFileName(in); got != want {
			t.Errorf("SanitizeFileName(%q) = %q, want %q", in, got, want)
		}
	}
	long := graph.SanitizeFileName(strings.Repeat("é", 300) + ".tar.gz")
	if len(long) > 200 || !utf8.ValidString(long) || !strings.HasSuffix(long, ".gz") || !strings.HasPrefix(long, "ééé") {
		t.Errorf("long name = %d bytes, %q", len(long), long)
	}
	if got := graph.SanitizeFileName(strings.Repeat("a", 199) + " " + strings.Repeat("b", 50)); len(got) != 199 {
		t.Errorf("cut at a space left %d bytes: %q", len(got), got)
	}
	if got := graph.SanitizeFileName(strings.Repeat("x", 250) + "." + strings.Repeat("e", 40)); len(got) != 200 {
		t.Errorf("an over-long extension is not preserved: %d bytes", len(got))
	}

	// Whatever goes in, the fake's SharePoint accepts what comes out, and
	// sanitizing is idempotent.
	e, folder := slack(t)
	hostile := []string{
		"", " ", ".", "..", "...", "~$", "~$~$x", "CON", "con.", "PRN .", "aux", "NUL", "COM0", "com9", "lpt0", "LPT5.", ".lock", ".LOCK ",
		"desktop.ini", "_vti_", "__vti__", "_vti_vti_", "x_VTI_y", `"*:<>?/\|`, "a/b/c", `c:\windows\system32`, " lead", "trail ", "trail.",
		"\x00\x01\x1f", "\xff\xfe", "名前.txt", "😀.png", strings.Repeat("a", 1000), strings.Repeat("😀", 100) + ".jpeg",
		strings.Repeat(".", 300), strings.Repeat(" ", 300) + "x", strings.Repeat("ab ", 100) + ".", "a.b.c.d", "CON.", " .lock ", "~$ ", "_ vti _",
		"nul.", "con ..  ", strings.Repeat("x", 195) + "_vti_", strings.Repeat("x", 199) + ". .txt",
	}
	for i, in := range hostile {
		name := graph.SanitizeFileName(in)
		if name == "" || len(name) > 200 || graph.SanitizeFileName(name) != name {
			t.Errorf("SanitizeFileName(%q) = %q: empty, too long or not idempotent", in, name)
			continue
		}
		s := spec(folder, name, int64(i%2)) // both the session and the simple PUT path
		s.Conflict = "replace"
		if _, err := e.client.Upload(context.Background(), s); err != nil {
			t.Errorf("the fake rejects SanitizeFileName(%q) = %q: %v", in, name, err)
		}
	}
}

func TestImportWithAttachments(t *testing.T) {
	ctx := context.Background()
	e, target := migrating(t)
	root, err := e.client.FilesFolder(ctx, engTeam, engGen)
	if err != nil {
		t.Fatal(err)
	}
	folder, err := e.client.EnsureFolder(ctx, root.DriveID, root.ID, "Slack")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := e.client.Upload(ctx, spec(folder, "plan q3.docx", 5000))
	if err != nil {
		t.Fatal(err)
	}
	pic, err := e.client.Upload(ctx, spec(folder, "chart.png", 0))
	if err != nil {
		t.Fatal(err)
	}
	marker := func(id string) string { return `<attachment id="` + id + `"></attachment>` }
	att := func(it graph.DriveItem) graph.Attachment {
		return graph.Attachment{ID: it.ETagGUID(), Name: it.Name, ContentURL: it.WebDavURL}
	}
	build := func(when string, html string, atts ...graph.Attachment) graph.ImportMessage {
		m := msgAt(when)
		m.HTML = "<p>" + bodyCanary + "</p>" + html
		m.Attachments = atts
		return m
	}

	// Two files, one with its GUID in upper case.
	upper := att(pic)
	upper.ID = strings.ToUpper(upper.ID)
	got, err := e.client.ImportMessage(ctx, target, build("2019-01-01T00:00:00Z", marker(doc.ETagGUID())+marker(upper.ID), att(doc), upper))
	if err != nil {
		t.Fatalf("import with attachments: %v", err)
	}
	stored := e.fake.Messages(engTeam, engGen)
	if len(stored) != 1 || len(stored[0].Attachments) != 2 ||
		stored[0].Attachments[0] != (fakegraph.AttachmentView{ID: doc.ETagGUID(), Name: "plan q3.docx", ContentURL: doc.WebDavURL}) ||
		stored[0].Attachments[1].ID != upper.ID || stored[0].ID != got.ID {
		t.Fatalf("stored = %+v", stored)
	}

	other := "0f8fad5b-d9cb-469f-a165-70867728950e"
	for name, m := range map[string]graph.ImportMessage{
		"no marker":                 build("2019-02-01T00:00:00Z", "", att(doc)),
		"marker without attachment": build("2019-02-01T00:00:00Z", marker(doc.ETagGUID())),
		"marker for another id":     build("2019-02-01T00:00:00Z", marker(other), att(doc)),
		"same file twice":           build("2019-02-01T00:00:00Z", marker(doc.ETagGUID()), att(doc), att(doc)),
		"unknown guid":              build("2019-02-01T00:00:00Z", marker(other), graph.Attachment{ID: other, Name: "x", ContentURL: doc.WebDavURL}),
		"guid in braces":            build("2019-02-01T00:00:00Z", marker("{"+doc.ETagGUID()+"}"), graph.Attachment{ID: "{" + doc.ETagGUID() + "}", Name: "x", ContentURL: doc.WebDavURL}),
		"item id instead of guid":   build("2019-02-01T00:00:00Z", marker(doc.ID), graph.Attachment{ID: doc.ID, Name: "x", ContentURL: doc.WebDavURL}),
		"a folder":                  build("2019-02-01T00:00:00Z", marker(folder.ETagGUID()), att(folder)),
		"webUrl, not webDavUrl":     build("2019-02-01T00:00:00Z", marker(doc.ETagGUID()), graph.Attachment{ID: doc.ETagGUID(), Name: "x", ContentURL: doc.WebURL}),
		"sharing link":              build("2019-02-01T00:00:00Z", marker(doc.ETagGUID()), graph.Attachment{ID: doc.ETagGUID(), Name: "x", ContentURL: "https://contoso.sharepoint.com/:w:/s/eng/EZx1"}),
		"another file's url":        build("2019-02-01T00:00:00Z", marker(doc.ETagGUID()), graph.Attachment{ID: doc.ETagGUID(), Name: "x", ContentURL: pic.WebDavURL}),
		"no url":                    build("2019-02-01T00:00:00Z", marker(doc.ETagGUID()), graph.Attachment{ID: doc.ETagGUID(), Name: "x"}),
		"no id":                     build("2019-02-01T00:00:00Z", marker(""), graph.Attachment{Name: "x", ContentURL: doc.WebDavURL}),
	} {
		_, err := e.client.ImportMessage(ctx, target, m)
		if err == nil || fail.From(err).Code != "graph.attachment_invalid" {
			t.Errorf("%s: got %v, want graph.attachment_invalid", name, err)
			continue
		}
		wantFail(t, err, "graph.attachment_invalid")
		assertNoSecrets(t, err, bodyCanary, "plan q3", doc.WebDavURL)
	}
	if got := len(e.fake.Messages(engTeam, engGen)); got != 1 {
		t.Errorf("rejected attachments were stored: %d messages", got)
	}

	// The lenient fake accepts references it cannot resolve, but still
	// pairs markers with attachments.
	lenient, target := migrating(t, withServer(fakegraph.WithLenientAttachments()))
	link := graph.Attachment{ID: "anything", Name: "x", ContentURL: "https://contoso.sharepoint.com/:w:/s/eng/EZx1"}
	if _, err := lenient.client.ImportMessage(ctx, target, build("2019-02-01T00:00:00Z", marker("anything"), link)); err != nil {
		t.Errorf("lenient attachments: %v", err)
	}
	_, err = lenient.client.ImportMessage(ctx, target, build("2019-02-02T00:00:00Z", "", link))
	wantFail(t, err, "graph.attachment_invalid")
}

func TestImportWithHostedContents(t *testing.T) {
	ctx := context.Background()
	e, target := migrating(t, withServer(fakegraph.WithMaxHostedContentBytes(300<<10)))
	img := func(n int) string { return fmt.Sprintf(`<img src="../hostedContents/%d/$value" alt="x">`, n) }
	png := bytes.Repeat([]byte(fileCanary), 200<<10/len(fileCanary)) // ~200 KB: over the body limit, under the image limit
	small := []byte("\x89PNG tiny")
	build := func(when, html string, hosted ...graph.HostedContent) graph.ImportMessage {
		m := msgAt(when)
		m.HTML = "<p>" + bodyCanary + "</p>" + html
		m.HostedContents = hosted
		return m
	}

	if _, err := e.client.ImportMessage(ctx, target, build("2019-01-01T00:00:00Z", img(1)+img(2),
		graph.HostedContent{ContentType: "image/png", Data: png}, graph.HostedContent{ContentType: "image/jpeg", Data: small})); err != nil {
		t.Fatalf("import with hosted contents: %v", err)
	}
	sum := sha256.Sum256(append(append([]byte{}, png...), small...))
	stored := e.fake.Messages(engTeam, engGen)
	if len(stored) != 1 || stored[0].HostedContents != 2 || stored[0].HostedBytes != int64(len(png)+len(small)) ||
		stored[0].HostedSHA256 != hex.EncodeToString(sum[:]) || strings.Contains(stored[0].BodyContent, fileCanary) {
		t.Fatalf("stored = %+v", stored[0])
	}

	for name, tc := range map[string]struct {
		msg  graph.ImportMessage
		code string
	}{
		"not referenced":            {build("2019-02-01T00:00:00Z", "", graph.HostedContent{ContentType: "image/png", Data: small}), "graph.hosted_content_invalid"},
		"reference missing":         {build("2019-02-01T00:00:00Z", img(1)+img(2), graph.HostedContent{ContentType: "image/png", Data: small}), "graph.hosted_content_invalid"},
		"reference without content": {build("2019-02-01T00:00:00Z", img(1)), "graph.hosted_content_invalid"},
		"not an image":              {build("2019-02-01T00:00:00Z", img(1), graph.HostedContent{ContentType: "application/pdf", Data: small}), "graph.hosted_content_invalid"},
		"no content type":           {build("2019-02-01T00:00:00Z", img(1), graph.HostedContent{Data: small}), "graph.hosted_content_invalid"},
		"too large":                 {build("2019-02-01T00:00:00Z", img(1), graph.HostedContent{ContentType: "image/png", Data: bytes.Repeat([]byte(fileCanary), 301<<10/len(fileCanary)+1)}), "graph.hosted_content_too_large"},
	} {
		_, err := e.client.ImportMessage(ctx, target, tc.msg)
		if err == nil || fail.From(err).Code != tc.code {
			t.Errorf("%s: got %v, want %s", name, err, tc.code)
			continue
		}
		wantFail(t, err, tc.code)
		assertNoSecrets(t, err, bodyCanary, fileCanary, "RklMRS1DT05URU5U") // the canary, raw and as base64
	}
	if got := len(e.fake.Messages(engTeam, engGen)); got != 1 {
		t.Errorf("rejected hosted contents were stored: %d messages", got)
	}
	// The body limit still applies to the body alone.
	big := build("2019-03-01T00:00:00Z", img(1)+strings.Repeat("x", fakegraph.DefaultMaxMessageBytes), graph.HostedContent{ContentType: "image/png", Data: small})
	_, err := e.client.ImportMessage(ctx, target, big)
	wantFail(t, err, "graph.payload_too_large")
}

// proxy is a tiny reverse proxy in front of the fake. It demands its own
// key, refuses client-supplied Authorization, and adds the bearer token
// itself, the way an egress proxy that holds the credentials would.
type proxy struct {
	*httptest.Server
	relayed, direct, withAuth, withoutKey atomic.Int64
}

func newProxy(t *testing.T, e *env) *proxy {
	p := &proxy{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			p.withAuth.Add(1)
		}
		if r.Header.Get("X-Proxy-Key") != "k1" {
			p.withoutKey.Add(1)
			http.Error(w, "proxy key required", http.StatusProxyAuthRequired)
			return
		}
		target := e.http.URL + r.URL.RequestURI()
		if r.URL.Path == "/relay" {
			if r.Header.Get("X-Relay") != "yes" {
				http.Error(w, "relay header required", http.StatusBadRequest)
				return
			}
			p.relayed.Add(1)
			target = r.URL.Query().Get("u")
		} else {
			p.direct.Add(1)
		}
		out, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		out.ContentLength = r.ContentLength
		for _, h := range []string{"Content-Type", "Content-Range", "Accept"} {
			if v := r.Header.Get(h); v != "" {
				out.Header.Set(h, v)
			}
		}
		if !strings.Contains(target, "/_upload/") {
			token, err := e.tokens.Token(r.Context())
			if err != nil {
				http.Error(w, "no token", http.StatusBadGateway)
				return
			}
			out.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := e.http.Client().Do(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(p.Close)
	return p
}

func TestProxyOptions(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, withSeed(manyUsers(25)), withServer(fakegraph.WithPageSize(10)))
	p := newProxy(t, e)
	rewrite := func(abs string) (string, http.Header) {
		return p.URL + "/relay?u=" + url.QueryEscape(abs), http.Header{"X-Relay": {"yes"}}
	}
	newClient := func(mod func(*graph.Options)) *graph.Client {
		opts := graph.Options{
			BaseURL: p.URL, HTTPClient: p.Client(), TokenSource: staticToken(""),
			Header: http.Header{"X-Proxy-Key": {"k1"}}, RewriteURL: rewrite,
			Sleep: e.sleep.Sleep, Now: e.clock.Now, Jitter: func() float64 { return 0.5 },
		}
		if mod != nil {
			mod(&opts)
		}
		c, err := graph.New(opts)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := newClient(nil)

	// Paging: the fake's next links point at the fake, not the proxy, so
	// they are rewritten through the relay.
	users, err := collect(c.Users(ctx, graph.UserQuery{}))
	if err != nil || len(users) != 25 {
		t.Fatalf("Users through the proxy = %d, %v", len(users), err)
	}
	if p.relayed.Load() != 2 || p.direct.Load() != 1 {
		t.Errorf("paging: %d relayed, %d direct; want 2 and 1", p.relayed.Load(), p.direct.Load())
	}

	// Upload: session URLs are absolute and go through the relay as well.
	root, err := c.FilesFolder(ctx, engTeamOf(e), engGenOf(e))
	if err != nil {
		t.Fatalf("FilesFolder through the proxy: %v", err)
	}
	const size = 2*kib320 + 9
	relayedBefore := p.relayed.Load()
	item, err := c.Upload(ctx, spec(root, "via proxy.bin", size))
	if err != nil {
		t.Fatalf("Upload through the proxy: %v", err)
	}
	if f := e.fake.Files(engTeamOf(e), engGenOf(e)); len(f) != 1 || f[0].SHA256 != patternSum(size) || f[0].ItemID != item.ID {
		t.Errorf("files = %+v", f)
	}
	if got := p.relayed.Load() - relayedBefore; got != 3 {
		t.Errorf("relayed chunk PUTs = %d, want 3", got)
	}
	if p.withAuth.Load() != 0 || p.withoutKey.Load() != 0 {
		t.Errorf("the client sent Authorization %d times and omitted the static header %d times", p.withAuth.Load(), p.withoutKey.Load())
	}

	// Without the static header the proxy refuses.
	_, err = newClient(func(o *graph.Options) { o.Header = nil }).Organization(ctx)
	wantFail(t, err, "graph.request_failed")
	// Without RewriteURL: upload URLs are called directly (they are
	// pre-authorized), and a paging link to another host is refused.
	plain := newClient(func(o *graph.Options) { o.RewriteURL = nil })
	relayedBefore = p.relayed.Load()
	if _, err := plain.Upload(ctx, spec(root, "direct.bin", size)); err != nil {
		t.Fatalf("Upload without RewriteURL: %v", err)
	}
	if p.relayed.Load() != relayedBefore {
		t.Error("without RewriteURL nothing should be relayed")
	}
	_, err = collect(plain.Users(ctx, graph.UserQuery{}))
	wantFail(t, err, "graph.bad_response")
	// A rewrite that returns nonsense is reported, not followed.
	_, err = newClient(func(o *graph.Options) {
		o.RewriteURL = func(string) (string, http.Header) { return "not a url", nil }
	}).Upload(ctx, spec(root, "x.bin", 10))
	wantFail(t, err, "graph.config_invalid")

	// A non-empty token is still sent as a bearer token, and never to the
	// upload URL (the fake answers 401 if it is).
	if _, err := e.client.Upload(ctx, spec(root, "bearer.bin", size)); err != nil {
		t.Fatalf("Upload with a real token: %v", err)
	}
}

// The proxy test uses a seed without the default teams' users, but the
// teams themselves are the default ones.
func engTeamOf(*env) string { return engTeam }
func engGenOf(*env) string  { return engGen }

// Case folding changes the byte length of some characters; the sanitizer
// must not slice by an index taken from a lower-cased copy.
func TestSanitizeFileNameCaseFolding(t *testing.T) {
	for _, in := range []string{"F1-Ⱥ_vti_", "F1-İ_vti_.txt", "İİİ_VTI_x_vTi_.pdf", "ȺȺ_vti_ȺȺ"} {
		got := graph.SanitizeFileName(in) // must not panic
		if !utf8.ValidString(got) {
			t.Errorf("SanitizeFileName(%q) = %q, which is not valid UTF-8", in, got)
		}
		if strings.Contains(strings.ToLower(got), "_vti_") {
			t.Errorf("SanitizeFileName(%q) = %q still contains _vti_", in, got)
		}
	}
}
