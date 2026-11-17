package transform

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/slack"
)

func blocksOf(t *testing.T, blocks ...any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func markdownBlock(text string) map[string]any {
	return map[string]any{"type": "markdown", "text": text}
}

func convertBlocks(t *testing.T, limit int, blocks ...any) Result {
	t.Helper()
	res, err := Convert(slack.Message{Blocks: blocksOf(t, blocks...)}, goldenResolver, Options{AuthorMapped: true, MaxBytes: limit})
	if err != nil {
		t.Fatal(err)
	}
	if limit == 0 {
		limit = DefaultMaxBytes
	}
	if err := checkResult(res, limit); err != nil {
		t.Fatalf("%v\n%s", err, res.Parts[0].HTML)
	}
	return res
}

func TestMarkdownInline(t *testing.T) {
	for in, want := range map[string]string{
		"plain":                            "<p>plain</p>",
		"**b**":                            "<p><strong>b</strong></p>",
		"__b__":                            "<p><strong>b</strong></p>",
		"*i* _i_":                          "<p><em>i</em> <em>i</em></p>",
		"***bi***":                         "<p><strong><em>bi</em></strong></p>",
		"~~s~~ ~s~":                        "<p><s>s</s> <s>s</s></p>",
		"a_b_c":                            "<p>a_b_c</p>",
		"* not a list item*":               "<ul><li>not a list item*</li></ul>",
		"a * b * c":                        "<p>a * b * c</p>",
		"**unclosed":                       "<p>**unclosed</p>",
		`\*literal\*`:                      "<p>*literal*</p>",
		"`a*b*c`":                          "<p><code>a*b*c</code></p>",
		"``a`b``":                          "<p><code>a`b</code></p>",
		"`unclosed":                        "<p>`unclosed</p>",
		"[t](https://e.example)":           `<p><a href="https://e.example">t</a></p>`,
		"[*t*](https://e.example 'title')": `<p><a href="https://e.example"><em>t</em></a></p>`,
		"[t](<https://e.example/a b>)":     `<p><a href="https://e.example/a%20b">t</a></p>`,
		"[t](javascript:alert(1))":         "<p>t (javascript:alert(1))</p>",
		"[t](vbscript:x)":                  "<p>t (vbscript:x)</p>",
		"[t]( https://e.example )":         `<p><a href="https://e.example">t</a></p>`,
		"[a [b](https://x.example)](https://y.example)": `<p><a href="https://y.example">a b</a></p>`,
		"[t]":                                "<p>[t]</p>",
		"[t](":                               "<p>[t](</p>",
		"![alt](https://e.example/i.png)":    `<p><a href="https://e.example/i.png">alt</a></p>`,
		"see https://e.example/a.":           `<p>see <a href="https://e.example/a">https://e.example/a</a>.</p>`,
		"(https://e.example/a_(b))":          `<p>(<a href="https://e.example/a_(b)">https://e.example/a_(b)</a>)</p>`,
		"xhttps://e.example":                 "<p>xhttps://e.example</p>",
		"http://":                            "<p>http://</p>",
		"<https://e.example>":                `<p><a href="https://e.example">https://e.example</a></p>`,
		"<b>x</b>":                           "<p>&lt;b&gt;x&lt;/b&gt;</p>",
		"a\nb":                               "<p>a<br>b</p>",
		"a  \nb":                             "<p>a<br>b</p>",
		"a\\\nb":                             "<p>a<br>b</p>",
		"&amp; &lt; &gt; &quot;":             "<p>&amp; &lt; &gt; &amp;quot;</p>",
		":tada: :not-an-emoji:":              "<p>🎉 :not-an-emoji:</p>",
		"# h":                                "<p><strong>h</strong></p>",
		"###### h ######":                    "<p><strong>h</strong></p>",
		"####### not a heading":              "<p>####### not a heading</p>",
		"#hashtag":                           "<p>#hashtag</p>",
		"h\n===":                             "<p><strong>h</strong></p>",
		"h\n---":                             "<p><strong>h</strong></p>",
		"a\n\n---\n\nb":                      "<p>a</p><p>b</p>",
		"***":                                "",
		"> q\n> r\n\np":                      "<blockquote><p>q<br>r</p></blockquote><p>p</p>",
		"> # h\n> - a":                       "<blockquote><p><strong>h</strong></p><ul><li>a</li></ul></blockquote>",
		"- a\n- b":                           "<ul><li>a</li><li>b</li></ul>",
		"- a\n\n- b":                         "<ul><li>a</li><li>b</li></ul>",
		"1. a\n2. b":                         "<ol><li>a</li><li>b</li></ol>",
		"3. a\n4. b":                         `<ol start="3"><li>a</li><li>b</li></ol>`,
		"- a\n  - b\n- c":                    "<ul><li>a<ul><li>b</li></ul></li><li>c</li></ul>",
		"1. a\n   - b\n2. c":                 "<ol><li>a<ul><li>b</li></ul></li><li>c</li></ol>",
		"- [ ] a\n- [x] b\n- [?] c":          "<ul><li>☐ a</li><li>☑ b</li><li>[?] c</li></ul>",
		"- a\n\n  b":                         "<ul><li>a<br>b</li></ul>",
		"1. a\n\n   ```\n   c\n   ```\n2. b": `<ol><li>a</li></ol><pre><code>c</code></pre><ol start="2"><li>b</li></ol>`,
		"-":                                  "<p>-</p>",
		"- ":                                 "<p>-</p>",
		"1.":                                 "<p>1.</p>",
		"```\ncode\n```":                     "<pre><code>code</code></pre>",
		"```go\n<b>\n```":                    "<pre><code>&lt;b&gt;</code></pre>",
		"~~~\na\n~~~":                        "<pre><code>a</code></pre>",
		"```\n```":                           "",
		"    code":                           "<pre><code>code</code></pre>",
		"|a|b|\n|-|-|\n|1|2|":                "<table><tr><td><strong>a</strong></td><td><strong>b</strong></td></tr><tr><td>1</td><td>2</td></tr></table>",
		"a|b\n:-:|-:\n1|2":                   `<table><tr><td style="text-align:center"><strong>a</strong></td><td style="text-align:right"><strong>b</strong></td></tr><tr><td style="text-align:center">1</td><td style="text-align:right">2</td></tr></table>`,
		"|a|b|\n|-|\n|1|2|":                  "<p>|a|b|<br>|-|<br>|1|2|</p>",
		"<@U1> and <@U3> and <!channel>":     `<p><at id="0">Ada Abbott</at> and @Chloe Castro and @channel</p>`,
		"[<@U1>](https://e.example)":         `<p><a href="https://e.example">@Ada Abbott</a></p>`,
		"https://files.slack.com/f?t=xox":    "<p>(Slack file link)</p>",
	} {
		res := convertBlocks(t, 0, markdownBlock(in))
		got := res.Parts[0].HTML
		if want == "" {
			want = "<p><em>" + emptyText + "</em></p>"
		}
		if got != want {
			t.Errorf("markdown %q\n got %s\nwant %s", in, got, want)
		}
	}
}

// Input built to make a careless parser slow or deep must still convert
// quickly, into valid parts.
func TestMarkdownAdversarial(t *testing.T) {
	const n = maxMdBytes - 16
	for name, src := range map[string]string{
		"brackets":       strings.Repeat("[", n),
		"bracket pairs":  strings.Repeat("[]", n/2),
		"open links":     strings.Repeat("[a](", n/4),
		"nested links":   strings.Repeat("[", n/8) + "x" + strings.Repeat("](https://e.example)", n/40),
		"stars":          strings.Repeat("*", n),
		"star words":     strings.Repeat("*a ", n/3),
		"closers":        strings.Repeat("a* ", n/3),
		"underscores":    strings.Repeat("_a_", n/3),
		"backticks":      strings.Repeat("`", n),
		"tick runs":      strings.Repeat("`` ` ", n/5),
		"angles":         strings.Repeat("<", n),
		"angle pairs":    strings.Repeat("<>", n/2),
		"quotes":         strings.Repeat(">", n),
		"quote lines":    strings.Repeat("> > > >\n", n/8),
		"bullets":        strings.Repeat("- ", n/2),
		"bullet lines":   strings.Repeat("- a\n", n/4),
		"deep bullets":   deepList(400),
		"blank in item":  "- a\n" + strings.Repeat("\n", n) + "  b",
		"pipes":          strings.Repeat("|", n),
		"pipe table":     "|a|\n|-|\n" + strings.Repeat("|x|\n", n/4),
		"wide table":     strings.Repeat("|a", n/8) + "\n" + strings.Repeat("|-", n/8) + "\n|b",
		"fences":         strings.Repeat("```\n", n/4),
		"hashes":         strings.Repeat("#", n),
		"heading lines":  strings.Repeat("# h\n", n/4),
		"setext":         strings.Repeat("a\n=\n", n/4),
		"urls":           strings.Repeat("https://e.example/a ", n/20),
		"long url":       "https://e.example/" + strings.Repeat("a", n-40),
		"escapes":        strings.Repeat(`\*`, n/2),
		"colons":         strings.Repeat(":", n),
		"numbers":        strings.Repeat("1. ", n/3),
		"number lines":   strings.Repeat("999999999. a\n", n/13),
		"newlines":       strings.Repeat("\n", n),
		"control":        strings.Repeat("\x00\x1b", n/2),
		"invalid utf-8":  strings.Repeat("\xff\xfe", n/2),
		"emphasis depth": strings.Repeat("*a _b ", n/12) + strings.Repeat("_ *", n/12),
	} {
		t.Run(name, func(t *testing.T) {
			for _, limit := range []int{0, MinMaxBytes} {
				start := time.Now()
				res, err := Convert(slack.Message{Blocks: blocksOf(t, markdownBlock(src))}, fuzzResolver{}, Options{AuthorMapped: true, MaxBytes: limit})
				if err != nil {
					t.Fatal(err)
				}
				if d := time.Since(start); d > 15*time.Second {
					t.Errorf("limit %d: took %s", limit, d)
				}
				if limit == 0 {
					limit = DefaultMaxBytes
				}
				for i, p := range res.Parts {
					if _, err := checkHTML(p.HTML); err != nil || p.HTML == "" || len(p.HTML) > limit {
						t.Fatalf("part %d (%d bytes): %v", i, len(p.HTML), err)
					}
				}
			}
		})
	}
}

func deepList(depth int) string {
	var sb strings.Builder
	for i := range depth {
		sb.WriteString(strings.Repeat(" ", 2*i))
		sb.WriteString("- level\n")
	}
	return sb.String()
}

// Nesting past maxMdDepth keeps the text and stops adding structure.
func TestMarkdownDepthIsBounded(t *testing.T) {
	res := convertBlocks(t, 0, markdownBlock(deepList(40)))
	html := res.Parts[0].HTML
	if n := strings.Count(html, "<ul>"); n > maxMdDepth+2 {
		t.Errorf("%d nested lists", n)
	}
	if n := strings.Count(html, "level"); n != 40 {
		t.Errorf("%d of 40 items kept their text", n)
	}
	res = convertBlocks(t, 0, markdownBlock(strings.Repeat("> ", 40)+"deep"))
	if n := strings.Count(res.Parts[0].HTML, "<blockquote>"); n > maxMdDepth+2 || !strings.Contains(res.Parts[0].HTML, "deep") {
		t.Errorf("%d nested quotes: %s", n, res.Parts[0].HTML)
	}
}

// Markdown far larger than Slack allows is not parsed. Its text is kept.
func TestMarkdownTooLarge(t *testing.T) {
	src := strings.Repeat("**word** <b>x</b>\n\n", maxMdBytes/19+1)
	res := convertBlocks(t, 0, markdownBlock(src))
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "too large") {
		t.Errorf("warnings %q", res.Warnings)
	}
	var all strings.Builder
	for _, p := range res.Parts {
		all.WriteString(p.HTML)
	}
	if strings.Contains(all.String(), "<strong>") || strings.Count(all.String(), "**word** &lt;b&gt;x&lt;/b&gt;") != maxMdBytes/19+1 {
		t.Error("oversized markdown was parsed, or lost text")
	}
}

func TestTableShapes(t *testing.T) {
	cellOf := func(s string) map[string]any { return map[string]any{"type": "raw_text", "text": s} }
	wide := make([]any, 30)
	for i := range wide {
		wide[i] = cellOf("w")
	}
	res := convertBlocks(t, 0, map[string]any{"type": "table", "rows": []any{wide, []any{cellOf("only")}, []any{}}})
	html := res.Parts[0].HTML
	rows := strings.Split(strings.TrimSuffix(strings.TrimPrefix(html, "<table><tr>"), "</tr></table>"), "</tr><tr>")
	if len(rows) != 2 || strings.Count(rows[0], "<td>") != 30 || strings.Count(rows[1], "<td>") != maxTableCols {
		t.Errorf("rows: %d, cells %d and %d, want 2 rows of 30 and %d", len(rows), strings.Count(rows[0], "<td>"), strings.Count(rows[1], "<td>"), maxTableCols)
	}

	// A table is cut between rows when it has to be divided.
	var many []any
	for range 200 {
		many = append(many, []any{cellOf("left cell"), cellOf("right cell")})
	}
	res = convertBlocks(t, MinMaxBytes, map[string]any{"type": "table", "rows": many})
	if len(res.Parts) < 3 {
		t.Fatalf("%d parts", len(res.Parts))
	}
	total := 0
	for i, p := range res.Parts {
		if !strings.HasPrefix(p.HTML, "<table><tr><td>left cell</td>") || !strings.HasSuffix(p.HTML, "<td>right cell</td></tr></table>") {
			t.Errorf("part %d does not hold whole rows: %s … %s", i, p.HTML[:40], p.HTML[len(p.HTML)-40:])
		}
		total += strings.Count(p.HTML, "<tr>")
	}
	if total != 200 {
		t.Errorf("%d rows after splitting, want 200", total)
	}
}

// A message whose blocks only decorate its text keeps using the text; one
// with no text uses whatever its blocks say.
func TestBlocksOrText(t *testing.T) {
	section := map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "from the section"}}
	for _, tc := range []struct {
		name, text string
		blocks     []any
		want       string
	}{
		{"section and text", "from the text", []any{section}, "<p>from the text</p>"},
		{"section alone", "", []any{section}, "<p>from the section</p>"},
		{"section and blank text", " \n", []any{section}, "<p>from the section</p>"},
		{"markdown and text", "from the text", []any{markdownBlock("from *markdown*")}, "<p>from <em>markdown</em></p>"},
		{"table and text", "from the text", []any{map[string]any{"type": "table", "rows": []any{[]any{map[string]any{"type": "raw_text", "text": "cell"}}}}}, "<table><tr><td>cell</td></tr></table>"},
		{"container of rich text", "from the text", []any{map[string]any{"type": "container", "child_blocks": []any{
			map[string]any{"type": "rich_text", "elements": []any{map[string]any{"type": "rich_text_section", "elements": []any{map[string]any{"type": "text", "text": "inside"}}}}}}}}, "<p>inside</p>"},
		{"container of a section", "from the text", []any{map[string]any{"type": "container", "child_blocks": []any{section}}}, "<p>from the text</p>"},
		{"empty markdown and text", "from the text", []any{markdownBlock(" ")}, "<p>from the text</p>"},
		{"unknown block alone", "", []any{map[string]any{"type": "carousel"}}, "<p><em>" + emptyText + "</em></p>"},
	} {
		res, err := Convert(slack.Message{Text: tc.text, Blocks: blocksOf(t, tc.blocks...)}, goldenResolver, Options{AuthorMapped: true})
		if err != nil {
			t.Fatal(err)
		}
		if got := res.Parts[0].HTML; got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}
