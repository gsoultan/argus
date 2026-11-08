-- Install scripts in the Software library.
--
-- A script is what someone would otherwise paste into a root shell or an
-- administrator PowerShell. Every save is a new version with the SHA-256 of
-- each part; a version is checked automatically, then reviewed by someone
-- other than its author, and only then signed and runnable. A run is a
-- package run (migration 050) with mode script that names the version, so
-- editing a script never changes what an approved run executes.
--
-- New tables, added columns and a widened CHECK: nothing here can fail on
-- rows that exist.

CREATE TABLE IF NOT EXISTS software_scripts (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL,
    platform    TEXT NOT NULL CHECK (platform IN ('linux', 'windows')),
    description TEXT NOT NULL DEFAULT '',
    created_by  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    archived_at TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS software_scripts_name
    ON software_scripts (lower(name)) WHERE archived_at IS NULL;

CREATE TABLE IF NOT EXISTS software_script_versions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    script_id       UUID NOT NULL REFERENCES software_scripts (id),
    version         INTEGER NOT NULL,
    install         TEXT NOT NULL,
    uninstall       TEXT NOT NULL DEFAULT '',
    check_text      TEXT NOT NULL DEFAULT '',
    -- [{name, label, kind: text|choice|secret, choices, required}]
    params          JSONB NOT NULL DEFAULT '[]',
    run_as          TEXT NOT NULL DEFAULT '',
    timeout_seconds INTEGER NOT NULL DEFAULT 1800,
    reboot          TEXT NOT NULL DEFAULT 'never' CHECK (reboot IN ('never', 'if_asked')),
    install_sha     TEXT NOT NULL,
    uninstall_sha   TEXT NOT NULL DEFAULT '',
    check_sha       TEXT NOT NULL DEFAULT '',
    -- What the automatic checks found, and whether any of it blocks.
    findings        JSONB NOT NULL DEFAULT '[]',
    blocked         BOOLEAN NOT NULL DEFAULT false,
    note            TEXT NOT NULL DEFAULT '',
    author          TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- The review: who, when, the warnings they accepted and why, and the
    -- signed approval agents check before running anything.
    reviewed_by     TEXT,
    reviewed_at     TIMESTAMPTZ,
    self_reviewed   BOOLEAN NOT NULL DEFAULT false,
    accepted        JSONB NOT NULL DEFAULT '[]',
    signature       TEXT NOT NULL DEFAULT '',
    -- The first server outside production it ran on successfully: a version
    -- must have one before it runs on production.
    tried_on        TEXT,
    tried_at        TIMESTAMPTZ,
    tried_run       UUID,
    UNIQUE (script_id, version)
);

ALTER TABLE package_runs DROP CONSTRAINT IF EXISTS package_runs_mode_check;
ALTER TABLE package_runs ADD CONSTRAINT package_runs_mode_check
    CHECK (mode IN ('all', 'security', 'selected', 'install', 'remove', 'script'));

ALTER TABLE package_runs ADD COLUMN IF NOT EXISTS script_version_id UUID;
-- install or uninstall: which part runs.
ALTER TABLE package_runs ADD COLUMN IF NOT EXISTS script_action TEXT NOT NULL DEFAULT '';
-- Parameter values that are not secret, by name.
ALTER TABLE package_runs ADD COLUMN IF NOT EXISTS script_params JSONB NOT NULL DEFAULT '{}';
-- Secret values, sealed with a key derived from signing_secret, and wiped
-- when the run ends.
ALTER TABLE package_runs ADD COLUMN IF NOT EXISTS script_secrets BYTEA;
-- What changed on the server while it ran.
ALTER TABLE package_runs ADD COLUMN IF NOT EXISTS script_report JSONB;
