// Package secret keeps credentials out of places they must never appear.
//
// It provides three things:
//
//   - Value, a wrapper whose every formatting and marshalling path yields a
//     fixed redaction marker, so a secret cannot be printed or serialized by
//     accident;
//   - Ref, a reference to where a secret lives (an environment variable or a
//     file), which is the only form in which the config file may mention one;
//   - Redact, LooksLikeSecret and NewRedactingHandler, which scrub or detect
//     secret-looking material in arbitrary text and log records.
package secret

import (
	"crypto/subtle"
	"fmt"
	"io"
	"log/slog"
)

// Marker is the fixed text that replaces secret material everywhere.
const Marker = "[redacted]"

// Value holds a secret string. The zero Value is empty.
//
// Reveal is the only way to obtain the plaintext. String, GoString, Format,
// MarshalJSON, MarshalText, MarshalYAML and LogValue all yield Marker, and the
// plaintext sits behind a pointer so that reflection-based printers that
// bypass those methods (for example fmt on an unexported struct field) show
// an address rather than the secret.
type Value struct {
	p *string
}

// New wraps s as a secret Value.
func New(s string) Value {
	if s == "" {
		return Value{}
	}
	return Value{p: &s}
}

// Reveal returns the plaintext secret. Call it only at the point of use and
// never store or log the result.
func (v Value) Reveal() string {
	if v.p == nil {
		return ""
	}
	return *v.p
}

// IsZero reports whether the Value holds no secret.
func (v Value) IsZero() bool { return v.p == nil || *v.p == "" }

// Equal reports whether both values hold the same secret, in constant time
// with respect to their contents.
func (v Value) Equal(other Value) bool {
	return subtle.ConstantTimeCompare([]byte(v.Reveal()), []byte(other.Reveal())) == 1
}

// String returns Marker.
func (v Value) String() string { return Marker }

// GoString returns Marker, so %#v does not leak the secret.
func (v Value) GoString() string { return Marker }

// Format writes Marker for every verb and flag combination.
func (v Value) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, Marker) }

// MarshalJSON encodes Marker as a JSON string.
func (v Value) MarshalJSON() ([]byte, error) { return []byte(`"` + Marker + `"`), nil }

// MarshalText returns Marker.
func (v Value) MarshalText() ([]byte, error) { return []byte(Marker), nil }

// MarshalYAML returns Marker.
func (v Value) MarshalYAML() (any, error) { return Marker, nil }

// LogValue returns Marker, so slog never records the secret.
func (v Value) LogValue() slog.Value { return slog.StringValue(Marker) }

var (
	_ fmt.Formatter  = Value{}
	_ fmt.GoStringer = Value{}
	_ slog.LogValuer = Value{}
)
