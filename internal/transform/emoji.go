package transform

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// slackEmojiAliases maps shortcodes Slack uses to the gemoji name of the same
// emoji, where the two differ. Every target must exist in emojiTable; a test
// checks that.
var slackEmojiAliases = map[string]string{
	"simple_smile":           "slightly_smiling_face",
	"thinking_face":          "thinking",
	"face_with_rolling_eyes": "roll_eyes",
	"hugging_face":           "hugs",
	"robot_face":             "robot",
	"face_with_monocle":      "monocle_face",
	"face_palm":              "facepalm",
	"the_horns":              "metal",
	"sign_of_the_horns":      "metal",
	"hand_with_index_and_middle_fingers_crossed": "crossed_fingers",
	"i_love_you_hand_sign":                       "love_you_gesture",
	"spock-hand":                                 "vulcan_salute",
	"face_with_raised_eyebrow":                   "raised_eyebrow",
	"rolling_on_the_floor_laughing":              "rofl",
	"smiling_face_with_3_hearts":                 "smiling_face_with_three_hearts",
	"face_vomiting":                              "vomiting_face",
	"face_with_symbols_on_mouth":                 "cursing_face",
	"face_with_hand_over_mouth":                  "hand_over_mouth",
	"white_frowning_face":                        "frowning_face",
	"lion_face":                                  "lion",
	"unicorn_face":                               "unicorn",
	"face_with_cowboy_hat":                       "cowboy_hat_face",
	"star-struck":                                "star_struck",
	"grinning_face_with_star_eyes":               "star_struck",
	"waving_hand":                                "wave",
	"party_popper":                               "tada",
	"ladybug":                                    "lady_beetle",
	"reversed_hand_with_middle_finger_extended":  "middle_finger",
	"shushing_face_with_finger":                  "shushing_face",
	"thumbs_up":                                  "+1",
	"thumbs_down":                                "-1",
}

// emojiBase resolves a shortcode without colons or skin tone to its Unicode
// string and the gemoji name it was found under.
func emojiBase(name string) (emoji, canonical string, ok bool) {
	if e, ok := emojiTable[name]; ok {
		return e, name, true
	}
	if alias, ok := slackEmojiAliases[name]; ok {
		if e, ok := emojiTable[alias]; ok {
			return e, alias, true
		}
	}
	// Slack writes flags as flag-xx; they are pairs of regional indicators.
	if cc, found := strings.CutPrefix(name, "flag-"); found && len(cc) == 2 &&
		cc[0] >= 'a' && cc[0] <= 'z' && cc[1] >= 'a' && cc[1] <= 'z' {
		return string([]rune{0x1F1E6 + rune(cc[0]-'a'), 0x1F1E6 + rune(cc[1]-'a')}), "", true
	}
	// Slack hyphenates and says male-/female- where gemoji uses underscores
	// and man_/woman_: man-shrugging, female-technologist.
	if strings.Contains(name, "-") {
		alt := strings.ReplaceAll(name, "-", "_")
		if e, ok := emojiTable[alt]; ok {
			return e, alt, true
		}
		for _, p := range [][2]string{{"male_", "man_"}, {"female_", "woman_"}} {
			if rest, found := strings.CutPrefix(alt, p[0]); found {
				if e, ok := emojiTable[p[1]+rest]; ok {
					return e, p[1] + rest, true
				}
			}
		}
	}
	return "", "", false
}

// emojiTone returns the emoji for name with skin tone 2..6 applied. Bases
// that take no skin tone are returned unchanged.
func emojiTone(name string, tone int) (string, bool) {
	e, canonical, ok := emojiBase(name)
	if !ok {
		return "", false
	}
	if tone < 2 || tone > 6 || !emojiToneBase[canonical] {
		return e, true
	}
	// The modifier follows the first code point and replaces an emoji
	// presentation selector there.
	_, w := utf8.DecodeRuneInString(e)
	rest := strings.TrimPrefix(e[w:], "️")
	return e[:w] + string(rune(0x1F3FB+tone-2)) + rest, true
}

// emojiFor resolves a shortcode as Slack writes it in reactions and text,
// without the outer colons: "wave" or "wave::skin-tone-3".
func emojiFor(code string) (string, bool) {
	name, toneCode, hasTone := strings.Cut(code, "::")
	if !hasTone {
		e, _, ok := emojiBase(name)
		return e, ok
	}
	tone := 0
	if d, found := strings.CutPrefix(toneCode, "skin-tone-"); found && len(d) == 1 && d[0] >= '2' && d[0] <= '6' {
		tone = int(d[0] - '0')
	}
	return emojiTone(name, tone)
}

// emojiFromHex decodes the "unicode" field of a rich_text emoji element:
// hexadecimal code points joined by hyphens, such as "1f44b-1f3fd".
func emojiFromHex(hex string) (string, bool) {
	if hex == "" || len(hex) > 128 {
		return "", false
	}
	var sb strings.Builder
	for _, part := range strings.Split(hex, "-") {
		v, err := strconv.ParseUint(part, 16, 32)
		if err != nil || v < 0x20 || v > utf8.MaxRune || (v >= 0xD800 && v <= 0xDFFF) || (v >= 0x7f && v < 0xa0) {
			return "", false
		}
		sb.WriteRune(rune(v))
	}
	return sb.String(), true
}

// isShortcodeByte reports whether c may appear in an emoji shortcode.
func isShortcodeByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '+' || c == '\'' || c == '-'
}

// matchShortcode looks for an emoji shortcode at the start of s, which must
// begin with ':'. It returns the emoji and the number of bytes consumed, or
// 0 if s does not start with a shortcode the table knows. A following
// ":skin-tone-N:" is consumed too, and applied when the emoji supports it.
func matchShortcode(s string) (emoji string, n int) {
	const maxName = 64
	i := 1
	for i < len(s) && i <= maxName && isShortcodeByte(s[i]) {
		i++
	}
	if i == 1 || i >= len(s) || s[i] != ':' {
		return "", 0
	}
	name := s[1:i]
	n = i + 1
	const tonePrefix = ":skin-tone-"
	if rest := s[n:]; len(rest) >= len(tonePrefix)+2 && strings.HasPrefix(rest, tonePrefix) &&
		rest[len(tonePrefix)] >= '2' && rest[len(tonePrefix)] <= '6' && rest[len(tonePrefix)+1] == ':' {
		if e, ok := emojiTone(name, int(rest[len(tonePrefix)]-'0')); ok {
			return e, n + len(tonePrefix) + 2
		}
		return "", 0
	}
	if e, _, ok := emojiBase(name); ok {
		return e, n
	}
	return "", 0
}
