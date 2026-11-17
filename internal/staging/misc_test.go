package staging

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/bgfdev/taut/internal/secret"
)

func TestValidateKey(t *testing.T) {
	for _, k := range hostileKeys {
		wantCode(t, ValidateKey(k), "staging.invalid_key")
	}
	for _, k := range []string{"a", "a/b", "runs/01HZX/extract/C012AB/000042.ndjson.gz", "a.b/c-d_e", ".hidden", "a/...", "ü/日本",
		strings.Repeat("k", maxSegmentLength), "with space/and:colon"} {
		if err := ValidateKey(k); err != nil {
			t.Errorf("ValidateKey(%q) = %v", k, err)
		}
	}
}

// The filesystem backend cannot be made to touch anything outside its root,
// even by a symbolic link planted inside it.
func TestFSCannotEscape(t *testing.T) {
	outside := t.TempDir()
	root := filepath.Join(t.TempDir(), "staging")
	b, err := NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	canary := filepath.Join(outside, "canary")
	if err := os.WriteFile(canary, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	if err := os.Symlink(canary, filepath.Join(root, "filelink")); err != nil {
		t.Fatal(err)
	}

	if _, err := b.Put(ctx, "link/planted", strings.NewReader("x")); err == nil {
		t.Error("Put followed a symlink out of the root")
	}
	if _, err := b.Put(ctx, "link/canary", strings.NewReader("overwritten")); err == nil {
		t.Error("Put overwrote a file outside the root")
	}
	if rc, err := b.Get(ctx, "link/canary"); err == nil {
		rc.Close()
		t.Error("Get read a file outside the root")
	}
	if rc, err := b.Get(ctx, "filelink"); err == nil {
		rc.Close()
		t.Error("Get read through a symlink to a file outside the root")
	}
	if _, err := b.Stat(ctx, "link/canary"); err == nil {
		t.Error("Stat reached a file outside the root")
	}
	_ = b.Delete(ctx, "link/canary")
	_, _ = b.DeletePrefix(ctx, "link/")
	_, _ = b.DeletePrefix(ctx, "")
	for info, err := range b.List(ctx, "") {
		if err == nil {
			t.Errorf("List exposed %q", info.Key)
		}
	}

	got, err := os.ReadFile(canary)
	if err != nil || string(got) != "untouched" {
		t.Fatalf("the file outside the root was changed: %q, %v", got, err)
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 1 {
		t.Fatalf("files appeared outside the root: %v", entries)
	}
}

func TestFSPermissionsAndLayout(t *testing.T) {
	root := filepath.Join(t.TempDir(), "new", "staging")
	b, err := NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	mustPut(t, b, "runs/r/export/file", []byte("data"))
	for path, want := range map[string]os.FileMode{
		root:                             0o700,
		filepath.Join(root, "runs", "r"): 0o700,
		filepath.Join(root, "runs/r/export/file"): 0o600,
	} {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := st.Mode().Perm(); got != want {
			t.Errorf("%s has mode %o, want %o", path, got, want)
		}
	}

	// An object and a "folder" cannot share a name on a filesystem.
	ctx := context.Background()
	_, err = b.Put(ctx, "runs/r/export/file/child", strings.NewReader("x"))
	wantCode(t, err, "staging.key_conflict")
	_, err = b.Put(ctx, "runs/r", strings.NewReader("x"))
	wantCode(t, err, "staging.key_conflict")
	if got := listKeys(t, b, ""); len(got) != 1 {
		t.Fatalf("conflicting Puts changed the store: %v", got)
	}

	// A purge removes stale temporary files and empty directories.
	stale := filepath.Join(root, "runs", "r", "export", tmpPrefix+"crashed")
	if err := os.WriteFile(stale, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := listKeys(t, b, ""); len(got) != 1 {
		t.Fatalf("a temporary file is listed: %v", got)
	}
	if n, err := b.DeletePrefix(ctx, "runs/r/"); err != nil || n != 1 {
		t.Fatalf("DeletePrefix = %d, %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(root, "runs", "r")); !os.IsNotExist(err) {
		t.Fatalf("purge left the run directory behind (%v)", err)
	}

	_, err = NewFS("")
	wantCode(t, err, "staging.config_invalid")
}

func TestOpen(t *testing.T) {
	b, err := Open(Config{Backend: "fs", FSRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.(*fsBlobs); !ok {
		t.Fatalf("fs backend is %T", b)
	}
	b, err = Open(Config{Backend: "s3", S3: S3Config{Endpoint: "http://127.0.0.1:9", Bucket: "b",
		AccessKey: secret.New("a"), SecretKey: secret.New("s")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.(*S3); !ok {
		t.Fatalf("s3 backend is %T", b)
	}
	for _, cfg := range []Config{{}, {Backend: "gcs"}, {Backend: "fs"}, {Backend: "s3"}} {
		_, err := Open(cfg)
		wantCode(t, err, "staging.config_invalid")
	}
}

func TestSHA256Reader(t *testing.T) {
	data := prngBytes(t, 1, 100_000)
	r, digest := SHA256Reader(iotest.HalfReader(bytes.NewReader(data)))
	if digest() != sha256Hex(nil) {
		t.Fatal("digest before any read is not the empty hash")
	}
	if _, err := io.CopyN(io.Discard, r, 1000); err != nil {
		t.Fatal(err)
	}
	if digest() != sha256Hex(data[:1000]) {
		t.Fatal("running digest is wrong")
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatal(err)
	}
	if digest() != sha256Hex(data) || digest() != sha256Hex(data) {
		t.Fatal("final digest is wrong or not repeatable")
	}
}

func TestKeyHelpers(t *testing.T) {
	if got := RunPrefix("01HZX"); got != "runs/01HZX/" {
		t.Errorf("RunPrefix = %q", got)
	}
	if got := ExportKey("01HZX"); got != "runs/01HZX/export/slack-export.zip" {
		t.Errorf("ExportKey = %q", got)
	}
	if got := BatchKey("01HZX", "extract", "C012AB.2024-01-31", 42); got != "runs/01HZX/extract/C012AB.2024-01-31/000042.ndjson.gz" {
		t.Errorf("BatchKey = %q", got)
	}
	// Batches list in sequence order.
	if !(BatchKey("r", "s", "l", 9) < BatchKey("r", "s", "l", 10) && BatchKey("r", "s", "l", 99999) < BatchKey("r", "s", "l", 100000)) {
		t.Error("batch keys do not sort by sequence")
	}

	hostile := []string{"", ".", "..", "...", "../../etc/passwd", "/abs", "a/b", `a\b`, "a b", "a\x00b", "a\nb", "~", "~2F", "a~b",
		".hidden", tmpPrefix + "x", "ü", "\xff\xfe", "C012AB", "c012ab", "a.b", "a_b-c", "a/b/", "//",
		strings.Repeat("x", 500), strings.Repeat("x", 499) + "y", strings.Repeat("/", 300)}
	seen := map[string]string{}
	for _, in := range hostile {
		seg := safeSegment(in)
		if strings.ContainsAny(seg, "/\\") || seg == "" || len(seg) > 120 || strings.HasPrefix(seg, ".") {
			t.Errorf("safeSegment(%q) = %q is not a safe single segment", in, seg)
		}
		if prev, dup := seen[seg]; dup {
			t.Errorf("safeSegment maps %q and %q to the same segment %q", prev, in, seg)
		}
		seen[seg] = in
		for _, key := range []string{RunPrefix(in) + "x", ExportKey(in), BatchKey(in, in, in, 1), BatchKey("r", "extract", in, -5)} {
			if err := ValidateKey(key); err != nil {
				t.Errorf("helper produced an invalid key from %q: %v", in, err)
			}
			if !strings.HasPrefix(key, "runs/") || strings.Count(key, "/") != strings.Count(strings.NewReplacer(in, "").Replace(key), "/") && in != "" && !strings.Contains(in, "/") {
				t.Errorf("unexpected key shape %q", key)
			}
		}
		if got := strings.Count(BatchKey("r", "s", in, 1), "/"); got != 4 {
			t.Errorf("BatchKey with lane %q has %d separators: %q", in, got, BatchKey("r", "s", in, 1))
		}
	}
	// A run can never reach into another run's prefix.
	if strings.HasPrefix(BatchKey("a/../b", "s", "l", 1), "runs/b/") || RunPrefix("a") == RunPrefix("a/") {
		t.Error("run IDs are not isolated")
	}
}
