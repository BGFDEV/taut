package export_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/fixtures"
	"github.com/bgfdev/taut/internal/slack"
	"github.com/bgfdev/taut/internal/slack/export"
)

// zipDir holds fixture archives shared by all tests; writing the larger ones
// once keeps the suite fast under the race detector.
var (
	zipDir   string
	zipPaths sync.Map // name -> func() (string, error)
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "taut-export-test-")
	if err != nil {
		panic(err)
	}
	zipDir = dir
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func openZip(t *testing.T, name string, opts ...fixtures.Option) *export.Export {
	t.Helper()
	if len(opts) > 0 {
		path, err := fixtures.Write(name, t.TempDir(), opts...)
		if err != nil {
			t.Fatal(err)
		}
		return open(t, path)
	}
	write, _ := zipPaths.LoadOrStore(name, sync.OnceValues(func() (string, error) {
		return fixtures.Write(name, zipDir)
	}))
	path, err := write.(func() (string, error))()
	if err != nil {
		t.Fatal(err)
	}
	return open(t, path)
}

func open(t *testing.T, path string) *export.Export {
	t.Helper()
	e, err := export.Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

// survey reads a whole export through the public API and counts what it
// saw, in the vocabulary of fixtures.FixtureFacts. It also checks ordering.
func survey(t *testing.T, e *export.Export) (fixtures.FixtureFacts, []string) {
	t.Helper()
	got := fixtures.FixtureFacts{Subtypes: map[string]int{}, PerChannel: map[string]fixtures.ChannelFacts{}}
	var sequence []string // every ts and error, in iteration order

	users, err := e.Users()
	if err != nil {
		t.Fatalf("Users: %v", err)
	}
	known := map[string]bool{}
	got.Users = len(users)
	for _, u := range users {
		known[u.ID] = true
		if u.Deleted {
			got.DeletedUsers++
		}
		if u.IsBot {
			got.Bots++
		}
		if u.IsGuest() {
			got.Guests++
		}
		if u.IsStranger {
			got.Strangers++
		}
		if !u.IsBot && u.Profile.Email == "" {
			got.UsersWithoutEmail++
		}
		if u.DisplayLabel() == "" {
			t.Errorf("user %s has no display label", u.ID)
		}
	}

	channels, err := e.Channels()
	if err != nil {
		t.Fatalf("Channels: %v", err)
	}
	unknown := map[string]bool{}
	fileIDs := map[string]bool{}
	for _, ch := range channels {
		got.Channels++
		switch ch.Kind {
		case slack.KindPublic:
			got.PublicChannels++
		case slack.KindPrivate:
			got.PrivateChannels++
		case slack.KindDM:
			got.DMs++
		case slack.KindMPIM:
			got.MPIMs++
		default:
			t.Errorf("channel %s has kind %q", ch.ID, ch.Kind)
		}
		if ch.IsArchived {
			got.ArchivedChannels++
		}
		days, err := e.Days(ch)
		if err != nil {
			t.Fatalf("Days(%s): %v", ch.Label(), err)
		}
		if !slices.IsSorted(days) {
			t.Errorf("Days(%s) not sorted", ch.Label())
		}
		if len(days) == 0 {
			got.EmptyChannels++
		}
		cf := fixtures.ChannelFacts{Name: ch.Name, Kind: string(ch.Kind), Folder: ch.Folder(), Days: len(days)}
		got.DayFiles += len(days)

		prev := ""
		perMS := map[int64]int{}
		threads := map[string]int{}
		for m, err := range e.Messages(ch) {
			if err != nil {
				var me *export.MessageError
				if !errors.As(err, &me) {
					t.Fatalf("Messages(%s): unexpected error %v", ch.Label(), err)
				}
				if strings.Contains(err.Error(), "xox") {
					t.Errorf("error leaks a token: %v", err)
				}
				cf.Malformed++
				sequence = append(sequence, "error:"+err.Error())
				continue
			}
			sequence = append(sequence, m.TS)
			if prev != "" && slack.CompareTS(prev, m.TS) >= 0 {
				t.Errorf("%s: ts %s does not follow %s", ch.Label(), m.TS, prev)
			}
			prev = m.TS
			if m.Time().IsZero() {
				t.Errorf("%s: message %s has no time", ch.Label(), m.TS)
			}
			if got.FirstTS == "" || slack.CompareTS(m.TS, got.FirstTS) < 0 {
				got.FirstTS = m.TS
			}
			if got.LastTS == "" || slack.CompareTS(m.TS, got.LastTS) > 0 {
				got.LastTS = m.TS
			}
			sec, micro, _ := slack.ParseTS(m.TS)
			perMS[sec*1000+micro/1000]++

			cf.Messages++
			got.Subtypes[m.Subtype]++
			if m.IsThreadParent() {
				got.ThreadParents++
			}
			if m.IsReply() {
				got.Replies++
				threads[m.ThreadTS]++
			}
			if m.Edited != nil {
				got.EditedMessages++
			}
			if m.IsBot() {
				got.BotMessages++
			}
			if len(m.Blocks) > 0 {
				got.MessagesWithBlocks++
			}
			if m.User != "" && !known[m.User] {
				got.UnknownUserMessages++
				unknown[m.User] = true
			}
			got.LargestMessageBytes = max(got.LargestMessageBytes, len(m.Text))
			for _, r := range m.Reactions {
				got.Reactions++
				got.ReactionCount += r.Count
			}
			if len(m.Files) > 0 {
				got.MessagesWithFiles++
			}
			for _, f := range m.Files {
				got.Files++
				got.FileBytes += f.Size
				fileIDs[f.ID] = true
				switch {
				case f.IsHidden():
					got.HiddenFiles++
				case f.IsTombstone():
					got.TombstonedFiles++
				case f.IsExternal:
					got.ExternalFiles++
				case f.URLPrivateDownload != "":
					got.DownloadableFiles++
					got.DownloadableBytes += f.Size
				}
				if f.IsCanvas() {
					got.CanvasFiles++
				}
				if strings.Contains(f.SafeURL(), "?") || strings.Contains(f.String(), "http") {
					t.Errorf("file %s exposes its URL", f.ID)
				}
			}
		}
		for _, n := range perMS {
			got.SameMillisecondCollisions += n - 1
		}
		for _, n := range threads {
			got.LargestThread = max(got.LargestThread, n)
		}
		got.Messages += cf.Messages
		got.MalformedMessages += cf.Malformed
		got.PerChannel[ch.ID] = cf
	}
	got.UnknownAuthors = len(unknown)
	got.UniqueFiles = len(fileIDs)
	return got, sequence
}

func TestReaderMatchesFacts(t *testing.T) {
	for _, name := range fixtures.Names() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := openZip(t, name)
			got, _ := survey(t, e)

			want := fixtures.Facts(name)
			// Not observable through the reader.
			got.DirectoryUsers = want.DirectoryUsers
			got.Canvases = want.Canvases
			if e.HasFile("canvases.json") != (want.Canvases > 0) {
				t.Errorf("HasFile(canvases.json) = %v with %d canvases", e.HasFile("canvases.json"), want.Canvases)
			}

			gv, wv := reflect.ValueOf(got), reflect.ValueOf(want)
			for i := range gv.NumField() {
				if !reflect.DeepEqual(gv.Field(i).Interface(), wv.Field(i).Interface()) {
					t.Errorf("%s: reader saw %v, fixture has %v", gv.Type().Field(i).Name, gv.Field(i).Interface(), wv.Field(i).Interface())
				}
			}
		})
	}
}

func TestZipAndDirectoryAgree(t *testing.T) {
	for _, name := range fixtures.Names() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, err := fixtures.WriteDir(name, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			zipFacts, zipSeq := survey(t, openZip(t, name))
			dirFacts, dirSeq := survey(t, open(t, dir))
			if !reflect.DeepEqual(zipFacts, dirFacts) {
				t.Errorf("facts differ:\n zip %+v\n dir %+v", zipFacts, dirFacts)
			}
			if !slices.Equal(zipSeq, dirSeq) {
				t.Errorf("message sequences differ (zip %d entries, dir %d)", len(zipSeq), len(dirSeq))
			}
		})
	}
}

func TestWrappingFolder(t *testing.T) {
	want, wantSeq := survey(t, openZip(t, "small"))

	t.Run("zip", func(t *testing.T) {
		got, seq := survey(t, openZip(t, "small", fixtures.Wrapped("Fixture Slack export Mar 4 2024")))
		if !reflect.DeepEqual(got, want) || !slices.Equal(seq, wantSeq) {
			t.Errorf("wrapped zip reads differently: %+v", got)
		}
	})
	t.Run("directory", func(t *testing.T) {
		dir, err := fixtures.WriteDir("small", t.TempDir(), fixtures.Wrapped("export"))
		if err != nil {
			t.Fatal(err)
		}
		// Archive-tool litter beside the wrapper must not confuse detection.
		if err := os.MkdirAll(filepath.Join(dir, "__MACOSX"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, seq := survey(t, open(t, dir))
		if !reflect.DeepEqual(got, want) || !slices.Equal(seq, wantSeq) {
			t.Errorf("wrapped directory reads differently: %+v", got)
		}
	})
}

// wantFail asserts err is a complete *fail.Error with the given code.
func wantFail(t *testing.T, err error, code string) *fail.Error {
	t.Helper()
	var fe *fail.Error
	if !errors.As(err, &fe) {
		t.Fatalf("got %T (%v), want *fail.Error %s", err, err, code)
	}
	if fe.Code != code {
		t.Errorf("code = %q, want %q", fe.Code, code)
	}
	if fe.What == "" || fe.Why == "" || fe.Next == "" {
		t.Errorf("%s is missing what/why/next: %+v", fe.Code, *fe)
	}
	return fe
}

func TestOpenErrors(t *testing.T) {
	dir := t.TempDir()

	_, err := export.Open(filepath.Join(dir, "nope.zip"))
	wantFail(t, err, "export.not_found")

	notZip := filepath.Join(dir, "export.zip")
	if err := os.WriteFile(notZip, []byte("this is a text file, not an archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = export.Open(notZip)
	wantFail(t, err, "export.not_zip")

	// A real export cut short mid-download.
	full, err := fixtures.Write("small", dir)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	truncated := filepath.Join(dir, "truncated.zip")
	if err := os.WriteFile(truncated, data[:len(data)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = export.Open(truncated)
	wantFail(t, err, "export.not_zip")

	empty := filepath.Join(dir, "empty")
	if err := os.MkdirAll(filepath.Join(empty, "photos"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(empty, "photos", "users.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = export.Open(empty)
	wantFail(t, err, "export.not_slack_export")

	// Two candidate folders: refuse to guess.
	_, err = export.OpenFS(fstest.MapFS{
		"a/users.json": {Data: []byte("[]")},
		"b/users.json": {Data: []byte("[]")},
	}, "ambiguous")
	wantFail(t, err, "export.not_slack_export")
}

func TestUsersErrors(t *testing.T) {
	e, err := export.OpenFS(fstest.MapFS{"channels.json": {Data: []byte(`[{"id":"C1","name":"general"}]`)}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Users()
	wantFail(t, err, "export.users_missing")
	if e.HasFile("users.json") || !e.HasFile("channels.json") || e.HasFile("../channels.json") {
		t.Error("HasFile is wrong")
	}

	for _, body := range []string{``, `{"id":"U1"}`, `[{"id":"U1"}, 7]`, `[{"id":"U1"}`, `[{"id":42}]`} {
		e, err := export.OpenFS(fstest.MapFS{"users.json": {Data: []byte(body)}}, "test")
		if err != nil {
			t.Fatal(err)
		}
		_, err = e.Users()
		wantFail(t, err, "export.corrupt_index")
	}
}

func TestEnterpriseUsers(t *testing.T) {
	e, err := export.OpenFS(fstest.MapFS{
		"users.json":     {Data: []byte(`[{"id":"U1","name":"local"}]`)},
		"org_users.json": {Data: []byte(`[{"id":"U1","name":"org-copy"},{"id":"U2","name":"org-only"}]`)},
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	users, err := e.Users()
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || users[0].Name != "local" || users[1].Name != "org-only" {
		t.Errorf("merged users = %+v", users)
	}

	e, err = export.OpenFS(fstest.MapFS{"org_users.json": {Data: []byte(`[{"id":"U2"}]`)}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if users, err := e.Users(); err != nil || len(users) != 1 {
		t.Errorf("org-only users = %v, %v", users, err)
	}
	if chans, err := e.Channels(); err != nil || len(chans) != 0 {
		t.Errorf("Channels with no index = %v, %v", chans, err)
	}
}

func channelNamed(t *testing.T, e *export.Export, name string) slack.Channel {
	t.Helper()
	chans, err := e.Channels()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chans {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no channel %q", name)
	return slack.Channel{}
}

func TestMalformedEntriesAreReportedAndSkipped(t *testing.T) {
	e := openZip(t, "nasty")
	ch := channelNamed(t, e, "malformed")
	days, err := e.Days(ch)
	if err != nil || len(days) != 2 {
		t.Fatalf("Days = %v, %v", days, err)
	}

	var texts []string
	var indexes []int
	for m, err := range e.Messages(ch) {
		if err != nil {
			var me *export.MessageError
			if !errors.As(err, &me) {
				t.Fatalf("unexpected error %v", err)
			}
			if me.Channel != "malformed" || me.ChannelID != ch.ID || me.Day != days[0] || me.File != "malformed/"+days[0]+".json" {
				t.Errorf("error does not locate the entry: %+v", me)
			}
			for _, part := range []string{"malformed", me.File, "entry"} {
				if !strings.Contains(me.Error(), part) {
					t.Errorf("error text %q lacks %q", me.Error(), part)
				}
			}
			indexes = append(indexes, me.Index)
			continue
		}
		texts = append(texts, m.Text)
	}
	if want := []int{1, 2, 4, 5, 6, 7}; !slices.Equal(indexes, want) {
		t.Errorf("malformed indexes = %v, want %v", indexes, want)
	}
	if len(texts) != 4 || !strings.Contains(texts[3], "following day") {
		t.Errorf("iteration did not continue past the damage: %q", texts)
	}

	msgs, err := e.DayMessages(ch, days[0])
	var me *export.MessageError
	if len(msgs) != 3 || !errors.As(err, &me) {
		t.Errorf("DayMessages = %d messages, err %v", len(msgs), err)
	}
	if msgs, err := e.DayMessages(ch, days[1]); len(msgs) != 1 || err != nil {
		t.Errorf("clean day = %d messages, err %v", len(msgs), err)
	}
	_, err = e.DayMessages(ch, "1999-01-01")
	wantFail(t, err, "export.day_unreadable")
	_, err = e.DayMessages(ch, "../users")
	wantFail(t, err, "export.bad_day")
}

func TestSameMillisecondOrdering(t *testing.T) {
	e := openZip(t, "nasty")
	ch := channelNamed(t, e, "collisions")
	var ts []string
	for m, err := range e.Messages(ch) {
		if err != nil {
			t.Fatal(err)
		}
		ts = append(ts, m.TS)
	}
	if len(ts) != 10 {
		t.Fatalf("got %d messages", len(ts))
	}
	// The fixture writes this day file newest-first; the reader must sort it.
	for i, suffix := range []string{".000001", ".000002", ".000003", ".000004", ".000999", ".123000", ".123456", ".123999", ".500999", ".501000"} {
		if !strings.HasSuffix(ts[i], suffix) {
			t.Errorf("message %d has ts %s, want suffix %s", i, ts[i], suffix)
		}
	}
	if ts[0][:10] != ts[4][:10] {
		t.Errorf("burst spans seconds: %v", ts[:5])
	}
}

func TestBrokenDayFile(t *testing.T) {
	e, err := export.OpenFS(fstest.MapFS{
		"users.json":              {Data: []byte(`[]`)},
		"channels.json":           {Data: []byte(`[{"id":"C1","name":"general"},{"id":"C2","name":"../evil"},{"id":"C3","name":"quiet"}]`)},
		"general/2024-03-04.json": {Data: []byte(`[{"ts":"1709546400.000002"},{"ts":"1709546400.000001"},{"ts":"1709546`)},
		"general/2024-03-05.json": {Data: []byte(`{"not":"an array"}`)},
		"general/2024-03-06.json": {Data: []byte(`[{"ts":"1709719200.000001"}]`)},
		"general/notes.json":      {Data: []byte(`[]`)},
		"general/2024-3-7.json":   {Data: []byte(`[]`)},
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	chans, err := e.Channels()
	if err != nil || len(chans) != 3 {
		t.Fatalf("Channels = %v, %v", chans, err)
	}
	if days, _ := e.Days(chans[0]); !slices.Equal(days, []string{"2024-03-04", "2024-03-05", "2024-03-06"}) {
		t.Errorf("Days = %v", days)
	}

	var got []string
	for m, err := range e.Messages(chans[0]) {
		if err != nil {
			got = append(got, wantFail(t, err, "export.day_unreadable").Code)
			continue
		}
		got = append(got, m.TS)
	}
	want := []string{"1709546400.000001", "1709546400.000002", "export.day_unreadable", "export.day_unreadable", "1709719200.000001"}
	if !slices.Equal(got, want) {
		t.Errorf("sequence = %v, want %v", got, want)
	}

	// A channel name that would escape its folder is refused, not followed.
	_, err = e.Days(chans[1])
	wantFail(t, err, "export.bad_channel")
	n := 0
	for _, err := range e.Messages(chans[1]) {
		wantFail(t, err, "export.bad_channel")
		n++
	}
	if n != 1 {
		t.Errorf("bad channel yielded %d values", n)
	}

	// A channel with no folder is empty.
	if days, err := e.Days(chans[2]); err != nil || len(days) != 0 {
		t.Errorf("empty channel Days = %v, %v", days, err)
	}
	for range e.Messages(chans[2]) {
		t.Error("empty channel yielded a value")
	}
}

func TestIterationCanStopEarly(t *testing.T) {
	e := openZip(t, "nasty")
	for _, name := range []string{"megathread", "malformed"} {
		n := 0
		for range e.Messages(channelNamed(t, e, name)) {
			if n++; n == 3 {
				break
			}
		}
		if n != 3 {
			t.Errorf("%s: stopped after %d", name, n)
		}
	}
	if err := e.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}
