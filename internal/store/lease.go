package store

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// laneRunnableSQL is a predicate over a lanes row aliased "l": the lane has a
// pending batch, and the first one (lowest seq) has no dependency that is
// not done. Both probes use indexes: batches_unfinished for the next pending
// batch, the batch_deps primary key and the batches primary key for the
// dependencies.
const laneRunnableSQL = `EXISTS (
	SELECT 1
	FROM (
		SELECT b.seq FROM batches b
		WHERE b.run_id = l.run_id AND b.lane_id = l.id
		  AND b.status <> 'done' AND b.status = 'pending'
		ORDER BY b.seq LIMIT 1
	) nb
	WHERE NOT EXISTS (
		SELECT 1
		FROM batch_deps d
		JOIN batches db ON db.run_id = d.run_id AND db.lane_id = d.dep_lane_id AND db.seq = d.dep_seq
		WHERE d.run_id = l.run_id AND d.lane_id = l.id AND d.seq = nb.seq
		  AND db.status <> 'done'
	)
)`

// claimPickSQL finds and locks the best claimable lane.
//
// Plan choice. The partial index lanes_claim holds only open lanes, already
// in claim order (stage_rank, run_created, run_id, id), so with LIMIT 1 the
// planner can walk it in order and stop at the first row that passes; no
// sort and no scan of finished lanes. Per row it first applies the cheap
// column filters (stage, lease, not_before), then the run's status by
// primary key, and only for lanes that survive those the runnable probe,
// which is two or three index lookups and does not depend on how many
// batches the lane has (batches_unfinished skips the done ones). The worst
// case is a long prefix of open lanes that are all blocked (every load lane
// waiting for transform, say): a few thousand lanes times a few index
// probes, which is single-digit milliseconds. Postgres may instead choose a
// sequential scan and a top-1 sort when the table is small; that is equally
// correct.
//
// Locking. FOR UPDATE OF l SKIP LOCKED locks the returned lane row. A lane
// whose row is locked by anyone else (another Claim, or a Fenced
// transaction, which holds FOR UPDATE on its lane) is skipped, not waited
// for, and rows skipped that way do not count against the LIMIT. If the row
// changed after this statement's snapshot, Postgres re-checks the lane
// conditions against the current version, but the subqueries still see the
// old snapshot, so the result is only a candidate: claimTakeSQL decides.
const claimPickSQL = `
SELECT l.run_id, l.id
FROM lanes l
JOIN runs r ON r.id = l.run_id
WHERE l.status = 'open'
  AND l.stage = ANY($1::text[])
  AND ($2::text = '' OR l.run_id = $2::text)
  AND (l.stage = 'release' OR r.status = 'finalizing' OR (r.status IN ('queued', 'running') AND l.stage NOT IN ('finalize', 'members')))
  AND (l.holder IS NULL OR l.lease_expires < now())
  AND l.not_before <= now()
  AND ` + laneRunnableSQL + `
ORDER BY l.stage_rank, l.run_created, l.run_id, l.id
LIMIT 1
FOR UPDATE OF l SKIP LOCKED`

// claimTakeSQL takes the lease on the lane claimPickSQL locked. It runs as a
// new statement in the same transaction, so under READ COMMITTED it has a
// fresh snapshot, and because this transaction holds the lane's row lock
// nothing about the lane or its own batches can change underneath it. Every
// claimability condition is therefore evaluated again here, against current
// data, and the fence is incremented in the same statement.
const claimTakeSQL = `
UPDATE lanes l
SET fence = l.fence + 1,
    holder = $3,
    lease_expires = now() + ($4::bigint * interval '1 microsecond')
WHERE l.run_id = $1 AND l.id = $2
  AND l.status = 'open'
  AND (l.holder IS NULL OR l.lease_expires < now())
  AND l.not_before <= now()
  AND EXISTS (SELECT 1 FROM runs r WHERE r.id = l.run_id
              AND (l.stage = 'release' OR r.status = 'finalizing' OR (r.status IN ('queued', 'running') AND l.stage NOT IN ('finalize', 'members'))))
  AND ` + laneRunnableSQL + `
RETURNING l.run_id, l.id, l.stage, l.key, l.holder, l.fence, l.lease_expires, l.cursor, l.attempts`

// claimAttempts bounds how often Claim re-picks when the candidate it
// locked turned out not to be claimable any more.
const claimAttempts = 8

// Claim leases one claimable lane to holder for ttl, or returns nil.
//
// A lane is claimable when its run is queued, running or finalizing (a failed
// run is not claimable until RetryFailed; a cancelled, completed or finalized
// run never; and finalize and members lanes only while the run is finalizing), it
// is open, it has no holder or its lease has expired by the database clock,
// its NotBefore has passed, and its next pending batch exists and has every
// dependency done. Candidates are ordered by stage (plan, extract,
// transform, team, files, load, finalize, members), then run creation time, then lane ID.
//
// Each attempt is one short transaction of two statements: pick-and-lock a
// candidate (claimPickSQL), then re-verify and take it (claimTakeSQL). The
// fence increases by one on every successful claim, including takeovers, so
// whoever held the lane before can no longer write.
func (s *PG) Claim(ctx context.Context, f ClaimFilter, holder string, ttl time.Duration) (*Lease, error) {
	if len(f.Stages) == 0 {
		return nil, errors.New("store: Claim needs at least one stage")
	}
	if holder == "" {
		return nil, errors.New("store: Claim needs a holder ID")
	}
	if micros(ttl) <= 0 {
		return nil, errors.New("store: Claim needs a positive ttl")
	}
	for range claimAttempts {
		var (
			lease  *Lease
			picked bool
		)
		err := s.inTx(ctx, func(tx pgx.Tx) error {
			lease, picked = nil, false
			var runID, laneID string
			err := tx.QueryRow(ctx, claimPickSQL, f.Stages, f.RunID).Scan(&runID, &laneID)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			picked = true
			l, err := scanLease(tx.QueryRow(ctx, claimTakeSQL, runID, laneID, holder, micros(ttl)))
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // stale candidate; pick again
			}
			if err != nil {
				return err
			}
			lease = &l
			return nil
		})
		if err != nil {
			return nil, err
		}
		if lease != nil || !picked {
			return lease, nil
		}
	}
	return nil, nil
}

func scanLease(row pgx.Row) (Lease, error) {
	var (
		l      Lease
		cursor []byte
	)
	if err := row.Scan(&l.RunID, &l.LaneID, &l.Stage, &l.Key, &l.Holder, &l.Fence, &l.Expires, &cursor, &l.Attempts); err != nil {
		return Lease{}, err
	}
	l.Expires = utc(l.Expires)
	l.Cursor = cursor
	return l, nil
}

// Renew sets the lease to expire ttl from now (database clock) and updates
// l.Expires. It returns ErrFenced if the lease is no longer current: the
// lane was claimed by someone else (the fence moved on), or released, or
// its run was cancelled. A lease that has expired but has not been taken
// over can still be renewed; nobody else has been promised the lane.
func (s *PG) Renew(ctx context.Context, l *Lease, ttl time.Duration) error {
	if l == nil {
		return errors.New("store: nil lease")
	}
	if micros(ttl) <= 0 {
		return errors.New("store: Renew needs a positive ttl")
	}
	var expires time.Time
	err := s.pool.QueryRow(ctx, `
		UPDATE lanes
		SET lease_expires = now() + ($5::bigint * interval '1 microsecond')
		WHERE run_id = $1 AND id = $2 AND fence = $3 AND holder = $4
		RETURNING lease_expires`,
		l.RunID, l.LaneID, l.Fence, l.Holder, micros(ttl)).Scan(&expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrFenced
	}
	if err != nil {
		return err
	}
	l.Expires = utc(expires)
	return nil
}

// Release gives a lane back. With retryAfter > 0 it is a failed attempt: the
// attempt counter is incremented, lastError recorded, and the lane is not
// claimable until retryAfter has passed. Otherwise it is a clean hand-back:
// the attempt counter is reset (and, for an open lane, the last error
// cleared; a failed lane keeps the reason it failed). The fence is left
// alone; the next Claim increments it. Releasing a lost lease changes
// nothing and returns nil.
func (s *PG) Release(ctx context.Context, l *Lease, retryAfter time.Duration, lastError string) error {
	if l == nil {
		return errors.New("store: nil lease")
	}
	if retryAfter > 0 && micros(retryAfter) == 0 {
		retryAfter = time.Microsecond
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE lanes
		SET holder = NULL,
		    lease_expires = NULL,
		    attempts = CASE WHEN $5::bigint > 0 THEN attempts + 1 ELSE 0 END,
		    not_before = CASE WHEN $5::bigint > 0 THEN now() + ($5::bigint * interval '1 microsecond') ELSE not_before END,
		    last_error = CASE WHEN $5::bigint > 0 THEN $6::text
		                      WHEN status = 'open' THEN ''
		                      ELSE last_error END
		WHERE run_id = $1 AND id = $2 AND fence = $3 AND holder = $4`,
		l.RunID, l.LaneID, l.Fence, l.Holder, micros(retryAfter), lastError)
	return err
}

// NextBatch returns the leased lane's next pending batch if all of its
// dependencies are done, or nil if the lane is done, failed or blocked. It
// returns ErrFenced if the lease is no longer current, so a worker that
// lost its lane finds out before doing the work rather than after.
func (s *PG) NextBatch(ctx context.Context, l *Lease) (*Batch, error) {
	if l == nil {
		return nil, errors.New("store: nil lease")
	}
	var (
		current bool
		seq     *int
	)
	err := s.pool.QueryRow(ctx, `
		SELECT l.fence = $3 AND l.holder IS NOT DISTINCT FROM $4::text,
		       (SELECT nb.seq
		        FROM (
		            SELECT b.seq FROM batches b
		            WHERE b.run_id = l.run_id AND b.lane_id = l.id
		              AND b.status <> 'done' AND b.status = 'pending'
		            ORDER BY b.seq LIMIT 1
		        ) nb
		        WHERE l.status = 'open'
		          AND NOT EXISTS (
		            SELECT 1
		            FROM batch_deps d
		            JOIN batches db ON db.run_id = d.run_id AND db.lane_id = d.dep_lane_id AND db.seq = d.dep_seq
		            WHERE d.run_id = l.run_id AND d.lane_id = l.id AND d.seq = nb.seq
		              AND db.status <> 'done'))
		FROM lanes l
		WHERE l.run_id = $1 AND l.id = $2`,
		l.RunID, l.LaneID, l.Fence, l.Holder).Scan(&current, &seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrFenced
	}
	if err != nil {
		return nil, err
	}
	if !current {
		return nil, ErrFenced
	}
	if seq == nil {
		return nil, nil
	}
	b, err := getBatch(ctx, s.pool, l.RunID, BatchRef{LaneID: l.LaneID, Seq: *seq})
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// Fenced runs fn in one transaction after verifying the lease.
//
// The transaction begins by locking the lane row:
//
//	SELECT 1 FROM lanes
//	WHERE run_id = $1 AND id = $2 AND fence = $3 AND holder = $4
//	FOR UPDATE
//
// No row means the lease is not current (taken over, released, or the run
// was cancelled): Fenced returns ErrFenced and fn is not called.
//
// Why FOR UPDATE rather than FOR SHARE. Either would exclude a takeover:
// Claim's FOR UPDATE SKIP LOCKED conflicts with both and skips the
// lane, and if a Claim got there first this statement waits for it and then
// re-checks the fence against the new row version, finding it changed. The
// exclusive lock is chosen because (a) the transaction usually goes on to
// update the lane row itself (cursor, done, failed), and two transactions
// that both hold FOR SHARE and both try to upgrade deadlock; (b) it makes
// transactions under the same lease strictly serial, which is what makes
// "the lane's next batch" and the audit log stream's sequence well defined.
// Nothing else needs to read-lock a lane, so nothing is lost. The lock is
// held until commit, so a write can never commit under a superseded fence:
// the fence cannot move while the transaction is open. Renew, Release and
// CancelRun on the same lane wait for the transaction to end.
//
// A lease that has EXPIRED but has not been claimed by anyone else still
// passes, deliberately. Expiry only makes the lane available; the fence
// changes when someone actually takes it. Until then the old holder is
// still the only writer, and letting it finish is both safe and what we
// want (a slow heartbeat should not discard good work).
//
// If fn returns an error, or panics, the transaction rolls back and nothing
// is written. Serialization and deadlock failures are returned as they are,
// never retried here: fn may have side effects the store cannot see. The Tx
// is valid only until fn returns and must not be used from several
// goroutines at once.
func (s *PG) Fenced(ctx context.Context, l *Lease, fn func(tx Tx) error) error {
	if l == nil {
		return errors.New("store: nil lease")
	}
	if fn == nil {
		return errors.New("store: nil function")
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var one int
		err := tx.QueryRow(ctx, `
			SELECT 1 FROM lanes
			WHERE run_id = $1 AND id = $2 AND fence = $3 AND holder = $4
			FOR UPDATE`,
			l.RunID, l.LaneID, l.Fence, l.Holder).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrFenced
		}
		if err != nil {
			return fmt.Errorf("store: verify lease: %w", err)
		}
		ftx := &fencedTx{tx: tx, lease: *l}
		defer ftx.closed.Store(true)
		return fn(ftx)
	})
}

// fencedTx implements Tx on one Postgres transaction that holds the lane's
// row lock.
type fencedTx struct {
	tx     pgx.Tx
	lease  Lease
	closed atomic.Bool
}

var errTxClosed = errors.New("store: Tx used after its Fenced call returned")

func (t *fencedTx) check() error {
	if t.closed.Load() {
		return errTxClosed
	}
	return nil
}
