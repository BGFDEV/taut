package staging

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"
	"testing/iotest"
)

const (
	tcs    = 32 // small chunk size, so tampering tests can be exhaustive
	tchunk = tcs + gcmTagSize
)

// smallChunks returns an encrypted view over mem with 32-byte chunks.
func smallChunks(mem *memBlobs, key DataKey) *encrypted {
	e := newEncrypted(mem, key)
	e.chunkSize = tcs
	return e
}

// seal encrypts plain under name with small chunks and returns the stored
// bytes.
func seal(t testing.TB, key DataKey, name string, plain []byte) []byte {
	t.Helper()
	mem := newMem()
	mustPut(t, smallChunks(mem, key), name, plain)
	return mem.raw(name)
}

// decrypt runs the decrypting reader over stored bytes and returns whatever
// plaintext it released along with its final error (nil for a clean EOF).
func decrypt(key DataKey, name string, stored []byte) ([]byte, error) {
	return decryptFrom(key, name, bytes.NewReader(stored))
}

func decryptFrom(key DataKey, name string, src io.Reader) ([]byte, error) {
	d := &decryptReader{src: src, closer: io.NopCloser(nil), key: key, objectKey: name}
	defer d.Close()
	var out bytes.Buffer
	_, err := io.Copy(&out, d)
	return out.Bytes(), err
}

// wantCorrupt asserts that decryption failed with staging.corrupt and that
// the only plaintext released is whole, genuine leading chunks, at most
// maxChunks of them.
func wantCorrupt(t testing.TB, what string, plain, got []byte, err error, maxChunks int) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: decrypted without error (%d bytes)", what, len(got))
	}
	wantCode(t, err, "staging.corrupt")
	if len(got)%tcs != 0 || len(got) > maxChunks*tcs || !bytes.HasPrefix(plain, got) {
		t.Fatalf("%s: released %d bytes of unauthenticated or out-of-place plaintext (limit %d chunks)", what, len(got), maxChunks)
	}
}

func TestEncryptedRoundTripSizes(t *testing.T) {
	mem := newMem()
	enc := Encrypted(mem, testKey(1))
	ctx := context.Background()
	for _, n := range []int{0, 1, ChunkSize - 1, ChunkSize, ChunkSize + 1, 2 * ChunkSize, 3*ChunkSize + 17} {
		key := fmt.Sprintf("rt/%d", n)
		plain := prngBytes(t, uint64(n)+1, n)
		mustPut(t, enc, key, plain)

		// Through readers of awkward granularity, too.
		for name, wrap := range map[string]func(io.Reader) io.Reader{
			"plain":    func(r io.Reader) io.Reader { return r },
			"one-byte": iotest.OneByteReader,
			"data-err": iotest.DataErrReader,
			"half":     iotest.HalfReader,
		} {
			if name == "one-byte" && n > ChunkSize+1 {
				continue // slow, and adds nothing at these sizes
			}
			got, err := decryptFrom(testKey(1), key, wrap(bytes.NewReader(mem.raw(key))))
			if err != nil || !bytes.Equal(got, plain) {
				t.Fatalf("size %d via %s reader: %v (got %d bytes)", n, name, err, len(got))
			}
		}

		stored := mem.raw(key)
		if bytes.Contains(stored, plain[:min(n, 24)]) && n >= 8 {
			t.Fatalf("size %d: plaintext visible in the stored object", n)
		}
		st, err := enc.Stat(ctx, key)
		if err != nil || st.Size != int64(len(stored)) {
			t.Fatalf("Stat = %+v, %v; stored %d", st, err, len(stored))
		}
		if got := CiphertextSize(int64(n)); got != st.Size {
			t.Errorf("CiphertextSize(%d) = %d, stored %d", n, got, st.Size)
		}
		if got := PlaintextSize(st.Size); got != int64(n) {
			t.Errorf("PlaintextSize(%d) = %d, want %d", st.Size, got, n)
		}
	}
	for _, bad := range []int64{-1, 0, int64(HeaderSize), int64(HeaderSize) + gcmTagSize - 1,
		CiphertextSize(ChunkSize) + 1, CiphertextSize(ChunkSize) + gcmTagSize, CiphertextSize(2*ChunkSize) + 5} {
		if got := PlaintextSize(bad); got != -1 {
			t.Errorf("PlaintextSize(%d) = %d, want -1", bad, got)
		}
	}
}

// The source may deliver its content in any pattern of reads.
func TestEncryptAwkwardSources(t *testing.T) {
	for _, n := range []int{0, 1, tcs - 1, tcs, tcs + 1, 2 * tcs, 3*tcs + 17} {
		plain := prngBytes(t, 9, n)
		for name, wrap := range map[string]func(io.Reader) io.Reader{
			"one-byte": iotest.OneByteReader,
			"data-err": iotest.DataErrReader,
			"half":     iotest.HalfReader,
		} {
			mem := newMem()
			nn, err := smallChunks(mem, testKey(1)).Put(context.Background(), "k", wrap(bytes.NewReader(plain)))
			if err != nil || nn != int64(n) {
				t.Fatalf("size %d via %s: Put = %d, %v", n, name, nn, err)
			}
			wantLen := HeaderSize + n + gcmTagSize*max(1, (n+tcs-1)/tcs)
			if len(mem.raw("k")) != wantLen {
				t.Fatalf("size %d via %s: stored %d bytes, want %d", n, name, len(mem.raw("k")), wantLen)
			}
			got, err := decrypt(testKey(1), "k", mem.raw("k"))
			if err != nil || !bytes.Equal(got, plain) {
				t.Fatalf("size %d via %s: %v", n, name, err)
			}
		}
	}
}

func TestEncryptedCiphertextsDiffer(t *testing.T) {
	plain := prngBytes(t, 3, 3*tcs+17)
	a := seal(t, testKey(1), "k", plain)
	b := seal(t, testKey(1), "k", plain)
	if bytes.Equal(a, b) {
		t.Fatal("two Puts of the same plaintext produced the same ciphertext")
	}
	// No chunk repeats either.
	for i := HeaderSize; i+tchunk <= len(a); i += tchunk {
		if bytes.Equal(a[i:i+tchunk], b[i:i+tchunk]) {
			t.Fatalf("chunk at offset %d is identical across two Puts", i)
		}
	}
}

func TestTamperEveryByte(t *testing.T) {
	key := testKey(1)
	for _, n := range []int{0, 1, tcs, 3*tcs + 17} {
		plain := prngBytes(t, 4, n)
		stored := seal(t, key, "k", plain)
		for i := range stored {
			for _, mask := range []byte{0x01, 0x80} {
				bad := bytes.Clone(stored)
				bad[i] ^= mask
				got, err := decrypt(key, "k", bad)
				// Chunks wholly before the flipped byte are genuine.
				clean := 0
				if i >= HeaderSize {
					clean = (i - HeaderSize) / tchunk
				}
				wantCorrupt(t, fmt.Sprintf("size %d, byte %d", n, i), plain, got, err, clean)
				if len(got) != clean*tcs {
					t.Fatalf("size %d, byte %d: released %d bytes, want exactly the %d clean chunks", n, i, len(got), clean)
				}
			}
		}
	}
}

func TestTruncationAtEveryLength(t *testing.T) {
	key := testKey(1)
	for _, n := range []int{0, 1, tcs, 2 * tcs, 3*tcs + 17} {
		plain := prngBytes(t, 5, n)
		stored := seal(t, key, "k", plain)
		for cut := 0; cut < len(stored); cut++ {
			got, err := decrypt(key, "k", stored[:cut])
			whole := 0
			if cut > HeaderSize {
				whole = (cut - HeaderSize) / tchunk
			}
			wantCorrupt(t, fmt.Sprintf("size %d cut to %d", n, cut), plain, got, err, whole)
			if len(got) >= len(plain) && len(plain) > 0 {
				t.Fatalf("size %d cut to %d: the whole plaintext was released before the error", n, cut)
			}
		}
	}
}

func TestChunkSurgery(t *testing.T) {
	key := testKey(1)
	plain := prngBytes(t, 6, 4*tcs+5) // five chunks
	stored := seal(t, key, "k", plain)
	header := stored[:HeaderSize]
	var chunks [][]byte
	for i := HeaderSize; i < len(stored); i += tchunk {
		chunks = append(chunks, stored[i:min(i+tchunk, len(stored))])
	}
	if len(chunks) != 5 {
		t.Fatalf("setup: %d chunks", len(chunks))
	}
	build := func(order ...int) []byte {
		out := bytes.Clone(header)
		for _, i := range order {
			out = append(out, chunks[i]...)
		}
		return out
	}
	cases := []struct {
		name  string
		data  []byte
		clean int // leading chunks still in their original position
	}{
		{"drop first", build(1, 2, 3, 4), 0},
		{"drop middle", build(0, 1, 3, 4), 2},
		{"drop last", build(0, 1, 2, 3), 3},
		{"swap 0 and 1", build(1, 0, 2, 3, 4), 0},
		{"swap 2 and 3", build(0, 1, 3, 2, 4), 2},
		{"duplicate middle", build(0, 1, 1, 2, 3, 4), 2},
		{"duplicate last", build(0, 1, 2, 3, 4, 4), 4},
		{"last only", build(4), 0},
		{"last moved first", build(4, 0, 1, 2, 3), 0},
		{"append one byte", append(build(0, 1, 2, 3, 4), 0), 4},
		{"append a tag-sized block", append(build(0, 1, 2, 3, 4), make([]byte, gcmTagSize)...), 4},
		{"append a whole chunk of zeros", append(build(0, 1, 2, 3, 4), make([]byte, tchunk)...), 4},
		{"append the object to itself", append(build(0, 1, 2, 3, 4), stored...), 4},
		{"header only", build(), 0},
		{"empty", nil, 0},
	}
	for _, c := range cases {
		got, err := decrypt(key, "k", c.data)
		wantCorrupt(t, c.name, plain, got, err, c.clean)
	}

	// Appending to an object whose last chunk is full, and to an empty one.
	for _, n := range []int{0, 2 * tcs} {
		p := prngBytes(t, 7, n)
		s := seal(t, key, "k", p)
		for _, extra := range [][]byte{{0}, make([]byte, gcmTagSize), make([]byte, tchunk)} {
			got, err := decrypt(key, "k", append(bytes.Clone(s), extra...))
			wantCorrupt(t, fmt.Sprintf("size %d plus %d bytes", n, len(extra)), p, got, err, n/tcs)
		}
	}

	// Splicing chunks between two objects under the same key and name.
	other := seal(t, key, "k", plain)
	spliced := append(bytes.Clone(stored[:HeaderSize+tchunk]), other[HeaderSize+tchunk:]...)
	got, err := decrypt(key, "k", spliced)
	wantCorrupt(t, "splice of two objects", plain, got, err, 1)
}

func TestWrongNameOrKey(t *testing.T) {
	plain := prngBytes(t, 8, 3*tcs+17)
	stored := seal(t, testKey(1), "runs/a/export", plain)

	got, err := decrypt(testKey(1), "runs/b/export", stored)
	wantCorrupt(t, "different key name", plain, got, err, 0)
	got, err = decrypt(testKey(2), "runs/a/export", stored)
	wantCorrupt(t, "wrong data key", plain, got, err, 0)

	// The same through the Blobs API: a store that swaps two objects.
	mem := newMem()
	enc := Encrypted(mem, testKey(1))
	mustPut(t, enc, "a", []byte("alpha"))
	mustPut(t, enc, "b", []byte("bravo"))
	a, b := mem.raw("a"), mem.raw("b")
	mem.set("a", b)
	mem.set("b", a)
	rc, err := enc.Get(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	out, err := io.ReadAll(rc)
	wantCode(t, err, "staging.corrupt")
	if len(out) != 0 {
		t.Fatalf("released %q from a swapped object", out)
	}
	if msg := err.Error(); strings.Contains(msg, "alpha") || strings.Contains(msg, "bravo") {
		t.Fatalf("plaintext in error: %s", msg)
	}
	// The error is sticky.
	if _, err2 := rc.Read(make([]byte, 1)); err2 != err {
		t.Fatalf("second Read returned %v", err2)
	}
}

// A stream that this package would never write (a full chunk followed by
// an empty final chunk) is rejected even though every chunk authenticates,
// so each plaintext has exactly one valid encoding per salt.
func TestNonCanonicalEmptyFinalChunkRejected(t *testing.T) {
	key := testKey(1)
	header := make([]byte, HeaderSize)
	copy(header, encMagic)
	binary.BigEndian.PutUint32(header[len(encMagic)+saltSize:], tcs)
	st, err := newStream(key, header, "k")
	if err != nil {
		t.Fatal(err)
	}
	plain := prngBytes(t, 1, tcs)
	out := bytes.Clone(header)
	n0, _ := st.next(false)
	out = st.aead.Seal(out, n0, plain, st.aad)
	n1, _ := st.next(true)
	out = st.aead.Seal(out, n1, nil, st.aad)

	got, err := decrypt(key, "k", out)
	wantCorrupt(t, "empty trailing chunk", plain, got, err, 1)
}

func TestHeaderChunkSizeBounds(t *testing.T) {
	for _, cs := range []uint32{0, maxChunkSize + 1, math.MaxUint32} {
		header := make([]byte, HeaderSize, HeaderSize+64)
		copy(header, encMagic)
		binary.BigEndian.PutUint32(header[len(encMagic)+saltSize:], cs)
		_, err := decrypt(testKey(1), "k", append(header, make([]byte, 64)...))
		wantCode(t, err, "staging.corrupt")
	}
}

func TestCounterOverflow(t *testing.T) {
	header := make([]byte, HeaderSize)
	st, err := newStream(testKey(1), header, "k")
	if err != nil {
		t.Fatal(err)
	}
	st.counter = math.MaxUint32
	nonce, err := st.next(false)
	if err != nil {
		t.Fatalf("the last counter value was refused: %v", err)
	}
	if !bytes.Equal(nonce[noncePrefixSize:], []byte{0xff, 0xff, 0xff, 0xff, 0}) {
		t.Fatalf("nonce tail = %x", nonce[noncePrefixSize:])
	}
	if _, err := st.next(true); err == nil {
		t.Fatal("the chunk counter wrapped")
	} else {
		wantCode(t, err, "staging.object_too_large")
	}

	// Both directions surface it.
	enc := &encryptReader{src: strings.NewReader("data"), st: st, plain: make([]byte, tcs), sealed: make([]byte, 0, tchunk)}
	_, err = io.ReadAll(enc)
	wantCode(t, err, "staging.object_too_large")

	stored := seal(t, testKey(1), "k", []byte("data"))
	d := &decryptReader{src: bytes.NewReader(stored), closer: io.NopCloser(nil), key: testKey(1), objectKey: "k"}
	if err := d.readHeader(); err != nil {
		t.Fatal(err)
	}
	d.st.counter = math.MaxUint32 + 1
	out, err := io.ReadAll(d)
	wantCode(t, err, "staging.object_too_large")
	if len(out) != 0 {
		t.Fatal("plaintext released past the counter limit")
	}
}

func TestNonceLayout(t *testing.T) {
	st, err := newStream(testKey(1), make([]byte, HeaderSize), "k")
	if err != nil {
		t.Fatal(err)
	}
	prefix := bytes.Clone(st.nonce[:noncePrefixSize])
	seen := map[string]bool{}
	for i, final := range []bool{false, false, true} {
		n, err := st.next(final)
		if err != nil {
			t.Fatal(err)
		}
		want := append(bytes.Clone(prefix), 0, 0, 0, byte(i), 0)
		if final {
			want[11] = 1
		}
		if !bytes.Equal(n, want) {
			t.Fatalf("nonce %d = %x, want %x", i, n, want)
		}
		seen[string(n)] = true
	}
	if len(seen) != 3 {
		t.Fatal("nonces repeat")
	}
}

// A failure of the underlying read is an I/O error, not corruption, and
// keeps its cause.
func TestReadErrorIsNotCorruption(t *testing.T) {
	plain := prngBytes(t, 2, 3*tcs)
	stored := seal(t, testKey(1), "k", plain)
	errNet := errors.New("connection reset")
	src := io.MultiReader(bytes.NewReader(stored[:HeaderSize+tchunk+5]), iotest.ErrReader(errNet))
	got, err := decryptFrom(testKey(1), "k", src)
	if !errors.Is(err, errNet) {
		t.Fatalf("err = %v, want the read error", err)
	}
	if !bytes.Equal(got, plain[:tcs]) {
		t.Fatalf("released %d bytes", len(got))
	}
}

func TestEncryptedPutErrors(t *testing.T) {
	mem := newMem()
	ctx := context.Background()

	// A destroyed key is refused before anything is stored or fetched.
	key := testKey(1)
	enc := Encrypted(mem, key)
	mustPut(t, enc, "k", []byte("x"))
	key.Destroy()
	_, err := enc.Put(ctx, "k2", strings.NewReader("x"))
	wantCode(t, err, "staging.key_destroyed")
	_, err = enc.Get(ctx, "k")
	wantCode(t, err, "staging.key_destroyed")
	_, err = Encrypted(mem, DataKey{}).Put(ctx, "k2", strings.NewReader("x"))
	wantCode(t, err, "staging.key_destroyed")
	if _, err := mem.Stat(ctx, "k2"); !errors.Is(err, ErrNotExist) {
		t.Fatal("an object was stored without a key")
	}

	// A failing randomness source stops the Put.
	e := newEncrypted(mem, testKey(1))
	e.rand = iotest.ErrReader(errors.New("no entropy"))
	if _, err := e.Put(ctx, "k3", strings.NewReader("x")); err == nil {
		t.Fatal("Put succeeded without a salt")
	}

	// A source that ends in io.ErrUnexpectedEOF is an error, not the end.
	e = newEncrypted(mem, testKey(1))
	src := io.MultiReader(strings.NewReader("partial"), iotest.ErrReader(io.ErrUnexpectedEOF))
	if _, err := e.Put(ctx, "k4", src); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Put of a truncated source: %v", err)
	}
	if _, err := mem.Stat(ctx, "k4"); !errors.Is(err, ErrNotExist) {
		t.Fatal("a truncated source was stored as a complete object")
	}

	// Close makes the reader unusable.
	enc = Encrypted(mem, testKey(1))
	mustPut(t, enc, "k5", []byte("hello"))
	rc, err := enc.Get(ctx, "k5")
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if n, err := rc.Read(make([]byte, 8)); n != 0 || err == nil {
		t.Fatalf("Read after Close = %d, %v", n, err)
	}
}

// --- known-answer test ---

// katSeal is an independent, straight-line implementation of the format
// (HKDF written out by hand over HMAC), used to cross-check both the
// production writer and the frozen vectors below.
func katSeal(t *testing.T, dek, salt []byte, chunkSize int, name string, plain []byte) []byte {
	t.Helper()
	header := append([]byte("TAUTENC1"), salt...)
	header = binary.BigEndian.AppendUint32(header, uint32(chunkSize))

	// RFC 5869: PRK = HMAC(salt, IKM); T(1) = HMAC(PRK, info | 0x01);
	// T(2) = HMAC(PRK, T(1) | info | 0x02).
	mac := hmac.New(sha256.New, salt)
	mac.Write(dek)
	prk := mac.Sum(nil)
	mac = hmac.New(sha256.New, prk)
	mac.Write([]byte("taut-staging-stream-v1\x01"))
	t1 := mac.Sum(nil)
	mac = hmac.New(sha256.New, prk)
	mac.Write(t1)
	mac.Write([]byte("taut-staging-stream-v1\x02"))
	okm := append(t1, mac.Sum(nil)...)
	subkey, prefix := okm[:32], okm[32:39]

	block, err := aes.NewCipher(subkey)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	aad := append(bytes.Clone(header), name...)
	out := bytes.Clone(header)
	for i := 0; ; i++ {
		chunk, final := plain, true
		if len(plain) > chunkSize {
			chunk, final = plain[:chunkSize], false
		}
		plain = plain[len(chunk):]
		nonce := append(bytes.Clone(prefix), byte(i>>24), byte(i>>16), byte(i>>8), byte(i), 0)
		if final {
			nonce[11] = 1
		}
		out = gcm.Seal(out, nonce, chunk, aad)
		if final {
			return out
		}
	}
}

func TestKnownAnswer(t *testing.T) {
	dek := testKey(0) // 00 01 02 … 1f
	salt, _ := hex.DecodeString("a0a1a2a3a4a5a6a7a8a9aaabacadaeaf")
	const name = "runs/kat/export/slack-export.zip"
	fox := []byte("The quick brown fox jumps over the lazy dog")

	vectors := []struct {
		label     string
		chunkSize int
		plain     []byte
		want      string
	}{
		{"three 16-byte chunks", 16, fox, katFox16},
		{"one default-size chunk", ChunkSize, fox, katFoxDefault},
		{"empty object", ChunkSize, nil, katEmpty},
		{"exactly two 16-byte chunks", 16, fox[:32], katFox32},
	}
	for _, v := range vectors {
		mem := newMem()
		e := newEncrypted(mem, dek)
		e.chunkSize = v.chunkSize
		e.rand = bytes.NewReader(salt)
		mustPut(t, e, name, v.plain)
		got := mem.raw(name)

		if ref := katSeal(t, dek.m().b[:], salt, v.chunkSize, name, v.plain); !bytes.Equal(got, ref) {
			t.Errorf("%s: production output differs from the reference implementation\n got %x\n ref %x", v.label, got, ref)
		}
		if hex.EncodeToString(got) != v.want {
			t.Errorf("%s: the on-disk format changed\n got  %x\n want %s", v.label, got, v.want)
		}
		want, _ := hex.DecodeString(v.want)
		plain, err := decrypt(dek, name, want)
		if err != nil || !bytes.Equal(plain, v.plain) {
			t.Errorf("%s: the frozen vector no longer decrypts: %v", v.label, err)
		}
	}
}

// Frozen ciphertexts for TestKnownAnswer. If one of these changes, the
// format changed: existing staged data would no longer decrypt.
const (
	katFox16      = "54415554454e4331a0a1a2a3a4a5a6a7a8a9aaabacadaeaf0000001041f3a483e32d712a7344af60326d92791881cc3779ebb0546b7b99889b1d6755382579d77b6a38a6be970d2676329be849355ac0cb10c390dad3498d7a8dc04012bb92cba0b9672c26ab5e0589d92f178ce2e870db0a356095f83c"
	katFoxDefault = "54415554454e4331a0a1a2a3a4a5a6a7a8a9aaabacadaeaf00010000b16fcd81c70ac50ba84d4b954d5821ff94647a001cc992c7519ce003439928aa7f77199262046c4b4ab80326c40eea0e38177635b1dc3904113aa0"
	katEmpty      = "54415554454e4331a0a1a2a3a4a5a6a7a8a9aaabacadaeaf000100009bcccad74273fc55bc66703f4a52c64d"
	katFox32      = "54415554454e4331a0a1a2a3a4a5a6a7a8a9aaabacadaeaf0000001041f3a483e32d712a7344af60326d92791881cc3779ebb0546b7b99889b1d67553e1dcb2b7afc9be2f447d0c5639e2a6ad4e4036ab61295cef6fb8d021fb8b604"
)
