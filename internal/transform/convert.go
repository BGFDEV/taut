// Package transform converts Slack messages from a workspace export into the
// HTML that Microsoft Graph's message-import API accepts for Teams.
//
// It is pure computation: no I/O, no globals that change, and the same input
// always gives the same output. Classify decides what happens to a message;
// Convert renders the ones that are imported.
//
// # Output
//
// The HTML uses only the tags p, br, strong, em, s, u, code, pre, blockquote,
// ul, ol, li, a and at, and only the attributes href (on a), id (on at) and
// start (on ol). In attach mode (Options.FileMode) it also holds file
// markers, described below. Everything else is escaped text. Links are emitted only for
// http, https and mailto URLs. Slack file URLs, which carry secret tokens,
// are never emitted.
//
// # File markers
//
// With Options.FileMode "attach", each file hosted by Slack is represented
// by a marker that a later stage replaces once the file is uploaded:
//
//	<p><attachment id="taut-file:{ID}"></attachment></p>
//	<p><img src="taut-file:{ID}" alt="{title or name}"></p>
//
// {ID} is the Slack file ID, admitted only if it matches
// ^[A-Za-z0-9_-]{1,64}$. The image form is used for PNG, JPEG, GIF and WebP
// files no larger than Options.InlineImageMax. A file appears once per
// message, each marker lies wholly within one part, and Part.Files lists the
// IDs of the markers in that part. Markers are written only for the
// message's files: message text is escaped, so text that spells out a marker
// can never become one. Text may still contain the bare characters
// "taut-file:", so the stage that resolves markers must match the whole
// element, from "<attachment id=" or "<img src=", not the prefix alone.
//
// Part sizes leave room for the replacement. A marker becomes
// <attachment id="{36-character GUID}"></attachment> or
// <img src="../hostedContents/{n}/$value" alt="…">, and neither is longer
// than a marker with a 64-character ID, so every marker counts towards
// MaxBytes as if its ID were 64 characters long.
//
// # White space
//
// Text is carried over as written, with these normalizations: control
// characters other than newline and tab are removed (so CRLF becomes a
// newline); a newline becomes <br>; in mrkdwn, one or more blank lines
// separate paragraphs, and spaces at the end of a line are dropped. Runs of
// spaces are kept in the HTML but collapse when displayed, as in any HTML.
package transform

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/bgfdev/taut/internal/slack"
)

// UserRef is what a Resolver knows about a Slack user.
type UserRef struct {
	Name    string // best display name
	EntraID string // Entra object ID; empty means the user has no Microsoft 365 identity
}

// Resolver looks up the references a message makes to users and channels.
type Resolver interface {
	User(slackID string) (UserRef, bool) // false: unknown ID
	Channel(slackID string) (name string, ok bool)
}

// Reaction policies for Options.Reactions.
const (
	ReactionsFooter = "footer" // a summary line at the end of the message
	ReactionsNative = "native" // left to the caller, from Result.Reactions
	ReactionsDrop   = "drop"   // not shown
)

// File modes for Options.FileMode.
const (
	FileModeNote   = "note"   // a line naming each file; nothing is attached
	FileModeAttach = "attach" // a marker per file, resolved after upload
)

// Part size limits for Options.MaxBytes.
const (
	DefaultMaxBytes = 80_000
	MinMaxBytes     = 2_000
)

// Options adjusts Convert.
type Options struct {
	Reactions string // "footer" (default), "native", "drop"
	MaxBytes  int    // max UTF-8 bytes of HTML per part; default 80_000; values < 2_000 are raised to 2_000
	// AuthorMapped says whether the post will be attributed to the author's own
	// Microsoft 365 identity. When false, the first part starts with a byline
	// naming the original author, because the post will appear to come from a
	// placeholder account.
	AuthorMapped bool
	// AuthorName is the original author's display name, used in the byline.
	AuthorName string
	// AuthorKind qualifies the byline: "bot", "former", or "" .
	AuthorKind string
	// FileMode says how files hosted by Slack are shown: "note" (the default)
	// writes a line saying the file is not migrated; "attach" writes a marker
	// for a later stage to resolve (see the package comment).
	FileMode string
	// InlineImageMax is, in attach mode, the largest PNG, JPEG, GIF or WebP
	// file in bytes that is shown inline rather than attached. 0 disables
	// inlining.
	InlineImageMax int64
}

// Mention is one entry of a Teams message's mentions array.
type Mention struct {
	ID      int    // index used in <at id="…">
	Text    string // the visible text, without "@"
	EntraID string
}

// Part is one Teams post. A long Slack message becomes several.
type Part struct {
	HTML     string
	Mentions []Mention // IDs are 0..n-1 within this part, in order of first appearance
	// Files holds the Slack file IDs of the file markers in this part, in
	// order of appearance, each once. It is empty unless FileMode is "attach".
	Files []string
}

// FileRef describes a file attached to the message, without any Slack URL.
type FileRef struct {
	ID, Name, Title, Mimetype string
	Size                      int64
	External                  bool // hosted outside Slack; ExternalURL is safe to show
	ExternalURL               string
	Unavailable               bool // hidden by plan limit or deleted
	Canvas                    bool
}

// Reaction is one emoji reaction and how many people gave it.
type Reaction struct {
	Emoji string // Unicode when known, else ":name:"
	Count int
}

// Result is a converted message.
type Result struct {
	Parts     []Part     // at least one
	Reactions []Reaction // always filled when the message has reactions, whatever the policy
	Files     []FileRef  // every file on the message, in order
	Warnings  []string   // e.g. "unknown rich_text element type \"foo\""; no message content in them
}

// Placeholder texts.
const (
	tombstoneText = "This message was deleted in Slack."
	huddleText    = "A huddle took place here."
	emptyText     = "(empty message)"
)

// conv holds the state of one conversion.
type conv struct {
	r        Resolver
	warnings []string
	warned   map[string]bool
}

// warn records a warning once. Warnings never include message content;
// callers pass only fixed text and type names.
func (c *conv) warn(format string, args ...any) {
	w := fmt.Sprintf(format, args...)
	if c.warned[w] {
		return
	}
	if c.warned == nil {
		c.warned = map[string]bool{}
	}
	c.warned[w] = true
	c.warnings = append(c.warnings, w)
}

// typeName makes a type string taken from the input safe to quote in a
// warning: it is a short identifier, never free text.
func typeName(s string) string {
	s = cleanLine(s)
	if len(s) > 40 {
		s = strings.ToValidUTF8(s[:40], "") + "…"
	}
	return strconv.Quote(s)
}

// Convert renders a message as Teams HTML, in one or more parts.
//
// It is meant for messages that Classify marks Import or Placeholder. A
// Placeholder message becomes a single fixed line. Unknown or malformed
// input degrades to escaped plain text and a warning; an error is returned
// only if the content cannot be divided into parts of the requested size,
// which the limits Convert applies should make impossible.
func Convert(m slack.Message, r Resolver, o Options) (Result, error) {
	c := &conv{r: r}
	limit := o.MaxBytes
	switch {
	case limit == 0:
		limit = DefaultMaxBytes
	case limit < MinMaxBytes:
		limit = MinMaxBytes
	}

	var res Result
	res.Files = fileRefs(m.Files)
	res.Reactions = reactions(m.Reactions)

	var blocks []*node
	if !o.AuthorMapped {
		blocks = append(blocks, byline(o))
	}

	switch cl := Classify(m); {
	case cl.Disposition == Placeholder && cl.Key == KeyHuddles:
		blocks = append(blocks, para(el("em", text(huddleText))))
	case cl.Disposition == Placeholder:
		blocks = append(blocks, para(el("em", text(tombstoneText))))
	default:
		content := c.body(m)
		content = append(content, c.attachments(m.Attachments)...)
		content = append(content, c.fileLines(m.Files, res.Files, o)...)
		content = prune(content)
		if len(content) == 0 {
			c.warn("message has no content to show")
			content = []*node{para(el("em", text(emptyText)))}
		}
		blocks = append(blocks, content...)
		if m.Edited != nil {
			blocks = append(blocks, para(el("em", text("(edited)"))))
		}
		switch o.Reactions {
		case "", ReactionsFooter:
			blocks = append(blocks, reactionFooter(res.Reactions)...)
		case ReactionsNative, ReactionsDrop:
		default:
			c.warn("unknown reactions policy %s; used \"footer\"", typeName(o.Reactions))
			blocks = append(blocks, reactionFooter(res.Reactions)...)
		}
	}

	blocks = prune(blocks)
	idWidth := len(strconv.Itoa(max(countMentions(blocks)-1, 0)))
	for _, b := range blocks {
		measure(b, idWidth)
	}
	// A link or mention is never divided, so one that could not share a part
	// with anything else is written as text before the parts are laid out.
	degraded := false
	for _, b := range blocks {
		if b.sz > limit/2 && degradeOversized(b, limit/2) {
			degraded = true
			measure(b, idWidth)
		}
	}
	parts, late, err := paginate(blocks, limit, idWidth)
	if err != nil {
		return Result{}, err
	}
	degraded = degraded || late
	if degraded {
		c.warn("a link or mention longer than a whole part was written as plain text")
	}
	for _, p := range parts {
		res.Parts = append(res.Parts, renderPart(p))
	}
	res.Warnings = c.warnings
	return res, nil
}

// body renders the message's own content: rich_text blocks when it has any,
// otherwise its mrkdwn text.
func (c *conv) body(m slack.Message) []*node {
	if blocks, ok := c.decodeBlocks(m.Blocks); ok {
		if out := prune(c.blocks(blocks)); len(out) > 0 {
			return out
		}
	}
	return c.mrkdwn(m.Text)
}

// byline names the original author of a post that will appear under a
// placeholder account.
func byline(o Options) *node {
	name := cleanLine(o.AuthorName)
	if name == "" {
		name = "Unknown author"
	}
	label := "via Slack"
	switch o.AuthorKind {
	case "bot":
		label = "bot"
	case "former":
		label = "former member"
	}
	return para(el("strong", text(name)), text(" "), el("em", text("("+label+")")))
}

// fileRefs describes the files of a message. No Slack URL is copied.
func fileRefs(files []slack.File) []FileRef {
	if len(files) == 0 {
		return nil
	}
	out := make([]FileRef, 0, len(files))
	for _, f := range files {
		ref := FileRef{
			ID:          cleanLine(f.ID),
			Name:        cleanLine(f.Name),
			Title:       cleanLine(f.Title),
			Mimetype:    cleanLine(f.Mimetype),
			Size:        f.Size,
			External:    f.IsExternal || f.Mode == slack.FileModeExternal,
			Unavailable: f.IsHidden() || f.IsTombstone(),
			Canvas:      f.IsCanvas(),
		}
		if ref.External {
			if href, kind := checkURL(f.ExternalURL); kind == urlOK {
				ref.ExternalURL = href
			}
		}
		out = append(out, ref)
	}
	return out
}

// maxAltBytes caps an inline image's alt text, so that an image marker is
// always far smaller than a part.
const maxAltBytes = 300

// inlineImageTypes are the image formats Teams shows inline.
var inlineImageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// fileLines renders the message's files, one block each. raw and files
// describe the same files.
//
// In note mode a hosted file becomes a line saying it is not migrated. In
// attach mode it becomes a marker, once per file ID; the ID is taken from
// the raw file and validated, and a file without a usable one is reported
// as unavailable. External and unavailable files look the same in both
// modes.
func (c *conv) fileLines(raw []slack.File, files []FileRef, o Options) []*node {
	attach := false
	switch o.FileMode {
	case "", FileModeNote:
	case FileModeAttach:
		attach = true
	default:
		c.warn("unknown file mode %s; used \"note\"", typeName(o.FileMode))
	}
	var (
		out  []*node
		seen map[string]bool
	)
	unavailable := func(name string) *node {
		return para(text("📎 "+name+" "), el("em", text("(not available in the Slack export)")))
	}
	for i, f := range files {
		name := f.Title
		if name == "" {
			name = f.Name
		}
		if name == "" {
			name = "File"
		}
		switch {
		case f.Unavailable:
			if f.Name != "" {
				name = f.Name
			}
			out = append(out, unavailable(name))
		case f.External && f.ExternalURL != "":
			out = append(out, para(text("📎 "), anchor(f.ExternalURL, text(name))))
		case f.External:
			out = append(out, para(text("📎 "+name+" "), el("em", text("(external file)"))))
		case !attach:
			out = append(out, para(text("📎 "+name+" "), el("em", text("(file not migrated yet)"))))
		default:
			id := raw[i].ID
			if !validFileID(id) {
				c.warn("a file has no usable ID and cannot be attached")
				out = append(out, unavailable(name))
				continue
			}
			if seen[id] {
				continue
			}
			if seen == nil {
				seen = map[string]bool{}
			}
			seen[id] = true
			inline := o.InlineImageMax > 0 && f.Size > 0 && f.Size <= o.InlineImageMax &&
				inlineImageTypes[strings.ToLower(f.Mimetype)]
			alt := name
			if len(alt) > maxAltBytes {
				alt = strings.ToValidUTF8(alt[:maxAltBytes], "") + "…"
			}
			out = append(out, fileNode(id, alt, inline))
		}
	}
	return out
}

// reactions summarizes the message's reactions. The count is Slack's count
// field, or the number of listed users when that is missing. Entries with
// no name, or that nobody is recorded as having given, are left out.
func reactions(in []slack.Reaction) []Reaction {
	var out []Reaction
	for _, r := range in {
		name := cleanLine(r.Name)
		count := r.Count
		if count <= 0 {
			count = len(r.Users)
		}
		if name == "" || count <= 0 {
			continue
		}
		emoji, ok := emojiFor(name)
		if !ok {
			emoji = ":" + name + ":"
		}
		out = append(out, Reaction{Emoji: emoji, Count: count})
	}
	return out
}

func reactionFooter(rs []Reaction) []*node {
	if len(rs) == 0 {
		return nil
	}
	var sb strings.Builder
	for i, r := range rs {
		if i > 0 {
			sb.WriteString(" · ")
		}
		sb.WriteString(r.Emoji)
		sb.WriteByte(' ')
		sb.WriteString(strconv.Itoa(r.Count))
	}
	return []*node{para(el("em", text(sb.String())))}
}

// --- references --------------------------------------------------------------

// userNode renders a reference to a Slack user: a mention entity when the
// user has a Microsoft 365 identity, otherwise plain text. label is the name
// the message itself carried for the user, if any.
func (c *conv) userNode(id, label string) *node {
	var (
		ref UserRef
		ok  bool
	)
	if c.r != nil && id != "" {
		ref, ok = c.r.User(id)
	}
	name := cleanLine(ref.Name)
	label = strings.TrimPrefix(cleanLine(label), "@")
	switch {
	case ok && name != "" && ref.EntraID != "":
		return &node{kind: kAt, text: name, entra: ref.EntraID}
	case ok && name != "":
		return text("@" + name)
	case label != "":
		return text("@" + label)
	}
	return text("@unknown-user")
}

// userPlain is userNode for places that can hold only text.
func (c *conv) userPlain(id string) string {
	n := c.userNode(id, "")
	if n.kind == kAt {
		return "@" + n.text
	}
	return n.text
}

// channelText renders a reference to a channel as "#name".
func (c *conv) channelText(id, label string) string {
	if c.r != nil && id != "" {
		if name, ok := c.r.Channel(id); ok {
			if name = cleanLine(name); name != "" {
				return "#" + strings.TrimPrefix(name, "#")
			}
		}
	}
	if label = strings.TrimPrefix(cleanLine(label), "#"); label != "" {
		return "#" + label
	}
	return "#unknown-channel"
}

// --- links -------------------------------------------------------------------

type urlKind int

const (
	urlNone      urlKind = iota // no scheme: not a URL as far as we can tell
	urlUnsafe                   // a scheme we do not link to
	urlSlackFile                // a Slack file URL, which may carry a secret token
	urlOK                       // http, https or mailto
)

var slackTokenRE = regexp.MustCompile(`(?i)xox[a-z]-|[?&]t=`)

// checkURL classifies raw and, for urlOK, returns the href to use. Control
// characters are removed first, so a scheme cannot be disguised with them.
func checkURL(raw string) (href string, kind urlKind) {
	raw = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || (r >= 0x7f && r < 0xa0) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(raw, "")))
	scheme := ""
	for i := 0; i < len(raw); i++ {
		ch := raw[i]
		if ch == ':' {
			scheme = strings.ToLower(raw[:i])
			break
		}
		letter := (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')
		if !letter && (i == 0 || !((ch >= '0' && ch <= '9') || ch == '+' || ch == '-' || ch == '.')) {
			break
		}
	}
	switch scheme {
	case "":
		return "", urlNone
	case "http", "https":
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return "", urlUnsafe
		}
		host := strings.ToLower(u.Hostname())
		if host == "files.slack.com" || host == "slack-files.com" || strings.HasSuffix(host, ".slack-files.com") ||
			(strings.HasSuffix(host, "slack.com") && slackTokenRE.MatchString(raw)) || strings.Contains(strings.ToLower(raw), "xoxe-") {
			return "", urlSlackFile
		}
	case "mailto":
		if len(raw) == len("mailto:") {
			return "", urlUnsafe
		}
	default:
		return "", urlUnsafe
	}
	return strings.ReplaceAll(raw, " ", "%20"), urlOK
}

const slackFileLinkText = "(Slack file link)"

// link renders a link with an optional label. Only http, https and mailto
// URLs become anchors; anything else is shown as text, and Slack file URLs
// are not shown at all. kind tells the caller what the URL turned out to be.
func (c *conv) link(rawURL, label string) (n *node, kind urlKind) {
	label = cleanLine(label)
	href, kind := checkURL(rawURL)
	shown := cleanLine(rawURL)
	switch kind {
	case urlOK:
		if label == "" {
			label = strings.TrimPrefix(shown, "mailto:")
		}
		if _, k := checkURL(label); k == urlSlackFile {
			label = slackFileLinkText
		}
		return anchor(href, text(label)), kind
	case urlSlackFile:
		if _, k := checkURL(label); label == "" || k == urlSlackFile || label == shown {
			label = slackFileLinkText
		}
		return text(label), kind
	}
	switch {
	case label == "" || label == shown:
		return text(shown), kind
	case shown == "":
		return text(label), kind
	}
	return text(label + " (" + shown + ")"), kind
}
