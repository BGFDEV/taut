// Package pipeline defines the records that flow between stages and how they
// are written to staging.
//
// Each stage reads one batch file and writes one batch file. A batch file is
// gzip-compressed NDJSON: one JSON record per line. Files are written through
// the encrypted staging layer, so nothing here handles keys.
package pipeline

import (
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"strconv"
	"time"
)

// Mention is one @mention inside a post.
type Mention struct {
	ID     int    `json:"id"`
	Text   string `json:"text"`
	UserID string `json:"userId"` // Entra object ID
}

// Post is one Teams message, fully decided: where it goes, when it claims
// to have been written, who it is from and what it says. The load stage
// sends it as-is; it makes no further decisions about content.
type Post struct {
	ChannelID string `json:"channel"` // Slack channel ID
	SlackTS   string `json:"ts"`
	Part      int    `json:"part"`
	Parts     int    `json:"parts"`
	// TargetMS is the allocated createdDateTime, Unix milliseconds.
	TargetMS int64 `json:"targetMs"`
	// AnchorTS is the Slack ts of the post this one replies to, or empty
	// for a top-level post.
	AnchorTS string `json:"anchor,omitempty"`
	// FromID is the Entra object ID the post is attributed to; FromName is
	// that account's display name.
	FromID   string    `json:"fromId"`
	FromName string    `json:"fromName"`
	HTML     string    `json:"html"`
	Mentions []Mention `json:"mentions,omitempty"`
	// Files are the Slack file IDs this post refers to through file
	// markers in HTML (see FileMarker). The load stage resolves each marker
	// to the uploaded file, or to a note if the file could not be moved.
	Files []string `json:"files,omitempty"`
	// Hash is ContentHash() at the time the post was produced.
	Hash string `json:"hash"`
}

// CreatedAt returns the post's timestamp.
func (p Post) CreatedAt() time.Time { return time.UnixMilli(p.TargetMS).UTC() }

// ContentHash is the hex SHA-256 of everything sent to Graph for this post:
// the timestamp, the author, the body and the mentions. Fields are
// length-prefixed so that no two different posts can encode to the same
// bytes.
func (p Post) ContentHash() string {
	h := sha256.New()
	field := func(s string) {
		// A hash.Hash never returns an error from Write.
		_, _ = fmt.Fprintf(h, "%d:", len(s))
		_, _ = io.WriteString(h, s)
	}
	field(strconv.FormatInt(p.TargetMS, 10))
	field(p.FromID)
	field(p.HTML)
	field(strconv.Itoa(len(p.Files)))
	for _, f := range p.Files {
		field(f)
	}
	field(strconv.Itoa(len(p.Mentions)))
	for _, m := range p.Mentions {
		field(strconv.Itoa(m.ID))
		field(m.Text)
		field(m.UserID)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// OverridesKey is the staging key of a run's identity override CSV, uploaded
// by `taut migrate` alongside the export.
func OverridesKey(runID string) string { return "runs/" + runID + "/source/overrides.csv" }

// PlaceholderUser is the reserved Slack ID under which the plan stage
// records the account that stands in for authors with no Microsoft 365
// identity. Real Slack IDs never start with "$".
const PlaceholderUser = "$placeholder"

// FileMarkerPrefix prefixes a Slack file ID wherever a post's HTML refers to
// a file before it has been uploaded: `<attachment id="taut-file:F123">` for
// an attachment and `<img src="taut-file:F123">` for an inline image.
const FileMarkerPrefix = "taut-file:"

// LayoutVersion identifies the shape of the ledger the plan stage writes:
// which lanes exist, what each batch number means, where stages find their
// input. Workers refuse to work a run planned under another layout, so a
// rollout that leaves old and new workers running together cannot have
// them misread each other's batches.
//
// Version 3: the members lane also adds the members of private channels
// (and their members to the team), and under the legacy strategy it
// completes the team's migration first; a channel's "open" batch does
// nothing under the legacy strategy. The lanes and batch numbers are those
// of version 2, but a version 2 worker would finish a members batch without
// doing any of that.
const LayoutVersion = 3

// LayoutWithChats is the layout of a run that migrates direct messages into
// chats. Such a run has, besides everything of LayoutVersion,
// conversations that belong to no team: their load lane begins with
// "create the chat" and "start its migration mode", they have no files
// lane, and their finalize lane completes a chat.
//
// Nothing changes for channels, so a run without direct messages keeps
// LayoutVersion and a worker of this version works both. A worker that
// predates chats knows only LayoutVersion and refuses a run with chats,
// rather than treating a chat as a channel of a team that does not exist.
const LayoutWithChats = 4

// LayoutFor is the layout a run needs, given whether its config migrates
// direct messages. It is decided when the run is created and stored with
// it, before any worker plans it, so a worker that does not know chats
// refuses the plan lane instead of planning the run without them.
func LayoutFor(directMessages bool) int {
	if directMessages {
		return LayoutWithChats
	}
	return LayoutVersion
}

// LayoutSupported reports whether this version of Taut can work a run
// planned under the given layout.
func LayoutSupported(layout int) bool {
	return layout == LayoutVersion || layout == LayoutWithChats
}

// Totals is what the plan stage knows about the size of a run, stored on the
// run for progress displays.
type Totals struct {
	// Layout is the LayoutVersion the run was planned under.
	Layout   int `json:"layout"`
	Teams    int `json:"teams"`
	Channels int `json:"channels"`
	// Chats is the number of direct-message conversations that migrate.
	// They are not counted in Channels.
	Chats  int `json:"chats,omitempty"`
	Days   int `json:"days"`
	Users  int `json:"users"`
	Mapped int `json:"mapped"`
}

// Writer writes records as gzip-compressed NDJSON.
type Writer struct {
	gz  *gzip.Writer
	enc *json.Encoder
	n   int
}

// NewWriter starts a batch file on w. Close must be called to flush it; it
// does not close w.
func NewWriter(w io.Writer) *Writer {
	gz := gzip.NewWriter(w)
	enc := json.NewEncoder(gz)
	enc.SetEscapeHTML(false)
	return &Writer{gz: gz, enc: enc}
}

// Write appends one record.
func (w *Writer) Write(v any) error {
	w.n++
	return w.enc.Encode(v)
}

// Count returns the number of records written.
func (w *Writer) Count() int { return w.n }

// Close flushes the compressed stream.
func (w *Writer) Close() error { return w.gz.Close() }

// maxRecord bounds one record. Slack caps message text far below this; the
// limit exists so a corrupt file cannot exhaust memory.
const maxRecord = 16 << 20

// Read decodes a batch file. Iteration stops at the first error, which is
// yielded with a zero record.
func Read[T any](r io.Reader) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		gz, err := gzip.NewReader(r)
		if err != nil {
			yield(zero, fmt.Errorf("batch file is not gzip: %w", err))
			return
		}
		defer gz.Close()
		sc := bufio.NewScanner(gz)
		sc.Buffer(make([]byte, 0, 64<<10), maxRecord)
		line := 0
		for sc.Scan() {
			line++
			if len(sc.Bytes()) == 0 {
				continue
			}
			var v T
			if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
				yield(zero, fmt.Errorf("batch record %d is not valid: %w", line, err))
				return
			}
			if !yield(v, nil) {
				return
			}
		}
		if err := sc.Err(); err != nil {
			yield(zero, fmt.Errorf("reading batch file: %w", err))
		}
	}
}
