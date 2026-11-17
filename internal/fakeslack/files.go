package fakeslack

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/bgfdev/taut/internal/slack"
	"github.com/bgfdev/taut/internal/slack/export"
)

// FileHost is a fake of files.slack.com: it serves the content of the files
// an export refers to. Content is generated, not stored, so a "1 GB file"
// costs no memory or disk: every byte is a pure function of the file ID and
// its offset, which also lets a test verify a transfer by hash without
// keeping the bytes.
//
// It reproduces the two things about Slack's file host that trip up real
// tools: a request without valid credentials is answered with 200 and an
// HTML sign-in page rather than an error status, and the token may arrive
// either in the URL (?t=…, as in exports) or as a bearer token.
type FileHost struct {
	mu    sync.Mutex
	files map[string]int64 // file ID → size served
	// sizeOf, when set, answers for an ID that files does not list.
	sizeOf func(id string) (int64, bool)

	// Bearer, when non-empty, is a bearer token that is also accepted.
	Bearer string

	// failEvery, when positive, makes every Nth download fail: odd
	// failures with a 503, even ones by cutting the body off part-way.
	failEvery atomic.Int64
	requests  atomic.Int64
	served    atomic.Int64 // completed downloads
}

// NewFileHost returns a host serving the given files (ID → size in bytes).
func NewFileHost(files map[string]int64) *FileHost {
	return &FileHost{files: files}
}

// FilesFromExport lists the Slack-hosted, downloadable files an export
// refers to. Sizes above maxBytes are served as maxBytes, so tests can run
// against fixtures that describe multi-gigabyte files; pass 0 for no cap.
func FilesFromExport(path string, maxBytes int64) (map[string]int64, error) {
	ex, err := export.Open(path)
	if err != nil {
		return nil, err
	}
	defer ex.Close()
	chans, err := ex.Channels()
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, ch := range chans {
		for msg, err := range ex.Messages(ch) {
			if err != nil {
				continue
			}
			for _, f := range msg.Files {
				if f.ID == "" || !f.Downloadable() {
					continue
				}
				size := f.Size
				if maxBytes > 0 && size > maxBytes {
					size = maxBytes
				}
				out[f.ID] = size
			}
		}
	}
	return out, nil
}

// NewComputedFileHost returns a host whose files are not listed but
// computed: sizeOf says whether an ID is a file and how large. It is for
// exports too large to read through just to learn their files.
func NewComputedFileHost(sizeOf func(id string) (int64, bool)) *FileHost {
	return &FileHost{files: map[string]int64{}, sizeOf: sizeOf}
}

// SetFiles replaces the set of files served.
func (h *FileHost) SetFiles(files map[string]int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.files = files
}

// FailEvery makes every nth download fail (0 turns failures off).
func (h *FileHost) FailEvery(n int) { h.failEvery.Store(int64(n)) }

// Served returns how many downloads completed.
func (h *FileHost) Served() int { return int(h.served.Load()) }

// Size returns the size the host serves for a file.
func (h *FileHost) Size(id string) (int64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n, ok := h.files[id]
	if !ok && h.sizeOf != nil {
		return h.sizeOf(id)
	}
	return n, ok
}

const blockSize = 1 << 20

// block returns the 1 MiB block that file content cycles through. The first
// eight bytes of each block as served are overwritten with the block's
// index, so content that is truncated, repeated or reordered hashes
// differently.
func block(id string) []byte {
	seed := sha256.Sum256([]byte(id))
	r := rand.NewChaCha8(seed)
	b := make([]byte, blockSize)
	_, _ = r.Read(b)
	return b
}

// content streams exactly size bytes of a file's content.
type content struct {
	base   []byte
	size   int64
	offset int64
	buf    []byte
	cur    int64 // index of the block held in buf; -1 before the first read
}

func newContent(id string, size int64) *content {
	return &content{base: block(id), size: size, buf: make([]byte, blockSize), cur: -1}
}

func (c *content) Read(p []byte) (int, error) {
	if c.offset >= c.size {
		return 0, io.EOF
	}
	idx, within := c.offset/blockSize, c.offset%blockSize
	if idx != c.cur {
		copy(c.buf, c.base)
		binary.BigEndian.PutUint64(c.buf[:8], uint64(idx))
		c.cur = idx
	}
	n := copy(p, c.buf[within:])
	if rest := c.size - c.offset; int64(n) > rest {
		n = int(rest)
	}
	c.offset += int64(n)
	return n, nil
}

// ContentSHA256 returns the hex SHA-256 of the content the host serves for
// a file of the given size, so a test can check what arrived elsewhere.
func ContentSHA256(id string, size int64) string {
	h := sha256.New()
	_, _ = io.Copy(h, newContent(id, size))
	return hex.EncodeToString(h.Sum(nil))
}

// fileID extracts the file ID from a files-pri path:
// /files-pri/T024BE7LD-F024BERPE/download/name or /files-pri/T…-F…/name.
func fileID(path string) string {
	rest, ok := strings.CutPrefix(path, "/files-pri/")
	if !ok {
		return ""
	}
	seg, _, _ := strings.Cut(rest, "/")
	_, id, ok := strings.Cut(seg, "-")
	if !ok {
		return ""
	}
	return id
}

// ServeHTTP implements http.Handler.
func (h *FileHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := h.requests.Add(1)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := fileID(r.URL.Path)
	size, ok := h.Size(id)
	if id == "" || !ok {
		http.NotFound(w, r)
		return
	}

	// Slack does not refuse an unauthenticated file request; it shows a
	// sign-in page with status 200.
	token := r.URL.Query().Get("t")
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	authed := strings.HasPrefix(token, "xoxe-") || (h.Bearer != "" && bearer == h.Bearer)
	if !authed {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<!doctype html><title>Slack</title><p>Sign in to Slack to view this file.</p>")
		return
	}

	every := h.failEvery.Load()
	failing := every > 0 && n%every == 0
	if failing && (n/every)%2 == 1 {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(size))
	if r.Method == http.MethodHead {
		return
	}
	src := io.Reader(newContent(id, size))
	if failing {
		// Cut the body off part-way and drop the connection, as a flaky
		// network would. The declared length makes the client notice.
		src = io.LimitReader(src, size/2)
		_, _ = io.Copy(w, src)
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
		return
	}
	if _, err := io.Copy(w, src); err == nil {
		h.served.Add(1)
	}
}

// Downloadable reports whether the fake would serve this file, mirroring
// what the export says about it.
func Downloadable(f slack.File) bool { return f.ID != "" && f.Downloadable() }
