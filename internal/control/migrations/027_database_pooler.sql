-- A PgBouncer in front of a database. Sessions go through it when set, so
-- Argus fits a server where applications already connect that way; the
-- agent's provisioning still goes to Postgres itself over the local socket.
-- Both are on the server, like host and port: a socket directory or a
-- loopback address, and the agent refuses anything else.
ALTER TABLE databases
    ADD COLUMN IF NOT EXISTS pooler_host TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS pooler_port INT  NOT NULL DEFAULT 0
        CHECK (pooler_port BETWEEN 0 AND 65535);
