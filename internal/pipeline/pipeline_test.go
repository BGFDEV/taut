package pipeline

import (
	"bytes"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	in := []Post{
		{ChannelID: "C1", SlackTS: "1700000000.000100", Parts: 1, TargetMS: 1700000000000, FromID: "u1", HTML: "<p>a &amp; b</p>"},
		{ChannelID: "C1", SlackTS: "1700000001.000100", Parts: 1, TargetMS: 1700000001000, AnchorTS: "1700000000.000100", FromID: "u2",
			HTML: `<p><at id="0">Ada</at></p>`, Mentions: []Mention{{ID: 0, Text: "Ada", UserID: "u1"}}},
	}
	for i := range in {
		in[i].Hash = in[i].ContentHash()
		if err := w.Write(in[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if w.Count() != 2 {
		t.Fatalf("count = %d", w.Count())
	}
	var out []Post
	for p, err := range Read[Post](&buf) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	if len(out) != 2 || out[1].Mentions[0].UserID != "u1" || out[0].HTML != in[0].HTML || out[1].Hash != out[1].ContentHash() {
		t.Fatalf("round trip mismatch: %+v", out)
	}
}

func TestContentHashIsUnambiguous(t *testing.T) {
	base := Post{TargetMS: 1, FromID: "ab", HTML: "c"}
	seen := map[string]string{}
	for name, p := range map[string]Post{
		"base":                  base,
		"shifted fields":        {TargetMS: 1, FromID: "a", HTML: "bc"},
		"other time":            {TargetMS: 2, FromID: "ab", HTML: "c"},
		"with mention":          {TargetMS: 1, FromID: "ab", HTML: "c", Mentions: []Mention{{ID: 0, Text: "x", UserID: "y"}}},
		"mention shift":         {TargetMS: 1, FromID: "ab", HTML: "c", Mentions: []Mention{{ID: 0, Text: "xy", UserID: ""}}},
		"with reaction":         {TargetMS: 1, FromID: "ab", HTML: "c", Reactions: []Reaction{{Type: "👍", UserID: "y"}}, FooterHTML: "f"},
		"reaction shift":        {TargetMS: 1, FromID: "ab", HTML: "c", Reactions: []Reaction{{Type: "👍y", UserID: ""}}, FooterHTML: "f"},
		"other reactor":         {TargetMS: 1, FromID: "ab", HTML: "c", Reactions: []Reaction{{Type: "👍", UserID: "z"}}, FooterHTML: "f"},
		"other fallback":        {TargetMS: 1, FromID: "ab", HTML: "c", Reactions: []Reaction{{Type: "👍", UserID: "y"}}, FooterHTML: "g"},
		"two reactions":         {TargetMS: 1, FromID: "ab", HTML: "c", Reactions: []Reaction{{Type: "👍", UserID: "y"}, {Type: "👍", UserID: "y"}}, FooterHTML: "f"},
		"mention, not reaction": {TargetMS: 1, FromID: "ab", HTML: "c", Mentions: []Mention{{ID: 1, Text: "👍", UserID: "y"}}},
	} {
		h := p.ContentHash()
		if other, dup := seen[h]; dup {
			t.Fatalf("%q and %q hash the same", name, other)
		}
		seen[h] = name
	}
	if base.ContentHash() != base.ContentHash() {
		t.Fatal("hash is not deterministic")
	}
	// A post without reactions hashes as it did before posts could carry
	// them, so the ledger of a run started by an older version still
	// matches its staged posts.
	// SHA-256 of "1:1" "2:ab" "1:c" "1:0" "1:0": time, author, body, no
	// files, no mentions.
	const before = "28d496def808ebea3e5ba9a2aa92a9f0b40dd58b4d21f026b6b57a64c1212e9f"
	if got := base.ContentHash(); got != before {
		t.Errorf("hash of a post without reactions = %s, want %s", got, before)
	}
	if empty := (Post{TargetMS: 1, FromID: "ab", HTML: "c", Reactions: []Reaction{}}); empty.ContentHash() != before {
		t.Error("an empty reactions list changes the hash")
	}
}

func TestReadRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "not gzip at all"} {
		for _, err := range Read[Post](strings.NewReader(in)) {
			if err == nil {
				t.Fatalf("no error for %q", in)
			}
		}
	}
	var buf bytes.Buffer
	w := NewWriter(&buf)
	_ = w.Write("a string, not a post")
	_ = w.Close()
	n := 0
	for _, err := range Read[Post](&buf) {
		n++
		if err == nil {
			t.Fatal("a mistyped record must be an error")
		}
	}
	if n != 1 {
		t.Fatalf("iteration should stop at the first error, saw %d yields", n)
	}
}
