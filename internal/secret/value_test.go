package secret

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const plaintext = "s3cr3t-plaintext-value"

func TestValueRevealAndZero(t *testing.T) {
	v := New(plaintext)
	if got := v.Reveal(); got != plaintext {
		t.Fatalf("Reveal() = %q, want %q", got, plaintext)
	}
	if v.IsZero() {
		t.Error("IsZero() = true for a non-empty value")
	}
	var zero Value
	if !zero.IsZero() || !New("").IsZero() {
		t.Error("zero and empty values must report IsZero")
	}
	if zero.Reveal() != "" {
		t.Error("zero value must reveal the empty string")
	}
	if !v.Equal(New(plaintext)) || v.Equal(New("other")) || !zero.Equal(New("")) {
		t.Error("Equal does not compare contents")
	}
}

func TestValueNeverLeaksThroughFmt(t *testing.T) {
	v := New(plaintext)
	type exported struct{ Token Value }
	type unexported struct{ token Value }

	verbs := []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%T-%[1]v", "%10v", "%-10s|"}
	subjects := map[string]any{
		"value":            v,
		"pointer":          &v,
		"exported field":   exported{v},
		"unexported field": unexported{v},
		"slice":            []Value{v},
		"map":              map[string]Value{"k": v},
		"pointer struct":   &exported{v},
	}
	for name, subject := range subjects {
		for _, verb := range verbs {
			got := fmt.Sprintf(verb, subject)
			if strings.Contains(got, plaintext) {
				t.Errorf("%s with %s leaked: %s", name, verb, got)
			}
		}
	}

	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		if got := fmt.Sprintf(verb, v); got != Marker {
			t.Errorf("Sprintf(%q) = %q, want %q", verb, got, Marker)
		}
	}
	if got := fmt.Sprint(v); got != Marker {
		t.Errorf("Sprint = %q", got)
	}
	if got := v.String(); got != Marker {
		t.Errorf("String() = %q", got)
	}
	if got := v.GoString(); got != Marker {
		t.Errorf("GoString() = %q", got)
	}
}

func TestValueNeverLeaksThroughJSON(t *testing.T) {
	v := New(plaintext)
	payload := map[string]any{
		"direct":  v,
		"pointer": &v,
		"nested":  struct{ Token Value }{v},
		"list":    []Value{v},
		"keyed":   map[Value]int{v: 1},
	}
	out, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte(plaintext)) {
		t.Fatalf("JSON leaked: %s", out)
	}
	if !bytes.Contains(out, []byte(`"direct":"`+Marker+`"`)) {
		t.Errorf("JSON does not carry the marker: %s", out)
	}
}

func TestValueNeverLeaksThroughYAML(t *testing.T) {
	v := New(plaintext)
	payload := map[string]any{
		"direct":  v,
		"pointer": &v,
		"nested":  struct{ Token Value }{v},
		"list":    []Value{v},
	}
	out, err := yaml.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte(plaintext)) {
		t.Fatalf("YAML leaked: %s", out)
	}
	if !bytes.Contains(out, []byte(Marker)) {
		t.Errorf("YAML does not carry the marker: %s", out)
	}
}

func TestValueNeverLeaksThroughSlog(t *testing.T) {
	v := New(plaintext)
	handlers := map[string]func(*bytes.Buffer) slog.Handler{
		"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
		"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
	}
	for name, mk := range handlers {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			log := slog.New(mk(&buf))
			log.Info("resolved", "token", v, "ptr", &v,
				slog.Group("g", slog.Any("inner", v)),
				"struct", struct{ Token Value }{v},
				"formatted", fmt.Sprintf("%v", v))
			log.With("with", v).Info("again")
			if strings.Contains(buf.String(), plaintext) {
				t.Fatalf("slog leaked: %s", buf.String())
			}
			if !strings.Contains(buf.String(), Marker) {
				t.Errorf("slog output lacks the marker: %s", buf.String())
			}
		})
	}
}
