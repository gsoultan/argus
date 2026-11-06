-- Operating system package updates, from the console.
--
-- The agent reports what can be upgraded on its server (agents.packages)
-- and whether it can update it at all (agents.packages_state). The console
-- asks it to look now (agents.packages_check_id) and to install a run
-- (package_runs). Rules say, per server tag, whether runs start on their
-- own and in what window; holds are packages nothing upgrades on its own.
--
-- Only added columns and new tables: nothing to rewrite on a populated
-- database, and an older control plane reading agents ignores the columns.

ALTER TABLE agents ADD COLUMN IF NOT EXISTS packages JSONB;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS packages_state JSONB;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS packages_check_id TEXT;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS packages_check_at TIMESTAMPTZ;

CREATE TABLE IF NOT EXISTS package_runs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- The name the agent reports under, and the server's own name.
    hostname        TEXT NOT NULL,
    asset_hostname  TEXT NOT NULL,
    mode            TEXT NOT NULL CHECK (mode IN ('all', 'security', 'selected')),
    packages        TEXT[] NOT NULL DEFAULT '{}',
    holds           TEXT[] NOT NULL DEFAULT '{}',
    reboot          TEXT NOT NULL DEFAULT 'never' CHECK (reboot IN ('never', 'if_needed')),
    trigger         TEXT NOT NULL CHECK (trigger IN ('manual', 'automatic')),
    state           TEXT NOT NULL CHECK (state IN ('awaiting_approval', 'waiting', 'queued', 'running',
                        'rebooting', 'done', 'failed', 'interrupted', 'cancelled', 'denied')),
    requested_by    TEXT NOT NULL,
    requested_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_by      TEXT,
    decided_at      TIMESTAMPTZ,
    -- When the run was first put on a heartbeat reply: from then on the
    -- agent may be working on it, so it can no longer be taken back.
    sent_at         TIMESTAMPTZ,
    started_at      TIMESTAMPTZ,
    finished_at     TIMESTAMPTZ,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    step            TEXT NOT NULL DEFAULT '',
    message         TEXT NOT NULL DEFAULT '',
    planned         INTEGER NOT NULL DEFAULT 0,
    upgraded        JSONB NOT NULL DEFAULT '[]',
    restarted       TEXT[] NOT NULL DEFAULT '{}',
    reboot_required BOOLEAN NOT NULL DEFAULT false,
    rebooted        BOOLEAN NOT NULL DEFAULT false,
    log             TEXT NOT NULL DEFAULT '',
    -- Several servers updated together, one at a time, in position order.
    batch_id        UUID,
    batch_position  INTEGER,
    -- An automatic run names the window it ran in, so a window starts at
    -- most one run on a server.
    window_start    TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS package_runs_host ON package_runs (lower(hostname), requested_at DESC);
-- One run at a time on a server, whatever asks for it.
CREATE UNIQUE INDEX IF NOT EXISTS package_runs_one_active ON package_runs (lower(hostname))
    WHERE state IN ('queued', 'running', 'rebooting');
CREATE INDEX IF NOT EXISTS package_runs_batch ON package_runs (batch_id, batch_position)
    WHERE batch_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS package_runs_one_per_window ON package_runs (lower(hostname), window_start)
    WHERE trigger = 'automatic';

CREATE TABLE IF NOT EXISTS package_update_rules (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    position            INTEGER NOT NULL,
    -- The server tag the rule is for; '' is every server no other rule
    -- names, and there is always exactly one.
    tag                 TEXT NOT NULL UNIQUE,
    mode                TEXT NOT NULL CHECK (mode IN ('off', 'notify', 'security', 'all')),
    -- 0 is Sunday.
    window_days         INTEGER[] NOT NULL DEFAULT '{0}',
    window_start_minute INTEGER NOT NULL DEFAULT 120 CHECK (window_start_minute BETWEEN 0 AND 1439),
    window_minutes      INTEGER NOT NULL DEFAULT 120 CHECK (window_minutes BETWEEN 30 AND 1440),
    reboot              TEXT NOT NULL DEFAULT 'never' CHECK (reboot IN ('never', 'if_needed')),
    -- A run asked for by hand on these servers waits for a second admin.
    needs_approval      BOOLEAN NOT NULL DEFAULT false
);

-- Look and tell, install nothing, until an admin chooses otherwise.
INSERT INTO package_update_rules (position, tag, mode)
SELECT 0, '', 'notify'
 WHERE NOT EXISTS (SELECT 1 FROM package_update_rules WHERE tag = '');

-- Packages whose upgrade restarts a database or every container: shown as
-- waiting, never upgraded unless named by hand.
ALTER TABLE gateway_policy ADD COLUMN IF NOT EXISTS package_holds TEXT[] NOT NULL
    DEFAULT '{postgresql*,mysql-server*,mariadb-server*,docker-ce*,containerd.io}';
