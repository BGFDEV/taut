package wire

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/secret"
)

func TestAuthRoundTrip(t *testing.T) {
	cases := []Auth{
		NoAuth(),
		SlackAuth(secret.Ref{Env: "TAUT_SLACK_TOKEN"}),
		EntraAuth(Entra{TenantID: "t", ClientID: "c", Cloud: "global",
			Credential: Credential{Certificate: secret.Ref{File: "/run/secrets/taut.pem"}}}),
		EntraAuth(Entra{TenantID: "t", ClientID: "c", Authority: "http://127.0.0.1:1", GraphBase: "http://127.0.0.1:2",
			Credential: Credential{ClientSecret: secret.Ref{Env: "TAUT_ENTRA_SECRET"}}}),
	}
	for _, a := range cases {
		enc := EncodeAuth(a)
		if strings.ContainsAny(enc, "=+/") {
			t.Errorf("encoding %q is not unpadded base64url", enc)
		}
		got, err := DecodeAuth(enc)
		if err != nil {
			t.Fatalf("DecodeAuth(%+v): %v", a, err)
		}
		if got != a {
			t.Errorf("round trip: got %+v, want %+v", got, a)
		}
	}
}

func TestAuthJSONShape(t *testing.T) {
	b, _ := json.Marshal(EntraAuth(Entra{TenantID: "t", ClientID: "c",
		Credential: Credential{Certificate: secret.Ref{File: "/run/secrets/x.pem"}}}))
	want := `{"kind":"entra","tenantId":"t","clientId":"c","credential":{"certificate":{"file":"/run/secrets/x.pem"}}}`
	if string(b) != want {
		t.Errorf("got  %s\nwant %s", b, want)
	}
	b, _ = json.Marshal(SlackAuth(secret.Ref{Env: "TAUT_SLACK_TOKEN"}))
	if want := `{"kind":"slack","token":{"env":"TAUT_SLACK_TOKEN"}}`; string(b) != want {
		t.Errorf("got  %s\nwant %s", b, want)
	}
	b, _ = json.Marshal(NoAuth())
	if want := `{"kind":"none"}`; string(b) != want {
		t.Errorf("got  %s\nwant %s", b, want)
	}
}

func TestDecodeAuthRejects(t *testing.T) {
	enc := rawEnc
	bad := map[string]string{
		"empty":           "",
		"not base64":      "!!!",
		"padded":          "e30=",
		"not json":        rawEnc("nope"),
		"no kind":         enc(`{}`),
		"unknown kind":    enc(`{"kind":"basic"}`),
		"entra no cred":   enc(`{"kind":"entra","tenantId":"t","clientId":"c"}`),
		"entra both":      enc(`{"kind":"entra","tenantId":"t","clientId":"c","credential":{"certificate":{"file":"/a"},"clientSecret":{"env":"B"}}}`),
		"entra no tenant": enc(`{"kind":"entra","clientId":"c","credential":{"clientSecret":{"env":"B"}}}`),
		"slack no token":  enc(`{"kind":"slack"}`),
		"slack + entra":   enc(`{"kind":"slack","tenantId":"t","token":{"env":"B"}}`),
		"none + token":    enc(`{"kind":"none","token":{"env":"B"}}`),
		"too long":        strings.Repeat("A", maxAuthHeader+1),
	}
	for name, v := range bad {
		if _, err := DecodeAuth(v); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func rawEnc(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func TestHeaders(t *testing.T) {
	h := Headers("https://graph.microsoft.com", NoAuth())
	if h.Get(HeaderUpstream) != "https://graph.microsoft.com" {
		t.Errorf("upstream header = %q", h.Get(HeaderUpstream))
	}
	if a, err := DecodeAuth(h.Get(HeaderAuth)); err != nil || a.Kind != KindNone {
		t.Errorf("auth header decodes to %+v, %v", a, err)
	}
}

func TestWriteErrorRedacts(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, 418, fail.New("x.y", "Bad", "saw xoxb-123456789012-abcdefghij", "Do it"))
	if strings.Contains(rec.Body.String(), "xoxb-1234") {
		t.Errorf("secret in body: %s", rec.Body)
	}
	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != "x.y" || body.Error.Next != "Do it" {
		t.Errorf("body %s: %+v, %v", rec.Body, body, err)
	}
	rec = httptest.NewRecorder()
	WriteError(rec, 500, errors.New("plain"))
	if !strings.Contains(rec.Body.String(), `"code":"unexpected"`) {
		t.Errorf("plain error body: %s", rec.Body)
	}
}
