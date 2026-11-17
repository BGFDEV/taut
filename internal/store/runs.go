package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const runCols = `id, name, config, config_hash, status, export_key, export_sha256, export_bytes, wrapped_key, created_at, updated_at, finished_at, retain, purged_at, layout, pass, base_run_id, workspace_id`

// prefixed qualifies a comma-separated column list with a table alias.
func prefixed(alias, cols string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

func scanRun(row pgx.Row) (Run, error) {
	var (
		r        Run
		status   string
		finished *time.Time
		purged   *time.Time
		base     *string
	)
	err := row.Scan(&r.ID, &r.Name, &r.Config, &r.ConfigHash, &status, &r.ExportKey,
		&r.ExportSHA256, &r.ExportBytes, &r.WrappedKey, &r.CreatedAt, &r.UpdatedAt, &finished, &r.Retain, &purged, &r.Layout,
		&r.Pass, &base, &r.WorkspaceID)
	if err != nil {
		return Run{}, err
	}
	if base != nil {
		r.BaseRunID = *base
	}
	r.Status = RunStatus(status)
	r.CreatedAt = utc(r.CreatedAt)
	r.UpdatedAt = utc(r.UpdatedAt)
	r.FinishedAt = utcPtr(finished)
	r.PurgedAt = utcPtr(purged)
	if len(r.WrappedKey) == 0 {
		r.WrappedKey = nil
	}
	return r, nil
}

func validRunStatus(s RunStatus) bool {
	switch s {
	case RunAwaitingExport, RunQueued, RunRunning, RunCompleted, RunFailed, RunCancelled, RunFinalizing, RunFinalized:
		return true
	}
	return false
}

// CreateRun inserts a run. An empty Status means awaiting_export. CreatedAt
// and UpdatedAt are set by the database clock; the values in r are ignored,
// as are FinishedAt, PurgedAt, WorkspaceID and, for an incremental pass,
// Pass.
//
// With BaseRunID set the run is an incremental pass. The base must be a run
// of the same name that finished importing (completed or finalized), and no
// run that is not cancelled may already follow it; the pass number is the
// base's plus one. These are checked in the statement that inserts, so two
// passes can never start from one base. It returns ErrNotFound when the base
// does not exist under this name, ErrBaseNotFinished and ErrBaseSuperseded
// otherwise.
func (s *PG) CreateRun(ctx context.Context, r Run) error {
	if r.BaseRunID != "" {
		return s.createPass(ctx, r)
	}
	if r.Pass > 1 {
		return errors.New("store: a run after pass 1 needs a base run")
	}
	if r.ID == "" || r.Name == "" {
		return errors.New("store: a run needs an ID and a name")
	}
	if r.Status == "" {
		r.Status = RunAwaitingExport
	}
	if !validRunStatus(r.Status) {
		return fmt.Errorf("store: unknown run status %q", r.Status)
	}
	config := r.Config
	if config == nil {
		config = []byte{}
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO runs (id, name, config, config_hash, status, export_key, export_sha256, export_bytes, wrapped_key, retain, finished_at, layout)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
		        CASE WHEN $5 IN ('completed', 'cancelled', 'finalized') THEN now() END,
		        CASE WHEN $11::int > 0 THEN $11::int ELSE 3 END)`,
		r.ID, r.Name, config, r.ConfigHash, string(r.Status), r.ExportKey, r.ExportSHA256, r.ExportBytes, r.WrappedKey, r.Retain, int32(r.Layout))
	if code, constraint := pgCode(err); code == sqlUniqueViolation {
		if constraint == "runs_one_active_per_name" {
			return ErrActiveRunExists
		}
		return fmt.Errorf("store: a run with ID %q already exists", r.ID)
	}
	return err
}

// createPass is CreateRun for an incremental pass.
func (s *PG) createPass(ctx context.Context, r Run) error {
	if r.ID == "" || r.Name == "" {
		return errors.New("store: a run needs an ID and a name")
	}
	if r.Status != "" && r.Status != RunAwaitingExport {
		return fmt.Errorf("store: an incremental pass starts awaiting its export, not %q", r.Status)
	}
	config := r.Config
	if config == nil {
		config = []byte{}
	}
	// The base row is locked for the statement, so a second pass racing
	// this one waits and then finds its follower.
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO runs (id, name, config, config_hash, status, wrapped_key, layout, pass, base_run_id)
		SELECT $1, $2, $3, $4, 'awaiting_export', $5,
		       CASE WHEN $6::int > 0 THEN $6::int ELSE 3 END, b.pass + 1, b.id
		FROM (SELECT id, pass FROM runs
		      WHERE id = $7 AND name = $2 AND status IN ('completed', 'finalized')
		      FOR NO KEY UPDATE) b
		WHERE NOT EXISTS (SELECT 1 FROM runs f WHERE f.base_run_id = b.id AND f.status <> 'cancelled')`,
		r.ID, r.Name, config, r.ConfigHash, r.WrappedKey, int32(r.Layout), r.BaseRunID)
	if code, constraint := pgCode(err); code == sqlUniqueViolation {
		if constraint == "runs_one_active_per_name" {
			return ErrActiveRunExists
		}
		return fmt.Errorf("store: a run with ID %q already exists", r.ID)
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	// Nothing was inserted; say which condition failed.
	var (
		status   string
		followed bool
	)
	err = s.pool.QueryRow(ctx, `
		SELECT b.status, EXISTS (SELECT 1 FROM runs f WHERE f.base_run_id = b.id AND f.status <> 'cancelled')
		FROM runs b WHERE b.id = $1 AND b.name = $2`, r.BaseRunID, r.Name).Scan(&status, &followed)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("%w: base run %s of %q", ErrNotFound, r.BaseRunID, r.Name)
	case err != nil:
		return err
	case RunStatus(status) != RunCompleted && RunStatus(status) != RunFinalized:
		return fmt.Errorf("%w: run %s is %s", ErrBaseNotFinished, r.BaseRunID, status)
	case followed:
		return fmt.Errorf("%w: run %s", ErrBaseSuperseded, r.BaseRunID)
	}
	// The base changed between the two statements; the caller may retry.
	return fmt.Errorf("%w: run %s", ErrBaseNotFinished, r.BaseRunID)
}

// GetRun returns a run, or ErrNotFound.
func (s *PG) GetRun(ctx context.Context, id string) (Run, error) {
	return getRun(ctx, s.pool, id)
}

func getRun(ctx context.Context, q querier, id string) (Run, error) {
	r, err := scanRun(q.QueryRow(ctx, `SELECT `+runCols+` FROM runs WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return r, err
}

// ActiveRun returns the active run with this name, if any.
func (s *PG) ActiveRun(ctx context.Context, name string) (Run, bool, error) {
	r, err := scanRun(s.pool.QueryRow(ctx, `
		SELECT `+runCols+` FROM runs
		WHERE name = $1 AND status IN ('awaiting_export', 'queued', 'running', 'failed', 'finalizing')`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, false, nil
	}
	return r, err == nil, err
}

// LatestRun returns the most recently created run with this name.
func (s *PG) LatestRun(ctx context.Context, name string) (Run, bool, error) {
	r, err := scanRun(s.pool.QueryRow(ctx, `
		SELECT `+runCols+` FROM runs
		WHERE name = $1 ORDER BY created_at DESC, id DESC LIMIT 1`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, false, nil
	}
	return r, err == nil, err
}

// PlanLaneID is the ID of the lane SetExport creates, and PlanBatchKey the
// key of its single batch.
const (
	PlanLaneID   = "plan"
	PlanBatchKey = "plan"
)

// SetExport records the uploaded export, creates the plan lane with its
// single batch (seq 0, key "plan", input = the export key), and moves the
// run from awaiting_export to queued.
//
// It is idempotent: repeating it with the same key and digest is a no-op
// whatever the run's status has become since. Repeating it with a different
// export is an error, because the plan lane may already be running against
// the first one.
func (s *PG) SetExport(ctx context.Context, runID, key, sha256 string, size int64) error {
	if key == "" {
		return errors.New("store: SetExport needs the export's staging key")
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var (
			status, curKey, curSHA string
			created                time.Time
		)
		err := tx.QueryRow(ctx, `
			SELECT status, export_key, export_sha256, created_at FROM runs
			WHERE id = $1 FOR NO KEY UPDATE`, runID).Scan(&status, &curKey, &curSHA, &created)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if RunStatus(status) != RunAwaitingExport {
			if curKey == key && curSHA == sha256 {
				return nil
			}
			return fmt.Errorf("store: run %s is %s and already has export %q; a different export cannot be recorded", runID, status, curKey)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE runs SET export_key = $2, export_sha256 = $3, export_bytes = $4,
			                status = 'queued', updated_at = now()
			WHERE id = $1`, runID, key, sha256, size); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO lanes (run_id, id, stage, key, run_created)
			VALUES ($1, $2, 'plan', '', $3)
			ON CONFLICT (run_id, id) DO NOTHING`, runID, PlanLaneID, created); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO batches (run_id, lane_id, seq, key, input)
			VALUES ($1, $2, 0, $3, $4)
			ON CONFLICT (run_id, lane_id, seq) DO NOTHING`, runID, PlanLaneID, PlanBatchKey, key)
		return err
	})
}

// Lock order. Transactions that lock several rows take them in this order,
// so they cannot deadlock with each other: lane rows first (several lanes in
// id order), then the run row. Fenced locks its lane and, in ApplyPlan only,
// the run; CancelRun, RetryFailed and BeginFinalize lock lanes then the run;
// Reconcile and MarkPurged lock only run rows. Run rows are locked FOR NO KEY UPDATE so that foreign
// key checks from concurrent inserts (audit log entries) are not blocked.

// CancelRun marks an active run cancelled and releases its lanes: every
// lease is cleared, so a worker still working gets ErrFenced on its next
// write or renewal. Cancelling an already cancelled or completed run is a
// no-op (a completed run stays completed).
//
// It locks and updates every lane of the run, not just the leased ones, and
// only then changes the run. Holding all the lane locks until commit is what
// closes the race with Claim: a claim already in flight holds its lane's
// lock, so the update here waits for it and then clears the lease it took;
// a claim that starts later skips the locked lanes, and once this commits it
// sees the run cancelled.
func (s *PG) CancelRun(ctx context.Context, runID string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		// Fixed order first (see "Lock order"). This also waits for any
		// Fenced transaction in flight on these lanes.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM lanes WHERE run_id = $1 ORDER BY id FOR UPDATE`, runID); err != nil {
			return err
		}
		// A fresh snapshot: it also covers lanes a plan added while the
		// statement above was waiting.
		if _, err := tx.Exec(ctx, `UPDATE lanes SET holder = NULL, lease_expires = NULL WHERE run_id = $1`, runID); err != nil {
			return err
		}
		var status string
		err := tx.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1 FOR NO KEY UPDATE`, runID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !RunStatus(status).Active() {
			return nil
		}
		var created time.Time
		if err = tx.QueryRow(ctx, `UPDATE runs SET status = 'cancelled', finished_at = now(), updated_at = now() WHERE id = $1 RETURNING created_at`, runID).Scan(&created); err != nil {
			return err
		}
		// The run's chats must not be left in migration mode: nobody will
		// finalize a cancelled run. Queue their release in the same
		// transaction, so a run is never cancelled without it.
		_, err = releaseChats(ctx, tx, runID, created, nil)
		return err
	})
}

// RetryFailed returns the run's failed batches to pending and its failed
// lanes to open, releases them, and returns how many failed lanes were
// reset. It also clears the retry backoff (attempt counter and NotBefore) of
// open lanes that are waiting to retry, since the operator has asked for a
// retry now. A failed run becomes running again (or queued, if it failed
// before it was planned; or finalizing, if it failed while finalizing, which is
// decided by whether it has any finalize or members lane that is not done). It is an error to retry a completed or cancelled
// run, and then nothing is changed.
//
// Serialization with Reconcile. Both decide the run's status only while
// holding the run's row lock, in a statement issued after the lock was
// granted. RetryFailed reopens the lanes first and takes the run lock last
// (the documented order: lanes, then run); it always takes it, whatever the
// status, and only then reads the status and flips failed to running. So
// for any Reconcile that marks the run failed, one of two things is true.
// Either Reconcile got the lock first: RetryFailed waits, then reads the
// failed status with a fresh snapshot and undoes it in the same transaction
// that reopens the lanes. Or RetryFailed holds the lock: Reconcile skips
// the run (SKIP LOCKED), and any later Reconcile takes its decision from a
// snapshot that includes the reopened lanes. Either way the run is never
// left failed with reopened lanes.
func (s *PG) RetryFailed(ctx context.Context, runID string) (int, error) {
	var reset int
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		reset = 0
		// Lanes first, in id order (see "Lock order"). The lanes this
		// statement locks and returns are the ones reset below, by ID:
		// while they are locked no worker can fail or complete a batch
		// in them, so lanes and batches are reset consistently even if
		// other lanes fail meanwhile.
		rows, err := tx.Query(ctx, `
			SELECT id, status FROM lanes
			WHERE run_id = $1 AND (status = 'failed' OR (status = 'open' AND (attempts > 0 OR not_before > now())))
			ORDER BY id FOR UPDATE`, runID)
		if err != nil {
			return err
		}
		var failed, backingOff []string
		for rows.Next() {
			var id, status string
			if err := rows.Scan(&id, &status); err != nil {
				rows.Close()
				return err
			}
			if LaneStatus(status) == LaneFailed {
				failed = append(failed, id)
			} else {
				backingOff = append(backingOff, id)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(failed) > 0 {
			if _, err := tx.Exec(ctx, `
				UPDATE batches SET status = 'pending', error = '', updated_at = now()
				WHERE run_id = $1 AND lane_id = ANY($2::text[]) AND status = 'failed'`, runID, failed); err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `
				UPDATE lanes SET status = 'open', holder = NULL, lease_expires = NULL,
				                 attempts = 0, not_before = now(), last_error = ''
				WHERE run_id = $1 AND id = ANY($2::text[]) AND status = 'failed'`, runID, failed)
			if err != nil {
				return err
			}
			reset = int(tag.RowsAffected())
		}
		if len(backingOff) > 0 {
			if _, err := tx.Exec(ctx, `
				UPDATE lanes SET attempts = 0, not_before = now(), last_error = ''
				WHERE run_id = $1 AND id = ANY($2::text[]) AND status = 'open' AND holder IS NULL`, runID, backingOff); err != nil {
				return err
			}
		}

		// Then the run, locked unconditionally; the status is read only
		// once the lock is held.
		var status string
		err = tx.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1 FOR NO KEY UPDATE`, runID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !RunStatus(status).Active() {
			// Returning an error rolls the lane changes back.
			return fmt.Errorf("store: run %s is %s and cannot be retried", runID, status)
		}
		if RunStatus(status) != RunFailed {
			return nil
		}
		// Which status the run returns to: finalizing if it has finalize or
		// members lanes that are not done (it failed while finalizing; the
		// lanes just reopened are among them), otherwise running, or
		// queued if it was never planned.
		_, err = tx.Exec(ctx, `
			UPDATE runs r
			SET status = CASE
			        WHEN EXISTS (SELECT 1 FROM lanes l
			                     WHERE l.run_id = r.id AND l.stage IN ('finalize', 'members') AND l.status <> 'done')
			          THEN 'finalizing'
			        WHEN r.planned_at IS NULL THEN 'queued'
			        ELSE 'running' END,
			    updated_at = now()
			WHERE r.id = $1`, runID)
		return err
	})
	if err != nil {
		return 0, err
	}
	return reset, nil
}

// Reconcile updates run statuses from their lanes and returns the runs it
// changed:
//
//   - running → completed when the run is planned and every lane is done.
//   - finalizing → finalized when every lane is done (so none is open, leased or
//     failed) and every channel mapping is in state completed. Finalized sets
//     FinishedAt.
//   - finalizing → completed when every lane is done but some channel is not
//     completed: the finalize plan selected only part of the run, and the rest
//     can be finalized later with another BeginFinalize.
//   - running, finalizing or queued → failed when at least one lane has failed and no
//     open lane can make progress, i.e. no open lane's next pending batch
//     has all of its dependencies done. (A lane that is merely leased,
//     backing off, or waiting behind another runnable lane keeps the run
//     alive; a lane waiting on a failed lane does not.) Queued is included
//     so a run whose plan lane failed is reported failed rather than
//     queued forever.
//
// It is safe to run from several controller replicas at once. It first locks the
// candidate run rows with SKIP LOCKED, so replicas never wait on each other
// and each changed run is returned by exactly one of them. The decision is
// then made by a second statement, issued after the locks are held, so its
// snapshot is newer than every transaction that held one of those run
// locks before. Both conditions are stable under concurrent worker work:
// lanes only move toward done, channel states are written only by lanes
// (which are all done when they matter here), and the transitions that
// reopen work (RetryFailed) or add lanes (ApplyPlan, BeginFinalize) take the
// same run lock before
// they commit; see RetryFailed for why the two cannot leave a run failed
// with reopened lanes. Because it never waits for a lock, Reconcile cannot
// take part in a deadlock.
func (s *PG) Reconcile(ctx context.Context) ([]Run, error) {
	var changed []Run
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		changed = nil
		rows, err := tx.Query(ctx, `
			SELECT id FROM runs WHERE status IN ('queued', 'running', 'finalizing')
			ORDER BY id FOR NO KEY UPDATE SKIP LOCKED`)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		rows, err = tx.Query(ctx, `
			UPDATE runs r
			SET status = v.new_status,
			    finished_at = CASE WHEN v.new_status IN ('completed', 'finalized') THEN now() ELSE r.finished_at END,
			    updated_at = now()
			FROM (
				SELECT c.id,
				       CASE
				         WHEN c.status = 'running' AND c.planned_at IS NOT NULL
				              AND NOT EXISTS (SELECT 1 FROM lanes l WHERE l.run_id = c.id AND l.status <> 'done')
				           THEN 'completed'
				         WHEN c.status = 'finalizing'
				              AND NOT EXISTS (SELECT 1 FROM lanes l WHERE l.run_id = c.id AND l.status <> 'done')
				           THEN CASE WHEN EXISTS (SELECT 1 FROM map_channels mc WHERE mc.run_id = c.id AND mc.state NOT IN ('completed', 'skipped'))
				                     THEN 'completed' ELSE 'finalized' END
				         WHEN EXISTS (SELECT 1 FROM lanes l WHERE l.run_id = c.id AND l.status = 'failed')
				              AND NOT EXISTS (SELECT 1 FROM lanes l WHERE l.run_id = c.id AND l.status = 'open' AND `+laneRunnableSQL+`)
				           THEN 'failed'
				       END AS new_status
				FROM runs c
				WHERE c.id = ANY($1::text[]) AND c.status IN ('queued', 'running', 'finalizing')
			) v
			WHERE r.id = v.id AND v.new_status IS NOT NULL
			RETURNING `+prefixed("r", runCols), ids)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRun(rows)
			if err != nil {
				return err
			}
			changed = append(changed, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return changed, nil
}
