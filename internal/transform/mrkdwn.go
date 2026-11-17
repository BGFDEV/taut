package transform

import (
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The mrkdwn parser follows the order Slack documents for its legacy text
// format:
//
//  1. Code (``` blocks and ` spans) is set aside; its content is literal.
//  2. <…> sequences are resolved: mentions, channels, commands and links.
//  3. The entities &amp; &lt; &gt; are unescaped (only those three).
//  4. Lines are grouped into quotes and paragraphs.
//  5. *bold*, _italic_ and ~strike~ are applied within each line.
//
// Steps 1 to 3 happen in one left-to-right scan that produces units: either
// one character of text, or an atom, a finished node that later steps treat
// as opaque. Because angle brackets and markers that come out of an entity
// or an atom are never scanned again, text cannot smuggle in markup.

// unit is one character of text or one atom.
type unit struct {
	r     rune
	n     *node // an atom when non-nil
	block bool  // the atom is a block (a code block), not inline content
}

// dateLayout is how a date with no fallback text is written.
const dateLayout = "2 Jan 2006 15:04 UTC"

// mrkdwn parses Slack mrkdwn into blocks.
func (c *conv) mrkdwn(s string) []*node {
	s = cleanText(s)
	if strings.TrimSpace(s) == "" {
		return nil
	}
	units := c.tokenize(s)
	var lines [][]unit
	start := 0
	for i, u := range units {
		if u.n == nil && u.r == '\n' {
			lines = append(lines, units[start:i])
			start = i + 1
		}
	}
	lines = append(lines, units[start:])
	return c.mdBlocks(lines, true)
}

var entities = [...]struct {
	name string
	r    rune
}{{"&amp;", '&'}, {"&lt;", '<'}, {"&gt;", '>'}}

// unescapeEntities reverses Slack's escaping of & < >.
func unescapeEntities(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	var sb strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '&' {
			matched := false
			for _, e := range entities {
				if strings.HasPrefix(s[i:], e.name) {
					sb.WriteRune(e.r)
					i += len(e.name)
					matched = true
					break
				}
			}
			if matched {
				continue
			}
		}
		sb.WriteByte(s[i])
		i++
	}
	return sb.String()
}

// tokenize scans cleaned mrkdwn into units.
func (c *conv) tokenize(s string) []unit {
	us := make([]unit, 0, len(s))
	noFence := false // no closing ``` exists from here on
	for i := 0; i < len(s); {
		switch s[i] {
		case '`':
			if strings.HasPrefix(s[i:], "```") {
				if !noFence {
					j := strings.Index(s[i+3:], "```")
					if j < 0 {
						noFence = true
					} else if body := s[i+3 : i+3+j]; strings.TrimSpace(body) != "" {
						body = unescapeEntities(body)
						body = strings.TrimPrefix(body, "\n")
						body = strings.TrimSuffix(body, "\n")
						us = append(us, unit{n: el("pre", el("code", text(body))), block: true})
						i += j + 6
						continue
					}
				}
				us = append(us, unit{r: '`'}, unit{r: '`'}, unit{r: '`'})
				i += 3
				continue
			}
			if strings.HasPrefix(s[i:], "``") {
				// A doubled backtick pairs with the next doubled backtick on
				// the line, so that ``code`` does not throw later spans off.
				rest := s[i+2:]
				if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
					rest = rest[:nl]
				}
				if j := strings.Index(rest, "``"); j > 0 {
					us = append(us, unit{n: el("code", text(unescapeEntities(rest[:j])))})
					i += j + 4
					continue
				}
				us = append(us, unit{r: '`'}, unit{r: '`'})
				i += 2
				continue
			}
			if j := strings.IndexAny(s[i+1:], "`\n"); j > 0 && s[i+1+j] == '`' {
				us = append(us, unit{n: el("code", text(unescapeEntities(s[i+1:i+1+j])))})
				i += j + 2
				continue
			}
			us = append(us, unit{r: '`'})
			i++
		case '<':
			if j := strings.IndexAny(s[i+1:], "<>\n"); j > 0 && s[i+1+j] == '>' {
				if n := c.mdToken(s[i+1 : i+1+j]); n != nil {
					us = append(us, unit{n: n})
					i += j + 2
					continue
				}
			}
			us = append(us, unit{r: '<'})
			i++
		case '&':
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
		case ':':
			if emoji, n := matchShortcode(s[i:]); n > 0 {
				us = append(us, unit{n: text(emoji)})
				i += n
				continue
			}
			us = append(us, unit{r: ':'})
			i++
		default:
			r, w := utf8.DecodeRuneInString(s[i:])
			us = append(us, unit{r: r})
			i += w
		}
	}
	return us
}

// mdToken resolves the inside of a <…> sequence. It returns nil if the
// sequence is not one Slack writes, in which case the "<" is literal text.
func (c *conv) mdToken(tok string) *node {
	switch tok[0] {
	case '@':
		id, label, _ := strings.Cut(tok[1:], "|")
		return c.userNode(unescapeEntities(id), unescapeEntities(label))
	case '#':
		id, label, _ := strings.Cut(tok[1:], "|")
		return text(c.channelText(unescapeEntities(id), unescapeEntities(label)))
	case '!':
		return c.mdCommand(tok[1:])
	}
	rawURL, label, _ := strings.Cut(tok, "|")
	n, kind := c.link(unescapeEntities(rawURL), unescapeEntities(label))
	if kind == urlNone {
		return nil
	}
	return n
}

// mdCommand resolves <!command^arg…|label>. Broadcasts become plain text:
// a historical import must not notify anyone.
func (c *conv) mdCommand(body string) *node {
	cmd, label, _ := strings.Cut(body, "|")
	cmd = unescapeEntities(cmd)
	label = cleanLine(unescapeEntities(label))
	args := strings.Split(cmd, "^")
	switch args[0] {
	case "here", "channel", "everyone", "group":
		return text("@" + args[0])
	case "subteam":
		if label == "" {
			label = "@group"
		}
		return text(label)
	case "date":
		ts, format, href := "", "", ""
		if len(args) > 1 {
			ts = args[1]
		}
		if len(args) > 2 {
			format = args[2]
		}
		if len(args) > 3 {
			href = strings.Join(args[3:], "^")
		}
		_ = format // Slack formats in the reader's time zone; we show the fallback
		return c.dateNode(ts, label, href)
	}
	if label == "" {
		label = cleanLine(cmd)
	}
	if label == "" {
		return nil
	}
	return text(label)
}

// dateNode renders a date: its fallback text, or the UTC time when there is
// none, linked if href is a URL we link to.
func (c *conv) dateNode(unix, fallback, href string) *node {
	shown := cleanLine(fallback)
	if shown == "" {
		shown = "date"
		// Parse as a whole number of seconds; ignore a fraction.
		whole, _, _ := strings.Cut(strings.TrimSpace(unix), ".")
		if sec, err := strconv.ParseInt(whole, 10, 64); err == nil && sec > -62_000_000_000 && sec < 250_000_000_000 {
			shown = time.Unix(sec, 0).UTC().Format(dateLayout)
		}
	}
	if href != "" {
		if n, kind := c.link(href, shown); kind == urlOK {
			return n
		}
	}
	return text(shown)
}

// --- blocks ------------------------------------------------------------------

func isTextUnit(u unit, r rune) bool { return u.n == nil && u.r == r }

func blankLine(ln []unit) bool {
	for _, u := range ln {
		if u.n != nil || !unicode.IsSpace(u.r) {
			return false
		}
	}
	return true
}

// quoteDepth returns 3 for a line starting with ">>>", 1 for one starting
// with ">", and 0 otherwise.
func quoteDepth(ln []unit) int {
	n := 0
	for n < len(ln) && n < 3 && isTextUnit(ln[n], '>') {
		n++
	}
	if n == 2 {
		n = 1
	}
	return n
}

// afterMarker drops a quote marker of n characters and one following space.
func afterMarker(ln []unit, n int) []unit {
	ln = ln[n:]
	if len(ln) > 0 && isTextUnit(ln[0], ' ') {
		ln = ln[1:]
	}
	return ln
}

// mdBlocks groups lines into paragraphs, code blocks and, when quotes is
// set, block quotes. A quote's content is parsed with quotes off, so quotes
// do not nest.
func (c *conv) mdBlocks(lines [][]unit, quotes bool) []*node {
	var (
		out  []*node
		para []*node // inline content of the paragraph being built
	)
	flush := func() {
		if len(para) > 0 {
			out = append(out, el("p", para...))
			para = nil
		}
	}
	addLine := func(seg []unit) {
		if blankLine(seg) {
			return
		}
		if len(para) > 0 {
			para = append(para, br())
		}
		para = append(para, formatLine(seg)...)
	}
	literalUntil := 0 // lines before this index were tried as a quote and are plain text
	for i := 0; i < len(lines); i++ {
		ln := lines[i]
		if quotes && i >= literalUntil {
			switch quoteDepth(ln) {
			case 3:
				rest := append([][]unit{afterMarker(ln, 3)}, lines[i+1:]...)
				if inner := c.mdBlocks(rest, false); len(inner) > 0 {
					flush()
					return append(out, el("blockquote", inner...))
				}
			case 1:
				j := i
				var quoted [][]unit
				for j < len(lines) && quoteDepth(lines[j]) == 1 {
					quoted = append(quoted, afterMarker(lines[j], 1))
					j++
				}
				if inner := c.mdBlocks(quoted, false); len(inner) > 0 {
					flush()
					out = append(out, el("blockquote", inner...))
					i = j - 1
					continue
				}
				literalUntil = j
			}
			// A marker with nothing after it is ordinary text.
		}
		if blankLine(ln) {
			flush()
			continue
		}
		start := 0
		for k, u := range ln {
			if u.block {
				addLine(ln[start:k])
				flush()
				out = append(out, u.n)
				start = k + 1
			}
		}
		rest := ln[start:]
		for start > 0 && len(rest) > 0 && rest[0].n == nil && unicode.IsSpace(rest[0].r) {
			rest = rest[1:] // white space between a code block and the text after it
		}
		addLine(rest)
	}
	flush()
	return out
}

// --- inline formatting -------------------------------------------------------

var markers = [...]struct {
	r   rune
	tag string
}{{'*', "strong"}, {'_', "em"}, {'~', "s"}}

func markerIndex(u unit) int {
	if u.n != nil {
		return -1
	}
	for k, m := range markers {
		if u.r == m.r {
			return k
		}
	}
	return -1
}

// formatter applies *bold*, _italic_ and ~strike~ to one line.
//
// A marker opens a span if it follows the start of the line, white space or
// punctuation (anything but a letter or digit) and is followed by something
// other than white space. It closes one if it follows something other than
// white space and is followed by the end of the line, white space or
// punctuation. Doubled markers (**, __) are literal. That is what keeps
// snake_case_name and 2*3*4 intact. Atoms count as punctuation, so a span
// can wrap a link or a mention.
type formatter struct {
	u       []unit
	closers [len(markers)][]int // positions that can close each marker, ascending
}

func formatLine(ln []unit) []*node {
	for len(ln) > 0 && ln[len(ln)-1].n == nil && unicode.IsSpace(ln[len(ln)-1].r) {
		ln = ln[:len(ln)-1]
	}
	f := &formatter{u: ln}
	for i, u := range ln {
		if k := markerIndex(u); k >= 0 && f.canClose(i) {
			f.closers[k] = append(f.closers[k], i)
		}
	}
	return f.run(0, len(ln), 0)
}

func (f *formatter) space(i int) bool { return f.u[i].n == nil && unicode.IsSpace(f.u[i].r) }

func (f *formatter) word(i int) bool {
	return f.u[i].n == nil && (unicode.IsLetter(f.u[i].r) || unicode.IsDigit(f.u[i].r))
}

func (f *formatter) same(i, j int) bool {
	return f.u[i].n == nil && f.u[j].n == nil && f.u[i].r == f.u[j].r
}

func (f *formatter) canOpen(i int) bool {
	if i > 0 && (f.word(i-1) || f.same(i-1, i)) {
		return false
	}
	return i+1 < len(f.u) && !f.space(i+1) && !f.same(i+1, i)
}

func (f *formatter) canClose(i int) bool {
	if i == 0 || f.space(i-1) || f.same(i-1, i) {
		return false
	}
	return i+1 == len(f.u) || (!f.word(i+1) && !f.same(i+1, i))
}

// run formats units lo..hi. active is the set of markers already open, which
// cannot open again inside themselves.
func (f *formatter) run(lo, hi int, active uint) []*node {
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
	for i := lo; i < hi; {
		u := f.u[i]
		if u.n != nil {
			flush()
			out = append(out, u.n)
			i++
			continue
		}
		if k := markerIndex(u); k >= 0 && active&(1<<k) == 0 && f.canOpen(i) {
			cl := f.closers[k]
			// The content must not be empty, so the closer is at i+2 or later.
			if p := sort.SearchInts(cl, i+2); p < len(cl) && cl[p] < hi {
				j := cl[p]
				flush()
				out = append(out, el(markers[k].tag, f.run(i+1, j, active|1<<k)...))
				i = j + 1
				continue
			}
		}
		sb.WriteRune(u.r)
		i++
	}
	flush()
	return out
}
