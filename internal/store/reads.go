package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// GetBatch returns one batch with its dependencies (ordered by lane ID, then
// seq), or ErrNotFound.
func (s *PG) GetBatch(ctx context.Context, runID string, ref BatchRef) (Batch, error) {
	return getBatch(ctx, s.pool, runID, ref)
}

func getBatch(ctx context.Context, q querier, runID string, ref BatchRef) (Batch, error) {
	var (
		b        Batch
		status   string
		stats    []byte
		depLanes []string
		depSeqs  []int32
	)
	err := q.QueryRow(ctx, `
		SELECT b.run_id, b.lane_id, b.seq, b.key, b.status, b.input, b.output, b.items, b.stats, b.error, b.updated_at,
		       ARRAY(SELECT d.dep_lane_id FROM batch_deps d
		             WHERE d.run_id = b.run_id AND d.lane_id = b.lane_id AND d.seq = b.seq
		             ORDER BY d.dep_lane_id COLLATE "C", d.dep_seq),
		       ARRAY(SELECT d.dep_seq FROM batch_deps d
		             WHERE d.run_id = b.run_id AND d.lane_id = b.lane_id AND d.seq = b.seq
		             ORDER BY d.dep_lane_id COLLATE "C", d.dep_seq)
		FROM batches b
		WHERE b.run_id = $1 AND b.lane_id = $2 AND b.seq = $3`,
		runID, ref.LaneID, ref.Seq).Scan(&b.RunID, &b.LaneID, &b.Seq, &b.Key, &status, &b.Input, &b.Output,
		&b.Items, &stats, &b.Error, &b.UpdatedAt, &depLanes, &depSeqs)
	if errors.Is(err, pgx.ErrNoRows) {
		return Batch{}, fmt.Errorf("%w: batch %s/%d", ErrNotFound, ref.LaneID, ref.Seq)
	}
	if err != nil {
		return Batch{}, err
	}
	b.Status = BatchStatus(status)
	b.Stats = stats
	b.UpdatedAt = utc(b.UpdatedAt)
	for i := range depLanes {
		b.DependsOn = append(b.DependsOn, BatchRef{LaneID: depLanes[i], Seq: int(depSeqs[i])})
	}
	return b, nil
}

// Users returns the run's user mappings ordered by Slack ID.
func (s *PG) Users(ctx context.Context, runID string) ([]UserMapping, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT slack_id, name, email, method, entra_id, entra_upn, reason
		FROM map_users WHERE run_id = $1 ORDER BY slack_id COLLATE "C"`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserMapping
	for rows.Next() {
		var u UserMapping
		if err := rows.Scan(&u.SlackID, &u.Name, &u.Email, &u.Method, &u.EntraID, &u.EntraUPN, &u.Reason); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Teams returns the run's teams ordered by index.
func (s *PG) Teams(ctx context.Context, runID string) ([]TeamMapping, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT idx, name, description, visibility, owner_upn, owner_id, teams_id, adopted
		FROM map_teams WHERE run_id = $1 ORDER BY idx`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TeamMapping
	for rows.Next() {
		var t TeamMapping
		if err := rows.Scan(&t.Index, &t.Name, &t.Description, &t.Visibility, &t.OwnerUPN, &t.OwnerID, &t.TeamsID, &t.Adopted); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

const channelCols = `slack_id, slack_name, kind, COALESCE(team_index, -1), target_name, is_general, created, teams_id, state, migration_start, COALESCE(files_drive_id, ''), COALESCE(files_folder_id, ''), members, topic, skip_code, skip_reason`

func scanChannel(row rowScanner) (ChannelMapping, error) {
	var (
		c        ChannelMapping
		created  *time.Time
		state    string
		migStart *time.Time
	)
	err := row.Scan(&c.SlackID, &c.SlackName, &c.Kind, &c.TeamIndex, &c.TargetName, &c.IsGeneral,
		&created, &c.TeamsID, &state, &migStart, &c.FilesDriveID, &c.FilesFolderID, &c.Members, &c.Topic, &c.SkipCode, &c.SkipReason)
	if err != nil {
		return ChannelMapping{}, err
	}
	if len(c.Members) == 0 {
		c.Members = nil
	}
	if created != nil {
		c.Created = created.UTC()
	}
	c.State = ChannelState(state)
	c.MigrationStart = utcPtr(migStart)
	return c, nil
}

// Channels returns the run's channels ordered by Slack ID.
func (s *PG) Channels(ctx context.Context, runID string) ([]ChannelMapping, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+channelCols+` FROM map_channels WHERE run_id = $1 ORDER BY slack_id COLLATE "C"`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChannelMapping
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Channel returns one channel, or ErrNotFound.
func (s *PG) Channel(ctx context.Context, runID, slackID string) (ChannelMapping, error) {
	c, err := scanChannel(s.pool.QueryRow(ctx, `
		SELECT `+channelCols+` FROM map_channels WHERE run_id = $1 AND slack_id = $2`, runID, slackID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ChannelMapping{}, fmt.Errorf("%w: channel %s", ErrNotFound, slackID)
	}
	return c, err
}

const messageCols = `channel_id, slack_ts, part, target_ms, anchor_ts, hash, state, teams_id, adopted, error_code, error, batch_seq, shift_ms`

func scanMessage(row rowScanner) (Message, error) {
	var (
		m     Message
		state string
	)
	err := row.Scan(&m.ChannelID, &m.SlackTS, &m.Part, &m.TargetMS, &m.AnchorTS, &m.Hash, &state,
		&m.TeamsID, &m.Adopted, &m.ErrorCode, &m.Error, &m.BatchSeq, &m.ShiftMS)
	m.State = MessageState(state)
	return m, err
}

// MessagesInBatch returns the posts of one channel's batch, ordered by
// TargetMS (which is unique within the channel, so the order is total).
func (s *PG) MessagesInBatch(ctx context.Context, runID, channelID string, batchSeq int) ([]Message, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+messageCols+` FROM map_messages
		WHERE run_id = $1 AND channel_id = $2 AND batch_seq = $3
		ORDER BY target_ms`, runID, channelID, batchSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetMessage returns one post, or ErrNotFound.
func (s *PG) GetMessage(ctx context.Context, runID string, key MessageKey) (Message, error) {
	m, err := scanMessage(s.pool.QueryRow(ctx, `
		SELECT `+messageCols+` FROM map_messages
		WHERE run_id = $1 AND channel_id = $2 AND slack_ts = $3 AND part = $4`,
		runID, key.ChannelID, key.SlackTS, key.Part))
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, fmt.Errorf("%w: message %s/%s/%d", ErrNotFound, key.ChannelID, key.SlackTS, key.Part)
	}
	return m, err
}

// LastTargetMS returns the latest millisecond allocated to any post of a
// conversation, or 0 if it has none.
func (s *PG) LastTargetMS(ctx context.Context, runID, channelID string) (int64, error) {
	var ms int64
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(max(target_ms), 0) FROM map_messages
		WHERE run_id = $1 AND channel_id = $2`, runID, channelID).Scan(&ms)
	return ms, err
}

// Anchor returns the anchor ts for a Slack thread, if recorded.
func (s *PG) Anchor(ctx context.Context, runID, channelID, threadTS string) (string, bool, error) {
	var anchor string
	err := s.pool.QueryRow(ctx, `
		SELECT anchor_ts FROM thread_anchors
		WHERE run_id = $1 AND channel_id = $2 AND thread_ts = $3`, runID, channelID, threadTS).Scan(&anchor)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return anchor, true, nil
}

const fileCols = `channel_id, slack_id, name, mimetype, size, state, staging_key, staged_bytes, sha256, drive_id, item_id, webdav_url, etag, error_code, error, batch_seq`

func scanFile(row rowScanner) (FileMapping, error) {
	var (
		f     FileMapping
		state string
	)
	err := row.Scan(&f.ChannelID, &f.SlackID, &f.Name, &f.Mimetype, &f.Size, &state, &f.StagingKey, &f.StagedBytes,
		&f.SHA256, &f.DriveID, &f.ItemID, &f.WebDavURL, &f.ETag, &f.ErrorCode, &f.Error, &f.BatchSeq)
	f.State = FileState(state)
	return f, err
}

// GetFile returns one file row, or ErrNotFound.
func (s *PG) GetFile(ctx context.Context, runID, channelID, slackID string) (FileMapping, error) {
	f, err := scanFile(s.pool.QueryRow(ctx, `
		SELECT `+fileCols+` FROM map_files
		WHERE run_id = $1 AND channel_id = $2 AND slack_id = $3`, runID, channelID, slackID))
	if errors.Is(err, pgx.ErrNoRows) {
		return FileMapping{}, fmt.Errorf("%w: file %s/%s", ErrNotFound, channelID, slackID)
	}
	return f, err
}

// FilesInBatch returns the files of one channel's files batch, ordered by
// Slack file ID (byte order).
func (s *PG) FilesInBatch(ctx context.Context, runID, channelID string, batchSeq int) ([]FileMapping, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+fileCols+` FROM map_files
		WHERE run_id = $1 AND channel_id = $2 AND batch_seq = $3
		ORDER BY slack_id COLLATE "C"`, runID, channelID, batchSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileMapping
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Progress aggregates the run for status displays.
//
// It runs a fixed handful of queries (the run, lanes by stage, batches by
// stage, channels with their message counts and load lane, file totals, and
// problem lanes) in one read-only REPEATABLE READ transaction, so the numbers
// describe a single instant and add up. The cost does not depend on the
// number of channels or lanes beyond the scans themselves; the message
// counts are answered from the map_messages_progress index.
//
// Stages are in pipeline order and include only stages that have lanes.
// Channels are ordered by team index, then Slack name, then Slack ID.
// A lane counts as leased (StageProgress.Leased, ChannelProgress.Active)
// while it is open and has a holder whose lease has not expired. Problems
// lists lanes that are failed, or open with a failed attempt behind them,
// in stage order.
func (s *PG) Progress(ctx context.Context, runID string) (Progress, error) {
	tx, err := s.pool.BeginTx(ctx, repeatableRead)
	if err != nil {
		return Progress{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var p Progress
	var totals []byte
	if err := tx.QueryRow(ctx, `SELECT totals FROM runs WHERE id = $1`, runID).Scan(&totals); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Progress{}, ErrNotFound
		}
		return Progress{}, err
	}
	p.Totals = totals
	if p.Run, err = getRun(ctx, tx, runID); err != nil {
		return Progress{}, err
	}

	// Lanes by stage.
	rows, err := tx.Query(ctx, `
		SELECT stage,
		       count(*),
		       count(*) FILTER (WHERE status = 'done'),
		       count(*) FILTER (WHERE status = 'failed'),
		       count(*) FILTER (WHERE status = 'open' AND holder IS NOT NULL AND lease_expires >= now())
		FROM lanes WHERE run_id = $1
		GROUP BY stage, stage_rank ORDER BY stage_rank`, runID)
	if err != nil {
		return Progress{}, err
	}
	byStage := map[string]int{}
	for rows.Next() {
		var sp StageProgress
		if err := rows.Scan(&sp.Stage, &sp.Lanes, &sp.Done, &sp.Failed, &sp.Leased); err != nil {
			rows.Close()
			return Progress{}, err
		}
		byStage[sp.Stage] = len(p.Stages)
		p.Stages = append(p.Stages, sp)
	}
	if err := rows.Err(); err != nil {
		return Progress{}, err
	}

	// Batches by stage.
	rows, err = tx.Query(ctx, `
		SELECT l.stage, count(*), count(*) FILTER (WHERE b.status = 'done')
		FROM batches b
		JOIN lanes l ON l.run_id = b.run_id AND l.id = b.lane_id
		WHERE b.run_id = $1
		GROUP BY l.stage`, runID)
	if err != nil {
		return Progress{}, err
	}
	for rows.Next() {
		var (
			stage       string
			total, done int
		)
		if err := rows.Scan(&stage, &total, &done); err != nil {
			rows.Close()
			return Progress{}, err
		}
		if i, ok := byStage[stage]; ok {
			p.Stages[i].Batches, p.Stages[i].BatchesDone = total, done
		}
	}
	if err := rows.Err(); err != nil {
		return Progress{}, err
	}

	// Channels, with message counts and the load lane.
	rows, err = tx.Query(ctx, `
		SELECT c.slack_id, c.slack_name, c.target_name, c.kind, c.skip_code, COALESCE(t.name, ''), c.state,
		       COALESCE(m.posts, 0), COALESCE(m.posted, 0), COALESCE(m.failed, 0),
		       COALESCE(m.held, 0), COALESCE(m.adopted, 0),
		       COALESCE(f.files, 0), COALESCE(f.files_done, 0),
		       COALESCE(l.status = 'open' AND l.holder IS NOT NULL AND l.lease_expires >= now(), false),
		       COALESCE(l.status = 'failed', false),
		       COALESCE(l.last_error, '')
		FROM map_channels c
		LEFT JOIN map_teams t ON t.run_id = c.run_id AND t.idx = c.team_index
		LEFT JOIN (
			SELECT channel_id,
			       count(*) AS posts,
			       count(*) FILTER (WHERE state = 'posted') AS posted,
			       count(*) FILTER (WHERE state = 'failed') AS failed,
			       count(*) FILTER (WHERE state = 'held') AS held,
			       count(*) FILTER (WHERE state = 'posted' AND adopted) AS adopted
			FROM map_messages WHERE run_id = $1
			GROUP BY channel_id
		) m ON m.channel_id = c.slack_id
		LEFT JOIN (
			SELECT channel_id,
			       count(*) AS files,
			       count(*) FILTER (WHERE state IN ('uploaded', 'skipped', 'failed')) AS files_done
			FROM map_files WHERE run_id = $1
			GROUP BY channel_id
		) f ON f.channel_id = c.slack_id
		LEFT JOIN lanes l ON l.run_id = c.run_id AND l.id = 'load:' || c.slack_id
		WHERE c.run_id = $1
		ORDER BY c.team_index NULLS LAST, c.slack_name COLLATE "C", c.slack_id COLLATE "C"`, runID)
	if err != nil {
		return Progress{}, err
	}
	for rows.Next() {
		var (
			cp      ChannelProgress
			state   string
			adopted int
		)
		if err := rows.Scan(&cp.SlackID, &cp.SlackName, &cp.TargetName, &cp.Kind, &cp.SkipCode, &cp.Team, &state,
			&cp.Posts, &cp.Posted, &cp.Failed, &cp.Held, &adopted, &cp.Files, &cp.FilesDone,
			&cp.Active, &cp.LaneFailed, &cp.LastError); err != nil {
			rows.Close()
			return Progress{}, err
		}
		cp.State = ChannelState(state)
		p.Channels = append(p.Channels, cp)
		p.Posts += cp.Posts
		p.Posted += cp.Posted
		p.Failed += cp.Failed
		p.Held += cp.Held
		p.Adopted += adopted
	}
	if err := rows.Err(); err != nil {
		return Progress{}, err
	}

	// Files, in one pass over the run's rows.
	if err := tx.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE state = 'uploaded'),
		       count(*) FILTER (WHERE state = 'skipped'),
		       count(*) FILTER (WHERE state = 'failed'),
		       COALESCE(sum(size), 0)::bigint,
		       COALESCE(sum(size) FILTER (WHERE state = 'uploaded'), 0)::bigint
		FROM map_files WHERE run_id = $1`, runID).
		Scan(&p.Files, &p.FilesUploaded, &p.FilesSkipped, &p.FilesFailed, &p.FileBytes, &p.FileBytesDone); err != nil {
		return Progress{}, err
	}

	// Problems.
	rows, err = tx.Query(ctx, `
		SELECT id, stage, status, attempts, not_before, last_error
		FROM lanes
		WHERE run_id = $1 AND (status = 'failed' OR (status = 'open' AND attempts > 0))
		ORDER BY stage_rank, id COLLATE "C"`, runID)
	if err != nil {
		return Progress{}, err
	}
	for rows.Next() {
		var lp LaneProblem
		if err := rows.Scan(&lp.LaneID, &lp.Stage, &lp.Status, &lp.Attempts, &lp.NotBefore, &lp.Error); err != nil {
			rows.Close()
			return Progress{}, err
		}
		lp.NotBefore = utc(lp.NotBefore)
		p.Problems = append(p.Problems, lp)
	}
	if err := rows.Err(); err != nil {
		return Progress{}, err
	}
	return p, nil
}
