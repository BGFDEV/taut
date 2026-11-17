-- Direct messages and group DMs go into Teams chats.
--
-- A conversation of kind 'dm' or 'mpim' is a row of map_channels like any
-- channel, so every stage finds it the same way. It differs in three
-- respects, each of which is a column here.

-- A chat belongs to no team. team_index is NULL for it, and the foreign key
-- to map_teams is simply not checked for a NULL (MATCH SIMPLE), so every
-- existing row and query keeps its meaning.
ALTER TABLE map_channels ALTER COLUMN team_index DROP NOT NULL;
ALTER TABLE map_channels ADD CONSTRAINT map_channels_chat_has_no_team
    CHECK ((team_index IS NULL) = (kind IN ('dm', 'mpim')));

-- A chat is created from its whole member list: the Slack user IDs of the
-- conversation's participants, decided by the plan stage. Empty for a
-- channel, whose members are read from the export at finalize.
ALTER TABLE map_channels ADD COLUMN members text[] NOT NULL DEFAULT '{}';

-- The name given to a group chat; empty leaves it unnamed.
ALTER TABLE map_channels ADD COLUMN topic text NOT NULL DEFAULT '';

-- A chat people already use can hold a message of theirs at the very
-- millisecond a Slack message was allocated. The load stage then moves the
-- post forward to a free millisecond. target_ms stays the time the post is
-- (to be) imported at, and remains unique per conversation; shift_ms is how
-- far that is from what the transform stage allocated, so the staged post
-- can still be checked against its row.
ALTER TABLE map_messages ADD COLUMN shift_ms bigint NOT NULL DEFAULT 0
    CHECK (shift_ms >= 0);

-- A conversation whose chat Microsoft refused to create or to put into
-- migration mode is skipped, with the reason, and the run goes on.
ALTER TABLE map_channels DROP CONSTRAINT map_channels_state_check;
ALTER TABLE map_channels ADD CONSTRAINT map_channels_state_check
    CHECK (state IN ('planned', 'created', 'migrating', 'completed', 'skipped'));
ALTER TABLE map_channels ADD COLUMN skip_code text NOT NULL DEFAULT '';
ALTER TABLE map_channels ADD COLUMN skip_reason text NOT NULL DEFAULT '';

-- map_channels named its run without a foreign key of its own; its rows
-- went with their team's. A chat has no team, so say it directly.
ALTER TABLE map_channels ADD CONSTRAINT map_channels_run_fkey
    FOREIGN KEY (run_id) REFERENCES runs (id) ON DELETE CASCADE;

-- The ledger layout a run needs, decided from its config when it is
-- created, before any worker has planned it.
ALTER TABLE runs ADD COLUMN layout integer NOT NULL DEFAULT 3;

-- Release lanes take a chat out of migration mode when its run will not be
-- worked to the end (cancelled, replaced, or failed). They are claimable
-- whatever the run's status and come before everything else.
ALTER TABLE lanes DROP CONSTRAINT lanes_stage_check;
ALTER TABLE lanes ADD CONSTRAINT lanes_stage_check
    CHECK (stage IN ('plan', 'extract', 'transform', 'team', 'files', 'load',
                     'finalize', 'members', 'release'));

DROP INDEX lanes_claim;
ALTER TABLE lanes DROP COLUMN stage_rank;
ALTER TABLE lanes ADD COLUMN stage_rank smallint NOT NULL GENERATED ALWAYS AS (
    CASE stage WHEN 'release' THEN -1 WHEN 'plan' THEN 0 WHEN 'extract' THEN 1
               WHEN 'transform' THEN 2 WHEN 'team' THEN 3
               WHEN 'files' THEN 4 WHEN 'load' THEN 5
               WHEN 'finalize' THEN 6 WHEN 'members' THEN 7 END) STORED;
CREATE INDEX lanes_claim ON lanes (stage_rank, run_created, run_id, id)
    WHERE status = 'open';
