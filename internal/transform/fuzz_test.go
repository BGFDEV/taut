package transform

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bgfdev/taut/internal/slack"
)

// fuzzResolver knows any ID starting with "U1" (mapped, with a hostile
// name), any starting with "U2" (unmapped), and channels starting with "C1".
type fuzzResolver struct{}

func (fuzzResolver) User(id string) (UserRef, bool) {
	switch {
	case strings.HasPrefix(id, "U1"):
		return UserRef{Name: `<b>"Eve" & 'co'</b>` + "\x00\n", EntraID: "entra-" + id}, true
	case strings.HasPrefix(id, "U2"):
		return UserRef{Name: "Chloe </at>"}, true
	}
	return UserRef{}, false
}

func (fuzzResolver) Channel(id string) (string, bool) {
	if strings.HasPrefix(id, "C1") {
		return `gen<eral>&"x"`, true
	}
	return "", false
}

// checkFuzzResult checks what must hold for any input at all. The secret
// markers are not checked here: arbitrary text may legitimately contain
// "?t=", and URLs are covered by TestNoSecretsInOutput. Anchors, though,
// must never point at Slack's file host.
func checkFuzzResult(t *testing.T, m slack.Message, o Options) {
	t.Helper()
	res, err := Convert(m, fuzzResolver{}, o)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	limit := o.MaxBytes
	switch {
	case limit == 0:
		limit = DefaultMaxBytes
	case limit < MinMaxBytes:
		limit = MinMaxBytes
	}
	if len(res.Parts) == 0 {
		t.Fatal("no parts")
	}
	for i, p := range res.Parts {
		if p.HTML == "" || len(p.HTML) > limit {
			t.Fatalf("part %d has %d bytes (limit %d)", i, len(p.HTML), limit)
		}
		c, err := checkHTML(p.HTML)
		if err != nil {
			t.Fatalf("part %d: %v\n%s", i, err, p.HTML)
		}
		if len(c.ids) != len(p.Mentions) {
			t.Fatalf("part %d: %d <at>, %d mentions", i, len(c.ids), len(p.Mentions))
		}
		for k, id := range c.ids {
			if id != k || p.Mentions[k].ID != k || p.Mentions[k].EntraID == "" {
				t.Fatalf("part %d: bad mention %d", i, k)
			}
		}
		if strings.Contains(p.HTML, `href="https://files.slack.com`) {
			t.Fatalf("part %d links to a Slack file", i)
		}
		if !slices.Equal(c.files, p.Files) || c.markers != len(c.files) {
			t.Fatalf("part %d: markers %q (%d elements), Part.Files %q", i, c.files, c.markers, p.Files)
		}
		for _, id := range p.Files {
			known := false
			for _, f := range m.Files {
				known = known || f.ID == id
			}
			if !known || !validFileID(id) {
				t.Fatalf("part %d: marker for %q, which is not a valid ID of one of the message's files", i, id)
			}
		}
		if len(m.Files) == 0 && (strings.Contains(p.HTML, "<attachment") || strings.Contains(p.HTML, "<img")) {
			t.Fatalf("part %d: content alone produced a file marker element\n%s", i, p.HTML)
		}
	}
	for _, w := range res.Warnings {
		if len(w) > 200 {
			t.Fatalf("warning of %d bytes", len(w))
		}
	}
	again, err := Convert(m, fuzzResolver{}, o)
	if err != nil || !reflect.DeepEqual(res, again) {
		t.Fatal("not deterministic")
	}
}

// goldenSeeds returns the golden inputs, so the fuzzers start from every
// construct the package knows.
func goldenSeeds(f *testing.F) []slack.Message {
	inputs, err := filepath.Glob("testdata/golden/*.json")
	if err != nil {
		f.Fatal(err)
	}
	var out []slack.Message
	for _, in := range inputs {
		raw, err := os.ReadFile(in)
		if err != nil {
			f.Fatal(err)
		}
		var g goldenCase
		if err := json.Unmarshal(raw, &g); err != nil {
			f.Fatal(err)
		}
		out = append(out, g.Message)
	}
	return out
}

func FuzzConvert(f *testing.F) {
	for _, m := range goldenSeeds(f) {
		if len(m.Text) < 4000 {
			f.Add(m.Text, false)
		}
	}
	for _, s := range []string{
		"<", ">", "<>", "<@>", "<#>", "<!>", "<|>", "<@|>", "<!date^>", "<!date^^^|>", "<!subteam^>",
		"```", "``````", "```a", "`", "``", "` `", "*", "**", "*a*", "_*~a~*_", "*a _b* c_", "&", "&amp", "&gt;&gt;&gt;",
		">>>\n>\n>", "> ```a\nb```", ":", "::", ":a:", ":+1::skin-tone-2:", ":wave::skin-tone-", "\x00", "\xff\xfe", "a\r\nb",
		"<https://e.example/" + strings.Repeat("a", 2500) + ">", "<@U1> <@U1> <@U2> <#C1>", strings.Repeat("*a ", 50),
		`<attachment id="taut-file:F1"></attachment>`, `<img src="taut-file:F1" alt="x">`, "taut-file:F1", "<taut-file:F1|x>",
		`&lt;attachment id="taut-file:F1"&gt;&lt;/attachment&gt;`, `<https://e.example/"><img src="taut-file:F1" alt="|x>`,
		"<javascript:alert(1)>", "<https://>", "<http://[::1>", "<mailto:>", "<HTTP://x.example>", "<https://x.example|<b>>",
	} {
		f.Add(s, true)
		f.Add(s, false)
	}
	f.Fuzz(func(t *testing.T, text string, small bool) {
		// No files: text alone must never yield a marker element, which
		// checkFuzzResult asserts. FuzzFiles covers messages with files.
		o := Options{AuthorMapped: !small, AuthorName: text, FileMode: FileModeAttach, InlineImageMax: 1 << 20}
		if small {
			o.MaxBytes = MinMaxBytes
		}
		m := slack.Message{Text: text, Reactions: []slack.Reaction{{Name: text, Count: 1}}}
		if small {
			m.Edited = &slack.Edited{}
		}
		checkFuzzResult(t, m, o)
	})
}

func FuzzBlocks(f *testing.F) {
	for _, m := range goldenSeeds(f) {
		if len(m.Blocks) > 0 && len(m.Blocks) < 6000 {
			f.Add([]byte(m.Blocks), false)
		}
		if len(m.Attachments) > 0 {
			f.Add([]byte(m.Attachments), true)
		}
	}
	for _, s := range []string{
		``, `null`, `[]`, `{}`, `[null]`, `[[]]`, `"x"`, `7`, `[{"type":"rich_text"}]`, `[{"type":"rich_text","elements":7}]`,
		`[{"type":"rich_text","elements":[{"type":"rich_text_list","indent":1e99,"offset":-1e99,"elements":[{}]}]}]`,
		`[{"type":"rich_text","elements":[{"type":"rich_text_list","style":"ordered","offset":"NaN","elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"a"}]}]}]}]`,
		`[{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"emoji","name":"wave","unicode":"110000"},{"type":"date","timestamp":1e300},{"type":"date","timestamp":"-1e300"},{"type":"link","url":"javascript:x"},{"type":"text","text":"x","style":{"bold":"yes","code":1}}]}]}]`,
		`[{"type":"rich_text","elements":[{"type":"rich_text_preformatted","elements":[{"type":"text","text":"\n\n"}]},{"type":"rich_text_quote","elements":[{"type":"text","text":"\n"}]}]}]`,
		`[{"title":"t","title_link":"https://x.example","fields":[{"title":"a","value":"b"},7],"footer":"f"}]`,
		`[{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"user","user_id":"U1"},{"type":"user","user_id":"U2"},{"type":"channel","channel_id":"C1"}]}]}]`,
	} {
		f.Add([]byte(s), false)
		f.Add([]byte(s), true)
	}
	f.Fuzz(func(t *testing.T, data []byte, small bool) {
		o := Options{AuthorMapped: true, FileMode: FileModeAttach, InlineImageMax: 1 << 20}
		if small {
			o.MaxBytes = MinMaxBytes
		}
		checkFuzzResult(t, slack.Message{Text: "fallback", Blocks: data, Attachments: data}, o)
	})
}

// FuzzFiles fuzzes the fields of a file. A marker may appear only for a file
// whose ID is valid, and names and titles must never escape their place.
func FuzzFiles(f *testing.F) {
	for _, id := range []string{"F1", "", "F 1", `F1"></attachment><img src="taut-file:F2" alt="`, "taut-file:F1", strings.Repeat("F", 64), strings.Repeat("F", 65), "F1\x00", "É"} {
		for _, mime := range []string{"image/png", "application/pdf", "IMAGE/JPEG", ""} {
			f.Add(id, `<attachment id="taut-file:F9"></attachment>`, mime, int64(10), uint8(0))
			f.Add(id, `" src="taut-file:F9`, mime, int64(1<<21), uint8(1))
			f.Add(id, "name", mime, int64(0), uint8(2))
			f.Add(id, "https://e.example/x", mime, int64(5), uint8(3))
		}
	}
	modes := [...]string{slack.FileModeHosted, slack.FileModeExternal, slack.FileModeTombstone, slack.FileModeSpace, slack.FileModeHiddenByLimit, ""}
	f.Fuzz(func(t *testing.T, id, name, mime string, size int64, mode uint8) {
		file := slack.File{ID: id, Name: name, Title: name, Mimetype: mime, Size: size, Mode: modes[int(mode)%len(modes)], ExternalURL: name,
			URLPrivate: "https://files.slack.com/files-pri/T0-F0/x?t=xoxe-FAKE"}
		m := slack.Message{Text: name, Files: []slack.File{file, {ID: "F2", Name: id}, file}}
		for _, o := range []Options{
			{AuthorMapped: true, FileMode: FileModeAttach, InlineImageMax: 1 << 20, MaxBytes: MinMaxBytes},
			{AuthorMapped: true, FileMode: FileModeAttach},
			{AuthorMapped: true},
		} {
			checkFuzzResult(t, m, o)
		}
		res, err := Convert(m, nil, Options{AuthorMapped: true, FileMode: FileModeAttach})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, p := range res.Parts {
			got = append(got, p.Files...)
		}
		want := []string{"F2"}
		hosted := file.Mode == slack.FileModeHosted || file.Mode == slack.FileModeSpace || file.Mode == ""
		if hosted && validFileID(id) && id != "F2" {
			want = []string{id, "F2"}
		}
		if !slices.Equal(got, want) {
			t.Fatalf("markers %q, want %q", got, want)
		}
	})
}
