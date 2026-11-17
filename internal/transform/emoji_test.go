package transform

import (
	"testing"
	"unicode/utf8"
)

func TestEmojiTable(t *testing.T) {
	if len(emojiTable) < 1800 {
		t.Fatalf("emoji table has %d entries", len(emojiTable))
	}
	for name, e := range emojiTable {
		if e == "" || !utf8.ValidString(e) {
			t.Errorf("%q maps to an invalid string", name)
		}
		for i := 0; i < len(name); i++ {
			if !isShortcodeByte(name[i]) {
				t.Errorf("shortcode %q has a byte the mrkdwn scanner does not accept", name)
				break
			}
		}
	}
	for name := range emojiToneBase {
		if _, ok := emojiTable[name]; !ok {
			t.Errorf("tone base %q is not in the table", name)
		}
	}
	for alias, target := range slackEmojiAliases {
		if _, ok := emojiTable[target]; !ok {
			t.Errorf("Slack alias %q points at %q, which is not in the table", alias, target)
		}
		if _, ok := emojiTable[alias]; ok {
			t.Errorf("Slack alias %q is already a table entry", alias)
		}
	}
}

func TestEmojiLookup(t *testing.T) {
	for code, want := range map[string]string{
		"thumbsup": "👍", "+1": "👍", "simple_smile": "🙂", "slightly_smiling_face": "🙂",
		"white_check_mark": "✅", "heavy_check_mark": "✔️", "tada": "🎉", "pray": "🙏",
		"fire": "🔥", "eyes": "👀", "rocket": "🚀", "100": "💯", "raised_hands": "🙌",
		"wave": "👋", "thinking_face": "🤔", "joy": "😂", "sob": "😭", "heart": "❤️",
		"x": "❌", "warning": "⚠️", "coffee": "☕", "pushpin": "📌",
		"wave::skin-tone-2": "👋🏻", "+1::skin-tone-6": "👍🏿", "point_up::skin-tone-4": "☝🏽",
		"fire::skin-tone-3": "🔥", "wave::skin-tone-9": "👋", "wave::nonsense": "👋",
		"flag-de": "🇩🇪", "man-shrugging": "🤷‍♂️", "female-technologist": "👩‍💻",
		"man-shrugging::skin-tone-5": "🤷🏾‍♂️", "spock-hand": "🖖",
	} {
		if got, ok := emojiFor(code); !ok || got != want {
			t.Errorf("emojiFor(%q) = %q, %t; want %q", code, got, ok, want)
		}
	}
	for _, code := range []string{"", "partyparrot", "flag-", "flag-usa", "FIRE", "::skin-tone-2", "nope::skin-tone-2"} {
		if got, ok := emojiFor(code); ok {
			t.Errorf("emojiFor(%q) = %q, want no match", code, got)
		}
	}
	for hex, want := range map[string]string{"1f44b-1f3fd": "👋🏽", "2764-fe0f": "❤️", "1F680": "🚀"} {
		if got, ok := emojiFromHex(hex); !ok || got != want {
			t.Errorf("emojiFromHex(%q) = %q, %t; want %q", hex, got, ok, want)
		}
	}
	for _, hex := range []string{"", "zz", "d800", "110000", "1f44b-", "0", "3c", "1f", "9b"} {
		if hex == "3c" {
			continue // "<" is a legal code point; it is escaped on output like any text
		}
		if got, ok := emojiFromHex(hex); ok {
			t.Errorf("emojiFromHex(%q) = %q, want failure", hex, got)
		}
	}
}
