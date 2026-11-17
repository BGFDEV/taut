package staging

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestConformance(t *testing.T) {
	for _, be := range backends {
		t.Run(be.name, func(t *testing.T) {
			t.Run("basics", func(t *testing.T) { testBasics(t, be.new(t)) })
			t.Run("sizes", func(t *testing.T) { testSizes(t, be.new(t)) })
			t.Run("list", func(t *testing.T) { testList(t, be.new(t)) })
			t.Run("delete-prefix", func(t *testing.T) { testDeletePrefix(t, be.new(t)) })
			t.Run("invalid-keys", func(t *testing.T) { testInvalidKeys(t, be.new(t)) })
			t.Run("interrupted-put", func(t *testing.T) { testInterruptedPut(t, be.new(t)) })
			t.Run("concurrent", func(t *testing.T) { testConcurrent(t, be.new(t)) })
			t.Run("large-streaming", func(t *testing.T) { testLargeStreaming(t, be.new(t)) })
		})
	}
}

func testBasics(t *testing.T, b instance) {
	ctx := context.Background()
	const key = "runs/r1/extract/C1/000001.ndjson.gz"

	if _, err := b.Get(ctx, key); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Get of a missing key: %v, want ErrNotExist", err)
	}
	if _, err := b.Stat(ctx, key); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Stat of a missing key: %v, want ErrNotExist", err)
	}
	if err := b.Delete(ctx, key); err != nil {
		t.Fatalf("Delete of a missing key: %v", err)
	}
	// A key under a missing "folder", and a "folder" that is not an object.
	mustPut(t, b, key, []byte("first"))
	if _, err := b.Get(ctx, "runs/r1/extract"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Get of a key that is only a prefix: %v, want ErrNotExist", err)
	}
	if _, err := b.Stat(ctx, "runs/r1"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Stat of a key that is only a prefix: %v, want ErrNotExist", err)
	}
	if err := b.Delete(ctx, "runs/r1"); err != nil {
		t.Fatalf("Delete of a key that is only a prefix: %v", err)
	}

	if got := mustGet(t, b, key); string(got) != "first" {
		t.Fatalf("got %q", got)
	}
	st, err := b.Stat(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if st.Key != key || st.Size < 5 {
		t.Fatalf("Stat = %+v", st)
	}

	// Overwrite with longer, then shorter content.
	mustPut(t, b, key, []byte("second, longer content"))
	if got := mustGet(t, b, key); string(got) != "second, longer content" {
		t.Fatalf("after overwrite got %q", got)
	}
	mustPut(t, b, key, []byte("3"))
	if got := mustGet(t, b, key); string(got) != "3" {
		t.Fatalf("after second overwrite got %q", got)
	}

	if err := b.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Get(ctx, key); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Get after Delete: %v", err)
	}
	if err := b.Delete(ctx, key); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if left := b.leftovers(t); len(left) != 0 {
		t.Fatalf("leftovers: %v", left)
	}
}

func testSizes(t *testing.T, b instance) {
	for _, n := range []int{0, 1, ChunkSize - 1, ChunkSize, ChunkSize + 1, 2 * ChunkSize, 3*ChunkSize + 17} {
		key := fmt.Sprintf("sizes/%d", n)
		want := prngBytes(t, uint64(n), n)
		mustPut(t, b, key, want)
		if got := mustGet(t, b, key); !bytes.Equal(got, want) {
			t.Fatalf("size %d: content differs (got %d bytes)", n, len(got))
		}
		st, err := b.Stat(context.Background(), key)
		if err != nil {
			t.Fatal(err)
		}
		if st.Size < int64(n) {
			t.Fatalf("size %d: Stat reports %d", n, st.Size)
		}
	}
}

func testList(t *testing.T, b instance) {
	ctx := context.Background()
	if got := listKeys(t, b, ""); len(got) != 0 {
		t.Fatalf("fresh store lists %v", got)
	}
	// Byte order: '.' < '/' < '0', so "a.txt" < "a/b" < "a0".
	keys := []string{"b", "a/c/d", "a0", "ab/x", "a.txt", "a/b", "a/c/e", "Z", "ü/1"}
	for _, k := range keys {
		mustPut(t, b, k, []byte(k))
	}
	sorted := slices.Clone(keys)
	slices.Sort(sorted)

	for _, prefix := range []string{"", "a", "a/", "a/c", "a/c/", "a/c/d", "ab", "nope", "nope/", "a/c/d/x", "ü"} {
		var want []string
		for _, k := range sorted {
			if strings.HasPrefix(k, prefix) {
				want = append(want, k)
			}
		}
		if got := listKeys(t, b, prefix); !slices.Equal(got, want) && (len(got)+len(want) > 0) {
			t.Errorf("List(%q) = %v, want %v", prefix, got, want)
		}
	}

	// Sizes agree with Stat, and iteration can stop early.
	n := 0
	for info, err := range b.List(ctx, "") {
		if err != nil {
			t.Fatal(err)
		}
		st, err := b.Stat(ctx, info.Key)
		if err != nil || st != info {
			t.Fatalf("List gave %+v, Stat gave %+v (%v)", info, st, err)
		}
		if n++; n == 3 {
			break
		}
	}

	for info, err := range b.List(ctx, "../") {
		wantCode(t, err, "staging.invalid_key")
		_ = info
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	sawErr := false
	for _, err := range b.List(cctx, "") {
		if err == nil {
			t.Fatal("List yielded an object under a cancelled context")
		}
		sawErr = errors.Is(err, context.Canceled)
	}
	if !sawErr {
		t.Fatal("List under a cancelled context did not report context.Canceled")
	}
}

func testDeletePrefix(t *testing.T, b instance) {
	ctx := context.Background()
	for _, k := range []string{"runs/a/1", "runs/a/x/2", "runs/a/x/3", "runs/ab/4", "runs/b/5", "other"} {
		mustPut(t, b, k, []byte(k))
	}
	n, err := b.DeletePrefix(ctx, "runs/a/")
	if err != nil || n != 3 {
		t.Fatalf("DeletePrefix = %d, %v; want 3", n, err)
	}
	if got, want := listKeys(t, b, ""), []string{"other", "runs/ab/4", "runs/b/5"}; !slices.Equal(got, want) {
		t.Fatalf("after DeletePrefix: %v, want %v", got, want)
	}
	if n, err := b.DeletePrefix(ctx, "runs/a/"); err != nil || n != 0 {
		t.Fatalf("second DeletePrefix = %d, %v", n, err)
	}
	// A prefix is a string prefix, not a folder.
	if n, err := b.DeletePrefix(ctx, "runs/a"); err != nil || n != 1 {
		t.Fatalf("DeletePrefix(runs/a) = %d, %v; want 1", n, err)
	}
	// The space is reusable afterwards.
	mustPut(t, b, "runs/a/1", []byte("again"))
	if n, err := b.DeletePrefix(ctx, ""); err != nil || n != 3 {
		t.Fatalf("DeletePrefix(\"\") = %d, %v; want 3", n, err)
	}
	if got := listKeys(t, b, ""); len(got) != 0 {
		t.Fatalf("store not empty: %v", got)
	}
	if _, err := b.DeletePrefix(ctx, "/etc"); err == nil {
		t.Fatal("DeletePrefix accepted an absolute prefix")
	}
}

var hostileKeys = []string{
	"",
	"/",
	"/etc/passwd",
	"/abs",
	"../x",
	"..",
	".",
	"a/../b",
	"a/..",
	"a/./b",
	"./a",
	"a//b",
	"a/",
	"a/b/",
	"//a",
	`a\b`,
	`..\x`,
	`C:\Windows\x`,
	"a\x00b",
	"a\nb",
	"a\tb",
	"a\x7fb",
	"a\u0085b",
	"a\xffb",
	"a/" + tmpPrefix + "x",
	tmpPrefix + "x/a",
	strings.Repeat("k", MaxKeyLength+1),
	"a/" + strings.Repeat("s", maxSegmentLength+1),
}

func testInvalidKeys(t *testing.T, b instance) {
	ctx := context.Background()
	mustPut(t, b, "x", []byte("canary"))
	for _, key := range hostileKeys {
		_, err := b.Put(ctx, key, strings.NewReader("payload"))
		wantCode(t, err, "staging.invalid_key")
		_, err = b.Get(ctx, key)
		wantCode(t, err, "staging.invalid_key")
		_, err = b.Stat(ctx, key)
		wantCode(t, err, "staging.invalid_key")
		wantCode(t, b.Delete(ctx, key), "staging.invalid_key")
		if strings.ContainsAny(err.Error(), "\x00\n\t\x7f\xff") {
			t.Errorf("error for %q contains raw control bytes: %q", key, err.Error())
		}
	}
	if got := listKeys(t, b, ""); !slices.Equal(got, []string{"x"}) {
		t.Fatalf("hostile keys changed the store: %v", got)
	}
	// Long but legal keys work.
	long := strings.Repeat("d", 200) + "/" + strings.Repeat("f", 200)
	mustPut(t, b, long, []byte("ok"))
	if got := mustGet(t, b, long); string(got) != "ok" {
		t.Fatalf("got %q", got)
	}
}

// interruptingReader yields deterministic data and, after limit bytes, runs
// trip once (cancel a context, or start failing).
type interruptingReader struct {
	src   io.Reader
	limit int64
	trip  func() error
	n     int64
}

func (r *interruptingReader) Read(p []byte) (int, error) {
	if r.n >= r.limit {
		if err := r.trip(); err != nil {
			return 0, err
		}
	}
	n, err := r.src.Read(p[:min(len(p), 64<<10)])
	r.n += int64(n)
	return n, err
}

func testInterruptedPut(t *testing.T, b instance) {
	bg := context.Background()
	const size = 4 * testPartSize // far more than is read before the interruption
	errBoom := errors.New("boom: upstream failed")

	cases := []struct {
		name  string
		limit int64
		run   func(key string, limit int64) error
		is    error
	}{
		{"cancel", 0, nil, context.Canceled},
		{"cancel-mid-multipart", 2*testPartSize + 4096, nil, context.Canceled},
		{"reader-error", 100, nil, errBoom},
		{"reader-error-mid-multipart", 2*testPartSize + 4096, nil, errBoom},
		{"reader-unexpected-eof", testPartSize + 10, nil, io.ErrUnexpectedEOF},
	}
	for i, c := range cases {
		for _, existing := range []bool{false, true} {
			key := fmt.Sprintf("interrupted/%d-%v", i, existing)
			if existing {
				mustPut(t, b, key, []byte("old content"))
			}
			ctx, cancel := context.WithCancel(bg)
			r := &interruptingReader{src: prng(1, size), limit: c.limit}
			switch c.is {
			case context.Canceled:
				r.trip = func() error { cancel(); return nil }
			default:
				r.trip = func() error { return c.is }
			}
			_, err := b.Put(ctx, key, r)
			cancel()
			if !errors.Is(err, c.is) {
				t.Fatalf("%s: Put returned %v, want %v", c.name, err, c.is)
			}
			if existing {
				if got := mustGet(t, b, key); string(got) != "old content" {
					t.Fatalf("%s: old object damaged: %d bytes", c.name, len(got))
				}
			} else if _, err := b.Get(bg, key); !errors.Is(err, ErrNotExist) {
				t.Fatalf("%s: object visible after failed Put (%v)", c.name, err)
			}
			if left := b.leftovers(t); len(left) != 0 {
				t.Fatalf("%s: leftovers after failed Put: %v", c.name, left)
			}
		}
	}

	// A context cancelled before Put starts.
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if _, err := b.Put(ctx, "interrupted/pre", strings.NewReader("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put under a cancelled context: %v", err)
	}
	want := []string{}
	for i := range cases {
		want = append(want, fmt.Sprintf("interrupted/%d-true", i))
	}
	if got := listKeys(t, b, "interrupted/"); !slices.Equal(got, want) {
		t.Fatalf("visible objects: %v, want %v", got, want)
	}
}

func testConcurrent(t *testing.T, b instance) {
	ctx := context.Background()
	var wg sync.WaitGroup

	// Different keys.
	const n = 16
	for i := range n {
		wg.Go(func() {
			key := fmt.Sprintf("conc/%02d", i)
			want := prngBytes(t, uint64(i), 100_000+i)
			if _, err := b.Put(ctx, key, bytes.NewReader(want)); err != nil {
				t.Errorf("Put %s: %v", key, err)
				return
			}
			rc, err := b.Get(ctx, key)
			if err != nil {
				t.Errorf("Get %s: %v", key, err)
				return
			}
			defer rc.Close()
			if got, err := io.ReadAll(rc); err != nil || !bytes.Equal(got, want) {
				t.Errorf("%s: content differs (%v)", key, err)
			}
		})
	}
	wg.Wait()
	if got := listKeys(t, b, "conc/"); len(got) != n {
		t.Fatalf("listed %d objects, want %d", len(got), n)
	}

	// One key, overwritten while it is read: every read sees one complete
	// version, never a mixture or a partial object.
	versions := [][]byte{bytes.Repeat([]byte{'A'}, 300_000), bytes.Repeat([]byte{'B'}, 200_000)}
	mustPut(t, b, "conc/same", versions[0])
	stop := make(chan struct{})
	for w := range 2 {
		wg.Go(func() {
			for i := 0; i < 15; i++ {
				if _, err := b.Put(ctx, "conc/same", bytes.NewReader(versions[(i+w)%2])); err != nil {
					t.Errorf("overwrite: %v", err)
					return
				}
			}
		})
	}
	var readers sync.WaitGroup
	for range 3 {
		readers.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				rc, err := b.Get(ctx, "conc/same")
				if err != nil {
					t.Errorf("Get during overwrite: %v", err)
					return
				}
				got, err := io.ReadAll(rc)
				rc.Close()
				if err != nil || !(bytes.Equal(got, versions[0]) || bytes.Equal(got, versions[1])) {
					t.Errorf("read during overwrite saw a partial or mixed object (%d bytes, %v)", len(got), err)
					return
				}
			}
		})
	}
	wg.Wait()
	close(stop)
	readers.Wait()
	if left := b.leftovers(t); len(left) != 0 {
		t.Fatalf("leftovers: %v", left)
	}
}

func testLargeStreaming(t *testing.T, b instance) {
	const size = 40 << 20
	// The bound is generous (the S3 backend alone holds one 5 MiB part) but
	// far below the object size: nothing may buffer the whole object.
	const bound = 16 << 20
	ctx := context.Background()
	defer debug.SetGCPercent(debug.SetGCPercent(10))

	wantHash := sha256.New()
	if _, err := io.Copy(wantHash, prng(42, size)); err != nil {
		t.Fatal(err)
	}
	want := hex.EncodeToString(wantHash.Sum(nil))

	var putDigest func() string
	putPeak := heapPeak(func() {
		r, digest := SHA256Reader(prng(42, size))
		putDigest = digest
		n, err := b.Put(ctx, "large/object", r)
		if err != nil || n != size {
			t.Fatalf("Put = %d, %v", n, err)
		}
	})
	if putDigest() != want {
		t.Fatal("SHA256Reader digest differs from the content hash")
	}
	st, err := b.Stat(ctx, "large/object")
	if err != nil || st.Size < size {
		t.Fatalf("Stat = %+v, %v", st, err)
	}

	var got string
	getPeak := heapPeak(func() {
		rc, err := b.Get(ctx, "large/object")
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		h := sha256.New()
		n, err := io.Copy(h, rc)
		if err != nil || n != size {
			t.Fatalf("read %d bytes, %v", n, err)
		}
		got = hex.EncodeToString(h.Sum(nil))
	})
	if got != want {
		t.Fatal("content hash did not round-trip")
	}
	t.Logf("%d MiB object: peak heap growth Put %.2f MiB, Get %.2f MiB (bound %d MiB)",
		size>>20, float64(putPeak)/(1<<20), float64(getPeak)/(1<<20), bound>>20)
	if putPeak > bound || getPeak > bound {
		t.Fatalf("heap grew by Put %d / Get %d bytes; streaming must stay under %d", putPeak, getPeak, bound)
	}
}
