-- The files stage, per-channel SharePoint folders, map_files.

-- ------------------------------------------------- the "files" stage ---

-- 1. Allow the new stage.
ALTER TABLE lanes DROP CONSTRAINT lanes_stage_check;
ALTER TABLE lanes ADD CONSTRAINT lanes_stage_check
    CHECK (stage IN ('plan', 'extract', 'transform', 'team', 'files', 'load'));

-- 2. Re-rank: plan < extract < transform < team < files < load.
--
-- stage_rank is a stored generated column, and the claim index is built on
-- it. A generated column's expression cannot be changed in place before
-- Postgres 17 (ALTER COLUMN ... SET EXPRESSION), so, to work on any
-- supported server, the column is dropped and added again with the new
-- expression. Adding a stored generated column rewrites the table and
-- computes the value for every existing row from its stage, so existing
-- lanes get their new rank (load moves from 4 to 5) with no data to carry
-- over and nothing that can drift. The index goes with the old column and
-- is recreated on the new one. All of this happens inside the migration's
-- transaction under an ACCESS EXCLUSIVE lock on lanes, so no claim can ever
-- observe a lane without a rank or the index half-built.
DROP INDEX lanes_claim;
ALTER TABLE lanes DROP COLUMN stage_rank;
ALTER TABLE lanes ADD COLUMN stage_rank smallint NOT NULL GENERATED ALWAYS AS (
    CASE stage WHEN 'plan' THEN 0 WHEN 'extract' THEN 1
               WHEN 'transform' THEN 2 WHEN 'team' THEN 3
               WHEN 'files' THEN 4 WHEN 'load' THEN 5 END) STORED;
CREATE INDEX lanes_claim ON lanes (stage_rank, run_created, run_id, id)
    WHERE status = 'open';

-- --------------------------------------------- channel files folders ---

ALTER TABLE map_channels ADD COLUMN files_drive_id text;
ALTER TABLE map_channels ADD COLUMN files_folder_id text;
ALTER TABLE map_channels ADD CONSTRAINT map_channels_files_folder_check
    CHECK ((files_drive_id IS NULL) = (files_folder_id IS NULL));

-- --------------------------------------------------------------- files ---

-- One Slack file in one channel. A file shared in two channels is two rows.
CREATE TABLE map_files (
    run_id       text NOT NULL,
    channel_id   text NOT NULL,
    slack_id     text NOT NULL,
    name         text NOT NULL DEFAULT '',
    mimetype     text NOT NULL DEFAULT '',
    size         bigint NOT NULL DEFAULT 0,
    state        text NOT NULL DEFAULT 'pending'
                 CHECK (state IN ('pending', 'staged', 'uploaded', 'skipped', 'failed')),
    staging_key  text NOT NULL DEFAULT '',
    staged_bytes bigint NOT NULL DEFAULT 0,
    sha256       text NOT NULL DEFAULT '',
    drive_id     text NOT NULL DEFAULT '',
    item_id      text NOT NULL DEFAULT '',
    webdav_url   text NOT NULL DEFAULT '',
    etag         text NOT NULL DEFAULT '',
    error_code   text NOT NULL DEFAULT '',
    error        text NOT NULL DEFAULT '',
    batch_seq    integer NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, channel_id, slack_id),
    FOREIGN KEY (run_id, channel_id)
        REFERENCES map_channels (run_id, slack_id) ON DELETE CASCADE,
    -- An uploaded row always carries its driveItem; a staged one its key.
    CHECK (state <> 'uploaded' OR item_id <> ''),
    CHECK (state <> 'staged' OR staging_key <> '')
);

-- FilesInBatch: one batch of one channel, in Slack file ID (byte) order.
CREATE INDEX map_files_batch
    ON map_files (run_id, channel_id, batch_seq, slack_id COLLATE "C");
