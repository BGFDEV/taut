package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

// --- ledger ---

// batchDepsUnmetSQL is true for a batches row aliased "b" that has a
// dependency which is not done.
const batchDepsUnmetSQL = `EXISTS (
	SELECT 1
	FROM batch_deps d
	JOIN batches db ON db.run_id = d.run_id AND db.lane_id = d.dep_lane_id AND db.seq = d.dep_seq
	WHERE d.run_id = b.run_id AND d.lane_id = b.lane_id AND d.seq = b.seq
	  AND db.status <> 'done')`

// CompleteBatch marks batch seq of the leased lane done.
//
// The store enforces the lane's order here rather than trusting the caller:
// the batch must be the lane's next runnable batch, that is, every earlier
// batch of the lane is done and every dependency is done. Otherwise it
// returns ErrOutOfOrder and changes nothing. Completing a batch that is
// already done is a no-op (its recorded output is kept); a batch that does
// not exist is ErrNotFound; a failed batch is an error.
//
// Completing a batch also resets the lane's attempt counter and last error
// (they describe the current batch, which has just changed), and marks the
// lane done if no batch is left. The lease is kept until Release.
func (t *fencedTx) CompleteBatch(ctx context.Context, seq int, output string, items int, stats json.RawMessage) error {
	if err := t.check(); err != nil {
		return err
	}
	if err := checkJSON("batch stats", stats); err != nil {
		return err
	}
	tag, err := t.tx.Exec(ctx, `
		UPDATE batches b
		SET status = 'done', output = $4, items = $5, stats = $6::json, error = '', updated_at = now()
		WHERE b.run_id = $1 AND b.lane_id = $2 AND b.seq = $3 AND b.status = 'pending'
		  AND NOT EXISTS (
		      SELECT 1 FROM batches p
		      WHERE p.run_id = b.run_id AND p.lane_id = b.lane_id
		        AND p.status <> 'done' AND p.seq < b.seq)
		  AND NOT `+batchDepsUnmetSQL,
		t.lease.RunID, t.lease.LaneID, seq, output, items, jsonArg(stats))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		status, err := t.batchStatus(ctx, seq)
		if err != nil {
			return err
		}
		switch status {
		case BatchDone:
			return nil
		case BatchFailed:
			return fmt.Errorf("store: batch %s/%d is failed; retry the run before completing it", t.lease.LaneID, seq)
		default:
			return fmt.Errorf("%w: %s/%d", ErrOutOfOrder, t.lease.LaneID, seq)
		}
	}
	_, err = t.tx.Exec(ctx, `
		UPDATE lanes l
		SET attempts = 0,
		    last_error = '',
		    status = CASE WHEN EXISTS (
		                      SELECT 1 FROM batches b
		                      WHERE b.run_id = l.run_id AND b.lane_id = l.id AND b.status <> 'done')
		                  THEN l.status ELSE 'done' END
		WHERE l.run_id = $1 AND l.id = $2`,
		t.lease.RunID, t.lease.LaneID)
	return err
}

// batchStatus returns the status of a batch of the leased lane, or
// ErrNotFound.
func (t *fencedTx) batchStatus(ctx context.Context, seq int) (BatchStatus, error) {
	var status string
	err := t.tx.QueryRow(ctx, `SELECT status FROM batches WHERE run_id = $1 AND lane_id = $2 AND seq = $3`,
		t.lease.RunID, t.lease.LaneID, seq).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: batch %s/%d", ErrNotFound, t.lease.LaneID, seq)
	}
	return BatchStatus(status), err
}

// FailBatch marks a pending batch permanently failed and the lane failed,
// recording reason on both. Failing an already failed batch is a no-op; a
// done batch cannot be failed.
func (t *fencedTx) FailBatch(ctx context.Context, seq int, reason string) error {
	if err := t.check(); err != nil {
		return err
	}
	tag, err := t.tx.Exec(ctx, `
		UPDATE batches SET status = 'failed', error = $4, updated_at = now()
		WHERE run_id = $1 AND lane_id = $2 AND seq = $3 AND status = 'pending'`,
		t.lease.RunID, t.lease.LaneID, seq, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		status, err := t.batchStatus(ctx, seq)
		if err != nil {
			return err
		}
		if status == BatchFailed {
			return nil
		}
		return fmt.Errorf("store: batch %s/%d is already done and cannot be failed", t.lease.LaneID, seq)
	}
	_, err = t.tx.Exec(ctx, `UPDATE lanes SET status = 'failed', last_error = $3 WHERE run_id = $1 AND id = $2`,
		t.lease.RunID, t.lease.LaneID, reason)
	return err
}

// SetCursor replaces the lane's cursor. An empty cursor stores NULL.
func (t *fencedTx) SetCursor(ctx context.Context, cursor json.RawMessage) error {
	if err := t.check(); err != nil {
		return err
	}
	if err := checkJSON("lane cursor", cursor); err != nil {
		return err
	}
	_, err := t.tx.Exec(ctx, `UPDATE lanes SET cursor = $3::json WHERE run_id = $1 AND id = $2`,
		t.lease.RunID, t.lease.LaneID, jsonArg(cursor))
	return err
}

// --- mapping ---

func int32Of(what string, v int) (int32, error) {
	if v < 0 || v > math.MaxInt32 {
		return 0, fmt.Errorf("store: %s %d is out of range", what, v)
	}
	return int32(v), nil
}

// PutMessages inserts post rows in state pending; State, TeamsID, Adopted,
// Carried and the error fields of the input are ignored. Edited is stored
// as given, with nil recorded as "not edited": a row written here always
// knows.
//
// A row whose key (channel, Slack ts, part) already exists is left
// completely untouched, whatever the new values are: ON CONFLICT on the
// primary key DO NOTHING. A row with a new key whose TargetMS is already
// allocated to another post of the same channel (in the table or earlier in
// the same call) violates the unique index on (run, channel, target_ms) and
// fails with ErrTimestampTaken. That error aborts the whole Fenced
// transaction: Postgres refuses further statements and the commit, so
// nothing is kept even if fn ignores the error.
func (t *fencedTx) PutMessages(ctx context.Context, msgs []Message) error {
	if err := t.check(); err != nil {
		return err
	}
	if len(msgs) == 0 {
		return nil
	}
	var (
		channels = make([]string, len(msgs))
		tss      = make([]string, len(msgs))
		parts    = make([]int32, len(msgs))
		targets  = make([]int64, len(msgs))
		anchors  = make([]string, len(msgs))
		hashes   = make([]string, len(msgs))
		seqs     = make([]int32, len(msgs))
		edits    = make([]string, len(msgs))
	)
	for i, m := range msgs {
		if m.ChannelID == "" || m.SlackTS == "" {
			return fmt.Errorf("store: message %d has no channel ID or Slack ts", i)
		}
		part, err := int32Of("message part", m.Part)
		if err != nil {
			return err
		}
		seq, err := int32Of("message batch seq", m.BatchSeq)
		if err != nil {
			return err
		}
		channels[i], tss[i], parts[i], targets[i] = m.ChannelID, m.SlackTS, part, m.TargetMS
		anchors[i], hashes[i], seqs[i] = m.AnchorTS, m.Hash, seq
		if m.Edited != nil {
			edits[i] = *m.Edited
		}
	}
	_, err := t.tx.Exec(ctx, `
		INSERT INTO map_messages (run_id, channel_id, slack_ts, part, target_ms, anchor_ts, hash, batch_seq, source_edited)
		SELECT $1, u.channel_id, u.slack_ts, u.part, u.target_ms, u.anchor_ts, u.hash, u.batch_seq, u.source_edited
		FROM unnest($2::text[], $3::text[], $4::int[], $5::bigint[], $6::text[], $7::text[], $8::int[], $9::text[])
		     AS u(channel_id, slack_ts, part, target_ms, anchor_ts, hash, batch_seq, source_edited)
		ON CONFLICT (run_id, channel_id, slack_ts, part) DO NOTHING`,
		t.lease.RunID, channels, tss, parts, targets, anchors, hashes, seqs, edits)
	switch code, constraint := pgCode(err); {
	case code == sqlUniqueViolation && constraint == "map_messages_target_ms_key":
		return fmt.Errorf("%w: %w", ErrTimestampTaken, err)
	case code == sqlForeignKeyViolation:
		return fmt.Errorf("store: PutMessages: a message refers to a channel that is not in the plan: %w", err)
	}
	return err
}

// SetAnchor records that replies in the Slack thread threadTS attach to the
// post anchorTS. First writer wins: a later call for the same thread changes
// nothing and returns nil.
func (t *fencedTx) SetAnchor(ctx context.Context, channelID, threadTS, anchorTS string) error {
	if err := t.check(); err != nil {
		return err
	}
	if channelID == "" || threadTS == "" || anchorTS == "" {
		return errors.New("store: SetAnchor needs a channel, a thread ts and an anchor ts")
	}
	_, err := t.tx.Exec(ctx, `
		INSERT INTO thread_anchors (run_id, channel_id, thread_ts, anchor_ts)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (run_id, channel_id, thread_ts) DO NOTHING`,
		t.lease.RunID, channelID, threadTS, anchorTS)
	if code, _ := pgCode(err); code == sqlForeignKeyViolation {
		return fmt.Errorf("store: SetAnchor: channel %q is not in the plan: %w", channelID, err)
	}
	return err
}

// messageState reads a post's state and Teams ID, or ErrNotFound.
func (t *fencedTx) messageState(ctx context.Context, key MessageKey) (MessageState, string, error) { //nolint:unparam // both results document the row; callers use what they need
	var state, teamsID string
	err := t.tx.QueryRow(ctx, `
		SELECT state, teams_id FROM map_messages
		WHERE run_id = $1 AND channel_id = $2 AND slack_ts = $3 AND part = $4`,
		t.lease.RunID, key.ChannelID, key.SlackTS, key.Part).Scan(&state, &teamsID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", fmt.Errorf("%w: message %s/%s/%d", ErrNotFound, key.ChannelID, key.SlackTS, key.Part)
	}
	return MessageState(state), teamsID, err
}

// MarkPosted records that a post exists in Teams. The row must exist
// (ErrNotFound). A row that is already posted is never changed: with the
// same teamsID the call is a no-op (the recorded Adopted flag is kept), with
// a different one it returns ErrTargetConflict. A pending, failed or held
// row becomes posted and its error fields are cleared.
func (t *fencedTx) MarkPosted(ctx context.Context, key MessageKey, teamsID string, adopted bool) error {
	if err := t.check(); err != nil {
		return err
	}
	if teamsID == "" {
		return errors.New("store: MarkPosted needs the Teams message ID")
	}
	tag, err := t.tx.Exec(ctx, `
		UPDATE map_messages
		SET state = 'posted', teams_id = $5, adopted = $6, error_code = '', error = '', updated_at = now()
		WHERE run_id = $1 AND channel_id = $2 AND slack_ts = $3 AND part = $4 AND state <> 'posted'`,
		t.lease.RunID, key.ChannelID, key.SlackTS, key.Part, teamsID, adopted)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	_, existing, err := t.messageState(ctx, key)
	if err != nil {
		return err
	}
	if existing != teamsID {
		return fmt.Errorf("%w: message %s/%s/%d is posted as %q, refusing to record %q",
			ErrTargetConflict, key.ChannelID, key.SlackTS, key.Part, existing, teamsID)
	}
	return nil
}

// ShiftMessages moves the post of a conversation at fromMS one millisecond
// later, together with every post that follows it without a gap: the parts
// of a split message, or messages allocated a millisecond apart. Moving
// only the first would land it on the second. The whole run moves or none
// of it does, so the order of the conversation never changes and a message
// is never left with its head at one time and its tail on top of it.
//
// Each row's ShiftMS grows by one; its Hash is left as the transform stage
// wrote it, so the staged post can still be matched with its row.
//
// It returns ErrTimestampTaken when a post in the run is already in Teams
// (it cannot move, so nothing behind it can), ErrShiftLimit when one of
// them has been moved maxShift milliseconds already, and ErrNotFound when
// no unposted post is at fromMS. Nothing is changed in those cases.
func (t *fencedTx) ShiftMessages(ctx context.Context, channelID string, fromMS, maxShift int64) (int, error) {
	if err := t.check(); err != nil {
		return 0, err
	}
	rows, err := t.tx.Query(ctx, `
		SELECT target_ms, state, shift_ms FROM map_messages
		WHERE run_id = $1 AND channel_id = $2 AND target_ms >= $3
		ORDER BY target_ms
		FOR UPDATE`, t.lease.RunID, channelID, fromMS)
	if err != nil {
		return 0, err
	}
	var run []int64
	next, blocked, limited := fromMS, false, false
	for rows.Next() {
		var (
			ms, shift int64
			state     string
		)
		if err := rows.Scan(&ms, &state, &shift); err != nil {
			rows.Close()
			return 0, err
		}
		if ms != next {
			break
		}
		if MessageState(state) == MessagePosted {
			blocked = true
			break
		}
		if shift >= maxShift {
			limited = true
		}
		run = append(run, ms)
		next++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	switch {
	case len(run) == 0 && blocked:
		return 0, fmt.Errorf("store: the post at %d in %s is already posted and cannot be moved", fromMS, channelID)
	case len(run) == 0:
		return 0, fmt.Errorf("%w: no post at %d in %s", ErrNotFound, fromMS, channelID)
	case blocked:
		return 0, ErrTimestampTaken
	case limited:
		return 0, ErrShiftLimit
	}
	// Last first: each row moves onto a millisecond the one after it has
	// just left, so the unique index is never violated on the way.
	for i := len(run) - 1; i >= 0; i-- {
		if _, err := t.tx.Exec(ctx, `
			UPDATE map_messages
			SET target_ms = target_ms + 1, shift_ms = shift_ms + 1, updated_at = now()
			WHERE run_id = $1 AND channel_id = $2 AND target_ms = $3`,
			t.lease.RunID, channelID, run[i]); err != nil {
			if code, constraint := pgCode(err); code == sqlUniqueViolation && constraint == "map_messages_target_ms_key" {
				return 0, fmt.Errorf("%w: %w", ErrTimestampTaken, err)
			}
			return 0, err
		}
	}
	return len(run), nil
}

// SkipChannel records that a conversation is not imported, and why. A
// conversation that is completed stays completed.
func (t *fencedTx) SkipChannel(ctx context.Context, slackID, code, reason string) error {
	if err := t.check(); err != nil {
		return err
	}
	tag, err := t.tx.Exec(ctx, `
		UPDATE map_channels SET state = 'skipped', skip_code = $3, skip_reason = $4
		WHERE run_id = $1 AND slack_id = $2 AND state <> 'completed'`,
		t.lease.RunID, slackID, code, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: channel %s (or it is already completed)", ErrNotFound, slackID)
	}
	return nil
}

// MarkFailed records that a post was not made: state must be MessageFailed
// (Graph rejected it permanently) or MessageHeld (its thread anchor
// failed). The row must exist (ErrNotFound). A posted row is never taken
// out of the posted state: that returns ErrTargetConflict.
func (t *fencedTx) MarkFailed(ctx context.Context, key MessageKey, state MessageState, code, reason string) error {
	if err := t.check(); err != nil {
		return err
	}
	if state != MessageFailed && state != MessageHeld {
		return fmt.Errorf("store: MarkFailed state must be %q or %q, not %q", MessageFailed, MessageHeld, state)
	}
	tag, err := t.tx.Exec(ctx, `
		UPDATE map_messages
		SET state = $5, error_code = $6, error = $7, updated_at = now()
		WHERE run_id = $1 AND channel_id = $2 AND slack_ts = $3 AND part = $4 AND state <> 'posted'`,
		t.lease.RunID, key.ChannelID, key.SlackTS, key.Part, string(state), code, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	_, existing, err := t.messageState(ctx, key)
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: message %s/%s/%d is already posted as %q and cannot be marked %s",
		ErrTargetConflict, key.ChannelID, key.SlackTS, key.Part, existing, state)
}

// SetTeam records a team's Teams ID. The team must be in the plan
// (ErrNotFound). Once an ID is recorded it is never replaced: the same ID
// again is a no-op, a different one returns ErrTargetConflict.
func (t *fencedTx) SetTeam(ctx context.Context, index int, teamsID string, adopted bool) error {
	if err := t.check(); err != nil {
		return err
	}
	if teamsID == "" {
		return errors.New("store: SetTeam needs the Teams ID")
	}
	tag, err := t.tx.Exec(ctx, `
		UPDATE map_teams SET teams_id = $3, adopted = $4
		WHERE run_id = $1 AND idx = $2 AND teams_id = ''`,
		t.lease.RunID, index, teamsID, adopted)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var existing string
	err = t.tx.QueryRow(ctx, `SELECT teams_id FROM map_teams WHERE run_id = $1 AND idx = $2`,
		t.lease.RunID, index).Scan(&existing)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: team %d", ErrNotFound, index)
	}
	if err != nil {
		return err
	}
	if existing != teamsID {
		return fmt.Errorf("%w: team %d is recorded as %q, refusing to record %q", ErrTargetConflict, index, existing, teamsID)
	}
	return nil
}

// SetChannel records a channel's Teams ID and state. The channel must be in
// the plan (ErrNotFound). The state is always set. An empty teamsID or a
// nil migrationStart leaves the recorded value as it is, so a state-only
// update cannot erase them. Once a Teams ID is recorded it is never
// replaced by a different one (ErrTargetConflict).
func (t *fencedTx) SetChannel(ctx context.Context, slackID, teamsID string, state ChannelState, migrationStart *time.Time) error {
	if err := t.check(); err != nil {
		return err
	}
	switch state {
	case ChannelPlanned, ChannelCreated, ChannelMigrating, ChannelCompleted:
	default:
		return fmt.Errorf("store: unknown channel state %q", state)
	}
	tag, err := t.tx.Exec(ctx, `
		UPDATE map_channels
		SET teams_id = CASE WHEN $3 = '' THEN teams_id ELSE $3 END,
		    state = $4,
		    migration_start = COALESCE($5, migration_start)
		WHERE run_id = $1 AND slack_id = $2 AND (teams_id = '' OR $3 = '' OR teams_id = $3)`,
		t.lease.RunID, slackID, teamsID, string(state), utcPtr(migrationStart))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var existing string
	err = t.tx.QueryRow(ctx, `SELECT teams_id FROM map_channels WHERE run_id = $1 AND slack_id = $2`,
		t.lease.RunID, slackID).Scan(&existing)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: channel %s", ErrNotFound, slackID)
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: channel %s is recorded as %q, refusing to record %q", ErrTargetConflict, slackID, existing, teamsID)
}
