package worker

import (
	"errors"
	"fmt"
	"testing"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/slack/export"
)

func TestEntryErrors(t *testing.T) {
	entry := func(i int) error {
		return &export.MessageError{Channel: "c", Day: "2024-03-04", Index: i, Err: errors.New("no ts")}
	}
	fileBroken := fail.New("export.day_unreadable", "day file is truncated", "", "re-export")

	n, other := entryErrors(errors.Join(entry(1), entry(4)))
	if n != 2 || other != nil {
		t.Fatalf("only entry errors: n=%d other=%v", n, other)
	}

	// A truncated file reports its bad entries AND the break. That must
	// not be mistaken for "a few unreadable messages".
	n, other = entryErrors(errors.Join(entry(1), fileBroken))
	if n != 1 || other == nil {
		t.Fatalf("truncated file: n=%d other=%v; the file-level error was swallowed", n, other)
	}
	n, other = entryErrors(errors.Join(errors.Join(entry(1), entry(2)), fmt.Errorf("wrapped: %w", fileBroken)))
	if n != 2 || other == nil {
		t.Fatalf("nested: n=%d other=%v", n, other)
	}
	if _, other := entryErrors(fileBroken); other == nil {
		t.Fatal("a bare file error must be reported")
	}
}

func TestRejectedClassification(t *testing.T) {
	code := func(c string) error { return fail.New(c, "x", "", "") }
	cases := []struct {
		err     error
		isReply bool
		want    bool
	}{
		{code("graph.payload_too_large"), false, true},
		{code("graph.invalid_author"), true, true},
		{code("graph.not_found"), true, true},   // the parent is gone
		{code("graph.not_found"), false, false}, // the channel is gone: not about the message
		{code("graph.bad_request"), false, false},
		{code("graph.throttled"), false, false},
		{code("graph.forbidden"), false, false},
		{errors.New("plain"), false, false},
	}
	for _, c := range cases {
		if got := rejected(c.err, c.isReply); got != c.want {
			t.Errorf("rejected(%v, reply=%v) = %v, want %v", c.err, c.isReply, got, c.want)
		}
	}
}

func TestClip(t *testing.T) {
	if got := clip("a\x00b\xffc"); got != "ab?c" {
		t.Errorf("clip = %q", got)
	}
	long := make([]byte, 5000)
	for i := range long {
		long[i] = 'x'
	}
	if len(clip(string(long))) != 1000 {
		t.Error("long reasons must be bounded")
	}
}

func TestValidSlackFileURL(t *testing.T) {
	ok := []string{
		"https://files.slack.com/files-pri/T024BE7LD-F024BERPE/download/report.pdf?t=xoxe-1",
		"https://files.slack.com/files-tmb/T0-F0-abc/image_480.png",
		"https://a.b.slack-files.com/files-pri/T0-F0/x",
	}
	bad := []string{
		"http://files.slack.com/files-pri/T0-F0/x",              // not https
		"https://slack.com/api/conversations.history?channel=C", // the Web API, not a file
		"https://files.slack.com/api/files.list",                // file host, wrong path
		"https://files.slack.com.evil.example/files-pri/T0-F0/x",
		"https://evilslack-files.com/files-pri/T0-F0/x",
		"https://user:pw@files.slack.com/files-pri/T0-F0/x",
		"https://files.slack.com:8443/files-pri/T0-F0/x",
		"https://169.254.169.254/latest/meta-data/",
		"file:///etc/passwd",
		"//files.slack.com/files-pri/T0-F0/x",
		"",
	}
	for _, u := range ok {
		if !ValidSlackFileURL(u) {
			t.Errorf("rejected a genuine Slack file URL: %s", u)
		}
	}
	for _, u := range bad {
		if ValidSlackFileURL(u) {
			t.Errorf("accepted %q", u)
		}
	}
}

// A "not migrated" note must never be longer than the marker it replaces,
// whatever the file is called.
func TestNoteFitsReservedSpace(t *testing.T) {
	const reserved = len(`<attachment id="taut-file:`) + 64 + len(`"></attachment>`)
	for _, name := range []string{
		"", "report.pdf", "a & b <c> \"d\".txt",
		"非常に長い名前のファイルでございますこれはテストですよろしくお願いします非常に長い名前.xlsx",
		"😀😀😀😀😀😀😀😀😀😀😀😀😀😀😀😀😀😀😀😀😀😀😀😀😀.png",
		string(make([]byte, 500)),
	} {
		note := "<em>📎 " + shortName(name) + " (not migrated)</em>"
		if len(note) > reserved {
			t.Errorf("note for %q is %d bytes, more than the %d reserved", name, len(note), reserved)
		}
		for _, c := range []string{"<c>", "&", "\""} {
			if name != "" && len(name) < 40 && c != "" && contains(shortName(name), c) {
				t.Errorf("shortName(%q) kept %q, which HTML would need escaped", name, c)
			}
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
