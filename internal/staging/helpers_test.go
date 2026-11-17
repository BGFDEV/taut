package staging

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"iter"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/secret"
)

// testKey returns a DataKey with known bytes (seed, seed+1, …).
func testKey(seed byte) DataKey {
	k := DataKey{newKeyRef()}
	for i := range k.m().b {
		k.m().b[i] = seed + byte(i)
	}
	return k
}

// prng returns n deterministic pseudo-random bytes as a stream.
func prng(seed uint64, n int64) io.Reader {
	var s [32]byte
	s[0], s[1] = byte(seed), byte(seed>>8)
	return io.LimitReader(rand.NewChaCha8(s), n)
}

func prngBytes(t testing.TB, seed uint64, n int) []byte {
	t.Helper()
	b, err := io.ReadAll(prng(seed, int64(n)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func wantCode(t testing.TB, err error, code string) {
	t.Helper()
	var fe *fail.Error
	if !errors.As(err, &fe) {
		t.Fatalf("want a *fail.Error with code %s, got %v", code, err)
	}
	if fe.Code != code {
		t.Fatalf("want code %s, got %s (%v)", code, fe.Code, err)
	}
	if fe.What == "" || fe.Next == "" {
		t.Fatalf("error %s lacks What or Next: %+v", code, fe)
	}
}

func mustPut(t testing.TB, b Blobs, key string, content []byte) {
	t.Helper()
	n, err := b.Put(context.Background(), key, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Put %q: %v", key, err)
	}
	if n != int64(len(content)) {
		t.Fatalf("Put %q returned %d, want %d", key, n, len(content))
	}
}

func mustGet(t testing.TB, b Blobs, key string) []byte {
	t.Helper()
	rc, err := b.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get %q: %v", key, err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading %q: %v", key, err)
	}
	return got
}

func listKeys(t testing.TB, b Blobs, prefix string) []string {
	t.Helper()
	keys := []string{}
	for info, err := range b.List(context.Background(), prefix) {
		if err != nil {
			t.Fatalf("List %q: %v", prefix, err)
		}
		keys = append(keys, info.Key)
	}
	return keys
}

// memBlobs is an in-memory Blobs whose stored bytes tests can tamper with.
type memBlobs struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newMem() *memBlobs { return &memBlobs{m: map[string][]byte{}} }

func (m *memBlobs) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	if err := ValidateKey(key); err != nil {
		return 0, err
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m[key] = b
	return int64(len(b)), nil
}

func (m *memBlobs) raw(key string) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.m[key])
}

func (m *memBlobs) set(key string, b []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m[key] = b
}

func (m *memBlobs) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.m[key]
	if !ok {
		return nil, notExist(key)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *memBlobs) Stat(ctx context.Context, key string) (Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.m[key]
	if !ok {
		return Info{}, notExist(key)
	}
	return Info{Key: key, Size: int64(len(b))}, nil
}

func (m *memBlobs) Delete(ctx context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.m, key)
	return nil
}

func (m *memBlobs) List(ctx context.Context, prefix string) iter.Seq2[Info, error] {
	return func(yield func(Info, error) bool) {
		m.mu.Lock()
		var infos []Info
		for k, v := range m.m {
			if strings.HasPrefix(k, prefix) {
				infos = append(infos, Info{Key: k, Size: int64(len(v))})
			}
		}
		m.mu.Unlock()
		slices.SortFunc(infos, func(a, b Info) int { return strings.Compare(a.Key, b.Key) })
		for _, i := range infos {
			if !yield(i, nil) {
				return
			}
		}
	}
}

func (m *memBlobs) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k := range m.m {
		if strings.HasPrefix(k, prefix) {
			delete(m.m, k)
			n++
		}
	}
	return n, nil
}

// --- backends under test ---

// instance is one fresh, empty store.
type instance struct {
	Blobs
	// leftovers lists what an interrupted Put must not leave behind:
	// temporary files (fs) or unfinished multipart uploads (s3).
	leftovers func(t testing.TB) []string
}

type backend struct {
	name string
	new  func(t testing.TB) instance
}

func fsInstance(t testing.TB) instance {
	root := t.TempDir()
	b, err := NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.(io.Closer).Close() })
	return instance{Blobs: b, leftovers: func(t testing.TB) []string {
		var found []string
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err == nil && strings.HasPrefix(d.Name(), tmpPrefix) {
				found = append(found, p)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return found
	}}
}

const testPartSize = MinPartSize

func s3TestConfig(t testing.TB) S3Config {
	endpoint := os.Getenv("TAUT_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("S3 tests skipped: set TAUT_TEST_S3_ENDPOINT, TAUT_TEST_S3_ACCESS_KEY and TAUT_TEST_S3_SECRET_KEY to run them against an S3-compatible server")
	}
	region := os.Getenv("TAUT_TEST_S3_REGION")
	if region == "" {
		region = "us-east-1"
	}
	var suffix [8]byte
	_, _ = crand.Read(suffix[:])
	return S3Config{
		Endpoint:  endpoint,
		Region:    region,
		Bucket:    "taut-test-" + hex.EncodeToString(suffix[:]),
		AccessKey: secret.New(os.Getenv("TAUT_TEST_S3_ACCESS_KEY")),
		SecretKey: secret.New(os.Getenv("TAUT_TEST_S3_SECRET_KEY")),
		PartSize:  testPartSize,
	}
}

// newTestS3 creates a uniquely named bucket and removes it afterwards.
func newTestS3(t testing.TB) *S3 {
	s, err := NewS3(s3TestConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureBucket(context.Background()); err != nil {
		t.Fatalf("EnsureBucket: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := s.DeletePrefix(ctx, ""); err != nil {
			t.Errorf("cleanup: %v", err)
		}
		if _, err := s.client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &s.bucket}); err != nil {
			t.Errorf("cleanup: deleting bucket: %v", err)
		}
	})
	return s
}

func s3Instance(t testing.TB) instance {
	s := newTestS3(t)
	return instance{Blobs: s, leftovers: func(t testing.TB) []string {
		ups, err := s.pendingUploads(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		var found []string
		for _, u := range ups {
			found = append(found, aws.ToString(u.Key)+"#"+aws.ToString(u.UploadId))
		}
		return found
	}}
}

func encryptedOver(inner func(testing.TB) instance) func(testing.TB) instance {
	return func(t testing.TB) instance {
		in := inner(t)
		in.Blobs = Encrypted(in.Blobs, testKey(7))
		return in
	}
}

var backends = []backend{
	{"fs", fsInstance},
	{"encrypted-fs", encryptedOver(fsInstance)},
	{"s3", s3Instance},
	{"encrypted-s3", encryptedOver(s3Instance)},
}

// heapPeak runs f and returns how far the live heap rose above its level at
// the start, sampled every millisecond.
func heapPeak(f func()) uint64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base, peak := ms.HeapAlloc, ms.HeapAlloc
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		var ms runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
				runtime.ReadMemStats(&ms)
				peak = max(peak, ms.HeapAlloc)
			}
		}
	}()
	f()
	close(stop)
	<-done
	if peak < base {
		return 0
	}
	return peak - base
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
