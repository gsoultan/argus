-- Queries people keep from the database workspace.
--
-- A saved query is text and nothing else: running one goes through the
-- person's own access like any statement they type, never the author's. A
-- shared one is listed to everyone who may open the database; only its
-- author changes it, and the author or an admin removes it. Saving, sharing
-- and removing are in the audit log.
--
-- A new table, so an upgrade rewrites nothing.
CREATE TABLE IF NOT EXISTS saved_queries (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    database_id UUID        NOT NULL REFERENCES databases(id) ON DELETE CASCADE,
    owner       TEXT        NOT NULL,
    name        TEXT        NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
    sql         TEXT        NOT NULL CHECK (length(sql) BETWEEN 1 AND 100000),
    shared      BOOLEAN     NOT NULL DEFAULT false,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS saved_queries_database_owner ON saved_queries (database_id, owner);
CREATE INDEX IF NOT EXISTS saved_queries_database_shared ON saved_queries (database_id) WHERE shared;
