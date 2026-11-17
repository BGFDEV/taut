package staging

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/secret"
)

// KeySize is the size in bytes of both the KEK and the data key (AES-256).
const KeySize = 32

const (
	// wrapMagic is the versioned header of a wrapped data key, and also the
	// AAD of its encryption.
	wrapMagic     = "taut-kek-v1"
	gcmNonceSize  = 12
	gcmTagSize    = 16
	wrappedKeyLen = len(wrapMagic) + gcmNonceSize + KeySize + gcmTagSize
)

// keyMaterial holds 32 secret bytes. Keys refer to it through a pointer, so
// that copies of a key share one buffer and Destroy reaches all of them.
type keyMaterial struct {
	b         [KeySize]byte
	destroyed bool
}

// keyRef is how KEK and DataKey hold their material: behind TWO pointers.
// The formatting methods below cover every case in which fmt, encoding/json
// or slog can call a method, but fmt cannot call methods on a key held in
// someone else's unexported struct field; there it prints the raw field. fmt
// follows one pointer to a struct or array when it reports a bad verb, but
// never two, so a double pointer always prints as an address.
type keyRef struct{ p **keyMaterial }

func newKeyRef() keyRef {
	m := new(keyMaterial)
	return keyRef{p: &m}
}

// m returns the key material, or nil for a zero key.
func (r keyRef) m() *keyMaterial {
	if r.p == nil {
		return nil
	}
	return *r.p
}

func (m *keyMaterial) bytes(what string) ([]byte, error) {
	if m == nil || m.destroyed {
		return nil, fail.New("staging.key_destroyed",
			"A staging encryption key is not available",
			"The "+what+" was never set or has already been destroyed.",
			"This is a bug in Taut. Please report it with the command you ran.")
	}
	return m.b[:], nil
}

func (m *keyMaterial) destroy() {
	if m != nil {
		clear(m.b[:])
		m.destroyed = true
	}
}

// KEK is the operator's key-encryption key. It only ever wraps and unwraps
// data keys. The zero KEK is unusable.
//
// A KEK prints as "[redacted]" under every fmt verb, in JSON, as text and in
// slog, and has no method that returns its bytes.
type KEK struct{ keyRef }

// DataKey is a run's data-encryption key. The zero DataKey is unusable.
//
// A DataKey prints as "[redacted]" everywhere and has no method that returns
// its bytes: the only way to persist one is KEK.Wrap.
type DataKey struct{ keyRef }

// Destroy overwrites the key with zeros; the KEK and all its copies are
// unusable afterwards. It must not be called while the key is in use.
//
// This is best effort. Go gives no control over copies the runtime or the
// standard library made: the AES key schedule inside a cipher that is still
// referenced, bytes the garbage collector moved or has not yet reclaimed,
// the operator's original string that ParseKEK decoded, or pages the
// operating system swapped out. It shortens the key's lifetime in memory; it
// does not guarantee erasure.
func (k KEK) Destroy() { k.m().destroy() }

// Destroy overwrites the key with zeros; the DataKey and all its copies are
// unusable afterwards. It must not be called while the key is in use, for
// instance while a Put or a Get through Encrypted is running. The limits
// described on KEK.Destroy apply.
func (k DataKey) Destroy() { k.m().destroy() }

// IsZero reports whether the KEK is unset or destroyed.
func (k KEK) IsZero() bool { m := k.m(); return m == nil || m.destroyed }

// IsZero reports whether the DataKey is unset or destroyed.
func (k DataKey) IsZero() bool { m := k.m(); return m == nil || m.destroyed }

func (KEK) String() string                 { return secret.Marker }
func (KEK) GoString() string               { return secret.Marker }
func (KEK) Format(f fmt.State, _ rune)     { _, _ = io.WriteString(f, secret.Marker) }
func (KEK) MarshalJSON() ([]byte, error)   { return []byte(`"` + secret.Marker + `"`), nil }
func (KEK) MarshalText() ([]byte, error)   { return []byte(secret.Marker), nil }
func (KEK) LogValue() slog.Value           { return slog.StringValue(secret.Marker) }
func (DataKey) String() string             { return secret.Marker }
func (DataKey) GoString() string           { return secret.Marker }
func (DataKey) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, secret.Marker) }
func (DataKey) MarshalJSON() ([]byte, error) {
	return []byte(`"` + secret.Marker + `"`), nil
}
func (DataKey) MarshalText() ([]byte, error) { return []byte(secret.Marker), nil }
func (DataKey) LogValue() slog.Value         { return slog.StringValue(secret.Marker) }

var (
	_ fmt.Formatter  = KEK{}
	_ fmt.GoStringer = KEK{}
	_ slog.LogValuer = KEK{}
	_ fmt.Formatter  = DataKey{}
	_ fmt.GoStringer = DataKey{}
	_ slog.LogValuer = DataKey{}
)

// GenerateKEK returns a fresh random key-encryption key in the standard
// base64 form that ParseKEK accepts, for a helper command to print once.
func GenerateKEK() (string, error) {
	var b [KeySize]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	defer clear(b[:])
	return base64.StdEncoding.EncodeToString(b[:]), nil
}

// ParseKEK decodes the operator-supplied key-encryption key: 32 random
// bytes written either as 64 hexadecimal characters or as base64 (standard
// or URL-safe alphabet, with or without padding). Surrounding whitespace,
// such as the newline at the end of a key file, is ignored.
//
// Errors describe what is wrong without ever including the input.
func ParseKEK(v secret.Value) (KEK, error) {
	const next = "Generate a key with `taut dev keygen` and supply it unchanged through the env var or file the config file names."
	bad := func(code, why string) (KEK, error) {
		return KEK{}, fail.New(code, "The staging key-encryption key cannot be used", why, next)
	}
	s := strings.TrimSpace(v.Reveal())
	if s == "" {
		return bad("staging.kek_missing", "It is empty.")
	}

	var raw []byte
	if len(s) == 2*KeySize && isHex(s) {
		raw, _ = hex.DecodeString(s)
	} else {
		enc := base64.RawURLEncoding
		if strings.ContainsAny(s, "+/") {
			enc = base64.RawStdEncoding
		}
		var err error
		raw, err = enc.DecodeString(strings.TrimRight(s, "="))
		if err != nil {
			// The decoder's own message is dropped: it describes the input.
			return bad("staging.kek_invalid",
				"It is neither base64 nor 64 hexadecimal characters.")
		}
	}
	defer clear(raw)
	if len(raw) != KeySize {
		return bad("staging.kek_invalid",
			"It decodes to "+strconv.Itoa(len(raw))+" bytes, but the key must be exactly "+strconv.Itoa(KeySize)+" bytes.")
	}
	if bytes.Equal(raw, make([]byte, KeySize)) {
		return bad("staging.kek_invalid", "It is all zeros, which is a placeholder, not a key.")
	}
	k := KEK{newKeyRef()}
	copy(k.m().b[:], raw)
	return k, nil
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return false
		}
	}
	return true
}

// NewDataKey returns a fresh random data key for one run.
func NewDataKey() (DataKey, error) {
	k := DataKey{newKeyRef()}
	if _, err := rand.Read(k.m().b[:]); err != nil {
		return DataKey{}, err
	}
	return k, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Wrap encrypts the data key under the KEK with AES-256-GCM and a random
// nonce. The result is self-describing (see the package documentation), is
// not secret, and is what a run stores to find its data key again.
func (k KEK) Wrap(dk DataKey) ([]byte, error) {
	kek, err := k.m().bytes("key-encryption key")
	if err != nil {
		return nil, err
	}
	plain, err := dk.m().bytes("data key")
	if err != nil {
		return nil, err
	}
	aead, err := newGCM(kek)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(wrapMagic)+gcmNonceSize, wrappedKeyLen)
	copy(out, wrapMagic)
	nonce := out[len(wrapMagic):]
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(out, nonce, plain, []byte(wrapMagic)), nil
}

// Unwrap recovers a data key wrapped by Wrap. Any failure (the wrong KEK,
// or a wrapped key that was truncated or altered) is reported as
// staging.key_mismatch; AES-GCM cannot tell those cases apart.
func (k KEK) Unwrap(wrapped []byte) (DataKey, error) {
	kek, err := k.m().bytes("key-encryption key")
	if err != nil {
		return DataKey{}, err
	}
	mismatch := func(why string) (DataKey, error) {
		return DataKey{}, fail.New("staging.key_mismatch",
			"The run's staging data cannot be decrypted with this key-encryption key", why,
			"The KEK differs from the one the run was started with. Supply the original key (the env var or file the config file names), or start a new run; data staged under the old key cannot be recovered without it.")
	}
	if len(wrapped) != wrappedKeyLen || string(wrapped[:len(wrapMagic)]) != wrapMagic {
		return mismatch("The stored data key is not in the " + wrapMagic + " format; it is damaged or was written by another version of Taut.")
	}
	aead, err := newGCM(kek)
	if err != nil {
		return DataKey{}, err
	}
	nonce := wrapped[len(wrapMagic) : len(wrapMagic)+gcmNonceSize]
	dk := DataKey{newKeyRef()}
	if _, err := aead.Open(dk.m().b[:0], nonce, wrapped[len(wrapMagic)+gcmNonceSize:], []byte(wrapMagic)); err != nil {
		return mismatch("The stored data key did not authenticate under the key supplied: either the key is a different one, or the stored data key was altered.")
	}
	return dk, nil
}
