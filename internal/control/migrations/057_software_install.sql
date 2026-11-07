-- Installing and removing software on Linux servers, from the console.
--
-- A change is a package run (migration 050) with mode install or remove, so
-- it travels like an update: on the heartbeat, one run per server, approved
-- by a second admin where the server's update rule says so. What a person
-- said yes to is the preview the agent simulated (software_previews); the
-- run carries its hash, and the agent stops when the server would now do
-- something else.
--
-- Widening a CHECK never fails on rows that exist; everything else is an
-- added column or a new table.

ALTER TABLE package_runs DROP CONSTRAINT IF EXISTS package_runs_mode_check;
ALTER TABLE package_runs ADD CONSTRAINT package_runs_mode_check
    CHECK (mode IN ('all', 'security', 'selected', 'install', 'remove'));

-- What to install or remove: [{name, version}], as the preview named them.
ALTER TABLE package_runs ADD COLUMN IF NOT EXISTS items JSONB NOT NULL DEFAULT '[]';
ALTER TABLE package_runs ADD COLUMN IF NOT EXISTS purge BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE package_runs ADD COLUMN IF NOT EXISTS start_services BOOLEAN NOT NULL DEFAULT false;
-- The preview's hash; '' for update runs.
ALTER TABLE package_runs ADD COLUMN IF NOT EXISTS expect TEXT NOT NULL DEFAULT '';
ALTER TABLE package_runs ADD COLUMN IF NOT EXISTS preview_id UUID;
-- The run this one undoes, for "Remove what this installed" and "Reinstall".
ALTER TABLE package_runs ADD COLUMN IF NOT EXISTS undo_of UUID;
-- Services an install started.
ALTER TABLE package_runs ADD COLUMN IF NOT EXISTS started TEXT[] NOT NULL DEFAULT '{}';

CREATE TABLE IF NOT EXISTS software_previews (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    hostname       TEXT NOT NULL,
    asset_hostname TEXT NOT NULL,
    action         TEXT NOT NULL CHECK (action IN ('install', 'remove')),
    items          JSONB NOT NULL,
    purge          BOOLEAN NOT NULL DEFAULT false,
    start_services BOOLEAN NOT NULL DEFAULT false,
    -- The agent's answer: changes, sizes, warnings, why it is blocked.
    result         JSONB NOT NULL,
    hash           TEXT NOT NULL,
    blocked        TEXT NOT NULL DEFAULT '',
    undo_of        UUID,
    created_by     TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS software_previews_created ON software_previews (created_at);
