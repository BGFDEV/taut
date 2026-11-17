package secret

import (
	"context"
	"fmt"
	"log/slog"
)

// NewRedactingHandler wraps inner so that Redact is applied to every record's
// message and to all string attribute values, recursively through groups.
// Errors are logged as their redacted message; other values that format
// themselves (fmt.Stringer) are replaced by their redacted text only when
// redaction changes it, so ordinary values keep their native encoding.
func NewRedactingHandler(inner slog.Handler) slog.Handler {
	if h, ok := inner.(*redactingHandler); ok {
		return h
	}
	return &redactingHandler{inner: inner}
}

type redactingHandler struct {
	inner slog.Handler
}

func (h *redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, Redact(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, out)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	clean := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		clean[i] = redactAttr(a)
	}
	return &redactingHandler{inner: h.inner.WithAttrs(clean)}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &redactingHandler{inner: h.inner.WithGroup(name)}
}

func redactAttr(a slog.Attr) slog.Attr {
	a.Value = redactValue(a.Value)
	return a
}

func redactValue(v slog.Value) slog.Value {
	v = v.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.StringValue(Redact(v.String()))
	case slog.KindGroup:
		attrs := v.Group()
		clean := make([]slog.Attr, len(attrs))
		for i, a := range attrs {
			clean[i] = redactAttr(a)
		}
		return slog.GroupValue(clean...)
	case slog.KindAny:
		return redactAny(v)
	default:
		return v
	}
}

func redactAny(v slog.Value) slog.Value {
	switch x := v.Any().(type) {
	case error:
		if s, ok := safeString(x.Error); ok {
			return slog.StringValue(Redact(s))
		}
	case []string:
		clean := make([]string, len(x))
		for i, s := range x {
			clean[i] = Redact(s)
		}
		return slog.AnyValue(clean)
	case []byte:
		if s := string(x); Redact(s) != s {
			return slog.StringValue(Redact(s))
		}
	case fmt.Stringer:
		if s, ok := safeString(x.String); ok {
			if r := Redact(s); r != s {
				return slog.StringValue(r)
			}
		}
	}
	return v
}

// safeString calls f, tolerating the panic a nil receiver would cause.
func safeString(f func() string) (s string, ok bool) {
	defer func() {
		if recover() != nil {
			s, ok = "", false
		}
	}()
	return f(), true
}
