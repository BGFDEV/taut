package transform

import (
	"encoding/json"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/bgfdev/taut/internal/fixtures"
	"github.com/bgfdev/taut/internal/slack"
	"github.com/bgfdev/taut/internal/slack/export"
)

// fixtureData is one fixture, read back through the export reader.
type fixtureData struct {
	resolver mapResolver
	messages []slack.Message
}

func loadFixture(t *testing.T, name string) fixtureData {
	t.Helper()
	path, err := fixtures.Write(name, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ex, err := export.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()

	inDirectory := map[string]bool{}
	for _, p := range fixtures.People(name) {
		inDirectory[p.SlackID] = p.InDirectory
	}
	d := fixtureData{resolver: mapResolver{users: map[string]UserRef{}, channels: map[string]string{}}}
	users, err := ex.Users()
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		ref := UserRef{Name: u.DisplayLabel()}
		if inDirectory[u.ID] {
			ref.EntraID = "entra-" + u.ID
		}
		d.resolver.users[u.ID] = ref
	}
	channels, err := ex.Channels()
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range channels {
		d.resolver.channels[ch.ID] = ch.Label()
		for m, err := range ex.Messages(ch) {
			if err != nil {
				continue // malformed entries are the reader's business
			}
			d.messages = append(d.messages, m)
		}
	}
	if len(d.messages) == 0 {
		t.Fatalf("fixture %s has no messages", name)
	}
	return d
}

var fileIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// squeeze removes all white space, the normalization under which the text
// of a message must survive conversion unchanged.
func squeeze(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// visibleText returns the concatenated visible text of all parts.
func visibleText(t *testing.T, res Result) string {
	t.Helper()
	var sb strings.Builder
	for _, p := range res.Parts {
		c, err := checkHTML(p.HTML)
		if err != nil {
			t.Fatal(err)
		}
		sb.WriteString(c.text)
	}
	return sb.String()
}

// richSourceText returns the text of all "text" elements of a message's
// blocks, in order.
func richSourceText(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case []any:
			for _, e := range v {
				walk(e)
			}
		case map[string]any:
			if v["type"] == "text" {
				s, _ := v["text"].(string)
				sb.WriteString(s)
			}
			walk(v["elements"])
		}
	}
	walk(v)
	return sb.String()
}

func TestFixtureProperties(t *testing.T) {
	optionSets := []Options{
		{AuthorMapped: true},
		{AuthorName: "Someone <else> & \"co\"", AuthorKind: "former", MaxBytes: MinMaxBytes},
		{AuthorMapped: true, Reactions: ReactionsNative, MaxBytes: 5_000},
		{AuthorMapped: true, FileMode: FileModeNote, InlineImageMax: 1 << 20},
		{AuthorMapped: true, FileMode: FileModeAttach},
		{AuthorName: "Someone", AuthorKind: "former", FileMode: FileModeAttach, InlineImageMax: 1 << 20, MaxBytes: MinMaxBytes},
	}
	for _, name := range fixtures.Names() {
		t.Run(name, func(t *testing.T) {
			d := loadFixture(t, name)
			converted, mentions, split, markers, images := 0, 0, 0, 0, 0
			for _, m := range d.messages {
				cl := Classify(m)
				if cl.Disposition == Skip {
					continue
				}
				converted++
				for _, o := range optionSets {
					res, err := Convert(m, d.resolver, o)
					if err != nil {
						t.Fatalf("%v: Convert: %v", m, err)
					}
					limit := o.MaxBytes
					if limit == 0 {
						limit = DefaultMaxBytes
					}
					if err := checkResult(res, limit); err != nil {
						t.Fatalf("%v (limit %d): %v", m, limit, err)
					}
					again, err := Convert(m, d.resolver, o)
					if err != nil || !reflect.DeepEqual(res, again) {
						t.Fatalf("%v: second run differs (err %v)", m, err)
					}
					if len(res.Files) != len(m.Files) {
						t.Fatalf("%v: %d file refs for %d files", m, len(res.Files), len(m.Files))
					}
					if cl.Disposition == Placeholder && (len(res.Parts) != 1 || !strings.Contains(res.Parts[0].HTML, "<em>")) {
						t.Fatalf("%v: placeholder rendered as %d parts", m, len(res.Parts))
					}
					if !o.AuthorMapped {
						for i, p := range res.Parts {
							if got := strings.Contains(p.HTML, "(former member)"); got != (i == 0) {
								t.Fatalf("%v: byline present in part %d: %t", m, i, got)
							}
						}
					}
					if m.Edited != nil && cl.Disposition == Import {
						for i, p := range res.Parts {
							if got := strings.Contains(p.HTML, "<p><em>(edited)</em></p>"); got != (i == len(res.Parts)-1) {
								t.Fatalf("%v: edited marker present in part %d of %d: %t", m, i, len(res.Parts), got)
							}
						}
					}
					// File markers: none outside attach mode; in it, exactly
					// the message's hosted, available files, each once.
					var got []string
					for _, p := range res.Parts {
						mentions += len(p.Mentions)
						got = append(got, p.Files...)
						images += strings.Count(p.HTML, "<img ")
					}
					var want []string
					if o.FileMode == FileModeAttach && cl.Disposition == Import {
						for _, f := range m.Files {
							if !f.IsExternal && f.Mode != slack.FileModeExternal && !f.IsHidden() && !f.IsTombstone() && !slices.Contains(want, f.ID) {
								want = append(want, f.ID)
							}
						}
					}
					if !slices.Equal(got, want) {
						t.Fatalf("%v (mode %q): markers %q, want %q", m, o.FileMode, got, want)
					}
					for _, id := range got {
						if !fileIDRE.MatchString(id) {
							t.Fatalf("%v: marker ID %q is not valid", m, id)
						}
					}
					if o.FileMode == FileModeAttach && strings.Contains(res.Parts[0].HTML+res.Parts[len(res.Parts)-1].HTML, "file not migrated yet") {
						t.Fatalf("%v: attach mode wrote the note line", m)
					}
					if o.InlineImageMax == 0 || o.FileMode != FileModeAttach {
						for _, p := range res.Parts {
							if strings.Contains(p.HTML, "<img") {
								t.Fatalf("%v: inline image without inlining enabled", m)
							}
						}
					}
					markers += len(got)
					if len(res.Parts) > 1 {
						split++
					}
				}
			}
			t.Logf("%s: %d messages, %d converted, %d mention entities, %d split results", name, len(d.messages), converted, mentions, split)
			t.Logf("%s: %d file markers, %d of them inline images", name, markers, images)
			if name != "small" && (markers == 0 || images == 0) {
				t.Error("attach mode produced no markers or no inline images")
			}
			if name != "small" && mentions == 0 {
				t.Error("no mention entity was produced; the resolver wiring is wrong")
			}
		})
	}
}

// TestLargeMessagesSplit checks the nasty fixture's 60 KB (rich_text) and
// 150 KB (mrkdwn) messages: how many parts they take, and that the parts'
// visible text, joined, is the original text. The comparison ignores white
// space only (the transform turns newlines into <br> and block boundaries;
// see the package comment).
func TestLargeMessagesSplit(t *testing.T) {
	d := loadFixture(t, "nasty")
	var rich, plain *slack.Message
	for i, m := range d.messages {
		switch {
		case len(m.Text) < 50_000:
		case len(m.Blocks) > 0:
			rich = &d.messages[i]
		case plain == nil || len(m.Text) > len(plain.Text):
			// The largest: a direct message has a smaller one that splits.
			plain = &d.messages[i]
		}
	}
	if rich == nil || plain == nil {
		t.Fatal("the nasty fixture no longer has its two large messages")
	}
	if n := len(rich.Text); n < 60<<10 || n > 62<<10 {
		t.Fatalf("rich message has %d bytes", n)
	}
	if n := len(plain.Text); n < 150<<10 || n > 152<<10 {
		t.Fatalf("plain message has %d bytes", n)
	}
	cases := []struct {
		name   string
		m      slack.Message
		source string
		limit  int // 0: default
		parts  int
	}{
		{"rich 60KB, default limit", *rich, richSourceText(t, rich.Blocks), 0, 1},
		{"plain 150KB, default limit", *plain, plain.Text, 0, 2},
		{"rich 60KB, 20KB limit", *rich, richSourceText(t, rich.Blocks), 20_000, 4},
		{"plain 150KB, 20KB limit", *plain, plain.Text, 20_000, 8},
		{"plain 150KB, 2KB limit", *plain, plain.Text, 2_000, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := Convert(c.m, d.resolver, Options{AuthorMapped: true, MaxBytes: c.limit})
			if err != nil {
				t.Fatal(err)
			}
			limit := c.limit
			if limit == 0 {
				limit = DefaultMaxBytes
			}
			if err := checkResult(res, limit); err != nil {
				t.Fatal(err)
			}
			total := 0
			for _, p := range res.Parts {
				total += len(p.HTML)
			}
			// Each part repeats <p></p> and may stop up to cutWindow bytes
			// short of the limit to break at a space.
			least := (total + limit - 1) / limit
			most := total/(limit-cutWindow-len("<p></p>")) + 1
			if n := len(res.Parts); n < least || n > most || (c.parts != 0 && n != c.parts) {
				t.Errorf("%d parts for %d bytes; want %d (bounds %d..%d)", n, total, c.parts, least, most)
			}
			got, want := squeeze(visibleText(t, res)), squeeze(c.source)
			if got != want {
				i := 0
				for i < len(got) && i < len(want) && got[i] == want[i] {
					i++
				}
				t.Errorf("visible text differs from the source at byte %d (got %d bytes, want %d)", i, len(got), len(want))
			}
			if len(res.Warnings) != 0 {
				t.Errorf("warnings: %q", res.Warnings)
			}
		})
	}
}

// TestSplitInvariants drives the splitter with content built to put every
// kind of boundary at the limit.
func TestSplitInvariants(t *testing.T) {
	texts := map[string]string{
		"entities":    strings.Repeat(`&amp;&lt;&gt;"'`, 3000),
		"no spaces":   strings.Repeat("x", 30_000),
		"astral":      strings.Repeat("𠮷🚀", 4000),
		"zwj":         strings.Repeat("👩‍👩‍👧‍👦", 1500),
		"combining":   strings.Repeat("é̂̃", 5000),
		"mentions":    strings.Repeat("<@U1> <@U2|ben> ", 2500),
		"links":       strings.Repeat("<https://wiki.example/a?b=1&amp;c=2|label> ", 1500),
		"formatting":  strings.Repeat("*bold _both ~all~_* `code` ", 2000),
		"code block":  "```" + strings.Repeat("line of code <&>\n", 3000) + "```",
		"quote":       "&gt;&gt;&gt; " + strings.Repeat("quoted words here\n", 3000),
		"lines":       strings.Repeat("a\n", 10_000),
		"paragraphs":  strings.Repeat("para\n\n", 5000),
		"long link":   "<https://wiki.example/" + strings.Repeat("a", 5000) + "|x> <@U1>",
		"emoji":       strings.Repeat(":wave::skin-tone-3:", 3000),
		"single rune": "x",
	}
	for name, src := range texts {
		for _, limit := range []int{MinMaxBytes, 3_333, 80_000} {
			res, err := Convert(slack.Message{Text: src, Edited: &slack.Edited{}, Reactions: []slack.Reaction{{Name: "eyes", Count: 2}}},
				goldenResolver, Options{MaxBytes: limit, AuthorName: "Ada"})
			if err != nil {
				t.Fatalf("%s/%d: %v", name, limit, err)
			}
			if err := checkResult(res, limit); err != nil {
				t.Errorf("%s/%d: %v", name, limit, err)
			}
			last := res.Parts[len(res.Parts)-1].HTML
			if !strings.HasSuffix(last, "<p><em>(edited)</em></p><p><em>👀 2</em></p>") {
				t.Errorf("%s/%d: last part does not end with the edited marker and reactions", name, limit)
			}
			if !strings.HasPrefix(res.Parts[0].HTML, "<p><strong>Ada</strong> <em>(via Slack)</em></p>") {
				t.Errorf("%s/%d: first part does not start with the byline", name, limit)
			}
		}
	}
	// Limits below the minimum are raised to it; zero means the default.
	big := slack.Message{Text: strings.Repeat("word ", 40_000)}
	for limit, want := range map[int]int{-5: MinMaxBytes, 1: MinMaxBytes, 1999: MinMaxBytes, 0: DefaultMaxBytes} {
		res, err := Convert(big, nil, Options{AuthorMapped: true, MaxBytes: limit})
		if err != nil {
			t.Fatal(err)
		}
		if err := checkResult(res, want); err != nil {
			t.Errorf("MaxBytes %d: %v", limit, err)
		}
		if len(res.Parts[0].HTML) <= want-cutWindow-10 {
			t.Errorf("MaxBytes %d: first part has only %d bytes, limit should be %d", limit, len(res.Parts[0].HTML), want)
		}
	}
}

// TestNoSecretsInOutput feeds Slack file URLs through every place a URL can
// appear and checks none survives.
func TestNoSecretsInOutput(t *testing.T) {
	const secret = "https://files.slack.com/files-pri/T0-F0/a.png?t=xoxe-FAKE-TOKEN"
	q, _ := json.Marshal(secret)
	m := slack.Message{
		Text:        "<" + secret + "> <" + secret + "|label> <" + secret + "|" + secret + ">",
		Blocks:      json.RawMessage(`[{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"link","url":` + string(q) + `},{"type":"link","url":` + string(q) + `,"text":` + string(q) + `}]},{"type":"rich_text_preformatted","elements":[{"type":"link","url":` + string(q) + `}]}]},{"type":"image","image_url":` + string(q) + `,"alt_text":"img"}]`),
		Attachments: json.RawMessage(`[{"title":"t","title_link":` + string(q) + `,"image_url":` + string(q) + `,"thumb_url":` + string(q) + `,"from_url":` + string(q) + `,"text":"<` + secret + `>"}]`),
		Files: []slack.File{
			{ID: "F1", Name: "a.png", URLPrivate: secret, URLPrivateDownload: secret, Permalink: secret},
			{ID: "F2", Name: "b", IsExternal: true, ExternalURL: secret, URLPrivate: secret},
		},
	}
	for _, withBlocks := range []bool{true, false} {
		if !withBlocks {
			m.Blocks = nil
		}
		res, err := Convert(m, nil, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := checkResult(res, DefaultMaxBytes); err != nil {
			t.Errorf("blocks=%t: %v\n%s", withBlocks, err, res.Parts[0].HTML)
		}
		if !strings.Contains(res.Parts[0].HTML, slackFileLinkText) {
			t.Errorf("blocks=%t: expected the file-link stand-in text", withBlocks)
		}
	}
}

func TestWarningsCarryNoContent(t *testing.T) {
	const secretWord = "TOPSECRETCONTENT"
	m := slack.Message{
		Text:        secretWord,
		Blocks:      json.RawMessage(`[{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"zzz","text":"` + secretWord + `"},"` + secretWord + `"]},{"type":"zzz2","text":"` + secretWord + `"}]},{"type":"zzz3","text":"` + secretWord + `"}]`),
		Attachments: json.RawMessage(`{"text":"` + secretWord + `"}`),
	}
	res, err := Convert(m, nil, Options{Reactions: "bogus"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) < 5 {
		t.Errorf("expected several warnings, got %q", res.Warnings)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, secretWord) {
			t.Errorf("warning carries message content: %q", w)
		}
	}
}

// TestFileMarkers checks the marker rules that the goldens cannot pin down
// exactly: validation of IDs, the size reserved for replacement, and that
// the renderer itself refuses anything but a valid marker.
func TestFileMarkers(t *testing.T) {
	for id, want := range map[string]bool{
		"F1": true, "f_1-A": true, strings.Repeat("a", 64): true,
		"": false, strings.Repeat("a", 65): false, "F 1": false, "F1\n": false, "F1\x00": false, `F1"`: false,
		"F1>": false, "taut-file:F1": false, "É": false, "F.1": false, "F/1": false,
	} {
		if validFileID(id) != want || fileIDRE.MatchString(id) != want {
			t.Errorf("validFileID(%q) = %t, want %t", id, validFileID(id), want)
		}
	}

	// The renderer is the last line of defence: a marker node with a bad ID
	// renders nothing and is not listed.
	bad := renderPart([]*node{{kind: kImg, text: `x" onerror="alert(1)`, attr: "a"}, {kind: kAttach, text: "a b"}, {kind: kAttach, text: ""}})
	if bad.HTML != "" || len(bad.Files) != 0 {
		t.Errorf("renderer wrote an invalid marker: %q %q", bad.HTML, bad.Files)
	}

	// Every marker is sized as if its ID had 64 characters, so a part still
	// fits once the marker is replaced by the longest replacement.
	short, long := fileNode("F1", "alt", false), fileNode(strings.Repeat("F", 64), "alt", false)
	if measure(short, 1) != measure(long, 1) || measure(long, 1) != len(renderPart([]*node{long}).HTML) {
		t.Errorf("attachment marker sizes: short %d, long %d, rendered long %d", short.sz, long.sz, len(renderPart([]*node{long}).HTML))
	}
	shortImg, longImg := fileNode("F1", `a "b" <c>`, true), fileNode(strings.Repeat("F", 64), `a "b" <c>`, true)
	if measure(shortImg, 1) != measure(longImg, 1) || measure(longImg, 1) != len(renderPart([]*node{longImg}).HTML) {
		t.Errorf("image marker sizes: short %d, long %d", shortImg.sz, longImg.sz)
	}
	const guid = "01234567-89ab-cdef-0123-456789abcdef"
	if got := len(`<p><attachment id="` + guid + `"></attachment></p>`); got > long.sz {
		t.Errorf("a resolved attachment (%d bytes) is larger than the reserved %d", got, long.sz)
	}
	if got := len(`<p><img src="../hostedContents/1234567890/$value" alt="a &quot;b&quot; &lt;c&gt;"></p>`); got > longImg.sz {
		t.Errorf("a resolved image (%d bytes) is larger than the reserved %d", got, longImg.sz)
	}

	// Many files with short IDs on a message that must split: after
	// replacing every marker by its longest replacement, each part still
	// fits, and Part.Files matches the markers part by part.
	var m slack.Message
	m.Text = strings.Repeat("words and more words ", 150)
	for i := range 60 {
		f := slack.File{ID: "F" + strconv.Itoa(i), Name: "n", Title: strings.Repeat("é", i*5), Mimetype: "application/pdf", Size: 10}
		if i%3 == 0 {
			f.Mimetype = "image/png"
		}
		m.Files = append(m.Files, f)
	}
	res, err := Convert(m, nil, Options{AuthorMapped: true, MaxBytes: MinMaxBytes, FileMode: FileModeAttach, InlineImageMax: 100})
	if err != nil {
		t.Fatal(err)
	}
	if err := checkResult(res, MinMaxBytes); err != nil {
		t.Fatal(err)
	}
	total := 0
	for i, p := range res.Parts {
		total += len(p.Files)
		resolved := p.HTML
		for n, id := range p.Files {
			resolved = strings.ReplaceAll(resolved, `<attachment id="`+FileMarkerPrefix+id+`"></attachment>`, `<attachment id="`+guid+`"></attachment>`)
			resolved = strings.ReplaceAll(resolved, `<img src="`+FileMarkerPrefix+id+`"`, `<img src="../hostedContents/`+strconv.Itoa(1_000_000_000+n)+`/$value"`)
		}
		if strings.Contains(resolved, FileMarkerPrefix) {
			t.Errorf("part %d: a marker is not listed in Part.Files", i)
		}
		if len(resolved) > MinMaxBytes {
			t.Errorf("part %d has %d bytes once its markers are resolved", i, len(resolved))
		}
	}
	if total != 60 || len(res.Parts) < 3 {
		t.Errorf("%d markers in %d parts, want 60 in several", total, len(res.Parts))
	}
}
