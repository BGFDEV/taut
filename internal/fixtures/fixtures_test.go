package fixtures

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

func zipBytes(t *testing.T, name string, opts ...Option) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := Build(name, &buf, opts...); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestBuildsAreByteIdentical(t *testing.T) {
	for _, name := range Names() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// Generate from scratch twice, bypassing the cache, so the test
			// covers the generator and not just the ZIP writer.
			var runs [2]bytes.Buffer
			for i := range runs {
				if err := writeZip(&runs[i], builders[name]().finish().files, config{}); err != nil {
					t.Fatal(err)
				}
			}
			if !bytes.Equal(runs[0].Bytes(), runs[1].Bytes()) {
				t.Fatal("two generations differ")
			}
			if !bytes.Equal(runs[0].Bytes(), zipBytes(t, name)) {
				t.Fatal("Build differs from a fresh generation")
			}
		})
	}
}

var tokenLike = regexp.MustCompile(`xox[a-z]-[A-Za-z0-9-]*`)

func TestArchivesAreWellFormed(t *testing.T) {
	for _, name := range Names() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			data := zipBytes(t, name)
			zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			tokens := 0
			for _, f := range zr.File {
				seen[f.Name] = true
				if !f.Modified.Equal(zipTime) {
					t.Errorf("%s: mod time %v", f.Name, f.Modified)
				}
				rc, err := f.Open()
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(rc)
				rc.Close()
				if err != nil {
					t.Fatal(err)
				}
				if !json.Valid(body) || !utf8.Valid(body) {
					t.Errorf("%s is not valid UTF-8 JSON", f.Name)
				}
				for _, tok := range tokenLike.FindAll(body, -1) {
					tokens++
					if !bytes.HasPrefix(tok, []byte("xoxe-FAKE-fixture-not-a-secret-")) {
						t.Errorf("%s contains a token-like string that is not marked fake: %s", f.Name, tok)
					}
				}
			}
			facts := Facts(name)
			for _, want := range []string{"users.json", "channels.json"} {
				if !seen[want] {
					t.Errorf("missing %s", want)
				}
			}
			for file, n := range map[string]int{"groups.json": facts.PrivateChannels, "dms.json": facts.DMs, "mpims.json": facts.MPIMs, "canvases.json": facts.Canvases} {
				if seen[file] != (n > 0) {
					t.Errorf("%s present = %v with count %d", file, seen[file], n)
				}
			}
			if tokens == 0 {
				t.Error("no file URL carries a (fake) token for redaction tests to find")
			}
			if got := countDayFiles(seen); got != facts.DayFiles {
				t.Errorf("day files: archive has %d, facts say %d", got, facts.DayFiles)
			}
		})
	}
}

func countDayFiles(names map[string]bool) int {
	n := 0
	for name := range names {
		if strings.Contains(name, "/") {
			n++
		}
	}
	return n
}

func TestFactsDescribeTheIntendedShape(t *testing.T) {
	small, medium, nasty := Facts("small"), Facts("medium"), Facts("nasty")

	// Three public channels, and one direct message and one group DM for
	// the move into Teams chats.
	if small.Users != 5 || small.PublicChannels != 3 || small.Channels != 5 || small.DMs != 1 || small.MPIMs != 1 || small.ThreadParents != 1 ||
		small.Files != 1 || small.Reactions != 1 || small.DirectoryUsers != 5 ||
		small.MalformedMessages+small.SameMillisecondCollisions+small.UnknownUserMessages+small.DeletedUsers != 0 {
		t.Errorf("small is not small and clean: %+v", small)
	}
	if small.Messages < 50 || small.Messages > 80 {
		t.Errorf("small has %d messages", small.Messages)
	}

	if medium.Users != 40 || medium.PublicChannels != 12 || medium.PrivateChannels != 3 || medium.DMs == 0 || medium.MPIMs == 0 ||
		medium.Bots != 3 || medium.DeletedUsers != 4 {
		t.Errorf("medium has the wrong population: %+v", medium)
	}
	if medium.Messages < 4500 || medium.Messages > 5500 {
		t.Errorf("medium has %d messages, want about 5000", medium.Messages)
	}
	if medium.MessagesWithBlocks*2 < medium.Messages || medium.MessagesWithBlocks == medium.Messages {
		t.Errorf("medium should have blocks on most but not all messages: %d of %d", medium.MessagesWithBlocks, medium.Messages)
	}
	if medium.ThreadParents == 0 || medium.Files == 0 || medium.Reactions == 0 || medium.EditedMessages == 0 || medium.BotMessages == 0 ||
		medium.MalformedMessages != 0 || medium.SameMillisecondCollisions != 0 {
		t.Errorf("medium lacks variety or is not clean: %+v", medium)
	}

	if nasty.LargestThread != 2500 || nasty.SameMillisecondCollisions == 0 || nasty.MalformedMessages == 0 ||
		nasty.UnknownAuthors < 3 || nasty.Strangers == 0 || nasty.DeletedUsers == 0 ||
		nasty.HiddenFiles == 0 || nasty.ExternalFiles == 0 || nasty.TombstonedFiles == 0 || nasty.CanvasFiles != 1 || nasty.Canvases != 1 ||
		nasty.ArchivedChannels != 1 || nasty.EmptyChannels != 1 || nasty.EditedMessages == 0 ||
		nasty.FileBytes < 6<<30 || nasty.LargestMessageBytes < 150<<10 || nasty.UniqueFiles >= nasty.Files {
		t.Errorf("nasty is not nasty enough: %+v", nasty)
	}
	// Direct messages of every kind that matters for chats: with a bot, a
	// guest, someone who left, someone unmapped, oneself, with threads and
	// files, and two group DMs.
	if nasty.DMs < 7 || nasty.MPIMs != 2 {
		t.Errorf("nasty has %d DMs and %d group DMs", nasty.DMs, nasty.MPIMs)
	}
	for _, subtype := range []string{"message_changed", "message_deleted", "channel_join", "pinned_item", "thread_broadcast",
		"huddle_thread", "tombstone", "bot_message", "taut_fixture_unknown_subtype"} {
		if nasty.Subtypes[subtype] == 0 {
			t.Errorf("nasty has no %s message", subtype)
		}
	}
	names := map[string]bool{}
	for _, cf := range nasty.PerChannel {
		names[cf.Name] = true
	}
	for _, name := range []string{"_archive", "forms", "con", "general", longChannelName} {
		if !names[name] {
			t.Errorf("nasty has no channel %q", name)
		}
	}
	if len(longChannelName) != 80 {
		t.Errorf("long channel name has %d characters", len(longChannelName))
	}

	// Facts returns a copy: callers cannot corrupt the cache.
	nasty.Subtypes["tombstone"] = 99
	if Facts("nasty").Subtypes["tombstone"] == 99 {
		t.Error("Facts exposes shared maps")
	}
}

func TestPeople(t *testing.T) {
	for _, name := range Names() {
		people, facts := People(name), Facts(name)
		if len(people) != facts.Users {
			t.Errorf("%s: %d people for %d users", name, len(people), facts.Users)
		}
		ids := map[string]bool{}
		var inDir, unmapped int
		for _, p := range people {
			if ids[p.SlackID] || p.SlackID == "" {
				t.Errorf("%s: bad or duplicate ID %q", name, p.SlackID)
			}
			ids[p.SlackID] = true
			if p.InDirectory {
				inDir++
				if p.Email == "" || p.Deleted || p.Bot || p.External {
					t.Errorf("%s: %+v should not be in the directory", name, p)
				}
			} else if !p.Deleted && !p.Bot && !p.External && p.Email != "" {
				unmapped++
			}
		}
		if inDir != facts.DirectoryUsers {
			t.Errorf("%s: %d in directory, facts say %d", name, inDir, facts.DirectoryUsers)
		}
		if name != "small" && unmapped == 0 {
			t.Errorf("%s: no active user is left unmapped", name)
		}
		if name == "medium" && (inDir*2 < len(people) || unmapped != 2) {
			t.Errorf("medium: %d of %d in directory, %d unmapped", inDir, len(people), unmapped)
		}
	}
}

func TestWriteVariants(t *testing.T) {
	dir := t.TempDir()
	path, err := Write("small", dir)
	if err != nil || path != filepath.Join(dir, "small.zip") {
		t.Fatalf("Write = %q, %v", path, err)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(onDisk, zipBytes(t, "small")) {
		t.Errorf("Write and Build disagree (%v)", err)
	}

	wrapped := zipBytes(t, "small", Wrapped("export"))
	zr, err := zip.NewReader(bytes.NewReader(wrapped), int64(len(wrapped)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, "export/") {
			t.Errorf("entry %q is outside the wrapping folder", f.Name)
		}
	}

	root, err := WriteDir("small", dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "general")); err != nil {
		t.Errorf("WriteDir did not create channel folders: %v", err)
	}

	if _, err := Write("huge", dir); err == nil {
		t.Error("Write accepted an unknown fixture")
	}
	if err := Build("huge", io.Discard); err == nil {
		t.Error("Build accepted an unknown fixture")
	}
}
