-- Taut durable store, initial schema.
--
-- Every object is created unqualified, in the first schema of the
-- connection's search_path, so one database can host several isolated
-- copies (tests rely on this).

-- ---------------------------------------------------------------- runs ---

CREATE TABLE runs (
    id            text PRIMARY KEY,
    name          text NOT NULL,
    config        bytea NOT NULL,
    config_hash text NOT NULL,
    status        text NOT NULL
                  CHECK (status IN ('awaiting_export', 'queued', 'running',
                                    'completed', 'failed', 'cancelled')),
    export_key    text NOT NULL DEFAULT '',
    export_sha256 text NOT NULL DEFAULT '',
    export_bytes  bigint NOT NULL DEFAULT 0,
    wrapped_key   bytea,
    -- totals is the plan stage's JSON, stored verbatim (json, not jsonb, so
    -- the bytes round-trip).
    totals        json,
    -- planned_at is set once, by ApplyPlan; it is what makes a second
    -- ApplyPlan a no-op.
    planned_at    timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    finished_at   timestamptz
);

-- At most one active run per name. The status list is RunStatus.Active().
CREATE UNIQUE INDEX runs_one_active_per_name ON runs (name)
    WHERE status IN ('awaiting_export', 'queued', 'running', 'failed');

CREATE INDEX runs_name_created ON runs (name, created_at DESC, id DESC);

-- --------------------------------------------------------------- lanes ---

CREATE TABLE lanes (
    run_id        text NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    id            text NOT NULL,
    stage         text NOT NULL
                  CHECK (stage IN ('plan', 'extract', 'transform', 'team', 'load')),
    -- stage_rank is the claim priority: earlier stages are fed first.
    stage_rank    smallint NOT NULL GENERATED ALWAYS AS (
                      CASE stage WHEN 'plan' THEN 0 WHEN 'extract' THEN 1
                                 WHEN 'transform' THEN 2 WHEN 'team' THEN 3
                                 WHEN 'load' THEN 4 END) STORED,
    -- run_created copies runs.created_at (immutable) so the claim order
    -- (stage, run age, lane id) is a single index on this table.
    run_created   timestamptz NOT NULL,
    key           text NOT NULL DEFAULT '',
    status        text NOT NULL DEFAULT 'open'
                  CHECK (status IN ('open', 'done', 'failed')),
    holder        text,
    lease_expires timestamptz,
    fence         bigint NOT NULL DEFAULT 0,
    cursor        json,
    attempts      integer NOT NULL DEFAULT 0,
    not_before    timestamptz NOT NULL DEFAULT now(),
    last_error    text NOT NULL DEFAULT '',
    PRIMARY KEY (run_id, id),
    CHECK ((holder IS NULL) = (lease_expires IS NULL))
);

-- The claim scan: open lanes in claim order. See the comment on claimPickSQL.
CREATE INDEX lanes_claim ON lanes (stage_rank, run_created, run_id, id)
    WHERE status = 'open';

-- ------------------------------------------------------------- batches ---

CREATE TABLE batches (
    run_id     text NOT NULL,
    lane_id    text NOT NULL,
    seq        integer NOT NULL CHECK (seq >= 0),
    key        text NOT NULL DEFAULT '',
    status     text NOT NULL DEFAULT 'pending'
               CHECK (status IN ('pending', 'done', 'failed')),
    input      text NOT NULL DEFAULT '',
    output     text NOT NULL DEFAULT '',
    items      integer NOT NULL DEFAULT 0,
    stats      json,
    error      text NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, lane_id, seq),
    FOREIGN KEY (run_id, lane_id) REFERENCES lanes (run_id, id) ON DELETE CASCADE
);

-- Unfinished batches only. "The lane's next pending batch" is the first
-- entry of this index for the lane, so finding it costs one probe no matter
-- how many batches the lane has already finished.
CREATE INDEX batches_unfinished ON batches (run_id, lane_id, seq)
    WHERE status <> 'done';

CREATE TABLE batch_deps (
    run_id      text NOT NULL,
    lane_id     text NOT NULL,
    seq         integer NOT NULL,
    dep_lane_id text NOT NULL,
    dep_seq     integer NOT NULL,
    PRIMARY KEY (run_id, lane_id, seq, dep_lane_id, dep_seq),
    FOREIGN KEY (run_id, lane_id, seq)
        REFERENCES batches (run_id, lane_id, seq) ON DELETE CASCADE,
    FOREIGN KEY (run_id, dep_lane_id, dep_seq)
        REFERENCES batches (run_id, lane_id, seq) ON DELETE CASCADE
);

CREATE INDEX batch_deps_reverse ON batch_deps (run_id, dep_lane_id, dep_seq);

-- ------------------------------------------------------------- mapping ---

CREATE TABLE map_users (
    run_id    text NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    slack_id  text NOT NULL,
    name      text NOT NULL DEFAULT '',
    email     text NOT NULL DEFAULT '',
    method    text NOT NULL DEFAULT '',
    entra_id  text NOT NULL DEFAULT '',
    entra_upn text NOT NULL DEFAULT '',
    reason    text NOT NULL DEFAULT '',
    PRIMARY KEY (run_id, slack_id)
);

CREATE TABLE map_teams (
    run_id      text NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    idx         integer NOT NULL,
    name        text NOT NULL DEFAULT '',
    description text NOT NULL DEFAULT '',
    visibility  text NOT NULL DEFAULT '',
    owner_upn   text NOT NULL DEFAULT '',
    owner_id    text NOT NULL DEFAULT '',
    teams_id    text NOT NULL DEFAULT '',
    adopted     boolean NOT NULL DEFAULT false,
    PRIMARY KEY (run_id, idx)
);

CREATE TABLE map_channels (
    run_id          text NOT NULL,
    slack_id        text NOT NULL,
    slack_name      text NOT NULL DEFAULT '',
    kind            text NOT NULL DEFAULT '',
    team_index      integer NOT NULL,
    target_name     text NOT NULL DEFAULT '',
    is_general      boolean NOT NULL DEFAULT false,
    created         timestamptz,
    teams_id        text NOT NULL DEFAULT '',
    state           text NOT NULL DEFAULT 'planned'
                    CHECK (state IN ('planned', 'created', 'migrating', 'completed')),
    migration_start timestamptz,
    PRIMARY KEY (run_id, slack_id),
    FOREIGN KEY (run_id, team_index) REFERENCES map_teams (run_id, idx) ON DELETE CASCADE
);

CREATE TABLE map_messages (
    run_id     text NOT NULL,
    channel_id text NOT NULL,
    -- Slack's ts exactly as written; never parsed.
    slack_ts   text NOT NULL,
    part       integer NOT NULL CHECK (part >= 0),
    target_ms  bigint NOT NULL,
    anchor_ts  text NOT NULL DEFAULT '',
    hash       text NOT NULL DEFAULT '',
    state      text NOT NULL DEFAULT 'pending'
               CHECK (state IN ('pending', 'posted', 'failed', 'held')),
    teams_id   text NOT NULL DEFAULT '',
    adopted    boolean NOT NULL DEFAULT false,
    error_code text NOT NULL DEFAULT '',
    error      text NOT NULL DEFAULT '',
    batch_seq  integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, channel_id, slack_ts, part),
    FOREIGN KEY (run_id, channel_id)
        REFERENCES map_channels (run_id, slack_id) ON DELETE CASCADE,
    -- A posted row always carries its Teams ID.
    CHECK (state <> 'posted' OR teams_id <> '')
);

-- The allocated createdDateTime is unique per channel: Graph rejects
-- duplicates and exactly-once posting relies on that. The name
-- is matched in Go to return ErrTimestampTaken.
CREATE UNIQUE INDEX map_messages_target_ms_key
    ON map_messages (run_id, channel_id, target_ms);

-- MessagesInBatch: one batch of one channel, in target order.
CREATE INDEX map_messages_batch
    ON map_messages (run_id, channel_id, batch_seq, target_ms);

-- Progress: per-channel counts by state, answerable from the index alone.
CREATE INDEX map_messages_progress
    ON map_messages (run_id, channel_id, state) INCLUDE (adopted);

CREATE TABLE thread_anchors (
    run_id     text NOT NULL,
    channel_id text NOT NULL,
    thread_ts  text NOT NULL,
    anchor_ts  text NOT NULL,
    PRIMARY KEY (run_id, channel_id, thread_ts),
    FOREIGN KEY (run_id, channel_id)
        REFERENCES map_channels (run_id, slack_id) ON DELETE CASCADE
);

-- ------------------------------------------------------------- audit log ---

-- No ON DELETE CASCADE: audit entries outlive attempts to remove a run.
CREATE TABLE audit_log (
    run_id       text NOT NULL REFERENCES runs (id),
    stream       text NOT NULL,
    seq          bigint NOT NULL CHECK (seq >= 1),
    at           timestamptz NOT NULL,
    actor        text NOT NULL DEFAULT '',
    op           text NOT NULL DEFAULT '',
    source_id    text NOT NULL DEFAULT '',
    target_id    text NOT NULL DEFAULT '',
    content_hash text NOT NULL DEFAULT '',
    outcome      text NOT NULL DEFAULT '',
    detail       text NOT NULL DEFAULT '',
    prev_hash    text NOT NULL,
    entry_hash   text NOT NULL,
    PRIMARY KEY (run_id, stream, seq)
);

-- The head of each stream: the last assigned seq and its hash. Appending
-- locks this row, which serializes appends to one stream. VerifyLog also
-- compares it with the chain's tail, which catches a truncated stream.
CREATE TABLE audit_log_heads (
    run_id text NOT NULL REFERENCES runs (id),
    stream text NOT NULL,
    seq    bigint NOT NULL DEFAULT 0,
    hash   text NOT NULL DEFAULT '',
    PRIMARY KEY (run_id, stream)
);

-- Append-only, enforced by the database rather than by convention.
CREATE FUNCTION audit_log_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'the audit log is append-only: % is not allowed', TG_OP
        USING ERRCODE = 'integrity_constraint_violation';
END;
$$;

CREATE TRIGGER audit_log_no_update_delete
    BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_append_only();

CREATE TRIGGER audit_log_no_truncate
    BEFORE TRUNCATE ON audit_log
    FOR EACH STATEMENT EXECUTE FUNCTION audit_log_append_only();
