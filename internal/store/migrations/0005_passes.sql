-- Incremental passes: a later run of the same migration, from a newer
-- export, that imports only what the passes before it did not.
--
-- A pass is a run. Pass 1 has no base; every later pass names the run it
-- follows, whose posted messages and uploaded files it starts with (see
-- ApplyPlan). Nothing here changes a run that has no base.

-- ---------------------------------------------------------------- runs ---

ALTER TABLE runs ADD COLUMN pass integer NOT NULL DEFAULT 1 CHECK (pass >= 1);
ALTER TABLE runs ADD COLUMN base_run_id text REFERENCES runs (id);
ALTER TABLE runs ADD CONSTRAINT runs_pass_has_base
    CHECK ((pass = 1) = (base_run_id IS NULL));

-- The Slack workspace the run's export came from, as the plan stage read
-- it; empty when the export does not say. A later pass must match it.
ALTER TABLE runs ADD COLUMN workspace_id text NOT NULL DEFAULT '';

-- Finding the pass that follows a run.
CREATE INDEX runs_base ON runs (base_run_id) WHERE base_run_id IS NOT NULL;

-- ------------------------------------------------------------- mapping ---

-- carried marks a row a pass started with: the conversation, post or file
-- was already in Teams when the pass was planned. Such rows are the pass's
-- knowledge of the earlier passes, not its own work, and are left out of
-- its progress.
ALTER TABLE map_channels ADD COLUMN carried boolean NOT NULL DEFAULT false;
ALTER TABLE map_messages ADD COLUMN carried boolean NOT NULL DEFAULT false;
ALTER TABLE map_files ADD COLUMN carried boolean NOT NULL DEFAULT false;

-- A carried conversation may be one the pass's own config and export no
-- longer describe (it was deleted in Slack, or its participants cannot be
-- told any more). It keeps its place in the ledger so that it is still
-- finalized. Such a chat need not name its participants.
-- (No constraint changes: members may be empty for any row.)

-- Slack's "edited" timestamp of the message as it was when it was
-- transformed; empty when it had not been edited. NULL means the row was
-- written before this column existed and the answer is not known. A later
-- pass compares it with the export to count messages edited since.
ALTER TABLE map_messages ADD COLUMN source_edited text;

-- A carried row belongs to no batch of the pass that carries it. It is
-- stored with batch_seq -1, which no batch has, so that the per-batch reads
-- never return it.

-- Progress counts a pass's own rows and its carried rows apart, from the
-- index alone.
DROP INDEX map_messages_progress;
CREATE INDEX map_messages_progress
    ON map_messages (run_id, channel_id, state) INCLUDE (adopted, carried);
