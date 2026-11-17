package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/identity"
	"github.com/bgfdev/taut/internal/pipeline"
	"github.com/bgfdev/taut/internal/slack"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/transform"
)

// cursor is the transform lane's state: the last timestamp it allocated in
// this channel.
type cursor struct {
	LastMS int64 `json:"lastMs"`
}

// resolver adapts the run's stored mappings to the transform package.
type resolver struct {
	users    map[string]store.UserMapping
	channels map[string]store.ChannelMapping
}

func (r resolver) User(id string) (transform.UserRef, bool) {
	u, ok := r.users[id]
	if !ok {
		return transform.UserRef{}, false
	}
	return transform.UserRef{Name: u.Name, EntraID: u.EntraID}, true
}

func (r resolver) Channel(id string) (string, bool) {
	c, ok := r.channels[id]
	// A direct message is not something a message can link to, and its
	// label is the names of its participants: never resolve one.
	if !ok || c.IsChat() {
		return "", false
	}
	return c.TargetName, true
}

// attribution decides who a post is from.
type attribution struct {
	fromID   string // Entra object ID the post is attributed to
	fromName string
	mapped   bool   // the author's own identity is used
	name     string // the original author's name, for the byline
	kind     string // "bot", "former" or ""
}

// attribute resolves a message's author against the run's identity map and
// the config file's policy for authors with no Microsoft 365 identity.
func attribute(msg slack.Message, users map[string]store.UserMapping, policy string) (attribution, error) {
	id := msg.User
	if id == "" {
		id = msg.BotID
	}
	u, known := users[id]
	if known && u.EntraID != "" {
		return attribution{fromID: u.EntraID, fromName: u.Name, mapped: true, name: u.Name}, nil
	}

	a := attribution{name: msg.AuthorLabel()}
	if known && u.Name != "" {
		a.name = u.Name
	}
	switch {
	case msg.IsBot() || (known && u.Reason == string(identity.ReasonBot)):
		a.kind = "bot"
	case known && u.Reason == string(identity.ReasonDeactivated):
		a.kind = "former"
	}

	if policy == config.PolicyBlock {
		who := a.name
		if who == "" {
			who = id
		}
		return a, permanent(fail.New("identity.unmapped",
			"A message's author has no Entra match and the config file's policy is to block",
			fmt.Sprintf("Author %q (%s) is not mapped.", who, id),
			"Add them to the override CSV (identity.overrides) or change identity.unmapped.policy, then run `taut migrate --new`."))
	}
	ph, ok := users[pipeline.PlaceholderUser]
	if !ok {
		return a, permanent(errors.New("the run's plan has no placeholder account for unmapped authors"))
	}
	a.fromID, a.fromName = ph.EntraID, ph.Name
	return a, nil
}

// transform converts one extracted day of a channel into Teams posts and
// allocates each post's timestamp.
//
// The transform lane of a channel is strictly sequential, which is what
// makes allocation well defined: timestamps are handed out in Slack ts
// order, each one later than the last (target = max(slack time, last+1)),
// so they are unique within the channel (Graph requires that, and Taut
// relies on it for exactly-once posting) and a reply is always later than
// the post it replies to.
//
// The posts, the mapping rows, the thread anchors and the advanced cursor
// commit in one transaction with the batch's completion. A crash before
// that leaves nothing behind, so the retry starts from the same cursor and
// allocates the same timestamps.
//
// A direct message goes into a chat, and a chat has no threads.
// Every message of one becomes a top-level post, thread replies included,
// each at its own time: the day is already in time order, so a reply lands
// where it was said. Nothing is anchored, so nothing can be held for a
// missing parent, and nothing is added to say what a reply answered. A
// chat's files are not moved either; they are recorded here as skipped, so
// the load stage can name them and the totals add up.
func (d *Worker) transform(ctx context.Context, rs *runState, lease *store.Lease, b *store.Batch) error {
	channelID := lease.Key
	users, channels, err := d.mappings(ctx, rs)
	if err != nil {
		return err
	}
	in, err := d.inputKey(ctx, rs.run.ID, store.StageExtract, channelID, b.Seq)
	if err != nil {
		return err
	}
	msgs, err := readBatch[slack.Message](ctx, rs.blobs, in)
	if err != nil {
		return err
	}

	var cur cursor
	if len(lease.Cursor) > 0 {
		if err := json.Unmarshal(lease.Cursor, &cur); err != nil {
			return permanent(fmt.Errorf("lane cursor is corrupt: %w", err))
		}
	}
	isChat := channels[channelID].IsChat()
	if isChat {
		// The load stage may have moved a post of an earlier day forward,
		// off a millisecond an existing message of the chat held. Allocate
		// after everything the ledger has, not only after the cursor. If
		// a move lands between this read and the commit, the store's
		// uniqueness check refuses the commit and the batch is tried again.
		last, err := d.cfg.Store.LastTargetMS(ctx, rs.run.ID, channelID)
		if err != nil {
			return err
		}
		cur.LastMS = max(cur.LastMS, last)
	}
	res := resolver{users: users, channels: channels}
	reactions := rs.m.Content.Reactions
	if reactions == config.ReactionsNative {
		// Importing native reactions is unverified against real Graph;
		// until it is, they are summarized like the default.
		reactions = config.ReactionsFooter
	}

	var (
		posts   []pipeline.Post
		rows    []store.Message
		anchors = map[string]string{} // thread ts → anchor ts, decided in this batch
		inBatch = map[string]bool{}   // Slack ts that produced a post in this batch
		stats   = map[string]int{}
	)

	// anchorFor returns the Slack ts of the post that replies in the thread
	// threadTS should attach to, or "" if this message must become the
	// anchor itself.
	anchorFor := func(threadTS string) (string, error) {
		if inBatch[threadTS] {
			return threadTS, nil
		}
		if a, ok := anchors[threadTS]; ok {
			return a, nil
		}
		// The thread's first message, if it was migrated, is its own anchor.
		_, err := d.cfg.Store.GetMessage(ctx, rs.run.ID, store.MessageKey{ChannelID: channelID, SlackTS: threadTS})
		if err == nil {
			return threadTS, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
		// Otherwise an earlier reply may have been promoted to anchor.
		a, ok, err := d.cfg.Store.Anchor(ctx, rs.run.ID, channelID, threadTS)
		if err != nil {
			return "", err
		}
		if ok {
			return a, nil
		}
		return "", nil
	}

	for _, msg := range msgs {
		// Slack's ts is unique per channel, and it is this pipeline's key.
		// An export that repeats one would otherwise lose the second copy
		// without a trace, so it is dropped here, counted and visible.
		if inBatch[msg.TS] {
			stats["duplicate_ts"]++
			continue
		}
		class := transform.Classify(msg)
		if class.Disposition == transform.Skip {
			stats["skipped_"+class.Key]++
			continue
		}
		who, err := attribute(msg, users, rs.m.Identity.Unmapped.Policy)
		if err != nil {
			return err
		}
		if !who.mapped {
			stats["unmapped_author"]++
		}

		out, err := transform.Convert(msg, res, transform.Options{
			Reactions: reactions, AuthorMapped: who.mapped, AuthorName: who.name, AuthorKind: who.kind,
			FileMode: transform.FileModeAttach, InlineImageMax: maxInlineImage,
		})
		if err != nil || len(out.Parts) == 0 {
			// Never drop a message silently: leave a visible marker where
			// it was, and count it.
			stats["unconvertible"]++
			out = transform.Result{Parts: []transform.Part{{HTML: "<p><em>This message could not be converted from Slack.</em></p>"}}}
		}
		if len(out.Warnings) > 0 {
			stats["with_warnings"]++
		}

		// Thread placement.
		anchor := ""
		if msg.IsReply() && isChat {
			stats["flattened_reply"]++
		} else if msg.IsReply() {
			a, err := anchorFor(msg.ThreadTS)
			if err != nil {
				return err
			}
			// A message is never its own anchor. That answer can only come
			// from state a later holder of this lane committed, which means
			// this attempt is stale; treat the reply as the anchor it is.
			if a == "" || a == msg.TS {
				// The thread's first message is not being migrated (it
				// predates the date range, or was deleted). This reply
				// becomes the top-level post later replies attach to.
				anchors[msg.ThreadTS] = msg.TS
				stats["promoted_reply"]++
			} else {
				anchor = a
			}
		}

		base := max(msg.Time().UnixMilli(), cur.LastMS+1)
		for i, part := range out.Parts {
			p := pipeline.Post{
				ChannelID: channelID, SlackTS: msg.TS, Part: i, Parts: len(out.Parts),
				TargetMS: base + int64(i), AnchorTS: anchor,
				FromID: who.fromID, FromName: who.fromName, HTML: part.HTML, Files: part.Files,
			}
			// Continuation parts of a top-level message go into its own
			// thread, where they sort directly beneath it. In a chat they
			// follow it, a millisecond apart.
			if i > 0 && anchor == "" && !isChat {
				p.AnchorTS = msg.TS
			}
			for _, mn := range part.Mentions {
				p.Mentions = append(p.Mentions, pipeline.Mention{ID: mn.ID, Text: mn.Text, UserID: mn.EntraID})
			}
			p.Hash = p.ContentHash()
			posts = append(posts, p)
			rows = append(rows, store.Message{
				MessageKey: store.MessageKey{ChannelID: channelID, SlackTS: msg.TS, Part: i},
				TargetMS:   p.TargetMS, AnchorTS: p.AnchorTS, Hash: p.Hash,
				State: store.MessagePending, BatchSeq: b.Seq,
			})
		}
		cur.LastMS = base + int64(len(out.Parts)) - 1
		inBatch[msg.TS] = true
		if len(out.Parts) > 1 {
			stats["split"]++
		}
	}

	key := outputKey(rs.run.ID, store.StageTransform, channelID, b.Seq, lease.Fence)
	if _, err := writeBatch(ctx, rs.blobs, key, func(w *pipeline.Writer) error {
		for _, p := range posts {
			if err := w.Write(p); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}

	var notMoved []store.FileMapping
	if isChat {
		notMoved = chatFileRows(msgs, channelID, b.Seq)
		if len(notMoved) > 0 {
			stats["files_not_moved"] = len(notMoved)
		}
	}

	curJSON, _ := json.Marshal(cur)
	statsJSON, _ := json.Marshal(stats)
	err = d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error {
		if err := tx.PutMessages(ctx, rows); err != nil {
			return err
		}
		if err := tx.PutFiles(ctx, notMoved); err != nil {
			return err
		}
		for thread, anchor := range anchors {
			if err := tx.SetAnchor(ctx, channelID, thread, anchor); err != nil {
				return err
			}
		}
		if err := tx.SetCursor(ctx, curJSON); err != nil {
			return err
		}
		return tx.CompleteBatch(ctx, b.Seq, key, len(posts), statsJSON)
	})
	if err != nil {
		return err
	}
	// The lease carries the cursor as of the claim. Keep it current, so the
	// next batch worked under this same lease continues from here.
	lease.Cursor = curJSON
	return nil
}

// fileNotMovedDM is why a chat's file is not copied: where a chat's files
// should live is not documented by Microsoft, so Taut does not guess.
const fileNotMovedDM = "dm_files_not_moved"

// chatFileRows lists the files one day of a direct message refers to, each
// settled as skipped: nothing is moved for a chat. A file that could not
// have been moved anyway keeps its own reason.
//
// It follows the files stage's rules for what counts as a file of a
// message, so a channel and a chat count their files alike.
func chatFileRows(msgs []slack.Message, channelID string, seq int) []store.FileMapping {
	seen := map[string]bool{}
	var rows []store.FileMapping
	for _, msg := range msgs {
		if transform.Classify(msg).Disposition != transform.Import {
			continue
		}
		for _, f := range msg.Files {
			if !fileIDPattern.MatchString(f.ID) || seen[f.ID] {
				continue
			}
			seen[f.ID] = true
			row := store.FileMapping{
				ChannelID: channelID, SlackID: f.ID, Name: firstNonEmpty(f.Name, f.Title, f.ID),
				Mimetype: f.Mimetype, Size: f.Size, State: store.FileSkipped, BatchSeq: seq,
				ErrorCode: fileNotMovedDM, Error: "files of a direct message are not moved; the message says a file was there",
			}
			if code, reason := skipReason(f); code != "" {
				row.ErrorCode, row.Error = code, reason
			}
			rows = append(rows, row)
		}
	}
	return rows
}
