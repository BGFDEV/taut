package preflight

import (
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/bgfdev/taut/internal/identity"
	"github.com/bgfdev/taut/internal/slack"
	"github.com/bgfdev/taut/internal/slack/export"
	"github.com/bgfdev/taut/internal/transform"
	"github.com/bgfdev/taut/internal/ui/sanitize"
)

// Direct messages and group DMs go into Teams chats. Which of
// them migrate is decided here, in one place: `taut preflight` reports it and
// the plan stage of a run executes it.
//
// A conversation migrates only if every one of its participants can be a
// participant of the chat as themselves: a full, current, human member of
// the Slack workspace with a Microsoft 365 account of their own. The
// account that stands in for unmapped authors is never put into a chat;
// that would give whoever holds it a private conversation to read. The
// conversation is left out instead, with its reason.

// MaxChatParticipants is the most people Teams allows in one chat.
const MaxChatParticipants = 250

// slackbotID is Slack's built-in bot, with which everybody has a DM.
const slackbotID = "USLACKBOT"

// Reasons a direct-message conversation is not migrated. The codes are
// stable; they appear in `taut preflight --json` and `taut map --json`.
const (
	DMSkipNoParticipants  = "no_participants"
	DMSkipSelf            = "self"
	DMSkipSlackbot        = "slackbot"
	DMSkipTooMany         = "too_many_participants"
	DMSkipUnknownUser     = "participant_not_in_export"
	DMSkipBot             = "participant_is_bot"
	DMSkipExternal        = "participant_is_external"
	DMSkipGuest           = "participant_is_guest"
	DMSkipDeactivated     = "participant_deactivated"
	DMSkipUnmapped        = "participant_unmapped"
	DMSkipAccountDisabled = "participant_account_disabled"
	DMSkipSameAccount     = "participants_share_account"
	DMSkipAccountGuest    = "participant_account_is_guest"
	DMSkipAccountUnclear  = "participant_account_ambiguous"
	DMSkipAccountGuessed  = "participant_account_guessed"
	DMSkipSameChat        = "same_chat_as_another_conversation"
	DMSkipNoMessages      = "no_messages"
)

// dmSkipText says each reason in words, for a sentence that begins "N
// conversations: …". The order is the order they are reported in.
var dmSkipText = []struct{ code, text string }{
	{DMSkipSelf, "a conversation with oneself"},
	{DMSkipSlackbot, "a conversation with Slackbot"},
	{DMSkipBot, "a participant is a bot or an app"},
	// A guest sees what they were let into. A chat is not that: it would
	// give them more than they had.
	{DMSkipGuest, "a participant is a guest in Slack"},
	{DMSkipExternal, "a participant belongs to another organisation (Slack Connect)"},
	{DMSkipDeactivated, "a participant's Slack account is deactivated"},
	{DMSkipUnknownUser, "a participant is not in the export's list of users"},
	{DMSkipUnmapped, "a participant has no Microsoft 365 account"},
	{DMSkipAccountDisabled, "a participant's Microsoft 365 account is disabled"},
	{DMSkipAccountGuest, "a participant's Microsoft 365 account is a guest account, not an account of their own in the tenant"},
	{DMSkipAccountUnclear, "a participant's address belongs to more than one Microsoft 365 account; map them by object ID in the override CSV (identity.overrides)"},
	{DMSkipAccountGuessed, "a participant was matched to a Microsoft 365 account by name (identity.match), not by email address; confirm the match in the override CSV (identity.overrides)"},
	{DMSkipSameAccount, "two participants map to the same Microsoft 365 account"},
	{DMSkipSameChat, "several Slack conversations have the same Microsoft 365 participants, because different Slack users map to one account; fix the identity mapping (identity.overrides) so each person has an account of their own"},
	{DMSkipTooMany, "more people than a Teams chat allows (250)"},
	{DMSkipNoParticipants, "the export lists no participants"},
	{DMSkipNoMessages, "no messages in the selected date range"},
}

// DMSkipReason returns the reason for a skip code in words.
func DMSkipReason(code string) string {
	for _, r := range dmSkipText {
		if r.code == code {
			return r.text
		}
	}
	return code
}

// DMPlan is one direct-message conversation and what becomes of it.
type DMPlan struct {
	SlackID string `json:"slackId"`
	// Kind is "dm" (two people) or "mpim" (a group DM).
	Kind string `json:"kind"`
	// Label names the conversation by its participants. It is for the
	// mapping view and the run's own ledger; a report never carries it,
	// since who talks to whom is nobody's business but theirs.
	Label        string `json:"-"`
	Participants int    `json:"participants"`
	// Skip is why the conversation is not migrated (a DMSkip… code), or
	// empty if it is. Reason says the same in words.
	Skip   string `json:"skip,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Topic is the name the group chat is created with; usually empty.
	Topic string `json:"-"`

	// Counted by Run for conversations that migrate. They add up to the
	// report's totals and are not reported per conversation: how much two
	// people wrote to each other, and when, is theirs as well.
	Messages int        `json:"-"`
	Replies  int        `json:"-"`
	Files    int        `json:"-"`
	First    *time.Time `json:"-"`
	Last     *time.Time `json:"-"`

	// Channel is the conversation in the export; Members the Slack IDs of
	// its participants, without duplicates, in a stable order.
	Channel slack.Channel `json:"-"`
	Members []string      `json:"-"`
	imports int
}

// Migrates reports whether the conversation will be imported.
func (d DMPlan) Migrates() bool { return d.Skip == "" }

// DMReasonCount is how many conversations are skipped for one reason.
type DMReasonCount struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

// DMReport is the "Direct messages" part of a preflight report.
type DMReport struct {
	// Enabled is target.directMessages.enabled.
	Enabled bool `json:"enabled"`
	// InExport is how many direct-message conversations the export holds.
	InExport int `json:"inExport"`
	// OneOnOne and Group count the conversations that migrate.
	OneOnOne int `json:"oneOnOne"`
	Group    int `json:"group"`
	Messages int `json:"messages"`
	// Replies are thread replies, which become ordinary messages: a chat
	// has no threads. Files are file references, none of which is moved.
	Replies int `json:"threadReplies"`
	Files   int `json:"files"`
	// IdentityChecked is false when people could not be matched against
	// Entra, so nobody could be ruled out for lacking an account.
	IdentityChecked bool            `json:"identityChecked"`
	Skipped         int             `json:"skipped"`
	SkippedByReason []DMReasonCount `json:"skippedByReason"`
	// Conversations lists every conversation, migrating ones first, by
	// Slack ID, kind, number of participants and the reason it is left
	// out, if it is. It is empty when direct messages are not migrated:
	// then only the count above is reported.
	Conversations []DMPlan `json:"conversations"`
}

func (d *DMPlan) skip(code string) {
	d.Skip, d.Reason = code, DMSkipReason(code)
}

// DirectMessages decides, for every DM and group DM of an export, whether
// it migrates and with whom. checked says whether idmap was resolved
// against the directory; without that nobody is ruled out for lacking a
// Microsoft 365 account (an offline preflight).
//
// It does not look at messages. A conversation that passes here is still
// left out if it has nothing to import in the config file's date range:
// see HasImportable.
func DirectMessages(channels []slack.Channel, users []slack.User, idmap *identity.Map, checked bool) []DMPlan {
	byID := make(map[string]slack.User, len(users))
	for _, u := range users {
		byID[u.ID] = u
	}
	var out []DMPlan
	for _, ch := range channels {
		if ch.Kind != slack.KindDM && ch.Kind != slack.KindMPIM {
			continue
		}
		d := DMPlan{SlackID: ch.ID, Kind: string(ch.Kind), Channel: ch}
		seen := map[string]bool{}
		for _, id := range ch.Members {
			if id = strings.TrimSpace(id); id != "" && !seen[id] {
				seen[id] = true
				d.Members = append(d.Members, id)
			}
		}
		sort.Strings(d.Members)
		d.Participants = len(d.Members)
		d.Label = dmLabel(d.Members, byID, ch)
		if ch.Kind == slack.KindMPIM {
			d.Topic = groupTopic(ch)
		}
		if code := dmSkip(d.Members, byID, idmap, checked); code != "" {
			d.skip(code)
		}
		out = append(out, d)
	}
	if !checked {
		return out
	}
	// Two Slack conversations whose participants are the same Microsoft
	// 365 accounts would be imported into one chat (there is one
	// one-on-one chat per pair, and a group chat is found again by its
	// members), their messages interleaved as if they were one
	// conversation. That happens when different Slack users map to one
	// account. None of them migrates until the mapping is fixed.
	byAccounts := map[string][]int{}
	for i, d := range out {
		if !d.Migrates() {
			continue
		}
		ids := make([]string, 0, len(d.Members))
		for _, m := range d.Members {
			ids = append(ids, strings.ToLower(idmap.Lookup(m).EntraID))
		}
		sort.Strings(ids)
		key := strings.Join(ids, ",")
		byAccounts[key] = append(byAccounts[key], i)
	}
	for _, same := range byAccounts {
		if len(same) < 2 {
			continue
		}
		for _, i := range same {
			out[i].skip(DMSkipSameChat)
		}
	}
	return out
}

// dmSkip returns why a conversation with these participants cannot become
// a chat, or "".
func dmSkip(members []string, byID map[string]slack.User, idmap *identity.Map, checked bool) string {
	switch {
	case len(members) == 0:
		return DMSkipNoParticipants
	case containsString(members, slackbotID):
		return DMSkipSlackbot
	case len(members) == 1:
		return DMSkipSelf
	case len(members) > MaxChatParticipants:
		return DMSkipTooMany
	}
	// What a person is in Slack comes first: it is true whatever the
	// directory says.
	for _, id := range members {
		u, known := byID[id]
		switch {
		case !known:
			return DMSkipUnknownUser
		case u.IsBot || u.IsAppUser:
			return DMSkipBot
		case u.IsStranger:
			return DMSkipExternal
		case u.IsGuest():
			return DMSkipGuest
		case u.Deleted:
			return DMSkipDeactivated
		}
	}
	if !checked {
		return ""
	}
	accounts := map[string]bool{}
	for _, id := range members {
		e := idmap.Lookup(id)
		switch {
		case !e.Mapped() || e.EntraID == "":
			return DMSkipUnmapped
		case e.Ambiguous:
			return DMSkipAccountUnclear
		case e.Method.Weak():
			// A guess is good enough to put a name on a message. It is not
			// good enough to show someone a private conversation.
			return DMSkipAccountGuessed
		case e.TargetDisabled:
			return DMSkipAccountDisabled
		case e.TargetGuest:
			return DMSkipAccountGuest
		}
		accounts[strings.ToLower(e.EntraID)] = true
	}
	if len(accounts) != len(members) {
		return DMSkipSameAccount
	}
	return ""
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// dmLabelNames is how many participants a label names before it counts the
// rest.
const dmLabelNames = 3

// dmLabel names a conversation by its participants: "Ada Abbott, Ben
// Banerjee", or for a larger group the first few and a count. Names are
// what Slack shows, cleaned of control characters; someone the export does
// not know is shown by ID.
func dmLabel(members []string, byID map[string]slack.User, ch slack.Channel) string {
	if len(members) == 0 {
		return sanitize.Text(ch.ID)
	}
	names := make([]string, 0, len(members))
	for _, id := range members {
		name := id
		if u, ok := byID[id]; ok {
			name = u.DisplayLabel()
		} else if id == slackbotID {
			name = "Slackbot"
		}
		names = append(names, sanitize.Text(name))
	}
	sort.Strings(names)
	if len(names) == 1 {
		return names[0] + " (alone)"
	}
	if len(names) > dmLabelNames+1 {
		rest := len(names) - dmLabelNames
		return strings.Join(names[:dmLabelNames], ", ") + " and " + Comma(int64(rest)) + " more"
	}
	return strings.Join(names, ", ")
}

// maxTopic is the longest chat topic Taut sets.
const maxTopic = 200

// groupTopic returns the name to give a group chat: what people called the
// group DM in Slack, if they did. Slack names every group DM
// "mpdm-ada--ben--cy-1" and gives it the purpose "Group messaging with: …";
// neither is a name anybody chose, so neither is carried across.
func groupTopic(ch slack.Channel) string {
	candidates := []string{ch.Name, ch.Topic.Value, ch.Purpose.Value}
	for _, c := range candidates {
		c = strings.TrimSpace(sanitize.Text(c))
		lower := strings.ToLower(c)
		if c == "" || strings.HasPrefix(lower, "mpdm-") || strings.HasPrefix(lower, "group messaging") {
			continue
		}
		if rs := []rune(c); len(rs) > maxTopic {
			c = string(rs[:maxTopic])
		}
		return c
	}
	return ""
}

// HasImportable reports whether a conversation has at least one message
// that would be imported: inside the date range (since inclusive, until
// exclusive; zero means unbounded) and not of a kind that is skipped. It is
// the same test, by the same classifier, that the import itself applies, so
// a chat is never created, and never put into migration mode, for a
// conversation that turns out to have nothing to import.
func HasImportable(ex *export.Export, ch slack.Channel, since, until time.Time) (bool, error) {
	for msg, err := range ex.Messages(ch) {
		if err != nil {
			var me *export.MessageError
			if errors.As(err, &me) {
				continue
			}
			return false, err
		}
		ts := msg.Time()
		if (!since.IsZero() && ts.Before(since)) || (!until.IsZero() && !ts.Before(until)) {
			continue
		}
		if transform.Classify(msg).Disposition != transform.Skip {
			return true, nil
		}
	}
	return false, nil
}

// summarize fills the counts of a DM report from its conversations and
// orders them: migrating ones first, each group by label.
func (r *DMReport) summarize() {
	byCode := map[string]int{}
	r.OneOnOne, r.Group, r.Messages, r.Replies, r.Files, r.Skipped = 0, 0, 0, 0, 0, 0
	for _, d := range r.Conversations {
		if !d.Migrates() {
			r.Skipped++
			byCode[d.Skip]++
			continue
		}
		if d.Kind == string(slack.KindMPIM) {
			r.Group++
		} else {
			r.OneOnOne++
		}
		r.Messages += d.Messages
		r.Replies += d.Replies
		r.Files += d.Files
	}
	r.SkippedByReason = []DMReasonCount{}
	for _, t := range dmSkipText {
		if n := byCode[t.code]; n > 0 {
			r.SkippedByReason = append(r.SkippedByReason, DMReasonCount{Code: t.code, Reason: t.text, Count: n})
		}
	}
	sort.SliceStable(r.Conversations, func(i, j int) bool {
		a, b := r.Conversations[i], r.Conversations[j]
		if a.Migrates() != b.Migrates() {
			return a.Migrates()
		}
		if a.Label != b.Label {
			return a.Label < b.Label
		}
		return a.SlackID < b.SlackID
	})
}
