package transform

import (
	"bytes"
	"encoding/json"
	"math"
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

// maxBlockDepth is how deep blocks that hold blocks (container) are
// followed. Slack allows two levels.
const maxBlockDepth = 3

// ownContent reports whether blocks carry the message itself rather than a
// decoration of its text: a rich_text, markdown, table, plan or task_card
// block, possibly inside a container. Without one, the text field is the
// better source: bots send section blocks alongside a complete text
// fallback.
func ownContent(items []any, depth int) bool {
	for _, it := range items {
		o := asObject(it)
		switch str(o, "type") {
		case "rich_text", "markdown", "table", "plan", "task_card":
			return true
		case "container":
			if depth < maxBlockDepth && ownContent(list(o, "child_blocks"), depth+1) {
				return true
			}
		}
	}
	return false
}

// blocks renders Block Kit blocks.
func (c *conv) blocks(items []any, depth int) []*node {
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
		case "markdown":
			out = append(out, c.markdown(str(o, "text"))...)
		case "table":
			out = append(out, c.tableBlock(o)...)
		case "video":
			out = append(out, c.videoBlock(o)...)
		case "file":
			// A remote file. The file itself is listed in the message's
			// files, which is where it is linked; the block adds its ID.
			if id := cleanLine(str(o, "external_id")); id != "" {
				out = append(out, para(el("em", text("(remote file: "+id+")"))))
			}
		case "card":
			out = append(out, c.cardBlock(o)...)
		case "container":
			out = append(out, c.containerBlock(o, depth)...)
		case "plan":
			if title := cleanLine(str(o, "title")); title != "" {
				out = append(out, para(el("strong", text(title))))
			}
			for _, task := range list(o, "tasks") {
				out = append(out, c.taskCard(asObject(task))...)
			}
		case "task_card":
			out = append(out, c.taskCard(o)...)
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

// --- table -------------------------------------------------------------------

// tableBlock renders a table block: rows of cells, each a raw_text or
// rich_text object, with optional per-column alignment. Slack's reference
// names a raw_number cell without giving its fields, so a number is looked
// for where the other cell types keep their content.
func (c *conv) tableBlock(o object) []*node {
	settings := list(o, "column_settings")
	align := func(col int) string {
		if col < len(settings) {
			return str(asObject(settings[col]), "align")
		}
		return ""
	}
	var rows [][]*node
	for _, r := range list(o, "rows") {
		cells, ok := r.([]any)
		if !ok {
			c.warn("malformed table row skipped")
			continue
		}
		row := make([]*node, 0, len(cells))
		for i, v := range cells {
			switch v := v.(type) {
			case string:
				// Not a cell object, but its content is plain enough.
				row = append(row, cell(align(i), trimBreaks(textLines(cleanText(v)))...))
			case float64:
				row = append(row, cell(align(i), text(formatNumber(v))))
			default:
				row = append(row, cell(align(i), c.tableCell(asObject(v))...))
			}
		}
		rows = append(rows, row)
	}
	if t := table(rows); t != nil {
		return []*node{t}
	}
	return nil
}

// tableCell renders the content of one cell as inline nodes.
func (c *conv) tableCell(o object) []*node {
	switch t := str(o, "type"); t {
	case "rich_text":
		return flatten(prune(c.richText(list(o, "elements"))))
	case "raw_text", "raw_number":
		for _, k := range [...]string{"text", "value", "number"} {
			if s, ok := o[k].(string); ok {
				return trimBreaks(textLines(cleanText(s)))
			}
			if f, ok := o[k].(float64); ok {
				return []*node{text(formatNumber(f))}
			}
		}
	default:
		if o != nil {
			c.warn("unknown table cell type %s", typeName(t))
			return trimBreaks(textLines(cleanText(str(o, "text"))))
		}
		c.warn("malformed table cell")
	}
	return nil
}

// formatNumber writes a JSON number the way it was most likely written:
// without an exponent unless it is too large or too small to read that way.
func formatNumber(f float64) string {
	if a := math.Abs(f); a != 0 && (a >= 1e15 || a < 1e-6) {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// --- blocks that show other things as text ---------------------------------------

// videoBlock renders a video block as its title, linked when the block has
// a link Taut may show, and its description. The player is not carried over.
func (c *conv) videoBlock(o object) []*node {
	title := cleanLine(str(asObject(o["title"]), "text"))
	if title == "" {
		title = cleanLine(str(o, "alt_text"))
	}
	if title == "" {
		title = "Video"
	}
	label := text(title)
	for _, k := range [...]string{"title_url", "video_url"} {
		if n, kind := c.link(str(o, k), title); kind == urlOK {
			label = n
			break
		}
	}
	line := []*node{text("🎬 "), label}
	var by []string
	for _, k := range [...]string{"author_name", "provider_name"} {
		if v := cleanLine(str(o, k)); v != "" {
			by = append(by, v)
		}
	}
	if len(by) > 0 {
		line = append(line, text(" "), el("em", text("("+strings.Join(by, ", ")+")")))
	}
	out := []*node{para(line...)}
	if kids := c.plainObject(o["description"]); len(kids) > 0 {
		out = append(out, para(kids...))
	}
	return out
}

// cardBlock renders a card's texts in reading order. Its images and
// buttons are not carried over; an image leaves its alt text.
func (c *conv) cardBlock(o object) []*node {
	var out []*node
	if alt := cleanLine(str(asObject(o["hero_image"]), "alt_text")); alt != "" {
		out = append(out, para(el("em", text("(image: "+alt+")"))))
	}
	for _, k := range [...]string{"title", "subtitle", "body", "subtext"} {
		blocks := c.textObject(o[k])
		if k == "title" {
			blocks = bold(blocks)
		}
		out = append(out, blocks...)
	}
	return out
}

// bold wraps the content of each paragraph in strong, in place of any bold
// it already holds.
func bold(blocks []*node) []*node {
	var plain func([]*node) []*node
	plain = func(kids []*node) []*node {
		out := make([]*node, 0, len(kids))
		for _, k := range kids {
			if k.kind == kElem {
				k.kids = plain(k.kids)
				if k.tag == "strong" {
					out = append(out, k.kids...)
					continue
				}
			}
			out = append(out, k)
		}
		return out
	}
	for _, b := range blocks {
		if b.kind == kElem && b.tag == "p" {
			b.kids = []*node{el("strong", plain(b.kids)...)}
		}
	}
	return blocks
}

// containerBlock renders a container: its title and subtitle, then the
// blocks it holds.
func (c *conv) containerBlock(o object, depth int) []*node {
	var out []*node
	if rt := asObject(o["rich_text_title"]); rt != nil {
		out = append(out, bold(c.richText(list(rt, "elements")))...)
	} else if kids := c.plainObject(o["title"]); len(kids) > 0 {
		out = append(out, para(el("strong", kids...)))
	}
	out = append(out, c.textObject(o["subtitle"])...)
	if depth >= maxBlockDepth {
		c.warn("blocks nested too deeply were skipped")
		return out
	}
	return append(out, c.blocks(list(o, "child_blocks"), depth+1)...)
}

// taskCard renders a task of a plan, or a task_card block: its title and
// status, then its details, its output and the sources it names.
func (c *conv) taskCard(o object) []*node {
	if o == nil {
		c.warn("malformed task skipped")
		return nil
	}
	var out []*node
	title := cleanLine(str(o, "title"))
	status := strings.ReplaceAll(cleanLine(str(o, "status")), "_", " ")
	if len(status) > 40 {
		status = strings.ToValidUTF8(status[:40], "") + "…"
	}
	switch {
	case title != "" && status != "":
		out = append(out, para(el("strong", text(title)), text(" "), el("em", text("("+status+")"))))
	case title != "":
		out = append(out, para(el("strong", text(title))))
	}
	for _, k := range [...]string{"details", "output"} {
		out = append(out, c.richText(list(asObject(o[k]), "elements"))...)
	}
	var sources []*node
	for _, s := range list(o, "sources") {
		so := asObject(s)
		if n, kind := c.link(str(so, "url"), str(so, "text")); kind != urlNone && so != nil {
			sources = append(sources, el("li", n))
		}
	}
	if len(sources) > 0 {
		out = append(out, el("ul", sources...))
	}
	return out
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
