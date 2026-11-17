package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/graph"
	"github.com/bgfdev/taut/internal/pipeline"
	"github.com/bgfdev/taut/internal/store"
)

// Direct messages and group DMs are imported into Teams chats.
//
// A conversation of kind "dm" or "mpim" is worked by the same lanes as a
// channel. This file holds what differs: how its chat comes to exist, how
// that chat is put into migration mode, and what happens when a message a
// person wrote in Teams already holds the millisecond a Slack message was
// given.
//
// Everything here follows Microsoft's documentation as read on 2026-11-15
// and has never been run against a real tenant. What had to be assumed is
// listed under "Known limitations" in docs/migration-guide.md.

// Audit log operations for chats.
const (
	opChatCreate   = "chat.create"
	opChatAdopt    = "chat.adopt"
	opChatStart    = "chat.start_migration"
	opChatComplete = "chat.complete_migration"
	// A conversation Microsoft refused, and a chat taken out of migration
	// mode because its run will not be finished.
	opChatSkip    = "chat.skip"
	opChatRelease = "chat.release_migration"
)

// chatClockSkew is how far this worker's clock and Microsoft's may differ
// without a chat this run created being mistaken for an older one (or the
// other way round).
const chatClockSkew = 5 * time.Minute

// chatCursor is what a chat's load lane remembers between attempts.
type chatCursor struct {
	// CreateStarted is when this lane first set out to create the chat.
	// It is recorded before the request, so a later attempt knows that an
	// earlier one may have created a chat whose ID was lost, and since
	// when to look for it.
	CreateStarted *time.Time `json:"createStarted,omitempty"`
	// Ours is true when the chat is one Taut made (in this run or an
	// earlier one), as opposed to a one-on-one chat two people already
	// had. Only a chat people already had may be newer than the history.
	Ours bool `json:"ours,omitempty"`
}

// chatSource is how the audit log names a direct-message conversation.
func chatSource(slackID string) string { return "chat/" + slackID }

// chatMembers returns the Entra object IDs of a chat's participants. The
// plan stage admitted the conversation only because every participant has
// one, so a gap here means the ledger is damaged.
func (d *Worker) chatMembers(ctx context.Context, rs *runState, cm store.ChannelMapping) ([]string, error) {
	users, _, err := d.mappings(ctx, rs)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(cm.Members))
	for _, slackID := range cm.Members {
		u, ok := users[slackID]
		if !ok || u.EntraID == "" || slackID == pipeline.PlaceholderUser {
			return nil, permanent(fmt.Errorf("participant %s of direct message %s has no Microsoft 365 account in the run's plan", slackID, cm.SlackID))
		}
		ids = append(ids, u.EntraID)
	}
	if len(ids) < 2 {
		return nil, permanent(fmt.Errorf("direct message %s has %d participants in the run's plan; a chat needs two", cm.SlackID, len(ids)))
	}
	return ids, nil
}

// chatRefusal is a definite refusal by Microsoft of one conversation's chat:
// to create it, or to put it into migration mode. It is about that chat or
// its people (a participant without a Teams licence, a policy), not about
// the run, so the conversation is skipped and the run goes on.
type chatRefusal struct {
	code, reason string
}

func (r *chatRefusal) Error() string { return r.reason }

// refusalOf says whether err is a refusal of this one chat, and if so
// which. The reason is Microsoft's own, without the names of anybody.
func refusalOf(err error, what string) *chatRefusal {
	if !graph.IsChatRefused(err) {
		return nil
	}
	return &chatRefusal{code: code(err), reason: clip(what + ": " + describe(err))}
}

// skipChat records that a conversation is not imported, with the reason
// and an audit entry, and completes the batch: the lane goes on, and its
// remaining batches find the conversation skipped.
func (d *Worker) skipChat(ctx context.Context, lease *store.Lease, b *store.Batch, cm store.ChannelMapping, r *chatRefusal) error {
	err := d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error {
		if err := tx.SkipChannel(ctx, cm.SlackID, r.code, r.reason); err != nil {
			return err
		}
		e := d.entry(lease, opChatSkip, chatSource(cm.SlackID), cm.TeamsID, "")
		e.Outcome, e.Detail = "error", clip(r.code+": "+r.reason)
		if err := tx.Log(ctx, e); err != nil {
			return err
		}
		if b == nil {
			return nil
		}
		return tx.CompleteBatch(ctx, b.Seq, "", 0, nil)
	})
	if err == nil {
		d.cfg.Log.Warn("a direct-message conversation is skipped", "conversation", cm.SlackID, "code", r.code)
	}
	return err
}

// definite reports whether Graph answered a request with a refusal, so
// that it certainly did not do what was asked. A lost answer, a timeout or
// a server error are not that.
func definite(err error) bool {
	var api *graph.APIError
	if !errors.As(err, &api) {
		return false
	}
	return api.Status >= 400 && api.Status < 500 && api.Status != http.StatusRequestTimeout &&
		api.Status != http.StatusTooManyRequests && api.Status != http.StatusConflict
}

func (d *Worker) setChatCursor(ctx context.Context, lease *store.Lease, cur chatCursor) error {
	raw, _ := json.Marshal(cur)
	if err := d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error { return tx.SetCursor(ctx, raw) }); err != nil {
		return err
	}
	lease.Cursor = raw
	return nil
}

func readChatCursor(lease *store.Lease) (chatCursor, error) {
	var cur chatCursor
	if len(lease.Cursor) > 0 {
		if err := json.Unmarshal(lease.Cursor, &cur); err != nil {
			return cur, permanent(fmt.Errorf("lane cursor is corrupt: %w", err))
		}
	}
	return cur, nil
}

// adoptable decides whether a group chat that has exactly the right people
// is one Taut made, as opposed to one those people made for themselves.
// Membership alone does not say: adopting a chat people use would pour
// history into it and put it into migration mode.
//
// It is Taut's if it still has the creation time Taut gave it (known for a
// chat an earlier run recorded), or if it is in migration mode, or if it
// is empty, as a chat is whose creation was interrupted before anything
// was imported.
func adoptable(ctx context.Context, g Graph, chat graph.Chat, members []string, expected *time.Time) (bool, error) {
	if chat.ChatType != graph.ChatGroup {
		return false, nil
	}
	have, err := g.ChatMembers(ctx, chat.ID)
	if err != nil {
		return false, err
	}
	if !graph.SameChatMembers(have, members) {
		return false, nil
	}
	if expected != nil && !chat.CreatedDateTime.IsZero() && chat.CreatedDateTime.Equal(*expected) {
		return true, nil
	}
	if chat.MigrationMode == graph.MigrationInProgress {
		return true, nil
	}
	return g.ChatIsEmpty(ctx, chat.ID)
}

// earlierChat looks for the group chat an earlier run of this migration
// created for the same Slack conversation in the same tenant. A group chat
// is new with every request, so without this `taut migrate --new` would
// give every group DM a second chat. The chat is taken only if it still
// has exactly this conversation's people and is recognisably Taut's.
func (d *Worker) earlierChat(ctx context.Context, rs *runState, g Graph, cm store.ChannelMapping, members []string) (graph.Chat, string, error) {
	earlier, err := d.cfg.Store.EarlierChats(ctx, rs.run.ID, cm.SlackID)
	if err != nil {
		return graph.Chat{}, "", err
	}
	tenant := strings.ToLower(strings.TrimSpace(rs.m.Target.Entra.TenantID))
	for _, e := range earlier {
		m, err := config.Parse(e.Config)
		if err != nil || strings.ToLower(strings.TrimSpace(m.Target.Entra.TenantID)) != tenant {
			continue
		}
		chat, err := g.GetChat(ctx, e.TeamsID)
		if graph.IsNotFound(err) {
			continue
		}
		if err != nil {
			return graph.Chat{}, "", err
		}
		ok, err := adoptable(ctx, g, chat, members, e.MigrationStart)
		if graph.IsNotFound(err) {
			continue
		}
		if err != nil {
			return graph.Chat{}, "", err
		}
		if ok {
			return chat, e.RunID, nil
		}
	}
	return graph.Chat{}, "", nil
}

// createChat is the first batch of a direct message's load lane: make sure
// its chat exists. It is safe to repeat.
//
// A one-on-one chat is unique to its two people: Graph answers a request to
// create one that exists with the existing chat. So the request is its own
// idempotency, and the history is imported into the chat the two already
// use, if they have one.
//
// A group chat is new with every request, and Graph offers no key to make
// creation repeatable. Two things keep a conversation to one chat:
//
//   - A chat an earlier run recorded for the same conversation is used
//     again (earlierChat), so starting a migration over does not double
//     every group DM.
//   - The lane records that it is about to create the chat before it asks.
//     If the answer is lost, any later attempt first looks through one
//     participant's group chats for one with exactly these people, created
//     since, that is recognisably Taut's (adoptable). The mark is cleared
//     once Graph has definitely refused, so the window in which a chat can
//     be adopted is only as long as an answer is in doubt.
//
// What remains open: the chat may not be listed yet when the retry looks
// (Graph's lists are eventually consistent), which leaves an extra, empty
// chat behind.
//
// A definite refusal of this one chat (a participant without a licence, a
// policy) skips the conversation; it does not fail the run.
func (d *Worker) createChat(ctx context.Context, rs *runState, lease *store.Lease, b *store.Batch, cm store.ChannelMapping) error {
	if cm.TeamsID != "" || cm.State == store.ChannelSkipped {
		return d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error { return tx.CompleteBatch(ctx, b.Seq, "", 1, nil) })
	}
	members, err := d.chatMembers(ctx, rs, cm)
	if err != nil {
		return err
	}
	g, err := d.client(ctx, rs)
	if err != nil {
		return err
	}
	cur, err := readChatCursor(lease)
	if err != nil {
		return err
	}
	group := cm.Kind == store.KindMPIM

	var chat graph.Chat
	op, detail, ours := opChatCreate, "", true
	if group {
		found, from, err := d.earlierChat(ctx, rs, g, cm, members)
		if err != nil {
			return err
		}
		if found.ID != "" {
			chat, op = found, opChatAdopt
			detail = "the chat run " + from + " created for this conversation; used again"
		}
	}
	if chat.ID == "" && group && cur.CreateStarted != nil {
		since := cur.CreateStarted.Add(-chatClockSkew)
		found, err := g.FindGroupChats(ctx, members[0], members, since)
		if err != nil {
			return err
		}
		for _, candidate := range found {
			// The listing does not say everything; ask the chat itself.
			full, err := g.GetChat(ctx, candidate.ID)
			if graph.IsNotFound(err) {
				continue
			}
			if err != nil {
				return err
			}
			ok, err := adoptable(ctx, g, full, members, nil)
			if err != nil {
				return err
			}
			if ok {
				chat, op = full, opChatAdopt
				detail = "found with exactly these participants, empty or in migration mode, created since this run set out to create it; taken to be the chat an interrupted attempt created"
				break
			}
		}
	}
	if chat.ID == "" {
		spec := graph.ChatSpec{Group: group, Topic: cm.Topic, Members: members}
		if group {
			// Members of a group chat see its history from a point in
			// time. Ask for that to be the start of the conversation;
			// whether Graph takes it at creation is not documented.
			start, err := d.channelStart(ctx, rs.run.ID, cm)
			if err != nil {
				return err
			}
			spec.VisibleHistoryStart = &start
		}
		if err := d.tenant.Wait(ctx); err != nil {
			return err
		}
		// From here on a chat may exist that the ledger does not know.
		if cur.CreateStarted == nil {
			now := time.Now().UTC()
			cur.CreateStarted = &now
			if err := d.setChatCursor(ctx, lease, cur); err != nil {
				return err
			}
		}
		since := cur.CreateStarted.Add(-chatClockSkew)
		chat, err = g.CreateChat(ctx, spec)
		if graph.IsChatHistoryStart(err) {
			spec.VisibleHistoryStart = nil
			detail = "created without a history start time for its members, which Microsoft Teams did not accept"
			chat, err = g.CreateChat(ctx, spec)
		}
		if err != nil && definite(err) {
			// Graph said no, so no chat came of it. Forget that one might
			// have: a later attempt must not go looking for it.
			cur.CreateStarted = nil
			if cerr := d.setChatCursor(ctx, lease, cur); cerr != nil {
				return cerr
			}
		}
		switch r := refusalOf(err, "Microsoft Teams refused to create the chat"); {
		case r != nil:
			return d.skipChat(ctx, lease, b, cm, r)
		case err != nil:
			return err
		case chat.ID == "":
			return errors.New("the chat was created, but Microsoft Graph did not say which it is")
		}
		if !group && !chat.CreatedDateTime.IsZero() && chat.CreatedDateTime.Before(since) {
			op, detail, ours = opChatAdopt, "the two people already had this chat; the history is imported into it", false
		}
	}
	raw, _ := json.Marshal(chatCursor{Ours: ours})
	err = d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error {
		if err := tx.SetChannel(ctx, cm.SlackID, chat.ID, store.ChannelCreated, nil); err != nil {
			return err
		}
		e := d.entry(lease, op, chatSource(cm.SlackID), chat.ID, "")
		e.Detail = detail
		if err := tx.Log(ctx, e); err != nil {
			return err
		}
		if err := tx.SetCursor(ctx, raw); err != nil {
			return err
		}
		return tx.CompleteBatch(ctx, b.Seq, "", 1, nil)
	})
	if err == nil {
		lease.Cursor = raw
	}
	return err
}

// ensureChatMigrating puts a chat into migration mode, with a creation time
// before the conversation's oldest message where that is possible, and
// records the creation time Graph then reports. It is safe to repeat, and
// it is how a chat is reopened if something completed its migration
// mid-import.
//
// Graph moves a chat's creation time backwards only, and only when
// migration mode starts. A chat Taut made is as new as the run, so it is
// always moved; if Graph leaves it where it is, nothing of the
// conversation could be imported and the lane fails, saying so. A chat two
// people already had may be older than their Slack history; then no time
// is sent and it keeps its own. If such a chat ends up newer than some of
// the history all the same, it is not failed, unlike a channel: two people
// have one one-on-one chat, there is no other to import into. The messages
// that are older than the chat are recorded as not imported and the rest
// go in.
//
// A definite refusal of this chat is returned as a *chatRefusal, for the
// caller to skip the conversation.
func (d *Worker) ensureChatMigrating(ctx context.Context, rs *runState, lease *store.Lease, g Graph, cm store.ChannelMapping) (time.Time, error) {
	cur, err := readChatCursor(lease)
	if err != nil {
		return time.Time{}, err
	}
	want, err := d.channelStart(ctx, rs.run.ID, cm)
	if err != nil {
		return time.Time{}, err
	}
	chat, err := g.GetChat(ctx, cm.TeamsID)
	if graph.IsNotFound(err) {
		return time.Time{}, &chatRefusal{code: "chat_gone", reason: "the chat no longer exists in Teams"}
	}
	if err != nil {
		return time.Time{}, err
	}
	needsBackdate := chat.CreatedDateTime.IsZero() || want.Before(chat.CreatedDateTime)
	reentered := chat.MigrationMode == graph.MigrationCompleted
	if chat.MigrationMode == graph.MigrationInProgress && needsBackdate {
		// In migration mode already, but not far enough back: close and
		// reopen it with the earlier time, as for a channel.
		if err := g.CompleteChatMigration(ctx, cm.TeamsID); err != nil && !graph.IsNotInMigration(err) {
			return time.Time{}, err
		}
		chat.MigrationMode = ""
	}
	if chat.MigrationMode != graph.MigrationInProgress {
		// Always with a creation time that is older than the chat's
		// current one; see restartTime.
		when := restartTime(want, chat.CreatedDateTime)
		if err := d.tenant.Wait(ctx); err != nil {
			return time.Time{}, err
		}
		switch err := g.StartChatMigration(ctx, cm.TeamsID, &when); {
		case err == nil, graph.IsAlreadyMigrating(err):
		case graph.IsNotFound(err):
			return time.Time{}, &chatRefusal{code: "chat_migration_unavailable",
				reason: "Microsoft Graph has no migration mode for this chat (startMigration answered that it does not exist; for chats it is available in the global cloud only)"}
		default:
			if r := refusalOf(err, "Microsoft Teams refused to put the chat into migration mode"); r != nil {
				return time.Time{}, r
			}
			return time.Time{}, err
		}
	}

	// Trust what Graph now reports, not what was asked for.
	chat, err = g.GetChat(ctx, cm.TeamsID)
	if err != nil {
		return time.Time{}, err
	}
	if chat.MigrationMode != "" && chat.MigrationMode != graph.MigrationInProgress {
		return time.Time{}, fmt.Errorf("the chat of direct message %s did not enter migration mode", cm.SlackID)
	}
	start, detail := want, ""
	if !chat.CreatedDateTime.IsZero() {
		start = chat.CreatedDateTime.UTC()
	}
	if start.After(want) {
		if cur.Ours {
			// Taut made this chat and asked for it to be older than the
			// conversation. If it is not, Graph did not do what its
			// documentation says, and that is true of every chat of the
			// run: stop and say so, rather than write off the history
			// message by message.
			return time.Time{}, permanent(fail.New("load.chat_not_backdated",
				"Microsoft Teams did not move a new chat's creation time back, so nothing can be imported into it",
				fmt.Sprintf("The chat of %s was created by Taut and asked to start at %s, but Microsoft Graph reports its creation time as %s. A message cannot be older than its chat.",
					chatLabel(cm), want.Format(time.RFC3339), start.Format(time.RFC3339)),
				"Nothing was imported into this chat. This is not something a retry fixes: report it, and leave direct messages out for now (target.directMessages.enabled: false, then `taut migrate --new`, which also takes the chats of this run out of migration mode)."))
		}
		detail = fmt.Sprintf("the chat's creation time stayed at %s; messages up to then cannot be imported", start.Format(time.RFC3339))
	}
	err = d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error {
		if err := tx.SetChannel(ctx, cm.SlackID, cm.TeamsID, store.ChannelMigrating, &start); err != nil {
			return err
		}
		e := d.entry(lease, opChatStart, chatSource(cm.SlackID), cm.TeamsID, "")
		e.Detail = detail
		if reentered && detail == "" {
			e.Detail = reenteredDetail
		}
		return tx.Log(ctx, e)
	})
	return start, err
}

// release takes a chat out of migration mode because its run will not be
// worked to the end: the run was cancelled or replaced, or it failed and
// the operator asked for its chats back. Without it the chat would stay in
// migration mode for good, with its banner, in front of the people in it:
// nobody finalizes a run that is over.
//
// It is safe to repeat. A chat that is not in migration mode, or is gone,
// is done.
func (d *Worker) release(ctx context.Context, rs *runState, lease *store.Lease, b *store.Batch) error {
	cm, err := d.cfg.Store.Channel(ctx, rs.run.ID, lease.Key)
	if err != nil {
		return err
	}
	done := func() error {
		return d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error { return tx.CompleteBatch(ctx, b.Seq, "", 1, nil) })
	}
	if !cm.IsChat() || cm.TeamsID == "" || cm.State == store.ChannelCompleted || cm.State == store.ChannelSkipped {
		return done()
	}
	g, err := d.client(ctx, rs)
	if err != nil {
		return err
	}
	if err := d.tenant.Wait(ctx); err != nil {
		return err
	}
	detail := "the run is " + string(rs.currentStatus(ctx, d)) + "; the chat is taken out of migration mode"
	switch err := g.CompleteChatMigration(ctx, cm.TeamsID); {
	case err == nil:
	case graph.IsNotInMigration(err):
		detail += " (it was not in it)"
	case graph.IsNotFound(err):
		detail += " (it no longer exists)"
	default:
		return err
	}
	err = d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error {
		if err := tx.SetChannel(ctx, cm.SlackID, cm.TeamsID, store.ChannelCompleted, cm.MigrationStart); err != nil {
			return err
		}
		e := d.entry(lease, opChatRelease, chatSource(cm.SlackID), cm.TeamsID, "")
		e.Detail = detail
		if err := tx.Log(ctx, e); err != nil {
			return err
		}
		return tx.CompleteBatch(ctx, b.Seq, "", 1, nil)
	})
	if err == nil {
		rs.mu.Lock()
		rs.users, rs.channels = nil, nil
		rs.mu.Unlock()
	}
	return err
}

// currentStatus reads the run's status now; the cached run is as old as
// this worker's first look at it.
func (rs *runState) currentStatus(ctx context.Context, d *Worker) store.RunStatus {
	if run, err := d.cfg.Store.GetRun(ctx, rs.run.ID); err == nil {
		return run.Status
	}
	return rs.run.Status
}

// maxShiftMS is how far a post may be moved from its allocated time to get
// off milliseconds that existing messages of a chat hold.
const maxShiftMS = 100

// shiftPosts moves a post one millisecond later, in the ledger first, after
// Graph refused it because a message that is not Taut's holds its
// millisecond. Every post of the conversation that follows it without a
// gap moves with it: the other parts of a split message, or a message
// allocated the very next millisecond. Moving the post alone would put it
// on top of the next one.
//
// It reports false when the posts cannot be moved: one of them is already
// in Teams, or has been moved as far as it may be.
//
// The move is recorded before the post is sent again, so every later
// attempt, by this worker or another, sends it at the same new time, and a
// post that Graph accepted there is recognised as Taut's on retry. The
// timestamp stays an idempotency key; it has only changed value, in the
// ledger, before the request that uses it.
func (d *Worker) shiftPosts(ctx context.Context, lease *store.Lease, row *store.Message, p *pipeline.Post) (bool, error) {
	err := d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error {
		_, err := tx.ShiftMessages(ctx, row.ChannelID, row.TargetMS, maxShiftMS)
		return err
	})
	switch {
	case errors.Is(err, store.ErrTimestampTaken), errors.Is(err, store.ErrShiftLimit):
		return false, nil
	case err != nil:
		return false, err
	}
	row.TargetMS++
	row.ShiftMS++
	p.TargetMS = row.TargetMS
	return true, nil
}

// failRest records that a message's remaining parts, from part on, are not
// imported, in one transaction. A message is imported whole from its first
// part or not from there at all: its tail is never left in a chat without
// its head.
func (d *Worker) failRest(ctx context.Context, lease *store.Lease, p pipeline.Post, errCode, reason string) error {
	err := d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error {
		for part := p.Part; part < max(p.Parts, p.Part+1); part++ {
			key := store.MessageKey{ChannelID: p.ChannelID, SlackTS: p.SlackTS, Part: part}
			if err := tx.MarkFailed(ctx, key, store.MessageFailed, errCode, reason); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					continue
				}
				return err
			}
			e := d.entry(lease, opFail, fmt.Sprintf("%s/%s/%d", p.ChannelID, p.SlackTS, part), "", "")
			e.Outcome, e.Detail = "error", errCode
			if err := tx.Log(ctx, e); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		d.cfg.Observer.Count(ctx, store.StageLoad, EventPostRejected, 1)
	}
	return err
}

// settleSkipped is a day batch of a conversation that was skipped: its
// posts are recorded as not imported, once, so the totals add up and the
// run can finish.
func (d *Worker) settleSkipped(ctx context.Context, rs *runState, lease *store.Lease, b *store.Batch, cm store.ChannelMapping) error {
	rows, err := d.cfg.Store.MessagesInBatch(ctx, rs.run.ID, cm.SlackID, b.Seq-loadSeqDays)
	if err != nil {
		return err
	}
	const errCode = "conversation_skipped"
	for len(rows) > 0 {
		n := min(len(rows), 200)
		part := rows[:n]
		err := d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error {
			for _, row := range part {
				if row.State == store.MessagePosted || (row.State == store.MessageFailed && row.ErrorCode == errCode) {
					continue
				}
				if err := tx.MarkFailed(ctx, row.MessageKey, store.MessageFailed, errCode, "its conversation was not imported: "+cm.SkipCode); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		rows = rows[n:]
	}
	stats, _ := json.Marshal(map[string]int{errCode: 1})
	return d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error { return tx.CompleteBatch(ctx, b.Seq, "", 0, stats) })
}

// mentionTag is a mention in a post's HTML.
var mentionTag = regexp.MustCompile(`<at id="\d+">(.*?)</at>`)

// plainMentions turns the mentions of a post's HTML into the names they
// show, for a chat that will not take a mention of someone outside it.
func plainMentions(html string) string { return mentionTag.ReplaceAllString(html, "$1") }

// shiftDetail is the audit log's note on a post that was moved.
func shiftDetail(ms int64) string {
	if ms == 0 {
		return ""
	}
	return fmt.Sprintf("imported %d ms later than allocated: an existing message of the chat held the millisecond", ms)
}

// chatLabel names a direct message in an error. Its participants' names
// stay out of errors and logs; the Slack ID identifies it.
func chatLabel(cm store.ChannelMapping) string {
	kind := "direct message"
	if cm.Kind == store.KindMPIM {
		kind = "group direct message"
	}
	return strings.TrimSpace(kind + " " + cm.SlackID)
}

// conversation names a channel or a direct message in an error.
func conversation(cm store.ChannelMapping) string {
	if cm.IsChat() {
		return chatLabel(cm)
	}
	return "channel " + cm.SlackName
}
