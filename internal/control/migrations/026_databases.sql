-- Database access: a database is something you open on a server, like a
-- terminal or a file browser, and it is recorded the same way.
--
-- A database belongs to an asset (the server its agent runs on). People and
-- teams are assigned to it with a plain access level; the agent turns that
-- into a Postgres role for each person. Nothing here holds a Postgres
-- password: the agent signs in over the server's local socket as the
-- postgres account, and a person's role gets a one-time password for the
-- length of their session only.
--
-- Postgres only for now; the engine column is here so a second one does not
-- need a migration that rewrites this one.

CREATE TABLE IF NOT EXISTS databases (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    asset_id    UUID NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    engine      TEXT NOT NULL DEFAULT 'postgres' CHECK (engine = 'postgres'),
    -- Where the agent reaches it, on its own server: a socket directory or a
    -- loopback address. The agent refuses anything that is not local.
    host        TEXT NOT NULL DEFAULT '/var/run/postgresql',
    port        INT  NOT NULL DEFAULT 5432 CHECK (port BETWEEN 1 AND 65535),
    dbname      TEXT NOT NULL CHECK (length(dbname) BETWEEN 1 AND 63),
    -- The account peer authentication maps to a superuser; 'postgres' by
    -- default. Never a password, only an account name.
    admin_user  TEXT NOT NULL DEFAULT 'postgres',
    -- production / staging / development. Drives the safe defaults: a
    -- production database asks before a schema change.
    label       TEXT NOT NULL DEFAULT 'production'
                CHECK (label IN ('production', 'staging', 'development')),
    -- The safety settings and limits, as the console's Settings tab writes
    -- them. Empty means every default applies.
    policy      JSONB NOT NULL DEFAULT '{}',
    created_by  TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- One database name per server+port: two rows would be two answers to
    -- "who can reach orders on core-db-01".
    UNIQUE (asset_id, port, dbname)
);
CREATE INDEX IF NOT EXISTS databases_asset_idx ON databases (asset_id);

-- Who may open a database, and at what level. A row is a team's or a
-- person's, never both, like asset assignments.
CREATE TABLE IF NOT EXISTS database_assignments (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    database_id UUID NOT NULL REFERENCES databases(id) ON DELETE CASCADE,
    team_id     UUID REFERENCES teams(id) ON DELETE CASCADE,
    user_email  TEXT,
    level       TEXT NOT NULL CHECK (level IN ('read', 'write', 'owner', 'dba')),
    -- When true, every session at this level needs an approved access
    -- request first; the agent only grants the level for that window. The
    -- console defaults it on for write and above on a production database.
    needs_approval BOOLEAN NOT NULL DEFAULT false,
    granted_by  TEXT NOT NULL DEFAULT '',
    granted_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- A team XOR a person.
    CHECK ((team_id IS NULL) <> (user_email IS NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS database_assignments_team_key
    ON database_assignments (database_id, team_id) WHERE team_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS database_assignments_user_key
    ON database_assignments (database_id, user_email) WHERE user_email IS NOT NULL;
CREATE INDEX IF NOT EXISTS database_assignments_db_idx ON database_assignments (database_id);

-- A database session is a session like any other, so live view, termination
-- and the sessions list all work on it. kind tells them it is a database and
-- the recording is statements, not a terminal cast.
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'terminal';
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS database_id UUID REFERENCES databases(id);

-- An access request can be for a database and a level, as well as for a
-- server and a principal. A request names one or the other.
ALTER TABLE access_requests ADD COLUMN IF NOT EXISTS database_id UUID REFERENCES databases(id);
ALTER TABLE access_requests ADD COLUMN IF NOT EXISTS database_level TEXT
    CHECK (database_level IS NULL OR database_level IN ('read', 'write', 'owner', 'dba'));
