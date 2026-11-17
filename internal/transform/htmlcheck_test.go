package transform

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// checked is what checkHTML learns about a well-formed part.
type checked struct {
	text string // visible text, entities decoded, <br> as newline
	ids  []int  // <at id> values in document order
	// files holds the file IDs of the markers, in order, each once; markers
	// counts the marker elements, so a repeated one shows as a difference.
	files   []string
	markers int
}

var markerRE = regexp.MustCompile(`^taut-file:([A-Za-z0-9_-]{1,64})$`)

var (
	tagRE  = regexp.MustCompile(`^<(/?)([a-z]+)((?: [a-z]+="[^"<>]*")*)>$`)
	attrRE = regexp.MustCompile(` ([a-z]+)="([^"<>]*)"`)

	blockTags  = map[string]bool{"p": true, "pre": true, "blockquote": true, "ul": true, "ol": true, "table": true}
	tableTags  = map[string]bool{"tr": true, "td": true}
	inlineTags = map[string]bool{"strong": true, "em": true, "s": true, "u": true, "code": true, "a": true, "at": true, "br": true}
	htmlEnts   = map[string]string{"&amp;": "&", "&lt;": "<", "&gt;": ">", "&quot;": `"`, "&#39;": "'"}
)

// decodeText checks that s holds no raw markup characters, control
// characters or entities other than the five the renderer writes, and
// returns it decoded.
func decodeText(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("invalid UTF-8")
	}
	var sb strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '&':
			found := false
			for ent, plain := range htmlEnts {
				if strings.HasPrefix(s[i:], ent) {
					sb.WriteString(plain)
					i += len(ent)
					found = true
					break
				}
			}
			if !found {
				return "", fmt.Errorf("raw or unterminated entity at %q", clip(s[i:]))
			}
			continue
		case c == '<' || c == '>' || c == '"' || c == '\'':
			return "", fmt.Errorf("raw %q in text", c)
		case (c < 0x20 && c != '\n' && c != '\t') || c == 0x7f:
			return "", fmt.Errorf("control character %#x in text", c)
		}
		r, w := utf8.DecodeRuneInString(s[i:])
		if r >= 0x80 && r < 0xa0 {
			return "", fmt.Errorf("control character %#x in text", r)
		}
		sb.WriteString(s[i : i+w])
		i += w
	}
	return sb.String(), nil
}

func clip(s string) string {
	if len(s) > 24 {
		s = strings.ToValidUTF8(s[:24], "")
	}
	return s
}

// allowedIn says whether tag may be a direct child of parent ("" is the top
// level), and whether text may be.
func allowedIn(parent, tag string) bool {
	switch parent {
	case "", "blockquote":
		return blockTags[tag]
	case "ul", "ol":
		return tag == "li"
	case "li":
		return inlineTags[tag] || tag == "ul" || tag == "ol"
	case "pre":
		return tag == "code"
	case "at":
		return false
	case "table":
		return tag == "tr"
	case "tr":
		return tag == "td"
	case "td":
		return inlineTags[tag]
	case "p":
		return inlineTags[tag] || tag == "attachment" || tag == "img"
	case "strong", "em", "s", "u", "code", "a":
		return inlineTags[tag]
	}
	return false
}

func textAllowedIn(parent string) bool {
	switch parent {
	case "", "blockquote", "ul", "ol", "pre", "attachment", "table", "tr":
		return false
	}
	return true
}

// checkHTML is a strict checker for the HTML Convert may produce: only the
// allowed tags and attributes, balanced and properly nested, a sensible
// content model, no empty elements, links only to http, https and mailto,
// and no raw markup characters in text.
func checkHTML(html string) (checked, error) {
	var (
		res   checked
		stack []string
		empty []bool // whether the open element has had content yet
		sb    strings.Builder
	)
	top := func() string {
		if len(stack) == 0 {
			return ""
		}
		return stack[len(stack)-1]
	}
	content := func() {
		if len(empty) > 0 {
			empty[len(empty)-1] = false
		}
	}
	within := func(tag string) bool {
		for _, t := range stack {
			if t == tag {
				return true
			}
		}
		return false
	}
	for i := 0; i < len(html); {
		if html[i] != '<' {
			j := strings.IndexByte(html[i:], '<')
			if j < 0 {
				j = len(html) - i
			}
			txt, err := decodeText(html[i : i+j])
			if err != nil {
				return res, err
			}
			if !textAllowedIn(top()) {
				return res, fmt.Errorf("text %q directly inside <%s>", clip(txt), top())
			}
			sb.WriteString(txt)
			content()
			i += j
			continue
		}
		j := strings.IndexByte(html[i:], '>')
		if j < 0 {
			return res, fmt.Errorf("unterminated tag at %q", clip(html[i:]))
		}
		raw := html[i : i+j+1]
		i += j + 1
		m := tagRE.FindStringSubmatch(raw)
		if m == nil {
			return res, fmt.Errorf("malformed tag %q", clip(raw))
		}
		closing, tag, attrs := m[1] == "/", m[2], attrRE.FindAllStringSubmatch(m[3], -1)
		if !blockTags[tag] && !inlineTags[tag] && !tableTags[tag] && tag != "li" && tag != "attachment" && tag != "img" {
			return res, fmt.Errorf("tag <%s> is not allowed", tag)
		}
		if closing {
			if len(attrs) > 0 || tag == "br" || tag == "img" {
				return res, fmt.Errorf("bad closing tag %q", raw)
			}
			if top() != tag {
				return res, fmt.Errorf("</%s> closes <%s>", tag, top())
			}
			if empty[len(empty)-1] != (tag == "attachment") {
				return res, fmt.Errorf("<%s> is empty, or should be and is not", tag)
			}
			if tag == "attachment" && !strings.HasSuffix(html[:i], "></attachment>") {
				return res, fmt.Errorf("<attachment> is not an adjacent open and close pair")
			}
			stack, empty = stack[:len(stack)-1], empty[:len(empty)-1]
			if blockTags[tag] || tag == "li" || tag == "tr" {
				sb.WriteByte('\n')
			}
			if tag == "td" {
				sb.WriteByte('\t')
			}
			continue
		}
		if !allowedIn(top(), tag) {
			return res, fmt.Errorf("<%s> directly inside <%s>", tag, top())
		}
		if tag == "a" && within("a") {
			return res, fmt.Errorf("nested <a>")
		}
		if within("pre") && tag != "code" {
			return res, fmt.Errorf("<%s> inside <pre>", tag)
		}
		want := map[string]int{"a": 1, "at": 1, "attachment": 1, "img": 2}[tag]
		if tag == "img" && (len(attrs) != 2 || attrs[0][1] != "src" || attrs[1][1] != "alt") {
			return res, fmt.Errorf("<img> must have exactly src and alt, in that order")
		}
		marker := func(val string) error {
			m := markerRE.FindStringSubmatch(val)
			if m == nil {
				return fmt.Errorf("<%s> does not hold a file marker: %q", tag, clip(val))
			}
			res.markers++
			for _, f := range res.files {
				if f == m[1] {
					return nil
				}
			}
			res.files = append(res.files, m[1])
			return nil
		}
		for _, a := range attrs {
			val, err := decodeText(a[2])
			if err != nil {
				return res, fmt.Errorf("attribute %s: %w", a[1], err)
			}
			switch {
			case tag == "a" && a[1] == "href":
				low := strings.ToLower(val)
				if !strings.HasPrefix(low, "http://") && !strings.HasPrefix(low, "https://") && !strings.HasPrefix(low, "mailto:") {
					return res, fmt.Errorf("href with a scheme that is not allowed: %q", clip(val))
				}
				if strings.ContainsAny(val, " \t\n") {
					return res, fmt.Errorf("white space in href")
				}
			case tag == "at" && a[1] == "id":
				id, err := strconv.Atoi(val)
				if err != nil || strconv.Itoa(id) != val {
					return res, fmt.Errorf("bad at id %q", val)
				}
				res.ids = append(res.ids, id)
			case (tag == "attachment" && a[1] == "id") || (tag == "img" && a[1] == "src"):
				if a[2] != val {
					return res, fmt.Errorf("escaped characters in a file marker")
				}
				if err := marker(val); err != nil {
					return res, err
				}
			case tag == "img" && a[1] == "alt":
			case tag == "ol" && a[1] == "start":
				if n, err := strconv.Atoi(val); err != nil || n < 1 {
					return res, fmt.Errorf("bad ol start %q", val)
				}
			case tag == "td" && a[1] == "style":
				if a[2] != "text-align:center" && a[2] != "text-align:right" {
					return res, fmt.Errorf("style %q on <td> is not allowed", clip(val))
				}
			default:
				return res, fmt.Errorf("attribute %s on <%s> is not allowed", a[1], tag)
			}
		}
		if len(attrs) != want && !((tag == "ol" || tag == "td") && len(attrs) == 1) {
			return res, fmt.Errorf("<%s> has %d attributes", tag, len(attrs))
		}
		content()
		if tag == "br" {
			sb.WriteByte('\n')
			continue
		}
		if tag == "img" {
			continue
		}
		stack, empty = append(stack, tag), append(empty, true)
	}
	if len(stack) > 0 {
		return res, fmt.Errorf("unclosed <%s>", top())
	}
	res.text = sb.String()
	return res, nil
}

// markerElemRE matches a whole marker paragraph, in either form.
var markerElemRE = regexp.MustCompile(`<p>(?:<attachment id="taut-file:[A-Za-z0-9_-]{1,64}"></attachment>|<img src="taut-file:[A-Za-z0-9_-]{1,64}" alt="[^"<>]*">)</p>`)

// secretMarkers must never appear in output: they are parts of Slack file
// URLs and of the export tokens embedded in them.
var secretMarkers = []string{"files.slack.com", "?t=", "xoxe-"}

// checkResult applies every invariant a Result must satisfy.
func checkResult(res Result, limit int) error {
	if len(res.Parts) == 0 {
		return fmt.Errorf("no parts")
	}
	filesSeen := map[string]struct{}{}
	for i, p := range res.Parts {
		if p.HTML == "" {
			return fmt.Errorf("part %d is empty", i)
		}
		if len(p.HTML) > limit {
			return fmt.Errorf("part %d has %d bytes, limit %d", i, len(p.HTML), limit)
		}
		c, err := checkHTML(p.HTML)
		if err != nil {
			return fmt.Errorf("part %d: %w", i, err)
		}
		if len(c.ids) != len(p.Mentions) {
			return fmt.Errorf("part %d: %d <at> elements, %d mentions", i, len(c.ids), len(p.Mentions))
		}
		for k, id := range c.ids {
			m := p.Mentions[k]
			if id != k || m.ID != k {
				return fmt.Errorf("part %d: mention %d has at id %d and Mention.ID %d", i, k, id, m.ID)
			}
			if m.EntraID == "" || m.Text == "" {
				return fmt.Errorf("part %d: mention %d is incomplete", i, k)
			}
			if !strings.Contains(p.HTML, `<at id="`+strconv.Itoa(k)+`">`+escaped(m.Text)+`</at>`) {
				return fmt.Errorf("part %d: mention %d text does not match its <at> element", i, k)
			}
		}
		if c.markers != len(c.files) {
			return fmt.Errorf("part %d: a file marker appears more than once", i)
		}
		if !slices.Equal(c.files, p.Files) {
			return fmt.Errorf("part %d: markers %q, Part.Files %q", i, c.files, p.Files)
		}
		for _, id := range p.Files {
			if _, dup := filesSeen[id]; dup {
				return fmt.Errorf("part %d: file %q already appeared in an earlier part", i, id)
			}
			filesSeen[id] = struct{}{}
		}
		// A marker must be alone in its paragraph, and nothing but the two
		// marker forms may contain the tag names.
		rest := markerElemRE.ReplaceAllString(p.HTML, "")
		if strings.Contains(rest, "<attachment") || strings.Contains(rest, "<img") {
			return fmt.Errorf("part %d: a marker element is not a standalone paragraph", i)
		}
		for _, s := range secretMarkers {
			if strings.Contains(p.HTML, s) {
				return fmt.Errorf("part %d contains %q", i, s)
			}
		}
		if err := checkNative(res, i, limit); err != nil {
			return fmt.Errorf("part %d: %w", i, err)
		}
	}
	for _, f := range res.Files {
		for _, s := range secretMarkers {
			if strings.Contains(f.ExternalURL+f.Name+f.Title, s) {
				return fmt.Errorf("file ref contains %q", s)
			}
		}
	}
	return nil
}

// checkNative checks what a part carries under the native reactions
// policy: reactions only on the last part, each a Unicode emoji from a
// mapped person, no person and emoji twice, at most MaxNativeReactions, and
// a fallback body that is itself a valid part with the same mentions and
// files.
func checkNative(res Result, i, limit int) error {
	p := res.Parts[i]
	if (len(p.Reactions) > 0) != (p.FooterHTML != "") {
		return fmt.Errorf("%d reactions but fallback %q", len(p.Reactions), clip(p.FooterHTML))
	}
	if len(p.Reactions) == 0 {
		return nil
	}
	if i != len(res.Parts)-1 {
		return fmt.Errorf("reactions on a part that is not the last")
	}
	if len(p.Reactions) > MaxNativeReactions {
		return fmt.Errorf("%d reactions, limit %d", len(p.Reactions), MaxNativeReactions)
	}
	seen := map[NativeReaction]bool{}
	for _, r := range p.Reactions {
		if r.EntraID == "" || r.Emoji == "" || strings.HasPrefix(r.Emoji, ":") || r.Emoji[0] < utf8.RuneSelf && !strings.ContainsRune(r.Emoji, 0x20E3) {
			return fmt.Errorf("reaction %q by %q is not a Unicode emoji from a mapped person", r.Emoji, r.EntraID)
		}
		if seen[r] {
			return fmt.Errorf("reaction %q by %q appears twice", r.Emoji, r.EntraID)
		}
		seen[r] = true
	}
	if len(p.FooterHTML) > limit {
		return fmt.Errorf("fallback has %d bytes, limit %d", len(p.FooterHTML), limit)
	}
	c, err := checkHTML(p.FooterHTML)
	if err != nil {
		return fmt.Errorf("fallback: %w", err)
	}
	if len(c.ids) != len(p.Mentions) || !slices.Equal(c.files, p.Files) {
		return fmt.Errorf("fallback has other mentions or files than the part")
	}
	return nil
}

func escaped(s string) string {
	var sb strings.Builder
	escape(&sb, s)
	return sb.String()
}
