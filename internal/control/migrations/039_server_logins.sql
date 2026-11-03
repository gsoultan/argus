-- Login accounts an administrator made on a server from the console.
--
-- The agent creates each one with useradd, the same way it makes personal
-- accounts, and marks it as its own in the comment field so it never touches
-- an account it did not make. Removing one locks it: nobody can log in, what
-- runs as it is stopped, and its home and files stay. Deleting it for good,
-- home folder included, is a second step an administrator takes on a locked
-- account. Every step is in the audit log; this table is what the agent is
-- told to keep, and the row stays after a delete so the name's history does.
CREATE TABLE IF NOT EXISTS server_logins (
    asset_id   UUID        NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    name       TEXT        NOT NULL CHECK (name ~ '^[a-z_][a-z0-9._-]{0,31}$'),
    -- active: the agent keeps it. locked: the agent locks it. delete: the
    -- agent deletes it and its home. deleted: the agent said it is gone.
    state      TEXT        NOT NULL DEFAULT 'active'
                           CHECK (state IN ('active', 'locked', 'delete', 'deleted')),
    created_by TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    changed_by TEXT        NOT NULL,
    changed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (asset_id, name)
);
