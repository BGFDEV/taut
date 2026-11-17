package staging

import (
	"context"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"iter"
	"math"

	"github.com/bgfdev/taut/internal/fail"
)

// Format constants; see the package documentation for the layout.
const (
	encMagic = "TAUTENC1"
	saltSize = 16
	// HeaderSize is the size of the header of every encrypted object.
	HeaderSize = len(encMagic) + saltSize + 4
	// ChunkSize is the plaintext size of every chunk but the last in the
	// objects this package writes.
	ChunkSize = 64 << 10
	// maxChunkSize bounds what a reader will allocate on the word of an
	// untrusted header.
	maxChunkSize    = 1 << 20
	noncePrefixSize = 7
	hkdfInfo        = "taut-staging-stream-v1"
)

// Encrypted returns a view of inner in which object contents are encrypted
// and authenticated under key (the format is described in the package
// documentation). Object key names, object sizes and the listing are NOT
// hidden from the store: keep sensitive data out of key names.
//
// Put encrypts as it streams and Get decrypts as it streams; both use
// memory proportional to ChunkSize, not to the object. Stat and List report
// the stored (ciphertext) size; PlaintextSize converts it.
//
// A Get reader verifies each chunk before returning any of its bytes and
// returns io.EOF only after verifying the final chunk. Any tampering
// (modified, truncated, extended, reordered or renamed ciphertext, or the
// wrong data key) surfaces as a staging.corrupt error from Read. Bytes
// returned before such an error were authentic, but the object as a whole
// is not: the caller must discard them.
func Encrypted(inner Blobs, key DataKey) Blobs {
	return newEncrypted(inner, key)
}

type encrypted struct {
	inner Blobs
	key   DataKey
	// chunkSize and rand are fixed in production; tests vary them.
	chunkSize int
	rand      io.Reader
}

func newEncrypted(inner Blobs, key DataKey) *encrypted {
	return &encrypted{inner: inner, key: key, chunkSize: ChunkSize, rand: rand.Reader}
}

// PlaintextSize returns the content size of an encrypted object from its
// stored size as reported by Stat or List, or -1 if no encrypted object has
// that size. It assumes the default ChunkSize, which every object written by
// this package uses.
func PlaintextSize(ciphertextSize int64) int64 {
	const full = ChunkSize + gcmTagSize
	body := ciphertextSize - int64(HeaderSize)
	if body < gcmTagSize {
		return -1
	}
	chunks := (body + full - 1) / full
	last := body - (chunks-1)*full
	if last < gcmTagSize || (last == gcmTagSize && chunks > 1) {
		return -1
	}
	return body - chunks*gcmTagSize
}

// CiphertextSize returns the stored size of an object with the given content
// size, at the default ChunkSize.
func CiphertextSize(plaintextSize int64) int64 {
	chunks := (plaintextSize + ChunkSize - 1) / ChunkSize
	if chunks == 0 {
		chunks = 1 // an empty object is one empty chunk
	}
	return int64(HeaderSize) + plaintextSize + chunks*gcmTagSize
}

// stream is the per-object cipher state shared by both directions.
type stream struct {
	aead    cipher.AEAD
	aad     []byte // header | object key name
	nonce   [gcmNonceSize]byte
	counter uint64 // index of the next chunk
}

// newStream derives the object's subkey and nonce prefix.
func newStream(key DataKey, header []byte, objectKey string) (*stream, error) {
	dek, err := key.m().bytes("data key")
	if err != nil {
		return nil, err
	}
	salt := header[len(encMagic) : len(encMagic)+saltSize]
	okm, err := hkdf.Key(sha256.New, dek, salt, hkdfInfo, KeySize+noncePrefixSize)
	if err != nil {
		return nil, err
	}
	defer clear(okm)
	aead, err := newGCM(okm[:KeySize])
	if err != nil {
		return nil, err
	}
	s := &stream{aead: aead}
	copy(s.nonce[:noncePrefixSize], okm[KeySize:])
	s.aad = make([]byte, 0, len(header)+len(objectKey))
	s.aad = append(append(s.aad, header...), objectKey...)
	return s, nil
}

// next returns the nonce for the next chunk and advances the counter. The
// counter never wraps: running out of chunk numbers is an error.
func (s *stream) next(final bool) ([]byte, error) {
	if s.counter > math.MaxUint32 {
		return nil, fail.New("staging.object_too_large",
			"A staging object is too large to encrypt",
			"It has more than 2^32 chunks, the limit of the encryption format.",
			"This is a bug in Taut: no real export reaches this size. Please report it.")
	}
	binary.BigEndian.PutUint32(s.nonce[noncePrefixSize:], uint32(s.counter))
	s.nonce[gcmNonceSize-1] = 0
	if final {
		s.nonce[gcmNonceSize-1] = 1
	}
	s.counter++
	return s.nonce[:], nil
}

func (e *encrypted) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	if err := ValidateKey(key); err != nil {
		return 0, err
	}
	header := make([]byte, HeaderSize)
	copy(header, encMagic)
	if _, err := io.ReadFull(e.rand, header[len(encMagic):len(encMagic)+saltSize]); err != nil {
		return 0, fmt.Errorf("staging: reading randomness: %w", err)
	}
	binary.BigEndian.PutUint32(header[len(encMagic)+saltSize:], uint32(e.chunkSize))
	st, err := newStream(e.key, header, key)
	if err != nil {
		return 0, err
	}
	enc := &encryptReader{
		src:    r,
		st:     st,
		plain:  make([]byte, e.chunkSize),
		out:    header,
		sealed: make([]byte, 0, e.chunkSize+gcmTagSize),
	}
	defer clear(enc.plain)
	if _, err := e.inner.Put(ctx, key, enc); err != nil {
		return 0, err
	}
	return enc.n, nil
}

// encryptReader turns a plaintext reader into the ciphertext stream. It is
// pull-based: the backend's Put reads from it, so there is no goroutine and
// no buffering beyond one chunk.
type encryptReader struct {
	src io.Reader
	st  *stream
	n   int64 // plaintext bytes consumed

	plain []byte // one chunk of plaintext
	// carried is true when plain[0] already holds the first byte of the
	// next chunk. Sealing a full chunk needs to know whether it is the last
	// one, which takes reading one byte ahead.
	carried bool

	out    []byte // ciphertext not yet returned (first the header)
	sealed []byte // backing array for out, reused for each chunk
	done   bool   // the final chunk has been sealed
	err    error
}

func (e *encryptReader) Read(p []byte) (int, error) {
	for len(e.out) == 0 {
		if e.err != nil {
			return 0, e.err
		}
		if e.done {
			return 0, io.EOF
		}
		if e.err = e.seal(); e.err != nil {
			return 0, e.err
		}
	}
	n := copy(p, e.out)
	e.out = e.out[n:]
	return n, nil
}

// seal reads the next chunk of plaintext and encrypts it into e.out.
func (e *encryptReader) seal() error {
	have := 0
	if e.carried {
		have, e.carried = 1, false
	}
	n, final, err := readFull(e.src, e.plain[have:])
	if err != nil {
		return err
	}
	have += n
	var ahead [1]byte
	if !final {
		// The chunk is full. It is the final one only if nothing follows.
		n, final, err = readFull(e.src, ahead[:])
		if err != nil {
			return err
		}
		final = final && n == 0
	}
	nonce, err := e.st.next(final)
	if err != nil {
		return err
	}
	e.out = e.st.aead.Seal(e.sealed[:0], nonce, e.plain[:have], e.st.aad)
	e.n += int64(have)
	e.done = final
	if !final {
		e.plain[0], e.carried = ahead[0], true
	}
	return nil
}

func (e *encrypted) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	// Fail on an unusable key before touching the store.
	if _, err := e.key.m().bytes("data key"); err != nil {
		return nil, err
	}
	rc, err := e.inner.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	return &decryptReader{src: rc, closer: rc, key: e.key, objectKey: key}, nil
}

// decryptReader verifies and decrypts a ciphertext stream chunk by chunk.
type decryptReader struct {
	src       io.Reader
	closer    io.Closer
	key       DataKey
	objectKey string

	st *stream // nil until the header has been read
	// buf holds one sealed chunk plus one byte read ahead, which is how the
	// reader learns whether a full-size chunk is the last one.
	buf     []byte
	carried bool   // buf[0] already holds the first byte of the next chunk
	plain   []byte // verified plaintext not yet returned; aliases buf
	done    bool   // the final chunk has been verified
	err     error
}

func (d *decryptReader) Close() error {
	clear(d.buf)
	d.plain = nil
	if d.err == nil {
		d.err = errors.New("staging: read from a closed object")
	}
	return d.closer.Close()
}

func (d *decryptReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, d.err
	}
	for len(d.plain) == 0 {
		if d.err != nil {
			return 0, d.err
		}
		if d.done {
			d.err = io.EOF
			return 0, io.EOF
		}
		if d.err = d.open(); d.err != nil {
			return 0, d.err
		}
	}
	n := copy(p, d.plain)
	d.plain = d.plain[n:]
	return n, nil
}

// open reads, verifies and decrypts the next chunk into d.plain. Nothing is
// exposed unless the chunk authenticates.
func (d *decryptReader) open() error {
	if d.st == nil {
		if err := d.readHeader(); err != nil {
			return err
		}
	}
	have := 0
	if d.carried {
		// Move the byte read ahead last time to the front.
		d.buf[0] = d.buf[len(d.buf)-1]
		have, d.carried = 1, false
	}
	n, _, err := readFull(d.src, d.buf[have:])
	if err != nil {
		return d.readError(err)
	}
	have += n
	// A whole buffer means a full chunk and at least one byte after it, so
	// the chunk is not the last. Anything shorter ended at end of stream.
	final := have < len(d.buf)
	sealed := d.buf[:have]
	if !final {
		sealed = d.buf[:len(d.buf)-1]
		d.carried = true
	}
	index := d.st.counter
	switch {
	case len(sealed) < gcmTagSize:
		return d.corrupt("it is truncated")
	case len(sealed) == gcmTagSize && index > 0:
		// Only an empty object has an empty chunk. Rejecting it elsewhere
		// keeps the encoding of every plaintext unique.
		return d.corrupt("it has an empty trailing chunk")
	}
	nonce, err := d.st.next(final)
	if err != nil {
		return err
	}
	plain, err := d.st.aead.Open(sealed[:0], nonce, sealed, d.st.aad)
	if err != nil {
		return d.corrupt("chunk " + fmt.Sprint(index) + " failed authentication")
	}
	d.plain, d.done = plain, final
	return nil
}

func (d *decryptReader) readHeader() error {
	header := make([]byte, HeaderSize)
	n, _, err := readFull(d.src, header)
	if err != nil {
		return d.readError(err)
	}
	if n < HeaderSize {
		return d.corrupt("it is truncated")
	}
	if string(header[:len(encMagic)]) != encMagic {
		return d.corrupt("it does not start with the " + encMagic + " header")
	}
	chunkSize := binary.BigEndian.Uint32(header[len(encMagic)+saltSize:])
	if chunkSize == 0 || chunkSize > maxChunkSize {
		return d.corrupt("its header is damaged")
	}
	st, err := newStream(d.key, header, d.objectKey)
	if err != nil {
		return err
	}
	d.st = st
	d.buf = make([]byte, int(chunkSize)+gcmTagSize+1)
	return nil
}

// readError reports a failure of the underlying read (as opposed to bad
// data), keeping the cause.
func (d *decryptReader) readError(err error) error {
	return fmt.Errorf("staging: get %s: %w", quoteKey(d.objectKey), err)
}

func (d *decryptReader) corrupt(why string) error {
	return fail.New("staging.corrupt",
		"A staging object failed its integrity check",
		"The object "+quoteKey(d.objectKey)+" cannot be trusted: "+why+
			". It was damaged or altered in the object store, or was written under a different data key.",
		"Do not use the partial output. Re-run the stage that produced this object (`taut migrate` redoes it); if it keeps happening, check the object store for corruption or tampering.")
}

func (e *encrypted) Stat(ctx context.Context, key string) (Info, error) {
	return e.inner.Stat(ctx, key)
}

func (e *encrypted) Delete(ctx context.Context, key string) error {
	return e.inner.Delete(ctx, key)
}

func (e *encrypted) List(ctx context.Context, prefix string) iter.Seq2[Info, error] {
	return e.inner.List(ctx, prefix)
}

func (e *encrypted) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	return e.inner.DeletePrefix(ctx, prefix)
}
