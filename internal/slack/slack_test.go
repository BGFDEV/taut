package slack

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestParseTS(t *testing.T) {
	tests := []struct {
		in         string
		sec, micro int64
		ok         bool
	}{
		{"1709546400.000200", 1709546400, 200, true},
		{"1709546400.999999", 1709546400, 999999, true},
		{"1709546400", 1709546400, 0, true},
		{"12.5", 12, 500000, true},
		{"0.000001", 0, 1, true},
		{"", 0, 0, false},
		{".000200", 0, 0, false},
		{"1709546400.", 0, 0, false},
		{"1709546400.0000001", 0, 0, false},
		{"1709546400,000200", 0, 0, false},
		{"-1.000000", 0, 0, false},
		{"1e9", 0, 0, false},
		{"17095464001709546400.000000", 0, 0, false},
		{" 1709546400.000200", 0, 0, false},
	}
	for _, tt := range tests {
		sec, micro, ok := ParseTS(tt.in)
		if sec != tt.sec || micro != tt.micro || ok != tt.ok {
			t.Errorf("ParseTS(%q) = %d, %d, %v; want %d, %d, %v", tt.in, sec, micro, ok, tt.sec, tt.micro, tt.ok)
		}
	}
}

func TestMessageTimeIsExact(t *testing.T) {
	// 1709546400.123457 is not representable as a float64; integer parsing
	// must keep every microsecond.
	m := Message{TS: "1709546400.123457"}
	want := time.Date(2024, 3, 4, 10, 0, 0, 123457000, time.UTC)
	if got := m.Time(); !got.Equal(want) || got.Location() != time.UTC {
		t.Errorf("Time() = %v, want %v", got, want)
	}
	if got := (Message{TS: "bogus"}).Time(); !got.IsZero() {
		t.Errorf("Time() of invalid ts = %v, want zero", got)
	}
}

func TestCompareTS(t *testing.T) {
	ordered := []string{"", "garbage", "9.999999", "10.000000", "10.000001", "10.5", "1709546400.000999", "1709546400.001000"}
	for i, a := range ordered {
		for j, b := range ordered {
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := CompareTS(a, b); got != want {
				t.Errorf("CompareTS(%q, %q) = %d, want %d", a, b, got, want)
			}
		}
	}
	if CompareTS("10.5", "10.500000") != 0 {
		t.Error("equal instants written differently should compare equal")
	}
}

func TestThreadHelpers(t *testing.T) {
	parent := Message{TS: "1.000001", ThreadTS: "1.000001"}
	reply := Message{TS: "2.000001", ThreadTS: "1.000001"}
	plain := Message{TS: "3.000001"}
	if !parent.IsThreadParent() || parent.IsReply() {
		t.Error("parent misclassified")
	}
	if reply.IsThreadParent() || !reply.IsReply() {
		t.Error("reply misclassified")
	}
	if plain.IsThreadParent() || plain.IsReply() {
		t.Error("plain message misclassified")
	}
}

func TestDisplayLabels(t *testing.T) {
	tests := []struct {
		u    User
		want string
	}{
		{User{ID: "U1", Name: "ada", RealName: "Ada A", Profile: Profile{DisplayName: "ada!", RealName: "Ada Abbott"}}, "ada!"},
		{User{ID: "U1", Name: "ada", RealName: "Ada A", Profile: Profile{DisplayName: "  ", RealName: "Ada Abbott"}}, "Ada Abbott"},
		{User{ID: "U1", Name: "ada", RealName: "Ada A"}, "Ada A"},
		{User{ID: "U1", Name: "ada"}, "ada"},
		{User{ID: "U1"}, "U1"},
	}
	for _, tt := range tests {
		if got := tt.u.DisplayLabel(); got != tt.want {
			t.Errorf("DisplayLabel() = %q, want %q", got, tt.want)
		}
	}

	var nilProfile *MessageProfile
	if nilProfile.DisplayLabel() != "" {
		t.Error("nil profile should have an empty label")
	}
	m := Message{User: "U9", UserProfile: &MessageProfile{RealName: "Kai Kowalski"}}
	if got := m.AuthorLabel(); got != "Kai Kowalski" {
		t.Errorf("AuthorLabel() = %q", got)
	}
	if got := (Message{BotID: "B1", Username: "jenkins"}).AuthorLabel(); got != "jenkins" {
		t.Errorf("AuthorLabel() for bot = %q", got)
	}
}

func TestChannelFolder(t *testing.T) {
	tests := []struct {
		c    Channel
		want string
	}{
		{Channel{ID: "C1", Name: "general", Kind: KindPublic}, "general"},
		{Channel{ID: "G1", Name: "mpdm-a--b-1", Kind: KindMPIM}, "mpdm-a--b-1"},
		{Channel{ID: "D1", Kind: KindDM}, "D1"},
		{Channel{ID: "D1", Name: "ignored", Kind: KindDM}, "D1"},
		{Channel{ID: "C2", Kind: KindPublic}, "C2"},
	}
	for _, tt := range tests {
		if got := tt.c.Folder(); got != tt.want {
			t.Errorf("Folder() = %q, want %q", got, tt.want)
		}
	}
}

const secretURL = "https://files.slack.com/files-pri/T0FIXTURE-F0FX1/plan.pdf?t=xoxe-FAKE-fixture-not-a-secret#frag"

func TestSafeURL(t *testing.T) {
	f := File{URLPrivate: secretURL}
	if got, want := f.SafeURL(), "https://files.slack.com/files-pri/T0FIXTURE-F0FX1/plan.pdf"; got != want {
		t.Errorf("SafeURL() = %q, want %q", got, want)
	}
	if got := (File{URLPrivateDownload: secretURL}).SafeURL(); strings.Contains(got, "xoxe") || got == "" {
		t.Errorf("SafeURL() fallback = %q", got)
	}
	if got := RedactURL("https://user:hunter2@files.slack.com/x?t=1"); got != "https://files.slack.com/x" {
		t.Errorf("RedactURL with userinfo = %q", got)
	}
	if got := RedactURL("::not a url@host?t=xoxe-FAKE"); strings.Contains(got, "xoxe") {
		t.Errorf("RedactURL of junk leaked: %q", got)
	}
	if got := (File{}).SafeURL(); got != "" {
		t.Errorf("SafeURL() of empty file = %q", got)
	}
}

func TestFormattingNeverLeaksFileURLs(t *testing.T) {
	f := File{ID: "F1", Name: "plan.pdf", Mode: FileModeHosted, Size: 10, URLPrivate: secretURL, URLPrivateDownload: secretURL, Permalink: secretURL}
	m := Message{TS: "1.000001", User: "U1", Text: "see " + secretURL, Files: []File{f}, Blocks: json.RawMessage(`["` + secretURL + `"]`)}

	var logged strings.Builder
	log := slog.New(slog.NewTextHandler(&logged, nil))
	log.Info("x", "file", f, "msg", m, "files", m.Files)

	outputs := []string{
		f.String(), fmt.Sprint(f), fmt.Sprintf("%v %+v %#v %s", f, f, f, f), fmt.Sprintf("%v %+v", &f, []File{f}),
		m.String(), fmt.Sprintf("%v %+v %#v %s", m, m, m, m), fmt.Sprintf("%v %+v", &m, []Message{m}),
		fmt.Sprintf("%v %+v", struct{ F File }{f}, struct{ M *Message }{&m}),
		logged.String(),
	}
	for i, out := range outputs {
		if strings.Contains(out, "xoxe") || strings.Contains(out, "?t=") {
			t.Errorf("output %d leaks the token: %s", i, out)
		}
	}
}

func TestFileFlags(t *testing.T) {
	var files []File
	in := `[{"id":"F1","mode":"hidden_by_limit"},{"id":"F2","hidden_by_limit":true},{"id":"F3","mode":"tombstone"},
		{"id":"F4","mode":"space","url_private":"https://x/y"},{"id":"F5","mode":"external","is_external":true,"url_private":"https://docs.google.com/x"},
		{"id":"F6","mode":"hosted","url_private_download":"https://x/z","size":5368709120}]`
	if err := json.Unmarshal([]byte(in), &files); err != nil {
		t.Fatal(err)
	}
	if !files[0].IsHidden() || !files[1].IsHidden() || files[2].IsHidden() {
		t.Error("IsHidden wrong")
	}
	if !files[2].IsTombstone() || !files[3].IsCanvas() {
		t.Error("IsTombstone/IsCanvas wrong")
	}
	wantDownloadable := []bool{false, false, false, true, false, true}
	for i, f := range files {
		if f.Downloadable() != wantDownloadable[i] {
			t.Errorf("%s: Downloadable() = %v", f.ID, f.Downloadable())
		}
	}
	if files[5].Size != 5<<30 {
		t.Errorf("5 GB size = %d", files[5].Size)
	}
}

func TestMessageDecodesLeniently(t *testing.T) {
	// Unknown fields are ignored, absent ones are zero, raw fields stay raw.
	in := `{"ts":"1709546400.000200","never_seen_before":{"a":[1,2]},"blocks":[{"type":"rich_text"}],"attachments":[{"id":1}],
		"user_profile":{"display_name":"","real_name":"Kai"},"edited":{"user":"U1","ts":"1709546401.000000"},
		"reactions":[{"name":"tada","users":["U1"],"count":1}],"reply_count":2,"upload":true,"hidden":true}`
	var m Message
	if err := json.Unmarshal([]byte(in), &m); err != nil {
		t.Fatal(err)
	}
	if m.TS != "1709546400.000200" || string(m.Blocks) != `[{"type":"rich_text"}]` || string(m.Attachments) != `[{"id":1}]` {
		t.Errorf("unexpected decode: %+v blocks=%s", m, m.Blocks)
	}
	if m.Edited == nil || m.Edited.TS != "1709546401.000000" || len(m.Reactions) != 1 || m.ReplyCount != 2 || !m.Upload || !m.Hidden {
		t.Errorf("fields lost: %+v", m)
	}
	if m.UserProfile.DisplayLabel() != "Kai" {
		t.Errorf("profile label = %q", m.UserProfile.DisplayLabel())
	}
	var empty Message
	if err := json.Unmarshal([]byte(`{}`), &empty); err != nil {
		t.Fatal(err)
	}
}
