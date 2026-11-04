-- Statements that need someone else's approval before they run.
--
-- On a database labelled production, a statement that changes rows, empties
-- or drops something, or puts something back from the trash runs only under
-- an approved request for exactly that statement, whatever the person's
-- level. The request is an ordinary access request (principal db:statement),
-- so it waits in Approvals, alerts the deciders and is decided the same way;
-- these rows hold what it is for. A batch (a grid's row edits) is one
-- request with an item per statement. Each item runs once: the relay marks it
-- used as it passes it to the agent, so an approval is not a standing grant.
--
-- New tables, so an upgrade rewrites nothing.

CREATE TABLE IF NOT EXISTS database_statements (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id      UUID NOT NULL REFERENCES access_requests(id) ON DELETE CASCADE,
    database_id     UUID NOT NULL REFERENCES databases(id) ON DELETE CASCADE,
    requester_email TEXT NOT NULL,
    -- What the console's check counted, shown to the approver: rows, tables.
    summary         JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS database_statements_request_key ON database_statements (request_id);
CREATE INDEX IF NOT EXISTS database_statements_requester ON database_statements (database_id, requester_email);

CREATE TABLE IF NOT EXISTS database_statement_items (
    statement_id UUID NOT NULL REFERENCES database_statements(id) ON DELETE CASCADE,
    seq          INT NOT NULL CHECK (seq >= 0),
    sql          TEXT NOT NULL CHECK (length(sql) BETWEEN 1 AND 100000),
    params       JSONB NOT NULL DEFAULT '[]',
    -- sha256 of the statement and its values, as the relay sees them.
    digest       TEXT NOT NULL,
    used_at      TIMESTAMPTZ,
    PRIMARY KEY (statement_id, seq)
);
