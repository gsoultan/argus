-- Each organization's approval policy (approval_policy.go): who may act
-- directly where Argus would otherwise wait for a second person. Only the
-- cells an organization changed are stored; a missing row is "a second
-- person approves", which is what every organization did before this, so an
-- upgrade loosens nothing.
CREATE TABLE IF NOT EXISTS approval_policy_rules (
    kind        TEXT NOT NULL,
    environment TEXT NOT NULL CHECK (environment IN ('production', 'staging', 'other', 'all')),
    choice      TEXT NOT NULL CHECK (choice IN ('second', 'owners', 'admins', 'anyone')),
    set_by      TEXT NOT NULL,
    set_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (kind, environment)
);

-- The policy's switches, one row. Missing means both on.
CREATE TABLE IF NOT EXISTS approval_policy_settings (
    id                BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    require_reason    BOOLEAN NOT NULL DEFAULT TRUE,
    notify_production BOOLEAN NOT NULL DEFAULT TRUE,
    updated_by        TEXT NOT NULL DEFAULT '',
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A change that lets more people act directly waits for another owner.
-- proposal is the whole policy as it would be; base is the version it was
-- asked against, so a policy changed meanwhile is not overwritten.
CREATE TABLE IF NOT EXISTS approval_policy_changes (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    requested_by TEXT NOT NULL,
    requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    reason       TEXT NOT NULL DEFAULT '',
    proposal     JSONB NOT NULL,
    base         TEXT NOT NULL DEFAULT '',
    state        TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'approved', 'denied', 'cancelled')),
    decided_by   TEXT,
    decided_at   TIMESTAMPTZ,
    note         TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS approval_policy_changes_one_pending
    ON approval_policy_changes ((true)) WHERE state = 'pending';
