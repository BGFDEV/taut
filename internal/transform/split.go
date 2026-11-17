package transform

import (
	"errors"
	"strconv"
	"unicode"
	"unicode/utf8"
)

// Splitting works on the node tree, not on HTML text, which is what makes
// every part well-formed by construction: a block that has to be divided is
// cut into a head and a tail that each repeat the enclosing elements, and the
// cut can only fall between two children or inside a text node. Links,
// mentions and line breaks are atomic and are never cut.
//
// All sizes are the upper bounds computed by measure, so a part can only be
// smaller than planned, never larger.

// errUnsplittable is returned when a block cannot be made to fit, which the
// limits on nesting depth and the minimum part size should rule out.
var errUnsplittable = errors.New("transform: content cannot be split to fit the part size")

// paginate distributes top-level blocks over parts of at most limit bytes.
// Blocks stay whole where they can; a block larger than a part is divided.
// It reports whether any oversized link or mention had to be turned into
// plain text to make that possible.
func paginate(blocks []*node, limit, idWidth int) (parts [][]*node, degraded bool, err error) {
	var cur []*node
	used := 0
	flush := func() {
		parts = append(parts, cur)
		cur, used = nil, 0
	}
	blocks = append([]*node(nil), blocks...)
	for i := 0; i < len(blocks); {
		b := blocks[i]
		switch {
		case used+b.sz <= limit:
			cur = append(cur, b)
			used += b.sz
			i++
		case b.sz <= limit || used > limit/2:
			// The block fits a part of its own, or too little room is left
			// here to be worth starting an oversized block in.
			flush()
		default:
			head, tail := split(b, limit-used)
			if head == nil {
				if used > 0 {
					flush()
					continue
				}
				// Nothing fits even in an empty part: the block starts with
				// an atom larger than a part. Turn it into text and retry.
				if !degrade(b) {
					return nil, degraded, errUnsplittable
				}
				degraded = true
				measure(b, idWidth)
				continue
			}
			cur = append(cur, head)
			flush()
			blocks[i] = tail
		}
	}
	if len(cur) > 0 {
		flush()
	}
	return parts, degraded, nil
}

// split divides n into a head of at most budget bytes and the remaining
// tail. head is nil if nothing of n fits; tail is nil if all of it does.
func split(n *node, budget int) (head, tail *node) {
	if n.sz <= budget {
		return n, nil
	}
	switch {
	case n.kind == kText:
		cut := cutText(n.text, budget)
		if cut == 0 {
			return nil, n
		}
		head = text(n.text[:cut])
		head.sz = escLen(head.text)
		tail = text(n.text[cut:])
		tail.sz = n.sz - head.sz
		return head, tail
	case n.atomic():
		return nil, n
	}

	over := n.overhead()
	room := budget - over
	i, used := 0, 0
	for i < len(n.kids) && used+n.kids[i].sz <= room {
		used += n.kids[i].sz
		i++
	}
	if i == len(n.kids) {
		// Cannot happen while sz is the sum of the parts; guard anyway.
		return n, nil
	}
	var h, t *node
	switch {
	case n.tag == "table" && i > 0:
		// Cut a table between rows, not through one, whenever at least one
		// row fits here.
		t = n.kids[i]
	case room-used > 0:
		h, t = split(n.kids[i], room-used)
	default:
		t = n.kids[i]
	}
	if h == nil && i == 0 {
		return nil, n
	}

	head = &node{kind: kElem, tag: n.tag, attr: n.attr, sz: over + used}
	head.kids = n.kids[:i:i]
	if h != nil {
		head.kids = append(head.kids, h)
		head.sz += h.sz
	}

	tail = &node{kind: kElem, tag: n.tag, attr: n.attr}
	if n.tag == "ol" {
		// Keep the numbering going. If an item was cut in two, its second
		// half keeps the item's number.
		tail.attr = strconv.Itoa(olStart(n) + i)
	}
	if h == nil {
		tail.kids = n.kids[i:]
	} else {
		tail.kids = make([]*node, 0, len(n.kids)-i)
		tail.kids = append(tail.kids, t)
		tail.kids = append(tail.kids, n.kids[i+1:]...)
	}
	// The tail holds whatever the head does not, plus its own tags.
	tail.sz = n.sz - over - used + tail.overhead()
	if h != nil {
		tail.sz -= n.kids[i].sz - t.sz
	}
	return head, tail
}

// olStart returns the number of an ordered list's first item.
func olStart(n *node) int {
	if v, err := strconv.Atoi(n.attr); err == nil && v > 0 {
		return v
	}
	return 1
}

// degrade replaces the first leaf of n, if it is an oversized link or
// mention, by plain text that can be divided. It reports whether it did.
func degrade(n *node) bool {
	for n.kind == kElem && !n.atomic() && len(n.kids) > 0 {
		first := n.kids[0]
		if first.kind == kAt || (first.kind == kElem && first.tag == "a") {
			n.kids = append(asText(first), n.kids[1:]...)
			return true
		}
		n = first
	}
	return false
}

// asText returns the plain-text replacement for a link or mention.
func asText(n *node) []*node {
	if n.kind == kAt {
		return []*node{text("@" + n.text)}
	}
	repl := append([]*node(nil), n.kids...)
	if !(len(repl) == 1 && repl[0].kind == kText && repl[0].text == n.attr) {
		repl = append(repl, text(" ("+n.attr+")"))
	}
	return repl
}

// degradeOversized replaces every link and mention under n that is larger
// than limit by plain text. It reports whether it replaced any.
func degradeOversized(n *node, limit int) bool {
	changed := false
	for i := 0; i < len(n.kids); i++ {
		k := n.kids[i]
		switch {
		case k.sz <= limit:
		case k.kind == kAt || (k.kind == kElem && k.tag == "a"):
			repl := asText(k)
			kids := make([]*node, 0, len(n.kids)+len(repl)-1)
			kids = append(kids, n.kids[:i]...)
			kids = append(kids, repl...)
			kids = append(kids, n.kids[i+1:]...)
			n.kids = kids
			i += len(repl) - 1
			changed = true
		case k.kind == kElem:
			changed = degradeOversized(k, limit) || changed
		}
	}
	return changed
}

// cutWindow is how far back from the size limit cutText looks for a newline
// or a space to break at.
const cutWindow = 512

// cutText returns the byte offset at which to cut s so that the escaped
// first piece takes at most budget bytes. The offset is always on a rune
// boundary and is 0 only if not even the first rune fits. A break right
// after a newline, or else after a space, is preferred when one lies within
// cutWindow bytes of the limit, and the cut avoids separating a character
// from the combining marks, joiners and modifiers that follow it.
func cutText(s string, budget int) int {
	end, used := 0, 0
	for end < len(s) {
		_, w := utf8.DecodeRuneInString(s[end:])
		cost := w
		if w == 1 {
			cost += escExtra(s[end])
		}
		if used+cost > budget {
			break
		}
		used += cost
		end += w
	}
	if end == 0 || end == len(s) {
		return end
	}
	lo := max(end-cutWindow, 1)
	space := 0
	for i := end; i >= lo; i-- {
		switch s[i-1] {
		case '\n':
			return i
		case ' ':
			if space == 0 {
				space = i
			}
		}
	}
	if space > 0 {
		return space
	}
	// No natural break nearby: at least keep grapheme clusters together.
	cut := end
	for range 32 {
		if cut == 0 || !joined(s, cut) {
			break
		}
		_, w := utf8.DecodeLastRuneInString(s[:cut])
		cut -= w
	}
	if cut == 0 || joined(s, cut) {
		return end
	}
	return cut
}

// joined reports whether the runes on either side of byte offset i in s
// belong to one user-perceived character.
func joined(s string, i int) bool {
	next, _ := utf8.DecodeRuneInString(s[i:])
	prev, _ := utf8.DecodeLastRuneInString(s[:i])
	const zwj = 0x200D
	switch {
	case prev == zwj, next == zwj:
		return true
	case next >= 0xFE00 && next <= 0xFE0F: // variation selectors
		return true
	case next >= 0x1F3FB && next <= 0x1F3FF: // skin tone modifiers
		return true
	case next == 0x20E3: // combining enclosing keycap
		return true
	case next >= 0xE0020 && next <= 0xE007F: // tag characters (subdivision flags)
		return true
	case prev >= 0x1F1E6 && prev <= 0x1F1FF && next >= 0x1F1E6 && next <= 0x1F1FF: // flag pair
		return true
	}
	return unicode.In(next, unicode.Mn, unicode.Me, unicode.Mc)
}
