package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Errors of the finalizing and purge operations.
var (
	// ErrNotFinalizable is returned (wrapped, with the status) by BeginFinalize
	// when the run is neither completed nor finalizing.
	ErrNotFinalizable = errors.New("store: only a completed or finalizing run can be finalized")
	// ErrNotFinalized is returned (wrapped, with the status) by MarkPurged
	// when the run is not finalized.
	ErrNotFinalized = errors.New("store: only a finalized run can be purged")
	// ErrChatsMigrating: the run still has a chat in migration mode, or
	// release work for one that is not done. Purging it would destroy the
	// key needed to take the chat out of migration mode.
	ErrChatsMigrating = errors.New("store: the run still has chats in migration mode")
	// ErrPurged: the run's staging and key are gone.
	ErrPurged = errors.New("store: the run has been purged")
)

func finalizeStage(stage string) bool { return stage == StageFinalize || stage == StageMembers }

// BeginFinalize adds a finalize plan to a completed or already finalizing run and makes
// it finalizing.
//
// Errors: ErrNotFound for an unknown run; an error wrapping ErrNotFinalizable
// if the run is in any other status (running, failed, cancelled, finalized,
// …); an error wrapping ErrInvalidPlan if the plan is malformed. On any
// error nothing is changed.
//
// The plan is validated like ApplyPlan's ledger (unique lane IDs, each
// lane's batches exactly seq 0..n-1, every dependency a batch of this
// plan), and its lanes must all be of stage finalize or members.
//
// Then, in one transaction:
//
//   - A lane of the plan that does not exist yet is created with its
//     batches and their dependencies, exactly as ApplyPlan creates lanes
//     (open and pending; a lane with no batches done).
//   - A lane that already exists is left as it is, batches included,
//     whatever the plan now says about it; so repeating a finalize request is
//     harmless and cannot disturb work in progress or work done.
//   - Except that an existing lane which has failed is reopened, with the
//     same effect RetryFailed has on a lane: its failed batches return to
//     pending, it becomes open and unleased, and its attempt counter,
//     backoff and last error are cleared.
//   - The run becomes finalizing, Retain becomes Retain OR p.Retain (a later
//     request never turns retention off), and FinishedAt is cleared: the
//     run is no longer finished.
//
// An existing lane with a plan lane's ID but another stage is refused as an
// invalid plan. An empty plan is allowed; the next Reconcile then settles
// the run as finalized or completed straight away.
//
// Locking follows the store's order, lanes then run: the plan's existing
// lanes are locked first (in ID order), then the run row, and the run's
// status is read only once that lock is held. Reconcile takes the same run
// lock before deciding, so it either sees the run before this call or with
// all of its new lanes, never in between.
func (s *PG) BeginFinalize(ctx context.Context, runID string, p FinalizePlan) error {
	batchCount, deps, err := validateLedger(runID, p.Lanes, p.Batches, finalizeStage)
	if err != nil {
		return err
	}
	laneIDs := make([]string, len(p.Lanes))
	for i, l := range p.Lanes {
		laneIDs[i] = l.ID
	}

	return s.inTx(ctx, func(tx pgx.Tx) error {
		// Lanes first.
		existing := map[string]LaneStatus{}
		var failed []string
		rows, err := tx.Query(ctx, `
			SELECT id, stage, status FROM lanes
			WHERE run_id = $1 AND id = ANY($2::text[])
			ORDER BY id FOR UPDATE`, runID, laneIDs)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, stage, status string
			if err := rows.Scan(&id, &stage, &status); err != nil {
				rows.Close()
				return err
			}
			if !finalizeStage(stage) {
				rows.Close()
				return invalidPlan("lane %s already exists with stage %q", id, stage)
			}
			existing[id] = LaneStatus(status)
			if LaneStatus(status) == LaneFailed {
				failed = append(failed, id)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}

		// Then the run; its status is read under the lock.
		var (
			status  string
			created time.Time
		)
		err = tx.QueryRow(ctx, `SELECT status, created_at FROM runs WHERE id = $1 FOR NO KEY UPDATE`, runID).
			Scan(&status, &created)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if st := RunStatus(status); st != RunCompleted && st != RunFinalizing {
			return fmt.Errorf("%w: run %s is %s", ErrNotFinalizable, runID, status)
		}

		// With the run locked no other BeginFinalize can be adding lanes, so
		// a lane missing from "existing" is missing now. (Two racing
		// BeginFinalize calls both find nothing to lock above; the second
		// then waits here and must look again.)
		rows, err = tx.Query(ctx, `SELECT id, stage, status FROM lanes WHERE run_id = $1 AND id = ANY($2::text[])`, runID, laneIDs)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, stage, status string
			if err := rows.Scan(&id, &stage, &status); err != nil {
				rows.Close()
				return err
			}
			if !finalizeStage(stage) {
				rows.Close()
				return invalidPlan("lane %s already exists with stage %q", id, stage)
			}
			if _, ok := existing[id]; !ok {
				// Created by a concurrent BeginFinalize a moment ago:
				// brand new, so not failed, and left as it is.
				existing[id] = LaneStatus(status)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}

		// Reopen failed lanes of the plan, as RetryFailed would.
		if len(failed) > 0 {
			if _, err := tx.Exec(ctx, `
				UPDATE batches SET status = 'pending', error = '', updated_at = now()
				WHERE run_id = $1 AND lane_id = ANY($2::text[]) AND status = 'failed'`, runID, failed); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE lanes SET status = 'open', holder = NULL, lease_expires = NULL,
				                 attempts = 0, not_before = now(), last_error = ''
				WHERE run_id = $1 AND id = ANY($2::text[]) AND status = 'failed'`, runID, failed); err != nil {
				return err
			}
		}

		// Create what is new.
		var (
			newLanes   []Lane
			newBatches []Batch
			newDeps    []planDep
		)
		for _, l := range p.Lanes {
			if _, ok := existing[l.ID]; !ok {
				newLanes = append(newLanes, l)
			}
		}
		for _, b := range p.Batches {
			if _, ok := existing[b.LaneID]; !ok {
				newBatches = append(newBatches, b)
			}
		}
		var crossing []planDep // new batch -> batch of an existing lane
		for _, d := range deps {
			if _, ok := existing[d.from.LaneID]; ok {
				continue
			}
			newDeps = append(newDeps, d)
			if _, ok := existing[d.to.LaneID]; ok {
				crossing = append(crossing, d)
			}
		}
		// A new batch may depend on a batch of a lane that already
		// exists; that batch must really be there, since the existing
		// lane keeps its own batches rather than the plan's.
		for _, d := range crossing {
			var one int
			err := tx.QueryRow(ctx, `SELECT 1 FROM batches WHERE run_id = $1 AND lane_id = $2 AND seq = $3`,
				runID, d.to.LaneID, d.to.Seq).Scan(&one)
			if errors.Is(err, pgx.ErrNoRows) {
				return invalidPlan("batch %s/%d depends on %s/%d, but lane %s already exists without that batch",
					d.from.LaneID, d.from.Seq, d.to.LaneID, d.to.Seq, d.to.LaneID)
			}
			if err != nil {
				return err
			}
		}
		if err := copyLedger(ctx, tx, runID, created, newLanes, newBatches, newDeps, batchCount); err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			UPDATE runs SET status = 'finalizing', retain = retain OR $2, finished_at = NULL, updated_at = now()
			WHERE id = $1`, runID, p.Retain)
		return err
	})
}

// MarkPurged records that a finalized run's staging was purged: PurgedAt is set
// to the database's current time and the wrapped data key is destroyed, so
// GetRun returns an empty WrappedKey from then on.
//
// It returns ErrNotFound for an unknown run and an error wrapping
// ErrNotFinalized for a run in any other status. It does not look at Retain;
// honouring retention is the caller's decision (Purgeable already
// leaves retained runs out). Purging an already purged run is a no-op that
// keeps the original PurgedAt.
func (s *PG) MarkPurged(ctx context.Context, runID string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1 FOR NO KEY UPDATE`, runID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if RunStatus(status) != RunFinalized && RunStatus(status) != RunCancelled {
			// A completed run that a later pass was planned from is over as
			// well: the pass took its ledger, and nothing will finalize it.
			var superseded bool
			if err := tx.QueryRow(ctx, `SELECT `+supersededSQL+` FROM runs WHERE id = $1`, runID).Scan(&superseded); err != nil {
				return err
			}
			if RunStatus(status) != RunCompleted || !superseded {
				return fmt.Errorf("%w: run %s is %s", ErrNotFinalized, runID, status)
			}
		}
		if n, err := chatsHeld(ctx, tx, runID); err != nil {
			return err
		} else if n > 0 {
			return fmt.Errorf("%w: run %s", ErrChatsMigrating, runID)
		}
		// What the ledger kept about private conversations goes with the
		// staging: who was in each direct message (the label made of their
		// names, the member list, a group's topic) and the names of the
		// files shared in it. The chat's ID and the counts stay; they are
		// the record of what was done.
		if _, err := tx.Exec(ctx, `
			UPDATE map_channels SET target_name = '', members = '{}', topic = '', slack_name = ''
			WHERE run_id = $1 AND kind IN ('dm', 'mpim')`, runID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE map_files f SET name = ''
			FROM map_channels c
			WHERE f.run_id = $1 AND c.run_id = f.run_id AND c.slack_id = f.channel_id AND c.kind IN ('dm', 'mpim')`, runID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			UPDATE runs SET purged_at = now(), wrapped_key = NULL, updated_at = now()
			WHERE id = $1 AND purged_at IS NULL`, runID)
		return err
	})
}

// chatsHeldSQL is true for a run (the row "runs") that still has a chat in
// migration mode, or release work for one that is not done. Such a run
// must keep its data key: releasing a chat needs the run's config.
//
// A run that a later pass was planned from holds no chat: the pass carried
// them, with their state, and is now the run that completes or releases
// them.
const chatsHeldSQL = `(NOT ` + supersededSQL + ` AND (
	EXISTS (SELECT 1 FROM map_channels mc
	        WHERE mc.run_id = runs.id AND mc.kind IN ('dm', 'mpim') AND mc.state = 'migrating')
	OR EXISTS (SELECT 1 FROM lanes rl
	           WHERE rl.run_id = runs.id AND rl.stage = 'release' AND rl.status <> 'done')))`

// supersededSQL is true for a run (the row "runs") that a later pass has
// been planned from. Planning is when the pass copies the run's ledger; an
// unplanned pass has taken nothing and can still be withdrawn.
const supersededSQL = `EXISTS (SELECT 1 FROM runs nx WHERE nx.base_run_id = runs.id AND nx.planned_at IS NOT NULL)`

func chatsHeld(ctx context.Context, tx pgx.Tx, runID string) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM runs WHERE id = $1 AND `+chatsHeldSQL, runID).Scan(&n)
	return n, err
}

// releaseChats queues release work in tx; see ReleaseChats. A chat needs
// releasing if it exists in Teams and is, or may be on its way to being,
// in migration mode: recorded as created or migrating. (A worker that was
// starting its migration when the run stopped may still get there.)
func releaseChats(ctx context.Context, tx pgx.Tx, runID string, runCreated time.Time, slackIDs []string) (int, error) {
	if slackIDs == nil {
		slackIDs = []string{}
	}
	rows, err := tx.Query(ctx, `
		SELECT c.slack_id FROM map_channels c
		WHERE c.run_id = $1 AND c.kind IN ('dm', 'mpim') AND c.teams_id <> ''
		  AND c.state IN ('created', 'migrating')
		  AND (cardinality($2::text[]) = 0 OR c.slack_id = ANY($2::text[]))
		  AND NOT EXISTS (SELECT 1 FROM lanes l WHERE l.run_id = c.run_id AND l.id = 'release:' || c.slack_id)
		ORDER BY c.slack_id COLLATE "C"`, runID, slackIDs)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	var (
		lanes   []Lane
		batches []Batch
		count   = map[string]int{}
	)
	for _, id := range ids {
		lane := StageRelease + ":" + id
		lanes = append(lanes, Lane{ID: lane, Stage: StageRelease, Key: id})
		batches = append(batches, Batch{LaneID: lane, Seq: 0, Key: "release"})
		count[lane] = 1
	}
	if err := copyLedger(ctx, tx, runID, runCreated, lanes, batches, nil, count); err != nil {
		return 0, err
	}
	// Release work that failed, or was done before the chat was put back
	// into migration mode, is tried again.
	if _, err := tx.Exec(ctx, `
		UPDATE batches b SET status = 'pending', error = '', updated_at = now()
		FROM lanes l, map_channels c
		WHERE b.run_id = $1 AND l.run_id = b.run_id AND l.id = b.lane_id AND l.stage = 'release'
		  AND c.run_id = b.run_id AND c.slack_id = l.key AND c.state IN ('created', 'migrating')
		  AND (cardinality($2::text[]) = 0 OR l.key = ANY($2::text[]))
		  AND b.status <> 'pending'`, runID, slackIDs); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE lanes l SET status = 'open', holder = NULL, lease_expires = NULL, attempts = 0, not_before = now(), last_error = ''
		WHERE l.run_id = $1 AND l.stage = 'release' AND l.status <> 'open'
		  AND EXISTS (SELECT 1 FROM batches b WHERE b.run_id = l.run_id AND b.lane_id = l.id AND b.status = 'pending')`, runID); err != nil {
		return 0, err
	}
	var open int
	err = tx.QueryRow(ctx, `SELECT count(*) FROM lanes WHERE run_id = $1 AND stage = 'release' AND status <> 'done'`, runID).Scan(&open)
	return open, err
}

// ReleaseChats implements Store.
func (s *PG) ReleaseChats(ctx context.Context, runID string, slackIDs []string) (int, error) {
	var n int
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// Lanes before the run, the documented order.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM lanes WHERE run_id = $1 AND stage = 'release' ORDER BY id FOR UPDATE`, runID); err != nil {
			return err
		}
		var (
			created time.Time
			purged  *time.Time
		)
		err := tx.QueryRow(ctx, `SELECT created_at, purged_at FROM runs WHERE id = $1 FOR NO KEY UPDATE`, runID).Scan(&created, &purged)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if purged != nil {
			return fmt.Errorf("%w: run %s", ErrPurged, runID)
		}
		n, err = releaseChats(ctx, tx, runID, created, slackIDs)
		return err
	})
	return n, err
}

// EarlierChats implements Store.
func (s *PG) EarlierChats(ctx context.Context, runID, slackID string) ([]EarlierChat, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.id, r.config, c.teams_id, c.migration_start
		FROM map_channels c
		JOIN runs r ON r.id = c.run_id
		WHERE c.slack_id = $2 AND c.run_id <> $1 AND c.kind IN ('dm', 'mpim') AND c.teams_id <> ''
		ORDER BY r.created_at DESC, r.id DESC`, runID, slackID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EarlierChat
	for rows.Next() {
		var e EarlierChat
		var start *time.Time
		if err := rows.Scan(&e.RunID, &e.Config, &e.TeamsID, &start); err != nil {
			return nil, err
		}
		e.MigrationStart = utcPtr(start)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Purgeable returns the runs whose staging is no longer needed and not yet
// purged, oldest first (by creation time): finalized runs that are not
// retained, cancelled runs, and completed runs that a later pass was
// planned from.
func (s *PG) Purgeable(ctx context.Context) ([]Run, error) {
	return s.listRuns(ctx, `
		WHERE ((status = 'finalized' AND NOT retain) OR status = 'cancelled'
		       OR (status = 'completed' AND NOT retain AND `+supersededSQL+`))
		  AND purged_at IS NULL
		  AND NOT EXISTS (
			SELECT 1 FROM lanes l
			WHERE l.run_id = runs.id AND l.holder IS NOT NULL AND l.lease_expires > now())
		  AND NOT `+chatsHeldSQL+`
		ORDER BY created_at, id`)
}

// ActiveRuns implements Store.
func (s *PG) ActiveRuns(ctx context.Context) ([]Run, error) {
	return s.listRuns(ctx, `
		WHERE status IN ('awaiting_export', 'queued', 'running', 'failed', 'finalizing')
		ORDER BY created_at, id`)
}

func (s *PG) listRuns(ctx context.Context, where string) ([]Run, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+runCols+` FROM runs `+where)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Holders returns the workers holding unexpired leases in the run, ordered
// by holder (byte order). A lease counts while its expiry, by the database
// clock, has not passed, whatever the lane's status (a lane just finished
// stays leased until it is released). Stages are the distinct stages of the
// holder's lanes in alphabetical order; LeaseExpires is the latest expiry.
func (s *PG) Holders(ctx context.Context, runID string) ([]Holder, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT holder, count(*), array_agg(DISTINCT stage), max(lease_expires)
		FROM lanes
		WHERE run_id = $1 AND holder IS NOT NULL AND lease_expires >= now()
		GROUP BY holder
		ORDER BY holder COLLATE "C"`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Holder
	for rows.Next() {
		var h Holder
		if err := rows.Scan(&h.Holder, &h.Lanes, &h.Stages, &h.LeaseExpires); err != nil {
			return nil, err
		}
		sort.Strings(h.Stages)
		h.LeaseExpires = utc(h.LeaseExpires)
		out = append(out, h)
	}
	return out, rows.Err()
}

// Limits of a LogQuery page.
const (
	logQueryDefaultLimit = 200
	logQueryMaxLimit     = 5000
)

// likeEscaper makes a string literal inside a LIKE pattern (ESCAPE '\').
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// LogQuery returns a filtered page of the run's audit log and the cursor for
// the next page.
//
// Entries come in (stream, seq) order, streams compared bytewise (the order
// VerifyLog and LogEntries use). Every non-zero field of the filter narrows
// the result, and they combine with AND: Stream, Op and Outcome match
// exactly; Contains is a case-insensitive substring of SourceID, TargetID
// or Detail, taken literally (% and _ are not wildcards); Since is
// inclusive and Until exclusive; After keeps only entries after that
// position in the order, so After{Stream: "x", Seq: 3} continues with
// x/4 and then the streams after x.
//
// Limit bounds the page: zero or negative means 200, and it is capped at
// 5000. The returned cursor is the position of the last entry of the page
// when more matching entries exist beyond it, and the zero LogCursor when
// the page is the last one. Passing it as After, with the rest of the
// filter unchanged, yields the next page; iterating until the zero cursor
// visits every matching entry exactly once (entries appended meanwhile to
// positions already passed are not revisited).
func (s *PG) LogQuery(ctx context.Context, runID string, f LogFilter) ([]LogEntry, LogCursor, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = logQueryDefaultLimit
	}
	if limit > logQueryMaxLimit {
		limit = logQueryMaxLimit
	}
	var since, until any
	if !f.Since.IsZero() {
		since = f.Since.UTC()
	}
	if !f.Until.IsZero() {
		until = f.Until.UTC()
	}
	var pattern any
	if f.Contains != "" {
		pattern = "%" + likeEscaper.Replace(f.Contains) + "%"
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+logCols+` FROM audit_log
		WHERE run_id = $1
		  AND ($2::text = '' OR stream = $2::text)
		  AND ($3::text = '' OR op = $3::text)
		  AND ($4::text = '' OR outcome = $4::text)
		  AND ($5::text IS NULL
		       OR source_id ILIKE $5::text ESCAPE '\'
		       OR target_id ILIKE $5::text ESCAPE '\'
		       OR detail ILIKE $5::text ESCAPE '\')
		  AND ($6::timestamptz IS NULL OR at >= $6::timestamptz)
		  AND ($7::timestamptz IS NULL OR at < $7::timestamptz)
		  AND (stream COLLATE "C", seq) > ($8::text COLLATE "C", $9::bigint)
		ORDER BY stream COLLATE "C", seq
		LIMIT $10`,
		runID, f.Stream, f.Op, f.Outcome, pattern, since, until, f.After.Stream, f.After.Seq, int64(limit)+1)
	if err != nil {
		return nil, LogCursor{}, err
	}
	defer rows.Close()
	var out []LogEntry
	for rows.Next() {
		e, err := scanLogEntry(rows)
		if err != nil {
			return nil, LogCursor{}, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, LogCursor{}, err
	}
	if len(out) <= limit {
		return out, LogCursor{}, nil
	}
	out = out[:limit]
	last := out[len(out)-1]
	return out, LogCursor{Stream: last.Stream, Seq: last.Seq}, nil
}

// Lanes implements Store.
func (s *PG) Lanes(ctx context.Context, runID string, stages ...string) ([]Lane, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, stage, key, status FROM lanes
		WHERE run_id = $1 AND (cardinality($2::text[]) = 0 OR stage = ANY($2::text[]))
		ORDER BY id COLLATE "C"`, runID, append([]string{}, stages...))
	if err != nil {
		return nil, fmt.Errorf("store: listing lanes: %w", err)
	}
	defer rows.Close()
	var out []Lane
	for rows.Next() {
		l := Lane{RunID: runID}
		var status string
		if err := rows.Scan(&l.ID, &l.Stage, &l.Key, &status); err != nil {
			return nil, fmt.Errorf("store: listing lanes: %w", err)
		}
		l.Status = LaneStatus(status)
		out = append(out, l)
	}
	return out, rows.Err()
}
