package transform

import (
	"fmt"
	"strings"
	"testing"

	"github.com/bgfdev/taut/internal/fixtures"
	"github.com/bgfdev/taut/internal/slack"
)

// numberedResolver maps every "M<n>" to its own Entra ID, every "S<n>" to
// one shared Entra ID, and nobody else.
type numberedResolver struct{}

func (numberedResolver) User(id string) (UserRef, bool) {
	switch {
	case strings.HasPrefix(id, "M"):
		return UserRef{Name: "Member " + id, EntraID: "entra-" + id}, true
	case strings.HasPrefix(id, "S"):
		return UserRef{Name: "Shared " + id, EntraID: "ENTRA-shared"}, true
	case strings.HasPrefix(id, "N"):
		return UserRef{Name: "Nobody " + id}, true
	}
	return UserRef{}, false
}

func (numberedResolver) Channel(string) (string, bool) { return "", false }

func users(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%d", prefix, i)
	}
	return out
}

func convertNative(t *testing.T, m slack.Message, r Resolver, limit int) Result {
	t.Helper()
	res, err := Convert(m, r, Options{AuthorMapped: true, Reactions: ReactionsNative, MaxBytes: limit})
	if err != nil {
		t.Fatal(err)
	}
	if limit == 0 {
		limit = DefaultMaxBytes
	}
	if err := checkResult(res, limit); err != nil {
		t.Fatal(err)
	}
	return res
}

// The documented limit is 250 reactions on an imported message. Everything
// past it is counted in the summary, so the total is unchanged.
func TestNativeReactionsLimit(t *testing.T) {
	m := slack.Message{Text: "popular", Reactions: []slack.Reaction{
		{Name: "tada", Users: users("M", 240), Count: 240},
		{Name: "eyes", Users: users("M", 30), Count: 30},
		{Name: "heart", Users: users("M", 5), Count: 5},
	}}
	res := convertNative(t, m, numberedResolver{}, 0)
	last := res.Parts[len(res.Parts)-1]
	if len(last.Reactions) != MaxNativeReactions {
		t.Fatalf("%d native reactions, want %d", len(last.Reactions), MaxNativeReactions)
	}
	byEmoji := map[string]int{}
	for _, r := range last.Reactions {
		byEmoji[r.Emoji]++
	}
	if byEmoji["🎉"] != 240 || byEmoji["👀"] != 10 || byEmoji["❤️"] != 0 {
		t.Errorf("native reactions by emoji: %v", byEmoji)
	}
	if want := "<p><em>Other reactions: 👀 20 · ❤️ 5</em></p>"; !strings.HasSuffix(last.HTML, want) {
		t.Errorf("summary of the overflow:\n got %s\nwant suffix %s", last.HTML, want)
	}
	if want := "<p><em>🎉 240 · 👀 30 · ❤️ 5</em></p>"; !strings.HasSuffix(last.FooterHTML, want) {
		t.Errorf("fallback:\n got %s\nwant suffix %s", last.FooterHTML, want)
	}
}

// Teams has one reaction per person and emoji. Two Slack accounts mapped to
// one person, two shortcodes for one emoji, and a user listed twice must
// each give one reaction, with the others counted in the summary.
func TestNativeReactionsOncePerPersonAndEmoji(t *testing.T) {
	m := slack.Message{Text: "x", Reactions: []slack.Reaction{
		{Name: "+1", Users: []string{"S1", "S2", "M1", "M1"}, Count: 4},
		{Name: "thumbsup", Users: []string{"M1", "M2"}, Count: 2},
	}}
	res := convertNative(t, m, numberedResolver{}, 0)
	p := res.Parts[0]
	var got []string
	for _, r := range p.Reactions {
		got = append(got, r.Emoji+" "+r.EntraID)
	}
	want := []string{"👍 ENTRA-shared", "👍 entra-M1", "👍 entra-M2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("native reactions %q, want %q", got, want)
	}
	if want := "<p>x</p><p><em>Other reactions: 👍 3</em></p>"; p.HTML != want {
		t.Errorf("HTML %s, want %s", p.HTML, want)
	}
}

// People with no Microsoft 365 identity, people the export does not know,
// people it counts without listing, and custom emoji are summarized.
func TestNativeReactionsRest(t *testing.T) {
	m := slack.Message{Text: "x", Reactions: []slack.Reaction{
		{Name: "tada", Users: []string{"M1", "N1", "ghost"}, Count: 5},
		{Name: "shipit-custom", Users: []string{"M1"}, Count: 1},
		{Name: "wave::skin-tone-6", Users: []string{"M2"}},
		{Name: "", Users: []string{"M1"}, Count: 1},
		{Name: "eyes"},
	}}
	res := convertNative(t, m, numberedResolver{}, 0)
	p := res.Parts[0]
	if len(p.Reactions) != 2 || p.Reactions[0] != (NativeReaction{"🎉", "entra-M1"}) || p.Reactions[1] != (NativeReaction{"👋🏿", "entra-M2"}) {
		t.Errorf("native reactions %v", p.Reactions)
	}
	if want := "<p>x</p><p><em>Other reactions: 🎉 4 · :shipit-custom: 1</em></p>"; p.HTML != want {
		t.Errorf("HTML %s, want %s", p.HTML, want)
	}
}

// Without anyone to attribute a reaction to, native is footer.
func TestNativeReactionsWithoutResolver(t *testing.T) {
	m := slack.Message{Text: "x", Reactions: []slack.Reaction{{Name: "tada", Users: []string{"M1"}, Count: 1}}}
	native, err := Convert(m, nil, Options{AuthorMapped: true, Reactions: ReactionsNative})
	if err != nil {
		t.Fatal(err)
	}
	footer, err := Convert(m, nil, Options{AuthorMapped: true, Reactions: ReactionsFooter})
	if err != nil {
		t.Fatal(err)
	}
	if native.Parts[0].HTML != footer.Parts[0].HTML || len(native.Parts[0].Reactions) != 0 || native.Parts[0].FooterHTML != "" {
		t.Errorf("native without a resolver: %+v", native.Parts[0])
	}
}

func TestNativePartRefusals(t *testing.T) {
	footer := reactionFooter([]Reaction{{Emoji: "🎉", Count: 2}})[0]
	body := para(text("body"))
	native := []NativeReaction{{Emoji: "🎉", EntraID: "e"}}
	rest := []Reaction{{Emoji: "🎉", Count: 1}}

	if _, ok := nativePart([]*node{footer}, footer, native, nil, 1000); ok {
		t.Error("a part that would be empty without the summary was accepted")
	}
	if p, ok := nativePart([]*node{footer}, footer, native, rest, 1000); !ok || p.HTML != "<p><em>Other reactions: 🎉 1</em></p>" {
		t.Errorf("a part holding only the summary of the rest: %q %t", p.HTML, ok)
	}
	if _, ok := nativePart([]*node{footer, body}, footer, native, rest, 1000); ok {
		t.Error("a part that does not end in the summary was accepted")
	}
	if _, ok := nativePart([]*node{body, para(text("other"))}, footer, native, rest, 1000); ok {
		t.Error("a part without the summary was accepted")
	}
	if _, ok := nativePart(nil, footer, native, rest, 1000); ok {
		t.Error("an empty part was accepted")
	}
	// "Other reactions: " makes the shorter summary longer than the full
	// one here. With no room for it the full summary stays.
	full := len(renderPart([]*node{body, footer}).HTML)
	if _, ok := nativePart([]*node{body, footer}, footer, native, rest, full); ok {
		t.Error("a part that does not fit was accepted")
	}
	p, ok := nativePart([]*node{body, footer}, footer, native, rest, full+len("Other reactions: "))
	if !ok || p.FooterHTML != "<p>body</p><p><em>🎉 2</em></p>" || len(p.Reactions) != 1 {
		t.Errorf("part %+v, ok %t", p, ok)
	}
}

// A message that fills its part to the byte keeps the full summary and
// carries no reactions, with a warning.
func TestNativeReactionsNoRoom(t *testing.T) {
	reactions := []slack.Reaction{{Name: "tada", Users: []string{"M1", "N1"}, Count: 2}}
	base, err := Convert(slack.Message{Text: "x", Reactions: reactions}, numberedResolver{}, Options{AuthorMapped: true})
	if err != nil {
		t.Fatal(err)
	}
	pad := MinMaxBytes - len(base.Parts[0].HTML)
	m := slack.Message{Text: "x" + strings.Repeat("y", pad), Reactions: reactions}
	res := convertNative(t, m, numberedResolver{}, MinMaxBytes)
	if len(res.Parts) != 1 || len(res.Parts[0].HTML) != MinMaxBytes {
		t.Fatalf("%d parts, first of %d bytes; the test no longer fills a part exactly", len(res.Parts), len(res.Parts[0].HTML))
	}
	if len(res.Parts[0].Reactions) != 0 || !strings.HasSuffix(res.Parts[0].HTML, "<p><em>🎉 2</em></p>") {
		t.Errorf("reactions %v, HTML ends %q", res.Parts[0].Reactions, res.Parts[0].HTML[len(res.Parts[0].HTML)-40:])
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "reactions were summarized") {
		t.Errorf("warnings %q", res.Warnings)
	}
}

// What is sent when reactions are refused is exactly what the footer policy
// would have sent, for every message of every fixture, and the parts before
// the last do not depend on the policy.
func TestNativeFallbackIsFooter(t *testing.T) {
	for _, name := range fixtures.Names() {
		t.Run(name, func(t *testing.T) {
			d := loadFixture(t, name)
			withNative := 0
			for _, m := range d.messages {
				if Classify(m).Disposition == Skip {
					continue
				}
				for _, limit := range []int{0, MinMaxBytes} {
					footer, err := Convert(m, d.resolver, Options{AuthorMapped: true, Reactions: ReactionsFooter, MaxBytes: limit})
					if err != nil {
						t.Fatal(err)
					}
					native, err := Convert(m, d.resolver, Options{AuthorMapped: true, Reactions: ReactionsNative, MaxBytes: limit})
					if err != nil {
						t.Fatal(err)
					}
					if len(native.Parts) != len(footer.Parts) {
						t.Fatalf("%v: %d parts with native, %d with footer", m, len(native.Parts), len(footer.Parts))
					}
					for i := range native.Parts {
						n, f := native.Parts[i], footer.Parts[i]
						sent := n.HTML
						if n.FooterHTML != "" {
							sent = n.FooterHTML
							withNative++
						}
						if sent != f.HTML {
							t.Fatalf("%v part %d: fallback differs from the footer policy\n%s\n%s", m, i, sent, f.HTML)
						}
					}
				}
			}
			if name != "small" && withNative == 0 {
				t.Error("no message of the fixture carries native reactions")
			}
		})
	}
}
