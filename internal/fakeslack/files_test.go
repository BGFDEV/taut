package fakeslack

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, url string, hdr map[string]string) (*http.Response, []byte, error) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp, body, err
}

func TestFileHost(t *testing.T) {
	const size = 3*blockSize + 12345
	h := NewFileHost(map[string]int64{"F0ONE": size, "F0EMPTY": 0})
	h.Bearer = "xoxb-FAKE-bearer"
	srv := httptest.NewServer(h)
	defer srv.Close()
	base := srv.URL + "/files-pri/T0FIXTURE-F0ONE/download/report.pdf"

	// Token in the URL, as exports carry it.
	resp, body, err := get(t, base+"?t=xoxe-FAKE-1", nil)
	if err != nil || resp.StatusCode != 200 || len(body) != size {
		t.Fatalf("status %v, %d bytes, err %v", resp.StatusCode, len(body), err)
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != ContentSHA256("F0ONE", size) {
		t.Fatal("served content does not match ContentSHA256")
	}
	// Bearer token works too, and content is identical.
	_, body2, _ := get(t, base, map[string]string{"Authorization": "Bearer xoxb-FAKE-bearer"})
	if string(body2) != string(body) {
		t.Fatal("content differs between requests")
	}
	// Different files, and different lengths of the same file, differ.
	if ContentSHA256("F0ONE", size) == ContentSHA256("F0TWO", size) || ContentSHA256("F0ONE", size) == ContentSHA256("F0ONE", size-1) {
		t.Fatal("content hash does not depend on file and length")
	}
	// Blocks are stamped: the second block is not a repeat of the first.
	if string(body[:blockSize]) == string(body[blockSize:2*blockSize]) {
		t.Fatal("blocks repeat; truncation or reordering would go unnoticed")
	}

	// No credentials: Slack answers 200 with an HTML sign-in page.
	resp, body, _ = get(t, base, nil)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") || !strings.Contains(string(body), "Sign in") {
		t.Fatalf("unauthenticated request: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	if resp, _, _ := get(t, srv.URL+"/files-pri/T0FIXTURE-F0NOPE/x?t=xoxe-1", nil); resp.StatusCode != 404 {
		t.Fatalf("unknown file: %d", resp.StatusCode)
	}
	if resp, body, _ := get(t, srv.URL+"/files-pri/T0FIXTURE-F0EMPTY/x?t=xoxe-1", nil); resp.StatusCode != 200 || len(body) != 0 {
		t.Fatalf("empty file: %d, %d bytes", resp.StatusCode, len(body))
	}
}

func TestFileHostFailures(t *testing.T) {
	h := NewFileHost(map[string]int64{"F0ONE": 2 * blockSize})
	srv := httptest.NewServer(h)
	defer srv.Close()
	url := srv.URL + "/files-pri/T0FIXTURE-F0ONE/x?t=xoxe-1"
	h.FailEvery(1)

	// First failure: a 503.
	if resp, _, _ := get(t, url, nil); resp.StatusCode != 503 {
		t.Fatalf("want 503, got %d", resp.StatusCode)
	}
	// Second failure: the body is cut off; the client must see an error,
	// not a short success.
	if _, body, err := get(t, url, nil); err == nil {
		t.Fatalf("truncated download returned %d bytes without an error", len(body))
	}
	h.FailEvery(0)
	if _, body, err := get(t, url, nil); err != nil || len(body) != 2*blockSize {
		t.Fatalf("after failures: %d bytes, err %v", len(body), err)
	}
	if h.Served() != 1 {
		t.Fatalf("served = %d, want 1 complete download", h.Served())
	}
}
