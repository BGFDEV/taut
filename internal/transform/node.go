package transform

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// The intermediate representation shared by the rich_text and mrkdwn paths is
// a small tree of nodes. Only the renderer below turns it into HTML, so the
// set of tags and attributes that can ever appear in the output is decided in
// one place, and every piece of text passes through one escaper.

type nodeKind uint8

const (
	kText   nodeKind = iota // text: raw, unescaped
	kElem                   // tag + kids; attr is href (a) or start (ol)
	kBr                     // <br>
	kAt                     // <at id="n">text</at>; entra is the mentioned user
	kAttach                 // <attachment id="taut-file:ID"></attachment>; text is the file ID
	kImg                    // <img src="taut-file:ID" alt="…">; text is the file ID, attr the alt text
)

// FileMarkerPrefix starts the placeholder that stands for a migrated file in
// the id of an <attachment> element or the src of an <img> element. A later
// stage replaces the marker once the file has been uploaded.
const FileMarkerPrefix = "taut-file:"

// markerIDWidth is the file ID length every marker is sized for; see measure.
const markerIDWidth = 64

// validFileID reports whether id may be written into a file marker: 1 to 64
// characters from A-Z, a-z, 0-9, "_" and "-". Markers are later replaced by
// exact string match, so nothing that could end the attribute, or make one
// marker look like another, is ever let in.
func validFileID(id string) bool {
	if len(id) == 0 || len(id) > markerIDWidth {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// fileNode returns the block holding the marker for a migrated file: an
// inline image when inline is set, else an attachment reference. The caller
// has validated id.
func fileNode(id, alt string, inline bool) *node {
	if inline {
		return para(&node{kind: kImg, text: id, attr: alt})
	}
	return para(&node{kind: kAttach, text: id})
}

type node struct {
	kind  nodeKind
	tag   string
	attr  string
	text  string
	entra string
	kids  []*node
	// sz is the node's rendered size in bytes, set by measure. It assumes
	// every mention id is as wide as the widest id the message could need,
	// so it is an upper bound on the real size.
	sz int
}

func text(s string) *node { return &node{kind: kText, text: s} }

func el(tag string, kids ...*node) *node { return &node{kind: kElem, tag: tag, kids: kids} }

func br() *node { return &node{kind: kBr} }

func anchor(href string, kids ...*node) *node {
	return &node{kind: kElem, tag: "a", attr: href, kids: kids}
}

func para(kids ...*node) *node { return el("p", kids...) }

// atomic reports whether n must never be divided between two parts.
func (n *node) atomic() bool {
	return n.kind != kText && (n.kind != kElem || n.tag == "a")
}

// escLen returns the length of s once HTML-escaped.
func escLen(s string) int {
	n := len(s)
	for i := 0; i < len(s); i++ {
		n += escExtra(s[i])
	}
	return n
}

// escExtra returns how many bytes escaping adds for byte c.
func escExtra(c byte) int {
	switch c {
	case '&':
		return 4 // &amp;
	case '<', '>':
		return 3 // &lt; &gt;
	case '"':
		return 5 // &quot;
	case '\'':
		return 4 // &#39;
	}
	return 0
}

// escape writes s to sb with the five HTML-significant characters replaced by
// entities. It is used for text and for attribute values alike.
func escape(sb *strings.Builder, s string) {
	last := 0
	for i := 0; i < len(s); i++ {
		var rep string
		switch s[i] {
		case '&':
			rep = "&amp;"
		case '<':
			rep = "&lt;"
		case '>':
			rep = "&gt;"
		case '"':
			rep = "&quot;"
		case '\'':
			rep = "&#39;"
		default:
			continue
		}
		sb.WriteString(s[last:i])
		sb.WriteString(rep)
		last = i + 1
	}
	sb.WriteString(s[last:])
}

// overhead returns the bytes an element's own tags take.
func (n *node) overhead() int {
	o := 2*len(n.tag) + 5 // <tag></tag>
	switch {
	case n.tag == "a":
		o += len(` href=""`) + escLen(n.attr)
	case n.tag == "ol" && n.attr != "":
		o += len(` start=""`) + escLen(n.attr)
	}
	return o
}

// measure sets sz on n and everything below it. idWidth is the number of
// digits to assume for every mention id.
func measure(n *node, idWidth int) int {
	switch n.kind {
	case kText:
		n.sz = escLen(n.text)
	case kBr:
		n.sz = len("<br>")
	case kAt:
		n.sz = len(`<at id="">`) + idWidth + escLen(n.text) + len("</at>")
	case kAttach:
		// A marker is replaced later, by <attachment id="{36-character
		// GUID}"></attachment> or <img src="../hostedContents/{n}/$value"
		// alt="…">. Neither is longer than a marker holding a 64-character
		// file ID, so every marker is sized as if its ID were that long,
		// whatever its real length: the part still fits after replacement.
		n.sz = len(`<attachment id="`) + len(FileMarkerPrefix) + markerIDWidth + len(`"></attachment>`)
	case kImg:
		n.sz = len(`<img src="`) + len(FileMarkerPrefix) + markerIDWidth + len(`" alt="`) + escLen(n.attr) + len(`">`)
	case kElem:
		n.sz = n.overhead()
		for _, k := range n.kids {
			n.sz += measure(k, idWidth)
		}
	}
	return n.sz
}

// renderer writes nodes as HTML and numbers the mentions it meets.
type renderer struct {
	sb       strings.Builder
	mentions []Mention
	files    []string
}

// file notes a file marker and reports whether it may be written. This is
// the only way an <attachment> or <img> reaches the output, and it refuses
// any ID that is not valid, so nothing else can ever produce one.
func (r *renderer) file(id string) bool {
	if !validFileID(id) {
		return false
	}
	for _, f := range r.files {
		if f == id {
			return true
		}
	}
	r.files = append(r.files, id)
	return true
}

func (r *renderer) node(n *node) {
	switch n.kind {
	case kText:
		escape(&r.sb, n.text)
	case kBr:
		r.sb.WriteString("<br>")
	case kAt:
		id := len(r.mentions)
		r.mentions = append(r.mentions, Mention{ID: id, Text: n.text, EntraID: n.entra})
		r.sb.WriteString(`<at id="`)
		r.sb.WriteString(strconv.Itoa(id))
		r.sb.WriteString(`">`)
		escape(&r.sb, n.text)
		r.sb.WriteString("</at>")
	case kAttach:
		if r.file(n.text) {
			r.sb.WriteString(`<attachment id="` + FileMarkerPrefix + n.text + `"></attachment>`)
		}
	case kImg:
		if r.file(n.text) {
			r.sb.WriteString(`<img src="` + FileMarkerPrefix + n.text + `" alt="`)
			escape(&r.sb, n.attr)
			r.sb.WriteString(`">`)
		}
	case kElem:
		r.sb.WriteByte('<')
		r.sb.WriteString(n.tag)
		switch {
		case n.tag == "a":
			r.sb.WriteString(` href="`)
			escape(&r.sb, n.attr)
			r.sb.WriteByte('"')
		case n.tag == "ol" && n.attr != "":
			r.sb.WriteString(` start="`)
			escape(&r.sb, n.attr)
			r.sb.WriteByte('"')
		}
		r.sb.WriteByte('>')
		for _, k := range n.kids {
			r.node(k)
		}
		r.sb.WriteString("</")
		r.sb.WriteString(n.tag)
		r.sb.WriteByte('>')
	}
}

// renderPart renders top-level blocks as one part.
func renderPart(blocks []*node) Part {
	var r renderer
	for _, b := range blocks {
		r.node(b)
	}
	return Part{HTML: r.sb.String(), Mentions: r.mentions, Files: r.files}
}

// prune removes empty text nodes and elements left without children, and
// merges neighbouring text nodes, so the output never holds an empty tag.
func prune(nodes []*node) []*node {
	out := nodes[:0:0]
	var run []string // texts of the run of text nodes being merged
	flush := func() {
		switch len(run) {
		case 0:
		case 1:
			out = append(out, text(run[0]))
		default:
			out = append(out, text(strings.Join(run, "")))
		}
		run = run[:0]
	}
	for _, n := range nodes {
		switch n.kind {
		case kText:
			if n.text != "" {
				run = append(run, n.text)
			}
			continue
		case kElem:
			n.kids = prune(n.kids)
			if len(n.kids) == 0 {
				continue
			}
		}
		flush()
		out = append(out, n)
	}
	flush()
	return out
}

// countMentions returns the number of mention nodes under nodes.
func countMentions(nodes []*node) int {
	c := 0
	for _, n := range nodes {
		if n.kind == kAt {
			c++
		}
		c += countMentions(n.kids)
	}
	return c
}

// isBlank reports whether inline nodes show nothing: only white space and
// line breaks.
func isBlank(nodes []*node) bool {
	for _, n := range nodes {
		switch n.kind {
		case kBr:
		case kText:
			if strings.TrimSpace(n.text) != "" {
				return false
			}
		case kElem:
			if !isBlank(n.kids) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// trimBreaks drops line breaks (and white space next to them) from both ends
// of an inline run.
func trimBreaks(nodes []*node) []*node {
	for len(nodes) > 0 && edgeBlank(nodes[0]) {
		nodes = nodes[1:]
	}
	for len(nodes) > 0 && edgeBlank(nodes[len(nodes)-1]) {
		nodes = nodes[:len(nodes)-1]
	}
	return nodes
}

func edgeBlank(n *node) bool {
	return n.kind == kBr || (n.kind == kText && strings.TrimSpace(n.text) == "")
}

// textLines turns text into text nodes separated by line breaks.
func textLines(s string) []*node {
	if !strings.Contains(s, "\n") {
		if s == "" {
			return nil
		}
		return []*node{text(s)}
	}
	var out []*node
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			out = append(out, br())
		}
		if line != "" {
			out = append(out, text(line))
		}
	}
	return out
}

// cleanText makes s safe to carry as text: invalid UTF-8 becomes U+FFFD, and
// control characters (C0 except newline and tab, DEL, and C1) are removed. A
// CRLF therefore becomes a plain newline.
func cleanText(s string) string {
	clean := true
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= utf8.RuneSelf || (c < 0x20 && c != '\n' && c != '\t') || c == 0x7f {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	s = strings.ToValidUTF8(s, "�")
	return strings.Map(func(r rune) rune {
		if (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7f && r < 0xa0) {
			return -1
		}
		return r
	}, s)
}

// cleanLine is cleanText for single-line values such as names and labels:
// newlines and tabs become spaces and the ends are trimmed.
func cleanLine(s string) string {
	s = cleanText(s)
	if strings.ContainsAny(s, "\n\t") {
		s = strings.Map(func(r rune) rune {
			if r == '\n' || r == '\t' {
				return ' '
			}
			return r
		}, s)
	}
	return strings.TrimSpace(s)
}
