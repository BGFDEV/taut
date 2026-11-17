package fixtures

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
)

// dayMessages decodes every day file of a built fixture into ts → message,
// by export folder.
func dayMessages(t *testing.T, b *built) map[string]map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]map[string]any{}
	for name, data := range b.files {
		folder, _, nested := strings.Cut(name, "/")
		if !nested {
			continue
		}
		var list []map[string]any
		if err := json.Unmarshal(data, &list); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if out[folder] == nil {
			out[folder] = map[string]map[string]any{}
		}
		for _, m := range list {
			if ts, _ := m["ts"].(string); ts != "" {
				out[folder][ts] = m
			}
		}
	}
	return out
}

// A later fixture is its base plus what its facts say: the same messages
// except the ones it names as edited or deleted, and exactly the stated
// number of new ones.
func TestLaterFixturesExtendTheirBase(t *testing.T) {
	for _, base := range []string{"small", "medium"} {
		t.Run(base, func(t *testing.T) {
			name := LaterOf(base)
			if !Known(name) {
				t.Fatalf("%s is not a known fixture", name)
			}
			b, l := mustGet(base), mustGet(name)
			f := LaterFacts(name)

			was, now := dayMessages(t, b), dayMessages(t, l)
			deleted, edited := map[string]bool{}, map[string]bool{}
			for _, tss := range f.DeletedTS {
				for _, ts := range tss {
					deleted[ts] = true
				}
			}
			for _, tss := range f.EditedTS {
				for _, ts := range tss {
					edited[ts] = true
				}
			}
			if len(deleted) != f.Deleted || len(edited) != f.Edited || f.Deleted == 0 || f.Edited == 0 {
				t.Fatalf("facts name %d deleted and %d edited messages; counts say %d and %d", len(deleted), len(edited), f.Deleted, f.Edited)
			}

			missing, changed := 0, 0
			for folder, msgs := range was {
				for ts, m := range msgs {
					got, ok := now[folder][ts]
					switch {
					case !ok && !deleted[ts]:
						t.Errorf("%s/%s is gone from the later export but not listed as deleted", folder, ts)
					case !ok:
						missing++
					case got["text"] != m["text"]:
						if !edited[ts] {
							t.Errorf("%s/%s changed its text but is not listed as edited", folder, ts)
						}
						if got["edited"] == nil {
							t.Errorf("%s/%s changed its text without an edited marker", folder, ts)
						}
						changed++
					}
				}
			}
			if missing != f.Deleted || changed != f.Edited {
				t.Errorf("%d messages missing and %d changed; facts say %d and %d", missing, changed, f.Deleted, f.Edited)
			}

			added, replies, toOld := 0, 0, 0
			for folder, msgs := range now {
				for ts, m := range msgs {
					if _, ok := was[folder][ts]; ok {
						continue
					}
					added++
					if thread, _ := m["thread_ts"].(string); thread != "" && thread != ts {
						replies++
						if _, old := was[folder][thread]; old {
							toOld++
						}
					}
				}
			}
			if added != f.NewMessages || replies != f.NewReplies || toOld != f.RepliesToOld {
				t.Errorf("later export adds %d messages, %d replies, %d to old threads; facts say %d, %d, %d",
					added, replies, toOld, f.NewMessages, f.NewReplies, f.RepliesToOld)
			}
			if f.RepliesToOld == 0 || f.LateArrivals == 0 || f.NewFiles == 0 || f.NewChannels != 1 || f.NewUsers != 2 {
				t.Errorf("the later fixture lost a case it exists for: %+v", f)
			}
			if l.facts.Messages != b.facts.Messages+f.NewMessages-f.Deleted {
				t.Errorf("later export has %d messages; base %d + %d new - %d deleted", l.facts.Messages, b.facts.Messages, f.NewMessages, f.Deleted)
			}
			if l.facts.Users != b.facts.Users+f.NewUsers || l.facts.Channels != b.facts.Channels+f.NewChannels {
				t.Errorf("later export has %d users and %d channels", l.facts.Users, l.facts.Channels)
			}
			// The base's people are the later export's first people, so a
			// directory seeded from either gives them the same accounts.
			if !slices.Equal(People(name)[:len(People(base))], People(base)) {
				t.Error("the later fixture changed the base's people")
			}
			if f.QuietChannel != "" {
				for ts := range now[f.QuietChannel] {
					if _, ok := was[f.QuietChannel][ts]; !ok {
						t.Errorf("the quiet channel #%s has a new message %s", f.QuietChannel, ts)
					}
				}
			}
			if _, ok := was[f.NewChannel]; ok || len(now[f.NewChannel]) == 0 {
				t.Errorf("the new channel #%s is not new, or is empty", f.NewChannel)
			}
		})
	}
}

func TestLaterFixturesAreDeterministic(t *testing.T) {
	for _, name := range LaterNames() {
		var a, b bytes.Buffer
		if err := Build(name, &a); err != nil {
			t.Fatal(err)
		}
		if err := Build(name, &b); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a.Bytes(), b.Bytes()) {
			t.Errorf("%s is not byte-identical across builds", name)
		}
		// Built from scratch, not only from the cache.
		again := builders[name]().finish()
		for _, file := range slices.Sorted(maps.Keys(again.files)) {
			if !bytes.Equal(again.files[file], mustGet(name).files[file]) {
				t.Errorf("%s: %s differs between two builds", name, file)
				break
			}
		}
	}
}

func TestFixtureContentOptions(t *testing.T) {
	b := mustGet("small")
	other, err := newConfig([]Option{OtherWorkspace()}).render(b.files)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != len(b.files) {
		t.Fatalf("another workspace has %d files, want %d", len(other), len(b.files))
	}
	for name, data := range other {
		if bytes.Contains(data, []byte(teamID)) || bytes.Contains(data, []byte(idInfix)) || strings.Contains(name, idInfix) {
			t.Errorf("%s still carries the fixture workspace's IDs", name)
		}
	}
	if !bytes.Contains(other["users.json"], []byte(otherTeamID)) {
		t.Error("users.json does not name the other workspace")
	}

	without, err := newConfig([]Option{Without("random")}).render(b.files)
	if err != nil {
		t.Fatal(err)
	}
	for name := range without {
		if strings.HasPrefix(name, "random/") {
			t.Errorf("%s was kept", name)
		}
	}
	if bytes.Contains(without["channels.json"], []byte(`"random"`)) {
		t.Error("channels.json still lists #random")
	}
	if !bytes.Contains(without["channels.json"], []byte(`"general"`)) {
		t.Error("channels.json lost a channel it should keep")
	}
	// No option, no copy.
	same, _ := newConfig(nil).render(b.files)
	if len(same) != len(b.files) {
		t.Error("render changed an export it had no option for")
	}
}
