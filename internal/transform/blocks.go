package transform

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Blocks and attachments arrive as raw JSON of no guaranteed shape, so they
// are decoded into generic values and read through accessors that return
// zero values for anything missing or of the wrong type.

type object = map[string]any

func asObject(v any) object {
	o, _ := v.(object)
	return o
}

func str(o object, key string) string {
	s, _ := o[key].(string)
	return s
}

func list(o object, key string) []any {
	l, _ := o[key].([]any)
	return l
}

// number reads a JSON number, or a string holding one.
func number(o object, key string) (float64, bool) {
	switch v := o[key].(type) {
	case float64:
		return v, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil
	}
	return 0, false
}

// integer reads a number as an int clamped to lo..hi; missing is lo.
func integer(o object, key string, lo, hi int) int {
	f, ok := number(o, key)
	if !ok || f != f || f < float64(lo) {
		return lo
	}
	if f > float64(hi) {
		return hi
	}
	return int(f)
}

// decodeArray decodes raw as a JSON array. ok is false if raw is absent.
func (c *conv) decodeArray(raw json.RawMessage, what string) (items []any, ok bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, false
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		c.warn("%s are not a JSON array; ignored", what)
		return nil, false
	}
	return items, true
}

// decodeBlocks returns the message's blocks if they include at least one
// rich_text block. Without one, the text field is the better source: bots
// send section blocks alongside a complete text fallback.
func (c *conv) decodeBlocks(raw json.RawMessage) ([]any, bool) {
	items, ok := c.decodeArray(raw, "blocks")
	if !ok {
		return nil, false
	}
	for _, it := range items {
		if str(asObject(it), "type") == "rich_text" {
			return items, true
		}
	}
	return nil, false
}

// blocks renders Block Kit blocks.
func (c *conv) blocks(items []any) []*node {
	var out []*node
	for _, it := range items {
		o := asObject(it)
		if o == nil {
			c.warn("malformed block skipped")
			continue
		}
		switch t := str(o, "type"); t {
		case "rich_text":
			out = append(out, c.richText(list(o, "elements"))...)
		case "section":
			out = append(out, c.textObject(o["text"])...)
			for _, f := range list(o, "fields") {
				out = append(out, c.textObject(f)...)
			}
		case "header":
			if kids := c.plainObject(o["text"]); len(kids) > 0 {
				out = append(out, para(el("strong", kids...)))
			}
		case "context":
			for _, e := range list(o, "elements") {
				out = append(out, c.textObject(e)...)
			}
		case "image":
			if alt := cleanLine(str(o, "alt_text")); alt != "" {
				out = append(out, para(el("em", text("(image: "+alt+")"))))
			}
		case "divider":
		default:
			c.warn("unknown block type %s skipped", typeName(t))
		}
	}
	return out
}

// textObject renders a Block Kit text object: mrkdwn is parsed, plain_text
// is escaped. Objects of other types (images in a context block) show
// nothing.
func (c *conv) textObject(v any) []*node {
	o := asObject(v)
	switch str(o, "type") {
	case "mrkdwn":
		return c.mrkdwn(str(o, "text"))
	case "plain_text":
		if kids := c.plainObject(v); len(kids) > 0 {
			return []*node{para(kids...)}
		}
	}
	return nil
}

func (c *conv) plainObject(v any) []*node {
	kids := trimBreaks(textLines(cleanText(str(asObject(v), "text"))))
	if isBlank(kids) {
		return nil
	}
	return kids
}

// --- rich_text -----------------------------------------------------------------

// maxIndent is the deepest list nesting kept. Slack's editor stops well
// short of it; the cap bounds what hostile input can make us nest.
const maxIndent = 8

// richText renders the containers of a rich_text block.
func (c *conv) richText(elements []any) []*node {
	var (
		out   []*node
		stack []*node // the open list at each indent level
	)
	for _, e := range elements {
		o := asObject(e)
		if o == nil {
			c.warn("malformed rich_text element skipped")
			continue
		}
		t := str(o, "type")
		if t != "rich_text_list" {
			stack = nil
		}
		switch t {
		case "rich_text_section":
			if kids := c.inlineRun(list(o, "elements")); kids != nil {
				out = append(out, para(kids...))
			}
		case "rich_text_list":
			out, stack = c.richList(out, stack, o)
		case "rich_text_preformatted":
			code := strings.Trim(c.plainRun(list(o, "elements")), "\n")
			if strings.TrimSpace(code) != "" {
				out = append(out, el("pre", el("code", text(code))))
			}
		case "rich_text_quote":
			if kids := c.inlineRun(list(o, "elements")); kids != nil {
				out = append(out, el("blockquote", para(kids...)))
			}
		default:
			c.warn("unknown rich_text element type %s", typeName(t))
			kids := c.inlineRun(list(o, "elements"))
			if kids == nil {
				kids = c.inlineRun([]any{o})
			}
			if kids != nil {
				out = append(out, para(kids...))
			}
		}
	}
	return out
}

// richList adds one rich_text_list container. Slack writes a nested list as
// a run of sibling containers with an indent number; stack tracks the open
// <ul>/<ol> at each level so that deeper containers are placed inside the
// last item of the level above, giving properly nested HTML lists.
func (c *conv) richList(out, stack []*node, o object) (newOut, newStack []*node) {
	tag := "ul"
	if str(o, "style") == "ordered" {
		tag = "ol"
	}
	indent := integer(o, "indent", 0, maxIndent)
	offset := integer(o, "offset", 0, 1_000_000)

	if len(stack) > indent+1 {
		stack = stack[:indent+1]
	}
	if len(stack) == indent+1 {
		// Continue the open list at this level only if it is the same kind
		// and, for a numbered list, the numbering carries on.
		top := stack[indent]
		if top.tag != tag || (tag == "ol" && offset != 0 && offset != olStart(top)-1+len(top.kids)) {
			stack = stack[:indent]
		}
	}
	for len(stack) < indent+1 {
		nl := el(tag)
		if tag == "ol" && offset > 0 && len(stack) == indent {
			nl.attr = strconv.Itoa(offset + 1)
		}
		if len(stack) == 0 {
			out = append(out, nl)
		} else {
			parent := stack[len(stack)-1]
			if len(parent.kids) == 0 {
				// A list that starts indented: hang it on an empty item.
				parent.kids = append(parent.kids, el("li"))
			}
			item := parent.kids[len(parent.kids)-1]
			item.kids = append(item.kids, nl)
		}
		stack = append(stack, nl)
	}
	cur := stack[indent]
	for _, it := range list(o, "elements") {
		item := asObject(it)
		kids := c.inlineRun(list(item, "elements"))
		if _, isContainer := item["elements"]; kids == nil && item != nil && !isContainer {
			// Not a section but a bare inline element.
			kids = c.inlineRun([]any{item})
		}
		if kids != nil {
			cur.kids = append(cur.kids, el("li", kids...))
		}
	}
	return out, stack
}

// inlineRun renders inline elements, trimmed of leading and trailing line
// breaks. It returns nil if they show nothing.
func (c *conv) inlineRun(elements []any) []*node {
	var out []*node
	for _, e := range elements {
		o := asObject(e)
		if o == nil {
			c.warn("malformed rich_text element skipped")
			continue
		}
		out = append(out, c.inline(o)...)
	}
	out = trimBreaks(out)
	if isBlank(out) {
		return nil
	}
	return out
}

// inline renders one inline rich_text element.
func (c *conv) inline(o object) []*node {
	var kids []*node
	switch t := str(o, "type"); t {
	case "text":
		kids = textLines(cleanText(str(o, "text")))
	case "link":
		n, _ := c.link(str(o, "url"), str(o, "text"))
		kids = []*node{n}
	case "user":
		kids = []*node{c.userNode(str(o, "user_id"), "")}
	case "channel":
		kids = []*node{text(c.channelText(str(o, "channel_id"), ""))}
	case "date":
		ts := ""
		if f, ok := number(o, "timestamp"); ok && f == f {
			ts = strconv.FormatFloat(f, 'f', 0, 64)
		}
		kids = []*node{c.dateNode(ts, str(o, "fallback"), str(o, "url"))}
	default:
		s := c.plainInline(o)
		if s == "" {
			return nil
		}
		kids = textLines(s)
	}
	if len(kids) == 0 {
		return nil
	}
	return styled(kids, asObject(o["style"]))
}

// styled wraps kids in the tags for a rich_text style object, always nested
// in the same order: strong, em, s, then code or u. Inside code no other
// styling is applied.
func styled(kids []*node, style object) []*node {
	if style == nil {
		return kids
	}
	on := func(k string) bool { b, _ := style[k].(bool); return b }
	if on("code") {
		kids = []*node{el("code", kids...)}
	} else if on("underline") {
		kids = []*node{el("u", kids...)}
	}
	if on("strike") {
		kids = []*node{el("s", kids...)}
	}
	if on("italic") {
		kids = []*node{el("em", kids...)}
	}
	if on("bold") {
		kids = []*node{el("strong", kids...)}
	}
	return kids
}

// plainRun renders inline elements as text only, for code blocks.
func (c *conv) plainRun(elements []any) string {
	var sb strings.Builder
	for _, e := range elements {
		if o := asObject(e); o != nil {
			sb.WriteString(c.plainInline(o))
		}
	}
	return sb.String()
}

// plainInline renders one inline element as plain text. Broadcasts and user
// groups are only ever text: a historical import must not notify anyone.
func (c *conv) plainInline(o object) string {
	switch t := str(o, "type"); t {
	case "text":
		return cleanText(str(o, "text"))
	case "link":
		if label := cleanLine(str(o, "text")); label != "" {
			return label
		}
		n, _ := c.link(str(o, "url"), "")
		if n.kind == kElem {
			return n.kids[0].text
		}
		return n.text
	case "user":
		return c.userPlain(str(o, "user_id"))
	case "channel":
		return c.channelText(str(o, "channel_id"), "")
	case "emoji":
		return emojiElement(o)
	case "broadcast":
		switch r := str(o, "range"); r {
		case "here", "channel", "everyone":
			return "@" + r
		}
		return "@channel"
	case "usergroup":
		if fb := cleanLine(str(o, "fallback")); fb != "" {
			return fb
		}
		return "@group"
	case "date":
		n := c.dateNode("", str(o, "fallback"), "")
		if f, ok := number(o, "timestamp"); ok && f == f && cleanLine(str(o, "fallback")) == "" && f > -6e10 && f < 2.5e11 {
			return time.Unix(int64(f), 0).UTC().Format(dateLayout)
		}
		return n.text
	case "color":
		return cleanLine(str(o, "value"))
	case "team":
		if fb := cleanLine(str(o, "fallback")); fb != "" {
			return fb
		}
		return "@team"
	default:
		c.warn("unknown rich_text element type %s", typeName(t))
		for _, k := range [...]string{"text", "fallback", "url"} {
			if s := cleanText(str(o, k)); strings.TrimSpace(s) != "" {
				if k == "url" {
					n, _ := c.link(s, "")
					if n.kind == kElem {
						return n.kids[0].text
					}
					return n.text
				}
				return s
			}
		}
	}
	return ""
}

// emojiElement renders a rich_text emoji element: the code points Slack
// recorded when present, else the table, else ":name:" for custom emoji.
func emojiElement(o object) string {
	if e, ok := emojiFromHex(str(o, "unicode")); ok {
		return e
	}
	name := cleanLine(str(o, "name"))
	if name == "" {
		return ""
	}
	if tone := integer(o, "skin_tone", 0, 9); tone > 0 && !strings.Contains(name, "::") {
		if e, ok := emojiTone(name, tone); ok {
			return e
		}
	}
	if e, ok := emojiFor(name); ok {
		return e
	}
	return ":" + name + ":"
}

// --- legacy attachments ----------------------------------------------------------

// attachments renders legacy attachments, each as a block quote.
func (c *conv) attachments(raw json.RawMessage) []*node {
	items, ok := c.decodeArray(raw, "attachments")
	if !ok {
		return nil
	}
	var out []*node
	for _, it := range items {
		o := asObject(it)
		if o == nil {
			c.warn("malformed attachment skipped")
			continue
		}
		var inner []*node
		inner = append(inner, c.mrkdwn(str(o, "pretext"))...)
		if author := cleanLine(unescapeEntities(str(o, "author_name"))); author != "" {
			inner = append(inner, para(text(author)))
		}
		if title := cleanLine(unescapeEntities(str(o, "title"))); title != "" {
			t := text(title)
			if n, kind := c.link(str(o, "title_link"), title); kind == urlOK {
				t = n
			}
			inner = append(inner, para(el("strong", t)))
		}
		inner = append(inner, c.mrkdwn(str(o, "text"))...)
		for _, f := range list(o, "fields") {
			fo := asObject(f)
			if title := cleanLine(unescapeEntities(str(fo, "title"))); title != "" {
				inner = append(inner, para(el("strong", text(title))))
			}
			inner = append(inner, c.mrkdwn(str(fo, "value"))...)
		}
		inner = append(inner, c.mrkdwn(str(o, "footer"))...)
		if inner = prune(inner); len(inner) == 0 {
			inner = prune(c.mrkdwn(str(o, "fallback")))
		}
		if len(inner) > 0 {
			out = append(out, el("blockquote", inner...))
		}
	}
	return out
}
