package secret

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"testing"
)

func TestRedactingHandler(t *testing.T) {
	handlers := map[string]func(*bytes.Buffer) slog.Handler{
		"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
		"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
	}
	fileURL, err := url.Parse("https://files.slack.com/files-pri/T1-F2/a.png?t=" + fakeSlackBot)
	if err != nil {
		t.Fatal(err)
	}
	for name, mk := range handlers {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			log := slog.New(NewRedactingHandler(mk(&buf)))

			log = log.With("with_attr", "Bearer "+fakeJWT).WithGroup("scope").With("scoped", fakeEntra)
			log.Info("calling slack with "+fakeSlackBot,
				"string", "Authorization: Bearer "+fakeSlackBot,
				"err", fmt.Errorf("GET %s: %w", fileURL, errors.New("403")),
				"url", fileURL,
				"list", []string{"ok", fakeSlackApp},
				"bytes", []byte(fakePEM),
				slog.Group("outer", slog.Group("inner", slog.String("deep", "client_secret="+fakeEntra))),
				"count", 42,
				"plain", "channel general",
				"wrapped", New("wrapped-plaintext-secret"),
			)

			out := buf.String()
			for _, s := range []string{fakeSlackBot, fakeSlackApp, fakeJWT, fakeEntra, "MIIEvQIBADANBgkq", "wrapped-plaintext-secret"} {
				if strings.Contains(out, s) {
					t.Errorf("secret leaked through the handler: %s", out)
				}
			}
			for _, s := range []string{"calling slack with " + Marker, "channel general", "42", "403", "files.slack.com", "scope"} {
				if !strings.Contains(out, s) {
					t.Errorf("output lost %q: %s", s, out)
				}
			}
		})
	}
}

func TestRedactingHandlerPassesThrough(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	h := NewRedactingHandler(inner)

	if NewRedactingHandler(h) != h {
		t.Error("wrapping twice should be a no-op")
	}
	if h.Enabled(t.Context(), slog.LevelInfo) || !h.Enabled(t.Context(), slog.LevelError) {
		t.Error("Enabled does not defer to the inner handler")
	}

	var nilURL *url.URL
	slog.New(h).Warn("plain message", "n", 7, "nil_stringer", nilURL, "nil", nil)
	if out := buf.String(); !strings.Contains(out, "plain message") || !strings.Contains(out, "n=7") {
		t.Errorf("ordinary record was altered: %s", out)
	}
}
