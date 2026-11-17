package transform

import (
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The Markdown parser handles the `markdown` block, whose text is standard
// Markdown rather than Slack's mrkdwn. It covers what Slack documents for
// the block: headings, bold, italic, strikethrough, links, images (shown as
// links), ordered, unordered and task lists, inline code, code blocks, block
// quotes, dividers, tables and backslash escapes.
//
// It produces the same node tree as the other parsers, so nothing in the
// source can become markup: raw HTML in Markdown is text here, and links go
// through the same URL check as everywhere else. Where the output's content
// model is narrower than Markdown's, content is kept and structure is
// simplified: every heading level is a bold paragraph, a list item holds
// inline content and nested lists, and a block that cannot sit in a list
// item (a code block, a quote, a table) ends the list, follows it, and the
// list carries on after it. A line break inside a paragraph is kept as a
// line break.

// maxMdDepth bounds the nesting of quotes and lists, and of emphasis and
// link labels, that the parser follows. Deeper content is kept as text.
const maxMdDepth = 8

// maxMdBytes is the largest Markdown text that is parsed. Slack caps the
// Markdown of a message at 12,000 characters; anything far beyond that is
// not something Slack wrote, and is shown as plain text.
const maxMdBytes = 64 << 10

// maxMdLabel bounds how far the parser looks for the end of a link's label
// or destination.
const maxMdLabel = 1000

// markdown parses standard Markdown into blocks.
func (c *conv) markdown(src string) []*node {
	src = cleanText(src)
	if strings.TrimSpace(src) == "" {
		return nil
	}
	if len(src) > maxMdBytes {
		c.warn("a markdown block is too large to parse and was written as plain text")
		return c.mdParse(strings.Split(src, "\n"), maxMdDepth+1)
	}
	src = strings.ReplaceAll(src, "\t", "    ")
	return c.mdParse(strings.Split(src, "\n"), 0)
}

// indentOf returns the number of leading spaces of a line.
func indentOf(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}

// mdFence reports whether line opens a fenced code block and returns the
// fence character and its length.
func mdFence(line string) (ch byte, n int, ok bool) {
	s := strings.TrimLeft(line, " ")
	if len(line)-len(s) > 3 || len(s) < 3 || (s[0] != '`' && s[0] != '~') {
		return 0, 0, false
	}
	for n < len(s) && s[n] == s[0] {
		n++
	}
	if n < 3 || (s[0] == '`' && strings.Contains(s[n:], "`")) {
		return 0, 0, false
	}
	return s[0], n, true
}

// mdHeading returns the text of an ATX heading line ("## Title").
func mdHeading(line string) (string, bool) {
	s := strings.TrimLeft(line, " ")
	if len(line)-len(s) > 3 {
		return "", false
	}
	n := 0
	for n < len(s) && s[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || (n < len(s) && s[n] != ' ') {
		return "", false
	}
	s = strings.TrimSpace(s[n:])
	// A closing run of #s is decoration.
	if t := strings.TrimRight(s, "#"); t == "" || strings.HasSuffix(t, " ") {
		s = strings.TrimSpace(t)
	}
	return s, true
}

// mdRule reports whether line is a thematic break: three or more of the
// same one of - * _, with nothing but spaces between them.
func mdRule(line string) bool {
	s := strings.TrimLeft(line, " ")
	if len(line)-len(s) > 3 || s == "" {
		return false
	}
	ch, n := s[0], 0
	if ch != '-' && ch != '*' && ch != '_' {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ch:
			n++
		case ' ':
		default:
			return false
		}
	}
	return n >= 3
}

// mdSetext reports whether line underlines a paragraph to make it a
// heading: a run of = or of -.
func mdSetext(line string) bool {
	s := strings.TrimSpace(line)
	if s == "" || indentOf(line) > 3 || (s[0] != '=' && s[0] != '-') {
		return false
	}
	return strings.Trim(s, string(s[0])) == ""
}

// mdMarker describes a list item's first line.
type mdMarker struct {
	ordered bool
	delim   byte // the bullet character, or '.' or ')' after a number
	number  int
	indent  int // column of the marker
	content int // column where the item's content starts
}

// mdListMarker parses the start of a list item.
func mdListMarker(line string) (mdMarker, bool) {
	m := mdMarker{indent: indentOf(line)}
	s := line[m.indent:]
	i := 0
	switch {
	case s == "":
		return m, false
	case s[0] == '-' || s[0] == '+' || s[0] == '*':
		m.delim, i = s[0], 1
	default:
		for i < len(s) && i < 9 && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == 0 || i >= len(s) || (s[i] != '.' && s[i] != ')') {
			return m, false
		}
		m.ordered, m.delim = true, s[i]
		m.number, _ = strconv.Atoi(s[:i])
		i++
	}
	// A marker with nothing after it is kept as the text it is, not turned
	// into an empty item that would show nothing.
	if strings.TrimSpace(s[i:]) == "" || s[i] != ' ' {
		return m, false
	}
	spaces := indentOf(s[i:])
	if spaces > 4 {
		// Content that is itself indented code: it starts one column after
		// the marker.
		spaces = 1
	}
	m.content = m.indent + i + spaces
	return m, true
}

// sameList reports whether an item marked b continues the list a began.
func (a mdMarker) sameList(b mdMarker) bool {
	return a.ordered == b.ordered && a.delim == b.delim
}

// mdTableRow splits a table row into its cells. ok is false if the line
// holds no cell separator.
func mdTableRow(line string) (cells []string, ok bool) {
	s := strings.TrimSpace(line)
	if !strings.Contains(s, "|") {
		return nil, false
	}
	s = strings.TrimPrefix(s, "|")
	if strings.HasSuffix(s, "|") && !strings.HasSuffix(s, `\|`) {
		s = s[:len(s)-1]
	}
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\\' && i+1 < len(s) && s[i+1] == '|':
			sb.WriteByte('|')
			i++
		case s[i] == '|':
			cells = append(cells, strings.TrimSpace(sb.String()))
			sb.Reset()
		default:
			sb.WriteByte(s[i])
		}
	}
	return append(cells, strings.TrimSpace(sb.String())), true
}

// mdTableAligns parses a table's delimiter row ("| :-- | --: |") into the
// alignment of each column. ok is false if the line is not one.
func mdTableAligns(line string) (aligns []string, ok bool) {
	cells, ok := mdTableRow(line)
	if !ok {
		return nil, false
	}
	for _, c := range cells {
		left, right := strings.HasPrefix(c, ":"), strings.HasSuffix(c, ":")
		if dashes := strings.Trim(c, ":"); dashes == "" || strings.Trim(dashes, "-") != "" {
			return nil, false
		}
		switch {
		case left && right:
			aligns = append(aligns, "center")
		case right:
			aligns = append(aligns, "right")
		default:
			aligns = append(aligns, "")
		}
	}
	return aligns, true
}

// mdParse parses lines into blocks.
func (c *conv) mdParse(lines []string, depth int) []*node {
	var (
		out  []*node
		para []string // lines of the paragraph being collected
	)
	flush := func() {
		if len(para) > 0 {
			if kids := c.mdInline(strings.Join(para, "\n"), depth); len(kids) > 0 {
				out = append(out, el("p", kids...))
			}
			para = nil
		}
	}
	if depth > maxMdDepth {
		// Too deep to follow: keep the text, a paragraph per run of lines.
		for _, ln := range append(lines[:len(lines):len(lines)], "") {
			if ln = strings.TrimSpace(ln); ln != "" {
				para = append(para, ln)
			} else if len(para) > 0 {
				out = append(out, el("p", textLines(strings.Join(para, "\n"))...))
				para = nil
			}
		}
		return out
	}

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if ch, n, ok := mdFence(line); ok {
			flush()
			strip := indentOf(line)
			var code []string
			i++
			for ; i < len(lines); i++ {
				if c2, n2, ok := mdFence(lines[i]); ok && c2 == ch && n2 >= n && strings.TrimSpace(lines[i]) == strings.Repeat(string(ch), n2) {
					break
				}
				code = append(code, lines[i][min(strip, indentOf(lines[i])):])
			}
			if body := strings.Join(code, "\n"); strings.TrimSpace(body) != "" {
				out = append(out, el("pre", el("code", text(body))))
			}
			continue
		}
		if title, ok := mdHeading(line); ok {
			flush()
			if kids := c.mdInline(title, depth); len(kids) > 0 {
				out = append(out, el("p", el("strong", kids...)))
			}
			continue
		}
		if len(para) > 0 && mdSetext(line) {
			kids := c.mdInline(strings.Join(para, "\n"), depth)
			para = nil
			if len(kids) > 0 {
				out = append(out, el("p", el("strong", kids...)))
			}
			continue
		}
		if mdRule(line) {
			flush()
			continue
		}
		if s := strings.TrimLeft(line, " "); len(line)-len(s) <= 3 && strings.HasPrefix(s, ">") {
			flush()
			var quoted []string
			for ; i < len(lines); i++ {
				q := strings.TrimLeft(lines[i], " ")
				if len(lines[i])-len(q) > 3 || !strings.HasPrefix(q, ">") {
					break
				}
				quoted = append(quoted, strings.TrimPrefix(q[1:], " "))
			}
			i--
			if inner := c.mdParse(quoted, depth+1); len(inner) > 0 {
				out = append(out, el("blockquote", inner...))
			}
			continue
		}
		if m, ok := mdListMarker(line); ok && m.indent <= 3 && (len(para) == 0 || !m.ordered || m.number == 1) {
			flush()
			var next int
			out, next = c.mdList(out, lines, i, m, depth)
			i = next - 1
			continue
		}
		if len(para) == 0 && i+1 < len(lines) {
			if head, ok := mdTableRow(line); ok {
				if aligns, ok := mdTableAligns(lines[i+1]); ok && len(aligns) == len(head) {
					rows := [][]*node{c.mdTableCells(head, aligns, true, depth)}
					for i += 2; i < len(lines); i++ {
						cells, ok := mdTableRow(lines[i])
						if !ok || strings.TrimSpace(lines[i]) == "" {
							break
						}
						rows = append(rows, c.mdTableCells(cells, aligns, false, depth))
					}
					i--
					if t := table(rows); t != nil {
						out = append(out, t)
					}
					continue
				}
			}
		}
		if len(para) == 0 && indentOf(line) >= 4 {
			var code []string
			for ; i < len(lines); i++ {
				if strings.TrimSpace(lines[i]) != "" && indentOf(lines[i]) < 4 {
					break
				}
				code = append(code, lines[i][min(4, len(lines[i])):])
			}
			i--
			if body := strings.Trim(strings.Join(code, "\n"), "\n"); strings.TrimSpace(body) != "" {
				out = append(out, el("pre", el("code", text(body))))
			}
			continue
		}
		para = append(para, strings.TrimSpace(line))
	}
	flush()
	return out
}

// mdTableCells renders one table row.
func (c *conv) mdTableCells(cells, aligns []string, header bool, depth int) []*node {
	row := make([]*node, 0, len(cells))
	for i, s := range cells {
		align := ""
		if i < len(aligns) {
			align = aligns[i]
		}
		kids := c.mdInline(s, depth)
		if header && len(kids) > 0 {
			kids = []*node{el("strong", kids...)}
		}
		row = append(row, cell(align, kids...))
	}
	return row
}

// mdList parses the list that starts at lines[start], whose first item has
// marker first, and appends it to out. It returns the index of the first
// line after the list.
//
// An item's content is its first line after the marker plus the following
// lines indented to that content, parsed as blocks. Paragraphs and nested
// lists go into the item. Any other block ends the list there and follows
// it, with the rest of that item, and the next item starts a new list that
// keeps the numbering.
func (c *conv) mdList(out []*node, lines []string, start int, first mdMarker, depth int) ([]*node, int) {
	tag := "ul"
	if first.ordered {
		tag = "ol"
	}
	var cur *node // the list being built, or nil after it was interrupted
	number := first.number
	open := func() *node {
		if cur == nil {
			cur = el(tag)
			if first.ordered && number > 1 {
				cur.attr = strconv.Itoa(number)
			}
			out = append(out, cur)
		}
		return cur
	}

	i := start
	for i < len(lines) {
		m, ok := mdListMarker(lines[i])
		if !ok || m.indent > first.indent+3 || m.indent < first.indent || !first.sameList(m) || mdRule(lines[i]) {
			break
		}
		// Collect the item: the rest of its first line, then every line
		// that is blank or indented to its content. A blank line counts
		// only if indented content follows it.
		item := []string{lines[i][min(m.content, len(lines[i])):]}
		i++
		for i < len(lines) {
			if strings.TrimSpace(lines[i]) == "" {
				j := i
				for j < len(lines) && strings.TrimSpace(lines[j]) == "" {
					j++
				}
				if j == len(lines) || indentOf(lines[j]) < m.content {
					break
				}
				item = append(item, "")
				i = j
				continue
			}
			if indentOf(lines[i]) < m.content {
				// A line that is not indented continues the item's
				// paragraph unless it starts something else.
				if _, isItem := mdListMarker(lines[i]); isItem || indentOf(lines[i]) <= first.indent && mdStartsBlock(lines[i]) || len(item) > 0 && item[len(item)-1] == "" {
					break
				}
				item = append(item, strings.TrimLeft(lines[i], " "))
				i++
				continue
			}
			item = append(item, lines[i][m.content:])
			i++
		}

		if box, rest, ok := mdTask(item[0]); ok {
			item[0] = box + rest
		}
		li := el("li")
		lifted := false
		for _, b := range c.mdParse(item, depth+1) {
			switch {
			case lifted:
				out = append(out, b)
			case b.kind == kElem && b.tag == "p":
				if len(li.kids) > 0 {
					li.kids = append(li.kids, br())
				}
				li.kids = append(li.kids, b.kids...)
			case b.kind == kElem && (b.tag == "ul" || b.tag == "ol"):
				li.kids = append(li.kids, b)
			default:
				if len(li.kids) > 0 {
					open().kids = append(open().kids, li)
					li = el("li")
				}
				out = append(out, b)
				cur, lifted = nil, true
			}
		}
		if len(li.kids) > 0 {
			open().kids = append(open().kids, li)
		}
		number++
		// Blank lines between items do not end the list.
		j := i
		for j < len(lines) && strings.TrimSpace(lines[j]) == "" {
			j++
		}
		if j < len(lines) && j > i {
			if n, ok := mdListMarker(lines[j]); ok && n.indent >= first.indent && n.indent <= first.indent+3 && first.sameList(n) && !mdRule(lines[j]) {
				i = j
			}
		}
	}
	return out, i
}

// mdStartsBlock reports whether a line begins a block that interrupts a
// paragraph: a heading, a fence, a quote or a divider.
func mdStartsBlock(line string) bool {
	if _, ok := mdHeading(line); ok {
		return true
	}
	if _, _, ok := mdFence(line); ok {
		return true
	}
	return mdRule(line) || strings.HasPrefix(strings.TrimLeft(line, " "), ">")
}

// mdTask recognises a task list item, "[ ] text" or "[x] text", and returns
// the box to show for it.
func mdTask(s string) (box, rest string, ok bool) {
	if len(s) < 4 || s[0] != '[' || s[2] != ']' || s[3] != ' ' {
		return "", s, false
	}
	switch s[1] {
	case ' ':
		return "☐ ", s[4:], true
	case 'x', 'X':
		return "☑ ", s[4:], true
	}
	return "", s, false
}

// --- inline ------------------------------------------------------------------

// mdPunct reports whether r is punctuation for the purposes of emphasis.
func mdPunct(r rune) bool { return unicode.IsPunct(r) || unicode.IsSymbol(r) }

// mdInline parses the inline content of a paragraph, heading or cell.
//
// Like the mrkdwn parser it works in two steps. A scan turns the text into
// units: characters, and atoms (code spans, links, escaped characters) that
// later steps treat as opaque. Emphasis is then matched over the units, so
// a marker that came out of an escape or sits inside code never formats
// anything.
func (c *conv) mdInline(s string, depth int) []*node {
	us := c.mdUnits(s, depth, true)
	return trimBreaks(mdEmphasis(us))
}

// mdUnits scans inline Markdown into units. With links off, link syntax is
// kept as text: a link's label cannot hold another link.
func (c *conv) mdUnits(s string, depth int, links bool) []unit {
	us := make([]unit, 0, len(s))
	atom := func(n *node) { us = append(us, unit{n: n}) }
	// noCode[n] says no run of exactly n backticks lies ahead, so an
	// unclosed code span is looked for once, not once per backtick.
	var noCode map[int]bool
	for i := 0; i < len(s); {
		switch ch := s[i]; {
		case ch == '\\' && i+1 < len(s) && s[i+1] == '\n':
			i++ // a backslash before a line end is a hard break; the newline does that already
		case ch == '\\' && i+1 < len(s) && s[i+1] < utf8.RuneSelf && mdPunct(rune(s[i+1])):
			atom(text(s[i+1 : i+2]))
			i += 2
		case ch == '\n':
			// Spaces around a line break are not content.
			for len(us) > 0 && us[len(us)-1].n == nil && us[len(us)-1].r == ' ' {
				us = us[:len(us)-1]
			}
			atom(br())
			i++
			for i < len(s) && s[i] == ' ' {
				i++
			}
		case ch == '`':
			n := 1
			for i+n < len(s) && s[i+n] == '`' {
				n++
			}
			end := -1
			if !noCode[n] {
				if end = mdCodeEnd(s[i+n:], n); end < 0 {
					if noCode == nil {
						noCode = map[int]bool{}
					}
					noCode[n] = true
				}
			}
			if end >= 0 {
				code := strings.ReplaceAll(s[i+n:i+n+end], "\n", " ")
				if len(code) > 2 && code[0] == ' ' && code[len(code)-1] == ' ' && strings.TrimSpace(code) != "" {
					code = code[1 : len(code)-1]
				}
				if code != "" {
					atom(el("code", text(code)))
				}
				i += 2*n + end
				continue
			}
			for range n {
				us = append(us, unit{r: '`'})
			}
			i += n
		case ch == '<':
			// An autolink, or one of Slack's own references.
			if j := strings.IndexAny(s[i+1:], "<>\n"); j > 0 && s[i+1+j] == '>' {
				if n := c.mdToken(s[i+1 : i+1+j]); n != nil && (links || !(n.kind == kElem && n.tag == "a")) {
					if n.kind == kAt && !links {
						// A link's label holds no mention.
						n = text("@" + n.text)
					}
					atom(n)
					i += j + 2
					continue
				}
			}
			us = append(us, unit{r: '<'})
			i++
		case ch == '&':
			matched := false
			for _, e := range entities {
				if strings.HasPrefix(s[i:], e.name) {
					us = append(us, unit{r: e.r})
					i += len(e.name)
					matched = true
					break
				}
			}
			if !matched {
				us = append(us, unit{r: '&'})
				i++
			}
		case ch == ':':
			if emoji, n := matchShortcode(s[i:]); n > 0 {
				atom(text(emoji))
				i += n
				continue
			}
			us = append(us, unit{r: ':'})
			i++
		case ch == '!' && i+1 < len(s) && s[i+1] == '[', ch == '[':
			image := ch == '!'
			open := i
			if image {
				open++
			}
			label, dest, n, ok := mdLink(s[open:])
			if !ok || depth >= maxMdDepth {
				us = append(us, unit{r: rune(ch)})
				i++
				continue
			}
			i = open + n
			if !links {
				// Inside a label: keep the inner label's content only.
				us = append(us, c.mdUnits(label, depth+1, false)...)
				continue
			}
			for _, n := range c.mdLinkNodes(label, dest, image, depth) {
				atom(n)
			}
		case links && (ch == 'h' || ch == 'H') && (i == 0 || !isWordByte(s[i-1])):
			if n := mdBareURL(s[i:]); n > 0 {
				node, _ := c.link(s[i:i+n], "")
				atom(node)
				i += n
				continue
			}
			us = append(us, unit{r: rune(ch)})
			i++
		default:
			r, w := utf8.DecodeRuneInString(s[i:])
			us = append(us, unit{r: r})
			i += w
		}
	}
	return us
}

func isWordByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b >= utf8.RuneSelf
}

// mdCodeEnd returns the offset in s of the run of exactly n backticks that
// closes a code span, or -1.
func mdCodeEnd(s string, n int) int {
	for i := 0; i < len(s); {
		if s[i] != '`' {
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] == '`' {
			j++
		}
		if j-i == n {
			return i
		}
		i = j
	}
	return -1
}

// mdLink parses "[label](destination)" at the start of s, which begins with
// '['. It returns the label, the destination and the bytes consumed. A
// title after the destination is dropped.
func mdLink(s string) (label, dest string, n int, ok bool) {
	level, end := 0, -1
scan:
	for i := 0; i < len(s) && i < maxMdLabel; i++ {
		switch s[i] {
		case '\\':
			i++
		case '[':
			level++
		case ']':
			level--
			if level == 0 {
				end = i
				break scan
			}
		}
	}
	if end < 0 || end+1 >= len(s) || s[end+1] != '(' {
		return "", "", 0, false
	}
	label = s[1:end]
	rest := s[end+2:]
	level = 1
	closeAt := -1
find:
	for i := 0; i < len(rest) && i < maxMdLabel; i++ {
		switch rest[i] {
		case '\\':
			i++
		case '\n':
			break find
		case '(':
			level++
		case ')':
			level--
			if level == 0 {
				closeAt = i
				break find
			}
		}
	}
	if closeAt < 0 {
		return "", "", 0, false
	}
	dest = strings.TrimSpace(rest[:closeAt])
	if strings.HasPrefix(dest, "<") {
		if gt := strings.IndexByte(dest, '>'); gt > 0 {
			dest = dest[1:gt]
		}
	} else if sp := strings.IndexAny(dest, " \n"); sp >= 0 {
		dest = dest[:sp] // what follows is the title
	}
	var sb strings.Builder
	for i := 0; i < len(dest); i++ {
		if dest[i] == '\\' && i+1 < len(dest) && mdPunct(rune(dest[i+1])) {
			i++
		}
		sb.WriteByte(dest[i])
	}
	return label, sb.String(), end + 2 + closeAt + 1, true
}

// mdLinkNodes renders a link or an image, which is shown as a link. The
// label keeps its formatting. A destination Taut does not link to is shown
// as text after the label, and a Slack file URL is not shown at all.
func (c *conv) mdLinkNodes(label, dest string, image bool, depth int) []*node {
	kids := trimBreaks(mdEmphasis(c.mdUnits(label, depth+1, false)))
	plain := plainOf(kids)
	if _, k := checkURL(plain); k == urlSlackFile {
		kids, plain = []*node{text(slackFileLinkText)}, slackFileLinkText
	}
	href, kind := checkURL(dest)
	shown := cleanLine(dest)
	if isBlank(kids) {
		switch {
		case kind == urlSlackFile:
			kids = []*node{text(slackFileLinkText)}
		case image:
			kids = []*node{text("image")}
		default:
			kids = []*node{text(strings.TrimPrefix(shown, "mailto:"))}
		}
		plain = plainOf(kids)
	}
	switch kind {
	case urlOK:
		return []*node{anchor(href, kids...)}
	case urlSlackFile:
		return kids
	}
	if shown != "" && shown != plain {
		kids = append(kids, text(" ("+shown+")"))
	}
	return kids
}

// plainOf returns the text under nodes.
func plainOf(nodes []*node) string {
	var sb strings.Builder
	var walk func([]*node)
	walk = func(ns []*node) {
		for _, n := range ns {
			switch n.kind {
			case kText, kAt:
				sb.WriteString(n.text)
			case kBr:
				sb.WriteByte(' ')
			}
			walk(n.kids)
		}
	}
	walk(nodes)
	return sb.String()
}

// mdBareURL returns the length of the bare http or https URL at the start
// of s, or 0. Punctuation that ends a sentence, and a closing parenthesis
// with no opening one inside the URL, are left out.
func mdBareURL(s string) int {
	low := strings.ToLower(s[:min(len(s), 8)])
	if !strings.HasPrefix(low, "http://") && !strings.HasPrefix(low, "https://") {
		return 0
	}
	n := 0
	for n < len(s) && s[n] > ' ' && s[n] != '<' && s[n] != '>' && s[n] != '`' && s[n] != '|' && s[n] != '*' && s[n] != '[' && s[n] != ']' {
		n++
	}
	for n > 0 {
		last := s[n-1]
		if strings.IndexByte(".,;:!?'\"_~", last) >= 0 ||
			(last == ')' && strings.Count(s[:n], ")") > strings.Count(s[:n], "(")) {
			n--
			continue
		}
		break
	}
	if n <= strings.Index(low, "//")+2 {
		return 0
	}
	return n
}

// --- emphasis ----------------------------------------------------------------

// mdRun is a run of one emphasis character.
type mdRun struct {
	at, n         int // position in the units, and length
	ch            rune
	opens, closes bool
}

var mdTags = map[rune][]string{
	'*': {"em", "strong"},
	'_': {"em", "strong"},
	'~': {"s", "s"},
}

// mdEmphasis applies *, _ and ~ emphasis to units and returns the nodes.
//
// A run of one to three markers opens emphasis when it is followed by
// something other than white space, and closes it when it follows something
// other than white space; next to punctuation the other side must be white
// space or punctuation, and an underscore inside a word does neither. A run
// pairs with the nearest later run of the same character and length. Runs
// that pair with nothing are text.
func mdEmphasis(us []unit) []*node {
	isSpace := func(i int) bool {
		return i < 0 || i >= len(us) || (us[i].n == nil && unicode.IsSpace(us[i].r)) || (us[i].n != nil && us[i].n.kind == kBr)
	}
	isPunct := func(i int) bool {
		return i >= 0 && i < len(us) && (us[i].n != nil && us[i].n.kind != kBr || us[i].n == nil && mdPunct(us[i].r))
	}
	var runs []mdRun
	closers := map[[2]int][]int{} // (character, length) → indexes into runs, ascending
	for i := 0; i < len(us); {
		if us[i].n != nil || mdTags[us[i].r] == nil {
			i++
			continue
		}
		j := i
		for j < len(us) && us[j].n == nil && us[j].r == us[i].r {
			j++
		}
		r := mdRun{at: i, n: j - i, ch: us[i].r}
		left := !isSpace(j) && (!isPunct(j) || isSpace(i-1) || isPunct(i-1))
		right := !isSpace(i-1) && (!isPunct(i-1) || isSpace(j) || isPunct(j))
		maxLen := 3
		if r.ch == '~' {
			maxLen = 2
		}
		if r.n <= maxLen {
			r.opens, r.closes = left, right
			if r.ch == '_' {
				r.opens, r.closes = left && (!right || isPunct(i-1)), right && (!left || isPunct(j))
			}
		}
		if r.closes {
			key := [2]int{int(r.ch), r.n}
			closers[key] = append(closers[key], len(runs))
		}
		runs = append(runs, r)
		i = j
	}

	// span formats units lo..hi, which hold runs first..last-1.
	var span func(lo, hi, first, last, depth int) []*node
	span = func(lo, hi, first, last, depth int) []*node {
		var (
			out []*node
			sb  strings.Builder
		)
		flush := func() {
			if sb.Len() > 0 {
				out = append(out, text(sb.String()))
				sb.Reset()
			}
		}
		k := first
		for i := lo; i < hi; {
			if us[i].n != nil {
				flush()
				out = append(out, us[i].n)
				i++
				continue
			}
			if k < last && runs[k].at == i {
				r := runs[k]
				if r.opens && depth < maxMdDepth {
					cl := closers[[2]int{int(r.ch), r.n}]
					if p := sort.SearchInts(cl, k+1); p < len(cl) && cl[p] < last && runs[cl[p]].at > r.at+r.n {
						end := cl[p]
						inner := span(r.at+r.n, runs[end].at, k+1, end, depth+1)
						if len(inner) > 0 {
							flush()
							tags := mdTags[r.ch]
							switch r.n {
							case 1:
								out = append(out, el(tags[0], inner...))
							case 2:
								out = append(out, el(tags[1], inner...))
							default:
								out = append(out, el(tags[1], el(tags[0], inner...)))
							}
							i, k = runs[end].at+runs[end].n, end+1
							continue
						}
					}
				}
				for range r.n {
					sb.WriteRune(r.ch)
				}
				i += r.n
				k++
				continue
			}
			sb.WriteRune(us[i].r)
			i++
		}
		flush()
		return out
	}
	return span(0, len(us), 0, len(runs), 0)
}
