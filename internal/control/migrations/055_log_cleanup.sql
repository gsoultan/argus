-- Cleaning log files on servers, from the console.
--
-- The agent reports what logs use its server's disk (agents.logs) and
-- whether it can clean them (agents.logs_state). The console asks it to
-- scan now (agents.logs_scan_id) and to clean (log_cleanup_runs). Rules say,
-- per server tag, what a cleanup keeps and whether one starts on its own.
-- Extra paths to clean and to protect are on gateway_policy.
--
-- Only added columns and new tables: nothing to rewrite on a populated
-- database, and an older control plane reading agents ignores the columns.

ALTER TABLE agents ADD COLUMN IF NOT EXISTS logs JSONB;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS logs_state JSONB;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS logs_scan_id TEXT;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS logs_scan_at TIMESTAMPTZ;

CREATE TABLE IF NOT EXISTS log_cleanup_runs (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- The name the agent reports under, and the server's own name.
    hostname       TEXT NOT NULL,
    asset_hostname TEXT NOT NULL,
    trigger        TEXT NOT NULL CHECK (trigger IN ('manual', 'automatic')),
    state          TEXT NOT NULL CHECK (state IN ('awaiting_approval', 'queued', 'running', 'done', 'failed',
                       'interrupted', 'cancelled', 'denied')),
    -- What the agent is told to do: the scan's own files, never a path
    -- the console made up.
    actions        JSONB NOT NULL DEFAULT '[]',
    items          TEXT[] NOT NULL DEFAULT '{}',
    summary        TEXT NOT NULL DEFAULT '',
    planned_bytes  BIGINT NOT NULL DEFAULT 0,
    -- Why an automatic cleanup started.
    reason         TEXT NOT NULL DEFAULT '',
    requested_by   TEXT NOT NULL,
    requested_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_by     TEXT,
    decided_at     TIMESTAMPTZ,
    -- When the cleanup was first put on a heartbeat reply: from then on the
    -- agent may be deleting, so it can no longer be taken back.
    sent_at        TIMESTAMPTZ,
    started_at     TIMESTAMPTZ,
    finished_at    TIMESTAMPTZ,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    step           TEXT NOT NULL DEFAULT '',
    message        TEXT NOT NULL DEFAULT '',
    freed          BIGINT NOT NULL DEFAULT 0,
    -- Every file removed or emptied, with its size and modified time:
    -- what a cleanup deleted is evidence too.
    removed        JSONB NOT NULL DEFAULT '[]',
    removed_more   INTEGER NOT NULL DEFAULT 0,
    skipped        JSONB NOT NULL DEFAULT '[]',
    -- An automatic cleanup names the server's day it ran on, so a rule
    -- starts at most one a day on a server.
    auto_day       TEXT
);

CREATE INDEX IF NOT EXISTS log_cleanup_runs_host ON log_cleanup_runs (lower(hostname), requested_at DESC);
-- One cleanup at a time on a server, whatever asks for it.
CREATE UNIQUE INDEX IF NOT EXISTS log_cleanup_runs_one_active ON log_cleanup_runs (lower(hostname))
    WHERE state IN ('queued', 'running');
CREATE UNIQUE INDEX IF NOT EXISTS log_cleanup_runs_one_per_day ON log_cleanup_runs (lower(hostname), auto_day)
    WHERE trigger = 'automatic';

CREATE TABLE IF NOT EXISTS log_cleanup_rules (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    position            INTEGER NOT NULL,
    -- The server tag the rule is for; '' is every server no other rule
    -- names, and there is always exactly one.
    tag                 TEXT NOT NULL UNIQUE,
    -- off: clean only by hand. disk: when a disk passes disk_percent.
    -- daily: in the window, by the server's own clock.
    mode                TEXT NOT NULL CHECK (mode IN ('off', 'disk', 'daily')),
    disk_percent        INTEGER NOT NULL DEFAULT 85 CHECK (disk_percent BETWEEN 50 AND 99),
    window_start_minute INTEGER NOT NULL DEFAULT 180 CHECK (window_start_minute BETWEEN 0 AND 1439),
    window_minutes      INTEGER NOT NULL DEFAULT 120 CHECK (window_minutes BETWEEN 30 AND 1440),
    rotated_keep_days   INTEGER NOT NULL DEFAULT 14 CHECK (rotated_keep_days BETWEEN 0 AND 3650),
    -- The journal keeps sshd and sudo records too: never less than a week.
    journal_keep_days   INTEGER NOT NULL DEFAULT 28 CHECK (journal_keep_days BETWEEN 7 AND 3650),
    journal_max_mb      INTEGER NOT NULL DEFAULT 2048 CHECK (journal_max_mb BETWEEN 256 AND 1048576),
    -- A log still being written is emptied when bigger than this; 0 never.
    active_over_mb      INTEGER NOT NULL DEFAULT 0 CHECK (active_over_mb = 0 OR active_over_mb BETWEEN 100 AND 1048576),
    active_keep_mb      INTEGER NOT NULL DEFAULT 100 CHECK (active_keep_mb BETWEEN 0 AND 256),
    -- A cleanup asked for by hand on these servers waits for a second admin.
    needs_approval      BOOLEAN NOT NULL DEFAULT false
);

-- Show and alert, clean nothing on its own, until an admin chooses otherwise.
INSERT INTO log_cleanup_rules (position, tag, mode)
SELECT 0, '', 'off'
 WHERE NOT EXISTS (SELECT 1 FROM log_cleanup_rules WHERE tag = '');

ALTER TABLE gateway_policy ADD COLUMN IF NOT EXISTS log_clean_paths TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE gateway_policy ADD COLUMN IF NOT EXISTS log_protect_paths TEXT[] NOT NULL DEFAULT '{}';
