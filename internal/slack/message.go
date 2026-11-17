package slack

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// Message is one entry of a channel's day file.
//
// TS and ThreadTS are kept as the exact strings Slack wrote. Together with
// the channel they identify a message, so they must round-trip unchanged; use
// Time, ParseTS and CompareTS to interpret them.
type Message struct {
	Type     string `json:"type,omitempty"`
	Subtype  string `json:"subtype,omitempty"`
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts,omitempty"`

	User     string `json:"user,omitempty"`
	UserTeam string `json:"user_team,omitempty"`
	BotID    string `json:"bot_id,omitempty"`
	Username string `json:"username,omitempty"`
	// UserProfile is a snapshot of the author embedded in the message. It is
	// the only source of a name for authors absent from users.json.
	UserProfile *MessageProfile `json:"user_profile,omitempty"`

	Text string `json:"text,omitempty"`
	// Blocks and Attachments are kept raw for the transform stage.
	Blocks      json.RawMessage `json:"blocks,omitempty"`
	Attachments json.RawMessage `json:"attachments,omitempty"`

	Files     []File     `json:"files,omitempty"`
	Upload    bool       `json:"upload,omitempty"`
	Reactions []Reaction `json:"reactions,omitempty"`
	Edited    *Edited    `json:"edited,omitempty"`
	Hidden    bool       `json:"hidden,omitempty"`

	ReplyCount   int    `json:"reply_count,omitempty"`
	ParentUserID string `json:"parent_user_id,omitempty"`
}

// MessageProfile is the author snapshot Slack embeds in a message as
// "user_profile".
type MessageProfile struct {
	DisplayName string `json:"display_name,omitempty"`
	RealName    string `json:"real_name,omitempty"`
	Name        string `json:"name,omitempty"`
	Team        string `json:"team,omitempty"`
}

// DisplayLabel returns the best human-readable name in the snapshot, or ""
// if it holds none. It is safe to call on a nil profile.
func (p *MessageProfile) DisplayLabel() string {
	if p == nil {
		return ""
	}
	return firstNonBlank(p.DisplayName, p.RealName, p.Name)
}

// Time returns the message time in UTC with microsecond precision, parsed
// from TS with integer math. It is the zero time if TS is invalid.
func (m Message) Time() time.Time { return TSTime(m.TS) }

// IsThreadParent reports whether the message is the root of a thread.
func (m Message) IsThreadParent() bool { return m.ThreadTS != "" && m.ThreadTS == m.TS }

// IsReply reports whether the message is a reply inside a thread. Replies
// also broadcast to the channel (subtype "thread_broadcast") count.
func (m Message) IsReply() bool { return m.ThreadTS != "" && m.ThreadTS != m.TS }

// IsBot reports whether the message was posted by a bot or integration.
func (m Message) IsBot() bool { return m.BotID != "" || m.Subtype == "bot_message" }

// AuthorLabel returns the best author name available from the message alone:
// the embedded profile, then the bot username, then the user or bot ID.
// Prefer User.DisplayLabel when the author is in users.json.
func (m Message) AuthorLabel() string {
	return firstNonBlank(m.UserProfile.DisplayLabel(), m.Username, m.User, m.BotID)
}

// String identifies the message without its text, blocks or file URLs, so it
// is safe to log.
func (m Message) String() string {
	return fmt.Sprintf("message ts=%s user=%s subtype=%q files=%d", m.TS, firstNonBlank(m.User, m.BotID, "-"), m.Subtype, len(m.Files))
}

// GoString keeps %#v from printing message content or secret URLs.
func (m Message) GoString() string { return "slack.Message{" + m.String() + "}" }

// LogValue keeps structured logs from capturing content or secret URLs.
func (m Message) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("ts", m.TS),
		slog.String("user", firstNonBlank(m.User, m.BotID)),
		slog.String("subtype", m.Subtype),
		slog.Int("files", len(m.Files)),
	)
}
