package fixtures_test

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/bgfdev/taut/internal/fixtures"
	"github.com/bgfdev/taut/internal/slack"
	"github.com/bgfdev/taut/internal/slack/export"
)

const smallLoad = "load:channels=12,messages=3000,users=60,days=40,threads=0.25,files=0.05,private=0.25,dms=10,dmmessages=400"

// The load fixture says what it wrote, and what it says is what a reader
// of the export finds.
func TestLoadFixtureMatchesItsFacts(t *testing.T) {
	path, err := fixtures.Write(smallLoad, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	facts := fixtures.Facts(smallLoad)
	if facts.Messages != 3400 || facts.Channels != 12+10+2 || facts.DMs != 10 || facts.MPIMs != 2 || facts.Users != 60 {
		t.Fatalf("facts = %+v", facts)
	}
	if facts.PrivateChannels == 0 || facts.PublicChannels+facts.PrivateChannels != 12 || facts.Replies == 0 || facts.Files == 0 || facts.ThreadParents == 0 {
		t.Errorf("the fixture lacks private channels, threads or files: %+v", facts)
	}
	ex, err := export.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()
	users, err := ex.Users()
	if err != nil || len(users) != facts.Users {
		t.Fatalf("users: %d, %v", len(users), err)
	}
	known := map[string]bool{}
	for _, u := range users {
		known[u.ID] = true
	}
	chans, err := ex.Channels()
	if err != nil || len(chans) != facts.Channels {
		t.Fatalf("channels: %d, %v", len(chans), err)
	}
	var messages, replies, files, days, channelMessages int
	sizes := make([]int, 0, len(chans))
	sets := map[string]bool{}
	for _, ch := range chans {
		want := facts.PerChannel[ch.ID]
		d, err := ex.Days(ch)
		if err != nil || len(d) != want.Days {
			t.Errorf("%s: %d days, want %d (%v)", ch.ID, len(d), want.Days, err)
		}
		days += len(d)
		n, last := 0, ""
		seen := map[string]bool{}
		for m, err := range ex.Messages(ch) {
			if err != nil {
				t.Fatalf("%s: %v", ch.ID, err)
			}
			n++
			if m.TS <= last {
				t.Fatalf("%s: message %s is not after %s", ch.ID, m.TS, last)
			}
			last = m.TS
			ms := m.TS[:len(m.TS)-3]
			if seen[ms] {
				t.Fatalf("%s: two messages in millisecond %s", ch.ID, ms)
			}
			seen[ms] = true
			if m.User != "" && !known[m.User] {
				t.Fatalf("%s: unknown author %s", ch.ID, m.User)
			}
			if m.ThreadTS != "" && m.ThreadTS != m.TS {
				replies++
			}
			files += len(m.Files)
			for _, f := range m.Files {
				if size, ok := fixtures.LoadFileSize(f.ID); !ok || size != f.Size {
					t.Errorf("file %s: size %d, LoadFileSize %d %v", f.ID, f.Size, size, ok)
				}
			}
		}
		if n != want.Messages || n == 0 {
			t.Errorf("%s: %d messages, want %d", ch.ID, n, want.Messages)
		}
		messages += n
		if ch.Kind == slack.KindDM || ch.Kind == slack.KindMPIM {
			key := strings.Join(ch.Members, ",")
			if sets[key] {
				t.Errorf("two direct conversations between %s", key)
			}
			sets[key] = true
		} else {
			channelMessages += n
			sizes = append(sizes, n)
		}
	}
	if messages != facts.Messages || channelMessages != 3000 || replies != facts.Replies || files != facts.Files || days != facts.DayFiles {
		t.Errorf("read %d messages (%d in channels), %d replies, %d files, %d days; facts %+v", messages, channelMessages, replies, files, days, facts)
	}
	// A few large channels and a long tail: the largest holds several
	// times what the median does.
	if largest, median := sizes[0], sizes[len(sizes)/2]; largest < 4*median {
		t.Errorf("channel sizes are not skewed: largest %d, median %d", largest, median)
	}
	// The people are those of users.json, without reading the export.
	people := fixtures.People(smallLoad)
	if len(people) != len(users) || people[0].SlackID != users[0].ID || !people[0].InDirectory {
		t.Errorf("people = %d, first %+v", len(people), people[0])
	}
}

// The same spec is the same bytes, a different seed is not, and a spec
// reads back from its own name.
func TestLoadFixtureIsDeterministic(t *testing.T) {
	sum := func(name string) [32]byte {
		h := sha256.New()
		if err := fixtures.Build(name, h); err != nil {
			t.Fatal(err)
		}
		return [32]byte(h.Sum(nil))
	}
	if sum(smallLoad) != sum(smallLoad) {
		t.Error("two builds of one spec differ")
	}
	if sum(smallLoad) == sum(smallLoad+",seed=2") {
		t.Error("the seed changes nothing")
	}
	s, err := fixtures.ParseLoadSpec(smallLoad)
	if err != nil {
		t.Fatal(err)
	}
	if back, err := fixtures.ParseLoadSpec(s.String()); err != nil || back != s {
		t.Errorf("%s reads back as %+v (%v)", s, back, err)
	}
	if d, err := fixtures.ParseLoadSpec("load"); err != nil || d != fixtures.DefaultLoadSpec() {
		t.Errorf("load = %+v (%v)", d, err)
	}
	// Without skew every channel is the same size.
	flat := fixtures.Facts("load:channels=7,messages=700,users=20,days=10,skew=0")
	for id, c := range flat.PerChannel {
		if c.Messages != 100 {
			t.Errorf("skew=0: %s has %d messages", id, c.Messages)
		}
	}
	for _, bad := range []string{"load:skew=9", "load:channels=0", "load:messages=3,channels=5", "load:nope=1", "load:threads=2", "load:users=3", "load:days=5000", "load:dms=100000,users=20", "load:channels"} {
		if _, err := fixtures.ParseLoadSpec(bad); err == nil {
			t.Errorf("%s was accepted", bad)
		}
		if fixtures.Known(bad) {
			t.Errorf("%s is known", bad)
		}
	}
	if !fixtures.Known("load") || !fixtures.Known("medium") || fixtures.Known("large") {
		t.Error("Known does not know the fixtures")
	}
}

// Generating does not hold the export in memory: the ZIP is written as it
// goes, and what is written is a readable archive of JSON.
func TestLoadFixtureIsAStream(t *testing.T) {
	var buf bytes.Buffer
	s, _ := fixtures.ParseLoadSpec("load:channels=5,messages=500,users=20,days=10")
	var writes int
	facts, err := fixtures.WriteLoad(s, writerFunc(func(p []byte) (int, error) { writes++; return buf.Write(p) }))
	if err != nil {
		t.Fatal(err)
	}
	if writes < facts.DayFiles/8 {
		t.Errorf("%d writes for %d day files: the archive was not streamed", writes, facts.DayFiles)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(rc)
		rc.Close()
		if !json.Valid(raw) {
			t.Errorf("%s is not JSON", f.Name)
		}
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
