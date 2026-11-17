-- Finalizing (finalizing channels), purge bookkeeping, and a
-- keyset index for audit log queries.

-- ---------------------------------------------------------------- runs ---

ALTER TABLE runs ADD COLUMN retain boolean NOT NULL DEFAULT false;
ALTER TABLE runs ADD COLUMN purged_at timestamptz;

ALTER TABLE runs DROP CONSTRAINT runs_status_check;
ALTER TABLE runs ADD CONSTRAINT runs_status_check
    CHECK (status IN ('awaiting_export', 'queued', 'running', 'completed',
                      'failed', 'cancelled', 'finalizing', 'finalized'));

-- A finalizing run is active (RunStatus.Active()), so it holds its name.
DROP INDEX runs_one_active_per_name;
CREATE UNIQUE INDEX runs_one_active_per_name ON runs (name)
    WHERE status IN ('awaiting_export', 'queued', 'running', 'failed', 'finalizing');

-- --------------------------------------------------------------- lanes ---

ALTER TABLE lanes DROP CONSTRAINT lanes_stage_check;
ALTER TABLE lanes ADD CONSTRAINT lanes_stage_check
    CHECK (stage IN ('plan', 'extract', 'transform', 'team', 'files', 'load',
                     'finalize', 'members'));

-- Re-rank with finalize and members after load. As in 0002, the stored
-- generated column is dropped and re-added so every existing row's rank is
-- recomputed from its stage, and the claim index is rebuilt on it; all
-- inside this migration's transaction.
DROP INDEX lanes_claim;
ALTER TABLE lanes DROP COLUMN stage_rank;
ALTER TABLE lanes ADD COLUMN stage_rank smallint NOT NULL GENERATED ALWAYS AS (
    CASE stage WHEN 'plan' THEN 0 WHEN 'extract' THEN 1
               WHEN 'transform' THEN 2 WHEN 'team' THEN 3
               WHEN 'files' THEN 4 WHEN 'load' THEN 5
               WHEN 'finalize' THEN 6 WHEN 'members' THEN 7 END) STORED;
CREATE INDEX lanes_claim ON lanes (stage_rank, run_created, run_id, id)
    WHERE status = 'open';

-- ------------------------------------------------------------- audit log ---

-- LogQuery pages in (stream, seq) order with streams compared bytewise, so
-- the order is the same on every server whatever its default collation.
CREATE INDEX audit_log_keyset ON audit_log (run_id, stream COLLATE "C", seq);
