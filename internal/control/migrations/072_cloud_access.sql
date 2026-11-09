-- Cloud, phase 5: just-in-time access to an account's AWS console and
-- command line. A grant is asked for with a reason and a length, approved
-- where the account's environment asks for it, and ends on its own at
-- ends_at. Sessions are made while it lasts and never stored.
CREATE TABLE IF NOT EXISTS cloud_access (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id      UUID NOT NULL REFERENCES cloud_accounts (id),
    level           TEXT NOT NULL CHECK (level IN ('read', 'operate')),
    environment     TEXT NOT NULL CHECK (environment IN ('production', 'staging', 'development')),
    minutes         INTEGER NOT NULL CHECK (minutes BETWEEN 15 AND 720),
    reason          TEXT NOT NULL DEFAULT '',
    -- active grants end at ends_at without anyone touching them; ended is
    -- one someone ended early.
    state           TEXT NOT NULL CHECK (state IN ('awaiting_approval', 'active', 'declined', 'cancelled', 'ended')),
    requested_by    TEXT NOT NULL,
    requested_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_by      TEXT,
    decided_at      TIMESTAMPTZ,
    starts_at       TIMESTAMPTZ,
    ends_at         TIMESTAMPTZ,
    ended_by        TEXT,
    sessions        INTEGER NOT NULL DEFAULT 0,
    last_session_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS cloud_access_requester ON cloud_access (requested_by, requested_at DESC);
CREATE INDEX IF NOT EXISTS cloud_access_open ON cloud_access (account_id) WHERE state IN ('awaiting_approval', 'active');
