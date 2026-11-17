// Package fixtures generates fake Slack workspace exports for tests.
//
// Three exports are available, each fully deterministic (fixed random seed,
// fixed clock, fixed ZIP metadata), so the same name always produces the same
// bytes:
//
//   - "small": a handful of users and channels with clean data.
//   - "medium": a realistic mid-size workspace with every index file,
//     threads, files, reactions, edits, bots and rich_text blocks.
//   - "nasty": the edge cases that break migrations.
//
// Alongside the bytes the package reports what it generated: Facts gives the
// ground-truth counts and People the identities, so a parser, preflight check
// or fake Microsoft Graph directory can be asserted against the truth rather
// than against another parser.
//
// A fourth, "load", is generated to order for load tests: see LoadSpec. Its
// name carries its size ("load:channels=150,messages=200000"), and every
// function here that takes a fixture name takes such a name too.
//
// Nothing here is real: users, emails and the tokens embedded in file URLs
// are invented and carry the marker "FAKE".
package fixtures

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// Person is one user of a fixture's users.json, with the identity facts a
// test needs to seed a target directory.
type Person struct {
	SlackID     string
	Email       string // empty for bots and users Slack exported without one
	DisplayName string // the Slack display name, or the real name when that is blank
	Deleted     bool   // deactivated in Slack
	Bot         bool
	External    bool // guest or Slack Connect user from another organisation
	// InDirectory says whether a Microsoft Entra account with this Email
	// should exist in the target tenant. It is false for deleted users, bots,
	// externals and a few active users deliberately left unmapped.
	InDirectory bool
}

// ChannelFacts is the ground truth for one channel, keyed by channel ID in
// FixtureFacts.PerChannel.
type ChannelFacts struct {
	Name      string // empty for DMs
	Kind      string // "public", "private", "dm" or "mpim"
	Folder    string // export folder: the name, or the ID for DMs
	Messages  int    // well-formed messages
	Malformed int    // unusable day-file entries
	Days      int    // day files
}

// FixtureFacts is the ground truth about a fixture, counted by the generator
// from the data it wrote. (Go does not allow a type and a function to share
// the name Facts.)
//
// "Message" means a well-formed day-file entry of any subtype; unusable
// entries are counted only in MalformedMessages. Objects nested inside a
// message (the "message" of a message_changed, the "root" of a
// thread_broadcast) are not counted.
type FixtureFacts struct {
	// Users.
	Users             int // entries in users.json
	DeletedUsers      int
	Bots              int // users.json entries with is_bot
	Guests            int // is_restricted or is_ultra_restricted
	Strangers         int // users.json entries with is_stranger
	UsersWithoutEmail int // non-bot users with no profile.email
	DirectoryUsers    int // people with InDirectory set

	// Channels.
	Channels         int // all kinds
	PublicChannels   int
	PrivateChannels  int
	DMs              int
	MPIMs            int
	ArchivedChannels int
	EmptyChannels    int // listed in an index but with no folder in the export
	Canvases         int // entries in canvases.json

	// Messages.
	DayFiles           int
	Messages           int
	MalformedMessages  int
	ThreadParents      int // thread_ts == ts
	Replies            int // thread_ts set and != ts
	LargestThread      int // most replies under one parent
	EditedMessages     int
	BotMessages        int // bot_id set or subtype bot_message
	MessagesWithBlocks int
	MessagesWithFiles  int
	// UnknownUserMessages counts messages whose "user" is not in users.json;
	// UnknownAuthors counts the distinct such IDs. USLACKBOT is among them.
	UnknownUserMessages int
	UnknownAuthors      int
	// SameMillisecondCollisions counts messages that share a channel and a
	// millisecond (ts truncated to three decimals) with an earlier message:
	// the sum over each such group of its size minus one. Microsoft Graph
	// rejects these as duplicates unless they are nudged apart.
	SameMillisecondCollisions int
	LargestMessageBytes       int            // longest "text", in bytes
	Subtypes                  map[string]int // message count by subtype; "" is a plain message
	FirstTS, LastTS           string         // extremes over all messages

	// Reactions.
	Reactions     int // reaction entries (one per emoji per message)
	ReactionCount int // sum of their "count"

	// Files. Every file object attached to a message is counted, including
	// placeholders that cannot be downloaded.
	Files             int
	UniqueFiles       int   // distinct file IDs
	FileBytes         int64 // sum of declared "size" over Files
	DownloadableFiles int   // hosted by Slack with a url_private_download
	DownloadableBytes int64
	ExternalFiles     int // is_external (e.g. Google Drive)
	HiddenFiles       int // hidden_by_limit
	TombstonedFiles   int // mode "tombstone"
	CanvasFiles       int // mode "space"

	PerChannel map[string]ChannelFacts // by channel ID
}

// Option adjusts how a fixture is written.
type Option func(*config)

type config struct {
	wrap    string
	other   bool
	without []string
}

// Wrapped puts the whole export inside one top-level folder of the archive
// (or directory), the way re-zipping an extracted export does.
func Wrapped(folder string) Option {
	return func(c *config) { c.wrap = folder }
}

// OtherWorkspace writes the fixture as the export of a different Slack
// workspace: the same people and conversations under another workspace ID,
// with every user, channel and file ID different. It is what an operator
// who points an incremental pass at the wrong export has.
func OtherWorkspace() Option {
	return func(c *config) { c.other = true }
}

// Without leaves a channel out of the export, index entry and messages
// alike, the way a later export looks after the channel was deleted in
// Slack. The name is the channel's Slack name.
func Without(channel string) Option {
	return func(c *config) { c.without = append(c.without, channel) }
}

// otherTeamID is the workspace OtherWorkspace writes; otherInfix replaces
// idInfix, which every generated ID carries.
const (
	otherTeamID = "T0ELSEWHERE"
	idInfix     = "0FX"
	otherInfix  = "0QZ"
)

// render applies the options that change an export's content, as opposed
// to where it is written. It returns files itself when there is nothing to
// change.
func (c config) render(files map[string][]byte) (map[string][]byte, error) {
	if !c.other && len(c.without) == 0 {
		return files, nil
	}
	out := make(map[string][]byte, len(files))
	for name, data := range files {
		if folder, _, nested := strings.Cut(name, "/"); nested && slices.Contains(c.without, folder) {
			continue
		}
		if len(c.without) > 0 && (name == "channels.json" || name == "groups.json") {
			var list []map[string]any
			if err := json.Unmarshal(data, &list); err != nil {
				return nil, fmt.Errorf("fixtures: %s: %w", name, err)
			}
			list = slices.DeleteFunc(list, func(ch map[string]any) bool {
				n, _ := ch["name"].(string)
				return slices.Contains(c.without, n)
			})
			data = encode(list)
		}
		if c.other {
			data = bytes.ReplaceAll(data, []byte(teamID), []byte(otherTeamID))
			data = bytes.ReplaceAll(data, []byte(idInfix), []byte(otherInfix))
			name = strings.ReplaceAll(name, idInfix, otherInfix)
		}
		out[name] = data
	}
	return out, nil
}

var builders = map[string]func() *builder{
	"small":  buildSmall,
	"medium": buildMedium,
	"nasty":  buildNasty,
}

// built is a finished fixture: the export's files plus the truth about them.
type built struct {
	files  map[string][]byte
	facts  FixtureFacts
	people []Person
	// later is set for a later fixture (see later.go).
	later *LaterFixtureFacts
}

var cache = func() map[string]func() *built {
	m := map[string]func() *built{}
	for name, build := range builders {
		m[name] = sync.OnceValue(func() *built { return build().finish() })
	}
	return m
}()

// Names returns the available fixture names, smallest first.
func Names() []string { return []string{"small", "medium", "nasty"} }

func get(name string) (*built, error) {
	gen, ok := cache[name]
	if !ok {
		return nil, fmt.Errorf("fixtures: unknown fixture %q (have %v)", name, Names())
	}
	return gen(), nil
}

func mustGet(name string) *built {
	b, err := get(name)
	if err != nil {
		panic(err)
	}
	return b
}

// Facts returns the ground truth for the named fixture. It panics on an
// unknown name, which in a test is a programming error.
func Facts(name string) FixtureFacts {
	if IsLoad(name) {
		f, err := factsOfLoad(name)
		if err != nil {
			panic(err)
		}
		f.Subtypes = maps.Clone(f.Subtypes)
		f.PerChannel = maps.Clone(f.PerChannel)
		return f
	}
	f := mustGet(name).facts
	f.Subtypes = maps.Clone(f.Subtypes)
	f.PerChannel = maps.Clone(f.PerChannel)
	return f
}

// Build writes the named fixture to w as a ZIP archive. The output is
// byte-identical across calls and processes.
func Build(name string, w io.Writer, opts ...Option) error {
	if IsLoad(name) {
		s, err := ParseLoadSpec(name)
		if err != nil {
			return err
		}
		_, err = WriteLoad(s, w, opts...)
		return err
	}
	b, err := get(name)
	if err != nil {
		return err
	}
	cfg := newConfig(opts)
	files, err := cfg.render(b.files)
	if err != nil {
		return err
	}
	return writeZip(w, files, cfg)
}

// Write writes the named fixture to <dir>/<name>.zip and returns that path.
func Write(name, dir string, opts ...Option) (path string, err error) {
	file := name
	if IsLoad(name) {
		// Whatever its size, a load fixture is written as load.zip.
		if _, err := ParseLoadSpec(name); err != nil {
			return "", err
		}
		file = LoadPrefix
	} else if _, err := get(name); err != nil {
		return "", err
	}
	path = filepath.Join(dir, file+".zip")
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	return path, Build(name, f, opts...)
}

// WriteDir writes the named fixture as an extracted export under
// <dir>/<name> and returns that path.
func WriteDir(name, dir string, opts ...Option) (string, error) {
	b, err := get(name)
	if err != nil {
		return "", err
	}
	cfg := newConfig(opts)
	files, err := cfg.render(b.files)
	if err != nil {
		return "", err
	}
	root := filepath.Join(dir, name)
	for _, file := range slices.Sorted(maps.Keys(files)) {
		dst := filepath.Join(root, filepath.FromSlash(cfg.entry(file)))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(dst, files[file], 0o644); err != nil {
			return "", err
		}
	}
	return root, nil
}

// People returns the users of the named fixture's users.json, in file order.
// It panics on an unknown name.
func People(name string) []Person {
	if IsLoad(name) {
		s, err := ParseLoadSpec(name)
		if err != nil {
			panic(err)
		}
		return loadPeople(s)
	}
	return slices.Clone(mustGet(name).people)
}

// Known reports whether name is a fixture: one of Names, or a valid load
// fixture name.
func Known(name string) bool {
	if IsLoad(name) {
		_, err := ParseLoadSpec(name)
		return err == nil
	}
	_, ok := builders[name]
	return ok
}

func newConfig(opts []Option) config {
	var c config
	for _, o := range opts {
		o(&c)
	}
	return c
}

func (c config) entry(name string) string {
	if c.wrap == "" {
		return name
	}
	return c.wrap + "/" + name
}

// writeZip writes files in name order with fixed metadata so the archive is
// reproducible.
func writeZip(w io.Writer, files map[string][]byte, cfg config) error {
	zw := zip.NewWriter(w)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		fw, err := zw.CreateHeader(&zip.FileHeader{
			Name:     cfg.entry(name),
			Method:   zip.Deflate,
			Modified: zipTime,
		})
		if err != nil {
			return err
		}
		if _, err := fw.Write(files[name]); err != nil {
			return err
		}
	}
	return zw.Close()
}
