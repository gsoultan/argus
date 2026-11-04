-- The Data changes log: every statement from a console session that inserted,
-- updated or deleted rows, emptied or dropped a table, or put something back
-- from the trash, once it was committed. See data_changes.go.
--
-- The statement's text is here, as it is in the session's recording; its
-- values and its results are not. A statement inside a transaction is written
-- when the transaction commits, and never if it rolls back. Kept 180 days;
-- the recording is the evidence and is kept as long as recordings are.

CREATE TABLE IF NOT EXISTS database_changes (
    seq          BIGSERIAL   PRIMARY KEY,
    at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    database_id  UUID        NOT NULL REFERENCES databases(id) ON DELETE CASCADE,
    session_id   TEXT        NOT NULL,
    actor_email  TEXT        NOT NULL,
    -- insert, update, delete, merge, copy, truncate, drop, restore or write.
    verb         TEXT        NOT NULL,
    -- The tables (or schemas) it names, as written.
    objects      TEXT[]      NOT NULL DEFAULT '{}',
    -- Rows it changed, as Postgres counted them; -1 when it does not say.
    rows         BIGINT      NOT NULL DEFAULT -1,
    tag          TEXT        NOT NULL DEFAULT '',
    sql          TEXT        NOT NULL,
    -- The approval it ran under, on production.
    approval_id  TEXT        NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS database_changes_database ON database_changes (database_id, seq);
CREATE INDEX IF NOT EXISTS database_changes_at ON database_changes (at);

-- Data change alerts get their own kind in the bell, so people can mute them.
ALTER TABLE console_alerts DROP CONSTRAINT IF EXISTS console_alerts_kind_check;
ALTER TABLE console_alerts ADD CONSTRAINT console_alerts_kind_check
    CHECK (kind IN ('approval', 'signin', 'session', 'security', 'server', 'backup', 'data'));
