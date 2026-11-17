package transform

import (
	"testing"

	"github.com/bgfdev/taut/internal/slack"
)

func TestClassify(t *testing.T) {
	system := []string{
		"channel_join", "channel_leave", "channel_topic", "channel_purpose", "channel_name",
		"channel_archive", "channel_unarchive", "group_join", "group_leave", "group_topic",
		"group_purpose", "group_name", "group_archive", "group_unarchive",
		"channel_convert_to_private", "channel_convert_to_public", "channel_posting_permissions",
		"bot_add", "bot_remove", "reminder_add", "unpinned_item", "joiner_notification",
		"joiner_notification_for_inviter", "sh_room_created", "message_replied", "ekm_access_denied",
	}
	content := []string{
		"", "bot_message", "me_message", "thread_broadcast", "file_share", "file_comment",
		"file_mention", "file_reply", "slackbot_response", "assistant_app_thread", "document_mention",
	}
	type tc struct {
		subtype string
		hidden  bool
		want    Class
	}
	cases := []tc{
		{"pinned_item", false, Class{Skip, "pins"}},
		{"pinned_item", true, Class{Skip, "pins"}},
		{"message_changed", false, Class{Skip, "edit_log"}},
		{"message_changed", true, Class{Skip, "edit_log"}},
		{"message_deleted", false, Class{Skip, "deleted"}},
		{"message_deleted", true, Class{Skip, "deleted"}},
		{"tombstone", false, Class{Placeholder, "tombstones"}},
		{"tombstone", true, Class{Placeholder, "tombstones"}}, // subtype wins over hidden
		{"huddle_thread", false, Class{Placeholder, "huddles"}},
		{"huddle_thread", true, Class{Placeholder, "huddles"}},
		{"taut_fixture_unknown_subtype", false, Class{Import, "unknown_subtype"}},
		{"taut_fixture_unknown_subtype", true, Class{Skip, "hidden"}}, // hidden wins over unknown
	}
	for _, s := range system {
		cases = append(cases, tc{s, false, Class{Skip, "system"}}, tc{s, true, Class{Skip, "system"}})
		if !IsSystemSubtype(s) || IsContentSubtype(s) {
			t.Errorf("%q: IsSystemSubtype=%t IsContentSubtype=%t", s, IsSystemSubtype(s), IsContentSubtype(s))
		}
	}
	for _, s := range content {
		cases = append(cases, tc{s, false, Class{Import, ""}}, tc{s, true, Class{Skip, "hidden"}})
		if IsSystemSubtype(s) || !IsContentSubtype(s) {
			t.Errorf("%q: IsSystemSubtype=%t IsContentSubtype=%t", s, IsSystemSubtype(s), IsContentSubtype(s))
		}
	}
	if len(system) != len(systemSubtypes) || len(content) != len(contentSubtypes) {
		t.Errorf("the test lists %d system and %d content subtypes; the package has %d and %d",
			len(system), len(content), len(systemSubtypes), len(contentSubtypes))
	}
	for _, s := range []string{"pinned_item", "message_changed", "message_deleted", "tombstone", "huddle_thread", "nope"} {
		if IsSystemSubtype(s) || IsContentSubtype(s) {
			t.Errorf("%q is in a subtype set", s)
		}
	}
	for _, c := range cases {
		got := Classify(slack.Message{Subtype: c.subtype, Hidden: c.hidden, Text: "x"})
		if got != c.want {
			t.Errorf("Classify(subtype=%q hidden=%t) = %+v, want %+v", c.subtype, c.hidden, got, c.want)
		}
	}
	if Import.String() != "import" || Placeholder.String() != "placeholder" || Skip.String() != "skip" {
		t.Error("Disposition.String")
	}
}
