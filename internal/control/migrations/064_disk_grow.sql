-- Growing disks on servers, from the console.
--
-- The agent reports each filesystem's stack and whether it can grow
-- (agents.disks) and whether it can grow disks at all (agents.disks_state).
-- The console asks it to scan now, optionally asking the disks for their
-- size first (agents.disks_scan_id, disks_rescan), and to grow a filesystem
-- (disk_grow_runs). Rules say, per server tag, whether a grow waits for a
-- second admin and whether one starts on its own.
--
-- Only added columns and new tables: nothing to rewrite on a populated
-- database, and an older control plane reading agents ignores the columns.

ALTER TABLE agents ADD COLUMN IF NOT EXISTS disks JSONB;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS disks_state JSONB;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS disks_scan_id TEXT;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS disks_scan_at TIMESTAMPTZ;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS disks_rescan BOOLEAN NOT NULL DEFAULT false;

CREATE TABLE IF NOT EXISTS disk_grow_runs (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- The name the agent reports under, and the server's own name.
    hostname       TEXT NOT NULL,
    asset_hostname TEXT NOT NULL,
    trigger        TEXT NOT NULL CHECK (trigger IN ('manual', 'automatic')),
    state          TEXT NOT NULL CHECK (state IN ('awaiting_approval', 'queued', 'running', 'done', 'failed',
                       'interrupted', 'cancelled', 'denied')),
    -- The filesystem, and what the scan said is under it. The agent refuses
    -- to write when the server no longer matches this.
    fs_key         TEXT NOT NULL,
    mount          TEXT NOT NULL,
    path           JSONB NOT NULL,
    -- How much to give a logical volume from its group; 0 is all the room.
    grow_bytes     BIGINT NOT NULL DEFAULT 0,
    planned_bytes  BIGINT NOT NULL DEFAULT 0,
    summary        TEXT NOT NULL DEFAULT '',
    -- Why an automatic grow started.
    reason         TEXT NOT NULL DEFAULT '',
    requested_by   TEXT NOT NULL,
    requested_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_by     TEXT,
    decided_at     TIMESTAMPTZ,
    -- When the grow was first put on a heartbeat reply: from then on the
    -- agent may be writing, so it can no longer be taken back.
    sent_at        TIMESTAMPTZ,
    started_at     TIMESTAMPTZ,
    finished_at    TIMESTAMPTZ,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    step           TEXT NOT NULL DEFAULT '',
    message        TEXT NOT NULL DEFAULT '',
    -- Each command and its output, and the partition table before anything
    -- was written: what a grow changed is evidence too.
    steps          JSONB NOT NULL DEFAULT '[]',
    table_before   TEXT NOT NULL DEFAULT '',
    size_before    BIGINT NOT NULL DEFAULT 0,
    size_after     BIGINT NOT NULL DEFAULT 0,
    -- An automatic grow names the day it ran on, so a rule starts at most
    -- one a day on a server.
    auto_day       TEXT
);

CREATE INDEX IF NOT EXISTS disk_grow_runs_host ON disk_grow_runs (lower(hostname), requested_at DESC);
-- One grow at a time on a server, whatever asks for it.
CREATE UNIQUE INDEX IF NOT EXISTS disk_grow_runs_one_active ON disk_grow_runs (lower(hostname))
    WHERE state IN ('queued', 'running');
CREATE UNIQUE INDEX IF NOT EXISTS disk_grow_runs_one_per_day ON disk_grow_runs (lower(hostname), auto_day)
    WHERE trigger = 'automatic';

CREATE TABLE IF NOT EXISTS disk_grow_rules (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    position       INTEGER NOT NULL,
    -- The server tag the rule is for; '' is every server no other rule
    -- names, and there is always exactly one.
    tag            TEXT NOT NULL UNIQUE,
    -- A grow asked for by hand on these servers waits for a second admin.
    needs_approval BOOLEAN NOT NULL DEFAULT false,
    -- Grow on its own when a filesystem passes this percent and has room
    -- below it; 0 never. It never makes a cloud volume bigger.
    auto_percent   INTEGER NOT NULL DEFAULT 0 CHECK (auto_percent = 0 OR auto_percent BETWEEN 50 AND 99)
);

-- Show, and grow only by hand; production waits for a second admin.
INSERT INTO disk_grow_rules (position, tag, needs_approval)
SELECT 0, 'production', true
 WHERE NOT EXISTS (SELECT 1 FROM disk_grow_rules);
INSERT INTO disk_grow_rules (position, tag, needs_approval)
SELECT 1, '', false
 WHERE NOT EXISTS (SELECT 1 FROM disk_grow_rules WHERE tag = '');
