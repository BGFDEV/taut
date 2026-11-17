package staging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bgfdev/taut/internal/fail"
)

// Info describes a stored object.
type Info struct {
	Key string
	// Size is the stored size in bytes. Through an Encrypted view this is the
	// ciphertext size; see PlaintextSize.
	Size int64
}

// Blobs is a minimal object store. Implementations are safe for concurrent
// use.
type Blobs interface {
	// Put stores the reader's content under key, replacing any existing
	// object, and returns the number of bytes it read from r. It is atomic
	// from a reader's point of view: a concurrent or later Get sees either
	// the old object, the complete new one, or ErrNotExist, never a partial
	// write. If r fails or ctx is cancelled nothing becomes visible.
	Put(ctx context.Context, key string, r io.Reader) (int64, error)
	// Get opens the object for reading. The error wraps ErrNotExist if there
	// is no such object. The caller must Close the reader.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Stat describes the object. The error wraps ErrNotExist if absent.
	Stat(ctx context.Context, key string) (Info, error)
	// Delete removes the object. Deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error
	// List yields every object whose key starts with prefix, in
	// lexicographic (byte) order of key. An empty prefix lists everything.
	// Iteration stops after the first error.
	List(ctx context.Context, prefix string) iter.Seq2[Info, error]
	// DeletePrefix removes every object whose key starts with prefix, along
	// with any leftovers of interrupted uploads under it, and returns the
	// number of objects removed.
	DeletePrefix(ctx context.Context, prefix string) (int, error)
}

// ErrNotExist reports that the requested object does not exist.
var ErrNotExist = errors.New("staging: object does not exist")

const (
	// MaxKeyLength is the longest key accepted, in bytes.
	MaxKeyLength = 1024
	// maxSegmentLength keeps every path segment a legal file name.
	maxSegmentLength = 255
	// tmpPrefix marks the filesystem backend's in-progress files. Keys may
	// not contain a segment that starts with it.
	tmpPrefix = ".taut-tmp-"
)

// ValidateKey reports whether key is acceptable to every backend. Keys are
// slash-separated relative paths: non-empty, valid UTF-8, at most
// MaxKeyLength bytes, with no leading or trailing slash, no empty, "." or
// ".." segments, no backslashes and no control characters. Segments starting
// with ".taut-tmp-" are reserved.
func ValidateKey(key string) error {
	if why := keyProblem(key); why != "" {
		return invalidKey(key, why)
	}
	return nil
}

func keyProblem(key string) string {
	switch {
	case key == "":
		return "it is empty"
	case len(key) > MaxKeyLength:
		return "it is longer than " + strconv.Itoa(MaxKeyLength) + " bytes"
	case !utf8.ValidString(key):
		return "it is not valid UTF-8"
	case strings.ContainsRune(key, '\\'):
		return "it contains a backslash"
	case strings.HasPrefix(key, "/"):
		return "it starts with a slash"
	}
	for _, r := range key {
		if unicode.IsControl(r) {
			return "it contains a control character"
		}
	}
	for seg := range strings.SplitSeq(key, "/") {
		switch {
		case seg == "":
			return "it has an empty path segment"
		case seg == "." || seg == "..":
			return "it has a relative path segment"
		case len(seg) > maxSegmentLength:
			return "a path segment is longer than " + strconv.Itoa(maxSegmentLength) + " bytes"
		case strings.HasPrefix(seg, tmpPrefix):
			return "a path segment uses the reserved prefix " + tmpPrefix
		}
	}
	return ""
}

// validatePrefix checks a List/DeletePrefix prefix: empty, or a valid key
// optionally followed by one slash.
func validatePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if why := keyProblem(strings.TrimSuffix(prefix, "/")); why != "" {
		return invalidKey(prefix, why)
	}
	return nil
}

func invalidKey(key, why string) error {
	return fail.New("staging.invalid_key",
		"A staging object key is not valid",
		"The key "+quoteKey(key)+" was rejected because "+why+".",
		"This is a bug in Taut, not in your configuration. Please report it with the command you ran.")
}

// quoteKey renders a key safely for an error message: escaped and bounded.
func quoteKey(key string) string {
	const limit = 80
	if len(key) > limit {
		key = strings.ToValidUTF8(key[:limit], "") + "…"
	}
	return strconv.QuoteToASCII(key)
}

func notExist(key string) error {
	return fmt.Errorf("staging: %s: %w", quoteKey(key), ErrNotExist)
}

// sourceReader wraps the content reader handed to Put. It stops at context
// cancellation and remembers a failure of the caller's reader, so that it is
// not reported as a storage failure.
type sourceReader struct {
	ctx context.Context
	r   io.Reader
	err error
}

func (s *sourceReader) Read(p []byte) (int, error) {
	if err := s.ctx.Err(); err != nil {
		s.err = err
		return 0, err
	}
	n, err := s.r.Read(p)
	if err != nil && err != io.EOF {
		s.err = err
	}
	return n, err
}

// failed wraps the error that stopped a Put while reading its content.
func (s *sourceReader) failed(key string, err error) error {
	return fmt.Errorf("staging: put %s: reading content: %w", quoteKey(key), err)
}

// readFull reads from r until buf is full or r is exhausted. Unlike
// io.ReadFull it never confuses the end of the content with an error: eof is
// set only when r itself returned io.EOF, and any other error (including an
// io.ErrUnexpectedEOF from a truncated upstream) is returned as err.
func readFull(r io.Reader, buf []byte) (n int, eof bool, err error) {
	for idle := 0; n < len(buf); {
		m, rerr := r.Read(buf[n:])
		n += m
		switch {
		case rerr == io.EOF:
			return n, true, nil
		case rerr != nil:
			return n, false, rerr
		case m > 0:
			idle = 0
		default:
			if idle++; idle >= 100 {
				return n, false, io.ErrNoProgress
			}
		}
	}
	return n, false, nil
}
