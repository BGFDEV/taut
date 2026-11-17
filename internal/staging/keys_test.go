package staging

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/secret"
)

// kekBytes is a key whose base64 form uses both '+' and '/' (and so '-'
// and '_' in the URL alphabet) and needs padding.
var kekBytes = append([]byte{0xfb, 0xef, 0xbe, 0xff, 0xff, 0xff}, bytes.Repeat([]byte{0x5a, 0xc3}, 13)...)

func TestParseKEKEncodings(t *testing.T) {
	std := base64.StdEncoding.EncodeToString(kekBytes)
	if !strings.ContainsAny(std, "+") || !strings.Contains(std, "/") || !strings.HasSuffix(std, "=") {
		t.Fatalf("setup: %q does not exercise the alphabets", std)
	}
	forms := map[string]string{
		"base64 std padded":   std,
		"base64 std unpadded": base64.RawStdEncoding.EncodeToString(kekBytes),
		"base64 url padded":   base64.URLEncoding.EncodeToString(kekBytes),
		"base64 url unpadded": base64.RawURLEncoding.EncodeToString(kekBytes),
		"hex lower":           hex.EncodeToString(kekBytes),
		"hex upper":           strings.ToUpper(hex.EncodeToString(kekBytes)),
		"trailing newline":    std + "\n",
		"surrounding space":   "  " + hex.EncodeToString(kekBytes) + "\r\n",
	}
	for name, form := range forms {
		k, err := ParseKEK(secret.New(form))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !bytes.Equal(k.m().b[:], kekBytes) {
			t.Errorf("%s: decoded to the wrong key", name)
		}
	}

	gen, err := GenerateKEK()
	if err != nil {
		t.Fatal(err)
	}
	gen2, _ := GenerateKEK()
	if gen == gen2 || len(gen) != 44 {
		t.Fatalf("GenerateKEK gave %d chars, repeated=%v", len(gen), gen == gen2)
	}
	if _, err := ParseKEK(secret.New(gen)); err != nil {
		t.Fatalf("a generated key does not parse: %v", err)
	}
}

func TestParseKEKRejects(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString
	cases := []struct {
		name, in, code, why string
	}{
		{"empty", "", "staging.kek_missing", "empty"},
		{"blank", " \n", "staging.kek_missing", "empty"},
		{"31 bytes", b64(kekBytes[:31]), "staging.kek_invalid", "decodes to 31 bytes"},
		{"33 bytes", b64(append(bytes.Clone(kekBytes), 7)), "staging.kek_invalid", "decodes to 33 bytes"},
		{"16 bytes hex", hex.EncodeToString(kekBytes[:16]), "staging.kek_invalid", "decodes to 24 bytes"},
		{"63 hex chars", hex.EncodeToString(kekBytes)[:63], "staging.kek_invalid", "decodes to 47 bytes"},
		{"64 chars, not hex", "zz" + hex.EncodeToString(kekBytes)[2:], "staging.kek_invalid", "decodes to 48 bytes"},
		{"a passphrase", "correct horse battery staple!", "staging.kek_invalid", "neither base64 nor"},
		{"mixed alphabets", "ab+/-_" + b64(kekBytes)[6:], "staging.kek_invalid", "neither base64 nor"},
		{"inner whitespace", b64(kekBytes)[:20] + " " + b64(kekBytes)[20:], "staging.kek_invalid", "neither base64 nor"},
		{"all zeros", b64(make([]byte, 32)), "staging.kek_invalid", "all zeros"},
		{"all zeros hex", strings.Repeat("0", 64), "staging.kek_invalid", "all zeros"},
	}
	for _, c := range cases {
		k, err := ParseKEK(secret.New(c.in))
		if err == nil {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		wantCode(t, err, c.code)
		if !k.IsZero() {
			t.Errorf("%s: a usable KEK was returned with the error", c.name)
		}
		msg := fmt.Sprintf("%v | %+v | %#v", err, err, err)
		if !strings.Contains(msg, c.why) {
			t.Errorf("%s: error %q does not say %q", c.name, err, c.why)
		}
		// Neither the input nor any 6-character piece of it may be echoed.
		in := strings.TrimSpace(c.in)
		for i := 0; i+6 <= len(in); i++ {
			if piece := in[i : i+6]; strings.Contains(msg, piece) && !strings.Contains(piece, " ") && piece != "000000" {
				t.Errorf("%s: error echoes input fragment %q", c.name, piece)
				break
			}
		}
	}
}

func TestWrapUnwrap(t *testing.T) {
	kek, err := ParseKEK(secret.New(hex.EncodeToString(kekBytes)))
	if err != nil {
		t.Fatal(err)
	}
	dk, err := NewDataKey()
	if err != nil {
		t.Fatal(err)
	}
	dk2, _ := NewDataKey()
	if dk.m().b == dk2.m().b || dk.m().b == [KeySize]byte{} {
		t.Fatal("NewDataKey is not random")
	}

	wrapped, err := kek.Wrap(dk)
	if err != nil {
		t.Fatal(err)
	}
	if len(wrapped) != 71 || !bytes.HasPrefix(wrapped, []byte("taut-kek-v1")) {
		t.Fatalf("wrapped key is %d bytes: %q…", len(wrapped), wrapped[:11])
	}
	if bytes.Contains(wrapped, dk.m().b[:8]) {
		t.Fatal("the data key is visible in its wrapped form")
	}
	again, _ := kek.Wrap(dk)
	if bytes.Equal(wrapped, again) {
		t.Fatal("two wraps of one key are identical")
	}

	got, err := kek.Unwrap(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if got.m().b != dk.m().b {
		t.Fatal("Unwrap returned a different key")
	}
	// End to end: data written under the original key reads under the
	// unwrapped one.
	mem := newMem()
	mustPut(t, Encrypted(mem, dk), "k", []byte("payload"))
	if out := mustGet(t, Encrypted(mem, got), "k"); string(out) != "payload" {
		t.Fatalf("got %q", out)
	}

	// The wrong KEK.
	other := bytes.Clone(kekBytes)
	other[31] ^= 1
	wrong, err := ParseKEK(secret.New(hex.EncodeToString(other)))
	if err != nil {
		t.Fatal(err)
	}
	bad, err := wrong.Unwrap(wrapped)
	wantCode(t, err, "staging.key_mismatch")
	if !bad.IsZero() {
		t.Fatal("a key was returned alongside key_mismatch")
	}
	if next := fail.From(err).Next; !strings.Contains(next, "differs from the one the run was started with") {
		t.Fatalf("unhelpful next step: %s", next)
	}

	// Any single-byte change, any truncation, any extension.
	for i := range wrapped {
		c := bytes.Clone(wrapped)
		c[i] ^= 0x04
		_, err := kek.Unwrap(c)
		wantCode(t, err, "staging.key_mismatch")
	}
	for n := 0; n < len(wrapped); n++ {
		_, err := kek.Unwrap(wrapped[:n])
		wantCode(t, err, "staging.key_mismatch")
	}
	_, err = kek.Unwrap(append(bytes.Clone(wrapped), 0))
	wantCode(t, err, "staging.key_mismatch")
	_, err = kek.Unwrap(nil)
	wantCode(t, err, "staging.key_mismatch")

	// Unusable keys.
	_, err = KEK{}.Wrap(dk)
	wantCode(t, err, "staging.key_destroyed")
	_, err = KEK{}.Unwrap(wrapped)
	wantCode(t, err, "staging.key_destroyed")
	_, err = kek.Wrap(DataKey{})
	wantCode(t, err, "staging.key_destroyed")
}

func TestDestroy(t *testing.T) {
	dk := testKey(1)
	copyOfKey := dk
	dk.Destroy()
	if copyOfKey.m().b != [KeySize]byte{} || !copyOfKey.IsZero() || !dk.IsZero() {
		t.Fatal("Destroy left key bytes behind, or the key still claims to be usable")
	}
	dk.Destroy() // idempotent
	DataKey{}.Destroy()
	KEK{}.Destroy()

	kek, err := ParseKEK(secret.New(hex.EncodeToString(kekBytes)))
	if err != nil {
		t.Fatal(err)
	}
	kek.Destroy()
	if kek.m().b != [KeySize]byte{} {
		t.Fatal("Destroy left KEK bytes behind")
	}
	_, err = kek.Wrap(testKey(2))
	wantCode(t, err, "staging.key_destroyed")
}

func TestKeysNeverPrint(t *testing.T) {
	dk := testKey(0x41) // bytes "ABCDEFGH…": visible in any string dump
	kek, err := ParseKEK(secret.New(hex.EncodeToString(kekBytes)))
	if err != nil {
		t.Fatal(err)
	}
	type holder struct {
		Name string
		DK   DataKey
		KEK  KEK
		PDK  *DataKey
		priv DataKey
		Any  any
	}
	h := holder{Name: "run", DK: dk, KEK: kek, PDK: &dk, priv: dk, Any: kek}

	leaks := func(out string) string {
		for _, raw := range [][]byte{dk.m().b[:], kekBytes} {
			needles := []string{
				string(raw[:8]),
				hex.EncodeToString(raw[:6]),
				strings.ToUpper(hex.EncodeToString(raw[:6])),
				base64.StdEncoding.EncodeToString(raw[:6]),
				base64.URLEncoding.EncodeToString(raw[:6]),
				strings.Trim(fmt.Sprint(raw[:4]), "[]"),                      // "65 66 67 68"
				strings.ReplaceAll(fmt.Sprintf("%#v", raw[:4])[7:], "}", ""), // "0x41, 0x42, …"
			}
			for _, n := range needles {
				if strings.Contains(out, n) {
					return n
				}
			}
		}
		return ""
	}

	verbs := []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "% x", "%d", "%b", "%o", "%c", "%U", "%e", "%p", "%T", "%10v", "%-10s", "%t"}
	values := map[string]any{"DataKey": dk, "*DataKey": &dk, "KEK": kek, "*KEK": &kek, "holder": h, "*holder": &h,
		"slice": []DataKey{dk}, "map": map[string]KEK{"k": kek}}
	for name, v := range values {
		for _, verb := range verbs {
			out := fmt.Sprintf(verb, v)
			if n := leaks(out); n != "" {
				t.Errorf("fmt %s of %s leaks key material (%q): %s", verb, name, n, out)
			}
		}
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x", "%d", "%q"} {
		for _, v := range []any{dk, kek, &dk, &kek} {
			if out := fmt.Sprintf(verb, v); out != secret.Marker {
				t.Errorf("fmt %s of %T = %q, want the marker", verb, v, out)
			}
		}
	}
	if out := fmt.Sprint(dk) + fmt.Sprintln(kek) + dk.String() + kek.GoString(); strings.Count(out, secret.Marker) != 4 {
		t.Errorf("Sprint/String: %q", out)
	}

	js, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if n := leaks(string(js)); n != "" || strings.Count(string(js), secret.Marker) != 4 {
		t.Errorf("JSON leaks or fails to redact (%q): %s", n, js)
	}
	for _, v := range []any{dk, kek} {
		js, _ := json.Marshal(v)
		if string(js) != `"`+secret.Marker+`"` {
			t.Errorf("JSON of %T = %s", v, js)
		}
		txt, _ := v.(interface{ MarshalText() ([]byte, error) }).MarshalText()
		if string(txt) != secret.Marker {
			t.Errorf("MarshalText of %T = %s", v, txt)
		}
	}

	for name, mk := range map[string]func(*bytes.Buffer) slog.Handler{
		"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
		"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
	} {
		var buf bytes.Buffer
		log := slog.New(mk(&buf)).With("with", dk)
		log.Info("keys", "dk", dk, "kek", kek, "pdk", &dk, "holder", h, slog.Group("g", slog.Any("kek", kek)),
			"err", fmt.Errorf("wrapping %v with %v", dk, kek))
		out := buf.String()
		if n := leaks(out); n != "" {
			t.Errorf("slog %s leaks key material (%q): %s", name, n, out)
		}
		if strings.Count(out, secret.Marker) < 6 {
			t.Errorf("slog %s did not redact: %s", name, out)
		}
	}

	// Errors from the package never carry key material either.
	_, werr := KEK{}.Wrap(dk)
	stored := seal(t, dk, "k", []byte("secret plaintext"))
	_, derr := decrypt(testKey(9), "k", stored)
	for _, err := range []error{werr, derr} {
		msg := fmt.Sprintf("%v %+v %#v", err, err, err)
		if n := leaks(msg); n != "" || strings.Contains(msg, "secret plaintext") {
			t.Errorf("error leaks: %s", msg)
		}
	}
}
