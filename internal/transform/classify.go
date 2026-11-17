package transform

import "github.com/bgfdev/taut/internal/slack"

// Disposition says what happens to a Slack message during a migration.
type Disposition int

// Dispositions.
const (
	Import      Disposition = iota // becomes a Teams post
	Placeholder                    // becomes a short placeholder post (deleted thread starter, huddle)
	Skip                           // is not migrated
)

// String returns the disposition's name.
func (d Disposition) String() string {
	switch d {
	case Import:
		return "import"
	case Placeholder:
		return "placeholder"
	case Skip:
		return "skip"
	}
	return "unknown"
}

// Category keys reported in Class.Key.
const (
	KeySystem         = "system"
	KeyPins           = "pins"
	KeyEditLog        = "edit_log"
	KeyDeleted        = "deleted"
	KeyHidden         = "hidden"
	KeyTombstones     = "tombstones"
	KeyHuddles        = "huddles"
	KeyUnknownSubtype = "unknown_subtype"
)

// Class is the result of Classify.
type Class struct {
	Disposition Disposition
	// Key is a stable category used in reports: "" for ordinary content, or
	// one of "system", "pins", "edit_log", "deleted", "hidden", "tombstones",
	// "huddles", "unknown_subtype".
	Key string
}

// Subtypes that are Slack housekeeping rather than conversation. They are
// not migrated.
var systemSubtypes = map[string]bool{
	"channel_join": true, "channel_leave": true, "channel_topic": true, "channel_purpose": true,
	"channel_name": true, "channel_archive": true, "channel_unarchive": true,
	"group_join": true, "group_leave": true, "group_topic": true, "group_purpose": true,
	"group_name": true, "group_archive": true, "group_unarchive": true,
	"channel_convert_to_private": true, "channel_convert_to_public": true, "channel_posting_permissions": true,
	"bot_add": true, "bot_remove": true, "reminder_add": true, "unpinned_item": true,
	"joiner_notification": true, "joiner_notification_for_inviter": true, "sh_room_created": true,
	"message_replied": true, "ekm_access_denied": true,
}

// Subtypes that carry ordinary content.
var contentSubtypes = map[string]bool{
	"": true, "bot_message": true, "me_message": true, "thread_broadcast": true, "file_share": true,
	"file_comment": true, "file_mention": true, "file_reply": true, "slackbot_response": true,
	"assistant_app_thread": true, "document_mention": true,
}

// IsSystemSubtype reports whether subtype marks Slack housekeeping (joins,
// topic changes, archiving and the like), which is never migrated.
func IsSystemSubtype(subtype string) bool { return systemSubtypes[subtype] }

// IsContentSubtype reports whether subtype is one known to carry ordinary
// message content. The empty subtype, a plain message, is one of them.
func IsContentSubtype(subtype string) bool { return contentSubtypes[subtype] }

// Classify decides what happens to a message. It is the single place that
// makes this decision, so a preflight report and the migration itself cannot
// disagree.
//
// The subtype is checked before the hidden flag: Slack marks edit and
// deletion records, and tombstones, as hidden too.
func Classify(m slack.Message) Class {
	switch {
	case m.Subtype == "pinned_item":
		return Class{Skip, KeyPins}
	case systemSubtypes[m.Subtype]:
		return Class{Skip, KeySystem}
	case m.Subtype == "message_changed":
		return Class{Skip, KeyEditLog}
	case m.Subtype == "message_deleted":
		return Class{Skip, KeyDeleted}
	case m.Subtype == "tombstone":
		return Class{Placeholder, KeyTombstones}
	case m.Subtype == "huddle_thread":
		return Class{Placeholder, KeyHuddles}
	case m.Hidden:
		return Class{Skip, KeyHidden}
	case !contentSubtypes[m.Subtype]:
		return Class{Import, KeyUnknownSubtype}
	}
	return Class{Import, ""}
}
