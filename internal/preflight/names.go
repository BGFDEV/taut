package preflight

import (
	"strings"
	"unicode/utf8"
)

// Teams channel naming rules, from Microsoft's "Limits and specifications
// for Microsoft Teams". Slack allows names Teams rejects (a leading
// underscore, reserved words), so every Slack name is checked.
const (
	maxChannelName  = 50
	forbiddenChars  = "~#%&*{}+/\\:<>?|'\","
	teamsGeneral    = "General"
	maxChannelsTeam = 1000
)

var reservedNames = map[string]bool{
	"forms": true, "con": true, "conin$": true, "conout$": true, "prn": true, "aux": true, "nul": true,
	"desktop.ini": true, "_vti_": true,
}

func init() {
	for _, p := range []string{"com", "lpt"} {
		for i := '1'; i <= '9'; i++ {
			reservedNames[p+string(i)] = true
		}
	}
}

// TeamsChannelName converts a Slack channel name into one Teams accepts.
// It returns the name and, when it had to change, a short reason.
func TeamsChannelName(slackName string) (name, reason string) {
	name = slackName
	var reasons []string
	note := func(r string) {
		for _, x := range reasons {
			if x == r {
				return
			}
		}
		reasons = append(reasons, r)
	}

	// Forbidden characters become hyphens.
	if strings.ContainsAny(name, forbiddenChars) {
		name = strings.Map(func(r rune) rune {
			if strings.ContainsRune(forbiddenChars, r) {
				return '-'
			}
			return r
		}, name)
		note("contains characters Teams forbids")
	}
	for strings.Contains(name, "..") {
		name = strings.ReplaceAll(name, "..", ".")
		note("contains \"..\"")
	}
	if trimmed := strings.TrimLeft(name, "_. "); trimmed != name {
		name = trimmed
		note("cannot start with an underscore or period")
	}
	if trimmed := strings.TrimRight(name, ". "); trimmed != name {
		name = trimmed
		note("cannot end with a period")
	}
	if reservedNames[strings.ToLower(name)] || strings.Contains(strings.ToLower(name), "_vti_") {
		name += "-channel"
		note("is a reserved name in Teams")
	}
	if utf8.RuneCountInString(name) > maxChannelName {
		r := []rune(name)
		name = strings.TrimRight(string(r[:maxChannelName]), "-_. ")
		note("is longer than 50 characters")
	}
	if name == "" {
		name = "channel"
		note("has no usable characters")
	}
	return name, strings.Join(reasons, "; ")
}
