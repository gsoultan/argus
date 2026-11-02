-- Database backups and restores.
--
-- A backup is taken on the database's own server by its agent, as the local
-- admin account over peer authentication, encrypted there to the
-- organization's backup key (derived from the control plane's secret, never
-- stored) and kept on the server, in the control plane's object storage, or
-- both. These rows are the control plane's record of each one: where it is,
-- what it hashes to, and the wrapped data key only a restore grant unwraps.
--
-- A restore into a new database touches nothing that exists. A restore in
-- place renames the current database and keeps it for a day, so a bad
-- restore can be put back; it needs an approval, which is an access_requests
-- row pointing here.

CREATE TABLE IF NOT EXISTS database_backup_schedules (
    database_id  UUID PRIMARY KEY REFERENCES databases(id) ON DELETE CASCADE,
    enabled      BOOLEAN NOT NULL DEFAULT true,
    every        TEXT NOT NULL DEFAULT 'daily' CHECK (every IN ('hourly', 'daily', 'weekly')),
    at_hour      INT NOT NULL DEFAULT 2 CHECK (at_hour BETWEEN 0 AND 23),
    at_minute    INT NOT NULL DEFAULT 0 CHECK (at_minute BETWEEN 0 AND 59),
    weekday      INT NOT NULL DEFAULT 0 CHECK (weekday BETWEEN 0 AND 6),
    destination  TEXT NOT NULL DEFAULT 'server' CHECK (destination IN ('server', 'control', 'both')),
    keep_last    INT NOT NULL DEFAULT 14 CHECK (keep_last BETWEEN 1 AND 1000),
    -- 0 keeps by count only.
    keep_days    INT NOT NULL DEFAULT 0 CHECK (keep_days BETWEEN 0 AND 3650),
    next_run_at  TIMESTAMPTZ,
    -- Consecutive misses, for the alert after three.
    misses       INT NOT NULL DEFAULT 0,
    updated_by   TEXT NOT NULL DEFAULT '',
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS database_backups (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    database_id    UUID NOT NULL REFERENCES databases(id) ON DELETE CASCADE,
    -- An email, or schedule:<database id> for a scheduled run.
    started_by     TEXT NOT NULL,
    reason         TEXT NOT NULL DEFAULT '',
    state          TEXT NOT NULL DEFAULT 'running'
                   CHECK (state IN ('running', 'done', 'failed', 'skipped', 'missed', 'deleted')),
    stage          TEXT NOT NULL DEFAULT '',
    detail         TEXT NOT NULL DEFAULT '',
    error          TEXT NOT NULL DEFAULT '',
    destination    TEXT NOT NULL DEFAULT 'server' CHECK (destination IN ('server', 'control', 'both')),
    -- Where the file is: on the server (root-only), in the control plane's
    -- store, or both. Empty means not there.
    local_path     TEXT NOT NULL DEFAULT '',
    storage_key    TEXT NOT NULL DEFAULT '',
    -- The ciphertext's size and hash; a restore refuses a file that hashes
    -- to anything else.
    bytes          BIGINT NOT NULL DEFAULT 0,
    sha256         TEXT NOT NULL DEFAULT '',
    -- The data key, wrapped to the organization's key. Unwrapped only to
    -- sign a restore grant.
    wrapped_key    TEXT NOT NULL DEFAULT '',
    key_id         TEXT NOT NULL DEFAULT '',
    server_version TEXT NOT NULL DEFAULT '',
    dump_version   TEXT NOT NULL DEFAULT '',
    compression    TEXT NOT NULL DEFAULT '',
    -- pg_restore read the table of contents back after the write.
    verified       BOOLEAN NOT NULL DEFAULT false,
    -- Retention skips a pinned backup.
    pinned         BOOLEAN NOT NULL DEFAULT false,
    grant_id       TEXT NOT NULL DEFAULT '',
    started_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at    TIMESTAMPTZ,
    reported_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS database_backups_db_idx ON database_backups (database_id, started_at DESC);
-- One running job per database: the agent refuses a second anyway, and the
-- console should never show two.
CREATE UNIQUE INDEX IF NOT EXISTS database_backups_one_running
    ON database_backups (database_id) WHERE state = 'running';

CREATE TABLE IF NOT EXISTS database_restores (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    backup_id           UUID NOT NULL REFERENCES database_backups(id) ON DELETE CASCADE,
    -- The database the backup is of; a replace goes back over it, a new
    -- database is created on its server.
    database_id         UUID NOT NULL REFERENCES databases(id) ON DELETE CASCADE,
    mode                TEXT NOT NULL CHECK (mode IN ('new', 'replace')),
    new_name            TEXT NOT NULL DEFAULT '',
    requested_by        TEXT NOT NULL,
    reason              TEXT NOT NULL DEFAULT '',
    -- The approval a replace rests on.
    request_id          UUID REFERENCES access_requests(id),
    state               TEXT NOT NULL DEFAULT 'pending'
                        CHECK (state IN ('pending', 'denied', 'running', 'done', 'failed', 'undone')),
    stage               TEXT NOT NULL DEFAULT '',
    detail              TEXT NOT NULL DEFAULT '',
    error               TEXT NOT NULL DEFAULT '',
    -- replace: where the previous database went, and when it is dropped.
    renamed_to          TEXT NOT NULL DEFAULT '',
    keep_until          TIMESTAMPTZ,
    kept_dropped        BOOLEAN NOT NULL DEFAULT false,
    -- new: the databases row Argus added for it.
    created_database_id UUID REFERENCES databases(id) ON DELETE SET NULL,
    warnings            INT NOT NULL DEFAULT 0,
    grant_id            TEXT NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at          TIMESTAMPTZ,
    finished_at         TIMESTAMPTZ,
    reported_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((mode = 'replace') = (new_name = ''))
);
CREATE INDEX IF NOT EXISTS database_restores_db_idx ON database_restores (database_id, created_at DESC);

ALTER TABLE access_requests ADD COLUMN IF NOT EXISTS restore_id UUID REFERENCES database_restores(id) ON DELETE SET NULL;
