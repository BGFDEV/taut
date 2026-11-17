package fixtures

import (
	"strconv"
	"strings"
)

// The helpers below build a message body once and render it twice, as Slack
// does: rich_text blocks, and the equivalent mrkdwn in "text".

// inline is one element of a rich_text_section.
type inline struct {
	el M
	md string
}

// block is one top-level element of a rich_text block.
type block struct {
	el M
	md string
}

var mdEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func tx(s string) inline {
	return inline{M{"type": "text", "text": s}, mdEscaper.Replace(s)}
}

// styled returns text with one of the styles bold, italic, strike or code.
func styled(s, style string) inline {
	mark := map[string]string{"bold": "*", "italic": "_", "strike": "~", "code": "`"}[style]
	return inline{
		M{"type": "text", "text": s, "style": M{style: true}},
		mark + mdEscaper.Replace(s) + mark,
	}
}

func link(url, label string) inline {
	if label == "" {
		return inline{M{"type": "link", "url": url}, "<" + url + ">"}
	}
	return inline{M{"type": "link", "url": url, "text": label}, "<" + url + "|" + mdEscaper.Replace(label) + ">"}
}

func mention(u *user) inline {
	return inline{M{"type": "user", "user_id": u.ID}, "<@" + u.ID + ">"}
}

func chanRef(c *channel) inline {
	return inline{M{"type": "channel", "channel_id": c.id}, "<#" + c.id + "|" + c.name + ">"}
}

var emojiCodes = map[string]string{
	"wave": "1f44b", "tada": "1f389", "+1": "1f44d", "eyes": "1f440",
	"rocket": "1f680", "fire": "1f525", "white_check_mark": "2705",
	"thinking_face": "1f914", "pray": "1f64f", "joy": "1f602",
	"heart": "2764-fe0f", "coffee": "2615",
}

var emojiNames = []string{
	"+1", "coffee", "eyes", "fire", "heart", "joy", "pray", "rocket",
	"tada", "thinking_face", "wave", "white_check_mark",
}

func emoji(name string) inline {
	el := M{"type": "emoji", "name": name}
	if code, ok := emojiCodes[name]; ok {
		el["unicode"] = code
	}
	return inline{el, ":" + name + ":"}
}

// broadcast returns an @here, @channel or @everyone element.
func broadcast(scope string) inline {
	return inline{M{"type": "broadcast", "range": scope}, "<!" + scope + ">"}
}

func join(in []inline) (els []any, md string) {
	var sb strings.Builder
	for _, i := range in {
		els = append(els, i.el)
		sb.WriteString(i.md)
	}
	return els, sb.String()
}

func section(in ...inline) block {
	els, md := join(in)
	return block{M{"type": "rich_text_section", "elements": els}, md}
}

// list returns a rich_text_list with style "bullet" or "ordered".
func list(style string, items ...[]inline) block {
	var (
		els []any
		md  []string
	)
	for i, item := range items {
		s := section(item...)
		els = append(els, s.el)
		bullet := "• "
		if style == "ordered" {
			bullet = strconv.Itoa(i+1) + ". "
		}
		md = append(md, bullet+s.md)
	}
	return block{
		M{"type": "rich_text_list", "style": style, "indent": 0, "border": 0, "elements": els},
		strings.Join(md, "\n"),
	}
}

func pre(code string) block {
	return block{
		M{"type": "rich_text_preformatted", "border": 0, "elements": []any{M{"type": "text", "text": code}}},
		"```" + mdEscaper.Replace(code) + "```",
	}
}

func quote(in ...inline) block {
	els, md := join(in)
	return block{M{"type": "rich_text_quote", "elements": els}, "&gt; " + md}
}

// rich sets both the blocks and the mrkdwn text of m.
func (b *builder) rich(m *message, blocks ...block) *message {
	var (
		els []any
		md  []string
	)
	for _, bl := range blocks {
		els = append(els, bl.el)
		md = append(md, bl.md)
	}
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+/="
	id := make([]byte, 5)
	for i := range id {
		id[i] = alphabet[b.rng.IntN(len(alphabet))]
	}
	m.Blocks = []M{{"type": "rich_text", "block_id": string(id), "elements": els}}
	m.Text = strings.Join(md, "\n")
	return m
}

var words = strings.Fields(`
the a we our this that next last new old quick final
deploy release build review ship merge test fix plan sync update check
branch ticket customer dashboard report budget roadmap sprint retro standup
launch invoice contract design mockup draft spec doc agenda notes metrics
staging production pipeline migration rollback hotfix backlog estimate deadline
please thanks sorry maybe probably definitely tomorrow today monday friday
before after with without for from about until looks needs seems works
ready blocked done pending green red flaky slow fast stable broken
`)

func (b *builder) phrase(lo, hi int) string {
	n := b.between(lo, hi)
	out := make([]string, n)
	for i := range out {
		out[i] = words[b.rng.IntN(len(words))]
	}
	return strings.Join(out, " ")
}

func (b *builder) sentence() string {
	s := b.phrase(4, 12)
	return strings.ToUpper(s[:1]) + s[1:] + [...]string{".", ".", ".", "?", "!"}[b.rng.IntN(5)]
}

var linkHosts = []string{"wiki.fixture.example", "tickets.fixture.example", "ci.fixture.example", "docs.fixture.example"}

func (b *builder) url() string {
	return "https://" + linkHosts[b.rng.IntN(len(linkHosts))] + "/" + strings.ReplaceAll(b.phrase(1, 3), " ", "-")
}

var codeSnippets = []string{
	"kubectl rollout status deploy/api -n prod",
	"SELECT count(*) FROM orders WHERE created_at > now() - interval '1 day';",
	"if err != nil {\n\treturn fmt.Errorf(\"sync: %w\", err)\n}",
	"git rebase --onto main feature~3 feature",
	"curl -sS https://ci.fixture.example/health | jq '.status'",
}

// richBody fills m with a random rich_text body: mostly plain sections, with
// lists, preformatted blocks, quotes and broadcasts mixed in.
func (b *builder) richBody(m *message, people []*user, chans []*channel) *message {
	lead := []inline{tx(b.sentence())}
	if b.chance(25) {
		lead = append(lead, tx(" "), styled(b.phrase(1, 3), [...]string{"bold", "italic", "strike", "code"}[b.rng.IntN(4)]))
	}
	if b.chance(20) && len(people) > 0 {
		lead = append(lead, tx(" cc "), mention(b.pick(people)))
	}
	if b.chance(15) {
		lead = append(lead, tx(" see "), link(b.url(), b.phrase(1, 3)))
	}
	if b.chance(8) && len(chans) > 0 {
		lead = append(lead, tx(" in "), chanRef(chans[b.rng.IntN(len(chans))]))
	}
	if b.chance(20) {
		lead = append(lead, tx(" "), emoji(emojiNames[b.rng.IntN(len(emojiNames))]))
	}

	items := func() [][]inline {
		out := make([][]inline, b.between(2, 4))
		for i := range out {
			out[i] = []inline{tx(b.phrase(2, 6))}
		}
		return out
	}
	switch r := b.rng.IntN(100); {
	case r < 60:
		return b.rich(m, section(lead...))
	case r < 72:
		return b.rich(m, section(lead...), list("bullet", items()...))
	case r < 79:
		return b.rich(m, section(lead...), list("ordered", items()...))
	case r < 87:
		return b.rich(m, section(lead...), pre(codeSnippets[b.rng.IntN(len(codeSnippets))]))
	case r < 94:
		return b.rich(m, quote(tx(b.sentence())), section(lead...))
	default:
		scope := [...]string{"here", "channel", "everyone"}[b.rng.IntN(3)]
		return b.rich(m, section(append([]inline{broadcast(scope), tx(" ")}, lead...)...))
	}
}

// mrkdwnBody fills m with a text-only body, as older Slack clients wrote.
func (b *builder) mrkdwnBody(m *message, people []*user) *message {
	var sb strings.Builder
	sb.WriteString(b.sentence())
	if b.chance(20) {
		sb.WriteString(" *" + b.phrase(1, 3) + "*")
	}
	if b.chance(10) {
		sb.WriteString(" _" + b.phrase(1, 2) + "_")
	}
	if b.chance(20) && len(people) > 0 {
		sb.WriteString(" <@" + b.pick(people).ID + ">")
	}
	if b.chance(15) {
		sb.WriteString(" <" + b.url() + "|" + b.phrase(1, 3) + ">")
	}
	if b.chance(20) {
		sb.WriteString(" :" + emojiNames[b.rng.IntN(len(emojiNames))] + ":")
	}
	if b.chance(6) {
		sb.WriteString("\n```" + codeSnippets[b.rng.IntN(len(codeSnippets))] + "```")
	}
	m.Text = sb.String()
	return m
}
